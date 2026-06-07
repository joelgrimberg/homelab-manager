package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/robfig/cron/v3"
)

// ScheduleHandler bridges the PWA to the live Scheduler + on-disk config.
type ScheduleHandler struct {
	sched      *Scheduler
	configPath string
}

func NewScheduleHandler(sched *Scheduler, configPath string) *ScheduleHandler {
	return &ScheduleHandler{sched: sched, configPath: configPath}
}

func (h *ScheduleHandler) Handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		entries := h.sched.Entries()
		if entries == nil {
			entries = []ScheduleEntry{}
		}
		json.NewEncoder(w).Encode(entries)
	case http.MethodPut:
		var entries []ScheduleEntry
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		// Validate every cron expression before persisting; we want
		// 400 + a clear message rather than a half-applied state.
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		for i, e := range entries {
			if e.Cron == "" {
				http.Error(w, "entry "+e.Name+": cron is required", http.StatusBadRequest)
				return
			}
			if _, err := parser.Parse(e.Cron); err != nil {
				http.Error(w, "entry "+e.Name+": invalid cron: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := validateAction(e.Action, false); err != nil {
				http.Error(w, "entry "+e.Name+": "+err.Error(), http.StatusBadRequest)
				return
			}
			_ = i
		}
		if err := h.sched.Reload(entries); err != nil {
			http.Error(w, "scheduler reload failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := WriteScheduleToConfig(h.configPath, entries); err != nil {
			log.Printf("schedule: config writeback failed: %v", err)
			http.Error(w, "config writeback failed", http.StatusInternalServerError)
			return
		}
		log.Printf("schedule: reloaded %d entries", len(entries))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
