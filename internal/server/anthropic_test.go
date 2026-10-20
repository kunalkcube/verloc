package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Request translation: Anthropic Messages → OpenAI chat completions
// ---------------------------------------------------------------------------

func TestAnthropicToOpenAIBasic(t *testing.T) {
	body := []byte(`{
		"model": "nv/test-model",
		"max_tokens": 1024,
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": "Hello"}
		],
		"temperature": 0.5
	}`)
	chat, parsed, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Model != "nv/test-model" || parsed.MaxTokens != 1024 {
		t.Fatalf("parsed request wrong: %+v", parsed)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d want 2 (system + user)", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "You are helpful." {
		t.Fatalf("system message wrong: %v", sys)
	}
	user, _ := msgs[1].(map[string]any)
	if user["role"] != "user" || user["content"] != "Hello" {
		t.Fatalf("user message wrong: %v", user)
	}
	if chat["max_tokens"] != uint64(1024) && chat["max_tokens"] != float64(1024) {
		t.Fatalf("max_tokens not carried: %v", chat["max_tokens"])
	}
	if v, ok := chat["temperature"].(float64); !ok || v != 0.5 {
		t.Fatalf("temperature not carried: %v", chat["temperature"])
	}
}

func TestAnthropicToOpenAIContentBlocks(t *testing.T) {
	body := []byte(`{
		"model": "nv/test",
		"max_tokens": 100,
		"system": [{"type": "text", "text": "sys part 1"}, {"type": "text", "text": "sys part 2"}],
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "before"},
				{"type": "tool_result", "tool_use_id": "tu_1", "content": "result text"},
				{"type": "text", "text": "after"}
			]}
		]
	}`)
	chat, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3 (system, tool, user)", len(msgs))
	}
	tool, _ := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "tu_1" || tool["content"] != "result text" {
		t.Fatalf("tool message wrong: %v", tool)
	}
	user, _ := msgs[2].(map[string]any)
	if user["role"] != "user" || user["content"] != "beforeafter" {
		t.Fatalf("user text wrong: %v", user)
	}
	sys, _ := msgs[0].(map[string]any)
	if sys["content"] != "sys part 1sys part 2" {
		t.Fatalf("system blocks not folded: %v", sys)
	}
}

func TestAnthropicToOpenAIToolUseAndImages(t *testing.T) {
	body := []byte(`{
		"model": "nv/test",
		"max_tokens": 100,
		"messages": [
			{"role": "user", "content": "run the tool"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "internal", "signature": "sig123"},
				{"type": "tool_use", "id": "tu_9", "name": "get_weather", "input": {"city": "SF"}},
				{"type": "text", "text": "calling"}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_9", "content": [{"type": "text", "text": "sunny"}]},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aW1n"}}
			]}
		],
		"tools": [{"name": "get_weather", "description": "weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
		"tool_choice": {"type": "tool", "name": "get_weather"}
	}`)
	chat, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := chat["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4 (user, assistant, tool, image-user)", len(msgs))
	}
	assistant, _ := msgs[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%d want 1", len(calls))
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SF"}` {
		t.Fatalf("tool call wrong: %v", call)
	}
	if assistant["content"] != "calling" {
		t.Fatalf("assistant text lost: %v", assistant["content"])
	}
	// The thinking block must not appear anywhere in the forwarded body.
	encoded, _ := json.Marshal(chat)
	if strings.Contains(string(encoded), "sig123") || strings.Contains(string(encoded), `"thinking"`) {
		t.Fatal("thinking/signature leaked into forwarded body")
	}
	// tool_result becomes a tool message, image + no text becomes parts.
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu_9" || toolMsg["content"] != "sunny" {
		t.Fatalf("tool result message wrong: %v", toolMsg)
	}
	imgUser, _ := msgs[3].(map[string]any)
	parts, _ := imgUser["content"].([]any)
	_ = parts
	// tools + tool_choice
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%d want 1", len(tools))
	}
	choice, _ := chat["tool_choice"].(map[string]any)
	cfn, _ := choice["function"].(map[string]any)
	if cfn["name"] != "get_weather" {
		t.Fatalf("tool_choice wrong: %v", chat["tool_choice"])
	}
}

func TestAnthropicToOpenAIValidation(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing model", `{"max_tokens":10,"messages":[{"role":"user","content":"x"}]}`},
		{"missing messages", `{"model":"m","max_tokens":10}`},
		{"missing max_tokens", `{"model":"m","messages":[{"role":"user","content":"x"}]}`},
		{"invalid json", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := anthropicToOpenAI([]byte(tc.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestAnthropicToOpenAIDropsClientOnlyFields(t *testing.T) {
	body := []byte(`{
		"model": "nv/test", "max_tokens": 10,
		"messages": [{"role": "user", "content": "x"}],
		"thinking": {"type": "adaptive"},
		"context_management": {"edits": []},
		"output_config": {"effort": "high"},
		"metadata": {"user_id": "u"},
		"service_tier": "auto",
		"top_k": 40
	}`)
	chat, _, err := anthropicToOpenAI(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"thinking", "context_management", "output_config", "metadata", "service_tier", "top_k"} {
		if _, present := chat[banned]; present {
			t.Fatalf("field %q forwarded to upstream", banned)
		}
	}
}

// ---------------------------------------------------------------------------
// Response translation: OpenAI → Anthropic
// ---------------------------------------------------------------------------

func TestOpenAIToAnthropicNonStream(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-1",
		"model": "upstream-test",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "Hello!",
				"reasoning_content": "let me think",
				"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "run", "arguments": "{\"cmd\":\"ls\"}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "prompt_tokens_details": {"cached_tokens": 3}}
	}`)
	out, err := openAIToAnthropic(body, "nv/test")
	if err != nil {
		t.Fatal(err)
	}
	var msg anthropicMessageOut
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.ID != "msg_chatcmpl-1" || msg.Role != "assistant" || msg.Model != "nv/test" {
		t.Fatalf("envelope wrong: %+v", msg)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("blocks=%d want 3 (thinking, text, tool_use): %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != "thinking" || msg.Content[0].Thinking != "let me think" {
		t.Fatalf("thinking block wrong: %+v", msg.Content[0])
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Text != "Hello!" {
		t.Fatalf("text block wrong: %+v", msg.Content[1])
	}
	if msg.Content[2].Type != "tool_use" || msg.Content[2].ID != "call_1" || msg.Content[2].Name != "run" {
		t.Fatalf("tool_use block wrong: %+v", msg.Content[2])
	}
	var input map[string]any
	if err := json.Unmarshal(msg.Content[2].Input, &input); err != nil || input["cmd"] != "ls" {
		t.Fatalf("tool input wrong: %s", msg.Content[2].Input)
	}
	if msg.StopReason == nil || *msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason wrong: %v", msg.StopReason)
	}
	if msg.Usage.InputTokens != 10 || msg.Usage.OutputTokens != 5 || msg.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("usage wrong: %+v", msg.Usage)
	}
}

func TestAnthropicStopReasons(t *testing.T) {
	for _, tc := range []struct {
		finish, want string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"function_call", "tool_use"},
		{"content_filter", "refusal"},
		{"", "end_turn"},
	} {
		t.Run(tc.finish, func(t *testing.T) {
			var finish *string
			if tc.finish != "" {
				finish = &tc.finish
			}
			got, _ := anthropicStop(finish, "m")
			if *got != tc.want {
				t.Fatalf("stop_reason=%s want %s", *got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// SSE translation
// ---------------------------------------------------------------------------

func collectAnthropicEvents(t *testing.T, src string) []string {
	t.Helper()
	s := &Server{recent: NewRecentLog(10), reasonCache: newReasoningCache(10)}
	_ = s
	rec := httptest.NewRecorder()
	err := streamCopyAnthropic(rec, strings.NewReader(src), "nv/test", nil, nil)
	if err != nil {
		t.Fatalf("stream copy: %v", err)
	}
	return strings.Split(strings.TrimSpace(rec.Body.String()), "\n\n")
}

func TestAnthropicStreamTextAndFinish(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\" there\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	events := collectAnthropicEvents(t, src)
	var kinds []string
	for _, ev := range events {
		for _, line := range strings.Split(ev, "\n") {
			if strings.HasPrefix(line, "event: ") {
				kinds = append(kinds, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	// message_start, content_block_start, 2x content_block_delta,
	// content_block_stop, message_delta, message_stop
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds=%v want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event[%d]=%s want %s (all: %v)", i, kinds[i], want[i], kinds)
		}
	}
	// stop_reason + usage in message_delta
	var lastDelta string
	for _, ev := range events {
		if strings.Contains(ev, "message_delta") {
			for _, line := range strings.Split(ev, "\n") {
				if strings.HasPrefix(line, "data: ") {
					lastDelta = strings.TrimPrefix(line, "data: ")
				}
			}
		}
	}
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			InputTokens  uint64 `json:"input_tokens"`
			OutputTokens uint64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(lastDelta), &md); err != nil {
		t.Fatal(err)
	}
	if md.Delta.StopReason != "end_turn" {
		t.Fatalf("stop_reason=%s want end_turn", md.Delta.StopReason)
	}
	if md.Usage.InputTokens != 3 || md.Usage.OutputTokens != 2 {
		t.Fatalf("usage wrong: %+v", md.Usage)
	}
}

func TestAnthropicStreamToolCallFragments(t *testing.T) {
	// Tool arguments split across multiple deltas — the canonical OpenAI
	// streaming shape for function calls.
	src := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"run\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := collectAnthropicEvents(t, src)
	var startPayload, deltaJoined string
	stopReason := ""
	for _, ev := range events {
		if strings.Contains(ev, "content_block_start") {
			for _, line := range strings.Split(ev, "\n") {
				if strings.HasPrefix(line, "data: ") {
					startPayload = strings.TrimPrefix(line, "data: ")
				}
			}
		}
		if strings.Contains(ev, "input_json_delta") {
			for _, line := range strings.Split(ev, "\n") {
				if strings.HasPrefix(line, "data: ") {
					var d struct {
						Delta struct {
							PartialJSON string `json:"partial_json"`
						} `json:"delta"`
					}
					_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d)
					deltaJoined += d.Delta.PartialJSON
				}
			}
		}
		if strings.Contains(ev, "message_delta") {
			for _, line := range strings.Split(ev, "\n") {
				if strings.HasPrefix(line, "data: ") {
					var d struct {
						Delta struct {
							StopReason string `json:"stop_reason"`
						} `json:"delta"`
					}
					_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d)
					stopReason = d.Delta.StopReason
				}
			}
		}
	}
	var start struct {
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(startPayload), &start); err != nil {
		t.Fatal(err)
	}
	if start.ContentBlock.Type != "tool_use" || start.ContentBlock.ID != "call_1" || start.ContentBlock.Name != "run" {
		t.Fatalf("tool block start wrong: %s", startPayload)
	}
	if deltaJoined != `{"cmd":"ls"}` {
		t.Fatalf("partial_json joined wrong: %q", deltaJoined)
	}
	if stopReason != "tool_use" {
		t.Fatalf("stop_reason=%s want tool_use", stopReason)
	}
}

func TestAnthropicStreamThinkingBlocks(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"pondering\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	events := collectAnthropicEvents(t, src)
	joined := strings.Join(events, "\n--\n")
	if !strings.Contains(joined, "thinking_delta") || !strings.Contains(joined, "text_delta") {
		t.Fatalf("missing thinking or text deltas: %s", joined)
	}
	// thinking block must close before the text block opens
	thinkStop := strings.Index(joined, `"thinking"`)
	textStart := strings.Index(joined, `"text_delta"`)
	blockStopIdx := strings.Index(joined[thinkStop:], "content_block_stop")
	if blockStopIdx == -1 || thinkStop+blockStopIdx > textStart {
		t.Fatal("thinking block not closed before text block started")
	}
}

func TestAnthropicStreamTruncatedIsError(t *testing.T) {
	src := "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
	rec := httptest.NewRecorder()
	err := streamCopyAnthropic(rec, strings.NewReader(src), "nv/test", nil, nil)
	if err == nil {
		t.Fatal("truncated stream must return an error")
	}
}

// ---------------------------------------------------------------------------
// Auth: x-api-key
// ---------------------------------------------------------------------------

func TestClientCredentialXApiKey(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("x-api-key", "vlk_0123456789012345678901234567890123456789012")
	key, ok := clientCredential(r)
	if !ok || key != "vlk_0123456789012345678901234567890123456789012" {
		t.Fatalf("x-api-key not accepted: %q %v", key, ok)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r2.Header.Set("Authorization", "Bearer vlk_0123456789012345678901234567890123456789012")
	r2.Header.Set("x-api-key", "vlk_0123456789012345678901234567890123456789012")
	if _, ok := clientCredential(r2); !ok {
		t.Fatal("matching dual headers must be accepted")
	}
	r3 := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r3.Header.Set("Authorization", "Bearer vlk_0123456789012345678901234567890123456789012")
	r3.Header.Set("x-api-key", "vlk_9999999999999999999999999999999999999999999")
	if _, ok := clientCredential(r3); ok {
		t.Fatal("conflicting dual headers must be rejected")
	}
}

// ---------------------------------------------------------------------------
// End-to-end through the gateway
// ---------------------------------------------------------------------------

func anthropicGatewayTest(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	cfg := runtimeConfig(upstream.URL)
	cfg.ModelMap["nv/test"] = "primary:upstream-test"
	return runtimeTestServer(t, cfg)
}

func anthropicCall(s *Server, body string, streaming bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "vlk_0123456789012345678901234567890123456789012")
	s.handleAnthropicMessages(w, req)
	return w
}

func TestAnthropicEndToEndNonStream(t *testing.T) {
	var gotPath, gotModel string
	s := anthropicGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		io.WriteString(w, `{"id":"c1","model":"upstream-test","choices":[{"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`)
	})
	w := anthropicCall(s, `{"model":"nv/test","max_tokens":50,"messages":[{"role":"user","content":"hello"}]}`, false)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("upstream path=%s want /chat/completions", gotPath)
	}
	if gotModel != "upstream-test" {
		t.Fatalf("upstream model=%s want upstream-test (remap missing)", gotModel)
	}
	var msg anthropicMessageOut
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response not Messages-shaped: %v — %s", err, w.Body.String())
	}
	if msg.Role != "assistant" || len(msg.Content) != 1 || msg.Content[0].Text != "Hi" {
		t.Fatalf("message wrong: %+v", msg)
	}
	if msg.StopReason == nil || *msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason wrong: %v", msg.StopReason)
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.TokIn != 4 || rec.TokOut != 2 || rec.Model != "nv/test" {
		t.Fatalf("record wrong: %+v", rec)
	}
}

func TestAnthropicEndToEndStream(t *testing.T) {
	s := anthropicGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	w := anthropicCall(s, `{"model":"nv/test","max_tokens":50,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, true)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, marker := range []string{"message_start", "content_block_start", "text_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("stream missing %q: %s", marker, body)
		}
	}
	if strings.Contains(body, "chat.completion") {
		t.Fatalf("OpenAI chunk leaked to the Anthropic client: %s", body)
	}
	rec := runtimeRecord(t, s)
	if !rec.OK || rec.TokIn != 4 || rec.TokOut != 2 {
		t.Fatalf("record wrong: %+v", rec)
	}
}

func TestAnthropicEndToEndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		wantErrType string
	}{
		{"unknown model", `{"model":"nope","max_tokens":10,"messages":[{"role":"user","content":"x"}]}`, 404, "not_found_error"},
		{"missing max_tokens", `{"model":"nv/test","messages":[{"role":"user","content":"x"}]}`, 400, "invalid_request_error"},
		{"invalid body", `not json`, 400, "invalid_request_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := anthropicGatewayTest(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("upstream must not be reached")
			})
			w := anthropicCall(s, tc.body, false)
			if w.Code != tc.status {
				t.Fatalf("status=%d want %d: %s", w.Code, tc.status, w.Body.String())
			}
			var e struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Type != "error" || e.Error.Type != tc.wantErrType {
				t.Fatalf("error shape wrong: %s", w.Body.String())
			}
		})
	}
}

func TestAnthropicCountTokensStub(t *testing.T) {
	out, err := anthropicCountTokens([]byte(`{"model":"m","messages":[{"role":"user","content":"hello world"}],"max_tokens":10}`))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.InputTokens <= 0 {
		t.Fatalf("count_tokens output wrong: %s", out)
	}
}
