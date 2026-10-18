package server

import (
	"math"
	"testing"
)

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
