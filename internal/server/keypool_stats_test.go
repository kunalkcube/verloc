package server

// Concurrency guard for KeyPool.Stats buffer reuse. Deleted if the reuse is
// reverted; kept as a permanent regression test if the reuse stays.

import (
	"sync"
	"testing"
)

// TestKeyPoolStatsConcurrentSafety exercises Stats from multiple goroutines,
// which is what happens when two dashboard polls (or a poll plus the ticker)
// call Snapshot concurrently. If Stats returns a shared backing array, the
// readers alias each other and the output is corrupted.
func TestKeyPoolStatsConcurrentSafety(t *testing.T) {
	keys := make([]KeyEntry, 8)
	for i := range keys {
		keys[i] = KeyEntry{Key: "key-" + string(rune('a'+i))}
	}
	p := NewKeyPool(keys, 0)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				got := p.Stats()
				if len(got) != len(keys) {
					t.Errorf("stats length %d, want %d", len(got), len(keys))
					return
				}
				// Each returned slice must be independently valid: every entry
				// must carry a distinct masked key from the pool.
				seen := map[string]bool{}
				for _, k := range got {
					if k.Key == "" {
						t.Errorf("empty masked key in stats (aliased/corrupted buffer)")
						return
					}
					if seen[k.Key] {
						t.Errorf("duplicate masked key %q (aliased buffer)", k.Key)
						return
					}
					seen[k.Key] = true
				}
			}
		}()
	}
	wg.Wait()
}
