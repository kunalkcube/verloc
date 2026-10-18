package server

import (
	"testing"
)

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
