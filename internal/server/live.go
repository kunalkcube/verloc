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

// maxHubSubscribers bounds concurrent SSE subscribers. Each one costs a
// buffered channel, a goroutine and a heartbeat ticker, and every published
// event fans out over all of them — unbounded subscriptions let one
// misbehaving dashboard client tax every request. The gateway has one
// owner and a handful of tabs, so 64 is far above real use.
const maxHubSubscribers = 64

// Subscribe registers a subscriber and returns its channel (buffered so a
// slow client drops messages instead of blocking the gateway). At the cap
// (or after Close) the returned channel is already closed.
func (h *Hub) Subscribe() chan map[string]any {
	ch := make(chan map[string]any, 256)
	h.mu.Lock()
	if h.closed || len(h.subs) >= maxHubSubscribers {
		close(ch)
	} else {
		h.subs[ch] = true
	}
	h.mu.Unlock()
	return ch
}

// registered reports whether ch is a live subscription (versus a channel
// that Subscribe returned already-closed at the cap or after Close).
func (h *Hub) registered(ch chan map[string]any) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.subs[ch]
}

func (h *Hub) Unsubscribe(ch chan map[string]any) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// Publish sends one event to all subscribers, non-blocking. With no
// subscribers it returns before doing any sanitizing work — the common
// case on the request hot path.
func (h *Hub) Publish(msg map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) == 0 {
		return
	}
	if r, ok := msg["req"].(ReqRecord); ok {
		msg["req"] = sanitizeRecord(r)
	}
	if e, ok := msg["event"].(KeyEvent); ok {
		e.Key, e.Msg = "", ""
		e.Event = safeKeyEvent(e.Event)
		msg["event"] = e
	}
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
	ch := h.Subscribe()
	if !h.registered(ch) { // hub closed or subscriber cap reached
		http.Error(w, "too many live subscribers", http.StatusServiceUnavailable)
		return
	}
	defer h.Unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Send an initial snapshot so the client has state immediately.
	if hdr != nil {
		if !writeSSE(w, flusher, "init", hdr()) {
			return
		}
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if !writeWithDeadline(w, flusher, []byte(": ping\n\n")) {
				return // client stopped reading; reap the goroutine
			}
		case msg, open := <-ch:
			if !open {
				return
			}
			if !writeSSE(w, flusher, msgType(msg), msg) {
				return
			}
		}
	}
}

// sseWriteTimeout bounds a single SSE write. Without it a client that
// stops reading fills the TCP window and blocks the writer goroutine
// forever — the request context does not fire for a stalled socket.
const sseWriteTimeout = 30 * time.Second

func writeWithDeadline(w http.ResponseWriter, flusher http.Flusher, data []byte) bool {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	if _, err := w.Write(data); err != nil {
		return false
	}
	if err := rc.Flush(); err != nil {
		flusher.Flush()
	}
	return true
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, msg any) bool {
	data, err := json.Marshal(msg)
	if err != nil {
		return true // unmarshalable event: skip it, keep the stream
	}
	frame := make([]byte, 0, len(data)+len(event)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)
	return writeWithDeadline(w, flusher, frame)
}

func msgType(m map[string]any) string {
	if t, ok := m["type"].(string); ok {
		return t
	}
	return "event"
}
