package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ClientIdentity contains no bearer material and is immutable after publication.
type ClientIdentity struct {
	ID            string
	Name          string
	AllowedModels []string
	TPM           int
	authHash      [32]byte
}

func (c *ClientIdentity) AllowsModel(id string) bool {
	if c == nil {
		return false
	}
	if len(c.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range c.AllowedModels {
		if allowed == id {
			return true
		}
	}
	return false
}

type clientIdentityContextKey struct{}

func ClientIdentityFromRequest(r *http.Request) *ClientIdentity {
	identity, _ := r.Context().Value(clientIdentityContextKey{}).(*ClientIdentity)
	return identity
}

type ClientKeyView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Prefix        string   `json:"prefix"`
	AllowedModels []string `json:"allowed_models"`
	ExpiresAt     int64    `json:"expires_at"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	MaxConcurrent int      `json:"max_concurrent"`
	CreatedAt     int64    `json:"created_at"`
	RevokedAt     int64    `json:"revoked_at"`
}

type clientUsage struct {
	window   time.Time
	requests uint64
	tokens   uint64
	active   uint64
}

type clientKeyRecord struct {
	view     ClientKeyView
	hash     [32]byte
	identity *ClientIdentity
	usage    *clientUsage
}

type clientKeyRequest struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	ExpiresAt     int64    `json:"expires_at"`
	RPM           int      `json:"rpm"`
	TPM           int      `json:"tpm"`
	MaxConcurrent int      `json:"max_concurrent"`
}

func (s *Security) loadClientKeys() error {
	rows, err := s.control.db.Query("SELECT id,name,key_hash,prefix,allowed_models,expires_at,rpm,tpm,max_concurrent,created_at,revoked_at FROM client_keys")
	if err != nil {
		return fmt.Errorf("load client keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var view ClientKeyView
		var hash []byte
		var allowed string
		if err := rows.Scan(&view.ID, &view.Name, &hash, &view.Prefix, &allowed, &view.ExpiresAt, &view.RPM, &view.TPM, &view.MaxConcurrent, &view.CreatedAt, &view.RevokedAt); err != nil {
			return fmt.Errorf("load client key metadata: %w", err)
		}
		if len(hash) != sha256.Size || json.Unmarshal([]byte(allowed), &view.AllowedModels) != nil || view.RPM < 0 || view.TPM < 0 || view.MaxConcurrent < 0 {
			return errors.New("invalid stored client key policy")
		}
		if view.AllowedModels == nil {
			view.AllowedModels = []string{}
		}
		record := &clientKeyRecord{view: view, usage: &clientUsage{}}
		copy(record.hash[:], hash)
		record.identity = &ClientIdentity{ID: view.ID, Name: view.Name, AllowedModels: view.AllowedModels, TPM: view.TPM, authHash: record.hash}
		s.clients[view.ID] = record
		if view.RevokedAt == 0 {
			s.keys[record.hash] = record
		}
	}
	return rows.Err()
}

func (s *Security) ClientMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, key, separated := strings.Cut(r.Header.Get("Authorization"), " ")
		if len(r.Header.Values("Authorization")) != 1 || !separated || !strings.EqualFold(scheme, "Bearer") || len(key) != 47 || !strings.HasPrefix(key, "vlk_") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "valid client API key required")
			return
		}
		hash := sha256.Sum256([]byte(key))
		s.mu.Lock()
		record := s.keys[hash]
		var identity *ClientIdentity
		if record != nil && subtle.ConstantTimeCompare(record.hash[:], hash[:]) == 1 && record.view.RevokedAt == 0 && (record.view.ExpiresAt == 0 || record.view.ExpiresAt > time.Now().UnixMilli()) {
			identity = record.identity
		}
		s.mu.Unlock()
		if identity == nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "valid client API key required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIdentityContextKey{}, identity)))
	})
}

func validateClientKeyRequest(req *clientKeyRequest) error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 120 {
		return errors.New("name must contain 1 to 120 bytes")
	}
	if req.ExpiresAt < 0 || (req.ExpiresAt != 0 && req.ExpiresAt <= time.Now().UnixMilli()) {
		return errors.New("expires_at must be zero or a future timestamp in milliseconds")
	}
	if req.RPM < 0 || req.TPM < 0 || req.MaxConcurrent < 0 || req.RPM > 1000000000 || req.TPM > 1000000000 || req.MaxConcurrent > 1000000 {
		return errors.New("invalid client policy limits")
	}
	if len(req.AllowedModels) > 256 {
		return errors.New("too many allowed models")
	}
	seen := make(map[string]bool, len(req.AllowedModels))
	models := make([]string, 0, len(req.AllowedModels))
	for _, model := range req.AllowedModels {
		if model == "" || strings.TrimSpace(model) != model || len(model) > 256 {
			return errors.New("invalid allowed model ID")
		}
		if !seen[model] {
			models = append(models, model)
			seen[model] = true
		}
	}
	req.AllowedModels = models
	return nil
}

func (s *Security) handleClientKeysList(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAdmin(w, r) {
		return
	}
	s.mu.Lock()
	keys := make([]ClientKeyView, 0, len(s.clients))
	for _, record := range s.clients {
		keys = append(keys, record.view)
	}
	s.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].CreatedAt == keys[j].CreatedAt {
			return keys[i].ID < keys[j].ID
		}
		return keys[i].CreatedAt > keys[j].CreatedAt
	})
	writeJSON(w, 200, map[string]any{"keys": keys})
}

func newClientSecret() (string, [32]byte, string, error) {
	random, err := randomSecurityToken()
	if err != nil {
		return "", [32]byte{}, "", err
	}
	key := "vlk_" + random
	return key, sha256.Sum256([]byte(key)), key[:12], nil
}

func securityAudit(tx *sql.Tx, action, subject string) error {
	_, err := tx.Exec("INSERT INTO audit_events(ts,action,subject) VALUES(?,?,?)", time.Now().UnixMilli(), action, subject)
	return err
}

func (s *Security) handleClientKeyCreate(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAdmin(w, r) {
		return
	}
	var req clientKeyRequest
	if !decodeSecurityJSON(w, r, &req) {
		return
	}
	if err := validateClientKeyRequest(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	key, hash, prefix, err := newClientSecret()
	if err != nil {
		writeErr(w, 500, "client key creation failed")
		return
	}
	id, err := randomSecurityToken()
	if err != nil {
		writeErr(w, 500, "client key creation failed")
		return
	}
	view := ClientKeyView{ID: "ck_" + id, Name: req.Name, Prefix: prefix, AllowedModels: req.AllowedModels, ExpiresAt: req.ExpiresAt, RPM: req.RPM, TPM: req.TPM, MaxConcurrent: req.MaxConcurrent, CreatedAt: time.Now().UnixMilli()}
	allowed, _ := json.Marshal(view.AllowedModels)
	// Durability before exposure: commit the new secret to SQLite first, then take
	// s.mu only to publish it into the in-memory maps. The mutex is never held
	// across I/O, so an owner click cannot stall inference traffic.
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "client key creation failed")
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("INSERT INTO client_keys(id,name,key_hash,prefix,allowed_models,expires_at,rpm,tpm,max_concurrent,created_at,revoked_at) VALUES(?,?,?,?,?,?,?,?,?,?,0)", view.ID, view.Name, hash[:], view.Prefix, string(allowed), view.ExpiresAt, view.RPM, view.TPM, view.MaxConcurrent, view.CreatedAt)
	if err == nil {
		err = securityAudit(tx, "client_key.create", view.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeErr(w, 500, "client key creation failed")
		return
	}
	record := &clientKeyRecord{view: view, hash: hash, usage: &clientUsage{}, identity: &ClientIdentity{ID: view.ID, Name: view.Name, AllowedModels: view.AllowedModels, TPM: view.TPM, authHash: hash}}
	s.mu.Lock()
	s.clients[view.ID] = record
	s.keys[hash] = record
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "client": view})
}

func (s *Security) handleClientKeyEdit(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAdmin(w, r) {
		return
	}
	var req clientKeyRequest
	if !decodeSecurityJSON(w, r, &req) {
		return
	}
	if err := validateClientKeyRequest(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Peek at the existing record under a short lock to derive the new policy and
	// reject an already-revoked/missing key, then release s.mu for the DB write.
	// The final publish re-reads under the lock, because the record may have been
	// revoked/replaced between this peek and publication.
	id := r.PathValue("id")
	s.mu.Lock()
	old := s.clients[id]
	if old == nil {
		s.mu.Unlock()
		writeErr(w, 404, "client key not found")
		return
	}
	if old.view.RevokedAt != 0 {
		s.mu.Unlock()
		writeErr(w, 409, "client key is revoked")
		return
	}
	view := old.view
	s.mu.Unlock()
	view.Name, view.AllowedModels, view.ExpiresAt = req.Name, req.AllowedModels, req.ExpiresAt
	view.RPM, view.TPM, view.MaxConcurrent = req.RPM, req.TPM, req.MaxConcurrent
	allowed, _ := json.Marshal(view.AllowedModels)
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "client key update failed")
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("UPDATE client_keys SET name=?,allowed_models=?,expires_at=?,rpm=?,tpm=?,max_concurrent=? WHERE id=?", view.Name, string(allowed), view.ExpiresAt, view.RPM, view.TPM, view.MaxConcurrent, view.ID)
	if err == nil {
		err = securityAudit(tx, "client_key.update", view.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeErr(w, 500, "client key update failed")
		return
	}
	// Publish under s.mu. Re-read so a concurrent revoke wins: publishing the old
	// snapshot would clobber the in-memory revocation (fail-closed invariant). The
	// usage pointer is carried from the *current* record so the live quota window
	// is never refilled by a policy edit.
	s.mu.Lock()
	current := s.clients[id]
	if current == nil {
		s.mu.Unlock()
		writeErr(w, 404, "client key not found")
		return
	}
	if current.view.RevokedAt != 0 {
		s.mu.Unlock()
		writeErr(w, 409, "client key is revoked")
		return
	}
	record := &clientKeyRecord{view: view, hash: current.hash, usage: current.usage, identity: &ClientIdentity{ID: view.ID, Name: view.Name, AllowedModels: view.AllowedModels, TPM: view.TPM, authHash: current.hash}}
	s.clients[view.ID], s.keys[record.hash] = record, record
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"client": view})
}

func (s *Security) handleClientKeyRotate(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAdmin(w, r) {
		return
	}
	key, hash, prefix, err := newClientSecret()
	if err != nil {
		writeErr(w, 500, "client key rotation failed")
		return
	}
	// Peek under a short lock to reject missing/revoked keys and capture the
	// stable ID/hash, then release s.mu for the DB write. Unlike revoke, rotation
	// introduces a new secret, so the commit must land before publication.
	id := r.PathValue("id")
	s.mu.Lock()
	old := s.clients[id]
	if old == nil {
		s.mu.Unlock()
		writeErr(w, 404, "client key not found")
		return
	}
	if old.view.RevokedAt != 0 {
		s.mu.Unlock()
		writeErr(w, 409, "client key is revoked")
		return
	}
	view := old.view
	s.mu.Unlock()
	view.Prefix = prefix
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "client key rotation failed")
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("UPDATE client_keys SET key_hash=?,prefix=? WHERE id=?", hash[:], prefix, view.ID)
	if err == nil {
		err = securityAudit(tx, "client_key.rotate", view.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeErr(w, 500, "client key rotation failed")
		return
	}
	// Publish under s.mu. Re-read so a concurrent revoke/rotate is not clobbered;
	// usage is carried from the current record to preserve the live quota window.
	s.mu.Lock()
	current := s.clients[id]
	if current == nil {
		s.mu.Unlock()
		writeErr(w, 404, "client key not found")
		return
	}
	if current.view.RevokedAt != 0 {
		s.mu.Unlock()
		writeErr(w, 409, "client key is revoked")
		return
	}
	record := &clientKeyRecord{view: view, hash: hash, usage: current.usage, identity: &ClientIdentity{ID: view.ID, Name: view.Name, AllowedModels: view.AllowedModels, TPM: view.TPM, authHash: hash}}
	delete(s.keys, current.hash)
	s.keys[hash], s.clients[view.ID] = record, record
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"key": key, "client": view})
}

func (s *Security) handleClientKeyRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.CheckAdmin(w, r) {
		return
	}
	// Revocation must take effect immediately: publish the in-memory revocation
	// (drop the hash from s.keys, stamp RevokedAt) BEFORE the DB write, so inference
	// traffic is denied the moment the owner clicks. If the DB write then fails the
	// in-memory revocation stands (fail-closed) and can be retried — a failed write
	// must never silently re-enable a revoked key.
	id := r.PathValue("id")
	s.mu.Lock()
	record := s.clients[id]
	if record == nil {
		s.mu.Unlock()
		writeErr(w, 404, "client key not found")
		return
	}
	if record.view.RevokedAt != 0 {
		s.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
		return
	}
	revoked := time.Now().UnixMilli()
	record.view.RevokedAt = revoked
	delete(s.keys, record.hash)
	s.mu.Unlock()
	tx, err := s.control.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeErr(w, 500, "client key revocation failed")
		return
	}
	defer tx.Rollback()
	_, err = tx.Exec("UPDATE client_keys SET revoked_at=? WHERE id=?", revoked, record.view.ID)
	if err == nil {
		err = securityAudit(tx, "client_key.revoke", record.view.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		writeErr(w, 500, "client key revocation failed")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

type PolicyError struct {
	Status     int
	Message    string
	RetryAfter int
}

func (e *PolicyError) Error() string { return e.Message }

type ClientLease struct {
	Identity *ClientIdentity
	once     sync.Once
	security *Security
	usage    *clientUsage
	window   time.Time
	reserved uint64
}

// AcquireClient performs the policy check and all quota reservations under one
// lock. Usage belongs to the stable database ID, not the rotatable secret or
// policy snapshot; editing a policy cannot refill its current window.
func (s *Security) AcquireClient(r *http.Request, model string, tokenReservation uint64) (*ClientLease, *PolicyError) {
	identity := ClientIdentityFromRequest(r)
	if identity == nil {
		return nil, &PolicyError{Status: 401, Message: "valid client API key required"}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.keys[identity.authHash]
	if record == nil || record.view.ID != identity.ID || record.view.RevokedAt != 0 || (record.view.ExpiresAt != 0 && record.view.ExpiresAt <= now.UnixMilli()) {
		return nil, &PolicyError{Status: 401, Message: "valid client API key required"}
	}
	if !record.identity.AllowsModel(model) {
		return nil, &PolicyError{Status: 403, Message: "model is not allowed for this client key"}
	}
	// The proxy prepares conservative input/output bounds from the authenticated
	// policy snapshot. Enabling/changing TPM mid-request requires fresh preparation.
	if record.view.TPM > 0 && identity.TPM != record.view.TPM {
		return nil, &PolicyError{Status: http.StatusConflict, Message: "client token policy changed; retry the request"}
	}
	usage := record.usage
	if usage.window.IsZero() || now.Sub(usage.window) >= time.Minute {
		usage.window = now
		usage.requests = 0
		usage.tokens = 0
	}
	retryAfter := max(1, int(time.Until(usage.window.Add(time.Minute)).Seconds())+1)
	if record.view.MaxConcurrent > 0 && usage.active >= uint64(record.view.MaxConcurrent) {
		return nil, &PolicyError{Status: 429, Message: "client concurrency limit reached", RetryAfter: 1}
	}
	if record.view.RPM > 0 && usage.requests >= uint64(record.view.RPM) {
		return nil, &PolicyError{Status: 429, Message: "client request rate limit reached", RetryAfter: retryAfter}
	}
	if record.view.TPM > 0 && (tokenReservation > uint64(record.view.TPM) || usage.tokens > uint64(record.view.TPM)-tokenReservation) {
		return nil, &PolicyError{Status: 429, Message: "client token rate limit reached", RetryAfter: retryAfter}
	}
	usage.requests = saturatingTokenAdd(usage.requests, 1)
	usage.tokens = saturatingTokenAdd(usage.tokens, tokenReservation)
	usage.active++
	return &ClientLease{Identity: record.identity, security: s, usage: usage, window: usage.window, reserved: tokenReservation}, nil
}

func saturatingTokenAdd(a, b uint64) uint64 {
	if b > math.MaxUint64-a {
		return math.MaxUint64
	}
	return a + b
}

// Finish always releases concurrency, exactly once. Unknown usage retains its
// conservative reservation. A late completion cannot refund a newer window.
func (lease *ClientLease) Finish(actualTokens uint64, usageKnown bool) {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		lease.security.mu.Lock()
		defer lease.security.mu.Unlock()
		if lease.usage.active > 0 {
			lease.usage.active--
		}
		if !usageKnown || !lease.window.Equal(lease.usage.window) {
			return
		}
		if actualTokens >= lease.reserved {
			lease.usage.tokens = saturatingTokenAdd(lease.usage.tokens, actualTokens-lease.reserved)
		} else {
			refund := lease.reserved - actualTokens
			if refund > lease.usage.tokens {
				lease.usage.tokens = 0
			} else {
				lease.usage.tokens -= refund
			}
		}
	})
}
