package server

// Guards for the request-page summary memo. A cached aggregate must never
// outlive the rows it describes, and paging must not change any total.

import (
	"context"
	"testing"
	"time"
)

// TestRequestsSummaryMemoDoesNotOutliveInserts is the correctness guard for
// the memo. The memo is TTL-only (write-invalidation made it dead under
// continuous load), so the guarantee is BOUNDED staleness: after new rows
// land, totals must become correct within the TTL, never stay stale.
func TestRequestsSummaryMemoDoesNotOutliveInserts(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-30 * time.Minute).UnixMilli()
	insert := func(n int, offset int) {
		var batch []ReqRecord
		for i := range n {
			batch = append(batch, ReqRecord{TS: base + int64(offset+i)*1000, ID: "r", Model: "m", Backend: "b",
				Status: 200, OK: true, Outcome: "completed", UsageKnown: true, CostKnown: true,
				TokIn: 100, TokOut: 50, Cost: 0.01, LatMs: 100})
		}
		if err := s.insertRequests(batch); err != nil {
			t.Fatal(err)
		}
	}

	insert(10, 0)
	first, err := s.Requests(context.Background(), RequestFilter{Limit: 5, Since: base})
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.Requests != 10 {
		t.Fatalf("first summary requests = %d, want 10", first.Summary.Requests)
	}

	// Page 2 with the same filter: memo should serve the same total.
	page2, err := s.Requests(context.Background(), RequestFilter{Limit: 5, Since: base, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if page2.Summary.Requests != 10 {
		t.Fatalf("paged summary requests = %d, want 10 (memo wrong within a filter)", page2.Summary.Requests)
	}

	// New rows arrive: totals must converge within the memo TTL.
	insert(7, 100)
	var after RequestPage
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		after, err = s.Requests(context.Background(), RequestFilter{Limit: 5, Since: base})
		if err != nil {
			t.Fatal(err)
		}
		if after.Summary.Requests == 17 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after.Summary.Requests != 17 {
		t.Fatalf("summary requests = %d, want 17; the memo stayed stale past its TTL", after.Summary.Requests)
	}
	if diffFloat(after.Summary.Cost, 0.17) {
		t.Fatalf("summary cost = %v, want 0.17", after.Summary.Cost)
	}
}

// TestRequestsSummaryMatchesUncached proves the memo does not change results:
// a fresh Store with no cache must report identical numbers for the same filter.
func TestRequestsSummaryMatchesUncached(t *testing.T) {
	a := testKeyEventStore(t)
	b := testKeyEventStore(t)
	base := time.Now().Add(-20 * time.Minute).UnixMilli()
	for _, s := range []*Store{a, b} {
		var batch []ReqRecord
		for i := range 40 {
			ok := i%5 != 0
			usage := i%7 != 0
			batch = append(batch, ReqRecord{TS: base + int64(i)*1000, ID: "r", Model: "m", Backend: "b",
				Status: 200, OK: ok, Outcome: "completed", UsageKnown: usage, CostKnown: usage,
				TokIn: uint64(100 + i), TokOut: 50, Cost: 0.01, LatMs: float64(100 + i)})
		}
		if err := s.insertRequests(batch); err != nil {
			t.Fatal(err)
		}
	}
	f := RequestFilter{Limit: 10, Since: base}
	// a: queried twice so the second call hits the memo.
	if _, err := a.Requests(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	got, err := a.Requests(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	want, err := b.Requests(context.Background(), f) // b: cache cold
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != want.Summary {
		t.Fatalf("memoised summary differs from uncached:\n got  %+v\n want %+v", got.Summary, want.Summary)
	}
	if got.Total != want.Total {
		t.Fatalf("total differs: %d vs %d", got.Total, want.Total)
	}
}

// TestAnalyticsColumnProjectionMatchesRecordPath guards the SQL projection
// change: accumulating from columns must equal accumulating from the JSON
// record for the same rows.
func TestAnalyticsColumnProjectionMatchesRecordPath(t *testing.T) {
	s := testKeyEventStore(t)
	base := time.Now().Add(-45 * time.Minute).UnixMilli()

	var batch []ReqRecord
	for i := range 60 {
		ok := i%6 != 0
		usage := i%5 != 0
		costKnown := i%4 != 0
		batch = append(batch, ReqRecord{TS: base + int64(i)*1000, ID: "r",
			Model: "m/a", Upstream: "up", Backend: "b", ClientKeyID: "ck", ClientName: "client",
			Status: 200, OK: ok, Outcome: "completed", UsageKnown: usage, CostKnown: costKnown,
			TokIn: uint64(10 + i), TokOut: uint64(5 + i), TokCache: uint64(i), Cost: 0.002,
			LatMs: float64(50 + i*3), TTFTMs: float64(10 + i)})
	}
	if err := s.insertRequests(batch); err != nil {
		t.Fatal(err)
	}

	// Reference: fold the same records through the ReqRecord path.
	ref := &usageAccumulator{}
	for _, r := range batch {
		ref.addRecord(r)
	}
	want := ref.summary()

	got, err := s.Analytics(context.Background(), RequestFilter{Since: base})
	if err != nil {
		t.Fatal(err)
	}
	// Cost is compared with tolerance: SQL SUM and a sequential Go fold sum
	// the same values in different orders, so bit-exact float equality is
	// not a meaningful requirement. Everything else must match exactly.
	if got.Summary.Requests != want.Requests || got.Summary.Errors != want.Errors ||
		got.Summary.TokIn != want.TokIn || got.Summary.TokOut != want.TokOut ||
		got.Summary.TokCache != want.TokCache || got.Summary.UnknownUsage != want.UnknownUsage ||
		got.Summary.UnknownCost != want.UnknownCost || got.Summary.AvgLatencyMs != want.AvgLatencyMs ||
		got.Summary.AvgTTFTMs != want.AvgTTFTMs || diffFloat(got.Summary.Cost, want.Cost) {
		t.Fatalf("SQL aggregation disagrees with record fold:\n got  %+v\n want %+v", got.Summary, want)
	}
	var totalRequests int64
	for _, g := range got.Models {
		totalRequests += g.Requests
	}
	if totalRequests != 60 {
		t.Fatalf("model group requests = %d, want 60", totalRequests)
	}
	for _, g := range got.Clients {
		if g.Name != "client" {
			t.Fatalf("client name = %q, want %q", g.Name, "client")
		}
	}
}
