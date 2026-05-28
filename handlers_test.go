package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// mockProxmox implements ProxmoxAPI for testing.
type mockProxmox struct {
	mu       sync.Mutex
	statuses map[int]string // vmid -> status
	started  []int
	stopped  []int
	vms      []ProxmoxInstance
	lxcs     []ProxmoxInstance
}

func newMockProxmox(statuses map[int]string) *mockProxmox {
	return &mockProxmox{statuses: statuses}
}

func (m *mockProxmox) GetStatus(inst Instance) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.statuses[inst.VMID]
	if !ok {
		return "unknown", nil
	}
	return s, nil
}

func (m *mockProxmox) Start(inst Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = append(m.started, inst.VMID)
	m.statuses[inst.VMID] = "running"
	return nil
}

func (m *mockProxmox) Stop(inst Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, inst.VMID)
	m.statuses[inst.VMID] = "stopped"
	return nil
}

func (m *mockProxmox) ListVMs() ([]ProxmoxInstance, error) {
	return m.vms, nil
}

func (m *mockProxmox) ListLXCs() ([]ProxmoxInstance, error) {
	return m.lxcs, nil
}

func testInstances() ([]Instance, map[int]string) {
	instances := []Instance{
		{VMID: 100, Name: "vm-a", Type: "qemu", Tier: 1},
		{VMID: 101, Name: "lxc-b", Type: "lxc", Tier: 1},
		{VMID: 200, Name: "vm-c", Type: "qemu", Tier: 2},
	}
	tierNames := map[int]string{
		1: "infra",
		2: "apps",
	}
	return instances, tierNames
}

func TestComputeState(t *testing.T) {
	tests := []struct {
		name          string
		statuses      []string
		transitioning bool
		want          string
	}{
		{"all running", []string{"running", "running"}, false, "awake"},
		{"all stopped", []string{"stopped", "stopped"}, false, "asleep"},
		{"mixed", []string{"running", "stopped"}, false, "mixed"},
		{"transitioning", []string{"running", "stopped"}, true, "transitioning"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var instances []InstanceStatus
			for i, s := range tt.statuses {
				instances = append(instances, InstanceStatus{VMID: i, Status: s})
			}
			tiers := []TierStatus{{Tier: 1, Instances: instances}}
			got := computeState(tiers, tt.transitioning)
			if got != tt.want {
				t.Errorf("computeState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleStatusAllRunning(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 101: "running", 200: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	orch.HandleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}

	var resp StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if resp.State != "awake" {
		t.Errorf("state = %q, want awake", resp.State)
	}
	if resp.Transitioning {
		t.Error("transitioning should be false")
	}
	if len(resp.Tiers) != 2 {
		t.Fatalf("got %d tiers, want 2", len(resp.Tiers))
	}
	if resp.Tiers[0].Name != "infra" {
		t.Errorf("tier 1 name = %q, want infra", resp.Tiers[0].Name)
	}
	if len(resp.Tiers[0].Instances) != 2 {
		t.Errorf("tier 1 has %d instances, want 2", len(resp.Tiers[0].Instances))
	}
}

func TestHandleStatusAllStopped(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	orch.HandleStatus(w, req)

	var resp StatusResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.State != "asleep" {
		t.Errorf("state = %q, want asleep", resp.State)
	}
}

func TestHandleStatusMixed(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 101: "stopped", 200: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	orch.HandleStatus(w, req)

	var resp StatusResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.State != "mixed" {
		t.Errorf("state = %q, want mixed", resp.State)
	}
}

func TestRefreshReplacesDiscovery(t *testing.T) {
	// Start with two infra-tagged VMs.
	mock := &mockProxmox{
		statuses: map[int]string{100: "running", 101: "running"},
		vms: []ProxmoxInstance{
			{VMID: 100, Name: "vm-a", Status: "running", Tags: "infra", Type: "qemu"},
			{VMID: 101, Name: "vm-b", Status: "running", Tags: "infra", Type: "qemu"},
		},
	}
	tierDefs := []TierConfig{{Tag: "infra", Tier: 1, Name: "infra"}}
	instances, tierNames, err := DiscoverInstances(mock, tierDefs)
	if err != nil {
		t.Fatalf("initial discovery: %v", err)
	}
	orch := NewOrchestrator(instances, tierNames, tierDefs, mock)

	// Proxmox state changes: vm-b is gone, vm-c appears.
	mock.mu.Lock()
	mock.vms = []ProxmoxInstance{
		{VMID: 100, Name: "vm-a", Status: "running", Tags: "infra", Type: "qemu"},
		{VMID: 102, Name: "vm-c", Status: "running", Tags: "infra", Type: "qemu"},
	}
	mock.statuses = map[int]string{100: "running", 102: "running"}
	mock.mu.Unlock()

	if err := orch.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	resp := orch.Status()
	if len(resp.Tiers) != 1 || len(resp.Tiers[0].Instances) != 2 {
		t.Fatalf("after refresh: got %d tiers, want 1 with 2 instances", len(resp.Tiers))
	}

	vmids := map[int]bool{}
	for _, inst := range resp.Tiers[0].Instances {
		vmids[inst.VMID] = true
	}
	if !vmids[100] || !vmids[102] || vmids[101] {
		t.Errorf("vmids after refresh = %v, want {100, 102} (no 101)", vmids)
	}
}

func TestRefreshSkippedDuringTransition(t *testing.T) {
	mock := &mockProxmox{
		statuses: map[int]string{100: "stopped"},
		vms: []ProxmoxInstance{
			{VMID: 100, Name: "vm-a", Status: "stopped", Tags: "infra", Type: "qemu"},
		},
	}
	tierDefs := []TierConfig{{Tag: "infra", Tier: 1, Name: "infra"}}
	instances, tierNames, _ := DiscoverInstances(mock, tierDefs)
	orch := NewOrchestrator(instances, tierNames, tierDefs, mock)

	if !orch.Wake() {
		t.Fatal("Wake() returned false")
	}

	// Swap underlying Proxmox state — Refresh must NOT pick this up while
	// a transition is in flight.
	mock.mu.Lock()
	mock.vms = []ProxmoxInstance{
		{VMID: 200, Name: "vm-z", Status: "running", Tags: "infra", Type: "qemu"},
	}
	mock.mu.Unlock()

	if err := orch.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// findInstance(100) should still resolve (old snapshot retained).
	if inst := orch.findInstance(100); inst == nil {
		t.Error("findInstance(100) = nil after Refresh-during-transition; old state should be retained")
	}
	if inst := orch.findInstance(200); inst != nil {
		t.Error("findInstance(200) != nil; Refresh should have been skipped")
	}
}

func TestHandleStatusMethodNotAllowed(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/status", nil)
	w := httptest.NewRecorder()
	orch.HandleStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", w.Code)
	}
}

func TestHandleWake(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleWake(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "started" {
		t.Errorf("response status = %q, want started", resp["status"])
	}

	// Should be transitioning now
	if !orch.isTransitioning() {
		t.Error("expected orchestrator to be transitioning")
	}
}

func TestHandleWakeConflict(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	// Start first wake
	req := httptest.NewRequest("POST", "/api/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleWake(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("first wake: status code = %d, want 202", w.Code)
	}

	// Second wake should conflict
	req = httptest.NewRequest("POST", "/api/wake", nil)
	w = httptest.NewRecorder()
	orch.HandleWake(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("second wake: status code = %d, want 409", w.Code)
	}
}

func TestHandleSleep(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 101: "running", 200: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/sleep", nil)
	w := httptest.NewRecorder()
	orch.HandleSleep(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", w.Code)
	}
}

func TestHandleWakeMethodNotAllowed(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("GET", "/api/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleWake(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", w.Code)
	}
}

func TestHandleInstanceStart(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/instance/100/start", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "started" {
		t.Errorf("response status = %q, want started", resp["status"])
	}

	// Verify the mock was called
	if len(mock.started) != 1 || mock.started[0] != 100 {
		t.Errorf("started = %v, want [100]", mock.started)
	}
}

func TestHandleInstanceStop(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 101: "running", 200: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/instance/200/stop", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}

	if len(mock.stopped) != 1 || mock.stopped[0] != 200 {
		t.Errorf("stopped = %v, want [200]", mock.stopped)
	}
}

func TestHandleInstanceNotFound(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/instance/999/start", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status code = %d, want 404", w.Code)
	}
}

func TestHandleInstanceConflictDuringTransition(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	// Start a full wake first
	orch.Wake()

	req := httptest.NewRequest("POST", "/api/instance/100/start", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status code = %d, want 409", w.Code)
	}
}

func TestHandleInstanceInvalidAction(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/instance/100/restart", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandleTierWake(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/tier/1/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", w.Code)
	}

	// Wait for background goroutine to call Start on the two tier-1 instances.
	for i := 0; i < 50; i++ {
		mock.mu.Lock()
		n := len(mock.started)
		mock.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.mu.Lock()
	started := append([]int(nil), mock.started...)
	mock.mu.Unlock()

	if len(started) != 2 {
		t.Fatalf("started = %v, want 2 instances", started)
	}
	// Tier 1 contains VMIDs 100 and 101; tier 2 VMID 200 must NOT be touched.
	for _, id := range started {
		if id == 200 {
			t.Errorf("started tier-2 instance %d during tier-1 wake", id)
		}
	}
}

func TestHandleTierSleep(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 101: "running", 200: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/tier/2/sleep", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status code = %d, want 202", w.Code)
	}

	for i := 0; i < 50; i++ {
		mock.mu.Lock()
		n := len(mock.stopped)
		mock.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.mu.Lock()
	stopped := append([]int(nil), mock.stopped...)
	mock.mu.Unlock()

	if len(stopped) != 1 || stopped[0] != 200 {
		t.Errorf("stopped = %v, want [200]", stopped)
	}
}

func TestHandleTierUnknown(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/tier/99/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("status code = %d, want 404", w.Code)
	}
}

func TestHandleTierConflictDuringMasterTransition(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	// Start a full wake first
	orch.Wake()

	req := httptest.NewRequest("POST", "/api/tier/1/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("status code = %d, want 409", w.Code)
	}
}

func TestHandleTierInvalidAction(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/tier/1/restart", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandleTierInvalidTierNumber(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("POST", "/api/tier/abc/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestHandleTierMethodNotAllowed(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("GET", "/api/tier/1/wake", nil)
	w := httptest.NewRecorder()
	orch.HandleTierAction(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", w.Code)
	}
}

func TestHandleInstanceMethodNotAllowed(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, mock)

	req := httptest.NewRequest("GET", "/api/instance/100/start", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", w.Code)
	}
}
