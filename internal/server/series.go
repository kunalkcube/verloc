package server

import (
	"context"
	"encoding/json"
)

func (s *Store) SeriesSince(ctx context.Context, sinceMs int64, maxPoints int) ([]SeriesRow, error) {
	if maxPoints < 1 {
		maxPoints = 500
	}
	// Lock-free read: this used to hold s.mu across the whole scan, so one
	// 126ms dashboard read blocked the tick writer. WAL readers never
	// block the single writer, and this method touches only s.db.
	// NOTE: an earlier attempt consulted the rollup before counting ticks
	// and broke sparse-history windows (few old ticks must serve tick-level
	// points, not minute buckets). The count is an index-only walk and was
	// never the bottleneck — the per-tick JSON decode was, fixed by the
	// rollup itself. Correctness first: count, then choose.
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

// seriesScan reads every tick in the window. Lock-free: WAL readers never block the single writer.
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
		_ = hj
		_ = thj
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

		// One codec call replaces three json.Unmarshal calls per minute row,
		// which was the dominant remaining cost of this endpoint. Legacy JSON
		// rows are decoded transparently by the codec.
		var h, th []uint64
		var mm map[string]deltaModel
		var bb map[string]deltaBackend
		_ = decodeMinutes([]byte(mj), &mm, &bb, &h, &th)
		// Legacy JSON-era rows keep their histograms in the separate hist
		// columns, not in the dimension blob: fall back to them so those
		// minutes do not silently report zeroed percentiles.
		if len(h) == 0 && hj != "" {
			_ = json.Unmarshal([]byte(hj), &h)
		}
		if len(th) == 0 && thj != "" {
			_ = json.Unmarshal([]byte(thj), &th)
		}
		if len(h) == len(latBuckets)+1 {
			for i, v := range h {
				a.hist[i] += v
			}
		}
		if len(th) == len(latBuckets)+1 {
			for i, v := range th {
				a.ttftHist[i] += v
			}
		}
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

// seriesBuckets aggregates a long window in SQL. Lock-free read (WAL).
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
