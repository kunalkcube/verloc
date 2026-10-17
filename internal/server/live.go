package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Hub fans out live events to SSE subscribers (the dashboard's live tail,
// alert toasts). A single background goroutine per subscriber keeps
// broadcast overhead off the request hot path.
type Hub struct {
	mu     sync.Mutex
	subs   map[chan map[string]any]bool
	closed bool
}

func NewHub() *Hub {
	return &Hub{subs: map[chan map[string]any]bool{}}
}

// Subscribe registers a subscriber and returns its channel (buffered so a
// slow client drops messages instead of blocking the gateway).
func (h *Hub) Subscribe() chan map[string]any {
	ch := make(chan map[string]any, 256)
	h.mu.Lock()
	if h.closed {
		close(ch)
	} else {
		h.subs[ch] = true
	}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan map[string]any) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// Publish sends one event to all subscribers, non-blocking.
func (h *Hub) Publish(msg map[string]any) {
	if r, ok := msg["req"].(ReqRecord); ok {
		msg["req"] = sanitizeRecord(r)
	}
	if e, ok := msg["event"].(KeyEvent); ok {
		e.Key, e.Msg = "", ""
		e.Event = safeKeyEvent(e.Event)
		msg["event"] = e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // drop for slow consumers
		}
	}
}

// Close unblocks SSE readers and prevents any future subscriptions.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}

// SubCount is the number of active SSE clients (for /dash/api/state).
func (h *Hub) SubCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// ServeSSE upgrades the request to a text/event-stream and streams hub
// events as SSE frames. A heartbeat comment keeps proxies from closing
// idle connections.
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, hdr func() map[string]any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := h.Subscribe()
	defer h.Unsubscribe(ch)

	// Send an initial snapshot so the client has state immediately.
	if hdr != nil {
		writeSSE(w, flusher, "init", hdr())
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case msg, open := <-ch:
			if !open {
				return
			}
			writeSSE(w, flusher, msgType(msg), msg)
		}
	}
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, msg any) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	if _, err := w.Write([]byte("event: " + event + "\ndata: ")); err != nil {
		return
	}
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
	flusher.Flush()
}

func msgType(m map[string]any) string {
	if t, ok := m["type"].(string); ok {
		return t
	}
	return "event"
}
