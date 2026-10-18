package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddleware(t *testing.T) {
	h := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(strings.Repeat(`{"ok":true}`, 50)))
	}))
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("JSON response was not gzipped")
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), `"ok":true`) {
		t.Fatal("gzipped body does not round-trip")
	}

	// Streaming path: never compressed even when accepted.
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Content-Encoding") == "gzip" {
		t.Fatal("streaming path was gzipped")
	}
}

// C2/C3: hashed assets are immutable, index.html revalidates, ETag works.
func TestDashAssetCaching(t *testing.T) {
	s := &Server{}
	var jsPath string
	for p := range dashAssets {
		if strings.HasPrefix(p, "assets/") && strings.HasSuffix(p, ".js") {
			jsPath = p
			break
		}
	}
	if jsPath == "" {
		t.Skip("no JS asset in this build")
	}
	rec := httptest.NewRecorder()
	s.handleDashUI(rec, httptest.NewRequest("GET", "/dash/"+jsPath, nil))
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("hashed asset Cache-Control = %q, want immutable", cc)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("hashed asset has no ETag")
	}
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dash/"+jsPath, nil)
	req.Header.Set("If-None-Match", etag)
	s.handleDashUI(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match: got %d, want 304", rec2.Code)
	}
	rec3 := httptest.NewRecorder()
	s.handleDashUI(rec3, httptest.NewRequest("GET", "/dash/", nil))
	if cc := rec3.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("index.html Cache-Control = %q, must revalidate", cc)
	}
}
