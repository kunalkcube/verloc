package server

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestSpillBodyRoundTrip(t *testing.T) {
	old := nonStreamMemLimit
	nonStreamMemLimit = 1024
	defer func() { nonStreamMemLimit = old }()

	payload := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":5},"data":"` + strings.Repeat("x", 10000) + `"}`)
	sb, err := readSpillBody(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer sb.close()
	if sb.file == nil {
		t.Fatal("large body did not spill")
	}
	if sb.size != int64(len(payload)) {
		t.Fatalf("size = %d, want %d", sb.size, len(payload))
	}
	if !sb.validJSON() {
		t.Fatal("spilled body failed JSON validation")
	}
	got, err := io.ReadAll(sb.reader())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("spilled body does not round-trip")
	}
	if in, out, _, ok := usageFromReader(sb.reader()); !ok || in != 10 || out != 5 {
		t.Fatalf("usageFromReader = %d/%d ok=%v", in, out, ok)
	}
	// Small bodies stay in memory.
	small, err := readSpillBody(strings.NewReader(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer small.close()
	if small.file != nil {
		t.Fatal("small body spilled")
	}
	if !small.validJSON() || small.size != 11 {
		t.Fatal("small body invalid")
	}
}
