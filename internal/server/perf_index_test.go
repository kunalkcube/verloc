package server

// Guardrails for the ts-leading covering indexes added to requests. The
// dashboard's Analytics endpoint selects the non-indexed `record` column, so
// without these the planner abandons the (col,ts) indexes and full-scans the
// table. These tests assert the indexes exist after migrate on a fresh DB and
// that the planner actually chooses them once statistics are realistic.

import (
	"strings"
	"testing"
)

// TestMigrateAddsTsLeadingIndexes checks the additive indexes are created by
// migrate() on a fresh database. migrate runs on every boot with
// CREATE INDEX IF NOT EXISTS, so this also documents that the change is
// idempotent — a second OpenStore over the same file must not error.
func TestMigrateAddsTsLeadingIndexes(t *testing.T) {
	store := perfStore(t)
	want := []string{
		"idx_requests_ts_model",
		"idx_requests_ts_backend",
		"idx_requests_ts_client",
		"idx_requests_ts_status_ok",
	}
	for _, name := range want {
		var got string
		if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got); err != nil {
			t.Fatalf("index %s missing after migrate: %v", name, err)
		}
	}
	// Idempotency: migrate again on the live handle must be a no-op, not an error.
	if err := store.migrate(); err != nil {
		t.Fatalf("second migrate not idempotent: %v", err)
	}
}

// TestFilteredAnalyticsPlanUsesIndex asserts the planner stops reporting a bare
// `SCAN requests` for a time-window + model filter. It seeds 50k rows so the
// plan is meaningful — on an empty table SQLite always chooses a scan.
func TestFilteredAnalyticsPlanUsesIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("scaling probe")
	}
	store := perfStore(t)
	seedRequests(t, store, 50_000)
	if _, err := store.db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	plan := func(sql string) []string {
		rows, err := store.db.Query("EXPLAIN QUERY PLAN " + sql)
		if err != nil {
			t.Fatalf("explain %q: %v", sql, err)
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

	// The invariant that matters is "the planner uses SOME index for the time
	// window", not a specific index name: with realistic statistics SQLite may
	// prefer the existing (col,ts) equality index for `model=? AND ts>?` and the
	// new ts-leading index for the time-only probe. Asserting a bare scan never
	// reappears is the actual regression guard.
	for _, tc := range []struct{ label, sql string }{
		{"analytics_ts_only", `SELECT record FROM requests WHERE ts>=1000`},
		{"analytics_ts_model", `SELECT record FROM requests WHERE ts>=1000 AND model='model-1'`},
		{"analytics_ts_backend", `SELECT record FROM requests WHERE ts>=1000 AND backend='backend-1'`},
		{"analytics_ts_client", `SELECT record FROM requests WHERE ts>=1000 AND client_key_id='ck_1'`},
		{"analytics_ts_status", `SELECT record FROM requests WHERE ts>=1000 AND status=200 AND ok=1`},
	} {
		got := plan(tc.sql)
		t.Logf("%-22s %v", tc.label, got)
		joined := strings.Join(got, " | ")
		if !strings.Contains(joined, "USING INDEX") && !strings.Contains(joined, "USING COVERING INDEX") {
			t.Errorf("%s: planner did not use any index: %v", tc.label, got)
		}
		for _, d := range got {
			if strings.Contains(d, "SCAN requests") && !strings.Contains(d, "USING") {
				t.Errorf("%s: still a bare full scan: %v", tc.label, got)
			}
		}
	}
}
