package server

import (
	"testing"
	"time"
)

func TestKeyPoolUntriedDuplicateAndAccountIdentity(t *testing.T) {
	pool := NewKeyPool([]KeyEntry{{ID: "first", Key: "same", AccountID: "a"}, {ID: "duplicate", Key: "same", AccountID: "a"}, {ID: "second-account", Key: "same", AccountID: "b"}}, time.Second)
	tried := make(map[credentialKey]bool)
	for range 2 {
		key := pool.NextUntried(tried)
		if key == nil {
			t.Fatal("distinct account was skipped")
		}
		tried[keyIdentity(key)] = true
	}
	if pool.NextUntried(tried) != nil {
		t.Fatal("duplicate credential was returned after exhaustion")
	}
}

func TestKeyPoolNextDoesNotAllocateHealthySlice(t *testing.T) {
	pool := NewKeyPool([]KeyEntry{{Key: "one"}, {Key: "two"}, {Key: "three"}}, time.Second)
	pool.states[1].cooldownUntil.Store(time.Now().Add(time.Hour).UnixNano())
	allocations := testing.AllocsPerRun(100, func() {
		key := pool.Next()
		if key == nil || key.Key == "two" {
			t.Fatal("unhealthy selection")
		}
	})
	if allocations != 0 {
		t.Fatalf("healthy selection allocated %v times", allocations)
	}
	for _, key := range pool.states {
		key.cooldownUntil.Store(time.Now().Add(time.Hour).UnixNano())
	}
	if pool.Next() != nil {
		t.Fatal("cooling pool returned a key")
	}
	if NewKeyPool(nil, time.Second).Next() != nil {
		t.Fatal("empty pool returned a key")
	}
}

func TestKeyPoolRetryAfterIsBounded(t *testing.T) {
	pool := NewKeyPool([]KeyEntry{{Key: "one"}}, time.Second)
	key := pool.Next()
	pool.MarkFailure(key, 24*time.Hour)
	remaining := time.Until(time.Unix(0, key.cooldownUntil.Load()))
	if remaining > time.Hour || remaining < 59*time.Minute {
		t.Fatalf("cooldown=%v", remaining)
	}
}
