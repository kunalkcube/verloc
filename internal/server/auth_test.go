package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type securityTestEnv struct {
	store      *ControlStore
	security   *Security
	mux        *http.ServeMux
	dbPath     string
	masterPath string
	setupToken string
}

func newSecurityTestEnv(t *testing.T) *securityTestEnv {
	t.Helper()
	dir := privateTestDir(t)
	e := &securityTestEnv{dbPath: filepath.Join(dir, "control.db"), masterPath: filepath.Join(dir, "master.key")}
	var err error
	e.setupToken, err = LoadOrCreateSetupToken(filepath.Join(dir, "setup.token"))
	if err != nil {
		t.Fatal(err)
	}
	e.store, err = OpenControlStore(e.dbPath, e.masterPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.store.Close() })
	e.startSecurity(t)
	return e
}

func (e *securityTestEnv) startSecurity(t *testing.T) {
	t.Helper()
	var err error
	e.security, err = NewSecurity(e.store, SecurityOptions{SetupToken: e.setupToken})
	if err != nil {
		t.Fatal(err)
	}
	e.mux = http.NewServeMux()
	e.security.RegisterRoutes(e.mux)
	e.mux.Handle("GET /owner-test", e.security.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	e.mux.Handle("POST /owner-test", e.security.AdminMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
	e.mux.Handle("POST /inference-test", e.security.ClientMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })))
}

func (e *securityTestEnv) request(method, path string, body any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.RemoteAddr = "192.0.2.1:1234"
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	return w
}

func securitySessionFromResponse(t *testing.T, w *httptest.ResponseRecorder) (*http.Cookie, string) {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("authentication status %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Authenticated bool   `json:"authenticated"`
		CSRFToken     string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Authenticated || response.CSRFToken == "" {
		t.Fatalf("missing authenticated CSRF response: %s", w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("unsafe session cookie: %#v", cookies)
	}
	return cookies[0], response.CSRFToken
}

// Key-policy tests need a real persisted session, but not repeated expensive
// password derivations. Owner setup/login are covered end to end below.
func (e *securityTestEnv) seedSession(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	token, err := randomSecurityToken()
	if err != nil {
		t.Fatal(err)
	}
	csrf, err := randomSecurityToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(token))
	if _, err := e.store.db.Exec("INSERT INTO admin_sessions(token_hash,csrf_token,expires_at) VALUES(?,?,?)", hash[:], csrf, time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: ownerSessionCookie, Value: token}, csrf
}

func TestOwnerSetupSessionRestartAndLogout(t *testing.T) {
	e := newSecurityTestEnv(t)
	if w := e.request("GET", "/owner-test", nil, nil, ""); w.Code != 401 {
		t.Fatalf("anonymous owner access: %d", w.Code)
	}
	if w := e.request("POST", "/inference-test", nil, nil, ""); w.Code != 401 {
		t.Fatalf("anonymous inference access: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/auth/setup", map[string]string{"password": "a sufficiently long password"}, nil, ""); w.Code != 401 {
		t.Fatalf("setup without local token: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/auth/setup", map[string]string{"setup_token": e.setupToken, "password": "too short"}, nil, ""); w.Code != 400 {
		t.Fatalf("short owner password: %d", w.Code)
	}
	password := "a sufficiently long password"
	w := e.request("POST", "/dash/api/auth/setup", map[string]string{"setup_token": e.setupToken, "password": password}, nil, "")
	cookie, csrf := securitySessionFromResponse(t, w)
	if w := e.request("POST", "/dash/api/auth/setup", map[string]string{"setup_token": e.setupToken, "password": password}, nil, ""); w.Code != 409 {
		t.Fatalf("second owner setup: %d", w.Code)
	}
	if w := e.request("POST", "/owner-test", nil, cookie, ""); w.Code != 403 {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	if w := e.request("POST", "/owner-test", nil, cookie, "wrong"); w.Code != 403 {
		t.Fatalf("incorrect CSRF: %d", w.Code)
	}
	if w := e.request("POST", "/owner-test", nil, cookie, csrf); w.Code != 204 {
		t.Fatalf("valid owner write: %d", w.Code)
	}
	if w := e.request("POST", "/inference-test", nil, cookie, csrf); w.Code != 401 {
		t.Fatalf("owner cookie accepted for inference: %d", w.Code)
	}
	var salt, hash []byte
	if err := e.store.db.QueryRow("SELECT password_salt,password_hash FROM admin_auth WHERE id=1").Scan(&salt, &hash); err != nil {
		t.Fatal(err)
	}
	if len(salt) != 32 || len(hash) != 32 || bytes.Contains(hash, []byte(password)) {
		t.Fatal("password was not salted and hashed")
	}
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	e.store, err = OpenControlStore(e.dbPath, e.masterPath)
	if err != nil {
		t.Fatal(err)
	}
	e.startSecurity(t)
	if w := e.request("GET", "/owner-test", nil, cookie, ""); w.Code != 204 {
		t.Fatalf("session lost after restart: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/auth/logout", nil, cookie, ""); w.Code != 403 {
		t.Fatalf("logout missing CSRF: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/auth/logout", nil, cookie, csrf); w.Code != 200 {
		t.Fatalf("logout failed: %d %s", w.Code, w.Body.String())
	}
	if w := e.request("GET", "/owner-test", nil, cookie, ""); w.Code != 401 {
		t.Fatalf("logged out session accepted: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": "incorrect password"}, nil, ""); w.Code != 401 {
		t.Fatalf("wrong password: %d", w.Code)
	}
	cookie, _ = securitySessionFromResponse(t, e.request("POST", "/dash/api/auth/login", map[string]string{"password": password}, nil, ""))
	if w := e.request("GET", "/owner-test", nil, cookie, ""); w.Code != 204 {
		t.Fatalf("new login rejected: %d", w.Code)
	}
}

func TestOwnerRejectsCrossOriginWrites(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	for _, path := range []string{"/dash/api/auth/login", "/dash/api/auth/setup", "/dash/api/auth/logout", "/owner-test"} {
		for _, origin := range []string{"https://attacker.example", "null", "http://example.com.attacker.example", "https://example.com"} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
			r.Header.Set("Origin", origin)
			r.Header.Set("X-CSRF-Token", csrf)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			e.mux.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatalf("%s origin %q: %d", path, origin, w.Code)
			}
		}
	}
	r := httptest.NewRequest("POST", "/owner-test", nil)
	r.Header.Set("Origin", "http://example.com")
	r.Header.Set("X-CSRF-Token", csrf)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("same-origin write rejected: %d", w.Code)
	}
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w = httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-site fetch accepted: %d", w.Code)
	}
}

func TestOwnerLoginThrottlingAndConcurrency(t *testing.T) {
	e := newSecurityTestEnv(t)
	// The throttle counts FAILED LOGIN attempts. It no longer counts failures
	// from other routes (a setup-token probe must not lock the owner out of
	// login) and it is no longer keyed on a spoofable X-Forwarded-For, since
	// RemoteAddr is what the server can actually attest.
	for range 8 {
		w := e.request("POST", "/dash/api/auth/setup", map[string]string{"setup_token": "wrong", "password": "long enough password"}, nil, "")
		if w.Code != 401 {
			t.Fatalf("unexpected setup status: %d", w.Code)
		}
	}
	// Setup failures must NOT throttle login.
	fresh := httptest.NewRequest("POST", "/dash/api/auth/login", strings.NewReader(`{"password":"irrelevant"}`))
	fresh.RemoteAddr = "192.0.2.9:5678"
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, fresh)
	if w.Code == 429 {
		t.Fatal("setup-token failures throttled the login route")
	}
	// Eight genuine login failures from one address must throttle it.
	for range 8 {
		r := httptest.NewRequest("POST", "/dash/api/auth/login", strings.NewReader(`{"password":"wrong"}`))
		r.RemoteAddr = "192.0.2.1:5678"
		rr := httptest.NewRecorder()
		e.mux.ServeHTTP(rr, r)
	}
	r := httptest.NewRequest("POST", "/dash/api/auth/login", strings.NewReader(`{"password":"irrelevant"}`))
	r.RemoteAddr = "192.0.2.1:5678"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	w = httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("throttling bypassed: %d", w.Code)
	}
	for range cap(e.security.loginSlots) {
		e.security.loginSlots <- struct{}{}
	}
	r.RemoteAddr = "192.0.2.2:5678"
	w = httptest.NewRecorder()
	e.mux.ServeHTTP(w, r)
	if w.Code != 429 {
		t.Fatalf("hash concurrency unbounded: %d", w.Code)
	}
	for range cap(e.security.loginSlots) {
		<-e.security.loginSlots
	}
}

func TestSetupTokenIsPrivateStableAndRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private", "setup.token")
	first, err := LoadOrCreateSetupToken(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateSetupToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 43 {
		t.Fatal("setup token changed or lacked entropy")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("setup token permissions: %o", info.Mode().Perm())
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSetupToken(path); err == nil {
		t.Fatal("public setup token accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "setup-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateSetupToken(link); err == nil {
		t.Fatal("symlink setup token accepted")
	}
}

func TestExpiredOwnerSessionIsRejected(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, _ := e.seedSession(t)
	hash := sha256.Sum256([]byte(cookie.Value))
	if _, err := e.store.db.Exec("UPDATE admin_sessions SET expires_at=? WHERE token_hash=?", time.Now().Add(-time.Second).UnixMilli(), hash[:]); err != nil {
		t.Fatal(err)
	}
	if w := e.request("GET", "/owner-test", nil, cookie, ""); w.Code != 401 {
		t.Fatalf("expired session accepted: %d", w.Code)
	}
}

func TestOwnerPasswordRotationRevokesAllPriorSessions(t *testing.T) {
	e := newSecurityTestEnv(t)
	oldPassword := "the original owner password"
	newPassword := "the replacement owner password"
	cookie, csrf := securitySessionFromResponse(t, e.request("POST", "/dash/api/auth/setup", map[string]string{"setup_token": e.setupToken, "password": oldPassword}, nil, ""))
	otherCookie, _ := e.seedSession(t)
	path := "/dash/api/auth/password"
	body := map[string]string{"current_password": oldPassword, "new_password": newPassword}
	if w := e.request("POST", path, body, cookie, ""); w.Code != 403 {
		t.Fatalf("password rotation missing CSRF: %d", w.Code)
	}
	if w := e.request("POST", path, map[string]string{"current_password": oldPassword, "new_password": "short"}, cookie, csrf); w.Code != 400 {
		t.Fatalf("short replacement password: %d", w.Code)
	}
	if w := e.request("POST", path, map[string]string{"current_password": "wrong password", "new_password": newPassword}, cookie, csrf); w.Code != 401 {
		t.Fatalf("wrong current password accepted: %d", w.Code)
	}
	newCookie, newCSRF := securitySessionFromResponse(t, e.request("POST", path, body, cookie, csrf))
	if newCookie.Value == cookie.Value || newCSRF == csrf {
		t.Fatal("password rotation reused session credentials")
	}
	for _, prior := range []*http.Cookie{cookie, otherCookie} {
		if w := e.request("GET", "/owner-test", nil, prior, ""); w.Code != 401 {
			t.Fatalf("prior session survived password rotation: %d", w.Code)
		}
	}
	if w := e.request("POST", "/owner-test", nil, newCookie, newCSRF); w.Code != 204 {
		t.Fatalf("fresh password-rotation session rejected: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/auth/login", map[string]string{"password": oldPassword}, nil, ""); w.Code != 401 {
		t.Fatalf("old password survived rotation: %d", w.Code)
	}
	securitySessionFromResponse(t, e.request("POST", "/dash/api/auth/login", map[string]string{"password": newPassword}, nil, ""))
}

func TestOwnerCookieSecureOnTLSOrExplicitConfiguration(t *testing.T) {
	e := newSecurityTestEnv(t)
	plain := httptest.NewRequest("GET", "http://example.com", nil)
	tls := httptest.NewRequest("GET", "https://example.com", nil)
	if e.security.sessionCookie(plain, "token").Secure {
		t.Fatal("plain local HTTP unexpectedly requires secure cookie")
	}
	if !e.security.sessionCookie(tls, "token").Secure {
		t.Fatal("TLS session cookie is not secure")
	}
	e.security.options.SecureCookies = true
	if !e.security.sessionCookie(plain, "token").Secure {
		t.Fatal("configured secure cookie policy ignored")
	}
}

func TestOwnerOriginHonorsExplicitHTTPSWithoutTrustingForwardedHeaders(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	cases := []struct {
		name           string
		requestURL     string
		origin         string
		secureCookies  bool
		forwardedProto string
		forwardedHost  string
		wantStatus     int
	}{
		{name: "localhost HTTP", requestURL: "http://localhost:8080/owner-test", origin: "http://localhost:8080", wantStatus: 204},
		{name: "unconfigured HTTPS termination", requestURL: "http://example.com/owner-test", origin: "https://example.com", wantStatus: 403},
		{name: "spoofed HTTPS forwarding", requestURL: "http://example.com/owner-test", origin: "https://example.com", forwardedProto: "https", wantStatus: 403},
		{name: "spoofed forwarded host", requestURL: "http://example.com/owner-test", origin: "http://attacker.example", forwardedHost: "attacker.example", wantStatus: 403},
		{name: "configured HTTPS termination", requestURL: "http://example.com/owner-test", origin: "https://example.com", secureCookies: true, wantStatus: 204},
		{name: "configured HTTPS rejects HTTP origin", requestURL: "http://example.com/owner-test", origin: "http://example.com", secureCookies: true, wantStatus: 403},
		{name: "spoofed HTTP cannot downgrade configured HTTPS", requestURL: "http://example.com/owner-test", origin: "http://example.com", secureCookies: true, forwardedProto: "http", wantStatus: 403},
		{name: "direct HTTPS", requestURL: "https://example.com/owner-test", origin: "https://example.com", wantStatus: 204},
		{name: "spoofed HTTP cannot downgrade TLS", requestURL: "https://example.com/owner-test", origin: "http://example.com", forwardedProto: "http", wantStatus: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.security.options.SecureCookies = tc.secureCookies
			r := httptest.NewRequest("POST", tc.requestURL, nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CSRF-Token", csrf)
			r.Header.Set("X-Forwarded-Proto", tc.forwardedProto)
			r.Header.Set("X-Forwarded-Host", tc.forwardedHost)
			r.AddCookie(cookie)
			w := httptest.NewRecorder()
			e.mux.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("origin status %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}
