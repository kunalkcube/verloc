package server

import (
	"strings"
	"testing"
	"time"
)

func TestScopeTicksCarryErrAndFailover(t *testing.T) {
	td := TickDelta{
		Models:   map[string]setDelta{"nv/x": {Req: 10, Err: 2, Failover: 3}},
		Backends: map[string]setDelta{"nvidia": {Req: 10, Err: 2, Failover: 3}},
	}
	models, backends := scopeTicks(deltaFromTick(td))
	if got := models["nv/x"]; got.Err != 2 || got.Failover != 3 {
		t.Fatalf("model scope lost err/failover: %+v", got)
	}
	if got := backends["nvidia"]; got.Err != 2 || got.Failover != 3 {
		t.Fatalf("backend scope lost err/failover: %+v", got)
	}
}

// D2: telemetry_errors must be delta-based. With no store the value is 0;
// the baseline behaviour is pinned with a real store in store tests.
func TestTelemetryErrorDeltaWithoutStore(t *testing.T) {
	e := NewAlertEngine(nil, nil, nil)
	if v := e.telemetryErrorDelta(); v != 0 {
		t.Fatalf("delta without store = %v, want 0", v)
	}
}

// D3: the default TTFT rule renders with a unit.
func TestAlertMessageTTFTUnit(t *testing.T) {
	r := &AlertRule{Metric: "ttft_p95", Scope: "global", Threshold: 15000}
	if msg := alertMessage(r, 20000); !strings.Contains(msg, " ms") {
		t.Fatalf("ttft_p95 message lacks unit: %q", msg)
	}
}

// C1: dynamic JSON responses are gzipped only when accepted, never for
// streaming paths.

func TestAggregatorPrunesDeadScopes(t *testing.T) {
	a := NewAggregator(100, 60) // retain 60 minutes
	old := time.Now().Add(-3 * time.Hour).UnixMilli()
	a.Feed(tickAgg{TS: old, Req: 1}, map[string]tickAgg{"gone": {Req: 1}}, nil)
	now := time.Now().UnixMilli()
	a.Feed(tickAgg{TS: now, Req: 1}, map[string]tickAgg{"alive": {Req: 1}}, nil)
	if _, ok := a.byScope["model:gone"]; ok {
		t.Fatal("dead scope was not pruned")
	}
	if _, ok := a.byScope["model:alive"]; !ok {
		t.Fatal("live scope was pruned")
	}
}
