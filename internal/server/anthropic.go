package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// anthropicVersion is echoed back on Messages responses; the header value
// clients send is accepted as-is (Claude Code sends 2023-06-01).
const anthropicVersion = "2023-06-01"

// ---------------------------------------------------------------------------
// Anthropic Messages request shape (inbound from the client)
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model         string          `json:"model"`
	Messages      []anthropicMsg  `json:"messages"`
	MaxTokens     uint64          `json:"max_tokens"`
	System        json.RawMessage `json:"system"` // string or [{type:"text",...}]
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	Tools         []anthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	// Fields Claude Code sends that OpenAI-compatible backends reject.
	// They are consumed here, never forwarded.
	Metadata          json.RawMessage `json:"metadata"`
	Thinking          json.RawMessage `json:"thinking"`
	ContextManagement json.RawMessage `json:"context_management"`
	OutputConfig      json.RawMessage `json:"output_config"`
	ServiceTier       string          `json:"service_tier"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or block array
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicContentBlock is one block of a content array.
type anthropicContentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	// image
	Source *anthropicImageSource `json:"source,omitempty"`
	// thinking (stripped on inbound)
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// ---------------------------------------------------------------------------
// Request translation: Anthropic → OpenAI chat completions
// ---------------------------------------------------------------------------

// anthropicToOpenAI converts a Messages request body. The output is a
// complete chat-completions body: model, messages (with system folded in),
// tools, sampling params. Fields the upstream cannot accept are dropped.
func anthropicToOpenAI(body []byte) (map[string]any, *anthropicRequest, error) {
	var req anthropicRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return nil, nil, errors.New("invalid request JSON")
	}
	if req.Model == "" {
		return nil, nil, errors.New("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, nil, errors.New("messages is required and must be non-empty")
	}
	if req.MaxTokens == 0 {
		return nil, nil, errors.New("max_tokens is required and must be greater than zero")
	}
	out := map[string]any{"model": req.Model, "max_tokens": req.MaxTokens}
	var messages []any
	if system, ok := anthropicSystem(req.System); ok && system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for i := range req.Messages {
		converted, err := anthropicMessageToOpenAI(req.Messages[i])
		if err != nil {
			return nil, nil, err
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	// top_k has no chat-completions equivalent; deliberately dropped.
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Name == "" {
				continue
			}
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": schema},
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
			if choice, err := anthropicToolChoice(req.ToolChoice); err == nil && choice != nil {
				out["tool_choice"] = choice
			}
		}
	}
	if req.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out, &req, nil
}

// anthropicSystem folds the string-or-blocks system field into one string.
// Block-level cache_control is consumed, not forwarded.
func anthropicSystem(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", false
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), true
}

// anthropicMessageToOpenAI converts one Anthropic message into zero or more
// OpenAI messages:
//   - user text/image blocks → one user message (string or part array)
//   - user tool_result blocks → one OpenAI tool message each, then one user
//     message for any trailing text
//   - assistant text → one assistant message
//   - assistant tool_use blocks → tool_calls on the assistant message
//
// Inbound thinking blocks (with signature) and cache_control markers are
// dropped: backends cannot verify them and reject unknown fields.
func anthropicMessageToOpenAI(m anthropicMsg) ([]any, error) {
	// Plain string content is the common fast path.
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if s == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": m.Role, "content": s}}, nil
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, errors.New("message content must be a string or an array of content blocks")
	}
	var out []any
	if m.Role == "assistant" {
		assistant := map[string]any{"role": "assistant"}
		var text strings.Builder
		var toolCalls []any
		for _, b := range blocks {
			switch b.Type {
			case "text":
				text.WriteString(b.Text)
			case "tool_use":
				args := []byte("{}")
				if len(b.Input) > 0 && string(b.Input) != "null" {
					// Compact the raw input: clients send pretty-printed
					// JSON whose whitespace round-trips into arguments.
					var buf bytes.Buffer
					if err := json.Compact(&buf, b.Input); err == nil {
						args = buf.Bytes()
					} else {
						args = b.Input
					}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   b.ID,
					"type": "function",
					"function": map[string]any{
						"name":      b.Name,
						"arguments": string(args),
					},
				})
			case "thinking":
				// Dropped: signatures are Anthropic-side and cannot be
				// replayed against a different upstream.
			}
		}
		if text.Len() > 0 {
			assistant["content"] = text.String()
		}
		if len(toolCalls) > 0 {
			assistant["tool_calls"] = toolCalls
		}
		if _, hasContent := assistant["content"]; !hasContent && len(toolCalls) == 0 {
			return nil, nil
		}
		out = append(out, assistant)
		return out, nil
	}
	// user / tool-result path
	var userText strings.Builder
	var userParts []any
	var toolResults []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			userText.WriteString(b.Text)
			userParts = append(userParts, map[string]any{"type": "text", "text": b.Text})
		case "image":
			if b.Source != nil && b.Source.Type == "base64" && b.Source.Data != "" {
				userParts = append(userParts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
					},
				})
			}
		case "tool_result":
			content := ""
			switch {
			case len(b.Content) == 0 || string(b.Content) == "null":
			default:
				var cs string
				if json.Unmarshal(b.Content, &cs) == nil {
					content = cs
				} else {
					var cb []anthropicContentBlock
					if json.Unmarshal(b.Content, &cb) == nil {
						var sb strings.Builder
						for _, part := range cb {
							if part.Type == "text" {
								sb.WriteString(part.Text)
							}
						}
						content = sb.String()
					}
				}
			}
			toolResults = append(toolResults, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      content,
			})
		}
	}
	// Tool results are emitted as separate tool messages BEFORE any trailing
	// user text, matching the OpenAI ordering constraint.
	out = append(out, toolResults...)
	if len(userParts) > 0 {
		// Text-only user content collapses to a plain string (the common
		// case); images or mixed blocks keep the parts array.
		onlyText := true
		for _, p := range userParts {
			if pm, ok := p.(map[string]any); ok {
				if t, _ := pm["type"].(string); t != "text" {
					onlyText = false
					break
				}
			}
		}
		if onlyText {
			out = append(out, map[string]any{"role": m.Role, "content": userText.String()})
		} else {
			out = append(out, map[string]any{"role": m.Role, "content": userParts})
		}
	}
	return out, nil
}

// anthropicToolChoice converts the Messages tool_choice field. The Anthropic
// object form {"type":"tool","name":X} maps to OpenAI's specific-function
// form; "auto" and "any" map to "auto" and "required".
func anthropicToolChoice(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return "auto", nil
		case "any", "required":
			return "required", nil
		default:
			return nil, nil
		}
	}
	var o struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	switch o.Type {
	case "auto":
		return "auto", nil
	case "any":
		return "required", nil
	case "tool":
		if o.Name == "" {
			return nil, errors.New("tool_choice tool requires a name")
		}
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": o.Name},
		}, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Response translation: OpenAI chat completion → Anthropic Messages
// ---------------------------------------------------------------------------

type anthropicUsage struct {
	InputTokens              uint64 `json:"input_tokens"`
	OutputTokens             uint64 `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
}

type anthropicBlockOut struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// thinking
	Thinking string `json:"thinking,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type anthropicMessageOut struct {
	ID           string              `json:"id"`
	Type         string              `json:"type"`
	Role         string              `json:"role"`
	Model        string              `json:"model"`
	Content      []anthropicBlockOut `json:"content"`
	StopReason   *string             `json:"stop_reason"`
	StopSequence *string             `json:"stop_sequence"`
	Usage        anthropicUsage      `json:"usage"`
}

// openAIToAnthropic converts a chat-completions response body into a
// Messages response. model is the agent-facing verloc model id.
func openAIToAnthropic(body []byte, model string) ([]byte, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Reason    string          `json:"reasoning_content"`
				Reason2   string          `json:"reasoning"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        uint64 `json:"prompt_tokens"`
			CompletionTokens    uint64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens uint64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, errors.New("invalid upstream response")
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("upstream response has no choices")
	}
	msg := resp.Choices[0].Message
	blocks := []anthropicBlockOut{}
	if msg.Reason != "" {
		blocks = append(blocks, anthropicBlockOut{Type: "thinking", Thinking: msg.Reason})
	} else if msg.Reason2 != "" {
		blocks = append(blocks, anthropicBlockOut{Type: "thinking", Thinking: msg.Reason2})
	}
	// content may be a string or a part array.
	var text strings.Builder
	var parts []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	}
	if len(msg.Content) > 0 && string(msg.Content) != "null" {
		var s string
		if json.Unmarshal(msg.Content, &s) == nil {
			text.WriteString(s)
		} else if json.Unmarshal(msg.Content, &parts) == nil {
			for _, p := range parts {
				if p.Text != "" {
					text.WriteString(p.Text)
				}
				if p.Refusal != "" {
					text.WriteString(p.Refusal)
				}
			}
		}
	}
	if text.Len() > 0 || len(msg.ToolCalls) == 0 {
		blocks = append(blocks, anthropicBlockOut{Type: "text", Text: text.String()})
	}
	for _, tc := range msg.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, anthropicBlockOut{Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	stop, stopSequence := anthropicStop(resp.Choices[0].FinishReason, resp.Model)
	out := anthropicMessageOut{
		ID:           "msg_" + resp.ID,
		Type:         "message",
		Role:         "assistant",
		Model:        model,
		Content:      blocks,
		StopReason:   stop,
		StopSequence: stopSequence,
	}
	if resp.Usage != nil {
		out.Usage = anthropicUsage{
			InputTokens:          resp.Usage.PromptTokens,
			OutputTokens:         resp.Usage.CompletionTokens,
			CacheReadInputTokens: int64(resp.Usage.PromptTokensDetails.CachedTokens),
		}
	}
	return json.Marshal(out)
}

// anthropicStop maps an OpenAI finish_reason to Anthropic stop_reason +
// stop_sequence. Anthropic clients only accept the documented values.
func anthropicStop(finish *string, upstreamModel string) (*string, *string) {
	none := "end_turn"
	if finish == nil {
		return &none, nil
	}
	switch *finish {
	case "stop":
		// If the model stopped because it emitted one of the request's
		// stop sequences, Anthropic reports stop_reason "stop_sequence"
		// plus the sequence itself. The upstream does not tell us which
		// sequence matched, so end_turn is the honest default.
		return &none, nil
	case "length":
		s := "max_tokens"
		return &s, nil
	case "tool_calls", "function_call":
		s := "tool_use"
		return &s, nil
	case "content_filter":
		s := "refusal"
		return &s, nil
	default:
		return &none, nil
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// anthropicError writes a Messages-shaped error body. Claude Code matches
// recovery behaviour on the error message wording, so gateway-originated
// messages use the exact Anthropic phrasing where one exists.
func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 500:
		return "api_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// anthropicErrorMessage maps gateway-internal messages onto the Anthropic
// error wording Claude Code recognizes for capability recovery.
func anthropicErrorMessage(status int, message string) string {
	switch message {
	case "unknown or disabled model":
		return "model: The requested model is not available. Use GET /v1/models to list enabled models."
	case "upstream retry budget exhausted":
		return "The upstream is temporarily overloaded. Please retry."
	}
	if status == 429 {
		return "The request exceeded a rate limit. Please retry after the indicated interval."
	}
	return message
}

// writeAnthropicError emits a Messages-shaped error response.
func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": anthropicErrorMessage(status, message),
		},
	})
}

// ---------------------------------------------------------------------------
// count_tokens
// ---------------------------------------------------------------------------

// anthropicCountTokens is the char/4 estimate stub. Claude Code uses it for
// context display only; a real tokenizer is not required.
func anthropicCountTokens(body []byte) ([]byte, error) {
	// Rough byte-to-token estimate over the serialized request: 4 bytes per
	// token is the classic heuristic and errs on the high side of chars.
	inputTokens := len(body) / 4
	return json.Marshal(map[string]any{"input_tokens": inputTokens})
}

// isAnthropicClient reports whether the request came from an Anthropic-
// protocol client, keyed on the anthropic-version header (which every
// Anthropic SDK and Claude Code sends; discovery requests do not).
func isAnthropicClient(r *http.Request) bool {
	return r.Header.Get("anthropic-version") != ""
}
