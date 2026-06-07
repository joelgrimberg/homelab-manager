package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

// ScheduleHandler bridges the PWA to the live Scheduler + on-disk config.
// It owns both the global `/api/schedule` endpoint and a per-tier
// `/api/tiers/{n}/schedule` endpoint registered separately in main.
type ScheduleHandler struct {
	sched      *Scheduler
	configPath string
	tiers      []TierConfig // ordered, for naming + aggregator GET
}

func NewScheduleHandler(sched *Scheduler, configPath string, tiers []TierConfig) *ScheduleHandler {
	return &ScheduleHandler{sched: sched, configPath: configPath, tiers: tiers}
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

// HandleAggregate serves GET /api/schedules — a single read endpoint that
// returns the global schedule plus each tier's schedule with its name, so
// the PWA editor can render everything in one fetch.
type tierScheduleView struct {
	Tier     int             `json:"tier"`
	Name     string          `json:"name"`
	Schedule []ScheduleEntry `json:"schedule"`
}

type aggregateResponse struct {
	Global []ScheduleEntry    `json:"global"`
	Tiers  []tierScheduleView `json:"tiers"`
}

func (h *ScheduleHandler) HandleAggregate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := aggregateResponse{Global: h.sched.Entries()}
	if resp.Global == nil {
		resp.Global = []ScheduleEntry{}
	}
	for _, td := range h.tiers {
		entries := h.sched.TierSchedule(td.Tier)
		if entries == nil {
			entries = []ScheduleEntry{}
		}
		resp.Tiers = append(resp.Tiers, tierScheduleView{Tier: td.Tier, Name: td.Name, Schedule: entries})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// HandleTier serves GET/PUT /api/tiers/{n}/schedule. PUT replaces the
// tier's entries; rejects entries whose action isn't allowed in tier
// scope (only wake/sleep/"" are valid).
func (h *ScheduleHandler) HandleTier(w http.ResponseWriter, r *http.Request) {
	// Path: /api/tiers/{n}/schedule
	rest := strings.TrimPrefix(r.URL.Path, "/api/tiers/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] != "schedule" {
		http.Error(w, "expected /api/tiers/{n}/schedule", http.StatusBadRequest)
		return
	}
	tier, err := strconv.Atoi(parts[0])
	if err != nil {
		http.Error(w, "invalid tier", http.StatusBadRequest)
		return
	}
	if !h.knownTier(tier) {
		http.Error(w, "unknown tier", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		entries := h.sched.TierSchedule(tier)
		if entries == nil {
			entries = []ScheduleEntry{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	case http.MethodPut:
		var entries []ScheduleEntry
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
		for _, e := range entries {
			if e.Cron == "" {
				http.Error(w, "entry "+e.Name+": cron is required", http.StatusBadRequest)
				return
			}
			if _, err := parser.Parse(e.Cron); err != nil {
				http.Error(w, "entry "+e.Name+": invalid cron: "+err.Error(), http.StatusBadRequest)
				return
			}
			if err := validateAction(e.Action, true); err != nil {
				http.Error(w, "entry "+e.Name+": "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		if err := h.sched.ReplaceTierSchedule(tier, entries); err != nil {
			http.Error(w, "scheduler reload failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := WriteTierScheduleToConfig(h.configPath, tier, entries); err != nil {
			log.Printf("schedule: tier %d config writeback failed: %v", tier, err)
			http.Error(w, "config writeback failed", http.StatusInternalServerError)
			return
		}
		log.Printf("schedule: reloaded tier %d (%d entries)", tier, len(entries))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(entries)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *ScheduleHandler) knownTier(tier int) bool {
	for _, td := range h.tiers {
		if td.Tier == tier {
			return true
		}
	}
	return false
}
