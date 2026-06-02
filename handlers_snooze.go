package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// SnoozeHandler bridges the PWA to the live SnoozeManager + Scheduler.
type SnoozeHandler struct {
	snooze *SnoozeManager
	sched  *Scheduler
	orch   *Orchestrator
	pm     *PushManager
}

func NewSnoozeHandler(sm *SnoozeManager, sched *Scheduler, orch *Orchestrator, pm *PushManager) *SnoozeHandler {
	return &SnoozeHandler{snooze: sm, sched: sched, orch: orch, pm: pm}
}

type snoozeRequest struct {
	Name         string `json:"name"`
	Mode         string `json:"mode"` // "skip_tonight" or "postpone"
	DelayMinutes int    `json:"delay_minutes,omitempty"`
}

func (h *SnoozeHandler) Handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *SnoozeHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	var req snoozeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	// Refuse snooze if the homelab is already in night mode — there's
	// nothing left to suppress and the user probably wants the wake toggle.
	state := h.orch.Status().State
	if state == "night" || state == "asleep" {
		http.Error(w,
			"homelab is already in "+state+" mode — flip Night mode off instead",
			http.StatusBadRequest)
		return
	}

	switch req.Mode {
	case "skip_tonight":
		// Skip until tomorrow 06:00 local — well past any reasonable
		// night-sleep cron, so tomorrow's normal schedule kicks back in.
		now := time.Now()
		tomorrow := time.Date(now.Year(), now.Month(), now.Day()+1, 6, 0, 0, 0, now.Location())
		if err := h.snooze.Skip(req.Name, tomorrow); err != nil {
			http.Error(w, "could not skip: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("snooze: skip %q until %s", req.Name, tomorrow.Format(time.RFC3339))
	case "postpone":
		if req.DelayMinutes <= 0 || req.DelayMinutes > 8*60 {
			http.Error(w, "delay_minutes must be 1..480", http.StatusBadRequest)
			return
		}
		delay := time.Duration(req.DelayMinutes) * time.Minute
		name := req.Name

		// "Postpone" adds delay to the CURRENT scheduled sleep — either
		// an active snooze's deferred fire or the next recurring cron.
		// So "+30 min" tapped at 21:00 (warning) with sleep at 21:30
		// moves sleep to 22:00, not "30 min from now". Another tap at
		// 21:50 moves it to 22:30. The user-facing label matches the
		// effect: each tap always adds delay minutes on top.
		now := time.Now()
		var base time.Time
		if existing, ok := h.snooze.Get(name); ok && !existing.DeferredFireAt.IsZero() && existing.DeferredFireAt.After(now) {
			base = existing.DeferredFireAt
		} else if next, ok := h.sched.NextFires()[name]; ok && next.After(now) {
			base = next
		} else {
			base = now
		}
		target := base.Add(delay)
		actualDelay := target.Sub(now)
		if actualDelay <= 0 {
			actualDelay = delay // degenerate fallback
		}

		runOnce := func() { h.sched.RunOnce(name) }
		warnBefore, warn := h.warnPushFor(name, actualDelay)

		err := h.snooze.PostponeWithWarning(name, actualDelay, warnBefore, runOnce, warn)
		if err != nil {
			http.Error(w, "could not postpone: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("snooze: extend %q by %s → sleep at %s", req.Name, delay, target.Format(time.RFC3339))
	default:
		http.Error(w, fmt.Sprintf("mode must be skip_tonight or postpone (got %q)", req.Mode), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if s, ok := h.snooze.Get(req.Name); ok {
		json.NewEncoder(w).Encode(s)
	} else {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

// warnPushFor looks up the schedule entry for `name` and, if it declares
// a valid WarnBefore + Snooze*, returns the warn-before duration and a
// callback that pushes the T-X "Sleeping in <warnBefore>" reminder with
// another snooze button. Returns (0, nil) when the entry doesn't opt in
// or PushManager is unavailable.
func (h *SnoozeHandler) warnPushFor(name string, delay time.Duration) (time.Duration, func()) {
	if h.pm == nil {
		return 0, nil
	}
	for _, e := range h.sched.Entries() {
		if e.Name != name {
			continue
		}
		if e.WarnBefore == "" || e.SnoozeTarget == "" || e.SnoozeMinutes <= 0 {
			return 0, nil
		}
		d, err := time.ParseDuration(e.WarnBefore)
		if err != nil || d <= 0 || d >= delay {
			return 0, nil
		}
		body := fmt.Sprintf("Sleeping in %s", d)
		data := map[string]any{
			"name":      e.SnoozeTarget,
			"minutes":   e.SnoozeMinutes,
			"click_url": "/countdown",
		}
		actions := []NotifyAction{
			{Action: "snooze", Title: fmt.Sprintf("+%d min", e.SnoozeMinutes)},
		}
		warn := func() { h.pm.NotifyWithActions("Homelab", body, data, actions) }
		return d, warn
	}
	return 0, nil
}

func (h *SnoozeHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name query param is required", http.StatusBadRequest)
		return
	}

	// Capture the snooze state before clearing so we can detect the case
	// where Cancel would otherwise silently behave like "skip tonight":
	// the user postponed past the original cron fire, then cancelled — at
	// that point the next recurring cron is tomorrow, so the deferred fire
	// was the only remaining sleep event for today.
	prev, hadSnooze := h.snooze.Get(name)

	if err := h.snooze.Clear(name); err != nil {
		http.Error(w, "could not clear: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("snooze: cleared %q", name)

	// Postpone-style snoozes carry DeferredFireAt. If the next cron fire
	// for this entry is later than that deferred time, today's recurring
	// fire has already passed and Cancel would otherwise mean "no sleep
	// tonight." Run the action now instead so Cancel always means "back
	// to the schedule," never an accidental skip. Skip-style snoozes
	// (DeferredFireAt zero) are intentional skips and untouched.
	if hadSnooze && !prev.DeferredFireAt.IsZero() {
		next := h.sched.NextFires()[name]
		if next.IsZero() || next.After(prev.DeferredFireAt) {
			log.Printf("snooze: cancel after original cron passed — firing %q now", name)
			go h.sched.RunOnce(name)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}
