package server

import (
	"log"
	"strconv"
	"sync"
	"time"
)

// tickAgg is one tick of activity for the alert aggregator. P50/P95/P99
// are computed per-tick from the drained latency samples.
type tickAgg struct {
	TS        int64
	Req       int64
	Err       int64
	Failover  int64
	TokIn     int64
	TokOut    int64
	TokCache  int64
	Cost      float64
	LatSumMs  float64
	LatN      int64
	P50Ms     float64
	P95Ms     float64
	P99Ms     float64
	TTFTSumMs float64
	TTFTN     int64
	LatHist   [len(latBuckets) + 1]uint64
	TTFTHist  [len(latBuckets) + 1]uint64
}

func (t tickAgg) LatAvgMs() float64 {
	if t.LatN > 0 {
		return t.LatSumMs / float64(t.LatN)
	}
	return 0
}

func (t tickAgg) TTFTAvgMs() float64 {
	if t.TTFTN > 0 {
		return t.TTFTSumMs / float64(t.TTFTN)
	}
	return 0
}

// add accumulates o into t: counters, both histograms and max TS. It performs
// NO percentile math, so folding many ticks together stays O(buckets) and can be
// done before the distribution is final. Call finalize once afterwards when
// P50/P95/P99 are needed.
func (t *tickAgg) add(o tickAgg) {
	t.Req += o.Req
	t.Err += o.Err
	t.Failover += o.Failover
	t.TokIn += o.TokIn
	t.TokOut += o.TokOut
	t.TokCache += o.TokCache
	t.Cost += o.Cost
	t.LatSumMs += o.LatSumMs
	t.LatN += o.LatN
	t.TTFTSumMs += o.TTFTSumMs
	t.TTFTN += o.TTFTN
	for i, v := range o.LatHist {
		t.LatHist[i] += v
	}
	for i, v := range o.TTFTHist {
		t.TTFTHist[i] += v
	}
	if o.TS > t.TS {
		t.TS = o.TS
	}
}

// finalize derives the latency percentile estimates from the accumulated
// histogram. Call it once after the last add so a merged aggregate reports the
// distribution it actually holds (a zero histogram yields zero percentiles).
func (t *tickAgg) finalize() {
	t.P50Ms = histogramPercentile(t.LatHist, 50)
	t.P95Ms = histogramPercentile(t.LatHist, 95)
	t.P99Ms = histogramPercentile(t.LatHist, 99)
}

// merge accumulates o and immediately derives percentiles. Percentiles are
// order-independent over additive histograms, so this is equivalent to
// add followed by finalize and is kept for ad-hoc callers.
func (t *tickAgg) merge(o tickAgg) {
	t.add(o)
	t.finalize()
}

// Aggregator keeps a rolling view of per-tick activity: a 24h global tick
// ring plus per-scope (backend:<name>, model:<id>) minute buckets, lazily
// created for scopes that have alert rules.
type Aggregator struct {
	mu        sync.Mutex
	global    []tickAgg // ring buffer
	gHead     int
	gFull     bool
	byScope   map[string]*scopeAgg
	maxMinute int // minutes retained
}

func NewAggregator(capTicks, maxMinute int) *Aggregator {
	if capTicks < 1 {
		capTicks = 1
	}
	return &Aggregator{
		global:    make([]tickAgg, capTicks),
		byScope:   map[string]*scopeAgg{},
		maxMinute: maxMinute,
	}
}

type scopeAgg struct {
	minutes map[int64]tickAgg // minute-aligned ts
}

func (a *Aggregator) scope(name string) *scopeAgg {
	s, ok := a.byScope[name]
	if !ok {
		s = &scopeAgg{minutes: map[int64]tickAgg{}}
		a.byScope[name] = s
	}
	return s
}

func (a *Aggregator) minute(s *scopeAgg, minute int64, t tickAgg) {
	t.TS = minute * 60000
	v := s.minutes[minute]
	// Fold the tick into the minute bucket without deriving percentiles: the
	// bucket's histogram is what matters, and SumScope finalizes once after
	// merging all buckets in range.
	v.add(t)
	s.minutes[minute] = v
	if len(s.minutes) > a.maxMinute*2 {
		cutoff := minute - int64(a.maxMinute)
		for k := range s.minutes {
			if k < cutoff {
				delete(s.minutes, k)
			}
		}
	}
}

// Feed stores one tick. models/backends may be empty (nothing happened).
func (a *Aggregator) Feed(t tickAgg, models, backends map[string]tickAgg) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.global[a.gHead] = t
	a.gHead = (a.gHead + 1) % len(a.global)
	if a.gHead == 0 {
		a.gFull = true
	}
	mk := t.TS / 60000
	for name, agg := range models {
		a.minute(a.scope("model:"+name), mk, agg)
	}
	for name, agg := range backends {
		a.minute(a.scope("backend:"+name), mk, agg)
	}
}

// Sum returns the merged activity over the last `sec` seconds (0 = latest tick).
func (a *Aggregator) Sum(sec int) tickAgg {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out tickAgg
	now := time.Now().UnixMilli()
	if sec <= 0 {
		sec = 1
	}
	cutoff := now - int64(sec)*1000
	if a.gFull {
		n := len(a.global)
		for i := 0; i < n; i++ {
			t := a.global[(a.gHead+i)%n]
			if t.TS >= cutoff && t.TS > 0 {
				out.add(t)
			}
		}
	} else {
		for i := 0; i < a.gHead; i++ {
			if a.global[i].TS >= cutoff && a.global[i].TS > 0 {
				out.add(a.global[i])
			}
		}
	}
	out.finalize()
	return out
}

// SumScope merges activity for a scope (backend:<name> / model:<id>) over
// the last `sec` seconds. Returns ok=false when the scope has no data.
func (a *Aggregator) SumScope(scope string, sec int) (tickAgg, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.byScope[scope]
	if !ok {
		return tickAgg{}, false
	}
	cutoffMin := (time.Now().UnixMilli() - int64(sec)*1000) / 60000
	var out tickAgg
	for k, v := range s.minutes {
		if k >= cutoffMin {
			out.add(v)
		}
	}
	out.finalize()
	if out.Req == 0 && out.Err == 0 {
		return tickAgg{}, false
	}
	return out, true
}

// AlertEngine evaluates config rules against the Aggregator.
type AlertEngine struct {
	store    *Store
	hub      *Hub
	rules    []AlertRule
	agg      *Aggregator
	mu       sync.Mutex
	cooldown map[string]time.Time

	// recent is a fixed-size ring buffer of the last recentN alerts. Unlike an
	// append-and-reslice it never regrows and reclaims old entries in place.
	recent     []AlertEvent
	recentN    int
	recentHead int  // next write slot
	recentFull bool // true once the ring has wrapped
}

func NewAlertEngine(store *Store, hub *Hub, rules []AlertRule) *AlertEngine {
	rules = telemetryAlertRules(rules)
	return &AlertEngine{
		store:    store,
		hub:      hub,
		rules:    rules,
		agg:      NewAggregator(28800, 60*24),
		cooldown: map[string]time.Time{},
		recentN:  500,
	}
}

func defaultAlertRules() []AlertRule {
	// Every rule declares a cooldown. These values are load-bearing: a zero
	// cooldown suppresses nothing (see DefaultAlertCooldownSec), so a rule that
	// omits it re-fires on every eval while its condition holds.
	const cd = DefaultAlertCooldownSec
	return []AlertRule{
		{ID: "error-rate-global", Metric: "error_rate", Scope: "global", Threshold: 5, WindowSec: 300, Severity: "warning", CooldownSec: cd},
		{ID: "failover-global", Metric: "failover_rate", Scope: "global", Threshold: 10, WindowSec: 300, Severity: "warning", CooldownSec: cd},
		{ID: "cost-hour-global", Metric: "cost_per_hour", Scope: "global", Threshold: 0.50, WindowSec: 3600, Severity: "info", CooldownSec: cd},
		// Latency is alerted on TIME TO FIRST TOKEN, not total latency.
		//
		// Total latency tracks output size far more than it tracks upstream health:
		// measured on this gateway, decode throughput was a flat ~294-394 tok/s in
		// every latency bucket, while requests over 30s averaged ~12,230 output
		// tokens against ~272 for requests under 5s. So a fixed total-latency
		// threshold fires on every long generation — which is normal, healthy
		// behaviour — and is silent about a genuinely stalled upstream. It cannot
		// be fixed by changing the number; any ms threshold is the wrong shape.
		//
		// Time to first token is the part that measures the upstream rather than
		// the workload, so p95 TTFT >= 15s is the actionable signal.
		{ID: "ttft-p95-global", Metric: "ttft_p95", Scope: "global", Threshold: 15000, WindowSec: 600, Severity: "warning", CooldownSec: cd},
		// Deliberately NOT a default: a total-latency rule, for the reason above.
		// Add one from Settings if you want to watch the end-to-end distribution,
		// but expect it to track generation length rather than gateway health.
		{ID: "keys-cooldown-global", Metric: "keys_in_cooldown", Scope: "global", Threshold: 1, WindowSec: 0, Severity: "warning", CooldownSec: cd},
		{ID: "telemetry-errors", Metric: "telemetry_errors", Scope: "global", Threshold: 1, WindowSec: 0, Severity: "critical", CooldownSec: 300},
		{ID: "telemetry-pressure", Metric: "telemetry_queued", Scope: "global", Threshold: 1536, WindowSec: 0, Severity: "warning", CooldownSec: 300},
	}
}

// DefaultAlertCooldownSec is the suppression window applied to any rule that does
// not specify one. It matches the documented default on AlertRule.CooldownSec and
// the value applyDefaults already uses for user-configured rules.
//
// This matters because a zero cooldown is not "no suppression configured" — it is
// "never suppress", which is how a sustained condition came to re-fire on every
// 5s eval: `now.Sub(last) < 0*second` is always false, so the guard can never
// trip. A rule with no cooldown therefore emits one row (and one log line, and one
// SSE publish) per eval for as long as the condition holds, which measured at
// 720 rows/hour per rule and made the alerts table 22% of the analytics DB.
const DefaultAlertCooldownSec = 900

// Feed forwards a tick into the aggregator.
func (e *AlertEngine) Feed(t tickAgg, models, backends map[string]tickAgg) {
	e.agg.Feed(t, models, backends)
}

// Eval runs all rules once. keyCooldown returns the current count of keys
// in cooldown across all backends.
func (e *AlertEngine) Eval(now time.Time, keyCooldown func() int) {
	rules := e.Rules()
	for i := range rules {
		e.evalRule(&rules[i], now, keyCooldown)
	}
}

func (e *AlertEngine) evalRule(r *AlertRule, now time.Time, keyCooldown func() int) {
	var value float64

	if r.Scope == "" || r.Scope == "global" {
		t := e.agg.Sum(r.WindowSec)
		switch r.Metric {
		case "telemetry_errors":
			if e.store != nil {
				v := e.store.Telemetry()
				value = float64(v.WriteErrors + v.DroppedRequests + v.DroppedKeyEvents)
			}
		case "telemetry_queued":
			if e.store != nil {
				value = float64(e.store.Telemetry().Queued)
			}
		case "error_rate":
			if t.Req > 0 {
				value = float64(t.Err) / float64(t.Req) * 100
			}
		case "failover_rate":
			if t.Req > 0 {
				value = float64(t.Failover) / float64(t.Req) * 100
			}
		case "cost_per_hour":
			hrs := float64(r.WindowSec) / 3600
			if hrs <= 0 {
				hrs = 1
			}
			value = t.Cost / hrs
		case "rps":
			value = float64(t.Req) / windowDivisor(r.WindowSec)
		case "tokens_per_hour":
			value = float64(t.TokIn+t.TokOut) / windowDivisor(r.WindowSec) * 3600
		case "latency_p95":
			if t.LatN > 0 {
				value = t.P95Ms
			}
		case "ttft_p95":
			value = histogramPercentile(t.TTFTHist, 95)
		case "ttft_avg":
			if t.TTFTN > 0 {
				value = t.TTFTAvgMs()
			}
		case "keys_in_cooldown":
			value = float64(keyCooldown())
		}
	} else {
		t, has := e.agg.SumScope(r.Scope, r.WindowSec)
		if !has {
			return
		}
		switch r.Metric {
		case "error_rate":
			if t.Req > 0 {
				value = float64(t.Err) / float64(t.Req) * 100
			}
		case "failover_rate":
			if t.Req > 0 {
				value = float64(t.Failover) / float64(t.Req) * 100
			}
		case "cost_per_hour":
			hrs := float64(r.WindowSec) / 3600
			if hrs <= 0 {
				hrs = 1
			}
			value = t.Cost / hrs
		case "rps":
			value = float64(t.Req) / windowDivisor(r.WindowSec)
		case "tokens_per_hour":
			value = float64(t.TokIn+t.TokOut) / windowDivisor(r.WindowSec) * 3600
		case "latency_p95":
			if t.LatN > 0 {
				value = t.P95Ms
			}
		case "ttft_p95":
			value = histogramPercentile(t.TTFTHist, 95)
		case "ttft_avg":
			if t.TTFTN > 0 {
				value = t.TTFTAvgMs()
			}
		}
	}

	if value < r.Threshold {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if last, seen := e.cooldown[r.ID]; seen && now.Sub(last) < time.Duration(r.CooldownSec)*time.Second {
		return
	}
	e.cooldown[r.ID] = now

	ev := AlertEvent{
		TS:        now.UnixMilli(),
		RuleID:    r.ID,
		Name:      r.Metric + " · " + r.Scope,
		Severity:  r.Severity,
		Scope:     r.Scope,
		Metric:    r.Metric,
		Value:     value,
		Threshold: r.Threshold,
		Message:   alertMessage(r, value),
	}
	e.pushRecent(ev)
	if e.store != nil {
		if err := e.store.InsertAlert(ev); err != nil {
			log.Printf("[alerts] persist: %v", err)
		}
	}
	if e.hub != nil {
		e.hub.Publish(map[string]any{"type": "alert", "alert": ev})
	}
	log.Printf("[alert] %s %s", r.Severity, ev.Message)
}

func alertMessage(r *AlertRule, value float64) string {
	unit := map[string]string{
		"error_rate":       "%",
		"failover_rate":    "%",
		"cost_per_hour":    " $/h",
		"rps":              " req/s",
		"tokens_per_hour":  " tok/h",
		"latency_p95":      " ms",
		"ttft_avg":         " ms",
		"keys_in_cooldown": " keys",
	}[r.Metric]
	scope := r.Scope
	if scope == "" {
		scope = "global"
	}
	return r.Metric + " " + formatNum(value) + unit + " ≥ " + formatNum(r.Threshold) + unit + " over " + scope
}

func formatNum(v float64) string {
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// windowDivisor returns sec as a divisor that is never zero, so rate metrics
// cannot produce Inf/NaN when a rule has a non-positive window. Matches the
// cost_per_hour guard (hrs defaults to 1).
func windowDivisor(sec int) float64 {
	if sec <= 0 {
		return 1
	}
	return float64(sec)
}

// pushRecent writes ev into the fixed-size ring, reclaiming the oldest slot in
// place. Callers must hold e.mu.
func (e *AlertEngine) pushRecent(ev AlertEvent) {
	if e.recentN < 1 {
		return
	}
	if len(e.recent) != e.recentN {
		e.recent = make([]AlertEvent, e.recentN)
	}
	e.recent[e.recentHead] = ev
	e.recentHead = (e.recentHead + 1) % len(e.recent)
	if e.recentHead == 0 {
		e.recentFull = true
	}
}

// Recent returns recent alerts, newest first.
func (e *AlertEngine) Recent(n int) []AlertEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := len(e.recent)
	if !e.recentFull {
		count = e.recentHead
	}
	if n <= 0 || n > count {
		n = count
	}
	out := make([]AlertEvent, 0, n)
	for i := 0; i < n; i++ {
		idx := (e.recentHead - 1 - i + len(e.recent)) % len(e.recent)
		out = append(out, e.recent[idx])
	}
	return out
}

// Rules returns the configured rules.
func (e *AlertEngine) Rules() []AlertRule {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]AlertRule, len(e.rules))
	copy(out, e.rules)
	return out
}

func (e *AlertEngine) SetRules(rules []AlertRule) {
	rules = telemetryAlertRules(rules)
	e.mu.Lock()
	e.rules = rules
	// Drop cooldown state for rule IDs that no longer exist so the map cannot
	// grow without bound across rule edits. Surviving rules keep their cooldown.
	if len(e.cooldown) > 0 {
		live := make(map[string]struct{}, len(rules))
		for i := range rules {
			live[rules[i].ID] = struct{}{}
		}
		for id := range e.cooldown {
			if _, ok := live[id]; !ok {
				delete(e.cooldown, id)
			}
		}
	}
	e.mu.Unlock()
}

// withDefaultCooldown returns rules with a cooldown applied to every rule that
// does not specify one, so no construction path can yield a rule that re-fires on
// every eval. It runs on a copy and never mutates the caller's slice.
//
// Equivalent normalisation lives in applyDefaults for user-configured rules and
// in the admin write path for rule edits; this is the single place that also
// covers the built-in fallback rules and anything constructed directly.
func withDefaultCooldown(rules []AlertRule) []AlertRule {
	out := append([]AlertRule(nil), rules...)
	for i := range out {
		if out[i].CooldownSec == 0 {
			out[i].CooldownSec = DefaultAlertCooldownSec
		}
	}
	return out
}

func telemetryAlertRules(rules []AlertRule) []AlertRule {
	if len(rules) == 0 {
		return withDefaultCooldown(defaultAlertRules())
	}
	out := append([]AlertRule(nil), rules...)
	for _, rule := range defaultAlertRules() {
		if rule.Metric != "telemetry_errors" && rule.Metric != "telemetry_queued" {
			continue
		}
		found := false
		for _, configured := range out {
			if configured.Metric == rule.Metric {
				found = true
				break
			}
		}
		if !found {
			out = append(out, rule)
		}
	}
	return withDefaultCooldown(out)
}
