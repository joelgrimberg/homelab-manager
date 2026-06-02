package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// EventsHandler streams orchestrator events as Server-Sent Events. The
// /countdown page consumes this to render its terminal-style log.
type EventsHandler struct {
	hub *EventHub
}

func NewEventsHandler(hub *EventHub) *EventsHandler {
	return &EventsHandler{hub: hub}
}

func (h *EventsHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // for nginx in front

	// Send an initial comment so the browser's EventSource considers the
	// stream open even before any real event arrives.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := h.hub.Subscribe()
	defer cancel()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case e, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
