package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	db          *sql.DB
	mu          sync.Mutex
	queueMu     sync.RWMutex
	requestCh   chan ReqRecord
	keyEvCh     chan keyEventReq
	flushCh     chan chan error
	done        chan struct{}
	closed      bool
	closeOnce   sync.Once
	closeErr    error
	keyEvDrop   atomic.Uint64
	requestDrop atomic.Uint64
	writeErrors atomic.Uint64
	lastWrite   atomic.Int64
	queued      atomic.Int64
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
	db, err := sql.Open("sqlite", path)
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
		`CREATE TABLE IF NOT EXISTS request_hourly (
		 hour INTEGER NOT NULL, model TEXT NOT NULL, upstream TEXT NOT NULL, backend TEXT NOT NULL,
		 client_key_id TEXT NOT NULL, client_name TEXT NOT NULL, status INTEGER NOT NULL, ok INTEGER NOT NULL,
		 requests INTEGER NOT NULL, errors INTEGER NOT NULL, tok_in INTEGER NOT NULL, tok_out INTEGER NOT NULL,
		 tok_cache INTEGER NOT NULL, cost REAL NOT NULL, unknown_usage INTEGER NOT NULL, unknown_cost INTEGER NOT NULL,
		 lat_sum REAL NOT NULL, ttft_sum REAL NOT NULL, ttft_n INTEGER NOT NULL,
		 lat_hist TEXT NOT NULL, ttft_hist TEXT NOT NULL,
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
	if _, err = tx.Exec(`UPDATE key_events SET key='',msg='' WHERE key!='' OR msg!=''`); err != nil {
		return err
	}
	return tx.Commit()
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
	Cost     float64 `json:"cost"`
}

type deltaBackend struct {
	Requests int64   `json:"requests"`
	TokIn    int64   `json:"tok_in"`
	TokOut   int64   `json:"tok_out"`
	TokCache int64   `json:"tok_cache"`
	Err      int64   `json:"err"`
	Cost     float64 `json:"cost"`
}

func (s *Store) InsertDelta(d *Delta) error {
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
	_, err = s.db.Exec(
		`INSERT INTO snapshots
		   (ts, payload, d_requests, d_tok_in, d_tok_out, d_tok_cache, d_cost, d_ms, d_err, d_models, d_backends)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.TS, d.Payload, d.Requests, d.TokIn, d.TokOut, d.TokCache, d.Cost, d.MS, d.Err, string(mj), string(bj))
	s.noteWrite(err)
	return err
}

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

// SeriesSince returns snapshots in [sinceMs, +∞) oldest-first. When the
// result exceeds maxPoints it is downsampled in SQL into equal-width
// buckets. Per-model and per-backend JSON is folded in only from ticks
// that had traffic, so a year of idle 3s rows does not have to be parsed.
// ctx cancels the scan so a newer window can take the DB lock.
func (s *Store) SeriesSince(ctx context.Context, sinceMs int64, maxPoints int) ([]SeriesRow, error) {
	if maxPoints < 1 {
		maxPoints = 500
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	var minTs, maxTs int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM snapshots WHERE ts >= ?`,
		sinceMs).Scan(&n, &minTs, &maxTs); err != nil {
		return nil, err
	}
	if n == 0 {
		return []SeriesRow{}, nil
	}
	if n <= maxPoints {
		return s.seriesScan(ctx, sinceMs)
	}
	return s.seriesBuckets(ctx, sinceMs, minTs, maxTs, maxPoints)
}

// seriesScan reads every tick in the window. Caller holds s.mu.
func (s *Store) seriesScan(ctx context.Context, sinceMs int64) ([]SeriesRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT ts, payload, d_requests, d_tok_in, d_tok_out, d_tok_cache, d_cost, d_ms, d_err, d_models, d_backends
	                        FROM snapshots WHERE ts >= ? ORDER BY ts ASC`, sinceMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []SeriesRow
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var r SeriesRow
		var payload, mj, bj string
		if err := rows.Scan(&r.TS, &payload, &r.Requests, &r.TokIn, &r.TokOut, &r.TokCache, &r.Cost, &r.MS, &r.Err, &mj, &bj); err != nil {
			return nil, err
		}
		r.DistributionsUnavailable = r.Requests > 0
		if payload != "" {
			var ts TickStats
			if json.Unmarshal([]byte(payload), &ts) == nil {
				r.LatAvg, r.LatP50, r.LatP95, r.LatP99, r.TTFTAvg = ts.Avg, ts.P50, ts.P95, ts.P99, ts.TTFT
				r.DistributionsUnavailable = ts.DistributionsUnavailable || len(ts.LatHist) == 0
				r.DistributionsApproximate = len(ts.LatHist) > 0
				r.AggregateBucketMs = ts.AggregateBucketMs
			}
		}
		var m map[string]deltaModel
		var b map[string]deltaBackend
		_ = json.Unmarshal([]byte(mj), &m)
		_ = json.Unmarshal([]byte(bj), &b)
		r.Models = m
		r.Backends = b
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if all == nil {
		return []SeriesRow{}, nil
	}
	return all, nil
}

// seriesBuckets aggregates a long window in SQL. Caller holds s.mu.
func (s *Store) seriesBuckets(ctx context.Context, sinceMs, minTs, maxTs int64, maxPoints int) ([]SeriesRow, error) {
	span := maxTs - minTs
	if span < 1 {
		span = 1
	}
	// +1 so the last tick stays inside the final bucket (integer division
	// otherwise emits maxPoints+1 groups).
	bucket := span/int64(maxPoints) + 1
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ts - ?) / ?, MIN(ts),
		       SUM(d_requests), SUM(d_tok_in), SUM(d_tok_out), SUM(d_tok_cache),
		       SUM(d_cost), SUM(d_ms), SUM(d_err)
		FROM snapshots
		WHERE ts >= ?
		GROUP BY (ts - ?) / ?
		ORDER BY 1`, minTs, bucket, sinceMs, minTs, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byIdx := map[int64]*SeriesRow{}
	var order []int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var idx int64
		r := &SeriesRow{Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
		if err := rows.Scan(&idx, &r.TS, &r.Requests, &r.TokIn, &r.TokOut, &r.TokCache, &r.Cost, &r.MS, &r.Err); err != nil {
			return nil, err
		}
		byIdx[idx] = r
		order = append(order, idx)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	arows, err := s.db.QueryContext(ctx, `
		SELECT ts, payload, d_requests, d_models, d_backends
		FROM snapshots
		WHERE ts >= ? AND (d_requests > 0 OR d_cost != 0 OR d_err > 0)`, sinceMs)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	latW := map[int64]float64{}
	latSum := map[int64]float64{}
	hists := map[int64][len(latBuckets) + 1]uint64{}
	missing := map[int64]bool{}
	ttftW := map[int64]float64{}
	ttftSum := map[int64]float64{}
	for arows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var ts, req int64
		var payload, mj, bj string
		if err := arows.Scan(&ts, &payload, &req, &mj, &bj); err != nil {
			return nil, err
		}
		b := byIdx[(ts-minTs)/bucket]
		if b == nil {
			continue
		}
		var m map[string]deltaModel
		var backends map[string]deltaBackend
		_ = json.Unmarshal([]byte(mj), &m)
		_ = json.Unmarshal([]byte(bj), &backends)
		for k, v := range m {
			got := b.Models[k]
			got.Requests += v.Requests
			got.TokIn += v.TokIn
			got.TokOut += v.TokOut
			got.TokCache += v.TokCache
			got.Cost += v.Cost
			b.Models[k] = got
		}
		for k, v := range backends {
			got := b.Backends[k]
			got.Requests += v.Requests
			got.TokIn += v.TokIn
			got.TokOut += v.TokOut
			got.TokCache += v.TokCache
			got.Err += v.Err
			got.Cost += v.Cost
			b.Backends[k] = got
		}
		if req > 0 && payload == "" {
			missing[(ts-minTs)/bucket] = true
		}
		if req > 0 && payload != "" {
			var stats TickStats
			if json.Unmarshal([]byte(payload), &stats) == nil {
				w := float64(req)
				latW[(ts-minTs)/bucket] += w
				latSum[(ts-minTs)/bucket] += stats.Avg * w
				idx := (ts - minTs) / bucket
				if len(stats.LatHist) == len(latBuckets)+1 {
					h := hists[idx]
					for i, v := range stats.LatHist {
						h[i] += v
					}
					hists[idx] = h
				} else {
					missing[idx] = true
				}
				ttftSum[idx] += stats.TTFT * float64(stats.TTFTN)
				ttftW[idx] += float64(stats.TTFTN)
			} else {
				missing[(ts-minTs)/bucket] = true
			}
		}
	}
	if err := arows.Err(); err != nil {
		return nil, err
	}
	out := make([]SeriesRow, 0, len(order))
	for _, idx := range order {
		b := byIdx[idx]
		b.DistributionsUnavailable = missing[idx]
		b.DistributionsApproximate = !missing[idx]
		b.AggregateBucketMs = bucket
		if latW[idx] > 0 {
			b.LatAvg = latSum[idx] / latW[idx]
			if !missing[idx] {
				h := hists[idx]
				b.LatP50 = histogramPercentile(h, 50)
				b.LatP95 = histogramPercentile(h, 95)
				b.LatP99 = histogramPercentile(h, 99)
			}
			if ttftW[idx] > 0 {
				b.TTFTAvg = ttftSum[idx] / ttftW[idx]
			}
		}
		out = append(out, *b)
	}
	return out, nil
}

// SeriesRow is one chartable point.
type SeriesRow struct {
	TS                       int64                   `json:"ts"`
	Requests                 int64                   `json:"d_requests"`
	TokIn                    int64                   `json:"d_tok_in"`
	TokOut                   int64                   `json:"d_tok_out"`
	TokCache                 int64                   `json:"d_tok_cache"`
	Cost                     float64                 `json:"d_cost"`
	MS                       int64                   `json:"d_ms"`
	Err                      int64                   `json:"d_err"`
	Models                   map[string]deltaModel   `json:"d_models,omitempty"`
	Backends                 map[string]deltaBackend `json:"d_backends,omitempty"`
	LatAvg                   float64                 `json:"lat_avg_ms,omitempty"`
	LatP50                   float64                 `json:"lat_p50_ms,omitempty"`
	LatP95                   float64                 `json:"lat_p95_ms,omitempty"`
	LatP99                   float64                 `json:"lat_p99_ms,omitempty"`
	TTFTAvg                  float64                 `json:"ttft_avg_ms,omitempty"`
	DistributionsUnavailable bool                    `json:"distributions_unavailable"`
	DistributionsApproximate bool                    `json:"distributions_approximate"`
	AggregateBucketMs        int64                   `json:"aggregate_bucket_ms,omitempty"`
}

func (s *Store) Range() (oldest, newest int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT COALESCE(MIN(ts),0), COALESCE(MAX(ts),0), COUNT(*) FROM snapshots`)
	_ = row.Scan(&oldest, &newest, &count)
	return
}

// LifetimeTotals sums the retained window. This is the dashboard's
// headline lifetime cost — it survives verloc restarts.
func (s *Store) LifetimeTotals() (cost float64, requests, tokIn, tokOut, tokCache, errN int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(d_cost),0), COALESCE(SUM(d_requests),0), COALESCE(SUM(d_tok_in),0),
		        COALESCE(SUM(d_tok_out),0), COALESCE(SUM(d_tok_cache),0), COALESCE(SUM(d_err),0)
		 FROM snapshots`)
	_ = row.Scan(&cost, &requests, &tokIn, &tokOut, &tokCache, &errN)
	return
}

// RecentAlerts returns the last n alert events, newest first.
func (s *Store) RecentAlerts(n int) []AlertEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	return out
}

// RecentKeyEvents returns the last n key events, newest first.
func (s *Store) RecentKeyEvents(n int) []KeyEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	return tx.Commit()
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
	_, err = s.db.Exec(`VACUUM INTO '` + strings.ReplaceAll(path, `'`, `''`) + `'`)
	if err != nil {
		os.Remove(path)
	}
	return err
}

type RequestSummary struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	TokIn        uint64  `json:"tok_in"`
	TokOut       uint64  `json:"tok_out"`
	TokCache     uint64  `json:"tok_cache"`
	Cost         float64 `json:"cost"`
	UnknownUsage int64   `json:"unknown_usage"`
	UnknownCost  int64   `json:"unknown_cost"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	AvgTTFTMs    float64 `json:"avg_ttft_ms"`
}

type UsageGroup struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	RequestSummary
}

type StatusGroup struct {
	Status   int   `json:"status"`
	Requests int64 `json:"requests"`
}
type RequestFilter struct {
	Since, Until                                   int64
	Model, Backend, ClientKeyID, Status, Q, Cursor string
	Limit                                          int
}
type RequestPage struct {
	Requests   []ReqRecord    `json:"requests"`
	Total      int64          `json:"total"`
	NextCursor string         `json:"next_cursor,omitempty"`
	Summary    RequestSummary `json:"summary"`
}

func (f RequestFilter) where(rollup bool) (string, []any) {
	var terms []string
	var args []any
	col := "ts"
	if rollup {
		col = "hour"
	}
	if f.Since > 0 {
		start := f.Since
		if rollup {
			start = ((start + 3599999) / 3600000) * 3600000
		}
		terms = append(terms, col+">=?")
		args = append(args, start)
	}
	if f.Until > 0 {
		if rollup {
			terms = append(terms, "hour+3600000<=?")
		} else {
			terms = append(terms, "ts<=?")
		}
		args = append(args, f.Until)
	}
	for _, v := range []struct{ col, val string }{{"model", f.Model}, {"backend", f.Backend}, {"client_key_id", f.ClientKeyID}} {
		if v.val != "" {
			terms = append(terms, v.col+"=?")
			args = append(args, v.val)
		}
	}
	switch f.Status {
	case "ok":
		terms = append(terms, "ok=1")
	case "error":
		terms = append(terms, "ok=0")
	case "":
	default:
		terms = append(terms, "status=?")
		args = append(args, f.Status)
	}
	if f.Q != "" {
		search := `instr(lower(model),?)>0 OR instr(lower(upstream),?)>0 OR instr(lower(backend),?)>0 OR instr(lower(client_name),?)>0 OR instr(lower(client_key_id),?)>0`
		for range 5 {
			args = append(args, strings.ToLower(f.Q))
		}
		if !rollup {
			search += ` OR instr(lower(request_id),?)>0`
			args = append(args, strings.ToLower(f.Q))
		}
		terms = append(terms, "("+search+")")
	}
	if len(terms) == 0 {
		return "1=1", args
	}
	return strings.Join(terms, " AND "), args
}

func encodeCursor(ts, seq int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", ts, seq)))
}
func decodeCursor(v string) (int64, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return 0, 0, errors.New("invalid cursor")
	}
	p := strings.Split(string(raw), ":")
	if len(p) != 2 {
		return 0, 0, errors.New("invalid cursor")
	}
	ts, e1 := strconv.ParseInt(p[0], 10, 64)
	seq, e2 := strconv.ParseInt(p[1], 10, 64)
	if e1 != nil || e2 != nil || ts < 0 || seq <= 0 {
		return 0, 0, errors.New("invalid cursor")
	}
	return ts, seq, nil
}

const summarySQL = `COUNT(*),COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(tok_in),0),COALESCE(SUM(tok_out),0),COALESCE(SUM(tok_cache),0),
 COALESCE(SUM(cost),0),COALESCE(SUM(CASE WHEN usage_known=0 THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN cost_known=0 THEN 1 ELSE 0 END),0),COALESCE(AVG(lat_ms),0),
 COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0)`

func scanSummary(row interface{ Scan(...any) error }, s *RequestSummary) error {
	return row.Scan(&s.Requests, &s.Errors, &s.TokIn, &s.TokOut, &s.TokCache, &s.Cost, &s.UnknownUsage, &s.UnknownCost, &s.AvgLatencyMs, &s.AvgTTFTMs)
}

func (s *Store) Requests(ctx context.Context, f RequestFilter) (RequestPage, error) {
	out := RequestPage{Requests: []ReqRecord{}}
	where, args := f.where(false)
	// One read transaction makes totals and the page a consistent snapshot.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if err = scanSummary(tx.QueryRowContext(ctx, `SELECT `+summarySQL+` FROM requests WHERE `+where, args...), &out.Summary); err != nil {
		return out, err
	}
	out.Total = out.Summary.Requests
	if f.Cursor != "" {
		ts, seq, err := decodeCursor(f.Cursor)
		if err != nil {
			return out, err
		}
		where += ` AND (ts<? OR (ts=? AND seq<?))`
		args = append(args, ts, ts, seq)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, `SELECT ts,seq,record FROM requests WHERE `+where+` ORDER BY ts DESC,seq DESC LIMIT ?`, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var lastTS, lastSeq int64
	for rows.Next() {
		var ts, seq int64
		var raw string
		if err = rows.Scan(&ts, &seq, &raw); err != nil {
			return out, err
		}
		if len(out.Requests) == limit {
			out.NextCursor = encodeCursor(lastTS, lastSeq)
			break
		}
		var r ReqRecord
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			return out, err
		}
		out.Requests = append(out.Requests, sanitizeRecord(r))
		lastTS, lastSeq = ts, seq
	}
	return out, rows.Err()
}

type usageAccumulator struct {
	RequestSummary
	latSum, ttftSum   float64
	ttftN             int64
	latHist, ttftHist [len(latBuckets) + 1]uint64
}

func (a *usageAccumulator) addRecord(r ReqRecord) {
	a.Requests++
	if !r.OK {
		a.Errors++
	}
	a.TokIn += r.TokIn
	a.TokOut += r.TokOut
	a.TokCache += r.TokCache
	a.Cost += r.Cost
	if !r.UsageKnown {
		a.UnknownUsage++
	}
	if !r.CostKnown {
		a.UnknownCost++
	}
	a.latSum += r.LatMs
	a.latHist[bucketOf(r.LatMs)]++
	if r.TTFTMs > 0 {
		a.ttftSum += r.TTFTMs
		a.ttftN++
		a.ttftHist[bucketOf(r.TTFTMs)]++
	}
}
func (a *usageAccumulator) merge(b *usageAccumulator) {
	a.Requests += b.Requests
	a.Errors += b.Errors
	a.TokIn += b.TokIn
	a.TokOut += b.TokOut
	a.TokCache += b.TokCache
	a.Cost += b.Cost
	a.UnknownUsage += b.UnknownUsage
	a.UnknownCost += b.UnknownCost
	a.latSum += b.latSum
	a.ttftSum += b.ttftSum
	a.ttftN += b.ttftN
	for i := range a.latHist {
		a.latHist[i] += b.latHist[i]
		a.ttftHist[i] += b.ttftHist[i]
	}
}
func (a *usageAccumulator) summary() RequestSummary {
	s := a.RequestSummary
	if s.Requests > 0 {
		s.AvgLatencyMs = a.latSum / float64(s.Requests)
	}
	if a.ttftN > 0 {
		s.AvgTTFTMs = a.ttftSum / float64(a.ttftN)
	}
	return s
}

type Distribution struct {
	P50         float64   `json:"p50"`
	P95         float64   `json:"p95"`
	P99         float64   `json:"p99"`
	Buckets     []uint64  `json:"buckets,omitempty"`
	Bounds      []float64 `json:"bounds,omitempty"`
	Approximate bool      `json:"approximate"`
	Overflow    uint64    `json:"overflow"`
}

func histogramPercentile(h [len(latBuckets) + 1]uint64, p int) float64 {
	var n uint64
	for _, v := range h {
		n += v
	}
	if n == 0 {
		return 0
	}
	target := (n*uint64(p) + 99) / 100
	var cumulative uint64
	for i, v := range h {
		cumulative += v
		if cumulative >= target {
			if i == len(latBuckets) {
				return latBuckets[len(latBuckets)-1]
			}
			return latBuckets[i]
		}
	}
	return 0
}
func distribution(h [len(latBuckets) + 1]uint64) Distribution {
	return Distribution{P50: histogramPercentile(h, 50), P95: histogramPercentile(h, 95), P99: histogramPercentile(h, 99), Buckets: append([]uint64(nil), h[:]...), Bounds: append([]float64(nil), latBuckets[:]...), Approximate: true, Overflow: h[len(latBuckets)]}
}

type AnalyticsCoverage struct {
	OldestTS                       int64           `json:"oldest_ts"`
	NewestTS                       int64           `json:"newest_ts"`
	LegacyAggregatesOnly           bool            `json:"legacy_aggregates_only"`
	DetailOldestTS                 int64           `json:"detail_oldest_ts"`
	DetailNewestTS                 int64           `json:"detail_newest_ts"`
	AggregateOnlyRequests          int64           `json:"aggregate_only_requests"`
	AggregateBucketMs              int64           `json:"aggregate_bucket_ms"`
	PartialBucketsExcluded         bool            `json:"partial_buckets_excluded"`
	LegacyDistributionsUnavailable bool            `json:"legacy_distributions_unavailable"`
	LegacySummary                  *RequestSummary `json:"legacy_summary,omitempty"`
}
type AnalyticsResult struct {
	Summary   RequestSummary    `json:"summary"`
	Models    []UsageGroup      `json:"models"`
	Providers []UsageGroup      `json:"providers"`
	Clients   []UsageGroup      `json:"clients"`
	Status    []StatusGroup     `json:"status"`
	Latency   Distribution      `json:"latency"`
	TTFT      Distribution      `json:"ttft"`
	Coverage  AnalyticsCoverage `json:"coverage"`
}

func (s *Store) Analytics(ctx context.Context, f RequestFilter) (AnalyticsResult, error) {
	out := AnalyticsResult{Models: []UsageGroup{}, Providers: []UsageGroup{}, Clients: []UsageGroup{}, Status: []StatusGroup{}, Coverage: AnalyticsCoverage{AggregateBucketMs: 3600000, PartialBucketsExcluded: true, LegacyDistributionsUnavailable: true}}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	total := &usageAccumulator{}
	models, providers, clients := map[string]*usageAccumulator{}, map[string]*usageAccumulator{}, map[string]*usageAccumulator{}
	names := map[string]string{}
	nameTS := map[string]int64{}
	statuses := map[int]int64{}
	add := func(model, provider, client, name string, status int, ts int64, a *usageAccumulator) {
		total.merge(a)
		for _, v := range []struct {
			m map[string]*usageAccumulator
			k string
		}{{models, model}, {providers, provider}, {clients, client}} {
			x := v.m[v.k]
			if x == nil {
				x = &usageAccumulator{}
				v.m[v.k] = x
			}
			x.merge(a)
		}
		if ts > nameTS[client] || (ts == nameTS[client] && name > names[client]) {
			names[client] = name
			nameTS[client] = ts
		}
		statuses[status] += a.Requests
	}
	cover := func(ts int64) {
		if out.Coverage.OldestTS == 0 || ts < out.Coverage.OldestTS {
			out.Coverage.OldestTS = ts
		}
		if ts > out.Coverage.NewestTS {
			out.Coverage.NewestTS = ts
		}
	}
	where, args := f.where(false)
	rows, err := tx.QueryContext(ctx, `SELECT record FROM requests WHERE `+where, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return out, err
		}
		var r ReqRecord
		if err = json.Unmarshal([]byte(raw), &r); err != nil {
			rows.Close()
			return out, err
		}
		a := &usageAccumulator{}
		a.addRecord(r)
		add(r.Model, r.Backend, r.ClientKeyID, r.ClientName, r.Status, r.TS, a)
		cover(r.TS)
		if out.Coverage.DetailOldestTS == 0 || r.TS < out.Coverage.DetailOldestTS {
			out.Coverage.DetailOldestTS = r.TS
		}
		if r.TS > out.Coverage.DetailNewestTS {
			out.Coverage.DetailNewestTS = r.TS
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	where, args = f.where(true)
	rows, err = tx.QueryContext(ctx, `SELECT hour,model,backend,client_key_id,client_name,status,requests,errors,tok_in,tok_out,tok_cache,cost,unknown_usage,unknown_cost,lat_sum,ttft_sum,ttft_n,lat_hist,ttft_hist FROM request_hourly WHERE `+where, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var hour int64
		var model, provider, client, name, lh, th string
		var status int
		a := &usageAccumulator{}
		if err = rows.Scan(&hour, &model, &provider, &client, &name, &status, &a.Requests, &a.Errors, &a.TokIn, &a.TokOut, &a.TokCache, &a.Cost, &a.UnknownUsage, &a.UnknownCost, &a.latSum, &a.ttftSum, &a.ttftN, &lh, &th); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal([]byte(lh), &a.latHist); err != nil {
			rows.Close()
			return out, err
		}
		if err = json.Unmarshal([]byte(th), &a.ttftHist); err != nil {
			rows.Close()
			return out, err
		}
		add(model, provider, client, name, status, hour, a)
		cover(hour)
		out.Coverage.AggregateOnlyRequests += a.Requests
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Summary = total.summary()
	out.Latency = distribution(total.latHist)
	out.TTFT = distribution(total.ttftHist)
	groups := func(m map[string]*usageAccumulator, client bool) []UsageGroup {
		list := make([]UsageGroup, 0, len(m))
		for id, a := range m {
			name := id
			if client {
				name = names[id]
				if name == "" {
					name = "Unattributed"
				}
			}
			list = append(list, UsageGroup{id, name, a.summary()})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Requests == list[j].Requests {
				return list[i].ID < list[j].ID
			}
			return list[i].Requests > list[j].Requests
		})
		return list
	}
	out.Models = groups(models, false)
	out.Providers = groups(providers, false)
	out.Clients = groups(clients, true)
	for status, n := range statuses {
		out.Status = append(out.Status, StatusGroup{status, n})
	}
	sort.Slice(out.Status, func(i, j int) bool { return out.Status[i].Status < out.Status[j].Status })
	if f.Model == "" && f.Backend == "" && f.ClientKeyID == "" && f.Status == "" && f.Q == "" {
		start := ((f.Since + 3599999) / 3600000) * 3600000
		until := int64(math.MaxInt64)
		if f.Until > 0 {
			until = f.Until
		}
		legacy := &RequestSummary{}
		var oldest, newest int64
		err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0),COALESCE(SUM(errors),0),COALESCE(SUM(tok_in),0),COALESCE(SUM(tok_out),0),COALESCE(SUM(tok_cache),0),COALESCE(SUM(cost),0),COALESCE(MIN(hour),0),COALESCE(MAX(hour),0) FROM legacy_hourly WHERE hour>=? AND hour+3600000<=?`, start, until).Scan(&legacy.Requests, &legacy.Errors, &legacy.TokIn, &legacy.TokOut, &legacy.TokCache, &legacy.Cost, &oldest, &newest)
		if err != nil {
			return out, err
		}
		if legacy.Requests > 0 {
			legacy.UnknownUsage = legacy.Requests
			legacy.UnknownCost = legacy.Requests
			out.Coverage.LegacySummary = legacy
			out.Coverage.LegacyAggregatesOnly = out.Summary.Requests == 0
			cover(oldest)
			cover(newest)
		}
	}
	return out, nil
}

func requestFilter(values url.Values) (RequestFilter, error) {
	f := RequestFilter{Model: values.Get("model"), Backend: values.Get("backend"), ClientKeyID: values.Get("client_key_id"), Status: values.Get("status"), Q: values.Get("q"), Cursor: values.Get("cursor"), Limit: 100}
	for _, v := range []struct {
		k string
		p *int64
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if raw := values.Get(v.k); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 {
				return f, errors.New("invalid time filter")
			}
			*v.p = n
		}
	}
	if f.Until > 0 && f.Until < f.Since {
		return f, errors.New("until precedes since")
	}
	if f.Status != "" && f.Status != "ok" && f.Status != "error" {
		n, err := strconv.Atoi(f.Status)
		if err != nil || n < 0 || n > 599 {
			return f, errors.New("invalid status")
		}
	}
	if raw := values.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			return f, errors.New("invalid limit")
		}
		f.Limit = n
	}
	if f.Cursor != "" {
		if _, _, err := decodeCursor(f.Cursor); err != nil {
			return f, err
		}
	}
	return f, nil
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
