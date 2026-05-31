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

// Compile-time interface check.
var _ ActionRunner = (*fakeOrch)(nil)

// Avoid unused import warning if time becomes unused.
var _ = time.Second
