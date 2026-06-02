package main

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnoozeSkipSuppressesUntilExpiry(t *testing.T) {
	dir := t.TempDir()
	sm, err := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	if err != nil {
		t.Fatalf("init: %v", err)
	}

	until := time.Now().Add(50 * time.Millisecond)
	if err := sm.Skip("night-sleep", until); err != nil {
		t.Fatalf("Skip: %v", err)
	}
	if !sm.IsSuppressed("night-sleep") {
		t.Fatal("should be suppressed immediately after Skip")
	}
	time.Sleep(80 * time.Millisecond)
	if sm.IsSuppressed("night-sleep") {
		t.Error("should NOT be suppressed after expiry")
	}
}

func TestSnoozePostponeSchedulesCallback(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))

	var fired int32
	if err := sm.Postpone("night-sleep", 30*time.Millisecond, func() {
		atomic.AddInt32(&fired, 1)
	}); err != nil {
		t.Fatalf("Postpone: %v", err)
	}
	if !sm.IsSuppressed("night-sleep") {
		t.Fatal("postpone should suppress until grace window passes")
	}

	time.Sleep(80 * time.Millisecond)
	if atomic.LoadInt32(&fired) != 1 {
		t.Errorf("callback fired %d times, want 1", fired)
	}
}

func TestSnoozeClearCancelsTimer(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))

	var fired int32
	sm.Postpone("night-sleep", 30*time.Millisecond, func() {
		atomic.AddInt32(&fired, 1)
	})
	if err := sm.Clear("night-sleep"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if atomic.LoadInt32(&fired) != 0 {
		t.Errorf("callback fired %d times after Clear, want 0", fired)
	}
	if sm.IsSuppressed("night-sleep") {
		t.Error("Clear should remove suppression")
	}
}

func TestSnoozePersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snooze.json")
	sm1, _ := NewSnoozeManager(path)
	until := time.Now().Add(1 * time.Hour)
	sm1.Skip("night-sleep", until)

	sm2, err := NewSnoozeManager(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !sm2.IsSuppressed("night-sleep") {
		t.Error("reloaded snooze should still suppress")
	}
}

func TestSnoozePersistenceDropsExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snooze.json")
	sm1, _ := NewSnoozeManager(path)
	sm1.Skip("expired-entry", time.Now().Add(-1*time.Hour))

	sm2, _ := NewSnoozeManager(path)
	if _, ok := sm2.Get("expired-entry"); ok {
		t.Error("expired entry should be dropped on load")
	}
}

func TestSnoozeRearmDeferred(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "snooze.json")

	// Persist a state with a future DeferredFireAt directly.
	sm1, _ := NewSnoozeManager(path)
	sm1.mu.Lock()
	sm1.state["night-sleep"] = Snooze{
		SkipUntil:      time.Now().Add(1 * time.Hour),
		DeferredFireAt: time.Now().Add(30 * time.Millisecond),
	}
	sm1.persist()
	sm1.mu.Unlock()

	sm2, _ := NewSnoozeManager(path)
	var fired int32
	sm2.RearmDeferred(func(name string) {
		if name == "night-sleep" {
			atomic.AddInt32(&fired, 1)
		}
	})
	time.Sleep(80 * time.Millisecond)
	if atomic.LoadInt32(&fired) != 1 {
		t.Errorf("re-armed timer fired %d times, want 1", fired)
	}
}

func TestSnoozeIsSuppressedHonoredByScheduler(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	sm.Skip("ns", time.Now().Add(1*time.Hour))

	orch := &fakeOrch{}
	noti := &fakeNotifier{}
	s := NewScheduler(orch, noti, sm)

	s.run(ScheduleEntry{Name: "ns", Cron: "5 21 * * *", Action: "night_sleep", Notify: "x"})
	if atomic.LoadInt32(&orch.nightSleeps) != 0 {
		t.Error("action should not fire while suppressed")
	}
	if len(noti.snapshot()) != 0 {
		t.Error("notify should not fire while suppressed")
	}
}

func TestPostponeWithWarningFiresBoth(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))

	var runFired, warnFired int32
	var warnAt, runAt time.Time
	start := time.Now()

	err := sm.PostponeWithWarning("ns", 100*time.Millisecond, 60*time.Millisecond,
		func() { runAt = time.Now(); atomic.AddInt32(&runFired, 1) },
		func() { warnAt = time.Now(); atomic.AddInt32(&warnFired, 1) },
	)
	if err != nil {
		t.Fatalf("Postpone: %v", err)
	}
	time.Sleep(180 * time.Millisecond)

	if atomic.LoadInt32(&warnFired) != 1 {
		t.Errorf("warn fired %d times, want 1", warnFired)
	}
	if atomic.LoadInt32(&runFired) != 1 {
		t.Errorf("run fired %d times, want 1", runFired)
	}
	warnGap := warnAt.Sub(start)
	if warnGap < 30*time.Millisecond || warnGap > 70*time.Millisecond {
		t.Errorf("warn fired at +%s, want ~40ms (delay-warnBefore)", warnGap)
	}
	if runAt.Sub(warnAt) < 30*time.Millisecond {
		t.Errorf("run should fire after warn; warn=%s run=%s", warnAt, runAt)
	}
}

func TestPostponeWithWarningClearCancelsBoth(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))

	var runFired, warnFired int32
	sm.PostponeWithWarning("ns", 60*time.Millisecond, 30*time.Millisecond,
		func() { atomic.AddInt32(&runFired, 1) },
		func() { atomic.AddInt32(&warnFired, 1) },
	)
	if err := sm.Clear("ns"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if atomic.LoadInt32(&runFired) != 0 || atomic.LoadInt32(&warnFired) != 0 {
		t.Errorf("after Clear: run=%d warn=%d, want 0/0", runFired, warnFired)
	}
}

func TestSchedulerRunOnceBypassesSnooze(t *testing.T) {
	dir := t.TempDir()
	sm, _ := NewSnoozeManager(filepath.Join(dir, "snooze.json"))
	sm.Skip("ns", time.Now().Add(1*time.Hour))

	orch := &fakeOrch{}
	noti := &fakeNotifier{}
	s := NewScheduler(orch, noti, sm)

	// Register the entry so RunOnce can find it.
	s.Start([]ScheduleEntry{{Name: "ns", Cron: "5 21 * * *", Action: "night_sleep", Notify: "x"}})
	s.RunOnce("ns")

	if atomic.LoadInt32(&orch.nightSleeps) != 1 {
		t.Errorf("RunOnce should fire action despite snooze; got nightSleeps=%d", orch.nightSleeps)
	}
}
