package server

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A1: a corrupt rollup blob must be rejected before any count-sized
// allocation. Before the fix, a 6-byte blob claiming n=8,000,000 entries
// grew the heap by ~1 GB.
func TestDecodeMinutesRejectsOversizedCounts(t *testing.T) {
	// Version tag + uvarint(1<<40) and nothing else.
	blob := []byte{dimCodecVersion, 0x80, 0x80, 0x80, 0x80, 0x80, 0x04}
	var models map[string]deltaModel
	var backends map[string]deltaBackend
	var hist, ttft []uint64
	if err := decodeMinutes(blob, &models, &backends, &hist, &ttft); err == nil {
		t.Fatal("oversized model count decoded without error")
	}
	// A valid empty blob still decodes cleanly.
	if err := decodeMinutes([]byte{dimCodecVersion, 0, 0, 0, 0}, &models, &backends, &hist, &ttft); err != nil {
		t.Fatalf("empty blob: %v", err)
	}
}

// A1: encode/decode round-trip still works after the bounds checks.
func TestRollupCodecRoundTripUnchanged(t *testing.T) {
	models := map[string]deltaModel{"nv/x": {Requests: 3, TokIn: 10, Cost: 1.5}}
	backends := map[string]deltaBackend{"nvidia": {Requests: 3, Err: 1, Cost: 1.5}}
	blob := encodeMinutes(&bytes.Buffer{}, models, backends, []uint64{1, 2}, []uint64{4})
	var m map[string]deltaModel
	var b map[string]deltaBackend
	var h, th []uint64
	if err := decodeMinutes(blob, &m, &b, &h, &th); err != nil {
		t.Fatal(err)
	}
	if m["nv/x"].Requests != 3 || b["nvidia"].Err != 1 || len(h) != 2 || len(th) != 1 {
		t.Fatalf("round trip mismatch: %v %v %v %v", m, b, h, th)
	}
}

// A2: an upstream that never sends a blank-line event boundary must flush
// partial events instead of buffering without limit, and a legitimately
// large single event must still complete (flushed in pieces).
func TestStreamEventBufferIsBounded(t *testing.T) {
	chunk := "data: " + strings.Repeat("x", 4096) + "\n"
	src := strings.NewReader(strings.Repeat(chunk, (maxEventBufBytes/len(chunk))+10))
	rec := httptest.NewRecorder()
	_ = streamCopyWithUsage(rec, src, nil, func() {}, streamOptions{})
	if rec.Body.Len() == 0 {
		t.Fatal("oversized event was never flushed to the client")
	}
	// A complete oversized event followed by [DONE] succeeds end to end.
	big := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("a", 3<<20) + "\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	rec2 := httptest.NewRecorder()
	if err := streamCopyWithUsage(rec2, strings.NewReader(big), nil, func() {}, streamOptions{}); err != nil {
		t.Fatalf("large legit event failed: %v", err)
	}
	if rec2.Body.Len() < 3<<20 {
		t.Fatalf("large event truncated: %d bytes", rec2.Body.Len())
	}
}

// D1: scoped alert data must carry Err/Failover end to end, or model-scoped
// error_rate and any scoped failover_rate rule can never fire.
func TestScopeTicksCarryErrAndFailover(t *testing.T) {
	td := TickDelta{
		Models:   map[string]setDelta{"nv/x": {Req: 10, Err: 2, Failover: 3}},
		Backends: map[string]setDelta{"nvidia": {Req: 10, Err: 2, Failover: 3}},
	}
	models, backends := scopeTicks(deltaFromTick(td))
	if got := models["nv/x"]; got.Err != 2 || got.Failover != 3 {
		t.Fatalf("model scope lost err/failover: %+v", got)
	}
	if got := backends["nvidia"]; got.Err != 2 || got.Failover != 3 {
		t.Fatalf("backend scope lost err/failover: %+v", got)
	}
}

// D2: telemetry_errors must be delta-based. With no store the value is 0;
// the baseline behaviour is pinned with a real store in store tests.
func TestTelemetryErrorDeltaWithoutStore(t *testing.T) {
	e := NewAlertEngine(nil, nil, nil)
	if v := e.telemetryErrorDelta(); v != 0 {
		t.Fatalf("delta without store = %v, want 0", v)
	}
}

// D3: the default TTFT rule renders with a unit.
func TestAlertMessageTTFTUnit(t *testing.T) {
	r := &AlertRule{Metric: "ttft_p95", Scope: "global", Threshold: 15000}
	if msg := alertMessage(r, 20000); !strings.Contains(msg, " ms") {
		t.Fatalf("ttft_p95 message lacks unit: %q", msg)
	}
}

// C1: dynamic JSON responses are gzipped only when accepted, never for
// streaming paths.
func TestGzipMiddleware(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat(`{"ok":true}`, 50)))
	}))
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("JSON response was not gzipped")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatal("gzipped body does not round-trip")
	}

	// Streaming path: never compressed even when accepted.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("streaming path was gzipped")
	}
}

// C2/C3: hashed assets are immutable, index.html revalidates, ETag works.
func TestDashAssetCaching(t *testing.T) {
	s := &Server{}
	var jsPath string
	for p := range dashAssets {
		if strings.HasPrefix(p, "assets/") && strings.HasSuffix(p, ".js") {
			jsPath = p
			break
		}
	}
	if jsPath == "" {
		t.Skip("no JS asset in this build")
	}
	rec := httptest.NewRecorder()
	s.handleDashUI(rec, httptest.NewRequest("GET", "/dash/"+jsPath, nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed asset Cache-Control = %q, want immutable", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("hashed asset has no ETag")
	}
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dash/"+jsPath, nil)
	req.Header.Set("If-None-Match", etag)
	s.handleDashUI(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: got %d, want 304", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	s.handleDashUI(rec3, httptest.NewRequest("GET", "/dash/", nil))
	if cc := rec3.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("index.html Cache-Control = %q, must revalidate", cc)
	}
}
