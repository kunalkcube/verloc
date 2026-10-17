package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashUIServesRootAssetsAndFallsBackToShell(t *testing.T) {
	var s Server
	cases := []struct {
		path    string
		ct      string
		isAsset bool
	}{
		{"/dash", "text/html; charset=utf-8", false},
		{"/dash/", "text/html; charset=utf-8", false},
		{"/dash/favicon.ico", "image/x-icon", true},
		{"/dash/logo.png", "image/png", true},
		{"/dash/providers", "text/html; charset=utf-8", false},
		{"/dash/missing.png", "text/html; charset=utf-8", false},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		s.handleDashUI(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.ct {
			t.Fatalf("%s: content-type %q, want %q", tc.path, got, tc.ct)
		}
		if tc.isAsset && strings.Contains(rec.Body.String(), `id="root"`) {
			t.Fatalf("%s: expected asset bytes, got SPA shell", tc.path)
		}
	}
}
