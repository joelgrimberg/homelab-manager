package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newSnoozeFixture(t *testing.T) (*SnoozeHandler, *SnoozeManager, *Scheduler, *Orchestrator) {
	t.Helper()
	dir := t.TempDir()
	sm, err := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	if err != nil {
		t.Fatalf("snooze: %v", err)
	}
	// Real orchestrator (state=asleep with no instances → state="asleep");
	// but we want "awake" so snooze isn't blocked. Easiest: empty
	// instance list yields state="asleep" because allStopped == allRunning
	// over empty set. Use mocked status by routing through a wrapper.
	// Instead, give it a single running instance so state = "awake".
	mock := newMockProxmox(map[int]string{100: "running"})
	instances := []Instance{{VMID: 100, Name: "vm", Type: "qemu", Tier: 1, Tags: []string{"infra"}}}
	tierNames := map[int]string{1: "infra"}
	orch := NewOrchestrator(instances, tierNames, nil, []string{"dns"}, nil, mock)

	sched := NewScheduler(orch, &fakeNotifier{}, sm)
	sched.Start([]ScheduleEntry{
		{Name: "night-sleep", Cron: "5 21 * * *", Action: "night_sleep", Notify: "sleep"},
	})
	return NewSnoozeHandler(sm, sched, orch), sm, sched, orch
}

func TestSnoozeHandlerPostpone(t *testing.T) {
	h, sm, _, _ := newSnoozeFixture(t)
	body, _ := json.Marshal(map[string]any{
		"name":          "night-sleep",
		"mode":          "postpone",
		"delay_minutes": 30,
	})
	req := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if _, ok := sm.Get("night-sleep"); !ok {
		t.Error("snooze entry not stored")
	}
}

func TestSnoozeHandlerSkipTonight(t *testing.T) {
	h, sm, _, _ := newSnoozeFixture(t)
	body, _ := json.Marshal(map[string]any{"name": "night-sleep", "mode": "skip_tonight"})
	req := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	s, ok := sm.Get("night-sleep")
	if !ok {
		t.Fatal("snooze entry not stored")
	}
	if !s.DeferredFireAt.IsZero() {
		t.Error("skip_tonight should not set DeferredFireAt")
	}
}

func TestSnoozeHandlerInvalidMode(t *testing.T) {
	h, _, _, _ := newSnoozeFixture(t)
	body, _ := json.Marshal(map[string]any{"name": "night-sleep", "mode": "snooze_forever"})
	req := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "mode") {
		t.Errorf("body should explain mode; got %q", w.Body.String())
	}
}

func TestSnoozeHandlerPostponeBadDelay(t *testing.T) {
	h, _, _, _ := newSnoozeFixture(t)
	body, _ := json.Marshal(map[string]any{
		"name":          "night-sleep",
		"mode":          "postpone",
		"delay_minutes": 0,
	})
	req := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSnoozeHandlerBlockedWhenAlreadyNight(t *testing.T) {
	// Build a fixture whose state is "night" (only exempt running).
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	mock := newMockProxmox(map[int]string{100: "running", 200: "stopped"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns"}},
		{VMID: 200, Name: "x", Type: "qemu", Tier: 2, Tags: []string{"other"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra", 2: "other"}, nil, []string{"dns"}, nil, mock)
	sched := NewScheduler(orch, &fakeNotifier{}, sm)
	sched.Start(nil)
	h := NewSnoozeHandler(sm, sched, orch)

	body, _ := json.Marshal(map[string]any{
		"name":          "night-sleep",
		"mode":          "postpone",
		"delay_minutes": 30,
	})
	req := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "night") {
		t.Errorf("body should mention night; got %q", w.Body.String())
	}
}

func TestSnoozeHandlerDeleteClears(t *testing.T) {
	h, sm, _, _ := newSnoozeFixture(t)
	body, _ := json.Marshal(map[string]any{"name": "night-sleep", "mode": "skip_tonight"})
	postReq := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	h.Handle(httptest.NewRecorder(), postReq)
	if _, ok := sm.Get("night-sleep"); !ok {
		t.Fatal("setup: snooze should be set")
	}

	delReq := httptest.NewRequest("DELETE", "/api/snooze?name=night-sleep", nil)
	w := httptest.NewRecorder()
	h.Handle(w, delReq)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if _, ok := sm.Get("night-sleep"); ok {
		t.Error("snooze should be cleared")
	}
}
