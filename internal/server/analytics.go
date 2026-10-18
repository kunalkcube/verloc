package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

type usageAccumulator struct {
	RequestSummary
	latSum, ttftSum   float64
	ttftN             int64
	latHist, ttftHist [len(latBuckets) + 1]uint64
}

// analyticsAggColumns is the SELECT list (in scanAgg's scan order) that
// computes one usageAccumulator entirely in SQL: counts, token/cost sums,
// unknown counters, latency/TTFT sums, and both histograms as CASE sums
// matching bucketOf (first edge >= ms; TTFT only for ttft_ms > 0).
func analyticsAggColumns() string {
	var sb strings.Builder
	sb.WriteString(`COUNT(*),
		COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(tok_in),0), COALESCE(SUM(tok_out),0), COALESCE(SUM(tok_cache),0),
		COALESCE(SUM(cost),0),
		COALESCE(SUM(CASE WHEN usage_known=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN cost_known=0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(lat_ms),0),
		COALESCE(SUM(CASE WHEN ttft_ms>0 THEN ttft_ms ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN ttft_ms>0 THEN 1 ELSE 0 END),0)`)
	for i := range len(latBuckets) + 1 {
		var latCond, ttftCond string
		if i < len(latBuckets) {
			latCond = fmt.Sprintf("lat_ms<=%v", latBuckets[i])
			ttftCond = fmt.Sprintf("ttft_ms>0 AND ttft_ms<=%v", latBuckets[i])
			if i > 0 {
				latCond += fmt.Sprintf(" AND lat_ms>%v", latBuckets[i-1])
				ttftCond += fmt.Sprintf(" AND ttft_ms>%v", latBuckets[i-1])
			}
		} else {
			latCond = fmt.Sprintf("lat_ms>%v", latBuckets[len(latBuckets)-1])
			ttftCond = fmt.Sprintf("ttft_ms>%v", latBuckets[len(latBuckets)-1])
		}
		fmt.Fprintf(&sb, ", COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END),0)", latCond, ttftCond)
	}
	return sb.String()
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
	// Aggregate IN SQL instead of folding every row in Go. The previous
	// column projection removed the JSON decode (262MB / 3.2M allocs at
	// 200k rows), but the driver still scanned one row per request
	// (~25 allocs each). Every accumulator field is a SUM or a histogram
	// CASE, so the database returns one row per GROUP — the per-request
	// cost is gone rather than shaved. The hourly tier below still folds
	// in Go (it is tiny) through the same add(), keeping one merge path.
	where, args := f.where(false)
	aggCols := analyticsAggColumns()
	// scanAgg reads one aggregate row (optionally keyed) in aggCols order.
	scanAgg := func(scan func(...any) error, key *string) (*usageAccumulator, int64, int64, error) {
		a := &usageAccumulator{}
		var latH, ttftH [len(latBuckets) + 1]int64
		dest := []any{}
		if key != nil {
			dest = append(dest, key)
		}
		dest = append(dest, &a.Requests, &a.Errors, &a.TokIn, &a.TokOut, &a.TokCache, &a.Cost,
			&a.UnknownUsage, &a.UnknownCost, &a.latSum, &a.ttftSum, &a.ttftN)
		for i := range latH {
			dest = append(dest, &latH[i], &ttftH[i])
		}
		var oldest, newest int64
		dest = append(dest, &oldest, &newest)
		if err := scan(dest...); err != nil {
			return nil, 0, 0, err
		}
		for i := range latH {
			a.latHist[i] = uint64(latH[i])
			a.ttftHist[i] = uint64(ttftH[i])
		}
		return a, oldest, newest, nil
	}
	mergeInto := func(m map[string]*usageAccumulator, key string, a *usageAccumulator) {
		x := m[key]
		if x == nil {
			x = &usageAccumulator{}
			m[key] = x
		}
		x.merge(a)
	}

	totalAgg, detOld, detNew, err := scanAgg(tx.QueryRowContext(ctx,
		`SELECT `+aggCols+`, COALESCE(MIN(ts),0), COALESCE(MAX(ts),0) FROM requests WHERE `+where, args...).Scan, nil)
	if err != nil {
		return out, err
	}
	if totalAgg.Requests > 0 {
		total.merge(totalAgg)
		cover(detOld)
		cover(detNew)
		out.Coverage.DetailOldestTS = detOld
		out.Coverage.DetailNewestTS = detNew

		groupQueries := []struct {
			col string
			m   map[string]*usageAccumulator
		}{{"model", models}, {"backend", providers}, {"client_key_id", clients}}
		for _, gq := range groupQueries {
			grows, gerr := tx.QueryContext(ctx,
				`SELECT `+gq.col+`, `+aggCols+`, 0, 0 FROM requests WHERE `+where+` GROUP BY `+gq.col, args...)
			if gerr != nil {
				return out, gerr
			}
			for grows.Next() {
				var key string
				a2, _, _, serr := scanAgg(grows.Scan, &key)
				if serr != nil {
					grows.Close()
					return out, serr
				}
				mergeInto(gq.m, key, a2)
			}
			if gerr = grows.Err(); gerr != nil {
				grows.Close()
				return out, gerr
			}
			grows.Close()
		}
		// Latest client display name per key (max ts, name breaks ties) —
		// the same rule the Go fold applied per row.
		nameRows, nerr := tx.QueryContext(ctx,
			`SELECT client_key_id, client_name FROM (
			   SELECT client_key_id, client_name,
			          ROW_NUMBER() OVER (PARTITION BY client_key_id ORDER BY ts DESC, client_name DESC) AS rn
			   FROM requests WHERE `+where+`) WHERE rn = 1`, args...)
		if nerr != nil {
			return out, nerr
		}
		for nameRows.Next() {
			var id, name string
			if err = nameRows.Scan(&id, &name); err != nil {
				nameRows.Close()
				return out, err
			}
			names[id] = name
			nameTS[id] = detNew // hourly fold may only override with newer data
		}
		if err = nameRows.Err(); err != nil {
			nameRows.Close()
			return out, err
		}
		nameRows.Close()
		// Status counts.
		statusRows, serr := tx.QueryContext(ctx,
			`SELECT status, COUNT(*) FROM requests WHERE `+where+` GROUP BY status`, args...)
		if serr != nil {
			return out, serr
		}
		for statusRows.Next() {
			var st, n int64
			if err = statusRows.Scan(&st, &n); err != nil {
				statusRows.Close()
				return out, err
			}
			statuses[int(st)] = n
		}
		if err = statusRows.Err(); err != nil {
			statusRows.Close()
			return out, err
		}
		statusRows.Close()
	}
	// request_hourly is a COVERAGE TIER whose rows are aggregates of detail that
	// was PRUNED, so they are additive with any surviving detail for the same
	// hour (the documented legacy behaviour, and covered by
	// TestLegacyHourlyHistoryAndWindowBoundaries).
	//
	// The hazard this excludes: if a future writer mirrors detail into this
	// table WITHOUT deleting it, those rows would be counted twice. Such a row
	// must declare itself with mirrors_detail=1. Legacy rows default to 0 and
	// keep contributing exactly as before.
	hourlyWhere, hourlyArgs := f.where(true)
	// The mirrors_detail exclusion must apply to EVERY query shape — the
	// old string replacement only fired when an Until bound happened to
	// produce that exact substring, so since-only or model-only queries
	// would double-count a future mirroring writer's rows.
	if hourlyWhere == "1=1" {
		hourlyWhere = "mirrors_detail=0"
	} else {
		hourlyWhere += " AND mirrors_detail=0"
	}
	rows, qerr := tx.QueryContext(ctx, `SELECT hour,model,backend,client_key_id,client_name,status,requests,errors,tok_in,tok_out,tok_cache,cost,unknown_usage,unknown_cost,lat_sum,ttft_sum,ttft_n,lat_hist,ttft_hist FROM request_hourly WHERE `+hourlyWhere, hourlyArgs...)
	if qerr != nil {
		return out, qerr
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
