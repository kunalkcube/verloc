package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEnsureReasoningReplayCopy(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"","reasoning":"think","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`)
	out := ensureReasoningReplay(raw, newReasoningCache(8))
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["messages"].([]any)[0].(map[string]any)
	if msg["reasoning_content"] != "think" {
		t.Fatalf("got %#v", msg["reasoning_content"])
	}
}

func TestEnsureReasoningReplayFromCache(t *testing.T) {
	cache := newReasoningCache(8)
	cache.put("c1", "prior think")
	raw := []byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`)
	out := ensureReasoningReplay(raw, cache)
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["messages"].([]any)[0].(map[string]any)
	if msg["reasoning_content"] != "prior think" {
		t.Fatalf("got %#v", msg["reasoning_content"])
	}
}

func TestEnsureReasoningReplayEmptyOnToolCalls(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]}]}`)
	out := ensureReasoningReplay(raw, newReasoningCache(8))
	var p map[string]any
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	msg := p["messages"].([]any)[0].(map[string]any)
	s, ok := msg["reasoning_content"].(string)
	if !ok || s != "" {
		t.Fatalf("expected empty reasoning_content, got %#v", msg["reasoning_content"])
	}
}

func TestEnsureReasoningReplaySkipsPlainAssistant(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"assistant","content":"hello"}]}`)
	out := ensureReasoningReplay(raw, newReasoningCache(8))
	if string(out) != string(raw) {
		t.Fatalf("should pass through unchanged: %s", out)
	}
}

func TestSseReasoningAccStore(t *testing.T) {
	cache := newReasoningCache(8)
	acc := &sseReasoningAcc{}
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning_content":"ab"}}]}`))
	acc.ingest([]byte(`{"choices":[{"delta":{"reasoning":"cd","tool_calls":[{"index":0,"id":"c1"}]}}]}`))
	acc.store(cache)
	if got := cache.get("c1"); got != "abcd" {
		t.Fatalf("got %q", got)
	}
}

func TestAliasReasoningInChunk(t *testing.T) {
	in := []byte(`{"choices":[{"delta":{"reasoning":"think"}}]}`)
	out, ok := aliasReasoningInChunk(in)
	if !ok {
		t.Fatal("expected rewrite")
	}
	if !strings.Contains(string(out), `"reasoning_content":"think"`) {
		t.Fatalf("got %s", out)
	}
}
