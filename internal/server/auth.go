package server

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	ownerPasswordIterations = 600000
	ownerSessionCookie      = "verloc_session"
	ownerSessionLifetime    = 24 * time.Hour
	loginWindow             = 15 * time.Minute
	loginAttemptLimit       = 8
	loginRetryAfter         = "900"
	// loginMaxEntries caps throttle-map memory. Entries whose window has fully
	// elapsed are pruned first, so the cap only rejects a login when the map is
	// saturated with still-active throttles; without the prune-first ordering a
	// capped map could reject an unthrottled IP.
	loginMaxEntries = 4096
	// loginSweepLimit bounds the O(n) expiry sweep performed per attempt so an
	// unauthenticated probe cannot force a full-map scan of up to loginMaxEntries.
	loginSweepLimit = 64
)

type SecurityOptions struct {
	SetupToken    string
	SecureCookies bool
}

type loginAttempts struct {
	start time.Time
	count int
}

// Security owns the sole owner's sessions and the independent inference-key namespace.
// The control database contains only password/token hashes, never usable credentials.
type Security struct {
	control      *ControlStore
	options      SecurityOptions
	authMu       sync.Mutex
	loginMu      sync.Mutex
	logins       map[string]loginAttempts
	loginSweepAt time.Time
	loginSlots   chan struct{}
	mu           sync.Mutex
	keys         map[[32]byte]*clientKeyRecord
	clients      map[string]*clientKeyRecord
}

func NewSecurity(control *ControlStore, options SecurityOptions) (*Security, error) {
	if control == nil || control.db == nil {
		return nil, errors.New("security requires a control database")
	}
	s := &Security{control: control, options: options, logins: make(map[string]loginAttempts), loginSlots: make(chan struct{}, 2), keys: make(map[[32]byte]*clientKeyRecord), clients: make(map[string]*clientKeyRecord)}
	var count int
	if err := control.db.QueryRow("SELECT count(*) FROM admin_auth WHERE id=1").Scan(&count); err != nil {
		return nil, fmt.Errorf("load owner authentication: %w", err)
	}
	if count == 0 && len(options.SetupToken) < 32 {
		return nil, errors.New("first-run setup requires a high-entropy local setup token")
	}
	if _, err := control.db.Exec("DELETE FROM admin_sessions WHERE expires_at<=?", time.Now().UnixMilli()); err != nil {
		return nil, fmt.Errorf("expire owner sessions: %w", err)
	}
	if err := s.loadClientKeys(); err != nil {
		return nil, err
	}
	return s, nil
}

func randomSecurityToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// LoadOrCreateSetupToken keeps the bootstrap capability outside the database.
// Exclusive creation prevents competing initializers from silently replacing it.
func LoadOrCreateSetupToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("setup token file path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create setup token directory: %w", err)
	}
	for range 2 {
		f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err == nil {
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
				return "", errors.New("setup token file must be a private regular file (0600)")
			}
			b, err := io.ReadAll(io.LimitReader(f, 256))
			if err != nil {
				return "", err
			}
			token := strings.TrimSpace(string(b))
			raw, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil || len(raw) != 32 {
				return "", errors.New("invalid setup token file")
			}
			return token, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("open setup token file: %w", err)
		}
		token, err := randomSecurityToken()
		if err != nil {
			return "", err
		}
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create setup token file: %w", err)
		}
		_, writeErr := io.WriteString(f, token+"\n")
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return token, nil
	}
	return "", errors.New("setup token file changed during initialization")
}

func (s *Security) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /dash/api/auth/session", s.handleAuthSession)
	mux.HandleFunc("POST /dash/api/auth/setup", s.handleAuthSetup)
	mux.HandleFunc("POST /dash/api/auth/login", s.handleAuthLogin)
	mux.HandleFunc("POST /dash/api/auth/logout", s.handleAuthLogout)
	mux.HandleFunc("POST /dash/api/auth/password", s.handleAuthPassword)
	mux.HandleFunc("GET /dash/api/admin/client-keys", s.handleClientKeysList)
	mux.HandleFunc("POST /dash/api/admin/client-keys", s.handleClientKeyCreate)
	mux.HandleFunc("PUT /dash/api/admin/client-keys/{id}", s.handleClientKeyEdit)
	mux.HandleFunc("POST /dash/api/admin/client-keys/{id}/rotate", s.handleClientKeyRotate)
	mux.HandleFunc("DELETE /dash/api/admin/client-keys/{id}", s.handleClientKeyRevoke)
}

func securityNoStore(w http.ResponseWriter) { w.Header().Set("Cache-Control", "no-store") }

func decodeSecurityJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	// The login slot is acquired BEFORE this decode, and there is no server
	// ReadTimeout — without a deadline, two clients that send headers and
	// then stall the body hold both slots forever and the owner can never
	// log in. Ten seconds is generous for a ≤64 KiB body.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "invalid JSON request")
		return false
	}
	return true
}

func unsafeSecurityMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// Browser writes must come from the exact origin. Missing Origin is allowed for
// non-browser clients, but an authenticated write still needs the session CSRF token.
// SecureCookies explicitly declares an externally HTTPS origin when TLS
// terminates at a trusted proxy. Forwarded headers never change this decision.
func (s *Security) securityOriginOK(r *http.Request) bool {
	if !unsafeSecurityMethod(r.Method) {
		return true
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if r.TLS != nil || s.options.SecureCookies {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Path == ""
}

func (s *Security) requireSecurityOrigin(w http.ResponseWriter, r *http.Request) bool {
	securityNoStore(w)
	if !s.securityOriginOK(r) {
		writeErr(w, http.StatusForbidden, "cross-origin request rejected")
		return false
	}
	return true
}

func (s *Security) session(r *http.Request) (string, [32]byte, bool) {
	cookie, err := r.Cookie(ownerSessionCookie)
	if err != nil || len(cookie.Value) != 43 {
		return "", [32]byte{}, false
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	var csrf string
	var expires int64
	if err := s.control.db.QueryRowContext(r.Context(), "SELECT csrf_token,expires_at FROM admin_sessions WHERE token_hash=?", hash[:]).Scan(&csrf, &expires); err != nil || expires <= time.Now().UnixMilli() {
		return "", hash, false
	}
	return csrf, hash, true
}

func (s *Security) CheckAdmin(w http.ResponseWriter, r *http.Request) bool {
	_, ok := s.checkAdmin(w, r)
	return ok
}

// checkAdmin performs the full owner check (origin, session lookup, CSRF) and
// returns the validated session hash. Handlers that must re-confirm the session
// after acquiring authMu (password rotation) reuse this hash with sessionCurrent
// instead of issuing a second, redundant full validation + SQLite lookup.
func (s *Security) checkAdmin(w http.ResponseWriter, r *http.Request) ([32]byte, bool) {
	if !s.requireSecurityOrigin(w, r) {
		return [32]byte{}, false
	}
	csrf, hash, ok := s.session(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "owner authentication required")
		return [32]byte{}, false
	}
	// Compare fixed-width SHA-256 digests: subtle.ConstantTimeCompare (and
	// hmac.Equal, which wraps it) short-circuits on unequal lengths and would
	// leak the attacker-controlled X-CSRF-Token length via timing.
	if unsafeSecurityMethod(r.Method) && !constantTimeTokenEqual(csrf, r.Header.Get("X-CSRF-Token")) {
		writeErr(w, http.StatusForbidden, "CSRF token required")
		return [32]byte{}, false
	}
	return hash, true
}

// sessionCurrent reports whether an already-validated session is still present
// and unexpired. Callers hold s.authMu, which serializes the only operation
// that can invalidate a session (password rotation, which deletes every row),
// so a single indexed lookup is sufficient: the request's origin and CSRF token
// cannot change between the initial check and this one.
func (s *Security) sessionCurrent(r *http.Request, hash [32]byte) bool {
	var one int
	err := s.control.db.QueryRowContext(r.Context(), "SELECT 1 FROM admin_sessions WHERE token_hash=? AND expires_at>?", hash[:], time.Now().UnixMilli()).Scan(&one)
	return err == nil
}

// constantTimeTokenEqual compares two tokens in constant time regardless of
// length by hashing both to fixed-width digests first. It is used for the CSRF
// header, the only comparison in this file whose input length is attacker
// controlled; the password and setup-token compares always operate on equal,
// fixed-width inputs and can use subtle.ConstantTimeCompare directly.
func constantTimeTokenEqual(expected, provided string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

func (s *Security) AdminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.CheckAdmin(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

func (s *Security) setupRequired() (bool, error) {
	var count int
	err := s.control.db.QueryRow("SELECT count(*) FROM admin_auth WHERE id=1").Scan(&count)
	return count == 0, err
}

func (s *Security) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	securityNoStore(w)
	required, err := s.setupRequired()
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	csrf, _, authenticated := s.session(r)
	response := map[string]any{"authenticated": authenticated, "setup_required": required}
	if authenticated {
		response["csrf_token"] = csrf
	}
	writeJSON(w, 200, response)
}

// loginThrottled reports whether the caller's address has exhausted its FAILED
// attempt budget. It deliberately counts only failures: the previous version
// counted every attempt and rejected before the password was verified, so once
// an address was hot even the correct password was refused — the owner could be
// locked out of their own dashboard by anyone able to reach the login route
// (and behind a reverse proxy every client shares one address). Throttling
// failures preserves brute-force resistance without that denial.
func (s *Security) loginThrottled(r *http.Request) bool {
	ip := loginAddr(r)
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.sweepLogins(now)
	a, exists := s.logins[ip]
	if !exists {
		return false
	}
	// Release the address exactly when its window lapses, independently of the
	// bounded sweep.
	if now.Sub(a.start) >= loginWindow {
		delete(s.logins, ip)
		return false
	}
	return a.count >= loginAttemptLimit
}

// noteLoginFailure records one failed attempt for the caller's address.
func (s *Security) noteLoginFailure(r *http.Request) {
	ip := loginAddr(r)
	now := time.Now()
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.sweepLogins(now)
	a, exists := s.logins[ip]
	if exists && now.Sub(a.start) >= loginWindow {
		a = loginAttempts{}
		exists = false
	}
	if !exists && len(s.logins) >= loginMaxEntries {
		// Bounded memory, without either failure mode of the alternatives:
		// refusing to record once full would let an attacker who fills the map
		// keep guessing from fresh addresses unthrottled, and rejecting every
		// caller because the map is full (the original behaviour) let anyone
		// lock the owner out. Evict the oldest-started entry instead, so a new
		// failure is always recorded and the map stays capped.
		oldestIP, oldest := "", time.Time{}
		for k, v := range s.logins {
			if oldestIP == "" || v.start.Before(oldest) {
				oldestIP, oldest = k, v.start
			}
		}
		delete(s.logins, oldestIP)
	}
	if !exists {
		a.start = now
	}
	a.count++
	s.logins[ip] = a
}

// clearLoginFailures drops the caller's failure record after a success, so a
// legitimate owner is never penalised for earlier typos.
func (s *Security) clearLoginFailures(r *http.Request) {
	ip := loginAddr(r)
	s.loginMu.Lock()
	delete(s.logins, ip)
	s.loginMu.Unlock()
}

func loginAddr(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// beginLogin enforces origin, throttling and the global concurrency slot.
// The slot is still released by the caller in a defer.
func (s *Security) beginLogin(w http.ResponseWriter, r *http.Request) bool {
	if !s.requireSecurityOrigin(w, r) {
		return false
	}
	if s.loginThrottled(r) {
		w.Header().Set("Retry-After", loginRetryAfter)
		writeErr(w, http.StatusTooManyRequests, "authentication temporarily unavailable")
		return false
	}
	select {
	case s.loginSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "authentication temporarily unavailable")
		return false
	}
}

// sweepLogins prunes elapsed throttle entries. It runs while holding s.loginMu
// and bounds its work two ways so an unauthenticated probe cannot turn it into a
// full-map scan of up to loginMaxEntries: at most one non-empty sweep per
// loginWindow, and at most loginSweepLimit deletions per non-empty sweep.
// Because that leaves an occasional stale entry, the scan also runs whenever the
// map is at/over the cap: it then evicts every elapsed entry (violating only the
// per-call work bound, never the memory bound) before the caller's cap check, so
// the map always drops back below loginMaxEntries. Deleting only elapsed entries
// never removes an active throttle, keeping the 8-per-15-minute limit exact.
func (s *Security) sweepLogins(now time.Time) {
	empty := len(s.logins) == 0
	if empty && !s.loginSweepAt.IsZero() && now.Sub(s.loginSweepAt) < loginWindow {
		return
	}
	if empty {
		s.loginSweepAt = now
		return
	}
	full := len(s.logins) >= loginMaxEntries
	if !full && !s.loginSweepAt.IsZero() && now.Sub(s.loginSweepAt) < loginWindow {
		return
	}
	limit := loginSweepLimit
	if full {
		limit = len(s.logins)
	}
	for addr, attempts := range s.logins {
		if limit <= 0 {
			break
		}
		if now.Sub(attempts.start) >= loginWindow {
			delete(s.logins, addr)
			limit--
		}
	}
	s.loginSweepAt = now
}

func deriveOwnerPassword(password string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, ownerPasswordIterations, 32)
}

func (s *Security) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if !s.beginLogin(w, r) {
		return
	}
	defer func() { <-s.loginSlots }()
	var req struct {
		SetupToken string `json:"setup_token"`
		Password   string `json:"password"`
	}
	if !decodeSecurityJSON(w, r, &req) {
		return
	}
	provided := sha256.Sum256([]byte(req.SetupToken))
	expected := sha256.Sum256([]byte(s.options.SetupToken))
	if s.options.SetupToken == "" || subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		writeErr(w, 401, "authentication failed")
		return
	}
	if utf8.RuneCountInString(req.Password) < 12 || len(req.Password) > 1024 {
		writeErr(w, 400, "password must contain at least 12 characters and at most 1024 bytes")
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	required, err := s.setupRequired()
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if !required {
		writeErr(w, http.StatusConflict, "owner is already configured")
		return
	}
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	hash, err := deriveOwnerPassword(req.Password, salt[:])
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO admin_auth(id,password_salt,password_hash,created_at) VALUES(1,?,?,?)", salt[:], hash, time.Now().UnixMilli()); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if _, err = tx.Exec("INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)", time.Now().UnixMilli(), "owner.setup", "owner"); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	token, csrf, err := s.insertSession(tx)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	s.respondSession(w, r, token, csrf)
}

func (s *Security) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if !s.beginLogin(w, r) {
		return
	}
	defer func() { <-s.loginSlots }()
	var req struct {
		Password string `json:"password"`
	}
	if !decodeSecurityJSON(w, r, &req) {
		return
	}
	if len(req.Password) > 1024 {
		s.noteLoginFailure(r)
		writeErr(w, 401, "authentication failed")
		return
	}
	// Serialize verification/session creation with password rotation so an old
	// password cannot mint a session after rotation has revoked its peers.
	s.authMu.Lock()
	defer s.authMu.Unlock()
	var salt, expected []byte
	err := s.control.db.QueryRowContext(r.Context(), "SELECT password_salt,password_hash FROM admin_auth WHERE id=1").Scan(&salt, &expected)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	found := err == nil
	if !found {
		salt = make([]byte, 32)
		expected = make([]byte, 32)
	}
	hash, err := deriveOwnerPassword(req.Password, salt)
	if err != nil || !found || subtle.ConstantTimeCompare(hash, expected) != 1 {
		s.noteLoginFailure(r)
		writeErr(w, 401, "authentication failed")
		return
	}
	// Correct password: clear the failure record so a legitimate owner is never
	// penalised for earlier typos.
	s.clearLoginFailures(r)
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	defer tx.Rollback()
	// Remove expired sessions and cap owner sessions to prevent unbounded accumulation.
	if _, err := tx.Exec("DELETE FROM admin_sessions WHERE expires_at<=? OR token_hash IN (SELECT token_hash FROM admin_sessions ORDER BY expires_at DESC LIMIT -1 OFFSET 31)", time.Now().UnixMilli()); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	token, csrf, err := s.insertSession(tx)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if _, err := tx.Exec("INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)", time.Now().UnixMilli(), "owner.login", "owner"); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	s.respondSession(w, r, token, csrf)
}

func (s *Security) insertSession(tx *sql.Tx) (string, string, error) {
	token, err := randomSecurityToken()
	if err != nil {
		return "", "", err
	}
	csrf, err := randomSecurityToken()
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256([]byte(token))
	_, err = tx.Exec("INSERT INTO admin_sessions(token_hash,csrf_token,expires_at) VALUES(?,?,?)", hash[:], csrf, time.Now().Add(ownerSessionLifetime).UnixMilli())
	return token, csrf, err
}

func (s *Security) sessionCookie(r *http.Request, token string) *http.Cookie {
	return &http.Cookie{Name: ownerSessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.options.SecureCookies || r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(ownerSessionLifetime.Seconds())}
}

func (s *Security) respondSession(w http.ResponseWriter, r *http.Request, token, csrf string) {
	http.SetCookie(w, s.sessionCookie(r, token))
	writeJSON(w, 200, map[string]any{"authenticated": true, "setup_required": false, "csrf_token": csrf})
}

func (s *Security) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAdmin(w, r) {
		return
	}
	_, hash, ok := s.session(r)
	if !ok {
		writeErr(w, 401, "owner authentication required")
		return
	}
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM admin_sessions WHERE token_hash=?", hash[:]); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if _, err = tx.Exec("INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)", time.Now().UnixMilli(), "owner.logout", "owner"); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if err = tx.Commit(); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	cookie := s.sessionCookie(r, "")
	cookie.MaxAge = -1
	cookie.Expires = time.Unix(1, 0)
	http.SetCookie(w, cookie)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Security) handleAuthPassword(w http.ResponseWriter, r *http.Request) {
	sessionHash, ok := s.checkAdmin(w, r)
	if !ok || !s.beginLogin(w, r) {
		return
	}
	defer func() { <-s.loginSlots }()
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeSecurityJSON(w, r, &req) {
		return
	}
	if utf8.RuneCountInString(req.NewPassword) < 12 || len(req.NewPassword) > 1024 {
		writeErr(w, 400, "password must contain at least 12 characters and at most 1024 bytes")
		return
	}
	if len(req.CurrentPassword) > 1024 {
		writeErr(w, 401, "authentication failed")
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	// Re-confirm the session survived a concurrent password rotation without a
	// second origin+CSRF+session round trip (see checkAdmin/sessionCurrent).
	if !s.sessionCurrent(r, sessionHash) {
		writeErr(w, http.StatusUnauthorized, "owner authentication required")
		return
	}
	var oldSalt, expected []byte
	if err := s.control.db.QueryRowContext(r.Context(), "SELECT password_salt,password_hash FROM admin_auth WHERE id=1").Scan(&oldSalt, &expected); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	provided, err := deriveOwnerPassword(req.CurrentPassword, oldSalt)
	if err != nil || subtle.ConstantTimeCompare(provided, expected) != 1 {
		writeErr(w, 401, "authentication failed")
		return
	}
	var salt [32]byte
	if _, err := rand.Read(salt[:]); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	hash, err := deriveOwnerPassword(req.NewPassword, salt[:])
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE admin_auth SET password_salt=?,password_hash=? WHERE id=1", salt[:], hash); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if _, err := tx.Exec("DELETE FROM admin_sessions"); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	token, csrf, err := s.insertSession(tx)
	if err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if err := securityAudit(tx, "owner.password", "owner"); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, 500, "authentication unavailable")
		return
	}
	s.respondSession(w, r, token, csrf)
}

// Close does not close the shared control store, which remains owned by Server.
func (s *Security) Close() error { return nil }
