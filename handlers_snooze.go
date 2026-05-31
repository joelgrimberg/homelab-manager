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
}

func NewSnoozeHandler(sm *SnoozeManager, sched *Scheduler, orch *Orchestrator) *SnoozeHandler {
	return &SnoozeHandler{snooze: sm, sched: sched, orch: orch}
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
		err := h.snooze.Postpone(name, delay, func() { h.sched.RunOnce(name) })
		if err != nil {
			http.Error(w, "could not postpone: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("snooze: postpone %q by %s", req.Name, delay)
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

func (h *SnoozeHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name query param is required", http.StatusBadRequest)
		return
	}
	if err := h.snooze.Clear(name); err != nil {
		http.Error(w, "could not clear: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("snooze: cleared %q", name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}
