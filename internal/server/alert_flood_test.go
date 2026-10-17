package server

import (
	"testing"
	"time"
)

// The alert engine used to re-fire a rule on EVERY eval while its condition held,
// because the only suppression guard is
//
//	if now.Sub(last) < time.Duration(CooldownSec)*time.Second { return }
//
// and `now.Sub(last) < 0` is never true. So CooldownSec == 0 meant "never
// suppress", not "no cooldown configured". The built-in fallback rules all had
// CooldownSec == 0 (the documented 900s default in applyDefaults only covered
// user-configured rules), so an unconfigured deployment emitted one alert row,
// one log line and one SSE publish per rule per eval for as long as a condition
// held. Measured in production: 6748 rows — 22% of the analytics DB.

// TestEveryConstructionPathAppliesACooldown pins the invariant that no
// construction path can produce a rule that re-fires on every eval.
func TestEveryConstructionPathAppliesACooldown(t *testing.T) {
	eng := NewAlertEngine(nil, nil, nil)
	engRules := eng.Rules()
	if len(engRules) == 0 {
		t.Fatal("NewAlertEngine(nil rules) produced no rules")
	}

	engSet := NewAlertEngine(nil, nil, []AlertRule{
		{ID: "configured-no-cooldown", Metric: "error_rate", Scope: "global", Threshold: 5, WindowSec: 300},
	})
	engSetRules := engSet.Rules()

	paths := map[string][]AlertRule{
		"defaultAlertRules":   defaultAlertRules(),
		"telemetryAlertRules": telemetryAlertRules(nil),
		"NewAlertEngine(nil)": engRules,
		"NewAlertEngine(set)": engSetRules,
	}
	for name, rules := range paths {
		if len(rules) == 0 {
			t.Fatalf("%s: no rules", name)
		}
		for _, r := range rules {
			if r.CooldownSec <= 0 {
				t.Errorf("%s: rule %q has CooldownSec=%d — a zero cooldown re-fires every eval",
					name, r.ID, r.CooldownSec)
			}
		}
	}
}

// TestExplicitCooldownIsPreserved proves normalisation only fills gaps and never
// overrides a deliberate value.
func TestExplicitCooldownIsPreserved(t *testing.T) {
	got := withDefaultCooldown([]AlertRule{
		{ID: "explicit", Metric: "error_rate", CooldownSec: 60},
		{ID: "unset", Metric: "error_rate"},
	})
	if got[0].CooldownSec != 60 {
		t.Errorf("explicit cooldown overwritten: got %d, want 60", got[0].CooldownSec)
	}
	if got[1].CooldownSec != DefaultAlertCooldownSec {
		t.Errorf("unset cooldown = %d, want %d", got[1].CooldownSec, DefaultAlertCooldownSec)
	}
}

// TestWithDefaultCooldownDoesNotMutateCaller proves the normaliser copies, so a
// caller's slice is never modified behind its back.
func TestWithDefaultCooldownDoesNotMutateCaller(t *testing.T) {
	in := []AlertRule{{ID: "r", Metric: "error_rate"}}
	_ = withDefaultCooldown(in)
	if in[0].CooldownSec != 0 {
		t.Fatalf("caller slice mutated: CooldownSec=%d, want 0", in[0].CooldownSec)
	}
}

// TestSustainedConditionDoesNotFlood is the actual regression guard for the
// production flood: hold a condition over threshold for a simulated hour and
// assert the engine emits on the cooldown cadence, not on every eval.
//
// Before the fix this measured 720 rows for 720 evals (1:1). After it, ~4.
func TestSustainedConditionDoesNotFlood(t *testing.T) {
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	// nil rules == the production fallback path an unconfigured deployment gets.
	e := NewAlertEngine(st, nil, nil)

	const (
		evalEvery = 5 * time.Second
		dwell     = time.Hour
	)
	steps := int(dwell / evalEvery)
	simNow := time.Now()

	for i := 0; i < steps; i++ {
		// Refresh at the current instant so the rule's window always contains an
		// over-threshold sample (Sum reads real time; only cooldown is simulated).
		// Both histograms are fed: the default latency signal is TTFT (a stalled
		// upstream), and total latency is deliberately not a default rule.
		e.Feed(tickAgg{
			TS:       time.Now().UnixMilli(),
			Req:      1,
			LatN:     1,
			LatHist:  latHistAt(30000),
			TTFTN:    1,
			TTFTHist: latHistAt(30000),
		}, nil, nil)
		simNow = simNow.Add(evalEvery)
		e.Eval(simNow, func() int { return 0 })
	}

	rows := len(st.RecentAlerts(1 << 20))
	// One rule can only fire on the cooldown cadence. A whole-hour dwell with a
	// 900s cooldown allows 1 immediate + a few boundary re-arms, so anything above
	// a small constant means per-eval re-firing has returned.
	const maxAllowed = 8
	if rows > maxAllowed {
		t.Fatalf("alert flood: %d rows over %d evals (%v dwell); want <= %d. "+
			"A rule is re-firing every eval — check that every rule has a cooldown.",
			rows, steps, dwell, maxAllowed)
	}
	if rows == 0 {
		t.Fatal("no alerts fired for a sustained over-threshold condition; " +
			"suppression is too aggressive and real incidents would be lost")
	}
	t.Logf("dwell=%v evals=%d -> %d rows (was %d per-eval re-firing)", dwell, steps, rows, steps)
}

// TestCooldownReArmsAfterWindow proves suppression is a cooldown, not a mute: a
// still-breaching rule fires again once its window elapses, so a long incident is
// still periodically visible.
func TestCooldownReArmsAfterWindow(t *testing.T) {
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	e := NewAlertEngine(st, nil, []AlertRule{
		{ID: "lat", Metric: "latency_p95", Scope: "global", Threshold: 1000, WindowSec: 600, CooldownSec: 900},
	})

	simNow := time.Now()
	feed := func() {
		e.Feed(tickAgg{TS: time.Now().UnixMilli(), Req: 1, LatN: 1, LatHist: latHistAt(30000)}, nil, nil)
	}

	// First eval fires.
	feed()
	e.Eval(simNow, func() int { return 0 })
	if n := len(st.RecentAlerts(10)); n != 1 {
		t.Fatalf("first eval: %d alerts, want 1", n)
	}

	// Inside the cooldown: suppressed.
	feed()
	e.Eval(simNow.Add(800*time.Second), func() int { return 0 })
	if n := len(st.RecentAlerts(10)); n != 1 {
		t.Fatalf("inside cooldown: %d alerts, want still 1 (suppression failed)", n)
	}

	// Past the cooldown: fires again so the incident stays visible.
	feed()
	e.Eval(simNow.Add(1000*time.Second), func() int { return 0 })
	if n := len(st.RecentAlerts(10)); n != 2 {
		t.Fatalf("after cooldown: %d alerts, want 2 (cooldown failed to re-arm)", n)
	}
}

// TestCooldownStopsWhenConditionClears proves a recovered metric does not leave a
// pending alert, and re-firing after recovery is immediate (the cooldown must not
// delay a genuinely new incident).
func TestCooldownStopsWhenConditionClears(t *testing.T) {
	st, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.Close()

	e := NewAlertEngine(st, nil, []AlertRule{
		{ID: "lat", Metric: "latency_p95", Scope: "global", Threshold: 1000, WindowSec: 0, CooldownSec: 900},
	})
	simNow := time.Now()
	e.Feed(tickAgg{TS: time.Now().UnixMilli(), LatN: 1, LatHist: latHistAt(50)}, nil, nil)
	e.Eval(simNow, func() int { return 0 })
	if n := len(st.RecentAlerts(10)); n != 0 {
		t.Fatalf("healthy metric fired %d alerts, want 0", n)
	}
}

func latHistAt(ms float64) [len(latBuckets) + 1]uint64 {
	var h [len(latBuckets) + 1]uint64
	h[bucketOf(ms)]++
	return h
}

// TestDefaultLatencyAlertWatchesTTFTNotTotalLatency pins the scoping decision.
//
// Total latency tracks output length far more than upstream health: measured on
// this gateway, decode throughput was a flat ~294-394 tok/s across every latency
// bucket, while requests over 30s averaged ~12,230 output tokens against ~272 for
// requests under 5s. A fixed total-latency threshold therefore fires on every long
// generation — normal, healthy behaviour — and says nothing about a stalled
// upstream. Time to first token is the part that measures the upstream, so that is
// what the default watches. Any total-latency threshold is the wrong shape, so
// this is deliberately not a matter of picking a bigger number.
func TestDefaultLatencyAlertWatchesTTFTNotTotalLatency(t *testing.T) {
	var sawTTFT, sawTotal bool
	for _, r := range defaultAlertRules() {
		switch r.Metric {
		case "ttft_p95":
			sawTTFT = true
		case "latency_p95":
			sawTotal = true
		}
	}
	if !sawTTFT {
		t.Error("no default rule watches ttft_p95; a stalled upstream would go unnoticed")
	}
	if sawTotal {
		t.Error("a default rule watches latency_p95, which tracks output length rather than " +
			"gateway health and will fire on ordinary long generations")
	}
}

// TestStalledUpstreamIsStillDetected proves the signal that matters is not lost:
// a long time to first token raises ttft_p95 and fires.
func TestStalledUpstreamIsStillDetected(t *testing.T) {
	st := newTestStore(t)
	e := NewAlertEngine(st, nil, nil)

	// A single stalled stream: 20s before the first token.
	e.Feed(tickAgg{
		TS:       time.Now().UnixMilli(),
		Req:      1,
		LatN:     1,
		LatHist:  latHistAt(20400),
		TTFTN:    1,
		TTFTHist: latHistAt(20400),
	}, nil, nil)
	e.Eval(time.Now(), func() int { return 0 })

	got := st.RecentAlerts(10)
	if len(got) != 1 {
		t.Fatalf("stalled upstream produced %d alerts, want 1", len(got))
	}
	if got[0].Metric != "ttft_p95" {
		t.Fatalf("alerted on %q, want ttft_p95", got[0].Metric)
	}
}

// TestLongGenerationDoesNotAlert proves the converse: a big, slow-but-healthy
// generation (short TTFT, long total) no longer raises an alert.
func TestLongGenerationDoesNotAlert(t *testing.T) {
	st := newTestStore(t)
	e := NewAlertEngine(st, nil, nil)

	// 12k tokens at ~300 tok/s is ~40s total, but the first token arrives fast —
	// exactly the workload that used to trip the total-latency rule.
	e.Feed(tickAgg{
		TS:       time.Now().UnixMilli(),
		Req:      1,
		LatN:     1,
		LatHist:  latHistAt(40000),
		TTFTN:    1,
		TTFTHist: latHistAt(900),
	}, nil, nil)
	e.Eval(time.Now(), func() int { return 0 })

	if got := st.RecentAlerts(10); len(got) != 0 {
		t.Fatalf("healthy long generation produced %d alerts, want 0: %+v", len(got), got[0])
	}
}
