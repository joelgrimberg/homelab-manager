package main

import (
	"sync"
	"time"
)

// Event is one orchestrator status update streamed over SSE to anything
// listening (currently the /countdown page's terminal log).
type Event struct {
	Type   string    `json:"type"`             // sleep_start | vm_action | vm_error | sleep_complete
	Action string    `json:"action,omitempty"` // start | stop (when Type == vm_action)
	VMID   int       `json:"vmid,omitempty"`
	Name   string    `json:"name,omitempty"`
	Tier   int       `json:"tier,omitempty"`
	Error  string    `json:"error,omitempty"`
	TS     time.Time `json:"ts"`
}

// EventHub is a tiny fan-out broker: publishers (the orchestrator) call
// Publish, subscribers (each SSE connection) call Subscribe and read from
// the returned channel until they call the returned cancel func.
//
// Subscribers get a small buffered channel. A slow consumer doesn't block
// the publisher — its events are silently dropped instead. The orchestrator
// must never stall on this hub.
type EventHub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

const eventChanBuffer = 32

func NewEventHub() *EventHub {
	return &EventHub{subs: map[chan Event]struct{}{}}
}

func (h *EventHub) Publish(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
			// Drop on full buffer — better than blocking the orchestrator.
		}
	}
}

func (h *EventHub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, eventChanBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

func (h *EventHub) SubscriberCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
