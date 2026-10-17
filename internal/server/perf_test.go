package server

// Performance benchmarks for verloc's request hot path and telemetry.
//
// These are the regression net for the "blazingly fast" goal: every
// optimization must show a before/after delta in ns/op, B/op and allocs/op.
// They are committed deliberately so the next change has a baseline to beat.
//
//	make bench          # fast set (no DB seeding)
//	make bench-scale    # + SQLite scaling probes (slower; seeds synthetic history)
//	make bench-race     # fast set under -race
//
// Benchmarks never run as part of `go test ./...` — only with -bench.
// The SQL scaling probes additionally skip under -short so `make bench` stays fast.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---------- fixtures ----------

// perfChatBody builds a realistic agent-turn payload: rotating user turns,
// assistant tool calls carrying reasoning_content, and tool results. This
// exercises exactly the fields the Mistral remap and reasoning replay touch.
func perfChatBody(tb testing.TB, turns int) []byte {
	tb.Helper()
	msgs := make([]map[string]any, 0, turns*2)
	for i := range turns {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("x", 900)})
		if i%4 == 0 {
			msgs = append(msgs, map[string]any{
				"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{
					"id": fmt.Sprintf("call_%d", i), "type": "function",
					"function": map[string]any{"name": "read_file", "arguments": `{"path":"a.go"}`},
				}},
				"reasoning_content": strings.Repeat("r", 500),
			})
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i),
				"content": strings.Repeat("y", 400),
			})
		}
	}
	body, _ := json.Marshal(map[string]any{
		"model": "test", "messages": msgs, "stream": true, "max_tokens": 8192, "temperature": 0.7,
	})
	return body
}

// perfSSE is a synthetic 200-chunk stream terminated with usage + [DONE].
func perfSSE() string {
	var sb strings.Builder
	for range 200 {
		sb.WriteString(`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"token "},"finish_reason":null}]}` + "\n\n")
	}
	sb.WriteString(`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":200}}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

var (
	perfBody   = perfChatBody(&testing.T{}, 30) // ~100 KB
	perfStream = perfSSE()
	perfChunk  = []byte(`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning":"thinking about it","content":"hello "},"finish_reason":null}]}`)
)

func perfUpstreamNonStream(tb testing.TB) *httptest.Server {
	tb.Helper()
	content := strings.Repeat("word ", 7000) // ~35 KB response body
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":200}}`, content)
	}))
}

func perfUpstreamStream(tb testing.TB) *httptest.Server {
	tb.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, perfStream)
	}))
}

// perfServer builds a runtime with one provider, no analytics store, so the
// benchmark isolates the proxy path rather than the dashboard writer.
func perfServer(tb testing.TB, url string, strip bool) *Server {
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
	backends, err := cfg.resolveBackends()
	if err != nil {
		tb.Fatal(err)
	}
	s := &Server{metrics: NewMetrics(), recent: NewRecentLog(1000), reasonCache: newReasoningCache(maxReasoningEntries)}
	s.rt.Store(newRuntimeState(cfg, backends))
	s.initGateway()
	tb.Cleanup(func() {
		for _, b := range backends {
			b.client.CloseIdleConnections()
		}
	})
	return s
}

func perfCall(s *Server, body []byte) {
	w := httptest.NewRecorder()
	s.handleChat(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
}

// ---------- request body transform chain ----------

func BenchmarkRemapBodyOpenAI(b *testing.B) {
	b.SetBytes(int64(len(perfBody)))
	for b.Loop() {
		_ = remapBody(perfBody, "upstream-model", false, "nvidia")
	}
}

func BenchmarkRemapBodyMistral(b *testing.B) {
	b.SetBytes(int64(len(perfBody)))
	for b.Loop() {
		_ = remapBody(perfBody, "upstream-model", true, "mistral")
	}
}

func BenchmarkStripReasoningContent(b *testing.B) {
	b.SetBytes(int64(len(perfBody)))
	for b.Loop() {
		_ = stripReasoningContent(perfBody, true)
	}
}

func BenchmarkEnsureReasoningReplay(b *testing.B) {
	cache := newReasoningCache(maxReasoningEntries)
	b.SetBytes(int64(len(perfBody)))
	for b.Loop() {
		_ = ensureReasoningReplay(perfBody, cache)
	}
}

func BenchmarkCapMaxTokens(b *testing.B) {
	b.SetBytes(int64(len(perfBody)))
	for b.Loop() {
		_ = capMaxTokens(perfBody, 32768)
	}
}

// BenchmarkRequestTransformChain is the full pre-dispatch cost for a
// strip=true backend (the default): the number Stage 3 targets.
func BenchmarkRequestTransformChain(b *testing.B) {
	b.Run("strip", func(b *testing.B) {
		b.SetBytes(int64(len(perfBody)))
		for b.Loop() {
			m := remapBody(perfBody, "up", false, "nvidia")
			m = stripReasoningContent(m, true)
			_ = capMaxTokens(m, 32768)
		}
	})
	b.Run("replay", func(b *testing.B) {
		cache := newReasoningCache(maxReasoningEntries)
		b.SetBytes(int64(len(perfBody)))
		for b.Loop() {
			m := remapBody(perfBody, "up", false, "opencode")
			m = stripReasoningContent(m, false)
			m = ensureReasoningReplay(m, cache)
			_ = capMaxTokens(m, 32768)
		}
	})
	b.Run("metadata", func(b *testing.B) {
		b.SetBytes(int64(len(perfBody)))
		for b.Loop() {
			var md requestMetadata
			_ = json.Unmarshal(perfBody, &md)
		}
	})
}

// ---------- streaming chunk path ----------

func BenchmarkAliasReasoningInChunk(b *testing.B) {
	for b.Loop() {
		_, _ = aliasReasoningInChunk(perfChunk)
	}
}

func BenchmarkSSEChunkIngest(b *testing.B) {
	acc := &sseReasoningAcc{}
	for b.Loop() {
		acc.ingest(perfChunk)
		_, _ = aliasReasoningInChunk(perfChunk)
	}
}

// ---------- telemetry ----------

func perfMetrics(models, backends int) (*Metrics, map[string]*backend) {
	mt := NewMetrics()
	for i := range models {
		for range 50 {
			mt.RecordRequest(ReqRecord{
				Model: fmt.Sprintf("model-%d", i), Backend: fmt.Sprintf("backend-%d", i%backends),
				Stream: true, Status: 200, OK: true, UsageKnown: true, CostKnown: true,
				TokIn: 1000, TokOut: 200, TokCache: 100, Cost: 0.001, LatMs: 1200, TTFTMs: 300,
			})
		}
	}
	bk := make(map[string]*backend, backends)
	for i := range backends {
		name := fmt.Sprintf("backend-%d", i)
		bk[name] = &backend{name: name, pool: NewKeyPool([]KeyEntry{{Key: "k1"}, {Key: "k2"}}, 0)}
	}
	return mt, bk
}

// BenchmarkMetricsSnapshot matches production shape: 60 models x 19 backends.
// Called on every /dash/api/state poll and twice per ticker flush.
func BenchmarkMetricsSnapshot(b *testing.B) {
	for _, sz := range []struct{ models, backends int }{{10, 5}, {60, 19}, {200, 40}} {
		b.Run(fmt.Sprintf("m%d_b%d", sz.models, sz.backends), func(b *testing.B) {
			mt, bk := perfMetrics(sz.models, sz.backends)
			b.ResetTimer()
			for b.Loop() {
				_ = mt.Snapshot(bk)
			}
		})
	}
}

// BenchmarkTickerFlushCost is the per-tick cost the ticker pays today:
// two snapshots plus a delta diff.
func BenchmarkTickerFlushCost(b *testing.B) {
	mt, bk := perfMetrics(60, 19)
	prev := mt.Snapshot(bk)
	b.ResetTimer()
	for b.Loop() {
		cur := mt.Snapshot(bk)
		_ = computeDelta(prev, cur)
	}
}

// perfAggregator seeds a 24h ring with ticks whose timestamps are CURRENT, so
// the alert rules' windows actually contain data. A stale seed makes every
// window filter the ring out and under-measures the real 5s evaluation path.
func perfAggregator() *Aggregator {
	a := NewAggregator(28800, 60*24)
	now := time.Now().UnixMilli()
	for i := range 28800 {
		ts := now - int64(28800-i-1)*3000 // oldest..newest, ending now
		a.Feed(tickAgg{TS: ts, Req: 10, Err: 1, LatN: 10, LatSumMs: 100,
			LatHist: [len(latBuckets) + 1]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}, nil, nil)
	}
	return a
}

func BenchmarkAggregatorSum24h(b *testing.B) {
	a := perfAggregator()
	b.ResetTimer()
	for b.Loop() {
		_ = a.Sum(3600)
	}
}

// BenchmarkAlertEvalAllRules is the full 5s alert evaluation over a 24h ring
// with in-window data (the production shape).
func BenchmarkAlertEvalAllRules(b *testing.B) {
	a := perfAggregator()
	e := &AlertEngine{agg: a, cooldown: map[string]time.Time{}, rules: defaultAlertRules(), recentN: 500}
	b.ResetTimer()
	for b.Loop() {
		e.Eval(time.Now(), func() int { return 0 })
	}
}

func perfRecord() ReqRecord {
	return ReqRecord{TS: 1, ID: "abc", Model: "m", Upstream: "u", Backend: "b", OK: true,
		Status: 200, Outcome: "completed", UsageKnown: true, CostKnown: true,
		TokIn: 100, TokOut: 200, TokCache: 50, Cost: 0.001, LatMs: 900,
		AttemptLog: []RequestAttempt{{Backend: "b", Upstream: "u", Status: 200, Outcome: "completed"}}}
}

func BenchmarkRecentLogAdd(b *testing.B) {
	l := NewRecentLog(10000)
	rec := perfRecord()
	b.ResetTimer()
	for b.Loop() {
		l.Add(rec)
	}
}

func BenchmarkSanitizeRecord(b *testing.B) {
	rec := perfRecord()
	for b.Loop() {
		_ = sanitizeRecord(rec)
	}
}

// ---------- key pool / admission ----------

func BenchmarkKeyPoolNextUntried(b *testing.B) {
	keys := make([]KeyEntry, 10)
	for i := range keys {
		keys[i] = KeyEntry{Key: fmt.Sprintf("key-%d", i)}
	}
	p := NewKeyPool(keys, 0)
	tried := make(map[credentialKey]bool, 4)
	for b.Loop() {
		clear(tried)
		_ = p.NextUntried(tried)
	}
}

func BenchmarkKeyPoolStats(b *testing.B) {
	keys := make([]KeyEntry, 10)
	for i := range keys {
		keys[i] = KeyEntry{Key: fmt.Sprintf("key-%d", i)}
	}
	p := NewKeyPool(keys, 0)
	b.ResetTimer()
	for b.Loop() {
		_ = p.Stats()
	}
}

// BenchmarkAdmissionGate isolates the global admission mutex.
func BenchmarkAdmissionGate(b *testing.B) {
	s := perfServer(b, "http://127.0.0.1:1", true)
	limits := s.runtime().cfg.Limits
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := s.acquireGateway(context.Background(), limits); err != nil {
				b.Fatal(err)
			}
			s.releaseGateway()
		}
	})
}

// ---------- end-to-end ----------

func BenchmarkEndToEndNonStream100KB(b *testing.B) {
	up := perfUpstreamNonStream(b)
	defer up.Close()
	s := perfServer(b, up.URL, true)
	body := []byte(strings.Replace(string(perfBody), `"stream":true`, `"stream":false`, 1))
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		perfCall(s, body)
	}
}

func BenchmarkEndToEndStream100KB(b *testing.B) {
	up := perfUpstreamStream(b)
	defer up.Close()
	s := perfServer(b, up.URL, true)
	b.SetBytes(int64(len(perfBody)))
	b.ResetTimer()
	for b.Loop() {
		perfCall(s, perfBody)
	}
}

// BenchmarkEndToEndStreamReplay covers the strip=false path where the
// reasoning alias + accumulator are active.
func BenchmarkEndToEndStreamReplay(b *testing.B) {
	up := perfUpstreamStream(b)
	defer up.Close()
	s := perfServer(b, up.URL, false)
	b.SetBytes(int64(len(perfBody)))
	b.ResetTimer()
	for b.Loop() {
		perfCall(s, perfBody)
	}
}

func BenchmarkEndToEndParallelSmall(b *testing.B) {
	up := perfUpstreamNonStream(b)
	defer up.Close()
	s := perfServer(b, up.URL, true)
	body := []byte(`{"model":"test","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			perfCall(s, body)
		}
	})
}
