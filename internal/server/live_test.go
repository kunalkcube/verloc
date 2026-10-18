package server

import (
	"testing"
)

func TestHubSubscriberCap(t *testing.T) {
	h := NewHub()
	for i := 0; i < maxHubSubscribers; i++ {
		ch := h.Subscribe()
		if !h.registered(ch) {
			t.Fatalf("subscriber %d rejected below cap", i)
		}
	}
	over := h.Subscribe()
	if h.registered(over) {
		t.Fatal("subscriber beyond cap was registered")
	}
	if _, ok := <-over; ok {
		t.Fatal("rejected subscriber channel was not closed")
	}
	h.Publish(map[string]any{"type": "x"}) // must not block
}

// B2: model access is explicit — "*" allows, empty denies.
