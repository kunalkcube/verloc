package server

import (
	"math"
	"strings"
	"testing"
)

// A3: subscriptions are capped; Publish with no subscribers is a no-op.
func TestHubSubscriberCap(t *testing.T) {
	h := NewHub()
	for i := 0; i < maxHubSubscribers; i++ {
		ch := h.Subscribe()
		if !h.registered(ch) {
			t.Fatalf("subscriber %d rejected below cap", i)
		}
	}
	over := h.Subscribe()
	if h.registered(over) {
		t.Fatal("subscriber beyond cap was registered")
	}
	if _, ok := <-over; ok {
		t.Fatal("rejected subscriber channel was not closed")
	}
	h.Publish(map[string]any{"type": "x"}) // must not block
}

// B2: model access is explicit — "*" allows, empty denies.
func TestWildcardModelAccess(t *testing.T) {
	if !(&ClientIdentity{AllowedModels: []string{"*"}}).AllowsModel("any/model") {
		t.Fatal("wildcard must allow every model")
	}
	if (&ClientIdentity{AllowedModels: nil}).AllowsModel("any/model") {
		t.Fatal("empty allowlist must deny")
	}
	if err := validateClientKeyRequest(&clientKeyRequest{Name: "k"}); err == nil {
		t.Fatal("key creation without models must be rejected")
	}
	if err := validateClientKeyRequest(&clientKeyRequest{Name: "k", AllowedModels: []string{"*"}}); err != nil {
		t.Fatalf("wildcard key rejected: %v", err)
	}
}

// B5: the SSRF guard holds on the persist path, not only on probes.
func TestProviderPersistRejectsPrivateUpstream(t *testing.T) {
	cfg := &Config{Providers: map[string]*ProviderConfig{}}
	err := applyProviderRequest(cfg, providerUpsertReq{Name: "lan", BaseURL: "http://127.0.0.1:11434/v1", NoAuth: true})
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("private upstream persisted without opt-in: %v", err)
	}
	if err := applyProviderRequest(cfg, providerUpsertReq{Name: "lan", BaseURL: "http://127.0.0.1:11434/v1", NoAuth: true, AllowPrivateUpstreams: true}); err != nil {
		t.Fatalf("private upstream with explicit opt-in rejected: %v", err)
	}
}

// B6/D16: a %ACCOUNT% placeholder must not defeat the probe guard.
func TestProbeGuardHandlesAccountPlaceholder(t *testing.T) {
	if err := guardProbeHost(t.Context(), "https://api.cloudflare.com/client/v4/accounts/account/ai/v1", false); err != nil {
		t.Fatalf("public placeholder URL refused: %v", err)
	}
}

// D7: a non-finite tick cost is clamped instead of poisoning lifetime sums.
func TestInsertDeltaClampsNonFiniteCost(t *testing.T) {
	s := testKeyEventStore(t)
	d := &Delta{TS: 7200000, Cost: math.NaN(), Models: map[string]deltaModel{}, Backends: map[string]deltaBackend{}}
	if err := s.InsertDelta(d); err != nil {
		t.Fatal(err)
	}
	if d.Cost != 0 {
		t.Fatalf("NaN cost not clamped: %v", d.Cost)
	}
	cost, _, _, _, _, _ := s.LifetimeTotals()
	if math.IsNaN(cost) {
		t.Fatal("lifetime cost poisoned by NaN tick")
	}
}
