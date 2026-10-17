package server

import (
	"math"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// latBuckets are the histogram bucket edges in ms (last bucket is >60000).
var latBuckets = [...]float64{50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000, 60000}

// metricSet holds cumulative counters for a single dimension (model or backend).
type metricSet struct {
	requests atomic.Uint64
	ok       atomic.Uint64
	err      atomic.Uint64
	stream   atomic.Uint64
	tokIn    atomic.Uint64
	tokOut   atomic.Uint64
	tokCache atomic.Uint64
	latSumNs atomic.Uint64
	failover atomic.Uint64
	ttftSum  atomic.Uint64 // stream: time-to-first-token, ns
	ttftN    atomic.Uint64
	costBits atomic.Uint64 // float64 bits, USD (session scope)
	hist     [len(latBuckets) + 1]atomic.Uint64
	mu       sync.Mutex
	status   map[int]uint64
	// Delta counters, drained by DrainDelta for the 3s ticker. The ticker
	// diffs cumulative counters today, which forces a full Snapshot (with its
	// map-of-any allocations) twice per tick just to subtract numbers.
	dReq      atomic.Uint64
	dErr      atomic.Uint64
	dFailover atomic.Uint64
	dTokIn    atomic.Uint64
	dTokOut   atomic.Uint64
	dTokCache atomic.Uint64
	dCostBits atomic.Uint64
}

func newMetricSet() *metricSet { return &metricSet{status: map[int]uint64{}} }

func (m *metricSet) addCost(c float64) {
	if c <= 0 {
		return
	}
	for {
		old := m.costBits.Load()
		if m.costBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+c)) {
			return
		}
	}
}

func (m *metricSet) cost() float64 { return math.Float64frombits(m.costBits.Load()) }

func (m *metricSet) addDeltaCost(c float64) {
	if c <= 0 {
		return
	}
	for {
		old := m.dCostBits.Load()
		if m.dCostBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+c)) {
			return
		}
	}
}

// drainDelta reads and resets the per-tick counters. Reset is not atomic with
// the read, so a concurrent Add may land either side of the boundary; that is
// acceptable for a 3s tick (the value appears in one tick or the next, never
// lost) and avoids serializing the request path behind a mutex.
func (m *metricSet) drainDelta() setDelta {
	d := setDelta{
		Req: m.dReq.Swap(0), Err: m.dErr.Swap(0), Failover: m.dFailover.Swap(0),
		TokIn: m.dTokIn.Swap(0), TokOut: m.dTokOut.Swap(0), TokCache: m.dTokCache.Swap(0),
		Cost: math.Float64frombits(m.dCostBits.Swap(0)),
	}
	return d
}

// setDelta is one tick of activity for a model or backend. Typed on purpose:
// the ticker path must not build map[string]any trees per tick.
type setDelta struct {
	Req, Err, Failover, TokIn, TokOut, TokCache uint64
	Cost                                        float64
}

// TickDelta is the drained activity for the whole gateway since the last
// drain, split by model and backend.
type TickDelta struct {
	Total    setDelta
	Models   map[string]setDelta
	Backends map[string]setDelta
}

// DrainDelta returns activity accumulated since the previous call. It replaces
// (Snapshot, Snapshot, computeDelta) on the ticker path: no map-of-any, no
// per-set status/histogram copies, no JSON-typed intermediate.
func (mt *Metrics) DrainDelta() TickDelta {
	mt.mu.Lock()
	models := make(map[string]setDelta, len(mt.models))
	backends := make(map[string]setDelta, len(mt.backends))
	for k, v := range mt.models {
		d := v.drainDelta()
		if d.Req > 0 || d.TokIn > 0 || d.TokOut > 0 || d.Err > 0 || d.Cost > 0 || d.Failover > 0 {
			models[k] = d
		}
	}
	for k, v := range mt.backends {
		d := v.drainDelta()
		if d.Req > 0 || d.TokIn > 0 || d.TokOut > 0 || d.Err > 0 || d.Cost > 0 || d.Failover > 0 {
			backends[k] = d
		}
	}
	mt.mu.Unlock()

	out := TickDelta{Models: models, Backends: backends}
	for _, d := range models {
		out.Total.Req += d.Req
		out.Total.Err += d.Err
		out.Total.Failover += d.Failover
		out.Total.TokIn += d.TokIn
		out.Total.TokOut += d.TokOut
		out.Total.TokCache += d.TokCache
		out.Total.Cost += d.Cost
	}
	return out
}

func (m *metricSet) add(reqs, okc, errc uint64, stream bool, tin, tout, tcache, latNs, ttftNs uint64, failover bool, status int) {
	if reqs > 0 {
		m.requests.Add(reqs)
		m.dReq.Add(reqs)
	}
	if okc > 0 {
		m.ok.Add(okc)
	}
	if errc > 0 {
		m.err.Add(errc)
		m.dErr.Add(errc)
	}
	if stream {
		m.stream.Add(1)
	}
	if tin > 0 {
		m.tokIn.Add(tin)
		m.dTokIn.Add(tin)
	}
	if tout > 0 {
		m.tokOut.Add(tout)
		m.dTokOut.Add(tout)
	}
	if tcache > 0 {
		m.tokCache.Add(tcache)
		m.dTokCache.Add(tcache)
	}
	if latNs > 0 {
		m.latSumNs.Add(latNs)
		m.hist[bucketOf(float64(latNs)/1e6)].Add(1)
	}
	if ttftNs > 0 {
		m.ttftSum.Add(ttftNs)
		m.ttftN.Add(1)
	}
	if failover {
		m.failover.Add(1)
		m.dFailover.Add(1)
	}
	if status > 0 {
		m.mu.Lock()
		m.status[status]++
		m.mu.Unlock()
	}
}

func bucketOf(ms float64) int {
	for i, e := range latBuckets {
		if ms <= e {
			return i
		}
	}
	return len(latBuckets)
}

// histPercentile estimates a percentile from the latency histogram.
func (m *metricSet) histPercentile(p int) float64 {
	var counts [len(latBuckets) + 1]uint64
	for i := range m.hist {
		counts[i] = m.hist[i].Load()
	}
	return histogramPercentile(counts, p)
}

func (m *metricSet) snap() map[string]any {
	m.mu.Lock()
	st := make(map[string]uint64, len(m.status))
	for k, v := range m.status {
		st[strconv.Itoa(k)] = v
	}
	m.mu.Unlock()
	n := m.ok.Load() + m.err.Load()
	var avg float64
	if n > 0 {
		avg = float64(m.latSumNs.Load()) / float64(n) / 1e6
	}
	var ttftAvg float64
	if tn := m.ttftN.Load(); tn > 0 {
		ttftAvg = float64(m.ttftSum.Load()) / float64(tn) / 1e6
	}
	hist := make([]uint64, len(latBuckets)+1)
	for i := range m.hist {
		hist[i] = m.hist[i].Load()
	}
	return map[string]any{
		"requests":    m.requests.Load(),
		"ok":          m.ok.Load(),
		"err":         m.err.Load(),
		"stream":      m.stream.Load(),
		"tok_in":      m.tokIn.Load(),
		"tok_out":     m.tokOut.Load(),
		"tok_cache":   m.tokCache.Load(),
		"lat_avg_ms":  avg,
		"lat_p50_ms":  m.histPercentile(50),
		"lat_p95_ms":  m.histPercentile(95),
		"lat_p99_ms":  m.histPercentile(99),
		"ttft_avg_ms": ttftAvg,
		"cost":        m.cost(),
		"failover":    m.failover.Load(),
		"status":      st,
		"lat_hist":    hist,
	}
}

// Metrics is the global, thread-safe metrics registry.
type Metrics struct {
	start    time.Time
	mu       sync.Mutex
	models   map[string]*metricSet
	backends map[string]*metricSet
	rMu      sync.Mutex
	recent   []float64
	rIdx     int
	filled   int
	// Fixed-size tick histograms avoid per-request sample allocation.
	tMu  sync.Mutex
	tick tickAgg
}

func NewMetrics() *Metrics {
	return &Metrics{
		start:    time.Now(),
		models:   map[string]*metricSet{},
		backends: map[string]*metricSet{},
		recent:   make([]float64, 2000),
	}
}

func (mt *Metrics) model(name string) *metricSet {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	s, ok := mt.models[name]
	if !ok {
		s = newMetricSet()
		mt.models[name] = s
	}
	return s
}

func (mt *Metrics) backend(name string) *metricSet {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	s, ok := mt.backends[name]
	if !ok {
		s = newMetricSet()
		mt.backends[name] = s
	}
	return s
}

func (mt *Metrics) Record(model, backend string, stream bool, status int, tin, tout, tcache, latNs, ttftNs uint64) {
	// Derive ok from the status: hardcoding true recorded every 5xx as a
	// success for any caller of this convenience wrapper.
	mt.Record2(model, backend, stream, status, tin, tout, tcache, latNs, ttftNs, status >= 200 && status < 300)
}

// Record2 records one completed request with explicit ok/err classification.
func (mt *Metrics) Record2(model, backend string, stream bool, status int, tin, tout, tcache, latNs, ttftNs uint64, ok bool) {
	cost := costOf(model, tin, tout, tcache)
	mt.record(model, backend, stream, status, tin, tout, tcache, latNs, ttftNs, ok, cost)
}

// RecordRequest meters the immutable route-pricing decision made for this
// logical request. It never consults a subsequently edited runtime catalog.
func (mt *Metrics) RecordRequest(rec ReqRecord) {
	tin, tout, cache := rec.TokIn, rec.TokOut, rec.TokCache
	if !rec.UsageKnown {
		tin, tout, cache = 0, 0, 0
	}
	var cost float64
	if rec.UsageKnown && rec.CostKnown {
		cost = rec.Cost
	}
	mt.record(rec.Model, rec.Backend, rec.Stream, rec.Status, tin, tout, cache, uint64(rec.LatMs*1e6), uint64(rec.TTFTMs*1e6), rec.OK, cost)
}

func (mt *Metrics) record(model, backend string, stream bool, status int, tin, tout, tcache, latNs, ttftNs uint64, ok bool, cost float64) {
	var okc, errc uint64
	if ok {
		okc = 1
	} else {
		errc = 1
	}
	m, b := mt.model(model), mt.backend(backend)
	m.add(1, okc, errc, stream, tin, tout, tcache, latNs, ttftNs, false, status)
	b.add(1, okc, errc, stream, tin, tout, tcache, latNs, ttftNs, false, status)
	if cost > 0 && !math.IsInf(cost, 0) {
		m.addCost(cost)
		b.addCost(cost)
		m.addDeltaCost(cost)
		b.addDeltaCost(cost)
	}
	latMs := float64(latNs) / 1e6
	mt.pushRecent(latMs)
	mt.pushTick(latMs, float64(ttftNs)/1e6)
}

func (mt *Metrics) RecordErr(model, backend string, stream bool, status int, latNs uint64) {
	latMs := float64(latNs) / 1e6
	mt.model(model).add(1, 0, 1, stream, 0, 0, 0, latNs, 0, false, status)
	mt.backend(backend).add(1, 0, 1, stream, 0, 0, 0, latNs, 0, false, status)
	mt.pushRecent(latMs)
	mt.pushTick(latMs, 0)
}

func (mt *Metrics) RecordFailover(model, backend string) {
	mt.model(model).add(0, 0, 0, false, 0, 0, 0, 0, 0, true, 0)
	mt.backend(backend).add(0, 0, 0, false, 0, 0, 0, 0, 0, true, 0)
}

func (mt *Metrics) pushRecent(ms float64) {
	mt.rMu.Lock()
	cap := len(mt.recent)
	if mt.filled < cap {
		mt.recent[mt.filled] = ms
		mt.filled++
	} else {
		mt.recent[mt.rIdx] = ms
		mt.rIdx = (mt.rIdx + 1) % cap
	}
	mt.rMu.Unlock()
}

func (mt *Metrics) pushTick(latMs, ttftMs float64) {
	mt.tMu.Lock()
	mt.tick.LatSumMs += latMs
	mt.tick.LatN++
	mt.tick.LatHist[bucketOf(latMs)]++
	if ttftMs > 0 {
		mt.tick.TTFTSumMs += ttftMs
		mt.tick.TTFTN++
		mt.tick.TTFTHist[bucketOf(ttftMs)]++
	}
	mt.tMu.Unlock()
}

// DrainTick atomically drains bounded aggregatable latency/TTFT measurements.
func (mt *Metrics) DrainTick() tickAgg {
	mt.tMu.Lock()
	t := mt.tick
	mt.tick = tickAgg{}
	mt.tMu.Unlock()
	return t
}

func (mt *Metrics) percentiles() (p50, p95, p99 float64) {
	mt.rMu.Lock()
	cap := len(mt.recent)
	var cp []float64
	if mt.filled < cap {
		cp = make([]float64, mt.filled)
		copy(cp, mt.recent[:mt.filled])
	} else {
		cp = make([]float64, cap)
		copy(cp, mt.recent)
	}
	mt.rMu.Unlock()
	if len(cp) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(cp)
	p50 = cp[pct(len(cp), 50)]
	p95 = cp[pct(len(cp), 95)]
	p99 = cp[pct(len(cp), 99)]
	return
}

func pct(n, p int) int {
	i := (n*p+99)/100 - 1
	if i >= n {
		i = n - 1
	}
	return i
}

func sumSnaps(in map[string]any) map[string]any {
	out := map[string]any{
		"requests": uint64(0), "ok": uint64(0), "err": uint64(0),
		"stream": uint64(0), "tok_in": uint64(0), "tok_out": uint64(0),
		"tok_cache": uint64(0), "lat_avg_ms": float64(0), "failover": uint64(0),
		"status": map[string]uint64{},
	}
	var latSum float64
	var n uint64
	for _, v := range in {
		s, _ := v.(map[string]any)
		if s == nil {
			continue
		}
		out["requests"] = out["requests"].(uint64) + toU64(s["requests"])
		out["ok"] = out["ok"].(uint64) + toU64(s["ok"])
		out["err"] = out["err"].(uint64) + toU64(s["err"])
		out["stream"] = out["stream"].(uint64) + toU64(s["stream"])
		out["tok_in"] = out["tok_in"].(uint64) + toU64(s["tok_in"])
		out["tok_out"] = out["tok_out"].(uint64) + toU64(s["tok_out"])
		out["tok_cache"] = out["tok_cache"].(uint64) + toU64(s["tok_cache"])
		out["failover"] = out["failover"].(uint64) + toU64(s["failover"])
		latSum += toF64(s["lat_avg_ms"]) * float64(toU64(s["ok"])+toU64(s["err"]))
		n += toU64(s["ok"]) + toU64(s["err"])
		if st, ok := s["status"].(map[string]uint64); ok {
			agg := out["status"].(map[string]uint64)
			for k, v := range st {
				agg[k] += v
			}
		}
	}
	if n > 0 {
		out["lat_avg_ms"] = latSum / float64(n)
	}
	return out
}

func toU64(v any) uint64 {
	switch x := v.(type) {
	case uint64:
		return x
	case int64:
		return uint64(x)
	case float64:
		return uint64(x)
	case int:
		return uint64(x)
	}
	return 0
}

func toF64(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case uint64:
		return float64(x)
	case int:
		return float64(x)
	}
	return 0
}

// Snapshot returns a JSON-serializable view of all metrics.
func (mt *Metrics) Snapshot(backends map[string]*backend) map[string]any {
	mt.mu.Lock()
	models := map[string]any{}
	for k, v := range mt.models {
		models[k] = v.snap()
	}
	bs := map[string]any{}
	for k, v := range mt.backends {
		bs[k] = v.snap()
	}
	mt.mu.Unlock()

	p50, p95, p99 := mt.percentiles()

	keys := map[string]any{}
	for name, b := range backends {
		keys[name] = b.pool.Stats()
	}

	return map[string]any{
		"uptime_sec":  int64(time.Since(mt.start).Seconds()),
		"started_at":  mt.start.Unix(),
		"global":      sumSnaps(models),
		"per_model":   models,
		"per_backend": bs,
		"latency_ms":  map[string]float64{"p50": p50, "p95": p95, "p99": p99},
		"key_stats":   keys,
	}
}
