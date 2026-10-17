package server

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func createSecurityTestKey(t *testing.T, e *securityTestEnv, cookie *http.Cookie, csrf string, policy clientKeyRequest) (string, ClientKeyView) {
	t.Helper()
	if len(policy.AllowedModels) == 0 {
		policy.AllowedModels = []string{"*"} // unrestricted test key, stated explicitly
	}
	w := e.request("POST", "/dash/api/admin/client-keys", policy, cookie, csrf)
	if w.Code != 201 {
		t.Fatalf("create client key: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Key    string        `json:"key"`
		Client ClientKeyView `json:"client"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Key) != 47 || response.Client.ID == "" {
		t.Fatal("missing generated key or stable ID")
	}
	return response.Key, response.Client
}

func authenticatedClientRequest(t *testing.T, s *Security, key string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/inference-test", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	var authenticated *http.Request
	w := httptest.NewRecorder()
	s.ClientMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { authenticated = r; w.WriteHeader(204) })).ServeHTTP(w, r)
	if w.Code != 204 || authenticated == nil {
		t.Fatalf("key authentication: %d %s", w.Code, w.Body.String())
	}
	return authenticated
}

func assertClientKeyRejected(t *testing.T, s *Security, key string) {
	t.Helper()
	r := httptest.NewRequest("POST", "/inference-test", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.ClientMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("invalid key reached inference handler") })).ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("rejected key status: %d", w.Code)
	}
}

func TestClientKeyLifecycleAndPrivilegeSeparation(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	if w := e.request("POST", "/dash/api/admin/client-keys", clientKeyRequest{Name: "anonymous"}, nil, ""); w.Code != 401 {
		t.Fatalf("anonymous key creation: %d", w.Code)
	}
	if w := e.request("POST", "/dash/api/admin/client-keys", clientKeyRequest{Name: "missing csrf"}, cookie, ""); w.Code != 403 {
		t.Fatalf("key creation without CSRF: %d", w.Code)
	}
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "worker", AllowedModels: []string{"m", "m"}, RPM: 20})
	if len(view.AllowedModels) != 1 {
		t.Fatal("allowlist was not normalized")
	}
	r := authenticatedClientRequest(t, e.security, key)
	identity := ClientIdentityFromRequest(r)
	if identity.ID != view.ID || identity.Name != "worker" || !identity.AllowsModel("m") || identity.AllowsModel("other") {
		t.Fatalf("invalid client identity: %+v", identity)
	}
	adminReq := httptest.NewRequest("GET", "/dash/api/admin/client-keys", nil)
	adminReq.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, adminReq)
	if w.Code != 401 {
		t.Fatalf("inference key administered gateway: %d", w.Code)
	}
	list := e.request("GET", "/dash/api/admin/client-keys", nil, cookie, "")
	if list.Code != 200 || strings.Contains(list.Body.String(), key) || strings.Contains(list.Body.String(), "key_hash") {
		t.Fatalf("unsafe key listing: %d %s", list.Code, list.Body.String())
	}
	var stored []byte
	if err := e.store.db.QueryRow("SELECT key_hash FROM client_keys WHERE id=?", view.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256([]byte(key))
	if string(stored) != string(expected[:]) {
		t.Fatal("client secret not stored as SHA-256 hash")
	}
	rotation := e.request("POST", "/dash/api/admin/client-keys/"+view.ID+"/rotate", nil, cookie, csrf)
	if rotation.Code != 200 {
		t.Fatalf("rotation: %d %s", rotation.Code, rotation.Body.String())
	}
	var rotated struct {
		Key    string        `json:"key"`
		Client ClientKeyView `json:"client"`
	}
	if err := json.Unmarshal(rotation.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Client.ID != view.ID || rotated.Key == key {
		t.Fatal("rotation changed identity or reused secret")
	}
	assertClientKeyRejected(t, e.security, key)
	if _, err := e.security.AcquireClient(r, "m", 1); err == nil || err.Status != 401 {
		t.Fatal("already-authenticated old secret survived rotation")
	}
	rotatedRequest := authenticatedClientRequest(t, e.security, rotated.Key)
	if w := e.request("DELETE", "/dash/api/admin/client-keys/"+view.ID, nil, cookie, csrf); w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	assertClientKeyRejected(t, e.security, rotated.Key)
	if _, err := e.security.AcquireClient(rotatedRequest, "m", 1); err == nil || err.Status != 401 {
		t.Fatal("already-authenticated key survived revocation")
	}
	if w := e.request("POST", "/dash/api/admin/client-keys/"+view.ID+"/rotate", nil, cookie, csrf); w.Code != 409 {
		t.Fatalf("revoked key revived by rotation: %d", w.Code)
	}
	var revoked int64
	if err := e.store.db.QueryRow("SELECT revoked_at FROM client_keys WHERE id=?", view.ID).Scan(&revoked); err != nil || revoked == 0 {
		t.Fatalf("revocation history lost: %d %v", revoked, err)
	}
	assertClientKeyRejected(t, e.security, "old-shared-proxy-key")
	assertClientKeyRejected(t, e.security, cookie.Value)
}

func TestClientPolicyAtomicAdmissionAndIsolatedKeys(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "limited", AllowedModels: []string{"m"}, RPM: 4, TPM: 100, MaxConcurrent: 3})
	r := authenticatedClientRequest(t, e.security, key)
	if _, err := e.security.AcquireClient(r, "forbidden", 100); err == nil || err.Status != 403 {
		t.Fatal("model allowlist not enforced before quota")
	}
	var wg sync.WaitGroup
	leases := make(chan *ClientLease, 32)
	for range 32 {
		wg.Go(func() {
			lease, err := e.security.AcquireClient(r, "m", 25)
			if err == nil {
				leases <- lease
			} else if err.Status != 429 {
				t.Errorf("concurrent admission: %v", err)
			}
		})
	}
	wg.Wait()
	close(leases)
	if len(leases) != 3 {
		t.Fatalf("admitted %d concurrent requests, want 3", len(leases))
	}
	var held []*ClientLease
	for lease := range leases {
		held = append(held, lease)
	}
	held[0].Finish(10, true)
	held[0].Finish(0, true)
	fourth, policyErr := e.security.AcquireClient(r, "m", 40)
	if policyErr != nil {
		t.Fatalf("usage adjustment did not refund 15 tokens: %v", policyErr)
	}
	if fourth.Identity.ID != view.ID {
		t.Fatal("lease identity missing")
	}
	fourth.Finish(40, false)
	for _, lease := range held[1:] {
		lease.Finish(0, false)
	}
	if _, err := e.security.AcquireClient(r, "m", 0); err == nil || err.Status != 429 {
		t.Fatal("request limit bypassed after completion")
	}
	other, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "independent", RPM: 1, TPM: 1, MaxConcurrent: 1})
	second, err := e.security.AcquireClient(authenticatedClientRequest(t, e.security, other), "anything", 1)
	if err != nil {
		t.Fatalf("key limits were not isolated: %v", err)
	}
	second.Finish(1, true)
}

func TestClientPolicySurvivesRotationAndEdit(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "initial", RPM: 1, TPM: 10, MaxConcurrent: 1})
	lease, err := e.security.AcquireClient(authenticatedClientRequest(t, e.security, key), "m", 10)
	if err != nil {
		t.Fatal(err)
	}
	rotation := e.request("POST", "/dash/api/admin/client-keys/"+view.ID+"/rotate", nil, cookie, csrf)
	if rotation.Code != 200 {
		t.Fatalf("rotate: %d", rotation.Code)
	}
	var rotated struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rotation.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	r := authenticatedClientRequest(t, e.security, rotated.Key)
	if _, err := e.security.AcquireClient(r, "m", 0); err == nil {
		t.Fatal("rotation reset active admission")
	}
	policy := clientKeyRequest{Name: "renamed", RPM: 1, TPM: 10, MaxConcurrent: 1, AllowedModels: []string{"other"}}
	if w := e.request("PUT", "/dash/api/admin/client-keys/"+view.ID, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	if _, err := e.security.AcquireClient(r, "m", 0); err == nil || err.Status != 403 {
		t.Fatal("request authenticated before policy edit bypassed new allowlist")
	}
	lease.Finish(2, true)
	if _, err := e.security.AcquireClient(r, "other", 0); err == nil || err.Status != 429 {
		t.Fatal("edit or rotation refilled request quota")
	}
	// Requests admitted while a limit is unlimited are still counted, so setting
	// a limit later cannot create a fresh quota window.
	policy.RPM, policy.TPM, policy.MaxConcurrent = 0, 0, 0
	if w := e.request("PUT", "/dash/api/admin/client-keys/"+view.ID, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("remove limits: %d", w.Code)
	}
	unlimited, err := e.security.AcquireClient(r, "other", 20)
	if err != nil {
		t.Fatal(err)
	}
	unlimited.Finish(20, false)
	policy.RPM, policy.TPM = 1, 10
	if w := e.request("PUT", "/dash/api/admin/client-keys/"+view.ID, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("restore limits: %d", w.Code)
	}
	if _, err := e.security.AcquireClient(r, "other", 0); err == nil || err.Status != 429 {
		t.Fatal("unlimited policy edit discarded accounting")
	}
}

func TestClientTokenAdjustmentAndOldWindowRelease(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "tokens", TPM: 10, MaxConcurrent: 2})
	r := authenticatedClientRequest(t, e.security, key)
	if _, err := e.security.AcquireClient(r, "m", math.MaxUint64); err == nil || err.Status != 429 {
		t.Fatal("token reservation overflow bypassed limit")
	}
	first, err := e.security.AcquireClient(r, "m", 10)
	if err != nil {
		t.Fatal(err)
	}
	first.Finish(4, true)
	second, err := e.security.AcquireClient(r, "m", 6)
	if err != nil {
		t.Fatalf("known usage failed to refund reservation: %v", err)
	}
	second.Finish(20, true)
	if _, err := e.security.AcquireClient(r, "m", 0); err == nil {
		t.Fatal("actual usage above reservation did not consume quota")
	}
	e.security.mu.Lock()
	e.security.clients[view.ID].usage.window = time.Now().Add(-2 * time.Minute)
	e.security.mu.Unlock()
	old, err := e.security.AcquireClient(r, "m", 10)
	if err != nil {
		t.Fatal(err)
	}
	e.security.mu.Lock()
	e.security.clients[view.ID].usage.window = time.Now().Add(-2 * time.Minute)
	e.security.mu.Unlock()
	current, err := e.security.AcquireClient(r, "m", 10)
	if err != nil {
		t.Fatal(err)
	}
	old.Finish(0, true)
	if _, err := e.security.AcquireClient(r, "m", 1); err == nil || err.Status != 429 {
		t.Fatal("old lease refunded a new token window")
	}
	current.Finish(10, false)
}

func TestClientKeyExpiryAndCacheReload(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "expiring", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()})
	e.startSecurity(t)
	r := authenticatedClientRequest(t, e.security, key)
	e.security.mu.Lock()
	e.security.clients[view.ID].view.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	e.security.mu.Unlock()
	assertClientKeyRejected(t, e.security, key)
	if _, err := e.security.AcquireClient(r, "m", 0); err == nil || err.Status != 401 {
		t.Fatal("already-authenticated request bypassed expiry")
	}
}

func TestClientIdentityAllowlistAndSaturatingAccounting(t *testing.T) {
	if (*ClientIdentity)(nil).AllowsModel("m") {
		t.Fatal("nil identity allows a model")
	}
	if (&ClientIdentity{}).AllowsModel("m") {
		t.Fatal("empty allowlist must allow nothing — access is explicit")
	}
	if !(&ClientIdentity{AllowedModels: []string{"*"}}).AllowsModel("m") {
		t.Fatal("wildcard allowlist must allow configured models")
	}
	if (&ClientIdentity{AllowedModels: []string{"m"}}).AllowsModel("M") {
		t.Fatal("allowlist model IDs must match exactly")
	}
	if saturatingTokenAdd(math.MaxUint64-1, 10) != math.MaxUint64 {
		t.Fatal("token accounting overflowed")
	}
}

func TestClientTPMPolicyEditRequiresFreshAuthenticatedBounds(t *testing.T) {
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "policy race"})
	unlimitedRequest := authenticatedClientRequest(t, e.security, key)
	if ClientIdentityFromRequest(unlimitedRequest).TPM != 0 {
		t.Fatal("unlimited TPM snapshot is incorrect")
	}
	policy := clientKeyRequest{Name: "policy race", TPM: 100, AllowedModels: []string{"*"}}
	path := "/dash/api/admin/client-keys/" + view.ID
	if w := e.request("PUT", path, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("enable TPM: %d", w.Code)
	}
	if _, err := e.security.AcquireClient(unlimitedRequest, "m", 0); err == nil || err.Status != 409 {
		t.Fatal("new TPM policy accepted a request prepared without bounds")
	}
	boundedRequest := authenticatedClientRequest(t, e.security, key)
	if ClientIdentityFromRequest(boundedRequest).TPM != 100 {
		t.Fatal("updated TPM snapshot is incorrect")
	}
	lease, err := e.security.AcquireClient(boundedRequest, "m", 60)
	if err != nil {
		t.Fatalf("fresh policy snapshot rejected: %v", err)
	}
	lease.Finish(60, false)
	policy.TPM = 200
	if w := e.request("PUT", path, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("change TPM: %d", w.Code)
	}
	if _, err := e.security.AcquireClient(boundedRequest, "m", 0); err == nil || err.Status != 409 {
		t.Fatal("changed positive TPM accepted stale preparation")
	}
	freshRequest := authenticatedClientRequest(t, e.security, key)
	if ClientIdentityFromRequest(freshRequest).TPM != 200 {
		t.Fatal("changed TPM not published")
	}
	remaining, err := e.security.AcquireClient(freshRequest, "m", 140)
	if err != nil {
		t.Fatalf("TPM edit discarded prior usage or charged rejected requests: %v", err)
	}
	remaining.Finish(140, false)
	if _, err := e.security.AcquireClient(freshRequest, "m", 1); err == nil || err.Status != 429 {
		t.Fatal("TPM edit reset stable accounting")
	}
	policy.TPM = 0
	if w := e.request("PUT", path, policy, cookie, csrf); w.Code != 200 {
		t.Fatalf("disable TPM: %d", w.Code)
	}
	unlimited, err := e.security.AcquireClient(freshRequest, "m", 1)
	if err != nil {
		t.Fatalf("removing TPM needlessly rejects prepared bounded request: %v", err)
	}
	unlimited.Finish(1, false)
}
