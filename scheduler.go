package main

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// ActionRunner is the subset of Orchestrator the scheduler invokes. Kept
// narrow so tests can stub it.
type ActionRunner interface {
	NightSleep() (bool, bool)
	NightWake() (bool, bool)
	Wake() bool
	Sleep() bool
}

// Notifier abstracts PushManager for tests.
type Notifier interface {
	Notify(title, body string)
}

// Scheduler owns a robfig/cron.Cron and the current entry list. Reload is
// safe to call from request handlers while jobs may be firing.
type Scheduler struct {
	mu      sync.Mutex
	cron    *cron.Cron
	entries []ScheduleEntry
	orch    ActionRunner
	notify  Notifier
	snooze  *SnoozeManager
}

func NewScheduler(orch ActionRunner, notify Notifier, snooze *SnoozeManager) *Scheduler {
	s := &Scheduler{
		orch:   orch,
		notify: notify,
		snooze: snooze,
	}
	if snooze != nil {
		snooze.RearmDeferred(s.RunOnce)
	}
	return s
}

// Start registers entries and begins firing.
func (s *Scheduler) Start(entries []ScheduleEntry) error {
	return s.Reload(entries)
}

// Reload replaces the running entries atomically. Validates every cron
// expression before swapping; on any parse error the previous schedule
// stays active.
func (s *Scheduler) Reload(entries []ScheduleEntry) error {
	// Build a fresh cron with each entry pre-validated so we don't end up
	// with a half-loaded scheduler if one expression is bad.
	c := cron.New(cron.WithLocation(time.Local))
	for i, e := range entries {
		if e.Cron == "" {
			return fmt.Errorf("entry %d (%s): cron is required", i, e.Name)
		}
		if e.Action == "" && e.Notify == "" {
			return fmt.Errorf("entry %d (%s): must specify action and/or notify", i, e.Name)
		}
		if err := validateAction(e.Action); err != nil {
			return fmt.Errorf("entry %d (%s): %w", i, e.Name, err)
		}
		entry := e // capture
		if _, err := c.AddFunc(e.Cron, func() { s.run(entry) }); err != nil {
			return fmt.Errorf("entry %d (%s): invalid cron %q: %w", i, e.Name, e.Cron, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		ctx := s.cron.Stop()
		<-ctx.Done()
	}
	s.cron = c
	s.entries = entries
	c.Start()
	log.Printf("scheduler: loaded %d entries (TZ=%s)", len(entries), time.Local.String())
	return nil
}

// Entries returns the currently active schedule.
func (s *Scheduler) Entries() []ScheduleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScheduleEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

func (s *Scheduler) run(e ScheduleEntry) {
	if s.snooze != nil && s.snooze.IsSuppressed(e.Name) {
		log.Printf("scheduler: %q suppressed by snooze", e.Name)
		return
	}

	log.Printf("scheduler: firing %q (action=%q)", e.Name, e.Action)

	if e.Notify != "" && s.notify != nil {
		s.notify.Notify("Homelab", e.Notify)
	}

	s.dispatchAction(e)
}

// RunOnce fires the named entry's full run path (notify + action) bypassing
// the snooze check — used as the callback for deferred fires.
func (s *Scheduler) RunOnce(name string) {
	s.mu.Lock()
	var entry ScheduleEntry
	found := false
	for _, e := range s.entries {
		if e.Name == name {
			entry = e
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		log.Printf("scheduler: RunOnce: no entry named %q", name)
		return
	}
	// Bypass snooze: deferred fires must run even though their snooze
	// entry was set with a SkipUntil covering this moment.
	log.Printf("scheduler: deferred fire %q (action=%q)", entry.Name, entry.Action)
	if entry.Notify != "" && s.notify != nil {
		s.notify.Notify("Homelab", entry.Notify)
	}
	s.dispatchAction(entry)
}

// dispatchAction invokes the orchestrator method named by the entry.
func (s *Scheduler) dispatchAction(e ScheduleEntry) {
	switch e.Action {
	case "":
	case "night_sleep":
		if started, unconf := s.orch.NightSleep(); unconf {
			log.Printf("scheduler: %s wanted night_sleep but night mode unconfigured", e.Name)
		} else if !started {
			log.Printf("scheduler: %s night_sleep skipped — already transitioning", e.Name)
		}
	case "night_wake":
		if started, unconf := s.orch.NightWake(); unconf {
			log.Printf("scheduler: %s wanted night_wake but night mode unconfigured", e.Name)
		} else if !started {
			log.Printf("scheduler: %s night_wake skipped — already transitioning", e.Name)
		}
	case "wake":
		if !s.orch.Wake() {
			log.Printf("scheduler: %s wake skipped — already transitioning", e.Name)
		}
	case "sleep":
		if !s.orch.Sleep() {
			log.Printf("scheduler: %s sleep skipped — already transitioning", e.Name)
		}
	}
}

// NextFires returns a map from entry name → next scheduled fire time. Used
// by the PWA to know when sleep is imminent. Only includes recurring cron
// entries; deferred-fire timers aren't reflected here (the snooze map carries
// that information separately).
func (s *Scheduler) NextFires() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return nil
	}
	// Walk cron entries in registration order; they map 1:1 to s.entries
	// because Reload added them in order.
	cronEntries := s.cron.Entries()
	out := make(map[string]time.Time, len(cronEntries))
	for i, ce := range cronEntries {
		if i >= len(s.entries) {
			break
		}
		out[s.entries[i].Name] = ce.Next
	}
	return out
}

func validateAction(a string) error {
	switch a {
	case "", "night_sleep", "night_wake", "wake", "sleep":
		return nil
	default:
		return fmt.Errorf("unknown action %q (must be night_sleep, night_wake, wake, sleep, or empty)", a)
	}
}
