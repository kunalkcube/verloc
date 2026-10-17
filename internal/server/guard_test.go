package server

// Guards for the SSRF probe block, model-id round-tripping, and the login
// throttle's failure-only semantics.

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestProbeGuardBlocksPrivateAddresses covers the SSRF defence: the probe must
// refuse loopback / link-local / private / metadata targets unless the caller
// opts in, while still allowing ordinary public hosts.
func TestProbeGuardBlocksPrivateAddresses(t *testing.T) {
	blocked := []string{
		"http://127.0.0.1:11434/v1",
		"http://localhost:8080/v1",
		"http://169.254.169.254/latest/meta-data", // cloud metadata
		"http://10.0.0.5/v1",
		"http://192.168.1.10/v1",
		"http://172.16.4.4/v1",
		"http://[::1]:8080/v1",
		"http://0.0.0.0:8080/v1",
	}
	for _, u := range blocked {
		if err := guardProbeHost(context.Background(), u, false); err == nil {
			t.Errorf("guard allowed private target %s", u)
		}
		// The opt-in is what makes a legitimately local upstream usable.
		if err := guardProbeHost(context.Background(), u, true); err != nil {
			t.Errorf("guard blocked %s even with allow_private_upstreams: %v", u, err)
		}
	}
	if err := guardProbeHost(context.Background(), "https://api.example.com/v1", false); err != nil {
		t.Errorf("guard blocked a public host: %v", err)
	}
}

// TestProbeGuardIsEnforcedOnDiscover proves the guard is actually wired into the
// request path, not just available as a helper.
func TestProbeGuardIsEnforcedOnDiscover(t *testing.T) {
	srv, _ := testAdminServer(t)
	body := strings.NewReader(`{"base_url":"http://169.254.169.254/latest","api_key":"k"}`)
	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/discover", body)
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminDiscover(rec, req)
	if rec.Code != 400 {
		t.Fatalf("discover against a metadata address returned %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "private or local address") {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}

// TestModelIDRejectsUnroutableSegments covers the wrong-model-delete bug: a
// "."/".."/empty segment cannot survive the router's path cleaning, so such an
// id would address a different model on DELETE/PUT.
func TestModelIDRejectsUnroutableSegments(t *testing.T) {
	bad := []string{"nv/../y", "nv/./y", "nv//y", "../y", "./y", "/y", "nv/.hidden"}
	for _, id := range bad {
		if safeModelID(id) {
			t.Errorf("safeModelID accepted %q", id)
		}
	}
	good := []string{"nv/deepseek-ai/deepseek-v4-flash", "a", "a/b", "a-b_c.d/e"}
	for _, id := range good {
		if !safeModelID(id) {
			t.Errorf("safeModelID rejected legitimate id %q", id)
		}
	}
}

// TestModelPricingRejectsNonFinite proves NaN/Inf cannot be persisted: NaN
// propagates through every SUM it touches and compares false against every
// threshold, so a single poisoned price would corrupt aggregates silently.
func TestModelPricingRejectsNonFinite(t *testing.T) {
	cfg := &Config{Providers: map[string]*ProviderConfig{"p": {BaseURL: "https://x/v1", Keys: []KeyEntry{{Key: "k"}}}},
		ModelMap: map[string]string{}, ModelMeta: map[string]*ModelMeta{}, ModelRoutes: map[string][]RouteTarget{},
		DisabledModels: map[string]bool{}}
	nan := math.NaN()
	inf := math.Inf(1)
	for _, tc := range []struct {
		name string
		in   *float64
	}{{"nan", &nan}, {"inf", &inf}} {
		err := applyModelRequest(cfg, modelUpsertReq{ID: "m", Backend: "p", Upstream: "u", PricingInput: tc.in}, false)
		if err == nil {
			t.Errorf("%s pricing accepted", tc.name)
		}
	}
	// Absent pricing stays legitimate ("unknown", distinct from free).
	if err := applyModelRequest(cfg, modelUpsertReq{ID: "m2", Backend: "p", Upstream: "u"}, false); err != nil {
		t.Errorf("absent pricing rejected: %v", err)
	}
}
