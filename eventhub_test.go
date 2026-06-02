package main

import (
	"sync"
	"testing"
	"time"
)

func TestEventHubFanoutToMultipleSubscribers(t *testing.T) {
	h := NewEventHub()
	c1, cancel1 := h.Subscribe()
	c2, cancel2 := h.Subscribe()
	defer cancel1()
	defer cancel2()

	h.Publish(Event{Type: "sleep_start"})

	for i, ch := range []<-chan Event{c1, c2} {
		select {
		case e := <-ch:
			if e.Type != "sleep_start" {
				t.Errorf("subscriber %d got %q", i, e.Type)
			}
		case <-time.After(100 * time.Millisecond):
			t.Errorf("subscriber %d: timeout waiting for event", i)
		}
	}
}

func TestEventHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewEventHub()
	ch, cancel := h.Subscribe()
	cancel()

	h.Publish(Event{Type: "sleep_start"})

	// After cancel, the channel is closed. Reading must yield the zero
	// value with ok=false.
	select {
	case _, open := <-ch:
		if open {
			t.Error("expected closed channel after cancel")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("read on cancelled subscription blocked")
	}
	if h.SubscriberCount() != 0 {
		t.Errorf("SubscriberCount = %d, want 0", h.SubscriberCount())
	}
}

func TestEventHubSlowConsumerDoesNotBlockPublisher(t *testing.T) {
	h := NewEventHub()
	_, cancel := h.Subscribe()
	defer cancel()

	// Flood far more than the per-subscriber buffer (32). Publish must
	// never block — slow consumers drop instead.
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan struct{})
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			h.Publish(Event{Type: "vm_action", VMID: i})
		}
		close(done)
	}()

	select {
	case <-done:
		// good — publisher returned despite no one draining the channel.
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on slow consumer")
	}
	wg.Wait()
}
