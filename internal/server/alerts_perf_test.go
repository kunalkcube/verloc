package server

import (
	"math"
	"testing"
	"time"
)

// refMergePercentiles reproduces the pre-refactor per-merge behavior: every
// tick folded into the accumulator recomputes P50/P95/P99 from the histogram.
// It is the oracle the new add+finalize path must match exactly.
func refMergePercentiles(ticks []tickAgg) tickAgg {
	var out tickAgg
	for _, o := range ticks {
		out.Req += o.Req
		out.Err += o.Err
		out.Failover += o.Failover
		out.TokIn += o.TokIn
		out.TokOut += o.TokOut
		out.TokCache += o.TokCache
		out.Cost += o.Cost
		out.LatSumMs += o.LatSumMs
		out.LatN += o.LatN
		out.TTFTSumMs += o.TTFTSumMs
		out.TTFTN += o.TTFTN
		for i, v := range o.LatHist {
			out.LatHist[i] += v
		}
		for i, v := range o.TTFTHist {
			out.TTFTHist[i] += v
		}
		out.P50Ms = histogramPercentile(out.LatHist, 50)
		out.P95Ms = histogramPercentile(out.LatHist, 95)
		out.P99Ms = histogramPercentile(out.LatHist, 99)
		if o.TS > out.TS {
			out.TS = o.TS
		}
	}
	return out
}

// newAddFinalize is the new path: pure accumulation, then a single finalize.
func newAddFinalize(ticks []tickAgg) tickAgg {
	var out tickAgg
	for _, o := range ticks {
		out.add(o)
	}
	out.finalize()
	return out
}

// TestPercentileEquivalenceOldVsNew builds several known distributions and
// asserts the new add+finalize path yields identical percentiles, counters and
// TS to the old per-merge recompute.
func TestPercentileEquivalenceOldVsNew(t *testing.T) {
	mkHist := func(vals ...int) [len(latBuckets) + 1]uint64 {
		var h [len(latBuckets) + 1]uint64
		for _, v := range vals {
			h[v]++
		}
		return h
	}

	cases := []struct {
		name  string
		ticks []tickAgg
	}{
		{
			name: "single bucket",
			ticks: []tickAgg{
				{TS: 1000, Req: 10, Err: 1, LatN: 10, LatSumMs: 1000, LatHist: mkHist(3, 3, 3, 3, 3, 3, 3, 3, 3, 3)},
			},
		},
		{
			name: "two disjoint spikes",
			ticks: []tickAgg{
				{TS: 1000, Req: 10, LatN: 10, LatSumMs: 1000, LatHist: mkHist(1, 1, 1, 1, 1, 1, 1, 1, 1, 1)},
				{TS: 4000, Req: 90, LatN: 90, LatSumMs: 900000, LatHist: mkHist(7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7, 7)},
			},
		},
		{
			name: "spread across all buckets incl overflow",
			ticks: []tickAgg{
				{TS: 1000, Req: 11, LatN: 11, LatSumMs: 1, LatHist: mkHist(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)},
				{TS: 3000, Req: 5, LatN: 5, LatSumMs: 2, LatHist: mkHist(0, 2, 4, 6, 8, 10)},
				{TS: 2000, Req: 1, LatN: 1, LatSumMs: 3, LatHist: mkHist(10)}, // overflow bucket
			},
		},
		{
			name:  "empty",
			ticks: []tickAgg{{}, {}},
		},
		{
			name: "many tiny ticks",
			ticks: func() []tickAgg {
				var out []tickAgg
				for i := 0; i < 28800; i++ {
					out = append(out, tickAgg{
						TS: int64(1_700_000_000_000 + i*3000), Req: 10, Err: 1, Failover: 1,
						TokIn: 100, TokOut: 200, TokCache: 5, Cost: 0.001,
						LatN: 10, LatSumMs: 100,
						LatHist: mkHist(0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10),
					})
				}
				return out
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := refMergePercentiles(tc.ticks)
			neu := newAddFinalize(tc.ticks)
			if old.P50Ms != neu.P50Ms || old.P95Ms != neu.P95Ms || old.P99Ms != neu.P99Ms {
				t.Fatalf("percentile mismatch: old p50/95/99=%v/%v/%v new=%v/%v/%v",
					old.P50Ms, old.P95Ms, old.P99Ms, neu.P50Ms, neu.P95Ms, neu.P99Ms)
			}
			if old.Req != neu.Req || old.Err != neu.Err || old.Failover != neu.Failover ||
				old.TokIn != neu.TokIn || old.TokOut != neu.TokOut || old.TokCache != neu.TokCache ||
				old.LatN != neu.LatN || old.TTFTN != neu.TTFTN || old.TS != neu.TS {
				t.Fatalf("counter mismatch: old=%+v new=%+v", old, neu)
			}
			if math.Abs(old.Cost-neu.Cost) > 1e-12 || math.Abs(old.LatSumMs-neu.LatSumMs) > 1e-9 {
				t.Fatalf("float mismatch: old=%+v new=%+v", old, neu)
			}
			if old.LatHist != neu.LatHist || old.TTFTHist != neu.TTFTHist {
				t.Fatalf("histogram mismatch: old=%v new=%v", old.LatHist, neu.LatHist)
			}
		})
	}
}

// TestAggregatorSumPercentilesMatchReference proves Aggregator.Sum now returns
// the same percentiles the old per-merge path would have.
func TestAggregatorSumPercentilesMatchReference(t *testing.T) {
	a := NewAggregator(28800, 60*24)
	now := time.Now().UnixMilli()
	var ticks []tickAgg
	for i := 0; i < 400; i++ {
		o := tickAgg{
			TS: now - int64(400-i)*3000, Req: 10, Err: 1, Failover: 1,
			TokIn: 100, TokOut: 200, TokCache: 5, Cost: 0.001,
			LatN: 10, LatSumMs: 100,
			LatHist: [len(latBuckets) + 1]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		}
		a.Feed(o, nil, nil)
		ticks = append(ticks, o)
	}
	got := a.Sum(3600)
	want := refMergePercentiles(ticks)
	if got.P50Ms != want.P50Ms || got.P95Ms != want.P95Ms || got.P99Ms != want.P99Ms {
		t.Fatalf("Sum percentiles changed: got %v/%v/%v want %v/%v/%v",
			got.P50Ms, got.P95Ms, got.P99Ms, want.P50Ms, want.P95Ms, want.P99Ms)
	}
}

// TestAggregatorSumScopeFinalizesPercentiles proves a SumScope result that
// crosses minute buckets reports the combined distribution, not a single
// bucket's partial (or stale/zero) value.
func TestAggregatorSumScopeFinalizesPercentiles(t *testing.T) {
	a := NewAggregator(28800, 60*24)
	now := time.Now().UnixMilli()
	// Two ticks one minute apart so they land in distinct minute buckets.
	t1 := tickAgg{TS: now, Req: 1, LatN: 1, LatSumMs: 10, LatHist: func() [len(latBuckets) + 1]uint64 {
		var h [len(latBuckets) + 1]uint64
		h[bucketOf(100)] = 10
		return h
	}()}
	t2 := tickAgg{TS: now + 60000, Req: 1, LatN: 1, LatSumMs: 10, LatHist: func() [len(latBuckets) + 1]uint64 {
		var h [len(latBuckets) + 1]uint64
		h[bucketOf(10000)] = 90
		return h
	}()}
	a.Feed(t1, map[string]tickAgg{"m": t1}, nil)
	a.Feed(t2, map[string]tickAgg{"m": t2}, nil)
	got, ok := a.SumScope("model:m", 3600)
	if !ok {
		t.Fatal("scope not found")
	}
	// 10 samples at 100ms and 90 at 10000ms: p50/p95/p99 all fall in the
	// dominant 10000ms bucket, exactly as the old per-merge path produced.
	if got.P50Ms != 10000 || got.P95Ms != 10000 || got.P99Ms != 10000 {
		t.Fatalf("scope percentiles not finalized over all buckets: %+v", got)
	}
}

// BenchmarkAlertEvalAllRulesHot mirrors BenchmarkAlertEvalAllRules but seeds
// the ring with *current* timestamps and a window spanning it, so Sum folds all
// 24h of ticks and actually exercises the percentile path (the fixed 1.7e12
// seed in the shared benchmark is older than any window and is filtered out
// entirely, so it never merges a tick). Thresholds are high so the benchmark
// measures evaluation cost without emitting alerts.
func BenchmarkAlertEvalAllRulesHot(b *testing.B) {
	a := NewAggregator(28800, 60*24)
	now := time.Now().UnixMilli()
	for i := range 28800 {
		a.Feed(tickAgg{TS: now - int64(28800-i)*3000, Req: 10, Err: 1, Failover: 1,
			TokIn: 100, TokOut: 200, TokCache: 5, Cost: 0.001, LatN: 10, LatSumMs: 100,
			TTFTSumMs: 40, TTFTN: 10,
			LatHist:  [len(latBuckets) + 1]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			TTFTHist: [len(latBuckets) + 1]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		}, nil, nil)
	}
	rules := []AlertRule{
		{ID: "h-err", Metric: "error_rate", Scope: "global", Threshold: 1e9, WindowSec: 90000},
		{ID: "h-fo", Metric: "failover_rate", Scope: "global", Threshold: 1e9, WindowSec: 90000},
		{ID: "h-cost", Metric: "cost_per_hour", Scope: "global", Threshold: 1e9, WindowSec: 90000},
		{ID: "h-lat", Metric: "latency_p95", Scope: "global", Threshold: 1e9, WindowSec: 90000},
	}
	e := &AlertEngine{agg: a, cooldown: map[string]time.Time{}, rules: rules, recentN: 500}
	b.ResetTimer()
	for b.Loop() {
		e.Eval(time.Now(), func() int { return 0 })
	}
}

// BenchmarkAggregatorSumHot benchmarks one Sum over a fully-populated, current
// 24h ring — the hot path the alert engine drives.
func BenchmarkAggregatorSumHot(b *testing.B) {
	a := NewAggregator(28800, 60*24)
	now := time.Now().UnixMilli()
	for i := range 28800 {
		a.Feed(tickAgg{TS: now - int64(28800-i)*3000, Req: 10, Err: 1, LatN: 10, LatSumMs: 100,
			LatHist: [len(latBuckets) + 1]uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}, nil, nil)
	}
	b.ResetTimer()
	for b.Loop() {
		_ = a.Sum(3600)
	}
}

// TestEvalNoDivideByZeroWithZeroWindow proves rps/tokens_per_hour rules with a
// zero window cannot produce Inf/NaN and fire a bogus alert.
func TestEvalNoDivideByZeroWithZeroWindow(t *testing.T) {
	a := NewAggregator(28800, 60*24)
	a.Feed(tickAgg{TS: time.Now().UnixMilli(), Req: 10, TokIn: 100, TokOut: 100}, nil, nil)
	e := &AlertEngine{agg: a, cooldown: map[string]time.Time{}, recentN: 500,
		rules: []AlertRule{
			{ID: "rps-zero", Metric: "rps", Scope: "global", Threshold: 1, WindowSec: 0},
			{ID: "tph-zero", Metric: "tokens_per_hour", Scope: "global", Threshold: 1, WindowSec: 0},
		}}
	e.Eval(time.Now(), func() int { return 0 })
	for _, ev := range e.Recent(10) {
		if math.IsInf(ev.Value, 0) || math.IsNaN(ev.Value) {
			t.Fatalf("rule %s produced non-finite value %v", ev.RuleID, ev.Value)
		}
	}
}

// TestSetRulesPrunesCooldown verifies stale rule cooldowns are removed while
// surviving rules keep theirs.
func TestSetRulesPrunesCooldown(t *testing.T) {
	e := &AlertEngine{cooldown: map[string]time.Time{}, recentN: 500}
	now := time.Now()
	e.cooldown["keep"] = now
	e.cooldown["drop"] = now
	e.SetRules([]AlertRule{{ID: "keep", Metric: "error_rate", Scope: "global", Threshold: 1}})
	if _, ok := e.cooldown["drop"]; ok {
		t.Fatal("stale cooldown not pruned")
	}
	if _, ok := e.cooldown["keep"]; !ok {
		t.Fatal("surviving cooldown was dropped")
	}
}

// TestRecentRingOrderAndBound proves Recent stays newest-first and the ring
// never grows past recentN.
func TestRecentRingOrderAndBound(t *testing.T) {
	e := &AlertEngine{cooldown: map[string]time.Time{}, recentN: 3}
	for i := 1; i <= 5; i++ {
		e.pushRecent(AlertEvent{RuleID: "r", Value: float64(i)})
	}
	if len(e.recent) != 3 {
		t.Fatalf("ring grew to %d, want 3", len(e.recent))
	}
	got := e.Recent(0)
	if len(got) != 3 {
		t.Fatalf("Recent len = %d, want 3", len(got))
	}
	for i, want := range []float64{5, 4, 3} {
		if got[i].Value != want {
			t.Fatalf("Recent[%d] = %v, want %v (newest first)", i, got[i].Value, want)
		}
	}
	// Fewer than n requested.
	if short := e.Recent(2); len(short) != 2 || short[0].Value != 5 || short[1].Value != 4 {
		t.Fatalf("Recent(2) = %+v", short)
	}
}
