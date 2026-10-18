package server

import (
	"context"
	"testing"
)

// zeroSeedTick builds one snapshot delta carrying errN errors in every
// dimension, so the rewriter has both scalars and JSON blobs to correct.
func zeroSeedTick(ts, errN int64) *Delta {
	return &Delta{
		TS: ts, Requests: 2, TokIn: 10, TokOut: 5, Cost: 0.01, Err: errN,
		Models:   map[string]deltaModel{"m/a": {Requests: 2, TokIn: 10, TokOut: 5, Cost: 0.01, Err: errN}},
		Backends: map[string]deltaBackend{"p/a": {Requests: 2, TokIn: 10, TokOut: 5, Cost: 0.01, Err: errN}},
	}
}

func snapshotErrSum(t *testing.T, s *Store) int64 {
	t.Helper()
	var sum int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(d_err),0) FROM snapshots`).Scan(&sum); err != nil {
		t.Fatalf("sum d_err: %v", err)
	}
	return sum
}

// TestZeroErrorCountersRebuildsDerivedState pins the maintenance contract: the
// dry run reports without writing, apply removes exactly the error counters
// from ticks and dimension blobs, and the rebuilt minute rollup and lifetime
// totals agree — while traffic, token and cost history survive untouched.
// File-backed store: RebuildRollups streams ticks while holding its rebuild
// transaction, which deadlocks on the single-connection ":memory:" pool.
func TestZeroErrorCountersRebuildsDerivedState(t *testing.T) {
	s := testKeyEventStore(t)
	if err := s.InsertDelta(zeroSeedTick(1000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDelta(zeroSeedTick(2000, 3)); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertDelta(zeroSeedTick(3000, 1)); err != nil {
		t.Fatal(err)
	}

	stats, err := s.ZeroErrorCounters(true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if stats.Ticks != 3 || stats.TicksWithErr != 2 || stats.ErrSum != 4 {
		t.Fatalf("dry-run stats wrong: %+v", stats)
	}
	if got := snapshotErrSum(t, s); got != 4 {
		t.Fatalf("dry run wrote to disk: d_err sum = %d, want 4", got)
	}

	stats, err = s.ZeroErrorCounters(false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if stats.TicksWithErr != 2 || stats.ErrSum != 4 {
		t.Fatalf("apply stats wrong: %+v", stats)
	}
	if got := snapshotErrSum(t, s); got != 0 {
		t.Fatalf("d_err sum after apply = %d, want 0", got)
	}
	var modelErr int64
	if err := s.db.QueryRow(`SELECT COALESCE(json_extract(d_models,'$.\"m/a\".err'),0) FROM snapshots WHERE ts=2000`).Scan(&modelErr); err != nil {
		t.Fatal(err)
	}
	if modelErr != 0 {
		t.Fatalf("per-model error survived the rewrite: %d", modelErr)
	}

	if _, err := s.RebuildRollups(context.Background()); err != nil {
		t.Fatalf("rebuild rollups: %v", err)
	}
	if err := s.RebuildTotals(); err != nil {
		t.Fatalf("rebuild totals: %v", err)
	}
	var minuteErr int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(err),0) FROM snapshot_minute`).Scan(&minuteErr); err != nil {
		t.Fatal(err)
	}
	if minuteErr != 0 {
		t.Fatalf("rebuilt minute rollup still carries errors: %d", minuteErr)
	}
	_, _, _, _, _, totalErr := s.LifetimeTotals()
	if totalErr != 0 {
		t.Fatalf("rebuilt lifetime totals still carry errors: %d", totalErr)
	}
	// Traffic and cost history must survive the rewrite untouched.
	cost, requests, _, _, _, _ := s.LifetimeTotals()
	if requests != 6 {
		t.Fatalf("requests changed by the rewrite: %d, want 6", requests)
	}
	if diffFloat(cost, 0.03) {
		t.Fatalf("cost changed by the rewrite: %v, want 0.03", cost)
	}
	if again, err := s.ZeroErrorCounters(true); err != nil || again.TicksWithErr != 0 {
		t.Fatalf("second dry run not clean: %+v (err %v)", again, err)
	}
}
