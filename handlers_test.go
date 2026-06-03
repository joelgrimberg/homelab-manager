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
	mu        sync.Mutex
	statuses  map[int]string // vmid -> status
	started   []int
	stopped   []int
	vms       []ProxmoxInstance
	lxcs      []ProxmoxInstance
	startHook func(Instance) // optional: scripts state changes per call
	stopHook  func(Instance)
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
	if m.startHook != nil {
		m.startHook(inst)
	} else {
		m.statuses[inst.VMID] = "running"
	}
	return nil
}

func (m *mockProxmox) Stop(inst Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = append(m.stopped, inst.VMID)
	if m.stopHook != nil {
		m.stopHook(inst)
	} else {
		m.statuses[inst.VMID] = "stopped"
	}
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
			got := computeState(tiers, tt.transitioning, nil)
			if got != tt.want {
				t.Errorf("computeState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleStatusAllRunning(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 101: "running", 200: "running"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	instances, tierNames, err := DiscoverInstances(mock, nil, tierDefs)
	if err != nil {
		t.Fatalf("initial discovery: %v", err)
	}
	orch := NewOrchestrator(instances, tierNames, tierDefs, nil, nil, mock)

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
	instances, tierNames, _ := DiscoverInstances(mock, nil, tierDefs)
	orch := NewOrchestrator(instances, tierNames, tierDefs, nil, nil, mock)

	if started, _ := orch.SleepTier(1); !started {
		t.Fatal("SleepTier(1) returned false")
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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

	req := httptest.NewRequest("POST", "/api/status", nil)
	w := httptest.NewRecorder()
	orch.HandleStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", w.Code)
	}
}

func TestHandleInstanceStart(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 101: "stopped", 200: "stopped"})
	instances, tierNames := testInstances()
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

	// Start a tier transition to put the orchestrator into transitioning state
	orch.WakeTier(1)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

	// Start a tier transition to put the orchestrator into transitioning state
	orch.WakeTier(2)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

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
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

	req := httptest.NewRequest("GET", "/api/instance/100/start", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status code = %d, want 405", w.Code)
	}
}

// --- Night mode ---

// nightTestInstances returns a fixture with two keep-awake tags ("dns" and
// "homelab") and a mix of exempt + non-exempt instances spread across tiers.
func nightTestInstances() ([]Instance, map[int]string) {
	instances := []Instance{
		// Tier 1: dns is exempt, rest of infra is not.
		{VMID: 300, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns", "infra"}},
		{VMID: 301, Name: "openbao", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
		// Tier 2: nothing exempt.
		{VMID: 100, Name: "omni-cp-a", Type: "qemu", Tier: 2, Tags: []string{"local-omni"}},
		// Tier 3: all exempt (homelab).
		{VMID: 120, Name: "k8s-worker-a", Type: "qemu", Tier: 3, Tags: []string{"homelab", "homelab-worker"}},
		{VMID: 122, Name: "k8s-cp-a", Type: "qemu", Tier: 3, Tags: []string{"homelab", "homelab-cp"}},
	}
	tierNames := map[int]string{1: "infra", 2: "local-omni", 3: "homelab"}
	return instances, tierNames
}

func TestNightSleepSkipsExempt(t *testing.T) {
	mock := newMockProxmox(map[int]string{
		300: "running", 301: "running", 100: "running", 120: "running", 122: "running",
	})
	instances, tierNames := nightTestInstances()
	orch := NewOrchestrator(instances, tierNames, nil, []string{"dns", "homelab"}, nil, mock)
	orch.wakeTierDelay = 0
	orch.sleepTierDelay = 0

	started, unconfigured := orch.NightSleep()
	if unconfigured {
		t.Fatal("NightSleep returned unconfigured=true with keepAwakeTags set")
	}
	if !started {
		t.Fatal("NightSleep returned started=false")
	}

	// Wait for the background goroutine to finish.
	for i := 0; i < 100; i++ {
		if !orch.isTransitioning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if orch.isTransitioning() {
		t.Fatal("orchestrator still transitioning after 1s")
	}

	mock.mu.Lock()
	stopped := append([]int(nil), mock.stopped...)
	mock.mu.Unlock()

	// Non-exempt 301 (openbao) and 100 (omni) must be stopped; exempt 300, 120, 122 must NOT be.
	stoppedSet := map[int]bool{}
	for _, id := range stopped {
		stoppedSet[id] = true
	}
	if !stoppedSet[301] || !stoppedSet[100] {
		t.Errorf("non-exempt VMs not stopped: stopped=%v", stopped)
	}
	for _, exempt := range []int{300, 120, 122} {
		if stoppedSet[exempt] {
			t.Errorf("exempt VM %d was stopped during NightSleep", exempt)
		}
	}
}

func TestNightSleepStartsExemptIfDown(t *testing.T) {
	// Everything starts asleep — entering night mode should start the exempt VMs.
	mock := newMockProxmox(map[int]string{
		300: "stopped", 301: "stopped", 100: "stopped", 120: "stopped", 122: "stopped",
	})
	instances, tierNames := nightTestInstances()
	orch := NewOrchestrator(instances, tierNames, nil, []string{"dns", "homelab"}, nil, mock)
	orch.wakeTierDelay = 0
	orch.sleepTierDelay = 0

	if _, unconf := orch.NightSleep(); unconf {
		t.Fatal("unexpected unconfigured")
	}

	for i := 0; i < 100; i++ {
		if !orch.isTransitioning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.mu.Lock()
	started := append([]int(nil), mock.started...)
	mock.mu.Unlock()

	startedSet := map[int]bool{}
	for _, id := range started {
		startedSet[id] = true
	}
	for _, exempt := range []int{300, 120, 122} {
		if !startedSet[exempt] {
			t.Errorf("exempt VM %d not started by NightSleep from all-asleep state", exempt)
		}
	}
	if startedSet[301] || startedSet[100] {
		t.Errorf("non-exempt VM was started during NightSleep: started=%v", started)
	}
}

func TestNightSleepUnconfigured(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := nightTestInstances()
	orch := NewOrchestrator(instances, tierNames, nil, nil, nil, mock)

	req := httptest.NewRequest("POST", "/api/night/sleep", nil)
	w := httptest.NewRecorder()
	orch.HandleNightAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}

func TestNightWakeStartsOnlyNonExempt(t *testing.T) {
	// Coming out of night: exempt already running, non-exempt stopped.
	mock := newMockProxmox(map[int]string{
		300: "running", 301: "stopped", 100: "stopped", 120: "running", 122: "running",
	})
	instances, tierNames := nightTestInstances()
	orch := NewOrchestrator(instances, tierNames, nil, []string{"dns", "homelab"}, nil, mock)
	orch.wakeTierDelay = 0
	orch.sleepTierDelay = 0

	if _, unconf := orch.NightWake(); unconf {
		t.Fatal("unexpected unconfigured")
	}

	for i := 0; i < 100; i++ {
		if !orch.isTransitioning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.mu.Lock()
	started := append([]int(nil), mock.started...)
	mock.mu.Unlock()

	startedSet := map[int]bool{}
	for _, id := range started {
		startedSet[id] = true
	}
	if !startedSet[301] || !startedSet[100] {
		t.Errorf("non-exempt VMs not started: started=%v", started)
	}
	for _, exempt := range []int{300, 120, 122} {
		if startedSet[exempt] {
			t.Errorf("exempt VM %d touched during NightWake", exempt)
		}
	}
}

func TestComputeStateNight(t *testing.T) {
	tiers := []TierStatus{
		{Tier: 1, Instances: []InstanceStatus{
			{VMID: 300, Status: "running"}, // exempt
			{VMID: 301, Status: "stopped"}, // non-exempt
		}},
		{Tier: 3, Instances: []InstanceStatus{
			{VMID: 120, Status: "running"}, // exempt
		}},
	}
	exempt := map[int]bool{300: true, 120: true}
	got := computeState(tiers, false, exempt)
	if got != "night" {
		t.Errorf("computeState = %q, want night", got)
	}
}

// --- stragglers / verifySubset ---

func TestVerifySubsetRetriesStragglers(t *testing.T) {
	// VMID 200 starts in "stopped" — simulating a VM that the main
	// transition pass didn't reach. The Start call (default mock
	// behavior) will set it to "running", and verifySubset should detect
	// the laggard, re-issue Start, and find it at target.
	mock := newMockProxmox(map[int]string{100: "running", 200: "stopped"})

	instances := []Instance{
		{VMID: 100, Name: "a", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
		{VMID: 200, Name: "b", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra"}, nil, nil, nil, mock)
	orch.verifyTimeout = 200 * time.Millisecond
	orch.pollInterval = 20 * time.Millisecond

	stuck := orch.verifySubset(instances, "running")
	if len(stuck) != 0 {
		t.Errorf("expected no stuck after retry, got %v", stuck)
	}

	// Start should have been called for the laggard (200) only — 100 was
	// already at target so verify skipped it.
	mock.mu.Lock()
	started := append([]int(nil), mock.started...)
	mock.mu.Unlock()
	saw200 := 0
	for _, id := range started {
		if id == 200 {
			saw200++
		}
	}
	if saw200 < 1 {
		t.Errorf("verifySubset never re-issued Start for VMID 200; started=%v", started)
	}
}

func TestVerifySubsetSurfacesStuck(t *testing.T) {
	// Mock that never advances VMID 200 to "running" regardless of Start
	// calls. The verifySubset retry should give up and return 200.
	mock := newMockProxmox(map[int]string{200: "stopped"})
	mock.startHook = func(inst Instance) {
		// no-op — leave it stopped forever
	}

	instances := []Instance{
		{VMID: 200, Name: "b", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra"}, nil, nil, nil, mock)
	orch.verifyTimeout = 100 * time.Millisecond
	orch.pollInterval = 20 * time.Millisecond

	stuck := orch.verifySubset(instances, "running")
	if len(stuck) != 1 || stuck[0] != 200 {
		t.Errorf("stuck = %v, want [200]", stuck)
	}

	orch.recordStuck(stuck)
	resp := orch.Status()
	found := false
	for _, tier := range resp.Tiers {
		for _, inst := range tier.Instances {
			if inst.VMID == 200 {
				found = true
				if !inst.Stuck {
					t.Errorf("VMID 200 Status() Stuck=false, want true")
				}
			}
		}
	}
	if !found {
		t.Error("VMID 200 missing from Status response")
	}
}

func TestNightSleepKicksStragglers(t *testing.T) {
	// Two non-exempt VMs. The Stop call leaves VMID 200 in "running"
	// initially; the verify retry kicks it and then it goes to "stopped".
	mock := newMockProxmox(map[int]string{100: "running", 200: "running"})
	stopCalls := map[int]int{}
	mock.stopHook = func(inst Instance) {
		stopCalls[inst.VMID]++
		if inst.VMID == 100 {
			mock.statuses[100] = "stopped"
		} else if inst.VMID == 200 && stopCalls[200] >= 2 {
			mock.statuses[200] = "stopped"
		}
	}

	instances := []Instance{
		{VMID: 100, Name: "a", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
		{VMID: 200, Name: "b", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra"}, nil, []string{"dns"}, nil, mock)
	orch.wakeTierDelay = 0
	orch.sleepTierDelay = 0
	orch.waitTimeout = 100 * time.Millisecond
	orch.verifyTimeout = 200 * time.Millisecond
	orch.pollInterval = 20 * time.Millisecond

	if _, unconf := orch.NightSleep(); unconf {
		t.Fatal("unexpected unconfigured")
	}
	// Wait for transition to complete
	for i := 0; i < 500; i++ {
		if !orch.isTransitioning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if orch.isTransitioning() {
		t.Fatal("still transitioning after 10s")
	}

	if mock.statuses[200] != "stopped" {
		t.Errorf("VMID 200 = %q, want stopped (verify retry should have kicked it)", mock.statuses[200])
	}
	if stopCalls[200] < 2 {
		t.Errorf("Stop on VMID 200 called %d times, want at least 2", stopCalls[200])
	}
	stuck := *orch.stuck.Load()
	if stuck[200] {
		t.Error("VMID 200 should not be in stuck set after successful retry")
	}
}

// --- never_touch ---

func TestNeverTouchExcludedFromTierSleep(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 200: "running"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns", "infra"}},
		{VMID: 200, Name: "x", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra"}, nil, nil, []string{"dns"}, mock)
	orch.wakeTierDelay = 0
	orch.sleepTierDelay = 0

	if started, _ := orch.SleepTier(1); !started {
		t.Fatal("SleepTier(1) returned false")
	}
	for i := 0; i < 100; i++ {
		if !orch.isTransitioning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.mu.Lock()
	stopped := append([]int(nil), mock.stopped...)
	mock.mu.Unlock()
	for _, id := range stopped {
		if id == 100 {
			t.Error("never_touch instance 100 was stopped by SleepTier")
		}
	}
	if len(stopped) != 1 || stopped[0] != 200 {
		t.Errorf("stopped = %v, want [200]", stopped)
	}
}

func TestNeverTouchExcludedFromNightSleep(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "stopped", 200: "stopped", 300: "stopped"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns", "infra"}},
		{VMID: 200, Name: "infra-other", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
		{VMID: 300, Name: "k8s", Type: "qemu", Tier: 2, Tags: []string{"homelab"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra", 2: "homelab"}, nil,
		[]string{"dns", "homelab"}, []string{"dns"}, mock)
	orch.wakeTierDelay = 0
	orch.sleepTierDelay = 0

	if _, unconf := orch.NightSleep(); unconf {
		t.Fatal("unexpected unconfigured")
	}
	for i := 0; i < 100; i++ {
		if !orch.isTransitioning() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mock.mu.Lock()
	started := append([]int(nil), mock.started...)
	stopped := append([]int(nil), mock.stopped...)
	mock.mu.Unlock()

	for _, id := range started {
		if id == 100 {
			t.Error("never_touch instance 100 was started by NightSleep")
		}
	}
	for _, id := range stopped {
		if id == 100 {
			t.Error("never_touch instance 100 was stopped by NightSleep")
		}
	}
	// 300 (homelab) is exempt → started; 200 (non-exempt) → stopped
	startedSet := map[int]bool{}
	for _, id := range started {
		startedSet[id] = true
	}
	stoppedSet := map[int]bool{}
	for _, id := range stopped {
		stoppedSet[id] = true
	}
	if !startedSet[300] {
		t.Error("exempt k8s 300 should have been started")
	}
	if !stoppedSet[200] {
		t.Error("non-exempt 200 should have been stopped")
	}
}

func TestNeverTouchInstanceActionRefused(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns", "infra"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra"}, nil, nil, []string{"dns"}, mock)

	req := httptest.NewRequest("POST", "/api/instance/100/stop", nil)
	w := httptest.NewRecorder()
	orch.HandleInstanceAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestStatusReportsProtected(t *testing.T) {
	mock := newMockProxmox(map[int]string{100: "running", 200: "running"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns", "infra"}},
		{VMID: 200, Name: "x", Type: "qemu", Tier: 1, Tags: []string{"infra"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra"}, nil, nil, []string{"dns"}, mock)

	resp := orch.Status()
	if len(resp.Tiers) != 1 || len(resp.Tiers[0].Instances) != 2 {
		t.Fatalf("unexpected tiers: %+v", resp.Tiers)
	}
	gotProtected := map[int]bool{}
	for _, inst := range resp.Tiers[0].Instances {
		gotProtected[inst.VMID] = inst.Protected
	}
	if !gotProtected[100] {
		t.Error("VMID 100 should be Protected=true")
	}
	if gotProtected[200] {
		t.Error("VMID 200 should be Protected=false")
	}
}

func TestHandleNightActionInvalidAction(t *testing.T) {
	mock := newMockProxmox(map[int]string{})
	instances, tierNames := nightTestInstances()
	orch := NewOrchestrator(instances, tierNames, nil, []string{"dns"}, nil, mock)

	req := httptest.NewRequest("POST", "/api/night/restart", nil)
	w := httptest.NewRecorder()
	orch.HandleNightAction(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status code = %d, want 400", w.Code)
	}
}
