package server

import (
	"fmt"
	"testing"
	"time"
)

// newTestStore opens an isolated analytics store for a test.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// insertAlertAt stores an alert with an explicit timestamp so history can be laid
// out deterministically.
func insertAlertAt(t *testing.T, s *Store, ts int64, rule, metric string, v float64) {
	t.Helper()
	if err := s.InsertAlert(AlertEvent{
		TS: ts, RuleID: rule, Name: metric + " · global", Severity: "warning",
		Scope: "global", Metric: metric, Value: v, Threshold: 1,
		Message: fmt.Sprintf("%s %.2f >= 1.00 over global", metric, v),
	}); err != nil {
		t.Fatalf("InsertAlert: %v", err)
	}
}

// TestCollapseAlertHistoryKeepsIncidentCadence pins the core behaviour: a flood
// recorded before the engine enforced a cooldown collapses to one row per window
// per rule, keeping the first evaluation of each incident.
func TestCollapseAlertHistoryKeepsIncidentCadence(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()

	// 100 rows at the old 5s eval cadence, all one rule.
	for i := 0; i < 100; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "latency-p95-global", "latency_p95", 30000)
	}

	stats, err := s.CollapseAlertHistory(900, false)
	if err != nil {
		t.Fatalf("CollapseAlertHistory: %v", err)
	}
	if stats.Before != 100 {
		t.Fatalf("Before = %d, want 100", stats.Before)
	}
	// 100 rows spanning 495s with a 900s window => only the first survives.
	if stats.After != 1 {
		t.Fatalf("After = %d, want 1 (the incident's first row)", stats.After)
	}
	if stats.Dropped != 99 {
		t.Fatalf("Dropped = %d, want 99", stats.Dropped)
	}

	got := s.RecentAlerts(1000)
	if len(got) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(got))
	}
	if got[0].TS != base {
		t.Fatalf("kept the wrong row: ts=%d, want the first (%d)", got[0].TS, base)
	}
	if got[0].Value != 30000 || got[0].RuleID != "latency-p95-global" {
		t.Fatalf("row was not preserved faithfully: %+v", got[0])
	}
}

// TestCollapseAlertHistoryPreservesSeparateIncidents proves it removes duplicate
// re-firings without erasing genuinely distinct incidents.
func TestCollapseAlertHistoryPreservesSeparateIncidents(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()

	// Incident 1: persistent for ~120s (5s cadence).
	for i := 0; i < 25; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "latency-p95-global", "latency_p95", 30000)
	}
	// Incident 2, well past the cooldown, persistent for ~120s.
	start2 := base + 3_600_000
	for i := 0; i < 25; i++ {
		insertAlertAt(t, s, start2+int64(i)*5000, "latency-p95-global", "latency_p95", 20000)
	}

	stats, err := s.CollapseAlertHistory(900, false)
	if err != nil {
		t.Fatalf("CollapseAlertHistory: %v", err)
	}
	if stats.After != 2 {
		t.Fatalf("After = %d, want 2 (one row per distinct incident)", stats.After)
	}
	got := s.RecentAlerts(1000)
	if len(got) != 2 {
		t.Fatalf("stored rows = %d, want 2", len(got))
	}
	// Newest first, and each incident kept its own first evaluation.
	if got[0].TS != start2 || got[1].TS != base {
		t.Fatalf("incident boundaries not preserved: %d, %d", got[0].TS, got[1].TS)
	}
}

// TestCollapseAlertHistoryIsPerRule proves one rule's noisy window never
// suppresses a different rule's genuine alert.
func TestCollapseAlertHistoryIsPerRule(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()

	for i := 0; i < 10; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "latency-p95-global", "latency_p95", 30000)
	}
	// Different rule, same instants: must survive.
	for i := 0; i < 10; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "error-rate-global", "error_rate", 60)
	}

	stats, err := s.CollapseAlertHistory(900, false)
	if err != nil {
		t.Fatalf("CollapseAlertHistory: %v", err)
	}
	if stats.After != 2 {
		t.Fatalf("After = %d, want 2 (one per rule)", stats.After)
	}
	if stats.Rules["latency-p95-global"] != 9 || stats.Rules["error-rate-global"] != 9 {
		t.Fatalf("per-rule drops wrong: %+v", stats.Rules)
	}
}

// TestCollapseAlertHistoryDryRunWritesNothing is the safety property: the default
// must be a no-op on disk.
func TestCollapseAlertHistoryDryRunWritesNothing(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	for i := 0; i < 50; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "cost-hour-global", "cost_per_hour", 2)
	}
	before := s.RecentAlerts(1000)

	stats, err := s.CollapseAlertHistory(900, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if stats.Before != 50 || stats.After != 1 || stats.Dropped != 49 {
		t.Fatalf("dry-run counts wrong: %+v", stats)
	}
	after := s.RecentAlerts(1000)
	if len(after) != len(before) {
		t.Fatalf("dry run modified disk: %d rows before, %d after", len(before), len(after))
	}
	for i := range before {
		if before[i].TS != after[i].TS || before[i].Value != after[i].Value {
			t.Fatalf("dry run altered row %d", i)
		}
	}
}

// TestCollapseAlertHistoryIsIdempotent proves a second pass is a no-op, so an
// accidental re-run cannot keep chipping rows away.
func TestCollapseAlertHistoryIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	for i := 0; i < 60; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "latency-p95-global", "latency_p95", 30000)
	}
	first, err := s.CollapseAlertHistory(900, false)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := s.CollapseAlertHistory(900, false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.Dropped != 0 {
		t.Fatalf("second pass dropped %d rows, want 0 (not idempotent)", second.Dropped)
	}
	if second.After != first.After {
		t.Fatalf("second pass changed the count: %d -> %d", first.After, second.After)
	}
}

// TestCollapseAlertHistoryDefaultsCooldown proves a zero/negative cooldown falls
// back to the engine default instead of silently deleting everything.
func TestCollapseAlertHistoryDefaultsCooldown(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		insertAlertAt(t, s, base+int64(i)*5000, "latency-p95-global", "latency_p95", 30000)
	}
	stats, err := s.CollapseAlertHistory(0, false)
	if err != nil {
		t.Fatalf("zero cooldown: %v", err)
	}
	if stats.After != 1 {
		t.Fatalf("zero cooldown behaved unexpectedly: After = %d, want 1", stats.After)
	}
}

// TestCollapseAlertHistoryEmptyTableIsSafe covers the no-history case.
func TestCollapseAlertHistoryEmptyTableIsSafe(t *testing.T) {
	s := newTestStore(t)
	stats, err := s.CollapseAlertHistory(900, false)
	if err != nil {
		t.Fatalf("empty table: %v", err)
	}
	if stats.Before != 0 || stats.After != 0 || stats.Dropped != 0 {
		t.Fatalf("empty table produced counts: %+v", stats)
	}
}

// TestCollapseAlertHistoryKeepsOrdering proves the id rewrite (AUTOINCREMENT ids
// cannot be preserved) does not disturb what the read path returns.
func TestCollapseAlertHistoryKeepsOrdering(t *testing.T) {
	s := newTestStore(t)
	base := time.Now().UnixMilli()
	var want []int64
	for i := 0; i < 5; i++ {
		ts := base + int64(i)*1_000_000 // 1000s apart: all survive a 900s window
		want = append(want, ts)
		insertAlertAt(t, s, ts, "latency-p95-global", "latency_p95", float64(10000+i))
	}
	if _, err := s.CollapseAlertHistory(900, false); err != nil {
		t.Fatalf("collapse: %v", err)
	}
	got := s.RecentAlerts(1000)
	if len(got) != len(want) {
		t.Fatalf("rows = %d, want %d", len(got), len(want))
	}
	for i, ts := range want { // RecentAlerts is newest-first
		if got[len(want)-1-i].TS != ts {
			t.Fatalf("ordering broken at %d: got %d, want %d", i, got[len(want)-1-i].TS, ts)
		}
	}
}
