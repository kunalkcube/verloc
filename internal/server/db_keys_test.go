package server

import (
	"path/filepath"
	"testing"
	"time"
)

func testKeyEventStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitForKeyEvents(t *testing.T, s *Store, want int, timeout time.Duration) []KeyEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := s.RecentKeyEvents(want); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d key events", want)
	return nil
}

func TestQueueKeyEventPersisted(t *testing.T) {
	s := testKeyEventStore(t)
	now := time.Now().UnixMilli()
	s.QueueKeyEvent(now, "acme", "...111", "cooldown", 0, "30s")
	s.QueueKeyEvent(now, "acme", "...111", "recover", 0, "")
	got := waitForKeyEvents(t, s, 2, 5*time.Second)
	if got[0].Event != "recover" || got[1].Event != "cooldown" {
		t.Fatalf("newest-first order: %+v", got)
	}
}

// TestQueueKeyEventNeverBlocks floods the queue: enqueue must return
// promptly even when the writer falls behind, with drops accounted for
// (dropped + persisted == enqueued once drained).
func TestQueueKeyEventNeverBlocks(t *testing.T) {
	s := testKeyEventStore(t)
	const total = 5000
	now := time.Now().UnixMilli()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			s.QueueKeyEvent(now, "acme", "...111", "cooldown", 0, "30s")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("QueueKeyEvent blocked under flood")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n := len(s.RecentKeyEvents(total))
		if uint64(n)+s.DroppedKeyEvents() == total {
			t.Logf("persisted=%d dropped=%d", n, s.DroppedKeyEvents())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("accounting gap: persisted=%d dropped=%d want=%d",
		len(s.RecentKeyEvents(total)), s.DroppedKeyEvents(), total)
}
