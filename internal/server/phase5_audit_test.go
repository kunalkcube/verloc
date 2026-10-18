package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

// B3/B4: trusted proxies unlock forwarded client identity; untrusted
// peers can never spoof it. Host allowlist rejects rebinding-style hosts.
func TestTrustedProxyAndHostAllowlist(t *testing.T) {
	s := &Security{
		trustedProxies: parseTrustedProxies([]string{"10.0.0.0/8", "bad-entry"}),
		allowedHosts:   []string{"gateway.lan:8787"},
	}
	// Untrusted peer: XFF ignored.
	r := httptest.NewRequest("POST", "/dash/api/auth/login", nil)
	r.RemoteAddr = "192.0.2.5:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := s.loginAddr(r); got != "192.0.2.5" {
		t.Fatalf("untrusted XFF believed: %s", got)
	}
	if s.requestSecure(r) {
		t.Fatal("untrusted peer marked request secure")
	}
	// Trusted peer: first XFF entry wins; forwarded proto counts.
	r.RemoteAddr = "10.1.2.3:1234"
	if got := s.loginAddr(r); got != "203.0.113.9" {
		t.Fatalf("trusted XFF not used: %s", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if !s.requestSecure(r) {
		t.Fatal("trusted https forward not honoured")
	}
	// Host allowlist.
	r.Host = "evil.example:8787"
	if s.securityOriginOK(r) {
		t.Fatal("non-allowlisted host accepted")
	}
	r.Host = "gateway.lan:8787"
	r.Header.Set("Origin", "https://gateway.lan:8787")
	if !s.securityOriginOK(r) {
		t.Fatal("allowlisted host rejected")
	}
}

// D4: scopes that stop being fed are reclaimed; live scopes keep history.
func TestAggregatorPrunesDeadScopes(t *testing.T) {
	a := NewAggregator(100, 60) // retain 60 minutes
	old := time.Now().Add(-3 * time.Hour).UnixMilli()
	a.Feed(tickAgg{TS: old, Req: 1}, map[string]tickAgg{"gone": {Req: 1}}, nil)
	now := time.Now().UnixMilli()
	a.Feed(tickAgg{TS: now, Req: 1}, map[string]tickAgg{"alive": {Req: 1}}, nil)
	if _, ok := a.byScope["model:gone"]; ok {
		t.Fatal("dead scope was not pruned")
	}
	if _, ok := a.byScope["model:alive"]; !ok {
		t.Fatal("live scope was pruned")
	}
}
