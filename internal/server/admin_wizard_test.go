package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"auth"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"widget-v1","owned_by":"acme"},{"id":"gadget-v2"}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"auth"}}`))
			return
		}
		var p struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		if p.Model == "missing-v1" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"model missing-v1 not found","type":"not_found"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"PONG"}}]}`))
	})
	return httptest.NewServer(mux)
}

func TestAdminDiscoverExplicit(t *testing.T) {
	srv, _ := testAdminServer(t)
	up := mockUpstream(t)
	defer up.Close()

	body, _ := json.Marshal(map[string]any{
		"base_url": up.URL + "/v1", "allow_private_upstreams": true,
		"api_key": "good-key",
	})
	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/discover", bytes.NewReader(body))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminDiscover(rec, req)
	if rec.Code != 200 {
		t.Fatalf("discover: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []discoveredModel `json:"models"`
		Count  int               `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || out.Models[0].ID != "gadget-v2" || out.Models[1].ID != "widget-v1" {
		t.Fatalf("sorted models: %+v", out)
	}
}

func TestAdminDiscoverBadKey(t *testing.T) {
	srv, _ := testAdminServer(t)
	up := mockUpstream(t)
	defer up.Close()

	body, _ := json.Marshal(map[string]any{
		"base_url": up.URL + "/v1", "allow_private_upstreams": true,
		"api_key": "wrong",
	})
	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/discover", bytes.NewReader(body))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminDiscover(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminTestOKAndNotFound(t *testing.T) {
	srv, _ := testAdminServer(t)
	up := mockUpstream(t)
	defer up.Close()

	mkBody := func(upstream string) *bytes.Reader {
		b, _ := json.Marshal(map[string]any{
			"base_url": up.URL + "/v1", "allow_private_upstreams": true,
			"api_key":  "good-key",
			"upstream": upstream,
		})
		return bytes.NewReader(b)
	}

	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/test", mkBody("widget-v1"))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminTest(rec, req)
	if rec.Code != 200 {
		t.Fatalf("test ok: %d %s", rec.Code, rec.Body.String())
	}
	var okOut struct {
		OK    bool   `json:"ok"`
		Reply string `json:"reply"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &okOut); err != nil {
		t.Fatal(err)
	}
	if !okOut.OK || okOut.Reply != "PONG" {
		t.Fatalf("ok reply: %+v", okOut)
	}

	req = httptest.NewRequest(http.MethodPost, "/dash/api/admin/test", mkBody("missing-v1"))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminTest(rec, req)
	var errOut struct {
		OK     bool   `json:"ok"`
		Status int    `json:"status"`
		Hint   string `json:"hint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.OK || errOut.Status != 404 || errOut.Hint == "" {
		t.Fatalf("404 hint: %+v body=%s", errOut, rec.Body.String())
	}
}

func TestAdminDiscoverViaSavedProvider(t *testing.T) {
	srv, _ := testAdminServer(t)
	up := mockUpstream(t)
	defer up.Close()

	add, _ := json.Marshal(map[string]any{
		"name":     "acme",
		"base_url": up.URL + "/v1", "allow_private_upstreams": true,
		"keys": []string{"good-key"},
	})
	req := httptest.NewRequest(http.MethodPut, "/dash/api/admin/providers", bytes.NewReader(add))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminProviderPut(rec, req)
	if rec.Code != 200 {
		t.Fatalf("add provider: %d %s", rec.Code, rec.Body.String())
	}

	body, _ := json.Marshal(map[string]any{"provider": "acme"})
	req = httptest.NewRequest(http.MethodPost, "/dash/api/admin/discover", bytes.NewReader(body))
	authorizeAdminTest(t, srv, req)
	rec = httptest.NewRecorder()
	srv.handleAdminDiscover(rec, req)
	if rec.Code != 200 {
		t.Fatalf("discover via provider: %d %s", rec.Code, rec.Body.String())
	}
}
