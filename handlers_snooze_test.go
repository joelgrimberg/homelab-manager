package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	}, nil)
	return NewSnoozeHandler(sm, sched, orch, nil), sm, sched, orch
}

// TestSnoozeHandlerPostponeStacksOnExistingSnooze: a second postpone tap
// while a snooze is already active should ADD delay to the current
// deferred fire, not reset to "delay from now". I.e. +30 then +30 = +60.
func TestSnoozeHandlerPostponeStacksOnExistingSnooze(t *testing.T) {
	h, sm, _, _ := newSnoozeFixture(t)

	// First tap: with no active snooze, base = next cron fire (today's
	// or tomorrow's 21:05) so we use a small delay to keep the test
	// fast without needing to wait for the timer to fire.
	body, _ := json.Marshal(map[string]any{
		"name":          "night-sleep",
		"mode":          "postpone",
		"delay_minutes": 30,
	})
	req := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body))
	h.Handle(httptest.NewRecorder(), req)

	first, ok := sm.Get("night-sleep")
	if !ok {
		t.Fatal("first postpone didn't store snooze")
	}

	// Second tap: now base = first.DeferredFireAt, so the new deferred
	// fire should be ~30 min later than the first.
	body2, _ := json.Marshal(map[string]any{
		"name":          "night-sleep",
		"mode":          "postpone",
		"delay_minutes": 30,
	})
	req2 := httptest.NewRequest("POST", "/api/snooze", bytes.NewReader(body2))
	h.Handle(httptest.NewRecorder(), req2)

	second, ok := sm.Get("night-sleep")
	if !ok {
		t.Fatal("second postpone didn't store snooze")
	}

	gap := second.DeferredFireAt.Sub(first.DeferredFireAt)
	if gap < 29*time.Minute || gap > 31*time.Minute {
		t.Errorf("second tap should add ~30m on top of first; gap = %s", gap)
	}
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
	sched.Start(nil, nil)
	h := NewSnoozeHandler(sm, sched, orch, nil)

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

// TestSnoozeHandlerCancelFiresWhenCronAlreadyPassed: when the user cancels
// a postponement whose original recurring cron has already fired today
// (i.e. NextFires returns tomorrow), Cancel should fire RunOnce so the
// system never silently behaves like "skip tonight." We force this by
// directly setting DeferredFireAt into the past and using an hourly cron.
func TestSnoozeHandlerCancelFiresWhenCronAlreadyPassed(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	mock := newMockProxmox(map[int]string{100: "running", 200: "running"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns"}},
		{VMID: 200, Name: "vm", Type: "qemu", Tier: 2, Tags: []string{"other"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra", 2: "other"}, nil, []string{"dns"}, nil, mock)
	orch.sleepTierDelay = 0
	orch.waitTimeout = 300 * time.Millisecond
	orch.verifyTimeout = 100 * time.Millisecond
	orch.pollInterval = 10 * time.Millisecond

	sched := NewScheduler(orch, &fakeNotifier{}, sm)
	sched.Start([]ScheduleEntry{
		{Name: "night-sleep", Cron: "0 * * * *", Action: "night_sleep", Notify: "sleep"},
	}, nil)
	h := NewSnoozeHandler(sm, sched, orch, nil)

	// Inject a snooze whose deferred fire is already in the past — i.e.
	// today's recurring fire has effectively passed.
	sm.mu.Lock()
	sm.state["night-sleep"] = Snooze{
		SkipUntil:      time.Now().Add(1 * time.Hour),
		DeferredFireAt: time.Now().Add(-1 * time.Minute),
	}
	sm.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/snooze?name=night-sleep", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}

	// RunOnce runs in a goroutine — wait for non-exempt Stop to be observed.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mock.mu.Lock()
		n := len(mock.stopped)
		mock.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("Cancel after cron passed should fire RunOnce; no Stop observed")
}

// TestSnoozeHandlerCancelDoesNotFireWhenCronStillAhead: when the recurring
// cron is still going to fire later today, Cancel should NOT fire RunOnce.
// The cron will handle the sleep at its regular time.
func TestSnoozeHandlerCancelDoesNotFireWhenCronStillAhead(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	mock := newMockProxmox(map[int]string{100: "running", 200: "running"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns"}},
		{VMID: 200, Name: "vm", Type: "qemu", Tier: 2, Tags: []string{"other"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra", 2: "other"}, nil, []string{"dns"}, nil, mock)
	sched := NewScheduler(orch, &fakeNotifier{}, sm)
	// Every-minute cron: next fire is at most 60s away. Deferred fire is
	// well beyond that, so next < deferred → cron will handle today.
	sched.Start([]ScheduleEntry{
		{Name: "night-sleep", Cron: "* * * * *", Action: "night_sleep", Notify: "sleep"},
	}, nil)
	h := NewSnoozeHandler(sm, sched, orch, nil)

	sm.mu.Lock()
	sm.state["night-sleep"] = Snooze{
		SkipUntil:      time.Now().Add(10 * time.Minute),
		DeferredFireAt: time.Now().Add(5 * time.Minute),
	}
	sm.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/snooze?name=night-sleep", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	// Give the goroutine a moment in case it ran anyway.
	time.Sleep(150 * time.Millisecond)
	mock.mu.Lock()
	n := len(mock.stopped)
	mock.mu.Unlock()
	if n != 0 {
		t.Errorf("Cancel before cron fire should not trigger sleep; got %d stops", n)
	}
}

// TestSnoozeHandlerCancelSkipTonightDoesNotFire: cancelling a skip_tonight
// snooze (DeferredFireAt zero) is an explicit "undo skip" — must not
// trigger an immediate sleep.
func TestSnoozeHandlerCancelSkipTonightDoesNotFire(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	mock := newMockProxmox(map[int]string{100: "running", 200: "running"})
	instances := []Instance{
		{VMID: 100, Name: "dns", Type: "qemu", Tier: 1, Tags: []string{"dns"}},
		{VMID: 200, Name: "vm", Type: "qemu", Tier: 2, Tags: []string{"other"}},
	}
	orch := NewOrchestrator(instances, map[int]string{1: "infra", 2: "other"}, nil, []string{"dns"}, nil, mock)
	sched := NewScheduler(orch, &fakeNotifier{}, sm)
	sched.Start([]ScheduleEntry{
		{Name: "night-sleep", Cron: "0 * * * *", Action: "night_sleep", Notify: "sleep"},
	}, nil)
	h := NewSnoozeHandler(sm, sched, orch, nil)

	// Skip tonight: SkipUntil set, DeferredFireAt zero. The cron has
	// already passed (we don't care here — the explicit skip means the
	// user *wants* to stay awake), so Cancel must NOT silently fire.
	sm.mu.Lock()
	sm.state["night-sleep"] = Snooze{
		SkipUntil: time.Now().Add(8 * time.Hour),
	}
	sm.mu.Unlock()

	req := httptest.NewRequest("DELETE", "/api/snooze?name=night-sleep", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	time.Sleep(150 * time.Millisecond)
	mock.mu.Lock()
	n := len(mock.stopped)
	mock.mu.Unlock()
	if n != 0 {
		t.Errorf("Cancel of skip_tonight should not trigger sleep; got %d stops", n)
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
