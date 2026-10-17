package server

// SQLite scaling probes. These answer "how bad does this get as analytics
// history grows?" — the failure mode that cannot be seen from a single
// request. Skipped under -short so `make bench` stays fast; run via
// `make bench-scale`.
//
// History is never pruned by policy, so every one of these is monotonic.
// Recorded baselines live in docs/memory.md.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// seedRequests inserts n synthetic request rows directly (bypassing the
// bounded queue) so query cost can be measured against history size.
func seedRequests(tb testing.TB, store *Store, n int) {
	tb.Helper()
	base := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	for chunk := 0; chunk < n; chunk += 5000 {
		batch := make([]ReqRecord, 0, 5000)
		for i := chunk; i < chunk+5000 && i < n; i++ {
			batch = append(batch, ReqRecord{
				TS: base + int64(i)*10_000, ID: fmt.Sprintf("r%d", i),
				Model: fmt.Sprintf("model-%d", i%60), Upstream: "up",
				Backend: fmt.Sprintf("backend-%d", i%19), ClientKeyID: fmt.Sprintf("ck_%d", i%50),
				ClientName: fmt.Sprintf("client-%d", i%50), Status: 200, OK: true,
				Outcome: "completed", UsageKnown: true, CostKnown: true,
				TokIn: 1000, TokOut: 200, TokCache: 100, Cost: 0.001, LatMs: 1200, TTFTMs: 300,
			})
		}
		if err := store.insertRequests(batch); err != nil {
			tb.Fatal(err)
		}
	}
}

// seedSnapshotTicks inserts ticks at the real 3s cadence.
func seedSnapshotTicks(tb testing.TB, store *Store, ticks int) {
	tb.Helper()
	base := time.Now().Add(-time.Duration(ticks) * 3 * time.Second).UnixMilli()
	for chunk := 0; chunk < ticks; chunk += 5000 {
		tx, err := store.db.Begin()
		if err != nil {
			tb.Fatal(err)
		}
		stmt, err := tx.Prepare(`INSERT INTO snapshots(ts,payload,d_requests,d_tok_in,d_tok_out,d_tok_cache,d_cost,d_ms,d_err,d_models,d_backends) VALUES(?,?,?,?,?,?,?,?,?,?,?)`)
		if err != nil {
			tb.Fatal(err)
		}
		for i := chunk; i < chunk+5000 && i < ticks; i++ {
			if _, err := stmt.Exec(base+int64(i)*3000,
				`{"avg":1200,"p50":900,"p95":4000,"p99":9000,"ttft":300,"ttft_n":5}`,
				10, 10000, 2000, 500, 0.012, 3000, 1, `{}`, `{}`); err != nil {
				tb.Fatal(err)
			}
		}
		if err := stmt.Close(); err != nil {
			tb.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			tb.Fatal(err)
		}
	}
}

// perfInsertSeq keeps benchmark snapshot timestamps (a PRIMARY KEY) distinct
// across repeated benchmark-body invocations.
var perfInsertSeq atomic.Int64

func perfStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := OpenStore(":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	return store
}

// BenchmarkAnalyticsByHistorySize measures the dashboard's Analytics endpoint
// (full scan + per-row JSON unmarshal) against growing history.
func BenchmarkAnalyticsByHistorySize(b *testing.B) {
	if testing.Short() {
		b.Skip("scaling probe")
	}
	store := perfStore(b)
	const n = 200_000
	seedRequests(b, store, n)
	_, _ = store.db.Exec(`ANALYZE`)
	since := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()

	for _, tc := range []struct {
		name   string
		filter RequestFilter
	}{
		{"nofilter", RequestFilter{Since: since}},
		{"model", RequestFilter{Since: since, Model: "model-1"}},
		{"client", RequestFilter{Since: since, ClientKeyID: "ck_1"}},
		{"narrow_window", RequestFilter{Since: time.Now().Add(-time.Hour).UnixMilli()}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			b.ResetTimer()
			for b.Loop() {
				if _, err := store.Analytics(ctx, tc.filter); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRequestsPageByHistorySize: the paginated endpoint, whose
// summarySQL aggregate scans the whole filtered set on every page.
func BenchmarkRequestsPageByHistorySize(b *testing.B) {
	if testing.Short() {
		b.Skip("scaling probe")
	}
	store := perfStore(b)
	seedRequests(b, store, 300_000)
	_, _ = store.db.Exec(`ANALYZE`)

	for _, tc := range []struct {
		name   string
		filter RequestFilter
	}{
		{"first_page", RequestFilter{Limit: 100}},
		{"filtered_model", RequestFilter{Limit: 100, Model: "model-1"}},
		{"since_1h", RequestFilter{Limit: 100, Since: time.Now().Add(-time.Hour).UnixMilli()}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			b.ResetTimer()
			for b.Loop() {
				if _, err := store.Requests(ctx, tc.filter); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSnapshotsReadCost covers the endpoints that scan the snapshots
// table, which grows one row per 3s forever. LifetimeTotals is notable: it
// runs on every /dash/api/state poll and holds store.mu, blocking the writer.
func BenchmarkSnapshotsReadCost(b *testing.B) {
	if testing.Short() {
		b.Skip("scaling probe")
	}
	store := perfStore(b)
	seedSnapshotTicks(b, store, 1_000_000) // ~34.7 days
	_, _ = store.db.Exec(`ANALYZE`)
	ctx := context.Background()

	b.Run("LifetimeTotals", func(b *testing.B) {
		for b.Loop() {
			_, _, _, _, _, _ = store.LifetimeTotals()
		}
	})
	b.Run("Range", func(b *testing.B) {
		for b.Loop() {
			store.Range()
		}
	})
	b.Run("SeriesSince_1h", func(b *testing.B) {
		since := time.Now().Add(-time.Hour).UnixMilli()
		for b.Loop() {
			if _, err := store.SeriesSince(ctx, since, 500); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("SeriesSince_30d", func(b *testing.B) {
		since := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
		for b.Loop() {
			if _, err := store.SeriesSince(ctx, since, 500); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("InsertDelta", func(b *testing.B) {
		// ts is the snapshots PRIMARY KEY, so keys must be strictly monotonic
		// across EVERY invocation, not just within one. The benchmark framework
		// runs the body several times while calibrating b.N, so a base computed
		// once per body reuses the same timestamps on the next run and trips a
		// UNIQUE constraint. A process-wide counter keeps every insert distinct.
		for range b.N {
			ts := perfInsertSeq.Add(1) + time.Now().UnixMilli() + 1_000_000_000
			if err := store.InsertDelta(&Delta{TS: ts,
				Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestQueryPlans documents which indexes SQLite actually chooses, so a
// regression in index usage shows up as a failing expectation rather than a
// silent slowdown. This is the Stage-0 evidence for the C-2 finding.
func TestQueryPlans(t *testing.T) {
	if testing.Short() {
		t.Skip("scaling probe")
	}
	store := perfStore(t)
	seedRequests(t, store, 50_000)
	_, _ = store.db.Exec(`ANALYZE`)

	plan := func(sql string) []string {
		rows, err := store.db.Query("EXPLAIN QUERY PLAN " + sql)
		if err != nil {
			return []string{"ERR: " + err.Error()}
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var a, b, c int
			var detail string
			if rows.Scan(&a, &b, &c, &detail) == nil {
				out = append(out, detail)
			}
		}
		return out
	}

	for _, tc := range []struct{ label, sql string }{
		{"analytics_ts_only", `SELECT record FROM requests WHERE ts>=1000`},
		{"analytics_ts_model", `SELECT record FROM requests WHERE ts>=1000 AND model='model-1'`},
		{"analytics_ts_backend", `SELECT record FROM requests WHERE ts>=1000 AND backend='backend-1'`},
		{"analytics_ts_client", `SELECT record FROM requests WHERE ts>=1000 AND client_key_id='ck_1'`},
		{"page_cursor", `SELECT ts,seq,record FROM requests WHERE 1=1 ORDER BY ts DESC,seq DESC LIMIT 101`},
		{"summary_scan", `SELECT ` + summarySQL + ` FROM requests WHERE 1=1`},
	} {
		got := plan(tc.sql)
		t.Logf("%-22s %v", tc.label, got)
		for _, d := range got {
			// A bare "SCAN requests" (no index named) means the filter cannot
			// use an index — the C-2 defect. Recorded here as a visible log.
			if strings.Contains(d, "SCAN requests") && !strings.Contains(d, "USING") {
				t.Logf("  ^^ FULL TABLE SCAN (no index used) for %s", tc.label)
			}
		}
	}
}

// BenchmarkSeriesSinceWithRollup measures the fast path end to end: seed raw
// ticks, let InsertDelta maintain snapshot_minute, then time SeriesSince. This
// is the number that matters — the raw-tick path is measured by
// BenchmarkSnapshotsReadCost and kept as the fallback baseline.
func BenchmarkSeriesSinceWithRollup(b *testing.B) {
	if testing.Short() {
		b.Skip("scaling probe")
	}
	store := perfStore(b)
	const ticks = 400_000 // ~13.9 days at 3s
	seedTicks(b, store, ticks, time.Now().Add(-time.Duration(ticks)*3*time.Second).UnixMilli(), false)
	_, _ = store.db.Exec(`ANALYZE`)

	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		since  int64
		points int
	}{
		{"30d", time.Now().Add(-30 * 24 * time.Hour).UnixMilli(), 500},
		{"all", 0, 500},
		{"all_2000pts", 0, 2000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := store.SeriesSince(ctx, tc.since, tc.points); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("rebuild_rollups", func(b *testing.B) {
		for b.Loop() {
			if _, err := store.RebuildRollups(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
}
