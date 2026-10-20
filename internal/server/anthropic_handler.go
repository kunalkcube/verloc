package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// handleAnthropicMessages implements POST /v1/messages: translate the
// Anthropic Messages request to a chat-completions body, forward through
// the standard gateway path (admission, key rotation, failover, budget),
// and translate the upstream response back to Messages shape.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/chat/completions", &anthropicContext{})
}

// handleAnthropicCountTokens implements POST /v1/messages/count_tokens as
// a char/4 estimate. Claude Code uses it for the context display only.
func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		writeAnthropicError(w, http.StatusServiceUnavailable, "gateway shutting down")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	out, err := anthropicCountTokens(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request JSON")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// anthropicContext carries Messages-protocol state through forward. A nil
// context means the OpenAI passthrough path; every hook checks for nil, so
// the OpenAI path is byte-identical to before.
type anthropicContext struct {
	// model is the agent-facing verloc model id for response translation.
	model string
	// chatBody is the translated chat-completions request body.
	chatBody []byte
	// metadata holds the parsed original request for admission accounting.
	metadata requestMetadata
}

// translateRequest converts the inbound Messages body once, before any
// upstream attempt. Returns an error suitable for an Anthropic-shaped 400.
func (a *anthropicContext) translateRequest(raw []byte) error {
	chat, parsed, err := anthropicToOpenAI(raw)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(chat)
	if err != nil {
		return err
	}
	a.chatBody = encoded
	a.model = parsed.Model
	a.metadata = requestMetadata{Model: parsed.Model, Stream: parsed.Stream, MaxTokens: parsed.MaxTokens}
	return nil
}

// body returns the forwarded request bytes (the translated chat body).
func (a *anthropicContext) body() []byte { return a.chatBody }

// errorWriter emits Messages-shaped errors, including the wording Claude
// Code matches for capability-rejection recovery.
func (a *anthropicContext) errorWriter(w http.ResponseWriter, status int, message string) {
	writeAnthropicError(w, status, message)
}

// streamCopier returns the SSE translator for streaming responses.
func (a *anthropicContext) streamCopier(w http.ResponseWriter, src io.Reader, onUsage func(in, out, cache uint64), onFirst func()) error {
	return streamCopyAnthropic(w, src, a.model, onUsage, onFirst)
}

// translateResponse converts a complete chat-completions body into a
// Messages response. Used by the non-streaming path.
func (a *anthropicContext) translateResponse(body []byte) []byte {
	out, err := openAIToAnthropic(body, a.model)
	if err != nil {
		return body
	}
	return out
}
