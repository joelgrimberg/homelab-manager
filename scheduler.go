package main

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// ActionRunner is the subset of Orchestrator the scheduler invokes. Kept
// narrow so tests can stub it. NightSleep/NightWake operate across all
// tiers; WakeTier/SleepTier operate on the named tier only.
type ActionRunner interface {
	NightSleep() (bool, bool)
	NightWake() (bool, bool)
	WakeTier(tier int) (bool, bool)
	SleepTier(tier int) (bool, bool)
}

// scopedEntry pairs a ScheduleEntry with the tier it targets. Tier == 0
// means the entry was loaded from the top-level `schedule:` list and the
// action is global (night_sleep / night_wake). Tier > 0 means the entry
// came from `tiers[].schedule` and the action targets that tier only.
type scopedEntry struct {
	ScheduleEntry
	Tier int
}

// Notifier abstracts PushManager for tests.
type Notifier interface {
	Notify(title, body string)
}

// ActionNotifier is the optional extension that allows sending a push with
// a `data` blob and clickable action buttons. PushManager implements it;
// test fakes typically don't, in which case the scheduler falls back to
// plain Notify.
type ActionNotifier interface {
	NotifyWithActions(title, body string, data map[string]any, actions []NotifyAction)
}

// Scheduler owns a robfig/cron.Cron and the current entry list. Reload is
// safe to call from request handlers while jobs may be firing. Global and
// per-tier entries live side by side; PUT /api/schedule only replaces the
// global list, while per-tier entries are read once at startup from
// `tiers[].schedule` and stay put across reloads.
type Scheduler struct {
	mu      sync.Mutex
	cron    *cron.Cron
	global  []ScheduleEntry
	perTier map[int][]ScheduleEntry
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

// Start registers entries and begins firing. Convenience wrapper used at
// startup to load the global and per-tier lists in a single call.
func (s *Scheduler) Start(global []ScheduleEntry, perTier map[int][]ScheduleEntry) error {
	return s.rebuild(global, perTier)
}

// Reload replaces the running global entries atomically while preserving
// the per-tier ones. Used by PUT /api/schedule, which only round-trips
// the top-level `schedule:` block.
func (s *Scheduler) Reload(global []ScheduleEntry) error {
	s.mu.Lock()
	perTier := s.perTier
	s.mu.Unlock()
	return s.rebuild(global, perTier)
}

// ReplaceTierSchedule swaps a single tier's entries while preserving the
// global list and every other tier. Used by PUT /api/tiers/{n}/schedule.
// Passing nil or an empty slice removes the tier's schedule entirely.
func (s *Scheduler) ReplaceTierSchedule(tier int, entries []ScheduleEntry) error {
	s.mu.Lock()
	global := s.global
	perTier := clonePerTier(s.perTier)
	s.mu.Unlock()
	if perTier == nil {
		perTier = map[int][]ScheduleEntry{}
	}
	if len(entries) == 0 {
		delete(perTier, tier)
	} else {
		perTier[tier] = entries
	}
	return s.rebuild(global, perTier)
}

// TierSchedule returns a copy of the entries currently registered for a
// tier. Returns nil if the tier has no entries.
func (s *Scheduler) TierSchedule(tier int) []ScheduleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.perTier[tier]
	if len(src) == 0 {
		return nil
	}
	out := make([]ScheduleEntry, len(src))
	copy(out, src)
	return out
}

// rebuild flattens global + per-tier entries into a single scoped list,
// validates every cron expression and action before swapping; on any
// error the previous schedule stays active.
func (s *Scheduler) rebuild(global []ScheduleEntry, perTier map[int][]ScheduleEntry) error {
	scoped := flatten(global, perTier)

	c := cron.New(cron.WithLocation(time.Local))
	for i, se := range scoped {
		if se.Cron == "" {
			return fmt.Errorf("entry %d (%s): cron is required", i, se.Name)
		}
		if se.Action == "" && se.Notify == "" {
			return fmt.Errorf("entry %d (%s): must specify action and/or notify", i, se.Name)
		}
		if err := validateAction(se.Action, se.Tier > 0); err != nil {
			return fmt.Errorf("entry %d (%s): %w", i, se.Name, err)
		}
		entry := se // capture
		if _, err := c.AddFunc(se.Cron, func() { s.runScoped(entry) }); err != nil {
			return fmt.Errorf("entry %d (%s): invalid cron %q: %w", i, se.Name, se.Cron, err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		ctx := s.cron.Stop()
		<-ctx.Done()
	}
	s.cron = c
	s.global = append([]ScheduleEntry(nil), global...)
	s.perTier = clonePerTier(perTier)
	c.Start()
	log.Printf("scheduler: loaded %d global + %d tier entries (TZ=%s)", len(global), len(scoped)-len(global), time.Local.String())
	return nil
}

// flatten returns global entries (Tier=0) followed by per-tier entries
// ordered by tier number for deterministic registration and a stable
// 1:1 mapping into cron.Entries() for NextFires.
func flatten(global []ScheduleEntry, perTier map[int][]ScheduleEntry) []scopedEntry {
	out := make([]scopedEntry, 0, len(global)+totalLen(perTier))
	for _, e := range global {
		out = append(out, scopedEntry{ScheduleEntry: e})
	}
	tiers := make([]int, 0, len(perTier))
	for t := range perTier {
		tiers = append(tiers, t)
	}
	sort.Ints(tiers)
	for _, t := range tiers {
		for _, e := range perTier[t] {
			out = append(out, scopedEntry{ScheduleEntry: e, Tier: t})
		}
	}
	return out
}

func totalLen(m map[int][]ScheduleEntry) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

func clonePerTier(m map[int][]ScheduleEntry) map[int][]ScheduleEntry {
	if m == nil {
		return nil
	}
	out := make(map[int][]ScheduleEntry, len(m))
	for k, v := range m {
		out[k] = append([]ScheduleEntry(nil), v...)
	}
	return out
}

// Entries returns the global entries currently active. Per-tier entries
// are config-file-only and intentionally not exposed here — the PWA
// schedule editor edits and re-PUTs whatever Entries() returns, so
// surfacing per-tier ones would silently flatten them on save.
func (s *Scheduler) Entries() []ScheduleEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ScheduleEntry, len(s.global))
	copy(out, s.global)
	return out
}

// run is the legacy global-only fire path retained for tests. Production
// fires go through runScoped via the cron callback.
func (s *Scheduler) run(e ScheduleEntry) {
	s.runScoped(scopedEntry{ScheduleEntry: e})
}

func (s *Scheduler) runScoped(se scopedEntry) {
	if s.snooze != nil && s.snooze.IsSuppressed(se.Name) {
		log.Printf("scheduler: %q suppressed by snooze", se.Name)
		return
	}

	log.Printf("scheduler: firing %q (action=%q tier=%d)", se.Name, se.Action, se.Tier)

	s.emitNotify(se.ScheduleEntry)

	s.dispatchAction(se)
}

// RunOnce fires the named entry's full run path (notify + action) bypassing
// the snooze check — used as the callback for deferred fires. Searches
// global entries first, then per-tier, so a deferred snooze on a global
// "night-sleep" entry still fires correctly.
func (s *Scheduler) RunOnce(name string) {
	s.mu.Lock()
	var found *scopedEntry
	for _, e := range s.global {
		if e.Name == name {
			cp := scopedEntry{ScheduleEntry: e}
			found = &cp
			break
		}
	}
	if found == nil {
		for tier, entries := range s.perTier {
			for _, e := range entries {
				if e.Name == name {
					cp := scopedEntry{ScheduleEntry: e, Tier: tier}
					found = &cp
					break
				}
			}
			if found != nil {
				break
			}
		}
	}
	s.mu.Unlock()
	if found == nil {
		log.Printf("scheduler: RunOnce: no entry named %q", name)
		return
	}
	log.Printf("scheduler: deferred fire %q (action=%q tier=%d)", found.Name, found.Action, found.Tier)
	s.emitNotify(found.ScheduleEntry)
	s.dispatchAction(*found)
}

// emitNotify sends the entry's notify message. If the entry declares a
// SnoozeTarget and the underlying notifier supports actions, the push
// includes a Snooze button targeting that entry and a click_url of
// /countdown. Otherwise it falls back to a plain title+body push.
func (s *Scheduler) emitNotify(e ScheduleEntry) {
	if e.Notify == "" || s.notify == nil {
		return
	}
	if e.SnoozeTarget == "" || e.SnoozeMinutes <= 0 {
		s.notify.Notify("Homelab", e.Notify)
		return
	}
	an, ok := s.notify.(ActionNotifier)
	if !ok {
		s.notify.Notify("Homelab", e.Notify)
		return
	}
	data := map[string]any{
		"name":      e.SnoozeTarget,
		"minutes":   e.SnoozeMinutes,
		"click_url": "/countdown",
	}
	actions := []NotifyAction{
		{Action: "snooze", Title: fmt.Sprintf("+%d min", e.SnoozeMinutes)},
	}
	an.NotifyWithActions("Homelab", e.Notify, data, actions)
}

// dispatchAction invokes the orchestrator method named by the entry,
// scoped to the entry's tier when applicable.
func (s *Scheduler) dispatchAction(se scopedEntry) {
	switch se.Action {
	case "":
	case "night_sleep":
		if started, unconf := s.orch.NightSleep(); unconf {
			log.Printf("scheduler: %s wanted night_sleep but night mode unconfigured", se.Name)
		} else if !started {
			log.Printf("scheduler: %s night_sleep skipped — already transitioning", se.Name)
		}
	case "night_wake":
		if started, unconf := s.orch.NightWake(); unconf {
			log.Printf("scheduler: %s wanted night_wake but night mode unconfigured", se.Name)
		} else if !started {
			log.Printf("scheduler: %s night_wake skipped — already transitioning", se.Name)
		}
	case "wake":
		if started, unknown := s.orch.WakeTier(se.Tier); unknown {
			log.Printf("scheduler: %s wanted wake but tier %d unknown", se.Name, se.Tier)
		} else if !started {
			log.Printf("scheduler: %s wake skipped — already transitioning", se.Name)
		}
	case "sleep":
		if started, unknown := s.orch.SleepTier(se.Tier); unknown {
			log.Printf("scheduler: %s wanted sleep but tier %d unknown", se.Name, se.Tier)
		} else if !started {
			log.Printf("scheduler: %s sleep skipped — already transitioning", se.Name)
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
	// Walk cron entries in registration order; they map 1:1 to the
	// flattened scoped list (global first, then per-tier sorted by tier).
	scoped := flatten(s.global, s.perTier)
	cronEntries := s.cron.Entries()
	out := make(map[string]time.Time, len(cronEntries))
	for i, ce := range cronEntries {
		if i >= len(scoped) {
			break
		}
		out[scoped[i].Name] = ce.Next
	}
	return out
}

// validateAction checks that `a` is permitted in the entry's scope.
// Top-level entries (scope=false) may use night_sleep, night_wake, or "".
// Per-tier entries (scope=true) may use wake, sleep, or "".
func validateAction(a string, tierScope bool) error {
	if tierScope {
		switch a {
		case "", "wake", "sleep":
			return nil
		default:
			return fmt.Errorf("action %q not allowed under a tier (use wake, sleep, or empty)", a)
		}
	}
	switch a {
	case "", "night_sleep", "night_wake":
		return nil
	default:
		return fmt.Errorf("action %q not allowed at top level (use night_sleep, night_wake, or empty)", a)
	}
}
