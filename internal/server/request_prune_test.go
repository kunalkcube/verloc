package server

import (
	"testing"
)

// queueRequestAt enqueues one request record and drains the bounded writer so
// the row is queryable deterministically.
func queueRequestAt(t *testing.T, s *Store, rec ReqRecord) {
	t.Helper()
	if !s.QueueRequest(rec) {
		t.Fatal("unexpected telemetry drop")
	}
	if err := s.flushPending(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func requestRowCounts(t *testing.T, s *Store) (total, failed int) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(ok=0),0) FROM requests`).Scan(&total, &failed); err != nil {
		t.Fatalf("count: %v", err)
	}
	return total, failed
}

func failedRecord(ts int64, id string, status int, outcome string) ReqRecord {
	r := analyticsRecord(ts, id)
	r.OK = false
	r.Status = status
	r.Outcome = outcome
	return r
}

// TestPruneFailedRequestsDeletesOnlyFailedOutcomes pins the maintenance
// contract: the dry run reports without writing, apply removes exactly the
// ok=0 rows, and success rows survive untouched.
func TestPruneFailedRequestsDeletesOnlyFailedOutcomes(t *testing.T) {
	s := newTestStore(t)
	queueRequestAt(t, s, analyticsRecord(1000, "ok-1"))
	queueRequestAt(t, s, failedRecord(2000, "fail-1", 503, "unavailable"))
	queueRequestAt(t, s, analyticsRecord(3000, "ok-2"))
	queueRequestAt(t, s, failedRecord(4000, "fail-2", 499, "canceled"))

	stats, err := s.PruneFailedRequests(true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if stats.Total != 4 || stats.Failed != 2 || stats.OldestTs != 2000 || stats.NewestTs != 4000 {
		t.Fatalf("dry-run stats wrong: %+v", stats)
	}
	if total, failed := requestRowCounts(t, s); total != 4 || failed != 2 {
		t.Fatalf("dry run wrote to disk: total=%d failed=%d", total, failed)
	}

	stats, err = s.PruneFailedRequests(false)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if stats.Failed != 2 {
		t.Fatalf("apply deleted %d rows, want 2", stats.Failed)
	}
	total, failed := requestRowCounts(t, s)
	if total != 2 || failed != 0 {
		t.Fatalf("after apply: total=%d failed=%d, want 2/0", total, failed)
	}
	rows, err := s.db.Query(`SELECT request_id FROM requests ORDER BY ts ASC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "ok-1" || got[1] != "ok-2" {
		t.Fatalf("survivors wrong: %v, want [ok-1 ok-2]", got)
	}
}

// TestPruneFailedRequestsIsIdempotent proves a second pass is a no-op, so an
// accidental re-run cannot delete anything further.
func TestPruneFailedRequestsIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		queueRequestAt(t, s, failedRecord(int64(1000+i), "fail", 502, "upstream_rejected"))
	}
	if _, err := s.PruneFailedRequests(false); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	stats, err := s.PruneFailedRequests(false)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if stats.Total != 0 || stats.Failed != 0 {
		t.Fatalf("second pass found rows: %+v", stats)
	}
}

// TestPruneFailedRequestsEmptyTableIsSafe covers the no-history case.
func TestPruneFailedRequestsEmptyTableIsSafe(t *testing.T) {
	s := newTestStore(t)
	stats, err := s.PruneFailedRequests(false)
	if err != nil {
		t.Fatalf("empty table: %v", err)
	}
	if stats.Total != 0 || stats.Failed != 0 || stats.OldestTs != 0 || stats.NewestTs != 0 {
		t.Fatalf("empty table produced counts: %+v", stats)
	}
}
