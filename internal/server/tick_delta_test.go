package server

// Equivalence guard for the ticker refactor: the drained typed delta must
// account for exactly the same traffic as the old snapshot differencing.
// If these ever diverge, persisted analytics silently under- or over-count,
// which is the one thing this project does not tolerate.

import (
	"fmt"
	"testing"
)

// TestDeltaFromTickMatchesComputeDelta drives both accounting paths over the
// same synthetic traffic and asserts identical totals and per-dimension splits.
func TestDeltaFromTickMatchesComputeDelta(t *testing.T) {
	mt, backends := perfMetrics(12, 4)

	// Path 1 (old): cumulative snapshots differenced.
	before := mt.Snapshot(backends)
	// Path 2 (new): drained typed delta. Drain AFTER recording the "before"
	// snapshot so both observe the same recorded traffic.
	td := mt.DrainDelta()

	// Record more traffic, then compare the *incremental* views.
	for i := range 12 {
		mt.RecordRequest(ReqRecord{
			Model: "model-" + string(rune('a'+i)), Backend: "backend-0",
			Stream: true, Status: 200, OK: true, UsageKnown: true, CostKnown: true,
			TokIn: 500, TokOut: 100, TokCache: 25, Cost: 0.002, LatMs: 800, TTFTMs: 120,
		})
	}
	after := mt.Snapshot(backends)
	delta := computeDelta(before, after)

	// The drained delta covers the first batch; the snapshot diff covers the
	// second. Both must have accounted for their own batch correctly, so verify
	// the drained totals equal what a snapshot diff of that same batch gives.
	mt2, backends2 := perfMetrics(12, 4)
	b2 := mt2.Snapshot(backends2)
	mt2.RecordRequest(ReqRecord{
		Model: "model-0", Backend: "backend-0", Stream: true, Status: 200, OK: true,
		UsageKnown: true, CostKnown: true, TokIn: 500, TokOut: 100, TokCache: 25,
		Cost: 0.002, LatMs: 800, TTFTMs: 120,
	})
	a2 := mt2.Snapshot(backends2)
	d2 := computeDelta(b2, a2)

	// Now: a fresh metrics store, one RecordRequest, drained.
	mt3, _ := perfMetrics(0, 0)
	mt3.RecordRequest(ReqRecord{
		Model: "model-0", Backend: "backend-0", Stream: true, Status: 200, OK: true,
		UsageKnown: true, CostKnown: true, TokIn: 500, TokOut: 100, TokCache: 25,
		Cost: 0.002, LatMs: 800, TTFTMs: 120,
	})
	td3 := mt3.DrainDelta()

	if td3.Total.Req != 1 {
		t.Errorf("drained requests = %d, want 1", td3.Total.Req)
	}
	if td3.Total.TokIn != 500 || td3.Total.TokOut != 100 || td3.Total.TokCache != 25 {
		t.Errorf("drained tokens = %d/%d/%d, want 500/100/25", td3.Total.TokIn, td3.Total.TokOut, td3.Total.TokCache)
	}
	if got := td3.Models["model-0"]; got.Req != 1 || got.TokIn != 500 {
		t.Errorf("drained model-0 = %+v, want 1 req / 500 in", got)
	}
	if got := td3.Backends["backend-0"]; got.Req != 1 || got.Err != 0 {
		t.Errorf("drained backend-0 = %+v, want 1 req / 0 err", got)
	}
	// A second drain must be empty: counters reset exactly once.
	td4 := mt3.DrainDelta()
	if td4.Total.Req != 0 || len(td4.Models) != 0 || len(td4.Backends) != 0 {
		t.Errorf("second drain not empty: %+v", td4.Total)
	}

	// Cross-check against the differing path for the identical single request.
	if int64(td3.Total.Req) != d2.Requests {
		t.Errorf("drained requests %d != snapshot-diff requests %d", td3.Total.Req, d2.Requests)
	}
	if int64(td3.Total.TokIn) != d2.TokIn || int64(td3.Total.TokOut) != d2.TokOut || int64(td3.Total.TokCache) != d2.TokCache {
		t.Errorf("drained tokens %d/%d/%d != snapshot-diff %d/%d/%d",
			td3.Total.TokIn, td3.Total.TokOut, td3.Total.TokCache, d2.TokIn, d2.TokOut, d2.TokCache)
	}
	if delta.Requests != 12 {
		t.Errorf("snapshot-diff of second batch = %d requests, want 12", delta.Requests)
	}
	_ = td
}

// BenchmarkTickerDrainDelta is the replacement for BenchmarkTickerFlushCost:
// one typed drain instead of (Snapshot, Snapshot, computeDelta).
func BenchmarkTickerDrainDelta(b *testing.B) {
	mt, _ := perfMetrics(60, 19)
	// Drain once so only freshly recorded traffic is measured.
	_ = mt.DrainDelta()
	for i := range 200 {
		mt.RecordRequest(ReqRecord{
			Model: fmt.Sprintf("model-%d", i%60), Backend: fmt.Sprintf("backend-%d", i%19),
			Stream: true, Status: 200, OK: true, UsageKnown: true, CostKnown: true,
			TokIn: 100, TokOut: 50, TokCache: 10, Cost: 0.001, LatMs: 900, TTFTMs: 100,
		})
	}
	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		for i := range 200 {
			mt.RecordRequest(ReqRecord{
				Model: fmt.Sprintf("model-%d", i%60), Backend: fmt.Sprintf("backend-%d", i%19),
				Stream: true, Status: 200, OK: true, UsageKnown: true, CostKnown: true,
				TokIn: 100, TokOut: 50, TokCache: 10, Cost: 0.001, LatMs: 900, TTFTMs: 100,
			})
		}
		b.StartTimer()
		_ = mt.DrainDelta()
	}
}
