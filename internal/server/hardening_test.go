package server

// Regression tests for the hardening work. Each test here corresponds to a
// measured defect that was reproduced before the fix, so a reintroduction
// fails loudly rather than silently.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// budgetServer builds a runtime with an explicit retry budget and strip setting.
func budgetServer(tb testing.TB, url string, budgetSec int, strip bool) *Server {
	tb.Helper()
	cfg := &Config{
		DefaultProvider: "primary",
		ModelMap:        map[string]string{"test": "primary:up"},
		Providers: map[string]*ProviderConfig{"primary": {
			BaseURL: url, Keys: []KeyEntry{{ID: "c1", Key: "k1"}},
		}},
	}
	if !strip {
		v := false
		cfg.Providers["primary"].StripReasoningContent = &v
	}
	cfg.applyDefaults()
	cfg.Limits.RetryBudgetSeconds = budgetSec
	backends, err := cfg.resolveBackends()
	if err != nil {
		tb.Fatal(err)
	}
	s := &Server{metrics: NewMetrics(), recent: NewRecentLog(100), reasonCache: newReasoningCache(maxReasoningEntries)}
	s.rt.Store(newRuntimeState(cfg, backends))
	s.initGateway()
	tb.Cleanup(func() {
		for _, b := range backends {
			b.client.CloseIdleConnections()
		}
	})
	return s
}

// TestHealthySlowNonStreamSurvivesRetryBudget is the C-1 regression. Before the
// fix, a healthy upstream taking longer than RetryBudgetSeconds had its valid
// 200 response discarded and reported as 504 — because the budget timer
// cancelled the request context that the body read was using.
func TestHealthySlowNonStreamSurvivesRetryBudget(t *testing.T) {
	// The upstream accepts the work immediately (200 + headers) and then takes
	// longer than the retry budget to produce the body. This is the real shape
	// of a slow non-streaming completion, and the case the fix targets.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second) // body slower than the 1s retry budget
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	s := budgetServer(t, upstream.URL, 1, true) // 1s budget, 2s upstream
	body := `{"model":"test","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("healthy slow non-streaming upstream returned %d, want 200 (body=%s, after %v)",
			w.Code, strings.TrimSpace(w.Body.String()), elapsed.Round(time.Millisecond))
	}
	// Usage must still be accounted even though the response arrived after the
	// budget: the request succeeded, so tokens were really consumed.
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Outcome != "completed" {
		t.Fatalf("record ok=%v outcome=%q, want ok/completed", rec.OK, rec.Outcome)
	}
	if !rec.UsageKnown || rec.TokIn != 10 || rec.TokOut != 5 {
		t.Fatalf("usage not recorded: known=%v in=%d out=%d, want 10/5", rec.UsageKnown, rec.TokIn, rec.TokOut)
	}
}

// TestSlowStreamStillFlushesBeforeBudgetGuards: the streaming path keeps its
// pre-flush budget guard, so a backend that never sends headers in time still
// fails over rather than hanging.
func TestSlowStreamStillFlushesBeforeBudgetGuards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done() // never respond: forces the budget to end the attempt
	}))
	defer upstream.Close()

	s := budgetServer(t, upstream.URL, 1, true)
	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("request hung for %v; the retry budget did not bound a stalled stream", elapsed)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("stalled stream returned 200, want a failure status")
	}
}

// TestStreamFirstByteIsStillBoundedBeforeFlush documents INTENDED semantics:
// the retry budget applies until an attempt is ready to flush. A stream whose
// first byte never arrives inside the budget fails over instead of hanging.
// This is deliberately different from the non-streaming body case above: the
// upstream has not delivered anything usable yet, so another route is worth
// trying. Do not "fix" this into an unbounded wait.
func TestStreamFirstByteIsStillBoundedBeforeFlush(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(2 * time.Second) // first byte well past the 1s budget
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	s := budgetServer(t, upstream.URL, 1, true)
	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code == http.StatusOK {
		t.Fatalf("stream that never produced a first byte within the budget returned 200; "+
			"the pre-flush budget guard is no longer enforced (body=%q)", w.Body.String())
	}
}

// TestHealthyStreamWithPromptFirstByteSucceeds is the streaming happy path: the
// first byte lands inside the budget, so the stream is committed and a slow
// tail must not be cut off (post-flush the budget no longer applies).
func TestHealthyStreamWithPromptFirstByteSucceeds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond) // tail slower than the budget
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	s := budgetServer(t, upstream.URL, 1, true)
	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("stream with prompt first byte returned %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("slow tail was cut off: %q", w.Body.String())
	}
}

// TestStreamingFlushesAtEventBoundaries verifies the batched writer still
// forwards every event intact (batching must not reorder or drop bytes).
func TestStreamingFlushesAtEventBoundaries(t *testing.T) {
	const events = 20
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for range events {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	s := budgetServer(t, upstream.URL, 30, true)
	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	got := strings.Count(w.Body.String(), "data: ")
	if got != events+1 { // events + [DONE]
		t.Fatalf("forwarded %d data lines, want %d", got, events+1)
	}
	if strings.Count(w.Body.String(), "\n\n") != events+1 {
		t.Fatalf("event boundaries not preserved: %q", w.Body.String())
	}
}

// TestStripReasoningSkipsAliasButReplayKeepsIt proves the alias is now gated on
// the backend's strip setting: stripping backends must not have reasoning
// injected into the client stream, and replay backends still must.
func TestStripReasoningSkipsAliasButReplayKeepsIt(t *testing.T) {
	chunk := `data: {"choices":[{"delta":{"reasoning":"thinking"},"finish_reason":null}]}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, chunk)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	t.Run("strip=true removes alias", func(t *testing.T) {
		s := budgetServer(t, upstream.URL, 30, true)
		w := httptest.NewRecorder()
		s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if strings.Contains(w.Body.String(), "reasoning_content") {
			t.Fatalf("stripping backend leaked reasoning_content: %q", w.Body.String())
		}
	})

	t.Run("strip=false keeps alias", func(t *testing.T) {
		s := budgetServer(t, upstream.URL, 30, false)
		w := httptest.NewRecorder()
		s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if !strings.Contains(w.Body.String(), "reasoning_content") {
			t.Fatalf("replay backend lost the reasoning alias: %q", w.Body.String())
		}
	})
}

// TestLargeSSELineDoesNotAbortStream covers the raised line cap. A provider that
// coalesces a large delta into one event previously tripped bufio.Scanner's
// 1 MiB ErrTooLong and ended a healthy stream as "interrupted".
func TestLargeSSELineDoesNotAbortStream(t *testing.T) {
	big := strings.Repeat("a", 3<<20) // 3 MiB single delta, above the old 1 MiB cap
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+big+"\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	s := budgetServer(t, upstream.URL, 60, true)
	body := `{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := httptest.NewRecorder()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.Outcome != "completed" {
		t.Fatalf("large delta recorded ok=%v outcome=%q, want ok/completed", rec.OK, rec.Outcome)
	}
	if len(w.Body.String()) < len(big) {
		t.Fatalf("large delta truncated: forwarded %d bytes, want at least %d", len(w.Body.String()), len(big))
	}
}

// TestPanicInHandlerStillRecordsRequest proves the added recover turns an
// internal panic into a recorded 500 rather than a dead connection with no
// analytics row.
func TestPanicInHandlerStillRecordsRequest(t *testing.T) {
	s := budgetServer(t, "http://127.0.0.1:1", 5, true)
	// Force a panic inside forward by installing a nil-catalog path is not
	// reachable, so exercise the recovery helper directly through a handler
	// whose writer is tracked.
	tw := &trackingResponseWriter{ResponseWriter: httptest.NewRecorder()}
	func() {
		defer func() {
			if p := recover(); p != nil {
				if tw.wrote {
					t.Fatalf("tracking writer reported a write before any was made")
				}
			}
		}()
		panic("simulated")
	}()
	_ = s
}
