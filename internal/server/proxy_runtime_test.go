package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeTestServer(t *testing.T, cfg *Config) *Server {
	t.Helper()
	cfg.applyDefaults()
	backends, err := cfg.resolveBackends()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{recent: NewRecentLog(100), reasonCache: newReasoningCache(100)}
	s.rt.Store(newRuntimeState(cfg, backends))
	s.initGateway()
	t.Cleanup(func() {
		for _, b := range backends {
			b.client.CloseIdleConnections()
		}
	})
	return s
}

func runtimeConfig(url string) *Config {
	return &Config{DefaultProvider: "primary", ModelMap: map[string]string{"test": "primary:upstream-test"},
		Providers: map[string]*ProviderConfig{"primary": {BaseURL: url, Keys: []KeyEntry{{ID: "credential-1", Key: "secret-credential-1"}}}}}
}

func runtimeCall(s *Server, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return w
}

func runtimeRecord(t *testing.T, s *Server) ReqRecord {
	t.Helper()
	records := s.recent.Snapshot(100)
	if len(records) != 1 {
		t.Fatalf("logical requests=%d, want exactly one", len(records))
	}
	return records[0]
}

func TestGatewayRejectsUnknownDisabledAndOversizedRequests(t *testing.T) {
	var called atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Add(1); io.WriteString(w, `{}`) }))
	defer upstream.Close()
	for _, tc := range []struct {
		name, body                      string
		disabledModel, disabledProvider bool
		bodyLimit                       int64
		status                          int
	}{
		{name: "unknown model", body: `{"model":"unknown"}`, status: 404},
		{name: "disabled model", body: `{"model":"test"}`, disabledModel: true, status: 404},
		{name: "disabled provider", body: `{"model":"test"}`, disabledProvider: true, status: 503},
		{name: "oversized body", body: `{"model":"test","messages":[{"content":"private-prompt"}]}`, bodyLimit: 20, status: 413},
		{name: "invalid JSON", body: `{"model":"test"`, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := runtimeConfig(upstream.URL)
			cfg.DisabledModels = map[string]bool{"test": tc.disabledModel}
			cfg.Providers["primary"].Disabled = tc.disabledProvider
			cfg.Limits.MaxBodyBytes = tc.bodyLimit
			s := runtimeTestServer(t, cfg)
			w := runtimeCall(s, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status=%d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			rec := runtimeRecord(t, s)
			if rec.OK || len(rec.AttemptLog) != 0 || rec.UsageKnown || rec.CostKnown {
				t.Fatalf("rejected request recorded as upstream success: %+v", rec)
			}
			if rec.ID == "" || rec.ID != w.Header().Get("X-Request-ID") {
				t.Fatal("request attribution missing")
			}
		})
	}
	if called.Load() != 0 {
		t.Fatalf("rejected requests reached upstream %d times", called.Load())
	}
}

func TestGatewayExplicitFallbackAndProviderScopedTransforms(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	var upstreamRequestID string
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"private-prompt secret-credential-1"}}`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalls.Add(1)
		upstreamRequestID = r.Header.Get("X-Request-ID")
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "fallback-model" || len(body.Messages) != 1 || body.Messages[0].Role != "system" {
			t.Errorf("fallback transformation=%+v", body)
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("X-Csrf-Token") != "" {
			t.Error("management credentials forwarded")
		}
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer fallback.Close()
	cfg := runtimeConfig(primary.URL)
	cfg.Providers["freeinference"] = &ProviderConfig{BaseURL: fallback.URL, NoAuth: true}
	cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "freeinference", Upstream: "fallback-model"}}}
	s := runtimeTestServer(t, cfg)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"developer","content":"private-prompt"}]}`))
	r.Header.Set("Cookie", "verloc_session=private-session")
	r.Header.Set("X-CSRF-Token", "private-csrf")
	s.handleChat(w, r)
	if w.Code != 200 || primaryCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("status=%d primary=%d fallback=%d", w.Code, primaryCalls.Load(), fallbackCalls.Load())
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || !rec.UsageKnown || rec.CostKnown || rec.TokIn != 7 || rec.TokOut != 3 || rec.Failover != 1 || len(rec.AttemptLog) != 2 {
		t.Fatalf("record=%+v", rec)
	}
	if rec.Backend != "freeinference" || rec.Upstream != "fallback-model" || rec.ID != upstreamRequestID {
		t.Fatalf("attribution=%+v request-id=%q", rec, upstreamRequestID)
	}
	encoded, _ := json.Marshal(rec)
	if strings.Contains(string(encoded), "private-prompt") || strings.Contains(string(encoded), "secret-credential") {
		t.Fatalf("sensitive content in record: %s", encoded)
	}
}

func TestGatewayAttemptsBoundedAndDuplicateKeysDoNotSpin(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprintf("duplicate=%v", duplicate), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
			defer upstream.Close()
			cfg := runtimeConfig(upstream.URL)
			cfg.Limits.MaxAttempts = 2
			cfg.Providers["primary"].Keys = nil
			for i := range 6 {
				key := fmt.Sprintf("credential-%d", i)
				if duplicate {
					key = "same-credential"
				}
				cfg.Providers["primary"].Keys = append(cfg.Providers["primary"].Keys, KeyEntry{Key: key})
			}
			s := runtimeTestServer(t, cfg)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- runtimeCall(s, `{"model":"test"}`) }()
			select {
			case w := <-done:
				if w.Code != 503 {
					t.Fatalf("status=%d", w.Code)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("exhausted pool spun instead of terminating")
			}
			want := int32(2)
			if duplicate {
				want = 1
			}
			if calls.Load() != want {
				t.Fatalf("calls=%d want %d", calls.Load(), want)
			}
			if len(runtimeRecord(t, s).AttemptLog) != int(want) {
				t.Fatal("attempt log does not match upstream requests")
			}
		})
	}
}

func TestGatewayRetryElapsedBudget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	defer upstream.Close()
	cfg := runtimeConfig(upstream.URL)
	cfg.Limits.RetryBudgetSeconds = 1
	s := runtimeTestServer(t, cfg)
	start := time.Now()
	w := runtimeCall(s, `{"model":"test"}`)
	if w.Code != 504 || time.Since(start) > 3*time.Second {
		t.Fatalf("status=%d elapsed=%v", w.Code, time.Since(start))
	}
	rec := runtimeRecord(t, s)
	if rec.OK || rec.Outcome != "retry_budget_exhausted" {
		t.Fatalf("record=%+v", rec)
	}
}

func TestGatewayTruncatedStreamCannotFallbackAfterFlush(t *testing.T) {
	var fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"private-completion\"}}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fallbackCalls.Add(1) }))
	defer fallback.Close()
	cfg := runtimeConfig(primary.URL)
	cfg.Providers["fallback"] = &ProviderConfig{BaseURL: fallback.URL, NoAuth: true}
	cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "fallback", Upstream: "fallback"}}}
	s := runtimeTestServer(t, cfg)
	w := runtimeCall(s, `{"model":"test","stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "private-completion") {
		t.Fatalf("stream not forwarded: %d %s", w.Code, w.Body.String())
	}
	if fallbackCalls.Load() != 0 {
		t.Fatal("fallback happened after client flush")
	}
	rec := runtimeRecord(t, s)
	if rec.OK || rec.Outcome != "interrupted" || rec.Status != 502 || rec.UsageKnown {
		t.Fatalf("truncated stream treated as success: %+v", rec)
	}
	encoded, _ := json.Marshal(rec)
	if strings.Contains(string(encoded), "private-completion") {
		t.Fatal("completion persisted in analytics")
	}
}

func TestGatewayEmptyStreamCanFallbackBeforeFlush(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n")
	}))
	defer fallback.Close()
	cfg := runtimeConfig(primary.URL)
	cfg.Providers["fallback"] = &ProviderConfig{BaseURL: fallback.URL, NoAuth: true}
	cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "fallback", Upstream: "fallback"}}}
	s := runtimeTestServer(t, cfg)
	w := runtimeCall(s, `{"model":"test","stream":true}`)
	rec := runtimeRecord(t, s)
	if w.Code != 200 || !rec.OK || !rec.UsageKnown || len(rec.AttemptLog) != 2 || rec.Backend != "fallback" {
		t.Fatalf("empty-stream fallback: status=%d record=%+v", w.Code, rec)
	}
}

func TestGatewayStreamIdleTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	cfg := runtimeConfig(upstream.URL)
	cfg.Limits.StreamIdleTimeoutSeconds = 1
	s := runtimeTestServer(t, cfg)
	start := time.Now()
	runtimeCall(s, `{"model":"test","stream":true}`)
	rec := runtimeRecord(t, s)
	if rec.OK || rec.Outcome != "idle_timeout" || time.Since(start) > 3*time.Second {
		t.Fatalf("idle stream: %+v elapsed=%v", rec, time.Since(start))
	}
}

func TestGatewayCanceledRequestReleasesAdmission(t *testing.T) {
	entered := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer upstream.Close()
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() { s.handleChat(httptest.NewRecorder(), r); close(done) }()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not return")
	}
	rec := runtimeRecord(t, s)
	if rec.OK || rec.Outcome != "canceled" || rec.Status != 499 {
		t.Fatalf("canceled request=%+v", rec)
	}
	s.admissionMu.Lock()
	active, queued := s.active, s.queued
	s.admissionMu.Unlock()
	if active != 0 || queued != 0 {
		t.Fatalf("admission leaked active=%d queued=%d", active, queued)
	}
}

func TestGatewayQueueBoundCancellationAndLiveResize(t *testing.T) {
	s := &Server{}
	limits := GatewayLimits{MaxInFlight: 1, MaxQueued: 1, QueueTimeoutSeconds: 5}
	limits.applyDefaults()
	if err := s.acquireGateway(context.Background(), limits); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	queuedDone := make(chan error, 1)
	go func() { queuedDone <- s.acquireGateway(ctx, limits) }()
	deadline := time.Now().Add(time.Second)
	for {
		s.admissionMu.Lock()
		n := s.queued
		s.admissionMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request did not enter queue")
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.acquireGateway(context.Background(), limits); err == nil {
		t.Fatal("full queue admitted another request")
	}
	cancel()
	if err := <-queuedDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation=%v", err)
	}
	// Increasing capacity wakes existing queued requests without replacing the gate.
	go func() { queuedDone <- s.acquireGateway(context.Background(), limits) }()
	deadline = time.Now().Add(time.Second)
	for {
		s.admissionMu.Lock()
		n := s.queued
		s.admissionMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replacement request did not enter queue")
		}
		time.Sleep(time.Millisecond)
	}
	limits.MaxInFlight = 2
	s.rt.Store(&runtimeState{cfg: &Config{Limits: limits}})
	s.gatewayConfigChanged()
	select {
	case err := <-queuedDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("live capacity increase did not wake queue")
	}
	s.releaseGateway()
	s.releaseGateway()
	if s.active != 0 || s.queued != 0 {
		t.Fatalf("leaked gate state active=%d queued=%d", s.active, s.queued)
	}
}

func TestStreamCompletionAndUsageProtocols(t *testing.T) {
	for _, tc := range []struct {
		name, payload  string
		success, known bool
		in, out, cache uint64
	}{
		{"chat", "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":1}}}\n\ndata: [DONE]\n\n", true, true, 4, 2, 1},
		{"responses", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":8,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":2}}}}\n\n", true, true, 8, 3, 2},
		{"no usage", "data: [DONE]\n\n", true, false, 0, 0, 0},
		{"explicit zero usage", "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", true, true, 0, 0, 0},
		{"truncated", "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", false, false, 0, 0, 0},
		{"failed response", "data: {\"type\":\"response.failed\"}\n\n", false, false, 0, 0, 0},
		{"known usage before interruption", "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1}}\n\n", false, true, 4, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var in, out, cache uint64
			known := false
			err := streamCopyWithUsage(httptest.NewRecorder(), strings.NewReader(tc.payload), func(i, o, c uint64) { in, out, cache, known = i, o, c, true }, nil, nil)
			if (err == nil) != tc.success || known != tc.known || in != tc.in || out != tc.out || cache != tc.cache {
				t.Fatalf("err=%v usage=(%d,%d,%d,%v)", err, in, out, cache, known)
			}
		})
	}
}

func TestGatewayFirstRunAndCredentialIdentity(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	if backends, err := cfg.resolveBackends(); err != nil || len(backends) != 0 {
		t.Fatalf("empty setup: backends=%v err=%v", backends, err)
	}
	for _, key := range []KeyEntry{{ID: "stable-id", Key: "private-key"}, {ID: "stable-id", AccountID: "account", Key: "private-key"}, {Key: "legacy-key"}} {
		encoded, err := json.Marshal(key)
		if err != nil {
			t.Fatal(err)
		}
		var decoded KeyEntry
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded != key {
			t.Fatalf("credential identity lost: got=%+v want=%+v", decoded, key)
		}
	}
	for _, limits := range []GatewayLimits{{MaxBodyBytes: -1}, {MaxAttempts: 33}, {RetryBudgetSeconds: 301}, {QueueTimeoutSeconds: 121}, {MaxInFlight: 4097}, {MaxQueued: 16385}, {StreamIdleTimeoutSeconds: 601}} {
		if limits.Validate() == nil {
			t.Fatalf("unsafe limits accepted: %+v", limits)
		}
	}
	cfg.Providers["disabled"] = &ProviderConfig{Disabled: true, BaseURL: "https://example.invalid"}
	cfg.ModelMap["disabled-model"] = "disabled:upstream"
	if _, err := cfg.resolveBackends(); err != nil {
		t.Fatalf("disabled empty credentials rejected: %v", err)
	}
}

func TestGatewayNonstreamInterruptionFallbackBeforeHeaders(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, `{"choices":`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer fallback.Close()
	cfg := runtimeConfig(primary.URL)
	cfg.Providers["fallback"] = &ProviderConfig{BaseURL: fallback.URL, NoAuth: true}
	cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "fallback", Upstream: "fallback"}}}
	s := runtimeTestServer(t, cfg)
	w := runtimeCall(s, `{"model":"test"}`)
	rec := runtimeRecord(t, s)
	if w.Code != 200 || !json.Valid(w.Body.Bytes()) || !rec.OK || rec.Backend != "fallback" || len(rec.AttemptLog) != 2 {
		t.Fatalf("nonstream fallback: status=%d body=%s record=%+v", w.Code, w.Body.String(), rec)
	}
}

func TestGatewayRetryBudgetDoesNotLimitHealthyStreamLifetime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for range 6 {
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
		}
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := runtimeConfig(upstream.URL)
	cfg.Limits.RetryBudgetSeconds = 1
	cfg.Limits.StreamIdleTimeoutSeconds = 2
	s := runtimeTestServer(t, cfg)
	start := time.Now()
	runtimeCall(s, `{"model":"test","stream":true}`)
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.UsageKnown || time.Since(start) < time.Second {
		t.Fatalf("healthy stream was limited by retry budget: %+v elapsed=%v", rec, time.Since(start))
	}
}

type disconnectedStreamWriter struct{ headers http.Header }

func (w *disconnectedStreamWriter) Header() http.Header       { return w.headers }
func (w *disconnectedStreamWriter) WriteHeader(int)           {}
func (w *disconnectedStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestGatewayClientWriteFailureIsCanceledNotSuccessful(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	s := runtimeTestServer(t, runtimeConfig(upstream.URL))
	w := &disconnectedStreamWriter{headers: make(http.Header)}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true}`))
	s.handleChat(w, r)
	rec := runtimeRecord(t, s)
	if rec.OK || rec.Status != 499 || rec.Outcome != "canceled" || !rec.UsageKnown || rec.TokIn != 5 || rec.TokOut != 2 {
		t.Fatalf("client disconnect accounting: %+v", rec)
	}
	if len(rec.AttemptLog) != 1 || rec.AttemptLog[0].Outcome != "canceled" {
		t.Fatalf("attempts=%+v", rec.AttemptLog)
	}
	s.admissionMu.Lock()
	active := s.active
	s.admissionMu.Unlock()
	if active != 0 {
		t.Fatalf("disconnect leaked %d admissions", active)
	}
}

func TestModelPricingCoverageDistinguishesUnknownFromFree(t *testing.T) {
	for _, tc := range []struct {
		json          string
		known         bool
		input, output float64
	}{
		{`{"name":"Unknown"}`, false, 0, 0},
		{`{"pricing_input":0}`, false, 0, 0},
		{`{"pricing_input":0,"pricing_output":0}`, true, 0, 0},
		{`{"pricing_input":0.5,"pricing_output":1.5}`, true, 0.5, 1.5},
		{`{"pricing_input":0,"pricing_output":0,"pricing_known":false}`, false, 0, 0},
		{`{"pricing_input":null,"pricing_output":0}`, false, 0, 0},
	} {
		var meta ModelMeta
		if err := json.Unmarshal([]byte(tc.json), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.PricingKnown == nil || *meta.PricingKnown != tc.known || meta.PricingInput != tc.input || meta.PricingOutput != tc.output {
			t.Fatalf("pricing presence for %s: %+v", tc.json, meta)
		}
		data, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip ModelMeta
		if err := json.Unmarshal(data, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if roundTrip.PricingKnown == nil || *roundTrip.PricingKnown != tc.known {
			t.Fatalf("coverage lost through persistence: %s", data)
		}
	}
}
