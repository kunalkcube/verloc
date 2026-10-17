package server

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// ReqRecord is metadata for one logical request, never prompt or response content.
type ReqRecord struct {
	TS          int64            `json:"ts"`       // unix millis
	ID          string           `json:"id"`       // short hex id for the UI
	Model       string           `json:"model"`    // agent-facing model id
	Upstream    string           `json:"upstream"` // upstream model id
	Backend     string           `json:"backend"`
	Key         string           `json:"key"` // masked
	IP          string           `json:"ip,omitempty"`
	UA          string           `json:"ua,omitempty"`
	Stream      bool             `json:"stream"`
	OK          bool             `json:"ok"`
	Status      int              `json:"status"`
	Failover    int              `json:"failover"` // attempts taken
	LatMs       float64          `json:"lat_ms"`
	TTFTMs      float64          `json:"ttft_ms,omitempty"` // time to first byte, streaming only
	TokIn       uint64           `json:"tok_in"`
	TokOut      uint64           `json:"tok_out"`
	TokCache    uint64           `json:"tok_cache"`
	Cost        float64          `json:"cost"`
	ErrMsg      string           `json:"err_msg,omitempty"`
	ClientKeyID string           `json:"client_key_id,omitempty"`
	ClientName  string           `json:"client_name,omitempty"`
	Outcome     string           `json:"outcome"`
	UsageKnown  bool             `json:"usage_known"`
	CostKnown   bool             `json:"cost_known"`
	AttemptLog  []RequestAttempt `json:"attempts,omitempty"`
}

type RequestAttempt struct {
	Backend  string  `json:"backend"`
	Upstream string  `json:"upstream"`
	Status   int     `json:"status"`
	Outcome  string  `json:"outcome"`
	LatMs    float64 `json:"lat_ms"`
}

// sanitizeRecord is shared by persistence, the recent ring, and live events.
// Error text from upstreams may contain credentials or submitted content.
func sanitizeRecord(r ReqRecord) ReqRecord {
	r.ErrMsg = ""
	r.IP, r.UA, r.Key = "", "", ""
	if !r.OK {
		r.ErrMsg = "Request did not complete successfully"
	}
	r.Outcome = safeOutcome(r.Outcome, r.OK)
	r.AttemptLog = append([]RequestAttempt(nil), r.AttemptLog...)
	for i := range r.AttemptLog {
		a := &r.AttemptLog[i]
		a.Outcome = safeOutcome(a.Outcome, a.Status >= 200 && a.Status < 300)
	}
	if !r.UsageKnown {
		r.TokIn, r.TokOut, r.TokCache = 0, 0, 0
		r.CostKnown = false
	}
	if !r.CostKnown {
		r.Cost = 0
	}
	return r
}

func safeOutcome(v string, ok bool) string {
	switch v {
	case "completed", "error", "canceled", "interrupted", "rejected", "timeout", "upstream_error", "client_error", "success", "idle_timeout", "invalid_request", "body_too_large", "unavailable", "model_unavailable", "provider_disabled", "policy_rejected", "overloaded", "upstream_rejected", "retry_budget_exhausted":
		return v
	}
	if ok {
		return "completed"
	}
	return "error"
}

// RecentLog is a bounded ring buffer of the last N requests. Safe for
// concurrent use; the proxy appends, the dashboard reads snapshots.
type RecentLog struct {
	mu   sync.RWMutex
	buf  []ReqRecord
	head int // next write slot
	full bool
}

func NewRecentLog(n int) *RecentLog {
	if n < 1 {
		n = 1
	}
	return &RecentLog{buf: make([]ReqRecord, n)}
}

func (l *RecentLog) Add(r ReqRecord) {
	r = sanitizeRecord(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.ID == "" {
		r.ID = shortID()
	}
	l.buf[l.head] = r
	l.head = (l.head + 1) % len(l.buf)
	if l.head == 0 {
		l.full = true
	}
}

// Snapshot returns up to n most-recent records, newest first.
func (l *RecentLog) Snapshot(n int) []ReqRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || n > len(l.buf) {
		n = len(l.buf)
	}
	out := make([]ReqRecord, 0, n)
	start := l.head
	if l.full {
		// walk backwards from the newest entry
		for i := 0; i < n; i++ {
			idx := (start - 1 - i + len(l.buf)) % len(l.buf)
			out = append(out, l.buf[idx])
		}
	} else {
		for i := start - 1; i >= 0 && len(out) < n; i-- {
			out = append(out, l.buf[i])
		}
	}
	return out
}

// Since returns records with ts > sinceMs, oldest first (for SSE live tail).
func (l *RecentLog) Since(sinceMs int64) []ReqRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var out []ReqRecord
	if l.full {
		for i := 0; i < len(l.buf); i++ {
			r := l.buf[(l.head+i)%len(l.buf)]
			if r.TS > sinceMs && r.TS > 0 {
				out = append(out, r)
			}
		}
	} else {
		for i := 0; i < l.head; i++ {
			if l.buf[i].TS > sinceMs {
				out = append(out, l.buf[i])
			}
		}
	}
	return out
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// AlertEvent is one triggered alert.
type AlertEvent struct {
	TS        int64   `json:"ts"`
	RuleID    string  `json:"rule_id"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Scope     string  `json:"scope"`
	Metric    string  `json:"metric"`
	Value     float64 `json:"value"`
	Threshold float64 `json:"threshold"`
	Message   string  `json:"message"`
}

// KeyEvent is a key-pool transition: fail, cooldown or recover.
type KeyEvent struct {
	TS      int64  `json:"ts"`
	Backend string `json:"backend"`
	Key     string `json:"key"`
	Event   string `json:"event"`
	Status  int    `json:"status"`
	Msg     string `json:"msg"`
}

func nowMs() int64 { return time.Now().UnixMilli() }
