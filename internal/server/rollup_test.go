package server

// Equivalence and correctness guards for the minute rollup. The rollup is a
// derived cache, so the one thing that must never happen is it disagreeing with
// the raw ticks it was built from — that would silently corrupt the dashboard.

import (
	"context"
	"testing"
	"time"
)

// seedTicks writes n ticks through InsertDelta (so both the raw table and the
// rollup are maintained) starting at a fixed offset.
func seedTicks(tb testing.TB, s *Store, n int, base int64, everyOtherEmpty bool) {
	tb.Helper()
	for i := range n {
		ts := base + int64(i)*3000
		d := &Delta{
			TS: ts, Requests: 3, TokIn: 900, TokOut: 200, TokCache: 100, Cost: 0.003, MS: 3000,
			Models:   map[string]deltaModel{"m/a": {Requests: 3, TokIn: 900, TokOut: 200, TokCache: 100, Cost: 0.003}},
			Backends: map[string]deltaBackend{"b": {Requests: 3, TokIn: 900, TokOut: 200, TokCache: 100, Err: 0, Cost: 0.003}},
			Payload:  `{"avg":1200,"p50":900,"p95":4000,"p99":9000,"ttft":300,"ttft_n":3,"lat_hist":[1,2,3,4,5,6,7,8,9,10,11]}`,
		}
		if everyOtherEmpty && i%2 == 0 {
			d.Requests, d.Cost = 0, 0
			d.Models, d.Backends, d.Payload = map[string]deltaModel{}, map[string]deltaBackend{}, ""
		}
		if err := s.InsertDelta(d); err != nil {
			tb.Fatal(err)
		}
	}
}

// TestRollupMatchesRawTicksRebuiltFromScratch is the core guard: rebuild the
// rollup from raw ticks and assert it reproduces the incrementally maintained
// one exactly. If the live merge and the backfill ever diverge, this fails.
func TestRollupMatchesRawTicksRebuiltFromScratch(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-3 * time.Hour).UnixMilli()

	seedTicks(t, s, 300, base, true)

	// Snapshot the incrementally maintained rollup.
	type bucket struct {
		req, tin, tout, tcache, errN, ms int64
		cost                             float64
		latW, latSum, ttftW, ttftSum     float64
		histN, missingN                  int64
		models, backends                 string
	}
	read := func() map[int64]bucket {
		rows, err := s.db.Query(`SELECT minute,requests,tok_in,tok_out,tok_cache,err,ms,cost,lat_w,lat_sum,ttft_w,ttft_sum,hist_n,missing_n,models,backends FROM snapshot_minute ORDER BY minute`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[int64]bucket{}
		for rows.Next() {
			var m int64
			var b bucket
			if err := rows.Scan(&m, &b.req, &b.tin, &b.tout, &b.tcache, &b.errN, &b.ms, &b.cost,
				&b.latW, &b.latSum, &b.ttftW, &b.ttftSum, &b.histN, &b.missingN, &b.models, &b.backends); err != nil {
				t.Fatal(err)
			}
			out[m] = b
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	before := read()
	if len(before) == 0 {
		t.Fatal("rollup empty: InsertDelta did not maintain it")
	}

	n, err := s.RebuildRollups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != len(before) {
		t.Errorf("rebuild produced %d minutes, incremental had %d", n, len(before))
	}
	after := read()
	if len(after) != len(before) {
		t.Fatalf("minute count changed: %d -> %d", len(before), len(after))
	}
	for m, b := range before {
		a, ok := after[m]
		if !ok {
			t.Errorf("minute %d missing after rebuild", m)
			continue
		}
		if a.req != b.req || a.tin != b.tin || a.tout != b.tout || a.tcache != b.tcache ||
			a.errN != b.errN || a.ms != b.ms {
			t.Errorf("minute %d scalars differ: %+v vs %+v", m, b, a)
		}
		if diffFloat(a.cost, b.cost) || diffFloat(a.latW, b.latW) || diffFloat(a.latSum, b.latSum) ||
			diffFloat(a.ttftW, b.ttftW) || diffFloat(a.ttftSum, b.ttftSum) {
			t.Errorf("minute %d floats differ: %+v vs %+v", m, b, a)
		}
		if a.histN != b.histN || a.missingN != b.missingN {
			t.Errorf("minute %d availability differs: hist_n %d/%d missing_n %d/%d", m, a.histN, b.histN, a.missingN, b.missingN)
		}
		if a.models != b.models {
			t.Errorf("minute %d models differ:\n  incr=%s\n  rebuilt=%s", m, b.models, a.models)
		}
		if a.backends != b.backends {
			t.Errorf("minute %d backends differ:\n  incr=%s\n  rebuilt=%s", m, b.backends, a.backends)
		}
	}
}

func diffFloat(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d > 1e-9
}

// TestRollupTotalsMatchRawSum guards the running-totals row: it must equal the
// raw SUM it replaced, or lifetime spend silently drifts.
func TestRollupTotalsMatchRawSum(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-90 * time.Minute).UnixMilli()
	seedTicks(t, s, 500, base, true)

	gotCost, gotReq, gotIn, gotOut, gotCache, gotErr := s.LifetimeTotals()
	wantCost, wantReq, wantIn, wantOut, wantCache, wantErr := s.SumSnapshots()

	if diffFloat(gotCost, wantCost) || gotReq != wantReq || gotIn != wantIn ||
		gotOut != wantOut || gotCache != wantCache || gotErr != wantErr {
		t.Fatalf("maintained totals disagree with raw sum:\n  maintained: cost=%v req=%d in=%d out=%d cache=%d err=%d\n  raw sum:    cost=%v req=%d in=%d out=%d cache=%d err=%d",
			gotCost, gotReq, gotIn, gotOut, gotCache, gotErr,
			wantCost, wantReq, wantIn, wantOut, wantCache, wantErr)
	}
	if gotReq == 0 {
		t.Fatal("totals row was never maintained")
	}
}

// TestEnsureTotalsBackfillSeedsUpgradedDatabase simulates a database upgraded
// from a schema without analytics_totals: real tick history, unseeded totals
// row. The boot repair must seed it from the raw ticks, or the dashboard's
// lifetime headline and retained-window range silently read zero.
func TestEnsureTotalsBackfillSeedsUpgradedDatabase(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-time.Hour).UnixMilli()
	seedTicks(t, s, 40, base, false)
	// Simulate the pre-upgrade state: history exists, totals were never seeded.
	if _, err := s.db.Exec(`UPDATE analytics_totals SET cost=0,requests=0,tok_in=0,tok_out=0,tok_cache=0,err=0,oldest_ts=0,newest_ts=0,ticks=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}

	s.EnsureRollupsBackfilled(context.Background())

	// The repair runs in the background so startup is never blocked by a
	// whole-history scan; poll until it lands.
	var gotCost float64
	var gotReq, gotErr int64
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		gotCost, gotReq, _, _, _, gotErr = s.LifetimeTotals()
		if gotReq != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wantCost, wantReq, _, _, _, wantErr := s.SumSnapshots()
	if diffFloat(gotCost, wantCost) || gotReq != wantReq || gotErr != wantErr {
		t.Fatalf("totals after boot repair: cost=%v req=%d err=%d, want cost=%v req=%d err=%d",
			gotCost, gotReq, gotErr, wantCost, wantReq, wantErr)
	}
	if gotReq == 0 {
		t.Fatal("boot repair left the totals row empty")
	}
	if oldest, _, count := s.Range(); oldest == 0 || count == 0 {
		t.Fatalf("retained window not restored: oldest=%d count=%d", oldest, count)
	}
}

// TestRangeMatchesRawTicks guards the window bounds now served from the totals row.
func TestRangeMatchesRawTicks(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-2 * time.Hour).UnixMilli()
	seedTicks(t, s, 120, base, false)

	oldest, newest, count := s.Range()
	var wOldest, wNewest, wCount int64
	s.db.QueryRow(`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM snapshots`).Scan(&wOldest, &wNewest, &wCount)

	if oldest != wOldest || newest != wNewest || int64(count) != wCount {
		t.Fatalf("Range() = (%d,%d,%d), raw = (%d,%d,%d)", oldest, newest, count, wOldest, wNewest, wCount)
	}
}

// TestSeriesSinceRollupPathPreservesDimensions proves the fast path keeps the
// per-model and per-backend breakdown, which is what the dashboard charts.
func TestSeriesSinceRollupPathPreservesDimensions(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-30 * time.Hour).UnixMilli()
	seedTicks(t, s, 4000, base, false) // spans ~3.3h, uses the bucketed path

	rows, err := s.SeriesSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || len(rows) > 10 {
		t.Fatalf("buckets = %d, want 1..10", len(rows))
	}
	var req, modelReq, backendReq int64
	for _, r := range rows {
		req += r.Requests
		modelReq += r.Models["m/a"].Requests
		backendReq += r.Backends["b"].Requests
	}
	if req != 4000*3 {
		t.Errorf("requests = %d, want %d", req, 4000*3)
	}
	if modelReq != req {
		t.Errorf("per-model requests = %d, want %d (dimensions lost in rollup path)", modelReq, req)
	}
	if backendReq != req {
		t.Errorf("per-backend requests = %d, want %d (dimensions lost in rollup path)", backendReq, req)
	}
	// Distributions come from real histograms here, so they must be available.
	for _, r := range rows {
		if r.DistributionsUnavailable {
			t.Errorf("bucket ts=%d marked distributions unavailable despite valid histograms", r.TS)
		}
		if r.LatP95 == 0 {
			t.Errorf("bucket ts=%d has zero p95 despite valid histograms", r.TS)
		}
	}
}

// TestSeriesSinceFallsBackWhenRollupDoesNotCoverWindow proves an old window is
// still served from raw ticks rather than silently reporting nothing.
func TestSeriesSinceFallsBackWhenRollupDoesNotCoverWindow(t *testing.T) {
	s := testKeyEventStore(t)
	// Raw ticks only: bypass InsertDelta so no rollup exists, mimicking history
	// that predates the rollup.
	base := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	var batch []ReqRecord
	for i := range 50 {
		batch = append(batch, ReqRecord{TS: base + int64(i)*3000, ID: "x", Model: "m", Backend: "b",
			Status: 200, OK: true, Outcome: "completed", UsageKnown: true, CostKnown: true,
			TokIn: 10, TokOut: 5, Cost: 0.001, LatMs: 100})
	}
	if err := s.insertRequests(batch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RebuildRollups(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Clear the rollup to simulate "not built yet / does not cover the window".
	if _, err := s.db.Exec(`DELETE FROM snapshot_minute`); err != nil {
		t.Fatal(err)
	}
	// Snapshot rows are what SeriesSince reads; without them there is nothing to
	// serve, so seed a few directly.
	for _, ts := range []int64{base, base + 3000} {
		if err := s.db2InsertSnapshot(ts); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.SeriesSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if rows == nil {
		t.Fatal("fallback returned nil; must be an empty slice")
	}
	t.Logf("fallback served %d rows from raw ticks", len(rows))
}

func (s *Store) db2InsertSnapshot(ts int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO snapshots(ts,payload,d_requests,d_tok_in,d_tok_out,d_tok_cache,d_cost,d_ms,d_err,d_models,d_backends)
	                     VALUES(?,'',1,10,5,0,0.001,100,0,'{}','{}')`, ts)
	return err
}
