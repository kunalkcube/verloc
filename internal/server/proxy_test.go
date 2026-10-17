package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRemapBodySetsUpstreamModel(t *testing.T) {
	in := []byte(`{"model":"agent-id","messages":[{"role":"user","content":"hi"}]}`)
	out := remapBody(in, "upstream/model", false, "vercel")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "upstream/model" {
		t.Fatalf("model=%v", m["model"])
	}
}

func TestRemapBodyDeveloperRoleFreeinference(t *testing.T) {
	in := []byte(`{"model":"x","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`)
	out := remapBody(in, "minimax-m3", false, "freeinference")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	role := msgs[0].(map[string]any)["role"]
	if role != "system" {
		t.Fatalf("role=%v want system", role)
	}
}

func TestRemapBodyDeveloperRolePassthroughNonMistral(t *testing.T) {
	in := []byte(`{"model":"x","messages":[{"role":"developer","content":"sys"}]}`)
	out := remapBody(in, "m", false, "vercel")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	role := m["messages"].([]any)[0].(map[string]any)["role"]
	if role != "developer" {
		t.Fatalf("role=%v; non-mistral should keep developer", role)
	}
}

func TestRemapBodyMistralFlattensTextParts(t *testing.T) {
	in := []byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":" world"}]}]}`)
	out := remapBody(in, "mistral-medium-3-5", true, "mistral")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	content := m["messages"].([]any)[0].(map[string]any)["content"]
	s, ok := content.(string)
	if !ok || s != "hello world" {
		t.Fatalf("content=%v (%T)", content, content)
	}
}

func TestRemapBodyMistralAnthropicTools(t *testing.T) {
	in := []byte(`{
	  "model":"x",
	  "messages":[
	    {"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"bash","input":{"cmd":"ls"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}
	  ]
	}`)
	out := remapBody(in, "mistral-medium-3-5", true, "mistral")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("msgs=%d", len(msgs))
	}
	asst := msgs[0].(map[string]any)
	if _, ok := asst["tool_calls"]; !ok {
		t.Fatalf("missing tool_calls: %#v", asst)
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "tu1" {
		t.Fatalf("tool msg=%#v", tool)
	}
}

func TestRemapBodyMistralDedupsToolCallIDs(t *testing.T) {
	in := []byte(`{
	  "model":"x",
	  "messages":[
	    {"role":"assistant","content":null,"tool_calls":[
	      {"id":"chatcmplt","type":"function","function":{"name":"a","arguments":"{}"}},
	      {"id":"chatcmplt","type":"function","function":{"name":"b","arguments":"{}"}}
	    ]},
	    {"role":"tool","tool_call_id":"chatcmplt","content":"1"},
	    {"role":"tool","tool_call_id":"chatcmplt","content":"2"}
	  ]
	}`)
	out := remapBody(in, "mistral-medium-3-5", true, "mistral")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs := m["messages"].([]any)
	asst := msgs[0].(map[string]any)
	calls := asst["tool_calls"].([]any)
	id0 := calls[0].(map[string]any)["id"].(string)
	id1 := calls[1].(map[string]any)["id"].(string)
	if id0 == id1 {
		t.Fatalf("duplicate ids remained: %s %s", id0, id1)
	}
	r1 := msgs[1].(map[string]any)["tool_call_id"].(string)
	r2 := msgs[2].(map[string]any)["tool_call_id"].(string)
	if r1 != id0 || r2 != id1 {
		t.Fatalf("result ids %s/%s vs call ids %s/%s", r1, r2, id0, id1)
	}
}

func TestRemapBodyMistralMaxCompletionTokens(t *testing.T) {
	in := []byte(`{"model":"x","max_completion_tokens":100,"messages":[]}`)
	out := remapBody(in, "m", true, "mistral")
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["max_completion_tokens"]; ok {
		t.Fatal("max_completion_tokens should be stripped")
	}
	if m["max_tokens"] == nil {
		t.Fatal("max_tokens missing")
	}
}

func TestStripReasoningContent(t *testing.T) {
	in := []byte(`{"messages":[{"role":"assistant","content":"hi","reasoning_content":"secret","reasoning":"also"}]}`)
	out := stripReasoningContent(in, true)
	s := string(out)
	if strings.Contains(s, "reasoning_content") || strings.Contains(s, `"reasoning"`) {
		t.Fatalf("still present: %s", s)
	}
	kept := stripReasoningContent(in, false)
	if !strings.Contains(string(kept), "reasoning_content") {
		t.Fatal("strip=false should keep fields")
	}
}

func TestCapMaxTokens(t *testing.T) {
	in := []byte(`{"max_tokens":999999,"max_completion_tokens":999999}`)
	out := capMaxTokens(in, 32768)
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		n, err := m[field].(json.Number).Int64()
		if err != nil || n != 32768 {
			t.Fatalf("%s=%v", field, m[field])
		}
	}
	uncapped := capMaxTokens(in, 0)
	if string(uncapped) != string(in) {
		t.Fatalf("limit 0 should be no-op")
	}
}

func TestIsRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusOK:                  false,
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        true,
		http.StatusPaymentRequired:     true,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
	}
	for code, want := range cases {
		if got := isRetryableStatus(code); got != want {
			t.Fatalf("status %d: got %v want %v", code, got, want)
		}
	}
}
