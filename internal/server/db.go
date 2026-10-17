package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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

// minuteAcc accumulates one minute of ticks. Both the live per-tick rollup and
// the one-time backfill fold through this type, so the two paths cannot drift.
type minuteAcc struct {
	req, errN, tin, tout, tcache, ms int64
	cost                             float64
	latW, latSum, ttftW, ttftSum     float64
	hist, ttftHist                   [len(latBuckets) + 1]uint64
	histN, missingN                  int64
	models                           map[string]deltaModel
	backends                         map[string]deltaBackend
}

func newMinuteAcc() *minuteAcc {
	return &minuteAcc{models: map[string]deltaModel{}, backends: map[string]deltaBackend{}}
}

// addDelta folds one tick. A tick that carried traffic but no usable histogram
// marks the minute unavailable: snapshot percentiles are averages over the tick
// and must never be folded into a window distribution, matching the raw path.
func (a *minuteAcc) addDelta(d *Delta) {
	a.req += d.Requests
	a.errN += d.Err
	a.tin += d.TokIn
	a.tout += d.TokOut
	a.tcache += d.TokCache
	a.cost += d.Cost
	a.ms += d.MS
	if d.Requests > 0 {
		usable := false
		if d.Payload != "" {
			var st TickStats
			if json.Unmarshal([]byte(d.Payload), &st) == nil {
				a.latW += float64(d.Requests)
				a.latSum += st.Avg * float64(d.Requests)
				a.ttftW += float64(st.TTFTN)
				a.ttftSum += st.TTFT * float64(st.TTFTN)
				if len(st.LatHist) == len(latBuckets)+1 {
					for i, v := range st.LatHist {
						a.hist[i] += v
					}
					usable = true
				}
			}
		}
		if usable {
			a.histN++
		} else {
			a.missingN++
		}
	}
	for k, v := range d.Models {
		g := a.models[k]
		g.Requests += v.Requests
		g.TokIn += v.TokIn
		g.TokOut += v.TokOut
		g.TokCache += v.TokCache
		g.Cost += v.Cost
		a.models[k] = g
	}
	for k, v := range d.Backends {
		g := a.backends[k]
		g.Requests += v.Requests
		g.TokIn += v.TokIn
		g.TokOut += v.TokOut
		g.TokCache += v.TokCache
		g.Err += v.Err
		g.Cost += v.Cost
		a.backends[k] = g
	}
}

func (a *minuteAcc) write(tx *sql.Tx, minute int64) error {
	hj, _ := json.Marshal(a.hist[:])
	thj, _ := json.Marshal(a.ttftHist[:])
	mj, _ := json.Marshal(a.models)
	bj, _ := json.Marshal(a.backends)
	_, err := tx.Exec(`INSERT INTO snapshot_minute
	   (minute,requests,err,tok_in,tok_out,tok_cache,cost,ms,lat_w,lat_sum,ttft_w,ttft_sum,hist,ttft_hist,models,backends,hist_n,missing_n)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	   ON CONFLICT(minute) DO UPDATE SET
	     requests=excluded.requests, err=excluded.err, tok_in=excluded.tok_in, tok_out=excluded.tok_out,
	     tok_cache=excluded.tok_cache, cost=excluded.cost, ms=excluded.ms,
	     lat_w=excluded.lat_w, lat_sum=excluded.lat_sum, ttft_w=excluded.ttft_w, ttft_sum=excluded.ttft_sum,
	     hist=excluded.hist, ttft_hist=excluded.ttft_hist, models=excluded.models, backends=excluded.backends,
	     hist_n=excluded.hist_n, missing_n=excluded.missing_n`,
		minute, a.req, a.errN, a.tin, a.tout, a.tcache, a.cost, a.ms,
		a.latW, a.latSum, a.ttftW, a.ttftSum, string(hj), string(thj), string(mj), string(bj),
		a.histN, a.missingN)
	return err
}

// RebuildRollups rebuilds snapshot_minute from the raw tick table. This is the
// migration for history that predates the rollup (and the repair path if the
// rollup is ever lost). It streams ticks in time order and writes once per
// minute, so it is O(minutes) writes rather than one read-merge-write per tick.
func (s *Store) RebuildRollups(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT ts,payload,d_requests,d_tok_in,d_tok_out,d_tok_cache,d_cost,d_ms,d_err,d_models,d_backends
	                                     FROM snapshots ORDER BY ts ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM snapshot_minute`); err != nil {
		return 0, err
	}

	var (
		curMinute int64 = -1
		acc             = newMinuteAcc()
		written   int
	)
	flush := func() error {
		if curMinute < 0 {
			return nil
		}
		if err := acc.write(tx, curMinute); err != nil {
			return err
		}
		written++
		acc = newMinuteAcc()
		return nil
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		var d Delta
		var payload, mj, bj string
		if err := rows.Scan(&d.TS, &payload, &d.Requests, &d.TokIn, &d.TokOut, &d.TokCache,
			&d.Cost, &d.MS, &d.Err, &mj, &bj); err != nil {
			return 0, err
		}
		d.Payload = payload
		_ = json.Unmarshal([]byte(mj), &d.Models)
		_ = json.Unmarshal([]byte(bj), &d.Backends)
		if d.Models == nil {
			d.Models = map[string]deltaModel{}
		}
		if d.Backends == nil {
			d.Backends = map[string]deltaBackend{}
		}
		minute := d.TS / 60000 * 60000
		if minute != curMinute {
			if err := flush(); err != nil {
				return 0, err
			}
			curMinute = minute
		}
		acc.addDelta(&d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return written, nil
}

// EnsureRollupsBackfilled rebuilds the minute rollup when the analytics store
// has raw ticks but no rollup (i.e. history that predates it). Runs in the
// background so startup is never blocked by a large rebuild.
func (s *Store) EnsureRollupsBackfilled(ctx context.Context) {
	s.mu.Lock()
	var minutes, ticks int64
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshot_minute`).Scan(&minutes)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM snapshots`).Scan(&ticks)
	s.mu.Unlock()
	if minutes > 0 || ticks == 0 {
		return
	}
	go func() {
		start := time.Now()
		n, err := s.RebuildRollups(ctx)
		if err != nil {
			log.Printf("[analytics] rollup backfill failed: %v", err)
			return
		}
		log.Printf("[analytics] rollup backfill complete: %d minutes in %v", n, time.Since(start).Round(time.Millisecond))
	}()
}

// rollMinute folds one tick into its minute bucket. Guarded by the caller's
// transaction; the histogram and dimension maps are merged in Go because the
// dimension fold was measured to be ~2.8x slower in SQL (json_each over ~1M
// ticks produces millions of intermediate rows) and the per-minute merge is
// O(1) here instead of a scan later.
func (s *Store) rollMinute(tx *sql.Tx, d *Delta) error {
	minute := d.TS / 60000 * 60000
	var (
		latW, latSum, ttftW, ttftSum float64
		hist, ttftHist               [len(latBuckets) + 1]uint64
		models, backends             = map[string]deltaModel{}, map[string]deltaBackend{}
	)
	// A tick that carried traffic but no usable histogram must mark its bucket
	// unavailable: snapshot percentiles are not averages and cannot be folded
	// into a window distribution (mirrors seriesBuckets' `missing` tracking).
	var histN, missingN int64
	if d.Requests > 0 {
		usable := false
		if d.Payload != "" {
			var st TickStats
			if json.Unmarshal([]byte(d.Payload), &st) == nil {
				latW = float64(d.Requests)
				latSum = st.Avg * float64(d.Requests)
				ttftW = float64(st.TTFTN)
				ttftSum = st.TTFT * float64(st.TTFTN)
				if len(st.LatHist) == len(latBuckets)+1 {
					copy(hist[:], st.LatHist)
					usable = true
				}
			}
		}
		if usable {
			histN = 1
		} else {
			missingN = 1
		}
	}
	for k, v := range d.Models {
		models[k] = v
	}
	for k, v := range d.Backends {
		backends[k] = v
	}

	// Read the existing bucket, merge, write back in the same transaction.
	var (
		pReq, pErr, pIn, pOut, pCache, pMS int64
		pCost                              float64
		pLatW, pLatSum, pTTFTW, pTTFTSum   float64
		pHist, pTTFTHist, pModels, pBack   string
		pHistN, pMissingN                  int64
	)
	err := tx.QueryRow(`SELECT requests,err,tok_in,tok_out,tok_cache,cost,ms,lat_w,lat_sum,ttft_w,ttft_sum,hist,ttft_hist,models,backends,hist_n,missing_n
	                    FROM snapshot_minute WHERE minute=?`, minute).Scan(
		&pReq, &pErr, &pIn, &pOut, &pCache, &pCost, &pMS, &pLatW, &pLatSum, &pTTFTW, &pTTFTSum,
		&pHist, &pTTFTHist, &pModels, &pBack, &pHistN, &pMissingN)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var h, th [len(latBuckets) + 1]uint64
		var mm map[string]deltaModel
		var bb map[string]deltaBackend
		_ = json.Unmarshal([]byte(pHist), &h)
		_ = json.Unmarshal([]byte(pTTFTHist), &th)
		_ = json.Unmarshal([]byte(pModels), &mm)
		_ = json.Unmarshal([]byte(pBack), &bb)
		for i := range hist {
			hist[i] += h[i]
			ttftHist[i] += th[i]
		}
		for k, v := range mm {
			g := models[k]
			g.Requests += v.Requests
			g.TokIn += v.TokIn
			g.TokOut += v.TokOut
			g.TokCache += v.TokCache
			g.Cost += v.Cost
			models[k] = g
		}
		for k, v := range bb {
			g := backends[k]
			g.Requests += v.Requests
			g.TokIn += v.TokIn
			g.TokOut += v.TokOut
			g.TokCache += v.TokCache
			g.Err += v.Err
			g.Cost += v.Cost
			backends[k] = g
		}
		latW += pLatW
		latSum += pLatSum
		ttftW += pTTFTW
		ttftSum += pTTFTSum
		pReq += d.Requests
		pErr += d.Err
		pIn += d.TokIn
		pOut += d.TokOut
		pCache += d.TokCache
		pCost += d.Cost
		pMS += d.MS
		histN += pHistN
		missingN += pMissingN
	} else {
		pReq, pErr, pIn, pOut, pCache, pMS = d.Requests, d.Err, d.TokIn, d.TokOut, d.TokCache, d.MS
		pCost = d.Cost
	}

	hj, _ := json.Marshal(hist[:])
	thj, _ := json.Marshal(ttftHist[:])
	mj, _ := json.Marshal(models)
	bj, _ := json.Marshal(backends)
	_, err = tx.Exec(`INSERT INTO snapshot_minute
	   (minute,requests,err,tok_in,tok_out,tok_cache,cost,ms,lat_w,lat_sum,ttft_w,ttft_sum,hist,ttft_hist,models,backends,hist_n,missing_n)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	   ON CONFLICT(minute) DO UPDATE SET
	     requests=excluded.requests, err=excluded.err, tok_in=excluded.tok_in, tok_out=excluded.tok_out,
	     tok_cache=excluded.tok_cache, cost=excluded.cost, ms=excluded.ms,
	     lat_w=excluded.lat_w, lat_sum=excluded.lat_sum, ttft_w=excluded.ttft_w, ttft_sum=excluded.ttft_sum,
	     hist=excluded.hist, ttft_hist=excluded.ttft_hist, models=excluded.models, backends=excluded.backends,
	     hist_n=excluded.hist_n, missing_n=excluded.missing_n`,
		minute, pReq, pErr, pIn, pOut, pCache, pCost, pMS, latW, latSum, ttftW, ttftSum,
		string(hj), string(thj), string(mj), string(bj), histN, missingN)
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
	// Prefer the minute rollup: re-reading every raw tick to produce 500 points
	// cost 7.5s and 892MB at 1M ticks, and it re-read the whole table on every
	// 10s poll. The rollup carries the same merged dimensions, latency
	// histogram and TTFT accumulators, so the output is equivalent for any
	// window it covers.
	if rows, ok, err := s.seriesFromMinutes(ctx, sinceMs, minTs, maxPoints); ok {
		return rows, err
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

// seriesFromMinutes aggregates the minute rollup. ok is false when the window
// is not fully covered by the rollup (history predating it, or the rollup has
// not accumulated yet), in which case the caller falls back to raw ticks so no
// window is ever silently empty.
//
// The rollup is bucketed in Go rather than with SQL GROUP BY on purpose: the
// dimension maps and histograms are JSON columns, and a bare column under
// GROUP BY yields an arbitrary row from each group (SQLite/MySQL semantics),
// which silently dropped all but one minute's dimensions per bucket. Reading
// minute rows is still ~20x fewer rows than reading raw ticks (20 ticks per
// minute at the 3s cadence), which is where the win comes from.
func (s *Store) seriesFromMinutes(ctx context.Context, sinceMs, oldestTick int64, maxPoints int) ([]SeriesRow, bool, error) {
	var minutes, firstMinute, lastMinute int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(minute),0), COALESCE(MAX(minute),0) FROM snapshot_minute`).
		Scan(&minutes, &firstMinute, &lastMinute); err != nil {
		return nil, false, err
	}
	if minutes == 0 {
		return nil, false, nil
	}
	// The rollup is only usable when it covers everything the window can see.
	// Two ways that holds:
	//   - the rollup starts at least as early as the oldest raw tick (it is
	//     complete), or
	//   - the window itself starts after the rollup's first minute, so any
	//     uncovered older ticks are outside the window anyway.
	// Using only the second condition made the common case (selecting a window
	// wider than the retained history) fall back needlessly and stay slow.
	rollupComplete := oldestTick == 0 || oldestTick >= firstMinute-60000
	windowInsideRollup := sinceMs > 0 && sinceMs >= firstMinute-60000
	if !rollupComplete && !windowInsideRollup {
		return nil, false, nil
	}
	if maxPoints < 1 {
		maxPoints = 500
	}
	bucketMs := int64(60000)
	if minutes > int64(maxPoints) {
		span := lastMinute - firstMinute
		bucketMs = (span/int64(maxPoints)/60000 + 1) * 60000
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT minute, requests, tok_in, tok_out, tok_cache, cost, ms, err,
		       lat_w, lat_sum, ttft_w, ttft_sum, hist, ttft_hist, models, backends, missing_n
		FROM snapshot_minute
		WHERE minute >= ?
		ORDER BY minute ASC`, sinceMs)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	type acc struct {
		row                          SeriesRow
		latW, latSum, ttftW, ttftSum float64
		hist, ttftHist               [len(latBuckets) + 1]uint64
		missingN                     int64
		models                       map[string]deltaModel
		backends                     map[string]deltaBackend
	}
	buckets := map[int64]*acc{}
	var order []int64
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		var minute, req, tin, tout, tcache, ms, errN, missingN int64
		var cost, latW, latSum, ttftW, ttftSum float64
		var hj, thj, mj, bj string
		if err = rows.Scan(&minute, &req, &tin, &tout, &tcache, &cost, &ms, &errN,
			&latW, &latSum, &ttftW, &ttftSum, &hj, &thj, &mj, &bj, &missingN); err != nil {
			return nil, false, err
		}
		idx := (minute - firstMinute) / bucketMs
		a := buckets[idx]
		if a == nil {
			a = &acc{models: map[string]deltaModel{}, backends: map[string]deltaBackend{}}
			buckets[idx] = a
			order = append(order, idx)
			a.row.TS = minute
		}
		a.row.Requests += req
		a.row.TokIn += tin
		a.row.TokOut += tout
		a.row.TokCache += tcache
		a.row.Cost += cost
		a.row.MS += ms
		a.row.Err += errN
		a.latW += latW
		a.latSum += latSum
		a.ttftW += ttftW
		a.ttftSum += ttftSum
		a.missingN += missingN

		var h, th [len(latBuckets) + 1]uint64
		if json.Unmarshal([]byte(hj), &h) == nil {
			for i, v := range h {
				a.hist[i] += v
			}
		}
		if json.Unmarshal([]byte(thj), &th) == nil {
			for i, v := range th {
				a.ttftHist[i] += v
			}
		}
		var mm map[string]deltaModel
		var bb map[string]deltaBackend
		_ = json.Unmarshal([]byte(mj), &mm)
		_ = json.Unmarshal([]byte(bj), &bb)
		for k, v := range mm {
			g := a.models[k]
			g.Requests += v.Requests
			g.TokIn += v.TokIn
			g.TokOut += v.TokOut
			g.TokCache += v.TokCache
			g.Cost += v.Cost
			a.models[k] = g
		}
		for k, v := range bb {
			g := a.backends[k]
			g.Requests += v.Requests
			g.TokIn += v.TokIn
			g.TokOut += v.TokOut
			g.TokCache += v.TokCache
			g.Err += v.Err
			g.Cost += v.Cost
			a.backends[k] = g
		}
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	if len(order) == 0 {
		return []SeriesRow{}, true, nil
	}
	// order is already ascending because rows are ordered by minute.
	out := make([]SeriesRow, 0, len(order))
	for _, idx := range order {
		a := buckets[idx]
		a.row.Models = a.models
		a.row.Backends = a.backends
		a.row.AggregateBucketMs = bucketMs
		a.row.DistributionsUnavailable = a.missingN > 0
		a.row.DistributionsApproximate = a.missingN == 0
		if a.latW > 0 {
			a.row.LatAvg = a.latSum / a.latW
		}
		if a.ttftW > 0 {
			a.row.TTFTAvg = a.ttftSum / a.ttftW
		}
		if !a.row.DistributionsUnavailable {
			a.row.LatP50 = histogramPercentile(a.hist, 50)
			a.row.LatP95 = histogramPercentile(a.hist, 95)
			a.row.LatP99 = histogramPercentile(a.hist, 99)
		}
		out = append(out, a.row)
	}
	return out, true, nil
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

// Range reports the retained snapshot window from the maintained totals row
// instead of scanning every tick (MIN/MAX/COUNT over 1M rows cost 108ms and
// grew forever on a table that gains a row every 3s).
func (s *Store) Range() (oldest, newest int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(`SELECT oldest_ts, newest_ts, ticks FROM analytics_totals WHERE id=1`)
	_ = row.Scan(&oldest, &newest, &count)
	return
}

// RebuildTotals recomputes the running totals from the raw tick table. The
// maintained row is exact by construction (same transaction as the insert),
// so this is a repair/verification path — not something the request path runs.
// Use it after a manual import, or in tests asserting the invariant holds.
func (s *Store) RebuildTotals() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE analytics_totals SET
	   cost=(SELECT COALESCE(SUM(d_cost),0) FROM snapshots),
	   requests=(SELECT COALESCE(SUM(d_requests),0) FROM snapshots),
	   tok_in=(SELECT COALESCE(SUM(d_tok_in),0) FROM snapshots),
	   tok_out=(SELECT COALESCE(SUM(d_tok_out),0) FROM snapshots),
	   tok_cache=(SELECT COALESCE(SUM(d_tok_cache),0) FROM snapshots),
	   err=(SELECT COALESCE(SUM(d_err),0) FROM snapshots),
	   oldest_ts=(SELECT COALESCE(MIN(ts),0) FROM snapshots),
	   newest_ts=(SELECT COALESCE(MAX(ts),0) FROM snapshots),
	   ticks=(SELECT COUNT(*) FROM snapshots)
	 WHERE id=1`)
	return err
}

// SumSnapshots computes the lifetime totals directly from raw ticks. Only used
// to verify the maintained row (tests, repair) — never on a request path.
func (s *Store) SumSnapshots() (cost float64, requests, tokIn, tokOut, tokCache, errN int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT COALESCE(SUM(d_cost),0), COALESCE(SUM(d_requests),0), COALESCE(SUM(d_tok_in),0),
		        COALESCE(SUM(d_tok_out),0), COALESCE(SUM(d_tok_cache),0), COALESCE(SUM(d_err),0)
		 FROM snapshots`)
	_ = row.Scan(&cost, &requests, &tokIn, &tokOut, &tokCache, &errN)
	return
}

// LifetimeTotals sums the retained window. This is the dashboard's
// headline lifetime cost — it survives verloc restarts.
// LifetimeTotals reads the running totals row maintained by InsertDelta.
// It is an O(1) primary-key lookup: previously this SUMmed the entire snapshots
// table on every /dash/api/state poll (250ms at 1M ticks) while holding s.mu,
// which blocked the analytics writer for 241ms each time.
func (s *Store) LifetimeTotals() (cost float64, requests, tokIn, tokOut, tokCache, errN int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRow(
		`SELECT cost, requests, tok_in, tok_out, tok_cache, err FROM analytics_totals WHERE id=1`)
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

// invalidateSummary drops memoised page summaries. Called whenever rows are
// inserted: a cached total must never outlive the data it describes (a window
// like "last 1h" gains rows continuously).
func (s *Store) invalidateSummary() {
	s.summaryMu.Lock()
	if len(s.summaryCache) > 0 {
		s.summaryCache = map[string]summaryCacheEntry{}
	}
	s.summaryMu.Unlock()
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
	s.invalidateSummary()
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

// summaryCache memoises the full-scan summary aggregate. The dashboard
// recomputes it on every pagination click (the same filter, a new cursor), so
// without this a 300k-row history paid 138ms per click. Keyed by the filter
// identity (which excludes the cursor), so paging within one filter is free and
// any filter change recomputes. Bounded to a few entries; the dashboard uses
// one filter at a time per tab.
type summaryCacheEntry struct {
	at int64
	s  RequestSummary
}

// summaryMemoTTLMs is a safety net behind write-triggered invalidation: a
// summary is honoured for at most a few seconds, matching the ticker cadence.
const summaryMemoTTLMs = 3000

func (s *Store) Requests(ctx context.Context, f RequestFilter) (RequestPage, error) {
	out := RequestPage{Requests: []ReqRecord{}}
	where, args := f.where(false)
	// One read transaction makes totals and the page a consistent snapshot.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Serve the summary from cache when the same filter is being paged: the
	// aggregate covers the whole filtered set, so it cannot change while only
	// the cursor moves. This removes a full scan from every "next page" click.
	// cacheKey deliberately excludes the cursor and the limit.
	cacheKey := f.Model + "\x00" + f.Backend + "\x00" + f.ClientKeyID + "\x00" + f.Status + "\x00" + f.Q +
		"\x00" + strconv.FormatInt(f.Since, 10) + "\x00" + strconv.FormatInt(f.Until, 10)
	s.summaryMu.Lock()
	cached, ok := s.summaryCache[cacheKey]
	if ok && nowMs()-cached.at > summaryMemoTTLMs {
		ok = false
	}
	s.summaryMu.Unlock()
	if ok {
		out.Summary = cached.s
	} else {
		if err = scanSummary(tx.QueryRowContext(ctx, `SELECT `+summarySQL+` FROM requests WHERE `+where, args...), &out.Summary); err != nil {
			return out, err
		}
		s.summaryMu.Lock()
		if s.summaryCache == nil {
			s.summaryCache = map[string]summaryCacheEntry{}
		}
		if len(s.summaryCache) >= 8 {
			for k := range s.summaryCache {
				delete(s.summaryCache, k)
			}
		}
		s.summaryCache[cacheKey] = summaryCacheEntry{at: nowMs(), s: out.Summary}
		s.summaryMu.Unlock()
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

// addColumns accumulates one request from its stored columns. It must stay
// exactly equivalent to addRecord (which takes a full ReqRecord) so the SQL
// projection cannot change any reported total.
func (a *usageAccumulator) addColumns(ok, usageKnown, costKnown bool, tin, tout, tcache uint64, cost, latMs, ttftMs float64) {
	a.Requests++
	if !ok {
		a.Errors++
	}
	a.TokIn += tin
	a.TokOut += tout
	a.TokCache += tcache
	a.Cost += cost
	if !usageKnown {
		a.UnknownUsage++
	}
	if !costKnown {
		a.UnknownCost++
	}
	a.latSum += latMs
	a.latHist[bucketOf(latMs)]++
	if ttftMs > 0 {
		a.ttftSum += ttftMs
		a.ttftN++
		a.ttftHist[bucketOf(ttftMs)]++
	}
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
	// Project the aggregate columns instead of the JSON `record` blob: every
	// field this function sums already lives in a column, so decoding 200k
	// ReqRecord values was pure waste (measured 262MB / 3.2M allocs, and the
	// dominant cost of the unfiltered query). The accumulator is filled
	// directly, which also removes the intermediate ReqRecord entirely.
	where, args := f.where(false)
	rows, err := tx.QueryContext(ctx, `SELECT ts, model, backend, client_key_id, client_name, status, ok,
	                                        tok_in, tok_out, tok_cache, cost, usage_known, cost_known, lat_ms, ttft_ms
	                                 FROM requests WHERE `+where, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var (
			ts, status                  int64
			ok, usageKnown, costKnown   bool
			model, backend, ckID, cName string
			tin, tout, tcache           uint64
			cost, lat, ttft             float64
		)
		if err = rows.Scan(&ts, &model, &backend, &ckID, &cName, &status, &ok,
			&tin, &tout, &tcache, &cost, &usageKnown, &costKnown, &lat, &ttft); err != nil {
			rows.Close()
			return out, err
		}
		a := &usageAccumulator{}
		a.addColumns(ok, usageKnown, costKnown, tin, tout, tcache, cost, lat, ttft)
		add(model, backend, ckID, cName, int(status), ts, a)
		cover(ts)
		if out.Coverage.DetailOldestTS == 0 || ts < out.Coverage.DetailOldestTS {
			out.Coverage.DetailOldestTS = ts
		}
		if ts > out.Coverage.DetailNewestTS {
			out.Coverage.DetailNewestTS = ts
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
