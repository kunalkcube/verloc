package server

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func analyticsRecord(ts int64, id string) ReqRecord {
	return ReqRecord{TS: ts, ID: id, Model: "model-a", Upstream: "upstream-a", Backend: "provider-a", ClientKeyID: "client-a", ClientName: "Agent", OK: true, Status: 200, Outcome: "completed", UsageKnown: true, CostKnown: true, TokIn: 10, TokOut: 5, Cost: 0.25, LatMs: 100, TTFTMs: 50}
}

func TestRequestsRestartFiltersCursorAndUnknownUsage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "requests.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	a := analyticsRecord(7200001, "a")
	b := analyticsRecord(a.TS, "b")
	b.OK = false
	b.Status = 502
	b.Outcome = "upstream_error"
	b.UsageKnown = false
	b.CostKnown = false
	b.TokIn = 999
	b.Cost = 999
	// Error bodies and RAW credentials may never persist; client IP,
	// User-Agent and the masked credential are intentional tracing
	// metadata and do.
	b.ErrMsg = "secret-upstream-body"
	b.Key = "sk-raw-credential-1234"
	b.IP = "203.0.113.7"
	b.UA = "opencode/1.0"
	b.AttemptLog = []RequestAttempt{{Backend: "provider-a", Status: 502, Outcome: "secret-upstream-body", LatMs: 5}}
	if !s.QueueRequest(a) || !s.QueueRequest(b) {
		t.Fatal("unexpected telemetry drop")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.QueueRequest(a) {
		t.Fatal("closed store accepted a request")
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f := RequestFilter{ClientKeyID: "client-a", Model: "model-a", Q: "PROVIDER-A", Limit: 1}
	page, err := s.Requests(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Requests) != 1 || page.Requests[0].ID != "b" || page.NextCursor == "" {
		t.Fatalf("stable newest-first page: %+v", page)
	}
	if page.Summary.UnknownUsage != 1 || page.Summary.UnknownCost != 1 || page.Summary.TokIn != 10 || page.Summary.Cost != 0.25 || page.Summary.Errors != 1 {
		t.Fatalf("before-limit known totals: %+v", page.Summary)
	}
	raw, _ := json.Marshal(page.Requests)
	if strings.Contains(string(raw), "secret-upstream-body") || strings.Contains(string(raw), "sk-raw-credential-1234") {
		t.Fatalf("unsafe metadata leaked: %s", raw)
	}
	var persisted string
	if err = s.db.QueryRow(`SELECT record FROM requests WHERE request_id='b'`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, "secret-upstream-body") || strings.Contains(persisted, "sk-raw-credential-1234") {
		t.Fatal("unsafe metadata persisted")
	}
	if !strings.Contains(persisted, `"ip":"203.0.113.7"`) || !strings.Contains(persisted, `"ua":"opencode/1.0"`) || !strings.Contains(persisted, `"key":"...1234"`) {
		t.Fatalf("tracing metadata missing from persisted record: %s", persisted)
	}
	c := analyticsRecord(a.TS, "c")
	if err = s.insertRequests([]ReqRecord{c}); err != nil {
		t.Fatal(err)
	}
	f.Cursor = page.NextCursor
	next, err := s.Requests(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Requests) != 1 || next.Requests[0].ID != "a" || next.NextCursor != "" {
		t.Fatalf("equal timestamp insert shifted cursor: %+v", next)
	}
	f = RequestFilter{Status: "error", Backend: "provider-a", Since: a.TS, Until: a.TS, Limit: 1}
	page, err = s.Requests(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	analytic, err := s.Analytics(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || analytic.Summary != page.Summary {
		t.Fatalf("filtered summary mismatch: %+v %+v", page.Summary, analytic.Summary)
	}
	if len(analytic.Clients) != 1 || analytic.Clients[0].UnknownUsage != 1 || len(analytic.Status) != 1 || analytic.Status[0].Status != 502 {
		t.Fatalf("missing attribution: %+v", analytic)
	}
}

func TestLegacyHourlyHistoryAndWindowBoundaries(t *testing.T) {
	s := testKeyEventStore(t)
	ctx := context.Background()
	const hour int64 = 3600000
	var latHist, ttftHist [len(latBuckets) + 1]uint64
	latHist[bucketOf(100)], latHist[bucketOf(10000)] = 1, 1
	ttftHist[bucketOf(50)], ttftHist[bucketOf(1000)] = 1, 1
	latJSON, _ := json.Marshal(latHist)
	ttftJSON, _ := json.Marshal(ttftHist)
	// Model an hourly row written by an older release, without recreating
	// the removed detail-to-aggregate compaction path.
	if _, err := s.db.Exec(`INSERT INTO request_hourly (hour,model,upstream,backend,client_key_id,client_name,status,ok,requests,errors,tok_in,tok_out,tok_cache,cost,unknown_usage,unknown_cost,lat_sum,ttft_sum,ttft_n,lat_hist,ttft_hist) VALUES (7200000,'model-a','upstream-a','provider-a','client-a','Agent',200,1,2,0,20,10,0,0.5,0,0,10100,1050,2,?,?)`, string(latJSON), string(ttftJSON)); err != nil {
		t.Fatal(err)
	}
	fresh := analyticsRecord(4*hour+300, "new")
	fresh.ClientKeyID = "client-b"
	fresh.Backend = "provider-b"
	fresh.Cost = 0.5
	if err := s.insertRequests([]ReqRecord{fresh}); err != nil {
		t.Fatal(err)
	}
	page, err := s.Requests(ctx, RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Requests[0].ID != "new" {
		t.Fatalf("legacy hourly history appeared as request detail: %+v", page)
	}
	all, err := s.Analytics(ctx, RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Summary.Requests != 3 || all.Summary.Cost != 1 || all.Coverage.AggregateOnlyRequests != 2 || len(all.Clients) != 2 || len(all.Providers) != 2 {
		t.Fatalf("rollup totals/attribution lost: %+v", all)
	}
	oldOnly, err := s.Analytics(ctx, RequestFilter{Since: 2 * hour, Until: 3 * hour, ClientKeyID: "client-a"})
	if err != nil {
		t.Fatal(err)
	}
	if oldOnly.Summary.Requests != 2 || oldOnly.Latency.P50 != 100 || oldOnly.Latency.P95 != 10000 || !oldOnly.Latency.Approximate {
		t.Fatalf("histogram did not merge correctly: %+v", oldOnly)
	}
	partial, err := s.Analytics(ctx, RequestFilter{Since: 2*hour + 100, Until: 3 * hour})
	if err != nil {
		t.Fatal(err)
	}
	if partial.Summary.Requests != 0 || !partial.Coverage.PartialBucketsExcluded {
		t.Fatalf("partial old buckets must not claim exact coverage: %+v", partial)
	}
	exact, err := s.Analytics(ctx, RequestFilter{Since: fresh.TS, Until: fresh.TS})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Summary.Requests != 1 || exact.Summary.AvgLatencyMs != 100 {
		t.Fatalf("new-window inclusive boundary excluded request: %+v", exact)
	}
	excluded, err := s.Analytics(ctx, RequestFilter{Since: fresh.TS + 1})
	if err != nil {
		t.Fatal(err)
	}
	if excluded.Summary.Requests != 0 {
		t.Fatal("window included request outside boundary")
	}
	// New detail in an already aggregated hour must remain queryable alongside it.
	late := analyticsRecord(2*hour+400, "late")
	if err = s.insertRequests([]ReqRecord{late}); err != nil {
		t.Fatal(err)
	}
	all, err = s.Analytics(ctx, RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Summary.Requests != 4 || all.Coverage.AggregateOnlyRequests != 2 {
		t.Fatal("legacy hourly and late detail totals did not merge")
	}
}

func TestImportedCompactedHistorySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	d := &Delta{TS: 7200000, Requests: 4, TokIn: 40, Cost: 2, MS: 3000, Models: map[string]deltaModel{"old": {Requests: 4, Cost: 2}}, Backends: map[string]deltaBackend{"old-provider": {Requests: 4, Cost: 2}}, Payload: `{"distributions_unavailable":true,"aggregate_bucket_ms":3600000}`}
	if err = s.InsertDelta(d); err != nil {
		t.Fatal(err)
	}
	// Model a database predating the request-history migration.
	if _, err = s.db.Exec(`DELETE FROM analytics_meta WHERE name='legacy_imported'`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cost, n, _, _, _, _ := s.LifetimeTotals()
	if n != 4 || cost != 2 {
		t.Fatalf("legacy totals lost: %d %v", n, cost)
	}
	rows, err := s.SeriesSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Models["old"].Requests != 4 || !rows[0].DistributionsUnavailable || rows[0].LatP95 != 0 || rows[0].AggregateBucketMs != 3600000 {
		t.Fatalf("legacy distributions were invented: %+v", rows)
	}
	result, err := s.Analytics(context.Background(), RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Coverage.LegacyAggregatesOnly || result.Coverage.LegacySummary == nil || result.Coverage.LegacySummary.Requests != 4 || result.Summary.Requests != 0 {
		t.Fatalf("legacy coverage not honest: %+v", result)
	}
}

func TestStoreBackupAndBoundedWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	accepted := 0
	for i := range 5000 {
		r := analyticsRecord(7200001, "request")
		r.TS += int64(i)
		if s.QueueRequest(r) {
			accepted++
		}
	}
	if s.Telemetry().DroppedRequests == 0 {
		s.mu.Unlock()
		t.Fatal("unbounded queue")
	}
	s.mu.Unlock()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err = s.Backup(backup); err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(backup); err == nil {
		t.Fatal("backup overwrote existing destination")
	}
	restored, err := OpenStore(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	page, err := restored.Requests(context.Background(), RequestFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != int64(accepted) {
		t.Fatalf("drain/backup accounting: got %d want %d", page.Total, accepted)
	}
}

func TestHistogramNearestRankBoundaries(t *testing.T) {
	var h [len(latBuckets) + 1]uint64
	h[bucketOf(100)] = 95
	h[bucketOf(5000)] = 5
	if histogramPercentile(h, 95) != 100 || histogramPercentile(h, 99) != 5000 {
		t.Fatal("percentile rank off-by-one")
	}
	for _, n := range []int{1, 2, 100} {
		if pct(n, 100) != n-1 || pct(n, 1) < 0 {
			t.Fatal("sample percentile boundary")
		}
	}
	if math.IsNaN(distribution([len(latBuckets) + 1]uint64{}).P50) {
		t.Fatal("empty histogram is NaN")
	}
}

func TestHubCloseAndLiveSanitization(t *testing.T) {
	h := NewHub()
	ch := h.Subscribe()
	r := analyticsRecord(1, "one")
	r.OK = false
	r.ErrMsg = "unsafe-content"
	r.Key = "sk-live-credential-9876"
	r.IP = "198.51.100.9"
	h.Publish(map[string]any{"type": "req", "req": r})
	msg := <-ch
	raw, _ := json.Marshal(msg)
	if strings.Contains(string(raw), "unsafe-content") || strings.Contains(string(raw), "sk-live-credential-9876") {
		t.Fatal("live stream leaked content or raw credential")
	}
	if !strings.Contains(string(raw), `"ip":"198.51.100.9"`) || !strings.Contains(string(raw), `"key":"...9876"`) {
		t.Fatalf("live stream dropped tracing metadata: %s", raw)
	}
	h.Close()
	h.Close()
	h.Publish(map[string]any{"type": "req", "req": r})
	h.Unsubscribe(ch)
	if _, ok := <-ch; ok {
		t.Fatal("subscriber not closed")
	}
	if _, ok := <-h.Subscribe(); ok {
		t.Fatal("subscription after shutdown not closed")
	}
}

func TestTickerStopFlushesBeforeStoreClose(t *testing.T) {
	s := testKeyEventStore(t)
	srv := &Server{metrics: NewMetrics(), store: s}
	srv.rt.Store(&runtimeState{cfg: &Config{Dashboard: &DashboardConfig{}}, backends: map[string]*backend{}})
	ticker := NewTicker(srv)
	srv.metrics.Record2("m", "b", false, 200, 1, 2, 0, 1000000, 0, true)
	ticker.Stop()
	ticker.Stop()
	_, n, _, _, _, _ := s.LifetimeTotals()
	if n != 1 {
		t.Fatalf("shutdown lost final delta: %d", n)
	}
}

func TestAnalyticsHistorySurvivesTicksAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	const oldTS int64 = 7200100
	if !s.QueueRequest(analyticsRecord(oldTS, "old")) {
		t.Fatal("old request was not accepted")
	}
	for _, ts := range []int64{oldTS, oldTS + 100} {
		if err = s.InsertDelta(&Delta{TS: ts, Requests: 1, Payload: `{"avg":20,"p95":30}`}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.InsertAlert(AlertEvent{TS: oldTS, RuleID: "old-rule", Name: "Old alert"}); err != nil {
		t.Fatal(err)
	}
	s.InsertKeyEvent(oldTS, "provider-a", "", "cooldown", 429, "")

	cfg := &Config{}
	cfg.applyDefaults()
	srv := &Server{metrics: NewMetrics(), store: s}
	srv.rt.Store(&runtimeState{cfg: cfg, backends: map[string]*backend{}})
	ticker := NewTicker(srv)
	defer ticker.Stop()
	go ticker.Run()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		ticker.mu.Lock()
		flushed := ticker.last != nil
		ticker.mu.Unlock()
		if flushed {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("ordinary metrics tick did not run")
		case <-time.After(10 * time.Millisecond):
		}
	}
	ticker.Stop()

	assertHistory := func() {
		t.Helper()
		page, err := s.Requests(ctx, RequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 1 || page.Requests[0].ID != "old" {
			t.Fatalf("old request detail lost: %+v", page)
		}
		result, err := s.Analytics(ctx, RequestFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Summary.Requests != 1 || result.Coverage.AggregateOnlyRequests != 0 {
			t.Fatalf("old request was compacted: %+v", result)
		}
		series, err := s.SeriesSince(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(series) < 2 || series[0].TS != oldTS || series[1].TS != oldTS+100 || series[0].LatP95 != 30 || series[1].LatP95 != 30 {
			t.Fatalf("old snapshots were deleted or compacted: %+v", series)
		}
		if alerts := s.RecentAlerts(10); len(alerts) != 1 || alerts[0].TS != oldTS {
			t.Fatalf("old alert lost: %+v", alerts)
		}
		if events := s.RecentKeyEvents(10); len(events) != 1 || events[0].TS != oldTS {
			t.Fatalf("old key event lost: %+v", events)
		}
	}
	assertHistory()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s = reopened
	assertHistory()
}

func TestTickTTFTUsesMillisecondsAndDrainsBoundedHistogram(t *testing.T) {
	mt := NewMetrics()
	mt.Record2("m", "b", true, 200, 0, 0, 0, 250000000, 100000000, true)
	tick := mt.DrainTick()
	if tick.LatN != 1 || tick.LatAvgMs() != 250 || tick.TTFTN != 1 || tick.TTFTAvgMs() != 100 || tick.TTFTHist[bucketOf(100)] != 1 {
		t.Fatalf("incorrect time units: %+v", tick)
	}
	next := mt.DrainTick()
	if next.LatN != 0 || next.TTFTN != 0 {
		t.Fatal("tick was not drained")
	}
}

func TestTelemetryFailuresProduceAlertsWithCustomRules(t *testing.T) {
	s := testKeyEventStore(t)
	s.requestDrop.Add(2)
	engine := NewAlertEngine(s, nil, []AlertRule{{ID: "custom-spend", Metric: "cost_per_hour", Scope: "global", Threshold: 1000, WindowSec: 3600}})
	engine.Eval(time.UnixMilli(7200001), func() int { return 0 })
	found := false
	for _, event := range engine.Recent(10) {
		if event.Metric == "telemetry_errors" && event.Value == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("telemetry failures hidden by custom rules")
	}
}

func TestRequestFilterRejectsMalformedBoundsAndCursor(t *testing.T) {
	for _, values := range []url.Values{
		{"since": {"bad"}}, {"since": {"20"}, "until": {"10"}}, {"cursor": {"not-a-cursor"}},
		{"status": {"oops"}}, {"limit": {"1001"}},
	} {
		if _, err := requestFilter(values); err == nil {
			t.Fatalf("accepted malformed filter: %v", values)
		}
	}
}

func TestBackupFlushesAcceptedMetadata(t *testing.T) {
	s := testKeyEventStore(t)
	if !s.QueueRequest(analyticsRecord(7200001, "queued")) {
		t.Fatal("queue rejected request")
	}
	path := filepath.Join(t.TempDir(), "snapshot.db")
	if err := s.Backup(path); err != nil {
		t.Fatal(err)
	}
	restore, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restore.Close()
	page, err := restore.Requests(context.Background(), RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Requests[0].ID != "queued" {
		t.Fatalf("backup missed accepted metadata: %+v", page)
	}
}

func TestLegacyHourlyUnknownUsageSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hourly.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Persist a historical aggregate directly: new requests no longer create
	// these rows, but old installations must retain their accounting.
	if _, err = s.db.Exec(`INSERT INTO request_hourly (hour,model,upstream,backend,client_key_id,client_name,status,ok,requests,errors,tok_in,tok_out,tok_cache,cost,unknown_usage,unknown_cost,lat_sum,ttft_sum,ttft_n,lat_hist,ttft_hist) VALUES (7200000,'model-a','upstream-a','provider-a','client-a','Agent',200,1,2,0,10,5,3,0.25,1,1,200,100,2,'[]','[]')`); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	result, err := s.Analytics(context.Background(), RequestFilter{ClientKeyID: "client-a"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Requests != 2 || result.Summary.UnknownUsage != 1 || result.Summary.UnknownCost != 1 || result.Summary.Cost != 0.25 || result.Summary.TokIn != 10 || result.Summary.TokOut != 5 || result.Summary.TokCache != 3 {
		t.Fatalf("hourly unknown/known accounting lost across restart: %+v", result)
	}
	page, err := s.Requests(context.Background(), RequestFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Fatal("historical aggregate invented request detail")
	}
}

func TestRecordRequestUsesImmutableExplicitCost(t *testing.T) {
	const model = "test/immutable-request-price"
	catalog.pricing[model] = pricingPer1M{Input: 999999, Output: 999999}
	defer delete(catalog.pricing, model)
	mt := NewMetrics()
	prev := mt.Snapshot(nil)
	r := analyticsRecord(7200001, "priced")
	r.Model = model
	r.Cost = 0.75
	mt.RecordRequest(r)
	r.Backend = "fallback"
	r.OK = false
	r.Outcome = "interrupted"
	r.Cost = 0.5
	mt.RecordRequest(r)
	r.CostKnown = false
	r.Cost = 1000
	mt.RecordRequest(r)
	d := computeDelta(prev, mt.Snapshot(nil))
	if d.Cost != 1.25 || d.Backends["provider-a"].Cost != 0.75 || d.Backends["fallback"].Cost != 0.5 {
		t.Fatalf("explicit request price was replaced by mutable catalog: %+v", d)
	}
	if d.TokIn != 30 || d.TokOut != 15 {
		t.Fatal("unknown pricing must not erase known usage")
	}
}
