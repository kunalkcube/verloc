package server

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type KeyState struct {
	ID            string
	Key           string
	AccountID     string       // optional; substituted into base_url (%ACCOUNT%)
	masked        string       // precomputed maskKey(Key); immutable, safe to read concurrently
	cooldownUntil atomic.Int64 // unix nano; 0 = healthy
	failCount     atomic.Int32
	successStreak atomic.Int32 // consecutive successes since the last failure
	requests      atomic.Uint64
	ok            atomic.Uint64
	err           atomic.Uint64
	failover      atomic.Uint64
	tokIn         atomic.Uint64
	tokOut        atomic.Uint64
	tokCache      atomic.Uint64
	costBits      atomic.Uint64 // float64 bits
}

func (k *KeyState) addCost(c float64) {
	for {
		old := k.costBits.Load()
		if k.costBits.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+c)) {
			return
		}
	}
}

func (k *KeyState) cost() float64 { return math.Float64frombits(k.costBits.Load()) }

// KeyEventHook is called on key-pool transitions (fail → cooldown, recover).
type KeyEventHook func(key string, event string, status int, msg string)

type KeyPool struct {
	mu       sync.Mutex
	states   []*KeyState
	rr       atomic.Uint64
	cooldown time.Duration
	hookMu   sync.RWMutex
	hook     KeyEventHook
}

func NewKeyPool(keys []KeyEntry, cooldown time.Duration) *KeyPool {
	states := make([]*KeyState, len(keys))
	for i, k := range keys {
		states[i] = &KeyState{ID: k.ID, Key: k.Key, AccountID: k.AccountID, masked: maskKey(k.Key)}
	}
	return &KeyPool{states: states, cooldown: cooldown}
}

func (p *KeyPool) SetEventHook(h KeyEventHook) {
	p.hookMu.Lock()
	p.hook = h
	p.hookMu.Unlock()
}

func (p *KeyPool) emit(key, event string, status int, msg string) {
	p.hookMu.RLock()
	h := p.hook
	p.hookMu.RUnlock()
	if h != nil {
		h(key, event, status, msg)
	}
}

// NextUntried scans the immutable pool once without building a healthy slice.
// Duplicate credentials are excluded by account + key, not just pool position.
func (p *KeyPool) NextUntried(tried map[credentialKey]bool) *KeyState {
	n := len(p.states)
	if n == 0 {
		return nil
	}
	start := int((p.rr.Add(1) - 1) % uint64(n))
	now := time.Now().UnixNano()
	for i := range n {
		s := p.states[(start+i)%n]
		if s.cooldownUntil.Load() <= now && !tried[keyIdentity(s)] {
			return s
		}
	}
	return nil
}

type credentialKey struct{ account, key string }

func keyIdentity(s *KeyState) credentialKey { return credentialKey{s.AccountID, s.Key} }

func (p *KeyPool) Next() *KeyState { return p.NextUntried(nil) }

// MarkFailure puts the key into cooldown. If override > 0 it is used
// (e.g. parsed from a Retry-After header); otherwise cooldown grows with
// consecutive failures, capped at 5 minutes.
func (p *KeyPool) MarkFailure(s *KeyState, override time.Duration) {
	s.successStreak.Store(0)
	fc := s.failCount.Add(1)
	var d time.Duration
	if override > 0 {
		d = override
	} else {
		d = p.cooldown * time.Duration(min(fc, 3600))
		if d > 5*time.Minute {
			d = 5 * time.Minute
		}
	}
	if d > time.Hour {
		d = time.Hour
	}
	s.cooldownUntil.Store(time.Now().Add(d).UnixNano())
	p.emit(s.masked, "cooldown", 0, d.String())
}

// recoverSuccesses is how many consecutive successes a previously-failing
// key needs before its failure history is forgiven. One success used to
// zero failCount immediately, so a flapping key oscillated forever and
// never earned a longer cooldown — it kept re-hammering a struggling
// upstream at the minimum backoff.
const recoverSuccesses = 2

func (p *KeyPool) MarkSuccess(s *KeyState) {
	if s.failCount.Load() > 0 {
		if s.successStreak.Add(1) < recoverSuccesses {
			return // not yet trusted again; keep the failure history
		}
		s.successStreak.Store(0)
		s.failCount.Store(0)
		s.cooldownUntil.Store(0)
		p.emit(s.masked, "recover", 0, "")
	} else {
		s.successStreak.Store(0)
		s.failCount.Store(0)
		s.cooldownUntil.Store(0)
	}
}

// RecordSuccess attributes a completed request to the key. Marks the key
// healthy and accumulates usage + cost for the Keys dashboard.
func (p *KeyPool) RecordSuccess(s *KeyState, tin, tout, tcache uint64, cost float64) {
	p.MarkSuccess(s)
	s.requests.Add(1)
	s.ok.Add(1)
	if tin > 0 {
		s.tokIn.Add(tin)
	}
	if tout > 0 {
		s.tokOut.Add(tout)
	}
	if tcache > 0 {
		s.tokCache.Add(tcache)
	}
	if cost > 0 {
		s.addCost(cost)
	}
}

// RecordErr attributes a failed request to the key.
func (p *KeyPool) RecordErr(s *KeyState) {
	s.requests.Add(1)
	s.err.Add(1)
}

// RecordFailover bumps the key's failover counter (retry on another key).
func (p *KeyPool) RecordFailover(s *KeyState) {
	s.failover.Add(1)
}

func (p *KeyPool) Count() int { return len(p.states) }

// Stats returns a snapshot for health/debug endpoints.
//
// The returned slice is owned by the caller: it is retained by Snapshot's
// output map, and concurrent Snapshot calls (two dashboard polls, or a poll
// plus the ticker) would alias a shared buffer. Only the per-entry masked key
// is cached on the state, which removes the repeated maskKey work without
// handing out shared memory.
func (p *KeyPool) Stats() []KeyStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixNano()
	out := make([]KeyStat, 0, len(p.states))
	for _, s := range p.states {
		until := s.cooldownUntil.Load()
		out = append(out, KeyStat{
			Key:         s.masked,
			Healthy:     until <= now,
			FailCount:   s.failCount.Load(),
			CooldownSec: max(0, (until-now)/int64(time.Second)),
			Requests:    s.requests.Load(),
			OK:          s.ok.Load(),
			Err:         s.err.Load(),
			Failover:    s.failover.Load(),
			TokIn:       s.tokIn.Load(),
			TokOut:      s.tokOut.Load(),
			TokCache:    s.tokCache.Load(),
			Cost:        s.cost(),
		})
	}
	return out
}

// InCooldown counts keys currently cooling down (for alert rules).
func (p *KeyPool) InCooldown() int {
	now := time.Now().UnixNano()
	n := 0
	for _, s := range p.states {
		if s.cooldownUntil.Load() > now {
			n++
		}
	}
	return n
}

type KeyStat struct {
	Key         string  `json:"key"`
	Healthy     bool    `json:"healthy"`
	FailCount   int32   `json:"fail_count"`
	CooldownSec int64   `json:"cooldown_sec"`
	Requests    uint64  `json:"requests"`
	OK          uint64  `json:"ok"`
	Err         uint64  `json:"err"`
	Failover    uint64  `json:"failover"`
	TokIn       uint64  `json:"tok_in"`
	TokOut      uint64  `json:"tok_out"`
	TokCache    uint64  `json:"tok_cache"`
	Cost        float64 `json:"cost"`
}
