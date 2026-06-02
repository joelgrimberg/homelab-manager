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
}

func (f *fakeOrch) NightSleep() (bool, bool) { atomic.AddInt32(&f.nightSleeps, 1); return true, false }
func (f *fakeOrch) NightWake() (bool, bool)  { atomic.AddInt32(&f.nightWakes, 1); return true, false }

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
		{Name: "junk", Cron: "not a cron", Action: "wake"},
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

// Compile-time interface check.
var _ ActionRunner = (*fakeOrch)(nil)
var _ ActionNotifier = (*PushManager)(nil)
var _ ActionNotifier = (*fakeActionNotifier)(nil)

// Avoid unused import warning if time becomes unused.
var _ = time.Second
