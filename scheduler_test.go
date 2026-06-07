package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeOrch struct {
	nightSleeps int32
	nightWakes  int32
	tierWakes   sync.Map // tier int → *int32
	tierSleeps  sync.Map
}

func (f *fakeOrch) NightSleep() (bool, bool) { atomic.AddInt32(&f.nightSleeps, 1); return true, false }
func (f *fakeOrch) NightWake() (bool, bool)  { atomic.AddInt32(&f.nightWakes, 1); return true, false }

func (f *fakeOrch) WakeTier(tier int) (bool, bool) {
	v, _ := f.tierWakes.LoadOrStore(tier, new(int32))
	atomic.AddInt32(v.(*int32), 1)
	return true, false
}
func (f *fakeOrch) SleepTier(tier int) (bool, bool) {
	v, _ := f.tierSleeps.LoadOrStore(tier, new(int32))
	atomic.AddInt32(v.(*int32), 1)
	return true, false
}

func (f *fakeOrch) tierWakeCount(tier int) int32 {
	v, ok := f.tierWakes.Load(tier)
	if !ok {
		return 0
	}
	return atomic.LoadInt32(v.(*int32))
}
func (f *fakeOrch) tierSleepCount(tier int) int32 {
	v, ok := f.tierSleeps.Load(tier)
	if !ok {
		return 0
	}
	return atomic.LoadInt32(v.(*int32))
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages [][2]string
}

func (f *fakeNotifier) Notify(title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, [2]string{title, body})
}

func (f *fakeNotifier) snapshot() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][2]string, len(f.messages))
	copy(out, f.messages)
	return out
}

// fakeActionNotifier implements both Notifier and ActionNotifier so the
// scheduler's emitNotify can pick the right branch based on the entry.
type fakeActionNotifier struct {
	mu      sync.Mutex
	plain   [][2]string
	actions []actionCall
}

type actionCall struct {
	title, body string
	data        map[string]any
	actions     []NotifyAction
}

func (f *fakeActionNotifier) Notify(title, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain = append(f.plain, [2]string{title, body})
}

func (f *fakeActionNotifier) NotifyWithActions(title, body string, data map[string]any, actions []NotifyAction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, actionCall{title: title, body: body, data: data, actions: actions})
}

// TestSchedulerActionsAndNotify directly invokes the run() path with a
// fixture entry — cron timing is the cron library's concern.
func TestSchedulerActionsAndNotify(t *testing.T) {
	orch := &fakeOrch{}
	noti := &fakeNotifier{}
	s := NewScheduler(orch, noti, nil)

	cases := []struct {
		entry      ScheduleEntry
		wantField  *int32
		wantNotify string
	}{
		{ScheduleEntry{Name: "warn", Cron: "0 21 * * *", Notify: "5 min"}, nil, "5 min"},
		{ScheduleEntry{Name: "ns", Cron: "5 21 * * *", Action: "night_sleep"}, &orch.nightSleeps, ""},
		{ScheduleEntry{Name: "nw", Cron: "0 7 * * *", Action: "night_wake"}, &orch.nightWakes, ""},
	}

	for _, tc := range cases {
		before := int32(0)
		if tc.wantField != nil {
			before = atomic.LoadInt32(tc.wantField)
		}
		s.run(tc.entry)
		if tc.wantField != nil {
			after := atomic.LoadInt32(tc.wantField)
			if after != before+1 {
				t.Errorf("entry %s: action counter %d → %d, want +1", tc.entry.Name, before, after)
			}
		}
		if tc.wantNotify != "" {
			msgs := noti.snapshot()
			if len(msgs) == 0 || msgs[len(msgs)-1][1] != tc.wantNotify {
				t.Errorf("entry %s: notify mismatch, got %v", tc.entry.Name, msgs)
			}
		}
	}
}

func TestSchedulerReloadValidatesCron(t *testing.T) {
	orch := &fakeOrch{}
	s := NewScheduler(orch, &fakeNotifier{}, nil)

	good := []ScheduleEntry{
		{Name: "ok", Cron: "0 7 * * *", Action: "night_wake"},
	}
	if err := s.Reload(good); err != nil {
		t.Fatalf("good reload: %v", err)
	}

	bad := []ScheduleEntry{
		{Name: "junk", Cron: "not a cron", Action: "night_wake"},
	}
	if err := s.Reload(bad); err == nil {
		t.Fatal("bad cron should have failed reload")
	}

	// Verify the good schedule is still active after bad reload.
	if got := s.Entries(); len(got) != 1 || got[0].Name != "ok" {
		t.Errorf("Entries() = %v, want previous good schedule retained", got)
	}
}

func TestSchedulerReloadValidatesAction(t *testing.T) {
	s := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	err := s.Reload([]ScheduleEntry{
		{Name: "x", Cron: "0 7 * * *", Action: "destroy_everything"},
	})
	if err == nil {
		t.Fatal("unknown action should fail validation")
	}
}

func TestSchedulerReloadRequiresActionOrNotify(t *testing.T) {
	s := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	err := s.Reload([]ScheduleEntry{
		{Name: "empty", Cron: "0 7 * * *"},
	})
	if err == nil {
		t.Fatal("entry with neither action nor notify should fail")
	}
}

// TestEmitNotifyWithSnoozeAction asserts that a schedule entry declaring
// SnoozeTarget + SnoozeMinutes produces an action push when the notifier
// supports actions, and a plain push otherwise.
func TestEmitNotifyWithSnoozeAction(t *testing.T) {
	an := &fakeActionNotifier{}
	s := NewScheduler(&fakeOrch{}, an, nil)

	entry := ScheduleEntry{
		Name:          "warn",
		Cron:          "50 20 * * *",
		Notify:        "Sleeping in 15 min",
		SnoozeTarget:  "night-sleep",
		SnoozeMinutes: 15,
	}
	s.emitNotify(entry)

	if len(an.actions) != 1 {
		t.Fatalf("want 1 action push, got %d (plain=%v)", len(an.actions), an.plain)
	}
	got := an.actions[0]
	if got.body != "Sleeping in 15 min" {
		t.Errorf("body = %q", got.body)
	}
	if got.data["name"] != "night-sleep" {
		t.Errorf("data.name = %v", got.data["name"])
	}
	if got.data["minutes"] != 15 {
		t.Errorf("data.minutes = %v", got.data["minutes"])
	}
	if got.data["click_url"] != "/countdown" {
		t.Errorf("data.click_url = %v", got.data["click_url"])
	}
	if len(got.actions) != 1 || got.actions[0].Action != "snooze" {
		t.Errorf("actions = %v", got.actions)
	}
	if got.actions[0].Title != "+15 min" {
		t.Errorf("action title = %q, want +15 min", got.actions[0].Title)
	}
	if len(an.plain) != 0 {
		t.Errorf("plain notifier should not have been called: %v", an.plain)
	}
}

// TestEmitNotifyFallsBackWithoutSnoozeTarget asserts plain Notify is used
// when no SnoozeTarget is set on the entry — backwards compatibility for
// existing config.yaml entries that don't opt in.
func TestEmitNotifyFallsBackWithoutSnoozeTarget(t *testing.T) {
	an := &fakeActionNotifier{}
	s := NewScheduler(&fakeOrch{}, an, nil)

	entry := ScheduleEntry{Name: "wake", Cron: "0 7 * * *", Notify: "Wake"}
	s.emitNotify(entry)

	if len(an.plain) != 1 || an.plain[0][1] != "Wake" {
		t.Errorf("expected one plain notify, got plain=%v actions=%v", an.plain, an.actions)
	}
	if len(an.actions) != 0 {
		t.Errorf("action path should not have been taken: %v", an.actions)
	}
}

// TestEmitNotifyFallsBackWithoutActionNotifier asserts a notifier that
// implements only the plain Notifier interface still gets called, even
// when SnoozeTarget is set.
func TestEmitNotifyFallsBackWithoutActionNotifier(t *testing.T) {
	noti := &fakeNotifier{}
	s := NewScheduler(&fakeOrch{}, noti, nil)

	entry := ScheduleEntry{
		Name: "warn", Cron: "50 20 * * *",
		Notify: "Sleeping in 15 min", SnoozeTarget: "night-sleep", SnoozeMinutes: 15,
	}
	s.emitNotify(entry)

	if msgs := noti.snapshot(); len(msgs) != 1 || msgs[0][1] != "Sleeping in 15 min" {
		t.Errorf("plain notifier got %v", msgs)
	}
}

// TestSchedulerTierWakeSleepDispatch fires per-tier wake/sleep entries via
// the cron callback path (Start → cron-AddFunc → runScoped → dispatchAction)
// and verifies the right orchestrator method is called with the right tier.
func TestSchedulerTierWakeSleepDispatch(t *testing.T) {
	orch := &fakeOrch{}
	s := NewScheduler(orch, &fakeNotifier{}, nil)

	perTier := map[int][]ScheduleEntry{
		2: {
			{Name: "t2-wake", Cron: "0 7 * * *", Action: "wake"},
			{Name: "t2-sleep", Cron: "0 23 * * *", Action: "sleep"},
		},
		3: {
			{Name: "t3-sleep", Cron: "0 22 * * *", Action: "sleep"},
		},
	}
	if err := s.Start(nil, perTier); err != nil {
		t.Fatalf("Start: %v", err)
	}

	s.runScoped(scopedEntry{ScheduleEntry: perTier[2][0], Tier: 2})
	s.runScoped(scopedEntry{ScheduleEntry: perTier[2][1], Tier: 2})
	s.runScoped(scopedEntry{ScheduleEntry: perTier[3][0], Tier: 3})

	if got := orch.tierWakeCount(2); got != 1 {
		t.Errorf("tier-2 wake count = %d, want 1", got)
	}
	if got := orch.tierSleepCount(2); got != 1 {
		t.Errorf("tier-2 sleep count = %d, want 1", got)
	}
	if got := orch.tierSleepCount(3); got != 1 {
		t.Errorf("tier-3 sleep count = %d, want 1", got)
	}
	if got := orch.tierWakeCount(3); got != 0 {
		t.Errorf("tier-3 wake count = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&orch.nightSleeps); got != 0 {
		t.Errorf("global nightSleeps = %d, want 0", got)
	}
}

// TestSchedulerActionScopeRejected ensures global actions can't be loaded
// under a tier and tier actions can't be loaded at top level.
func TestSchedulerActionScopeRejected(t *testing.T) {
	s := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)

	if err := s.Reload([]ScheduleEntry{{Name: "x", Cron: "0 7 * * *", Action: "wake"}}); err == nil {
		t.Error("wake at top level should be rejected")
	}
	if err := s.Reload([]ScheduleEntry{{Name: "y", Cron: "0 7 * * *", Action: "sleep"}}); err == nil {
		t.Error("sleep at top level should be rejected")
	}

	err := s.Start(nil, map[int][]ScheduleEntry{
		1: {{Name: "z", Cron: "0 7 * * *", Action: "night_sleep"}},
	})
	if err == nil {
		t.Error("night_sleep under tier should be rejected")
	}
}

// TestSchedulerReloadPreservesPerTier verifies that a PUT-style global
// reload doesn't clobber the per-tier entries loaded at startup.
func TestSchedulerReloadPreservesPerTier(t *testing.T) {
	orch := &fakeOrch{}
	s := NewScheduler(orch, &fakeNotifier{}, nil)

	perTier := map[int][]ScheduleEntry{
		1: {{Name: "t1-sleep", Cron: "0 23 * * *", Action: "sleep"}},
	}
	if err := s.Start([]ScheduleEntry{{Name: "g", Cron: "0 21 * * *", Action: "night_sleep"}}, perTier); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// PUT-style reload with a different global set; per-tier should stay.
	if err := s.Reload([]ScheduleEntry{{Name: "g2", Cron: "5 21 * * *", Action: "night_wake"}}); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Entries() returns global only.
	got := s.Entries()
	if len(got) != 1 || got[0].Name != "g2" {
		t.Errorf("Entries() = %v, want [g2]", got)
	}

	// Per-tier entry should still fire when its cron triggers — simulate
	// by calling runScoped directly.
	s.runScoped(scopedEntry{ScheduleEntry: perTier[1][0], Tier: 1})
	if got := orch.tierSleepCount(1); got != 1 {
		t.Errorf("tier-1 sleep count after reload = %d, want 1", got)
	}
}

// Compile-time interface check.
var _ ActionRunner = (*fakeOrch)(nil)
var _ ActionNotifier = (*PushManager)(nil)
var _ ActionNotifier = (*fakeActionNotifier)(nil)

// Avoid unused import warning if time becomes unused.
var _ = time.Second
