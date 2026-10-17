package server

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestMergePercentilesUsesDistribution(t *testing.T) {
	a := tickAgg{TS: time.Now().UnixMilli(), Req: 10, LatN: 10, LatSumMs: 1000}
	a.LatHist[bucketOf(100)] = 10
	b := tickAgg{TS: a.TS + 3000, Req: 90, LatN: 90, LatSumMs: 900000}
	b.LatHist[bucketOf(10000)] = 90
	var out tickAgg
	out.merge(a)
	out.merge(b)
	if out.P50Ms != 10000 || out.P95Ms != 10000 || out.P99Ms != 10000 {
		t.Fatalf("merged percentiles must describe combined distribution: %+v", out)
	}
	if out.Req != 100 || out.LatN != 100 || math.Abs(out.LatAvgMs()-9010) > 1e-9 {
		t.Fatalf("counters not merged: %+v", out)
	}
	var empty tickAgg
	empty.merge(tickAgg{})
	if math.IsNaN(empty.P95Ms) || empty.P95Ms != 0 {
		t.Fatal("empty distribution must be zero")
	}
}

func TestBackendCostAttribution(t *testing.T) {
	catalog.pricing["test/alpha"] = pricingPer1M{Input: 1, Output: 2, CacheRead: 0.5}
	defer delete(catalog.pricing, "test/alpha")
	mt := NewMetrics()
	prev := mt.Snapshot(nil)
	// The same logical model was actually served by two different providers.
	mt.Record2("test/alpha", "primary", false, 200, 800, 300, 0, 1000000, 0, true)
	mt.Record2("test/alpha", "fallback", true, 200, 200, 200, 0, 2000000, 1000000, false)
	d := computeDelta(prev, mt.Snapshot(nil))
	if math.Abs(d.Cost-0.002) > 1e-9 {
		t.Fatalf("known interrupted usage must reconcile: %v", d.Cost)
	}
	if math.Abs(d.Backends["primary"].Cost-0.0014) > 1e-9 || math.Abs(d.Backends["fallback"].Cost-0.0006) > 1e-9 {
		t.Fatalf("provider attribution follows actual traffic: %+v", d.Backends)
	}
}

func TestDashboardReadinessTracksGatewayAvailability(t *testing.T) {
	srv, _ := testAdminServer(t)
	var err error
	srv.store, err = OpenStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.store.Close() })

	check := func(want bool) {
		t.Helper()
		for _, path := range []string{"/readyz", "/dash/api/state"} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			authorizeAdminTest(t, srv, req)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			wantStatus := http.StatusOK
			if path == "/readyz" && !want {
				wantStatus = http.StatusServiceUnavailable
			}
			var body struct {
				Ready bool `json:"ready"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if rec.Code != wantStatus || body.Ready != want {
				t.Fatalf("%s: status=%d ready=%v; want status=%d ready=%v", path, rec.Code, body.Ready, wantStatus, want)
			}
		}
	}
	check(false) // A configured model is insufficient without an owner.
	body, err := json.Marshal(map[string]string{
		"setup_token": srv.security.options.SetupToken,
		"password":    "readiness test owner password",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/dash/api/auth/setup", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner setup: %d %s", rec.Code, rec.Body.String())
	}
	check(true)

	apply := func(change func(*Config)) {
		t.Helper()
		cfg, err := cloneConfig(srv.runtime().cfg)
		if err != nil {
			t.Fatal(err)
		}
		change(cfg)
		if err := srv.applyLive(cfg); err != nil {
			t.Fatal(err)
		}
	}
	apply(func(cfg *Config) { cfg.DisabledModels["mock/a"] = true })
	check(false)
	apply(func(cfg *Config) {
		delete(cfg.DisabledModels, "mock/a")
		cfg.Providers["mock"].Disabled = true
	})
	check(false)
	apply(func(cfg *Config) {
		cfg.Providers["fallback"] = &ProviderConfig{BaseURL: "http://127.0.0.1:9/v1", NoAuth: true}
		cfg.ModelRoutes["mock/a"] = []RouteTarget{{Backend: "fallback", Upstream: "a"}}
	})
	check(true)
	srv.shuttingDown.Store(true)
	check(false)
	srv.shuttingDown.Store(false)
	apply(func(cfg *Config) {
		delete(cfg.ModelMap, "mock/a")
		delete(cfg.ModelRoutes, "mock/a")
	})
	check(false)
}
