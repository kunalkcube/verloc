package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

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

// summaryMemoTTLMs is the memo's ONLY freshness mechanism: a summary is
// honoured for at most 3s — one dashboard poll cycle — after which it is
// recomputed. (Write-triggered invalidation cleared the cache every
// ≤100ms batch under load, so it never survived to be useful.)
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
