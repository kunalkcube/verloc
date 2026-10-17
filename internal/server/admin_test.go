package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testAdminServer(t *testing.T) (*Server, string) {
	t.Helper()
	e := newSecurityTestEnv(t)
	cfg := &Config{
		Listen: ":0", DefaultProvider: "mock",
		ModelMap:  map[string]string{"mock/a": "mock:a"},
		Providers: map[string]*ProviderConfig{"mock": {BaseURL: "http://127.0.0.1:9/v1", Keys: []KeyEntry{{Key: "sk-test-key-aaaa"}}}},
	}
	cfg.applyDefaults()
	cfg, err := e.store.LoadOrImportConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.control, srv.security = e.store, e.security
	empty := cachedIndex{at: time.Now(), idx: indexCatalog(nil)}
	srv.hints = &hintResolver{vercel: empty, or: empty, md: empty, litellm: empty}
	return srv, e.dbPath
}

func authorizeAdminTest(t *testing.T, srv *Server, req *http.Request) {
	t.Helper()
	if srv.security == nil {
		e := newSecurityTestEnv(t)
		srv.control, srv.security = e.store, e.security
	}
	e := &securityTestEnv{store: srv.control}
	cookie, csrf := e.seedSession(t)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
}

func TestAdminAddProviderAndModel(t *testing.T) {
	srv, path := testAdminServer(t)

	body := `{
		"name":"acme",
		"base_url":"https://api.acme.test/v1",
		"keys":["sk-acme-11111111"]
	}`
	req := httptest.NewRequest(http.MethodPut, "/dash/api/admin/providers", bytes.NewBufferString(body))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminProviderPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("add provider: %d %s", rec.Code, rec.Body.String())
	}

	mbody := `{
		"id":"ac/widget",
		"backend":"acme",
		"upstream":"widget-v1",
		"name":"Widget",
		"context_window":128000,
		"max_tokens":8192,
		"pricing_input":0.5,
		"pricing_output":1.5,
		"pricing_cache":0.05
	}`
	req = httptest.NewRequest(http.MethodPut, "/dash/api/admin/models", bytes.NewBufferString(mbody))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminModelPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("add model: %d %s", rec.Code, rec.Body.String())
	}

	rt := srv.runtime()
	if _, ok := rt.backends["acme"]; !ok {
		t.Fatal("acme backend missing after apply")
	}
	if rt.cfg.ModelMap["ac/widget"] != "acme:widget-v1" {
		t.Fatalf("model_map=%v", rt.cfg.ModelMap["ac/widget"])
	}
	m := catalog.Lookup("ac/widget")
	if m == nil || m.PricingPerMillion.Input != 0.5 {
		t.Fatalf("catalog overlay missing: %+v", m)
	}
	cost := costOf("ac/widget", 1_000_000, 0, 0)
	if cost < 0.49 || cost > 0.51 {
		t.Fatalf("cost=%v want ~0.5", cost)
	}

	reopened, err := OpenControlStore(path, filepath.Join(filepath.Dir(path), "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	disk, err := reopened.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if disk.Providers["acme"] == nil || disk.ModelMap["ac/widget"] != "acme:widget-v1" || disk.ModelMeta["ac/widget"].PricingInput != 0.5 {
		t.Fatal("management mutation did not survive a new database connection")
	}
}

func TestAdminDeleteProviderBlockedByModel(t *testing.T) {
	srv, _ := testAdminServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/dash/api/admin/providers/mock", nil)
	req.SetPathValue("name", "mock")
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminProviderDelete(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminAuthRequired(t *testing.T) {
	srv, _ := testAdminServer(t)
	// The retired shared proxy key must not grant administrator access.
	req := httptest.NewRequest(http.MethodGet, "/dash/api/admin/providers", nil)
	rec := httptest.NewRecorder()
	srv.handleAdminProvidersList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	srv.handleAdminProvidersList(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("shared proxy key accepted: %d", rec.Code)
	}
	req.Header.Del("Authorization")
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminProvidersList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner session rejected: %d %s", rec.Code, rec.Body.String())
	}
}

func TestOnboardingIsAtomicAndRequiresExplicitOverwrite(t *testing.T) {
	srv, _ := testAdminServer(t)
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/onboard", bytes.NewBufferString(body))
		authorizeAdminTest(t, srv, req)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	bad := request(`{"provider":{"name":"new","base_url":"https://new.example/v1","no_auth":true},"models":[{"id":"new/valid","backend":"new","upstream":"valid"},{"id":"new/invalid","backend":"missing","upstream":"invalid"}]}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch: %d %s", bad.Code, bad.Body.String())
	}
	reloaded, err := srv.control.LoadOrImportConfig(srv.runtime().cfg)
	if err != nil {
		t.Fatal(err)
	}
	if srv.runtime().cfg.Providers["new"] != nil || reloaded.Providers["new"] != nil || reloaded.ModelMap["new/valid"] != "" {
		t.Fatal("failed onboarding leaked a partial provider or model")
	}
	conflict := request(`{"provider":{"name":"new","base_url":"https://new.example/v1","no_auth":true},"models":[{"id":"mock/a","backend":"new","upstream":"replacement"}]}`)
	if conflict.Code != http.StatusConflict || srv.runtime().cfg.Providers["new"] != nil {
		t.Fatalf("unconfirmed overwrite mutated state: %d", conflict.Code)
	}
	confirmed := request(`{"overwrite":true,"provider":{"name":"new","base_url":"https://new.example/v1","no_auth":true},"models":[{"id":"mock/a","backend":"new","upstream":"replacement"}]}`)
	if confirmed.Code != http.StatusOK || srv.runtime().cfg.ModelMap["mock/a"] != "new:replacement" {
		t.Fatalf("confirmed remap failed: %d %s", confirmed.Code, confirmed.Body.String())
	}
}

func TestUnknownPricingAndClientCatalogScope(t *testing.T) {
	srv, _ := testAdminServer(t)
	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/onboard", bytes.NewBufferString(`{"models":[{"id":"mock/unknown","backend":"mock","upstream":"unknown"},{"id":"mock/free","backend":"mock","upstream":"free","pricing_input":0,"pricing_output":0}]}`))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("onboard: %d %s", rec.Code, rec.Body.String())
	}
	if srv.modelView("mock/unknown", srv.runtime().cfg).PricingKnown || !srv.modelView("mock/free", srv.runtime().cfg).PricingKnown {
		t.Fatal("missing pricing was conflated with explicitly free pricing")
	}
	req = httptest.NewRequest(http.MethodPost, "/dash/api/admin/client-keys", bytes.NewBufferString(`{"name":"scoped agent","allowed_models":["mock/free"]}`))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create client key: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var listed struct {
		Data []modelInfo `json:"data"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("list models: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 || listed.Data[0].ID != "mock/free" || !listed.Data[0].PricingKnown {
		t.Fatalf("client catalog escaped model scope: %+v", listed.Data)
	}
	req = httptest.NewRequest(http.MethodPost, "/dash/api/admin/models/bulk", bytes.NewBufferString(`{"ids":["mock/free"],"action":"disable"}`))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable model: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 0 {
		t.Fatal("disabled model remained visible to client")
	}
}

func TestModelsEndpointIsPublic(t *testing.T) {
	srv, _ := testAdminServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous /v1/models: %d %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Data []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 || listed.Data[0].ID != "mock/a" {
		t.Fatalf("public catalog = %+v", listed.Data)
	}
	// A presented but invalid key must still be rejected.
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer vlk_"+strings.Repeat("x", 43))
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key on /v1/models: %d", rec.Code)
	}
}

func TestCachedInputIsDiscountedRatherThanCountedTwice(t *testing.T) {
	srv, _ := testAdminServer(t)
	req := httptest.NewRequest(http.MethodPut, "/dash/api/admin/models", bytes.NewBufferString(`{"id":"mock/cached","backend":"mock","upstream":"cached","pricing_input":2,"pricing_output":8,"pricing_cache":0.5}`))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configure model: %d %s", rec.Code, rec.Body.String())
	}
	for _, tc := range []struct {
		cached uint64
		want   float64
	}{{800000, 1.6}, {1000000, 1.3}, {2000000, 1.3}} {
		got := costOf("mock/cached", 1000000, 100000, tc.cached)
		if got < tc.want-0.000001 || got > tc.want+0.000001 {
			t.Fatalf("cached=%d cost=%g want=%g", tc.cached, got, tc.want)
		}
	}
}

func TestSettingsUpdateAndConfigResponsesWithoutRetention(t *testing.T) {
	srv, _ := testAdminServer(t)
	req := httptest.NewRequest(http.MethodPut, "/dash/api/admin/settings", bytes.NewBufferString(`{"limits":{"max_in_flight":17}}`))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save supported settings: %d %s", rec.Code, rec.Body.String())
	}
	saved, err := srv.control.LoadOrImportConfig(srv.runtime().cfg)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Limits.MaxInFlight != 17 || srv.runtime().cfg.Limits.MaxInFlight != 17 {
		t.Fatal("supported settings were not saved and applied")
	}
	for _, endpoint := range []struct {
		path    string
		handler http.HandlerFunc
	}{{"/dash/api/admin/settings", srv.handleAdminSettings}, {"/dash/api/config", srv.handleDashConfig}} {
		req = httptest.NewRequest(http.MethodGet, endpoint.path, nil)
		authorizeAdminTest(t, srv, req)
		rec = httptest.NewRecorder()
		endpoint.handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", endpoint.path, rec.Code, rec.Body.String())
		}
		var response map[string]json.RawMessage
		if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if _, exists := response["retention_days"]; exists {
			t.Fatalf("%s exposes removed retention setting", endpoint.path)
		}
	}
}

func TestEditAlertRulesPersistsAcrossReload(t *testing.T) {
	srv, _ := testAdminServer(t)
	srv.alerts = NewAlertEngine(nil, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/dash/api/state", nil)
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("load alert rules: %d %s", rec.Code, rec.Body.String())
	}
	var state dashState
	if err := json.Unmarshal(rec.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	const editedID = "operator-error-threshold"
	state.AlertRules = append(state.AlertRules, AlertRule{
		ID: editedID, Metric: "error_rate", Scope: "global", Threshold: 12.5, WindowSec: 300, Severity: "warning",
	})
	body, err := json.Marshal(map[string]any{"alerts": state.AlertRules})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPut, "/dash/api/admin/settings", bytes.NewReader(body))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save rules including those returned by the dashboard: %d %s", rec.Code, rec.Body.String())
	}
	persisted, err := srv.control.LoadOrImportConfig(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range persisted.Dashboard.Alerts {
		if rule.ID == editedID {
			if rule.Threshold != 12.5 || rule.Metric != "error_rate" {
				t.Fatalf("edited rule changed after reload: %+v", rule)
			}
			return
		}
	}
	t.Fatal("edited alert rule was not persisted")
}
