package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestScheduleHandlerGetReturnsEntries(t *testing.T) {
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	entries := []ScheduleEntry{{Name: "n", Cron: "0 7 * * *", Action: "night_wake"}}
	if err := sched.Start(entries, nil); err != nil {
		t.Fatalf("start: %v", err)
	}

	h := NewScheduleHandler(sched, "", nil)
	req := httptest.NewRequest("GET", "/api/schedule", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var out []ScheduleEntry
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Name != "n" {
		t.Errorf("entries = %v, want one entry named n", out)
	}
}

func TestScheduleHandlerPutBadCronReturns400(t *testing.T) {
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	sched.Start(nil, nil)
	h := NewScheduleHandler(sched, "", nil)

	body, _ := json.Marshal([]ScheduleEntry{
		{Name: "bad", Cron: "garbage", Action: "wake"},
	})
	req := httptest.NewRequest("PUT", "/api/schedule", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "cron") {
		t.Errorf("body = %q, want mention of cron", w.Body.String())
	}
}

func TestScheduleHandlerPutPersistsAndPreservesOtherSections(t *testing.T) {
	cfgYAML := `proxmox:
  url: "https://example.test"
  node: "pve"
  token_id: "a"
  token_secret: "b"

tiers:
  - tag: "infra"
    tier: 1
    name: infra

schedule:
  - name: old
    cron: "0 1 * * *"
    notify: "old"
`
	path := writeTempConfig(t, cfgYAML)

	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	sched.Start(nil, nil)
	h := NewScheduleHandler(sched, path, nil)

	newEntries := []ScheduleEntry{
		{Name: "warn", Cron: "0 21 * * *", Notify: "5 min"},
		{Name: "ns", Cron: "5 21 * * *", Action: "night_sleep", Notify: "good night"},
	}
	body, _ := json.Marshal(newEntries)
	req := httptest.NewRequest("PUT", "/api/schedule", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.Handle(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Other sections should still be present.
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(out)
	for _, must := range []string{"proxmox:", "tiers:", "schedule:", "warn", "night_sleep", "good night"} {
		if !strings.Contains(got, must) {
			t.Errorf("config missing %q after write\n---\n%s", must, got)
		}
	}
	if strings.Contains(got, "name: old") {
		t.Errorf("old entry not replaced:\n%s", got)
	}

	// And the scheduler should now hold the new entries.
	if e := sched.Entries(); len(e) != 2 || e[0].Name != "warn" {
		t.Errorf("scheduler entries = %v, want new entries", e)
	}
}

// TestTierScheduleHandlerRoundTrip: PUT a tier's entries → scheduler picks
// them up, config.yaml writeback preserves the global schedule and other
// tiers, GET returns what was PUT, and the aggregator GET groups them by
// tier with names.
func TestTierScheduleHandlerRoundTrip(t *testing.T) {
	cfgYAML := `proxmox:
  url: "https://example.test"
  node: "pve"
  token_id: "a"
  token_secret: "b"

tiers:
  - tag: "infra"
    tier: 1
    name: infra
  - tag: "apps"
    tier: 2
    name: apps

schedule:
  - name: nightly
    cron: "5 21 * * *"
    action: night_sleep
`
	path := writeTempConfig(t, cfgYAML)
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	sched.Start(cfg.Schedule, nil)
	h := NewScheduleHandler(sched, path, cfg.TierDefs)

	// PUT tier 1.
	entries := []ScheduleEntry{
		{Name: "infra-wake", Cron: "0 7 * * *", Action: "wake"},
		{Name: "infra-sleep", Cron: "0 23 * * *", Action: "sleep"},
	}
	body, _ := json.Marshal(entries)
	req := httptest.NewRequest("PUT", "/api/tiers/1/schedule", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleTier(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%s", w.Code, w.Body.String())
	}

	// GET tier 1 returns what we wrote.
	req = httptest.NewRequest("GET", "/api/tiers/1/schedule", nil)
	w = httptest.NewRecorder()
	h.HandleTier(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	var got []ScheduleEntry
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 || got[0].Name != "infra-wake" {
		t.Errorf("GET = %v", got)
	}

	// Aggregator returns global + each tier.
	req = httptest.NewRequest("GET", "/api/schedules", nil)
	w = httptest.NewRecorder()
	h.HandleAggregate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("aggregate status = %d", w.Code)
	}
	var agg aggregateResponse
	if err := json.NewDecoder(w.Body).Decode(&agg); err != nil {
		t.Fatalf("agg decode: %v", err)
	}
	if len(agg.Global) != 1 || agg.Global[0].Name != "nightly" {
		t.Errorf("aggregate.global = %v", agg.Global)
	}
	if len(agg.Tiers) != 2 || agg.Tiers[0].Tier != 1 || agg.Tiers[0].Name != "infra" || len(agg.Tiers[0].Schedule) != 2 {
		t.Errorf("aggregate.tiers[0] = %+v", agg.Tiers[0])
	}
	if agg.Tiers[1].Tier != 2 || len(agg.Tiers[1].Schedule) != 0 {
		t.Errorf("aggregate.tiers[1] = %+v", agg.Tiers[1])
	}

	// On disk: global preserved, tier 1 has the new schedule.
	out, _ := os.ReadFile(path)
	if !strings.Contains(string(out), "night_sleep") {
		t.Errorf("global schedule lost from disk:\n%s", out)
	}
	if !strings.Contains(string(out), "infra-sleep") {
		t.Errorf("tier 1 schedule missing from disk:\n%s", out)
	}
}

// TestTierScheduleHandlerRejectsGlobalAction: a tier entry can't use
// night_sleep / night_wake.
func TestTierScheduleHandlerRejectsGlobalAction(t *testing.T) {
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	sched.Start(nil, nil)
	h := NewScheduleHandler(sched, "", []TierConfig{{Tier: 1, Name: "infra"}})

	body, _ := json.Marshal([]ScheduleEntry{
		{Name: "x", Cron: "0 7 * * *", Action: "night_sleep"},
	})
	req := httptest.NewRequest("PUT", "/api/tiers/1/schedule", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleTier(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (got body=%s)", w.Code, w.Body.String())
	}
}

// TestTierScheduleHandlerUnknownTier: 404 for tiers not declared in config.
func TestTierScheduleHandlerUnknownTier(t *testing.T) {
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	sched.Start(nil, nil)
	h := NewScheduleHandler(sched, "", []TierConfig{{Tier: 1, Name: "infra"}})
	req := httptest.NewRequest("GET", "/api/tiers/99/schedule", nil)
	w := httptest.NewRecorder()
	h.HandleTier(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestScheduleHandlerMethodNotAllowed(t *testing.T) {
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	sched.Start(nil, nil)
	h := NewScheduleHandler(sched, "", nil)
	req := httptest.NewRequest("DELETE", "/api/schedule", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
