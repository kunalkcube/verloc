package server

import (
	"net/http/httptest"
	"testing"
)

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
