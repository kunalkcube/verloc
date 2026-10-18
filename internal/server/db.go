package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the SQLite-backed history for the dashboard. Aggregate writes
// happen off the request hot path (a 3s background ticker). The snapshots
// table keeps the exact schema of the legacy Node collector so an existing
// verloc.db migrates in place and history survives the upgrade.
//
// Key-pool transition events (cooldown/recover) fire synchronously inside
// request handling, so they are enqueued to a bounded channel and persisted
// by a background writer (dropping on overflow). A synchronous INSERT here
// once wedged the whole server: a newly added provider with bad keys emits
// a failure event per attempt, agents retry in a storm, and every request
// serialized on SQLite behind the ticker and dashboard reads.
type Store struct {
	db           *sql.DB
	mu           sync.Mutex
	queueMu      sync.RWMutex
	requestCh    chan ReqRecord
	keyEvCh      chan keyEventReq
	flushCh      chan chan error
	done         chan struct{}
	closed       bool
	closeOnce    sync.Once
	closeErr     error
	keyEvDrop    atomic.Uint64
	requestDrop  atomic.Uint64
	summaryMu    sync.Mutex
	summaryCache map[string]summaryCacheEntry
	writeErrors  atomic.Uint64
	lastWrite    atomic.Int64
	queued       atomic.Int64
}

type keyEventReq struct {
	ts      int64
	backend string
	key     string
	event   string
	status  int
	msg     string
}

func OpenStore(path string) (*Store, error) {
	if path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		err = f.Chmod(0600)
		closeErr := f.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	// busy_timeout must be in the DSN so EVERY pooled connection gets it.
	// Setting it with a one-off Exec (as migrate does) configures only the
	// connection that happened to run it; the others fall back to SQLite's
	// default of 0 and return SQLITE_BUSY immediately under contention.
	dsn := path
	if path != ":memory:" {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		dsn = path + sep + "_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc driver serializes internally; a modest pool is fine.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	}
	s := &Store{db: db, requestCh: make(chan ReqRecord, 2048), keyEvCh: make(chan keyEventReq, 1024), flushCh: make(chan chan error), done: make(chan struct{})}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	go s.runWriter()
	return s, nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS requests (
		 seq INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, request_id TEXT NOT NULL,
		 model TEXT NOT NULL, upstream TEXT NOT NULL, backend TEXT NOT NULL,
		 client_key_id TEXT NOT NULL, client_name TEXT NOT NULL, status INTEGER NOT NULL,
		 ok INTEGER NOT NULL, outcome TEXT NOT NULL, usage_known INTEGER NOT NULL, cost_known INTEGER NOT NULL,
		 tok_in INTEGER NOT NULL, tok_out INTEGER NOT NULL, tok_cache INTEGER NOT NULL, cost REAL NOT NULL,
		 lat_ms REAL NOT NULL, ttft_ms REAL NOT NULL, record TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_order ON requests(ts DESC,seq DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model,ts)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_backend ON requests(backend,ts)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_client ON requests(client_key_id,ts)`,
		// Leading-ts indexes for the dominant analytics predicate. The
		// dashboard reads one column per matching row (`SELECT record`), and
		// the existing (col,ts) indexes do not cover it, so the planner
		// abandoned them for a full `SCAN requests` — 1.15s over 200k rows.
		// The time window (`ts>=?`) is always present and is a range, so ts
		// must lead; the secondary column filters within the scanned range.
		// These are additive and idempotent on every boot; they cost disk and
		// insert time, an accepted tradeoff since history is never pruned.
		`CREATE INDEX IF NOT EXISTS idx_requests_ts_model ON requests(ts, model)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_ts_backend ON requests(ts, backend)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_ts_client ON requests(ts, client_key_id)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_ts_status_ok ON requests(ts, status, ok)`,
		// Covering index for the unfiltered Analytics projection. The query sums
		// these columns and never reads the JSON `record` blob, so an index-only
		// scan avoids paging through the much larger table rows.
		`CREATE INDEX IF NOT EXISTS idx_requests_analytics_cover ON requests(
		   ts, model, backend, client_key_id, client_name, status, ok,
		   tok_in, tok_out, tok_cache, cost, usage_known, cost_known, lat_ms, ttft_ms)`,
		`CREATE TABLE IF NOT EXISTS request_hourly (
		 hour INTEGER NOT NULL, model TEXT NOT NULL, upstream TEXT NOT NULL, backend TEXT NOT NULL,
		 client_key_id TEXT NOT NULL, client_name TEXT NOT NULL, status INTEGER NOT NULL, ok INTEGER NOT NULL,
		 requests INTEGER NOT NULL, errors INTEGER NOT NULL, tok_in INTEGER NOT NULL, tok_out INTEGER NOT NULL,
		 tok_cache INTEGER NOT NULL, cost REAL NOT NULL, unknown_usage INTEGER NOT NULL, unknown_cost INTEGER NOT NULL,
		 lat_sum REAL NOT NULL, ttft_sum REAL NOT NULL, ttft_n INTEGER NOT NULL,
		 lat_hist TEXT NOT NULL, ttft_hist TEXT NOT NULL,
		 mirrors_detail INTEGER NOT NULL DEFAULT 0,
		 PRIMARY KEY(hour,model,upstream,backend,client_key_id,client_name,status,ok))`,
		`CREATE TABLE IF NOT EXISTS analytics_meta (name TEXT PRIMARY KEY, value INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
		   ts           INTEGER PRIMARY KEY,
		   payload      TEXT    NOT NULL DEFAULT '',
		   d_requests   INTEGER NOT NULL DEFAULT 0,
		   d_tok_in     INTEGER NOT NULL DEFAULT 0,
		   d_tok_out    INTEGER NOT NULL DEFAULT 0,
		   d_tok_cache  INTEGER NOT NULL DEFAULT 0,
		   d_cost       REAL    NOT NULL DEFAULT 0,
		   d_ms         INTEGER NOT NULL DEFAULT 0,
		   d_err        INTEGER NOT NULL DEFAULT 0,
		   d_models     TEXT    NOT NULL DEFAULT '{}',
		   d_backends   TEXT    NOT NULL DEFAULT '{}'
		 );`,
		`CREATE INDEX IF NOT EXISTS idx_snap_ts ON snapshots(ts);`,
		// Derived caches. `requests` and `snapshots` stay the immutable detail
		// authority; these tables only take work OFF THE READ PATH by letting a
		// 30-day chart aggregate minutes instead of re-reading ~864k raw ticks
		// (measured 7.5s and 892MB per poll). They are additive-only and never
		// used to double-count detail: for a window that predates their earliest
		// row, the reader transparently falls back to raw ticks.
		`CREATE TABLE IF NOT EXISTS snapshot_minute (
		   minute INTEGER PRIMARY KEY,
		   requests INTEGER NOT NULL, err INTEGER NOT NULL,
		   tok_in INTEGER NOT NULL, tok_out INTEGER NOT NULL, tok_cache INTEGER NOT NULL,
		   cost REAL NOT NULL, ms INTEGER NOT NULL,
		   lat_w REAL NOT NULL, lat_sum REAL NOT NULL, ttft_w REAL NOT NULL, ttft_sum REAL NOT NULL,
		   hist TEXT NOT NULL, ttft_hist TEXT NOT NULL,
		   hist_n INTEGER NOT NULL DEFAULT 0, missing_n INTEGER NOT NULL DEFAULT 0,
		   models TEXT NOT NULL, backends TEXT NOT NULL
		 )`,
		`CREATE TABLE IF NOT EXISTS analytics_totals (
		   id INTEGER PRIMARY KEY CHECK (id=1),
		   cost REAL NOT NULL, requests INTEGER NOT NULL,
		   tok_in INTEGER NOT NULL, tok_out INTEGER NOT NULL, tok_cache INTEGER NOT NULL,
		   err INTEGER NOT NULL,
		   oldest_ts INTEGER NOT NULL, newest_ts INTEGER NOT NULL, ticks INTEGER NOT NULL
		 )`,
		`INSERT OR IGNORE INTO analytics_totals(id,cost,requests,tok_in,tok_out,tok_cache,err,oldest_ts,newest_ts,ticks)
		   VALUES(1,0,0,0,0,0,0,0,0,0)`,
		`CREATE TABLE IF NOT EXISTS alerts (
		   id         INTEGER PRIMARY KEY AUTOINCREMENT,
		   ts         INTEGER NOT NULL,
		   rule_id    TEXT    NOT NULL,
		   name       TEXT    NOT NULL,
		   severity   TEXT    NOT NULL,
		   scope      TEXT    NOT NULL,
		   metric     TEXT    NOT NULL,
		   value      REAL    NOT NULL,
		   threshold  REAL    NOT NULL,
		   message    TEXT    NOT NULL
		 );`,
		`CREATE INDEX IF NOT EXISTS idx_alerts_ts ON alerts(ts);`,
		`CREATE TABLE IF NOT EXISTS key_events (
		   id       INTEGER PRIMARY KEY AUTOINCREMENT,
		   ts       INTEGER NOT NULL,
		   backend  TEXT    NOT NULL,
		   key      TEXT    NOT NULL,
		   event    TEXT    NOT NULL,
		   status   INTEGER NOT NULL DEFAULT 0,
		   msg      TEXT    NOT NULL DEFAULT ''
		 );`,
		`CREATE INDEX IF NOT EXISTS idx_key_events_ts ON key_events(ts);`,
		`CREATE TABLE IF NOT EXISTS legacy_hourly (hour INTEGER PRIMARY KEY,requests INTEGER NOT NULL,errors INTEGER NOT NULL,tok_in INTEGER NOT NULL,tok_out INTEGER NOT NULL,tok_cache INTEGER NOT NULL,cost REAL NOT NULL)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	// Additive migration for databases created before mirrors_detail existed.
	// SQLite has no ADD COLUMN IF NOT EXISTS, so the duplicate-column error is
	// the expected already-migrated signal. Default 0 preserves the existing
	// legacy semantics (hourly rows are additive with detail).
	if _, err := s.db.Exec(`ALTER TABLE request_hourly ADD COLUMN mirrors_detail INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT OR IGNORE INTO analytics_meta(name,value) VALUES ('legacy_imported',1)`)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		if _, err = tx.Exec(`INSERT INTO legacy_hourly SELECT ts/3600000*3600000,SUM(d_requests),SUM(d_err),SUM(d_tok_in),SUM(d_tok_out),SUM(d_tok_cache),SUM(d_cost) FROM snapshots GROUP BY ts/3600000`); err != nil {
			return err
		}
	}
	// Legacy scrub: current code never populates these columns, so only pay
	// for the full-table UPDATE when a legacy row actually needs it.
	if _, err = tx.Exec(`UPDATE key_events SET key='',msg='' WHERE (key!='' OR msg!='') AND EXISTS (SELECT 1 FROM key_events WHERE key!='' OR msg!='' LIMIT 1)`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Refresh planner statistics for the new ts-leading indexes. Two guards
	// keep this cheap and safe:
	//   * analysis_limit caps how many rows ANALYZE samples per index, so it
	//     can never become a full-table ANALYZE on a large (200k+ row) DB;
	//   * PRAGMA optimize only analyzes tables whose stats are missing or
	//     stale for queries this connection has run, so on a settled database
	//     it is effectively a no-op rather than a boot-time full re-analyze.
	// Both run best-effort on one dedicated connection (pragmas are
	// per-connection) and never fail startup — stale stats slow queries, they
	// do not corrupt anything.
	if conn, cerr := s.db.Conn(context.Background()); cerr == nil {
		_, _ = conn.ExecContext(context.Background(), `PRAGMA analysis_limit=1000`)
		_, _ = conn.ExecContext(context.Background(), `PRAGMA optimize`)
		_ = conn.Close()
	}
	return nil
}

// Delta is one 3s tick of aggregate activity, matching the legacy
// collector's snapshots row so the old Node dashboard can still read it.
type Delta struct {
	TS       int64
	Requests int64
	TokIn    int64
	TokOut   int64
	TokCache int64
	Cost     float64
	MS       int64
	Err      int64
	Failover int64
	Models   map[string]deltaModel
	Backends map[string]deltaBackend
	// Payload is compact per-tick stats JSON (latency percentiles, TTFT),
	// written into the legacy payload column. Read back by SeriesSince.
	Payload string
}

// TickStats is the compact payload blob for one tick.
type TickStats struct {
	Avg                      float64  `json:"avg"`
	P50                      float64  `json:"p50"`
	P95                      float64  `json:"p95"`
	P99                      float64  `json:"p99"`
	TTFT                     float64  `json:"ttft"`
	LatHist                  []uint64 `json:"lat_hist,omitempty"`
	TTFTN                    int64    `json:"ttft_n,omitempty"`
	DistributionsUnavailable bool     `json:"distributions_unavailable"`
	AggregateBucketMs        int64    `json:"aggregate_bucket_ms,omitempty"`
}

type deltaModel struct {
	Requests int64   `json:"requests"`
	TokIn    int64   `json:"tok_in"`
	TokOut   int64   `json:"tok_out"`
	TokCache int64   `json:"tok_cache"`
	Err      int64   `json:"err"`
	Failover int64   `json:"failover"`
	Cost     float64 `json:"cost"`
}

type deltaBackend struct {
	Requests int64   `json:"requests"`
	TokIn    int64   `json:"tok_in"`
	TokOut   int64   `json:"tok_out"`
	TokCache int64   `json:"tok_cache"`
	Err      int64   `json:"err"`
	Failover int64   `json:"failover"`
	Cost     float64 `json:"cost"`
}

func (s *Store) InsertDelta(d *Delta) error {
	// A non-finite cost would poison every SUM it touches (NaN compares
	// false against everything and never washes out). QueueRequest already
	// clamps request costs; the tick path needs the same guard.
	if math.IsNaN(d.Cost) || math.IsInf(d.Cost, 0) {
		d.Cost = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mj, err := json.Marshal(d.Models)
	if err != nil {
		return err
	}
	bj, err := json.Marshal(d.Backends)
	if err != nil {
		return err
	}
	// One transaction: the tick row, the running totals, and the minute rollup
	// must not disagree. LifetimeTotals previously SUMmed all of snapshots on
	// every dashboard poll (250ms at 1M ticks, holding s.mu so it blocked this
	// very writer for 241ms) — the totals row is the fix.
	tx, err := s.db.Begin()
	if err != nil {
		s.noteWrite(err)
		return err
	}
	defer tx.Rollback()

	if _, err = tx.Exec(
		`INSERT INTO snapshots
		   (ts, payload, d_requests, d_tok_in, d_tok_out, d_tok_cache, d_cost, d_ms, d_err, d_models, d_backends)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.TS, d.Payload, d.Requests, d.TokIn, d.TokOut, d.TokCache, d.Cost, d.MS, d.Err, string(mj), string(bj)); err != nil {
		s.noteWrite(err)
		return err
	}
	if _, err = tx.Exec(
		`UPDATE analytics_totals SET
		   cost=cost+?, requests=requests+?, tok_in=tok_in+?, tok_out=tok_out+?, tok_cache=tok_cache+?, err=err+?,
		   oldest_ts=CASE WHEN oldest_ts=0 OR ?<oldest_ts THEN ? ELSE oldest_ts END,
		   newest_ts=CASE WHEN ? > newest_ts THEN ? ELSE newest_ts END,
		   ticks=ticks+1
		 WHERE id=1`,
		d.Cost, d.Requests, d.TokIn, d.TokOut, d.TokCache, d.Err,
		d.TS, d.TS, d.TS, d.TS); err != nil {
		s.noteWrite(err)
		return err
	}
	if err = s.rollMinute(tx, d); err != nil {
		s.noteWrite(err)
		return err
	}
	err = tx.Commit()
	s.noteWrite(err)
	return err
}

// mergeMinuteAcc folds the stored bucket (a) together with the incoming tick
// (b), preserving histogram availability counters.
func (s *Store) InsertAlert(a AlertEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO alerts (ts, rule_id, name, severity, scope, metric, value, threshold, message)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.TS, a.RuleID, a.Name, a.Severity, a.Scope, a.Metric, a.Value, a.Threshold, a.Message)
	s.noteWrite(err)
	return err
}

func (s *Store) InsertKeyEvent(ts int64, backend, key, event string, status int, msg string) {
	// Never persist upstream error text or credentials, including legacy callers.
	event = safeKeyEvent(event)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO key_events (ts,backend,key,event,status,msg) VALUES (?,?,'',?,?,'')`, ts, backend, event, status)
	s.noteWrite(err)
}

func safeKeyEvent(event string) string {
	switch event {
	case "fail", "cooldown", "recover":
		return event
	}
	return "transition"
}

func (s *Store) QueueKeyEvent(ts int64, backend, key, event string, status int, msg string) {
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	if s.closed {
		s.keyEvDrop.Add(1)
		return
	}
	select {
	case s.keyEvCh <- keyEventReq{ts: ts, backend: backend, event: safeKeyEvent(event), status: status}:
	default:
		s.keyEvDrop.Add(1)
	}
}

func (s *Store) DroppedKeyEvents() uint64 { return s.keyEvDrop.Load() }

// RecentAlerts returns the last n alert events, newest first.
func (s *Store) RecentAlerts(n int) []AlertEvent {
	rows, err := s.db.Query(`SELECT ts, rule_id, name, severity, scope, metric, value, threshold, message
	                        FROM alerts ORDER BY ts DESC LIMIT ?`, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		var a AlertEvent
		if err := rows.Scan(&a.TS, &a.RuleID, &a.Name, &a.Severity, &a.Scope, &a.Metric, &a.Value, &a.Threshold, &a.Message); err == nil {
			out = append(out, a)
		}
	}
	// Best-effort read: a driver error mid-scan yields the partial list
	// rather than failing the dashboard panel.
	_ = rows.Err()
	return out
}

// AlertCollapseStats reports what a retroactive cooldown pass did (or, in
// dry-run mode, would do) to the alert history.
type AlertCollapseStats struct {
	Before  int // rows present before the pass
	After   int // rows remaining after the pass
	Dropped int // Before - After
	Rules   map[string]int
}

// CollapseAlertHistory applies the engine's cooldown to alerts already stored,
// so a flood recorded before the engine enforced a cooldown is retired.
//
// This is the retroactive counterpart to the rule fix. The stored rows were each
// individually correct — the condition really was over threshold — but a
// sustained condition used to re-fire on every eval, so a single hour-long
// incident could leave one row every 5s. Rerunning the history through the
// cooldown the engine now enforces keeps the first evaluation of each incident,
// then one row per window while it persists, and drops the per-eval duplicates.
//
// Rows are re-inserted with rewritten ids because `alerts.id` is AUTOINCREMENT,
// so an explicit id cannot be preserved. The original ts, rule, value, threshold
// and message are kept; keeping ts preserves the ordering the read path depends
// on, so a rewrite cannot reorder or duplicate the surviving rows.
//
// It runs in one committed transaction: the table is cleared and the surviving
// rows are re-inserted, so a failure part-way rolls back to the original history
// rather than leaving a half-pruned table. With dryRun set, the transaction is
// rolled back and only the counts are reported.
func (s *Store) CollapseAlertHistory(cooldownSec int, dryRun bool) (AlertCollapseStats, error) {
	if cooldownSec <= 0 {
		cooldownSec = DefaultAlertCooldownSec
	}
	stats := AlertCollapseStats{Rules: map[string]int{}}
	windowMs := int64(cooldownSec) * 1000

	s.mu.Lock()
	defer s.mu.Unlock()

	// Read in id order within each rule so "previous" is the incident's last
	// surviving row. Ordering by (rule_id, ts, id) is stable even for rows that
	// share a millisecond.
	rows, err := s.db.Query(`SELECT id, ts, rule_id, name, severity, scope, metric, value, threshold, message
	                         FROM alerts ORDER BY rule_id, ts, id`)
	if err != nil {
		return stats, err
	}
	type storedAlert struct {
		id int64
		ev AlertEvent
	}
	var (
		all  []storedAlert
		keep []storedAlert
		last = map[string]int64{}
	)
	for rows.Next() {
		var a storedAlert
		if err := rows.Scan(&a.id, &a.ev.TS, &a.ev.RuleID, &a.ev.Name, &a.ev.Severity,
			&a.ev.Scope, &a.ev.Metric, &a.ev.Value, &a.ev.Threshold, &a.ev.Message); err != nil {
			continue
		}
		all = append(all, a)
		if prev, ok := last[a.ev.RuleID]; ok && a.ev.TS-prev < windowMs {
			stats.Rules[a.ev.RuleID]++
			continue
		}
		last[a.ev.RuleID] = a.ev.TS
		keep = append(keep, a)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return stats, rowsErr
	}

	stats.Before = len(all)
	stats.After = len(keep)
	stats.Dropped = stats.Before - stats.After

	tx, err := s.db.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()

	// Clear and re-insert inside one transaction. Deleting the kept rows by id
	// while re-inserting them would collide on the AUTOINCREMENT primary key, so
	// the table is wiped and the survivors rebuilt from the snapshot read above.
	if _, err := tx.Exec(`DELETE FROM alerts`); err != nil {
		return stats, err
	}
	for _, a := range keep {
		if _, err := tx.Exec(
			`INSERT INTO alerts (ts, rule_id, name, severity, scope, metric, value, threshold, message)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ev.TS, a.ev.RuleID, a.ev.Name, a.ev.Severity, a.ev.Scope,
			a.ev.Metric, a.ev.Value, a.ev.Threshold, a.ev.Message); err != nil {
			return stats, err
		}
	}
	if dryRun {
		return stats, nil // deferred Rollback leaves history untouched.
	}
	return stats, tx.Commit()
}

// PruneFailedStats reports the outcome of PruneFailedRequests.
type PruneFailedStats struct {
	Total    int64
	Failed   int64
	OldestTs int64
	NewestTs int64
}

// PruneFailedRequests deletes every failed request row (ok=0: errors, cancels,
// upstream rejections) from the requests table. Failed outcomes are recorded
// metadata like any other request, so this exists only as an explicit owner
// maintenance action; nothing on the request path calls it.
//
// Derived state is intentionally left alone: request_hourly is an additive
// legacy-coverage tier for pruned history, and the snapshot tick tables are
// historical captures that cannot be re-derived from raw rows. Only
// detail-level reads (the Requests list and attributed analytics) reflect the
// deletion.
//
// One committed transaction; with dryRun nothing is written and only the
// counts are reported.
func (s *Store) PruneFailedRequests(dryRun bool) (PruneFailedStats, error) {
	stats := PruneFailedStats{}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(ok=0),0),
			COALESCE(MIN(CASE WHEN ok=0 THEN ts END),0), COALESCE(MAX(CASE WHEN ok=0 THEN ts END),0)
			FROM requests`).Scan(&stats.Total, &stats.Failed, &stats.OldestTs, &stats.NewestTs); err != nil {
		return stats, err
	}
	if dryRun || stats.Failed == 0 {
		return stats, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM requests WHERE ok=0`)
	if err != nil {
		return stats, err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		stats.Failed = n
	}
	return stats, tx.Commit()
}

// RecentKeyEvents returns the last n key events, newest first.
func (s *Store) RecentKeyEvents(n int) []KeyEvent {
	rows, err := s.db.Query(`SELECT ts, backend, key, event, status, msg FROM key_events ORDER BY ts DESC,id DESC LIMIT ?`, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []KeyEvent
	for rows.Next() {
		var e KeyEvent
		if err := rows.Scan(&e.TS, &e.Backend, &e.Key, &e.Event, &e.Status, &e.Msg); err == nil {
			e.Key, e.Msg = "", ""
			e.Event = safeKeyEvent(e.Event)
			out = append(out, e)
		}
	}
	// Best-effort read: same partial-list semantics as RecentAlerts.
	_ = rows.Err()
	return out
}

type TelemetryStatus struct {
	Queued           int64  `json:"queued"`
	DroppedRequests  uint64 `json:"dropped_requests"`
	DroppedKeyEvents uint64 `json:"dropped_key_events"`
	WriteErrors      uint64 `json:"write_errors"`
	LastWriteTS      int64  `json:"last_write_ts"`
}

func (s *Store) Telemetry() TelemetryStatus {
	return TelemetryStatus{s.queued.Load() + int64(len(s.keyEvCh)), s.requestDrop.Load(), s.keyEvDrop.Load(), s.writeErrors.Load(), s.lastWrite.Load()}
}

func (s *Store) noteWrite(err error) {
	if err != nil {
		s.writeErrors.Add(1)
	} else {
		s.lastWrite.Store(nowMs())
	}
}

func (s *Store) QueueRequest(rec ReqRecord) bool {
	rec = sanitizeRecord(rec)
	if rec.ID == "" {
		rec.ID = shortID()
	}
	if rec.TS <= 0 {
		rec.TS = nowMs()
	}
	if math.IsNaN(rec.LatMs) || math.IsInf(rec.LatMs, 0) || rec.LatMs < 0 {
		rec.LatMs = 0
	}
	if math.IsNaN(rec.TTFTMs) || math.IsInf(rec.TTFTMs, 0) || rec.TTFTMs < 0 {
		rec.TTFTMs = 0
	}
	if math.IsNaN(rec.Cost) || math.IsInf(rec.Cost, 0) || rec.Cost < 0 {
		rec.CostKnown = false
		rec.Cost = 0
	}
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	if s.closed {
		s.requestDrop.Add(1)
		return false
	}
	s.queued.Add(1)
	select {
	case s.requestCh <- rec:
		return true
	default:
		s.queued.Add(-1)
		s.requestDrop.Add(1)
		return false
	}
}

func (s *Store) runWriter() {
	defer close(s.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	batch := make([]ReqRecord, 0, 128)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.insertRequests(batch)
		s.noteWrite(err)
		if err != nil {
			s.requestDrop.Add(uint64(len(batch)))
		}
		s.queued.Add(-int64(len(batch)))
		batch = batch[:0]
		return err
	}
	requests, events := s.requestCh, s.keyEvCh
	for requests != nil || events != nil {
		select {
		case rec, ok := <-requests:
			if !ok {
				requests = nil
				continue
			}
			batch = append(batch, rec)
			if len(batch) == cap(batch) {
				flush()
			}
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			s.InsertKeyEvent(ev.ts, ev.backend, "", ev.event, ev.status, "")
		case result := <-s.flushCh:
			// Drain the FIFO prefix that existed at the barrier, not an unbounded
			// stream of newer traffic arriving while a backup is in progress.
			var flushErr error
			for range len(requests) {
				rec := <-requests
				batch = append(batch, rec)
				if len(batch) == cap(batch) {
					if err := flush(); err != nil {
						flushErr = err
					}
				}
			}
			for range len(events) {
				ev := <-events
				s.InsertKeyEvent(ev.ts, ev.backend, "", ev.event, ev.status, "")
			}
			if err := flush(); err != nil {
				flushErr = err
			}
			result <- flushErr
		case <-ticker.C:
			flush()
		}
	}
	flush()
}

func (s *Store) insertRequests(batch []ReqRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO requests(ts,request_id,model,upstream,backend,client_key_id,client_name,status,ok,outcome,usage_known,cost_known,tok_in,tok_out,tok_cache,cost,lat_ms,ttft_ms,record) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range batch {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if _, err = stmt.Exec(r.TS, r.ID, r.Model, r.Upstream, r.Backend, r.ClientKeyID, r.ClientName, r.Status, r.OK, r.Outcome, r.UsageKnown, r.CostKnown, r.TokIn, r.TokOut, r.TokCache, r.Cost, r.LatMs, r.TTFTMs, string(raw)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Deliberately NO summary-cache invalidation here. It used to clear the
	// memo on every batch (≤100 ms under load), which made the cache dead
	// exactly when it would help. The memo now lives purely on its TTL:
	// staleness is bounded by summaryMemoTTLMs, one dashboard poll cycle,
	// and the guard test asserts that bound.
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.queueMu.Lock()
		s.closed = true
		close(s.requestCh)
		close(s.keyEvCh)
		s.queueMu.Unlock()
		<-s.done
		err := s.db.Close()
		if s.writeErrors.Load() > 0 {
			s.closeErr = errors.New("analytics persistence reported write errors")
		}
		if err != nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

func (s *Store) Backup(path string) error {
	if err := s.flushPending(); err != nil {
		return err
	}
	// VACUUM INTO produces a consistent standalone SQLite database (including WAL).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Bind the destination as a parameter (SQLite supports VACUUM INTO ?)
	// rather than splicing it into the SQL text. The previous manual
	// single-quote escaping was correct but fragile: any future edit that
	// forgot a quote would turn a crafted path into SQL injection, and it is
	// a needless second escaping rule next to control_store.go's parameterized
	// form. Verified against modernc.org/sqlite (v1.56.0): the bound form
	// compiles and writes the destination file.
	_, err = s.db.Exec(`VACUUM INTO ?`, path)
	if err != nil {
		os.Remove(path)
	}
	return err
}

func (s *Store) flushPending() error {
	result := make(chan error, 1)
	s.queueMu.RLock()
	if s.closed {
		s.queueMu.RUnlock()
		return errors.New("analytics store closed")
	}
	s.flushCh <- result
	s.queueMu.RUnlock()
	return <-result
}
