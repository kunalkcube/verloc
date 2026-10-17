package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// dashState is the full snapshot served at /dash/api/state.
type dashState struct {
	Ready       bool                    `json:"ready"`
	TS          int64                   `json:"ts"`
	UptimeSec   int64                   `json:"uptime_sec"`
	StartedAt   int64                   `json:"started_at"`
	Global      map[string]any          `json:"global"`
	Models      map[string]any          `json:"models"`
	Backends    map[string]any          `json:"backends"`
	Keys        map[string][]KeyStat    `json:"keys"`
	Latency     map[string]float64      `json:"latency_ms"`
	Lifetime    lifetimeTotals          `json:"lifetime"`
	Delta       map[string]any          `json:"delta"`
	Alerts      []AlertEvent            `json:"alerts"`
	AlertRules  []AlertRule             `json:"alert_rules"`
	KeyEvents   []KeyEvent              `json:"key_events"`
	Pricing     map[string]pricingPer1M `json:"pricing"`
	Subscribers int                     `json:"subscribers"`
	Version     string                  `json:"version"`
	Telemetry   TelemetryStatus         `json:"telemetry"`
}

type lifetimeTotals struct {
	Cost     float64 `json:"cost"`
	Requests int64   `json:"requests"`
	TokIn    int64   `json:"tok_in"`
	TokOut   int64   `json:"tok_out"`
	TokCache int64   `json:"tok_cache"`
	Err      int64   `json:"err"`
}

// Ticker periodically persists deltas and evaluates alerts. Run in its own
// goroutine; detailed requests have their own bounded background writer.
type Ticker struct {
	srv      *Server
	stop     chan struct{}
	mu       sync.Mutex
	last     *Delta // latest 3s delta, for /dash/api/state
	done     chan struct{}
	stopOnce sync.Once
	runOnce  sync.Once
	lastTS   int64
}

func NewTicker(srv *Server) *Ticker {
	t := &Ticker{srv: srv, stop: make(chan struct{}), done: make(chan struct{})}
	if srv.store != nil {
		_, t.lastTS, _ = srv.store.Range()
	}
	return t
}

// Stop is idempotent and waits for the final metrics delta to be persisted.
// Call after inference has drained, and before Store.Close.
func (t *Ticker) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
	t.Run()
	<-t.done
}

func (t *Ticker) LastDelta() *Delta {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		return &Delta{TS: nowMs(), Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
	}
	d := *t.last
	d.Models = cloneDM(d.Models)
	d.Backends = cloneDB(d.Backends)
	return &d
}

func cloneDM(m map[string]deltaModel) map[string]deltaModel {
	out := make(map[string]deltaModel, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneDB(m map[string]deltaBackend) map[string]deltaBackend {
	out := make(map[string]deltaBackend, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (t *Ticker) Run() {
	t.runOnce.Do(t.run)
}

func (t *Ticker) run() {
	defer close(t.done)
	lastTick := time.Now()
	flush := func() {
		// Drained typed deltas replace (Snapshot, Snapshot, computeDelta): the
		// ticker used to build two full map[string]any trees per tick purely to
		// subtract cumulative counters, allocating ~172 KB twice every 3s.
		dd := t.srv.metrics.DrainDelta()
		d := deltaFromTick(dd)
		if d.TS <= t.lastTS {
			d.TS = t.lastTS + 1
		}
		t.lastTS = d.TS
		d.MS = time.Since(lastTick).Milliseconds()
		lastTick = time.Now()
		if d.MS < 1 {
			d.MS = 1
		}
		ta := t.srv.metrics.DrainTick()
		ta.TS, ta.Req, ta.Err, ta.Failover = d.TS, d.Requests, d.Err, d.Failover
		ta.TokIn, ta.TokOut, ta.TokCache, ta.Cost = d.TokIn, d.TokOut, d.TokCache, d.Cost
		ta.P50Ms, ta.P95Ms, ta.P99Ms = histogramPercentile(ta.LatHist, 50), histogramPercentile(ta.LatHist, 95), histogramPercentile(ta.LatHist, 99)
		raw, err := json.Marshal(TickStats{Avg: ta.LatAvgMs(), P50: ta.P50Ms, P95: ta.P95Ms, P99: ta.P99Ms, TTFT: ta.TTFTAvgMs(), TTFTN: ta.TTFTN, LatHist: ta.LatHist[:]})
		if err == nil {
			d.Payload = string(raw)
		}
		t.mu.Lock()
		t.last = &d
		t.mu.Unlock()
		models, backends := scopeTicks(d)
		if t.srv.alerts != nil {
			t.srv.alerts.Feed(ta, models, backends)
		}
		if t.srv.store != nil {
			_ = t.srv.store.InsertDelta(&d)
		}
	}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	eval := time.NewTicker(5 * time.Second)
	defer eval.Stop()
	for {
		select {
		case <-t.stop:
			flush()
			return
		case <-ticker.C:
			flush()
		case <-eval.C:
			if t.srv.alerts != nil {
				t.srv.alerts.Eval(time.Now(), func() int {
					n := 0
					for _, b := range t.srv.runtime().backends {
						n += b.pool.InCooldown()
					}
					return n
				})
			}
		}
	}
}

func scopeTicks(d Delta) (models, backends map[string]tickAgg) {
	models = map[string]tickAgg{}
	for k, v := range d.Models {
		models[k] = tickAgg{Req: v.Requests, Err: v.Err, Failover: v.Failover, TokIn: v.TokIn, TokOut: v.TokOut, TokCache: v.TokCache, Cost: v.Cost}
	}
	backends = map[string]tickAgg{}
	for k, v := range d.Backends {
		backends[k] = tickAgg{Req: v.Requests, Err: v.Err, Failover: v.Failover, TokIn: v.TokIn, TokOut: v.TokOut, TokCache: v.TokCache, Cost: v.Cost}
	}
	return
}

// deltaFromTick converts a drained TickDelta into the persisted Delta shape.
// Field-for-field equivalent to the old snapshot differencing, without the
// allocation of two Snapshots per tick.
func deltaFromTick(td TickDelta) Delta {
	d := Delta{TS: nowMs(), Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
	d.Requests = int64(td.Total.Req)
	d.Err = int64(td.Total.Err)
	d.Failover = int64(td.Total.Failover)
	d.TokIn = int64(td.Total.TokIn)
	d.TokOut = int64(td.Total.TokOut)
	d.TokCache = int64(td.Total.TokCache)
	d.Cost = td.Total.Cost
	d.MS = 3000
	for k, v := range td.Models {
		d.Models[k] = deltaModel{
			Requests: int64(v.Req), Err: int64(v.Err), Failover: int64(v.Failover), TokIn: int64(v.TokIn),
			TokOut: int64(v.TokOut), TokCache: int64(v.TokCache), Cost: v.Cost,
		}
	}
	for k, v := range td.Backends {
		d.Backends[k] = deltaBackend{
			Requests: int64(v.Req), Err: int64(v.Err), Failover: int64(v.Failover), TokIn: int64(v.TokIn),
			TokOut: int64(v.TokOut), TokCache: int64(v.TokCache), Cost: v.Cost,
		}
	}
	return d
}

// computeDelta diffs two cumulative /metrics snapshots into a per-tick Delta.
// Costs follow the backend that actually served the request, including fallbacks.
func computeDelta(prev, cur map[string]any) Delta {
	d := Delta{TS: nowMs(), Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
	d.Requests = diffI64(prev, cur, "requests")
	d.Err = diffI64(prev, cur, "err")
	d.Failover = diffI64(prev, cur, "failover")
	d.TokIn = diffI64(prev, cur, "tok_in")
	d.TokOut = diffI64(prev, cur, "tok_out")
	d.TokCache = diffI64(prev, cur, "tok_cache")
	d.MS = 3000
	pm := prev["per_model"].(map[string]any)
	cm := cur["per_model"].(map[string]any)
	for k, cv := range cm {
		cmap := cv.(map[string]any)
		pv, ok := pm[k]
		var pmap map[string]any
		if ok {
			pmap = pv.(map[string]any)
		}
		m := deltaModel{
			Requests: diffMapI64(pmap, cmap, "requests"),
			Err:      diffMapI64(pmap, cmap, "err"),
			Failover: diffMapI64(pmap, cmap, "failover"),
			TokIn:    diffMapI64(pmap, cmap, "tok_in"),
			TokOut:   diffMapI64(pmap, cmap, "tok_out"),
			TokCache: diffMapI64(pmap, cmap, "tok_cache"),
			Cost:     toF64(cmap["cost"]) - toF64(pmap["cost"]),
		}
		if m.Requests > 0 || m.TokIn > 0 || m.TokOut > 0 {
			d.Models[k] = m
		}
	}
	pb := prev["per_backend"].(map[string]any)
	cb := cur["per_backend"].(map[string]any)
	for k, cv := range cb {
		cmap := cv.(map[string]any)
		pv, ok := pb[k]
		var pmap map[string]any
		if ok {
			pmap = pv.(map[string]any)
		}
		b := deltaBackend{
			Requests: diffMapI64(pmap, cmap, "requests"),
			Err:      diffMapI64(pmap, cmap, "err"),
			Failover: diffMapI64(pmap, cmap, "failover"),
			TokIn:    diffMapI64(pmap, cmap, "tok_in"),
			TokOut:   diffMapI64(pmap, cmap, "tok_out"),
			TokCache: diffMapI64(pmap, cmap, "tok_cache"),
			Cost:     toF64(cmap["cost"]) - toF64(pmap["cost"]),
		}
		if b.Requests > 0 || b.Err > 0 {
			d.Backends[k] = b
		}
	}
	for _, m := range d.Models {
		d.Cost += m.Cost
	}
	return d
}

func diffI64(prev, cur map[string]any, key string) int64 {
	var p, c int64
	if pm, ok := prev["global"].(map[string]any); ok {
		p = toI64(pm[key])
	}
	if cm, ok := cur["global"].(map[string]any); ok {
		c = toI64(cm[key])
	}
	return c - p
}

func diffMapI64(pv, cv map[string]any, key string) int64 {
	var p, c int64
	if pv != nil {
		p = toI64(pv[key])
	}
	if cv != nil {
		c = toI64(cv[key])
	}
	return c - p
}

func toI64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	}
	return 0
}

// handleDashState serves the full dashboard state snapshot.
func (s *Server) handleDashState(w http.ResponseWriter, r *http.Request) {
	rt := s.runtime()
	snap := s.metrics.Snapshot(rt.backends)
	ready, _ := s.readiness(r.Context())
	st := dashState{
		Ready:     ready,
		TS:        nowMs(),
		UptimeSec: snap["uptime_sec"].(int64),
		StartedAt: snap["started_at"].(int64),
		Global:    snap["global"].(map[string]any),
		Models:    snap["per_model"].(map[string]any),
		Backends:  snap["per_backend"].(map[string]any),
		Keys:      keysAsMap(snap["key_stats"]),
		Latency:   snap["latency_ms"].(map[string]float64),
		Pricing:   catalog.PricingMap(),
		Version:   Version,
	}
	if st.Keys == nil {
		st.Keys = map[string][]KeyStat{}
	}
	if s.store != nil {
		st.Lifetime.Cost, st.Lifetime.Requests, st.Lifetime.TokIn, st.Lifetime.TokOut, st.Lifetime.TokCache, st.Lifetime.Err = s.store.LifetimeTotals()
		st.Telemetry = s.store.Telemetry()
		st.KeyEvents = s.store.RecentKeyEvents(50)
		st.Alerts = s.store.RecentAlerts(20)
	}
	if st.KeyEvents == nil {
		st.KeyEvents = []KeyEvent{}
	}
	if s.alerts != nil {
		if s.store == nil {
			st.Alerts = s.alerts.Recent(20)
		}
		st.AlertRules = s.alerts.Rules()
	}
	if st.Alerts == nil {
		st.Alerts = []AlertEvent{}
	}
	if st.AlertRules == nil {
		st.AlertRules = []AlertRule{}
	}
	if s.hub != nil {
		st.Subscribers = s.hub.SubCount()
	}
	if s.ticker != nil {
		d := s.ticker.LastDelta()
		models := map[string]any{}
		for k, v := range d.Models {
			models[k] = map[string]any{
				"requests":  v.Requests,
				"tok_in":    v.TokIn,
				"tok_out":   v.TokOut,
				"tok_cache": v.TokCache,
				"cost":      v.Cost,
			}
		}
		st.Delta = map[string]any{
			"d_ms":      d.MS,
			"requests":  d.Requests,
			"err":       d.Err,
			"failover":  d.Failover,
			"tok_in":    d.TokIn,
			"tok_out":   d.TokOut,
			"tok_cache": d.TokCache,
			"cost":      d.Cost,
			"models":    models,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(st)
}

// handleDashSeries serves historical deltas from SQLite.
func (s *Server) handleDashSeries(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r)
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	if s.store == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return
	}
	rows, err := s.store.SeriesSince(r.Context(), since, limit)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		http.Error(w, "Historical analytics unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

func parseSince(r *http.Request) int64 {
	s := r.URL.Query().Get("since")
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// handleDashRange reports the retained data window.
func (s *Server) handleDashRange(w http.ResponseWriter, r *http.Request) {
	var oldest, newest int64
	var count int
	if s.store != nil {
		oldest, newest, count = s.store.Range()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"oldest_ts": oldest,
		"newest_ts": newest,
		"count":     count,
		"now":       nowMs(),
	})
}

// handleDashRecent serves the in-memory recent-request ring.
func (s *Server) handleDashRecent(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	if s.store != nil {
		page, err := s.store.Requests(r.Context(), RequestFilter{Limit: limit})
		if err != nil {
			http.Error(w, "Request history unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(page.Requests)
		return
	}
	if s.recent == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.recent.Snapshot(limit))
}

// handleDashAlerts serves alert history (DB-backed, else in-memory).
func (s *Server) handleDashAlerts(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	var out []AlertEvent
	if s.store != nil {
		out = s.store.RecentAlerts(limit)
	} else if s.alerts != nil {
		out = s.alerts.Recent(limit)
	}
	if out == nil {
		out = []AlertEvent{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleDashKeyEvents serves key-pool transition history.
func (s *Server) handleDashKeyEvents(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	var out []KeyEvent
	if s.store != nil {
		out = s.store.RecentKeyEvents(limit)
	}
	if out == nil {
		out = []KeyEvent{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleDashLive serves the SSE stream.
func (s *Server) handleDashLive(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		http.Error(w, "live stream unavailable", http.StatusServiceUnavailable)
		return
	}
	s.hub.ServeSSE(w, r, func() map[string]any {
		return map[string]any{"type": "init", "ts": nowMs(), "subscribers": s.hub.SubCount()}
	})
}

// handleDashConfig serves a sanitized view of the runtime config (no keys).
func (s *Server) handleDashConfig(w http.ResponseWriter, r *http.Request) {
	rt := s.runtime()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"listen":        rt.cfg.Listen,
		"model_count":   len(rt.cfg.ModelMap),
		"providers":     providerNames(rt.backends),
		"alerts":        s.alertsRules(),
		"recent_cap":    rt.cfg.Dashboard.RecentRequests,
		"auth_required": rt.cfg.ProxyKey != "",
	})
}

// keysAsMap converts the snapshot's key_stats (map[string]any of []KeyStat)
// into the dashboard's typed map.
func keysAsMap(v any) map[string][]KeyStat {
	out := map[string][]KeyStat{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for name, stats := range m {
		if ks, ok := stats.([]KeyStat); ok {
			out[name] = ks
			continue
		}
		// Fall back to re-marshaling (json.Number decoding happens in
		// Snapshot via direct value copies, so this is rarely needed).
		b, err := json.Marshal(stats)
		if err == nil {
			var list []KeyStat
			if json.Unmarshal(b, &list) == nil {
				out[name] = list
			}
		}
	}
	return out
}

func providerNames(backends map[string]*backend) []string {
	out := make([]string, 0, len(backends))
	for n := range backends {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func (s *Server) alertsRules() []AlertRule {
	if s.alerts != nil {
		return s.alerts.Rules()
	}
	return nil
}

func (s *Server) handleDashRequests(w http.ResponseWriter, r *http.Request) {
	f, err := requestFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "Request history unavailable", http.StatusServiceUnavailable)
		return
	}
	page, err := s.store.Requests(r.Context(), f)
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "Request history unavailable", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(page)
}

func (s *Server) handleDashAnalytics(w http.ResponseWriter, r *http.Request) {
	f, err := requestFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.store == nil {
		http.Error(w, "Analytics unavailable", http.StatusServiceUnavailable)
		return
	}
	result, err := s.store.Analytics(r.Context(), f)
	if err != nil {
		if r.Context().Err() == nil {
			http.Error(w, "Analytics unavailable", http.StatusInternalServerError)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) updateTelemetrySettings(cfg *Config) error {
	if s.alerts != nil {
		s.alerts.SetRules(cfg.Dashboard.Alerts)
	}
	// The ticker observes the published runtime retention setting on its next
	// maintenance pass. No database operation can fail after config publication.
	return nil
}
