package server

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestConstantTimeTokenEqual covers the CSRF comparison, the only token compare
// whose input length is attacker controlled. Equal tokens must match; unequal
// tokens (including unequal lengths) must be rejected without panicking.
func TestConstantTimeTokenEqual(t *testing.T) {
	if !constantTimeTokenEqual("abc", "abc") {
		t.Fatal("equal tokens rejected")
	}
	if constantTimeTokenEqual("abc", "abd") {
		t.Fatal("differing tokens accepted")
	}
	if constantTimeTokenEqual("", "abc") || constantTimeTokenEqual(strings.Repeat("a", 43), "a") {
		t.Fatal("unequal-length tokens accepted")
	}
}

// TestSessionCurrentTracksRotation verifies the cheap post-authMu re-check used
// by handleAuthPassword: it reports a live session current and a rotated-away
// (deleted) session as gone.
func TestSessionCurrentTracksRotation(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, _ := e.seedSession(t)
	hash := sha256.Sum256([]byte(cookie.Value))
	r := httptest.NewRequest("POST", "/owner-test", nil)
	r.AddCookie(cookie)
	r.RemoteAddr = "192.0.2.1:1234"
	if !e.security.sessionCurrent(r, hash) {
		t.Fatal("live session reported absent")
	}
	if _, err := e.store.db.Exec("DELETE FROM admin_sessions"); err != nil {
		t.Fatal(err)
	}
	if e.security.sessionCurrent(r, hash) {
		t.Fatal("rotated session reported current")
	}
}

// TestLoginThrottleExactLimitAndRetryAfter keeps the hardened throttle contract:
// eight attempts per window are allowed, the ninth gets 429 with Retry-After 900.
func TestLoginThrottleExactLimitAndRetryAfter(t *testing.T) {
	e := newSecurityTestEnv(t)
	for i := range loginAttemptLimit {
		if w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, ""); w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, "")
	if w.Code != 429 || w.Header().Get("Retry-After") != loginRetryAfter {
		t.Fatalf("throttle: %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}

// TestLoginThrottleReleasesAfterWindow ensures a throttled IP is released as soon
// as its window lapses, even when the bounded periodic sweep is suppressed.
func TestLoginThrottleReleasesAfterWindow(t *testing.T) {
	e := newSecurityTestEnv(t)
	for range loginAttemptLimit {
		e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, "")
	}
	if w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, ""); w.Code != 429 {
		t.Fatalf("not throttled: %d", w.Code)
	}
	sec := e.security
	sec.loginMu.Lock()
	a := sec.logins["192.0.2.1"]
	a.start = time.Now().Add(-2 * loginWindow)
	sec.logins["192.0.2.1"] = a
	sec.loginSweepAt = time.Now() // suppress the periodic sweep
	sec.loginMu.Unlock()
	if w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, ""); w.Code == 429 {
		t.Fatalf("throttle not released after window: %d", w.Code)
	}
}

// TestLoginSweepBoundedPerCall verifies the non-full sweep deletes at most
// loginSweepLimit entries per call.
func TestLoginSweepBoundedPerCall(t *testing.T) {
	e := newSecurityTestEnv(t)
	sec := e.security
	now := time.Now()
	stale := 200
	sec.loginMu.Lock()
	for i := range stale {
		sec.logins[fmt.Sprintf("203.0.113.%d", i)] = loginAttempts{start: now.Add(-2 * loginWindow), count: 1}
	}
	sec.loginSweepAt = time.Time{}
	sec.loginMu.Unlock()

	e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, "")

	sec.loginMu.Lock()
	n := len(sec.logins)
	sec.loginMu.Unlock()
	// Removed at most loginSweepLimit stale entries, then the caller's own fresh
	// entry was inserted.
	if n > stale-loginSweepLimit+1 {
		t.Fatalf("sweep removed too many in one call: %d", stale-n)
	}
	if n >= stale {
		t.Fatalf("sweep removed nothing: %d", n)
	}
}

// TestLoginSweepErrsOnSideOfMemoryCap verifies the 4096 cap still bounds memory
// without falsely rejecting an unthrottled IP: when the map is saturated with
// elapsed entries the cap path evicts them before the caller's cap check.
func TestLoginSweepErrsOnSideOfMemoryCap(t *testing.T) {
	e := newSecurityTestEnv(t)
	sec := e.security
	now := time.Now()
	sec.loginMu.Lock()
	for i := range loginMaxEntries {
		sec.logins[fmt.Sprintf("198.51.100.%d", i)] = loginAttempts{start: now.Add(-2 * loginWindow), count: 1}
	}
	// Mark a recent sweep so the time-gated path would otherwise skip cleaning.
	sec.loginSweepAt = now
	sec.loginMu.Unlock()

	w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, "")
	if w.Code == 429 {
		t.Fatalf("stale-saturated map rejected a fresh IP: %d", w.Code)
	}
	sec.loginMu.Lock()
	n := len(sec.logins)
	sec.loginMu.Unlock()
	if n >= loginMaxEntries {
		t.Fatalf("sweep did not free capacity: %d", n)
	}
}

// TestLoginSweepKeepsActiveThrottles verifies the cap path never evicts an active
// throttle: a genuinely saturated active map still returns 429.
func TestLoginSweepKeepsActiveThrottles(t *testing.T) {
	e := newSecurityTestEnv(t)
	sec := e.security
	now := time.Now()
	sec.loginMu.Lock()
	for i := range loginMaxEntries {
		sec.logins[fmt.Sprintf("198.51.100.%d", i)] = loginAttempts{start: now, count: 1}
	}
	sec.loginSweepAt = now
	sec.loginMu.Unlock()

	w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "x"}, nil, "")
	if w.Code != 429 || w.Header().Get("Retry-After") != loginRetryAfter {
		t.Fatalf("active-saturated map: %d retry=%q", w.Code, w.Header().Get("Retry-After"))
	}
}
