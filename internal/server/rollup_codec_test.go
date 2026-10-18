package server

import (
	"bytes"
	"testing"
)

func TestDecodeMinutesRejectsOversizedCounts(t *testing.T) {
	// Version tag + uvarint(1<<40) and nothing else.
	blob := []byte{dimCodecVersion, 0x80, 0x80, 0x80, 0x80, 0x80, 0x04}
	var models map[string]deltaModel
	var backends map[string]deltaBackend
	var hist, ttft []uint64
	if err := decodeMinutes(blob, &models, &backends, &hist, &ttft); err == nil {
		t.Fatal("oversized model count decoded without error")
	}
	// A valid empty blob still decodes cleanly.
	if err := decodeMinutes([]byte{dimCodecVersion, 0, 0, 0, 0}, &models, &backends, &hist, &ttft); err != nil {
		t.Fatalf("empty blob: %v", err)
	}
}

// A1: encode/decode round-trip still works after the bounds checks.
func TestRollupCodecRoundTripUnchanged(t *testing.T) {
	models := map[string]deltaModel{"nv/x": {Requests: 3, TokIn: 10, Cost: 1.5}}
	backends := map[string]deltaBackend{"nvidia": {Requests: 3, Err: 1, Cost: 1.5}}
	blob := encodeMinutes(&bytes.Buffer{}, models, backends, []uint64{1, 2}, []uint64{4})
	var m map[string]deltaModel
	var b map[string]deltaBackend
	var h, th []uint64
	if err := decodeMinutes(blob, &m, &b, &h, &th); err != nil {
		t.Fatal(err)
	}
	if m["nv/x"].Requests != 3 || b["nvidia"].Err != 1 || len(h) != 2 || len(th) != 1 {
		t.Fatalf("round trip mismatch: %v %v %v %v", m, b, h, th)
	}
}

// A2: an upstream that never sends a blank-line event boundary must flush
// partial events instead of buffering without limit, and a legitimately
// large single event must still complete (flushed in pieces).
