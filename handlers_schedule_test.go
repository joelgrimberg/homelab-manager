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

	h := NewScheduleHandler(sched, "")
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
	h := NewScheduleHandler(sched, "")

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
	h := NewScheduleHandler(sched, path)

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

func TestScheduleHandlerMethodNotAllowed(t *testing.T) {
	sched := NewScheduler(&fakeOrch{}, &fakeNotifier{}, nil)
	sched.Start(nil, nil)
	h := NewScheduleHandler(sched, "")
	req := httptest.NewRequest("DELETE", "/api/schedule", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
