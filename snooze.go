package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Snooze is one active suppression. SkipUntil is when the recurring cron
// resumes firing. DeferredFireAt, if non-zero, is when a one-off run of
// the entry's action should happen (postpone). Either may be zero.
type Snooze struct {
	SkipUntil       time.Time `json:"skip_until"`
	DeferredFireAt  time.Time `json:"deferred_fire_at,omitempty"`
}

// SnoozeManager owns the persistent snooze map and the live one-shot timers
// behind any deferred fires.
//
// warnTimers are best-effort, non-persisted T-1 (or similar) "imminent"
// nudges scheduled alongside a Postpone. Lost on restart by design — the
// actual sleep deferral still survives because it's in the persisted state.
type SnoozeManager struct {
	mu         sync.Mutex
	path       string
	state      map[string]Snooze
	timers     map[string]*time.Timer
	warnTimers map[string]*time.Timer
}

func NewSnoozeManager(path string) (*SnoozeManager, error) {
	sm := &SnoozeManager{
		path:       path,
		state:      map[string]Snooze{},
		timers:     map[string]*time.Timer{},
		warnTimers: map[string]*time.Timer{},
	}
	if err := sm.load(); err != nil {
		return nil, err
	}
	return sm, nil
}

func (sm *SnoozeManager) load() error {
	data, err := os.ReadFile(sm.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading snooze: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	var raw map[string]Snooze
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing snooze: %w", err)
	}
	now := time.Now()
	for name, s := range raw {
		// Drop fully-expired entries on load.
		if !s.SkipUntil.IsZero() && s.SkipUntil.Before(now) &&
			(s.DeferredFireAt.IsZero() || s.DeferredFireAt.Before(now)) {
			continue
		}
		sm.state[name] = s
	}
	return nil
}

// persist writes state to disk. Caller holds sm.mu.
func (sm *SnoozeManager) persist() error {
	if err := os.MkdirAll(filepath.Dir(sm.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sm.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(sm.path, data, 0o600)
}

// Skip suppresses the named entry until the given time. Any prior timer
// for this entry is cancelled.
func (sm *SnoozeManager) Skip(name string, until time.Time) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.cancelTimerLocked(name)
	sm.state[name] = Snooze{SkipUntil: until}
	return sm.persist()
}

// Postpone suppresses the named entry until now+delay+grace and schedules
// a one-shot timer that calls runOnce at now+delay. Any prior timer is
// cancelled.
func (sm *SnoozeManager) Postpone(name string, delay time.Duration, runOnce func()) error {
	return sm.PostponeWithWarning(name, delay, 0, runOnce, nil)
}

// PostponeWithWarning is Postpone plus an optional, non-persisted T-X
// nudge: when warnBefore > 0 and < delay, warn() fires at now+(delay-warnBefore).
// Both timers are cancelled atomically by Clear or another Postpone call.
func (sm *SnoozeManager) PostponeWithWarning(name string, delay, warnBefore time.Duration, runOnce func(), warn func()) error {
	const grace = 5 * time.Minute
	now := time.Now()
	fireAt := now.Add(delay)

	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.cancelTimerLocked(name)
	sm.state[name] = Snooze{
		SkipUntil:      fireAt.Add(grace),
		DeferredFireAt: fireAt,
	}
	sm.timers[name] = time.AfterFunc(delay, func() {
		sm.mu.Lock()
		delete(sm.timers, name)
		// Cancel any sibling warn timer that hasn't fired (defensive — it
		// should have already fired by now if scheduled correctly).
		if wt, ok := sm.warnTimers[name]; ok {
			wt.Stop()
			delete(sm.warnTimers, name)
		}
		// Clear the persisted entry — the deferred fire is happening now.
		delete(sm.state, name)
		_ = sm.persist()
		sm.mu.Unlock()
		runOnce()
	})
	if warn != nil && warnBefore > 0 && warnBefore < delay {
		sm.warnTimers[name] = time.AfterFunc(delay-warnBefore, func() {
			sm.mu.Lock()
			delete(sm.warnTimers, name)
			sm.mu.Unlock()
			warn()
		})
	}
	return sm.persist()
}

// IsSuppressed reports whether the recurring cron should be short-circuited.
// Lazily purges entries whose SkipUntil is past.
func (sm *SnoozeManager) IsSuppressed(name string) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.state[name]
	if !ok {
		return false
	}
	if s.SkipUntil.IsZero() || time.Now().After(s.SkipUntil) {
		delete(sm.state, name)
		_ = sm.persist()
		return false
	}
	return true
}

// Get returns the current snooze for an entry, if any.
func (sm *SnoozeManager) Get(name string) (Snooze, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	s, ok := sm.state[name]
	return s, ok
}

// All returns a snapshot of every active snooze.
func (sm *SnoozeManager) All() map[string]Snooze {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make(map[string]Snooze, len(sm.state))
	for k, v := range sm.state {
		out[k] = v
	}
	return out
}

// Clear cancels any timer and removes the entry from state.
func (sm *SnoozeManager) Clear(name string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.cancelTimerLocked(name)
	delete(sm.state, name)
	return sm.persist()
}

func (sm *SnoozeManager) cancelTimerLocked(name string) {
	if t, ok := sm.timers[name]; ok {
		t.Stop()
		delete(sm.timers, name)
	}
	if wt, ok := sm.warnTimers[name]; ok {
		wt.Stop()
		delete(sm.warnTimers, name)
	}
}

// RearmDeferred (re)schedules timers for any entries persisted with a
// future DeferredFireAt — used at startup so a server restart doesn't
// orphan a postponed fire. runOnce is the callback for any entry that
// fires.
func (sm *SnoozeManager) RearmDeferred(runOnce func(name string)) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	for name, s := range sm.state {
		if s.DeferredFireAt.IsZero() || !s.DeferredFireAt.After(now) {
			continue
		}
		if _, alreadyArmed := sm.timers[name]; alreadyArmed {
			continue
		}
		name := name // capture
		delay := time.Until(s.DeferredFireAt)
		sm.timers[name] = time.AfterFunc(delay, func() {
			sm.mu.Lock()
			delete(sm.timers, name)
			delete(sm.state, name)
			_ = sm.persist()
			sm.mu.Unlock()
			runOnce(name)
		})
		log.Printf("snooze: re-armed deferred fire for %q at %s", name, s.DeferredFireAt.Format(time.RFC3339))
	}
}
