package main

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventsHandlerStreamsPublishedEvent(t *testing.T) {
	hub := NewEventHub()
	h := NewEventsHandler(hub)

	srv := httptest.NewServer(http.HandlerFunc(h.Handle))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}

	// Give the server a moment to subscribe before publishing.
	deadline := time.Now().Add(500 * time.Millisecond)
	for hub.SubscriberCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.SubscriberCount() == 0 {
		t.Fatal("server never subscribed to hub")
	}

	hub.Publish(Event{Type: "vm_action", Action: "stop", VMID: 100, Name: "vm-a"})

	scanner := bufio.NewScanner(resp.Body)
	deadline = time.Now().Add(1500 * time.Millisecond)
	var dataLine string
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			dataLine = line
			break
		}
	}
	if dataLine == "" {
		t.Fatal("no data: line received")
	}
	if !strings.Contains(dataLine, `"type":"vm_action"`) {
		t.Errorf("missing vm_action in payload: %q", dataLine)
	}
	if !strings.Contains(dataLine, `"vmid":100`) {
		t.Errorf("missing vmid in payload: %q", dataLine)
	}
}

func TestEventsHandlerMethodNotAllowed(t *testing.T) {
	hub := NewEventHub()
	h := NewEventsHandler(hub)
	req := httptest.NewRequest("POST", "/api/events", nil)
	w := httptest.NewRecorder()
	h.Handle(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
