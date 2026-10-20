package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// SSE translation: OpenAI chat.completion.chunk → Anthropic Messages events
// ---------------------------------------------------------------------------

// anthropicStreamState tracks where we are in the Anthropic event sequence:
// message_start once, then content_block_start/delta/stop around each block,
// and message_delta + message_stop at the end. Block indices are implicit in
// the emitted events' index fields.
type anthropicStreamState struct {
	model    string
	msgID    string
	started  bool
	roleSent bool
	// block bookkeeping
	blockIndex       int
	blockType        string // "text" | "thinking" | "tool_use"
	toolOpen         bool
	textOpen         bool
	thinkOpen        bool
	toolJSON         strings.Builder
	toolID           string
	toolName         string
	toolEmittedStart bool
	// completion bookkeeping
	stopReason *string
	inputTok   uint64
	outputTok  uint64
	cacheTok   uint64
	usageKnown bool
}

func newAnthropicStreamState(model string) *anthropicStreamState {
	return &anthropicStreamState{model: model, msgID: "msg_" + randomUUID()}
}

// writeAnthropicEvent marshals one SSE event and writes it to the writer.
func writeAnthropicEvent(w http.ResponseWriter, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("event: " + event + "\n")); err != nil {
		return err
	}
	_, err = w.Write([]byte("data: " + string(b) + "\n\n"))
	return err
}

// messageStart is emitted exactly once before the first content.
func (a *anthropicStreamState) messageStart() map[string]any {
	return map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            a.msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         a.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                a.inputTok,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	}
}

// beginBlock opens the next content block if the type changed, closing the
// previous one first. Consecutive same-type deltas reuse the open block.
func (a *anthropicStreamState) beginBlock(w http.ResponseWriter, kind string) error {
	if !a.started {
		if err := writeAnthropicEvent(w, "message_start", a.messageStart()); err != nil {
			return err
		}
		a.started = true
	}
	if a.blockType == kind && (a.textOpen || a.thinkOpen || a.toolOpen) {
		return nil
	}
	if err := a.closeBlock(w); err != nil {
		return err
	}
	switch kind {
	case "text":
		a.textOpen = true
	case "thinking":
		a.thinkOpen = true
	case "tool_use":
		a.toolOpen = true
	}
	a.blockType = kind
	block := map[string]any{"type": "content_block_start", "index": a.blockIndex, "content_block": map[string]any{"type": kind}}
	if kind == "tool_use" {
		block["content_block"] = map[string]any{"type": "tool_use", "id": a.toolID, "name": a.toolName, "input": map[string]any{}}
	}
	if kind == "text" {
		block["content_block"] = map[string]any{"type": "text", "text": ""}
	}
	if kind == "thinking" {
		block["content_block"] = map[string]any{"type": "thinking", "thinking": ""}
	}
	return writeAnthropicEvent(w, "content_block_start", block)
}

// closeBlock emits content_block_stop for the open block, if any.
func (a *anthropicStreamState) closeBlock(w http.ResponseWriter) error {
	if !a.textOpen && !a.thinkOpen && !a.toolOpen {
		return nil
	}
	if err := writeAnthropicEvent(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": a.blockIndex}); err != nil {
		return err
	}
	a.textOpen, a.thinkOpen, a.toolOpen = false, false, false
	a.blockIndex++
	a.blockType = ""
	return nil
}

// finish writes message_delta (stop reason + usage) and message_stop. It
// must be the last thing written on a successful stream.
func (a *anthropicStreamState) finish(w http.ResponseWriter) error {
	if !a.started {
		if err := writeAnthropicEvent(w, "message_start", a.messageStart()); err != nil {
			return err
		}
		a.started = true
	}
	if err := a.closeBlock(w); err != nil {
		return err
	}
	stop, _ := anthropicStop(a.stopReason, a.model)
	if err := writeAnthropicEvent(w, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   *stop,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":            a.inputTok,
			"output_tokens":           a.outputTok,
			"cache_read_input_tokens": a.cacheTok,
		},
	}); err != nil {
		return err
	}
	return writeAnthropicEvent(w, "message_stop", map[string]any{"type": "message_stop"})
}

// emitPing writes an SSE comment ping. Claude Code's idle watchdog measures
// inter-byte gaps, and OpenAI upstreams do not send pings of their own.
func emitPing(w http.ResponseWriter) error {
	_, err := w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	return err
}

// ingestChunk translates one OpenAI SSE data payload into Anthropic events.
// The payload may be a chat chunk or a Responses event; only chat chunks are
// translated, everything else is ignored.
func (a *anthropicStreamState) ingestChunk(w http.ResponseWriter, payload []byte, model string) error {
	var chunk struct {
		Type    string `json:"type"` // set on Responses events; absent on chat
		Choices []struct {
			Delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				Reason    string `json:"reasoning_content"`
				Reason2   string `json:"reasoning"`
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil // not JSON we understand; ignore (byte passthrough is already handled upstream)
	}
	if chunk.Type != "" {
		return nil // Responses API event, not a chat chunk
	}
	if len(chunk.Choices) == 0 {
		// Usage-only chunk (stream_options.include_usage sends an empty-
		// choices chunk with usage last).
		a.ingestUsage(payload)
		return nil
	}
	choice := chunk.Choices[0]
	if choice.FinishReason != nil {
		a.stopReason = choice.FinishReason
	}
	d := choice.Delta
	if d.Content != "" {
		if err := a.beginBlock(w, "text"); err != nil {
			return err
		}
		if err := writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": d.Content},
		}); err != nil {
			return err
		}
	}
	thinking := d.Reason
	if thinking == "" {
		thinking = d.Reason2
	}
	if thinking != "" {
		if err := a.beginBlock(w, "thinking"); err != nil {
			return err
		}
		if err := writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
		}); err != nil {
			return err
		}
	}
	for _, tc := range d.ToolCalls {
		// A tool call opens a fresh block (consecutive tool deltas for the
		// same call continue it). We detect a new call by id or name.
		if a.toolOpen && (tc.ID != "" && tc.ID != a.toolID || tc.Function.Name != "" && tc.Function.Name != a.toolName) {
			if err := a.closeBlock(w); err != nil {
				return err
			}
		}
		if !a.toolOpen {
			a.toolID = tc.ID
			a.toolName = tc.Function.Name
			a.toolJSON.Reset()
			if err := a.beginBlock(w, "tool_use"); err != nil {
				return err
			}
		}
		if tc.ID != "" {
			a.toolID = tc.ID
		}
		if tc.Function.Name != "" {
			a.toolName = tc.Function.Name
		}
		a.toolJSON.WriteString(tc.Function.Arguments)
		if err := writeAnthropicEvent(w, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": a.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
		}); err != nil {
			return err
		}
	}
	a.ingestUsage(payload)
	return nil
}

// ingestUsage extracts usage from a chunk payload if present.
func (a *anthropicStreamState) ingestUsage(payload []byte) {
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	if in, out, cache, known := usageOf(payload); known {
		a.inputTok, a.outputTok, a.cacheTok = in, out, cache
		a.usageKnown = true
	}
}

// streamCopyAnthropic reads an OpenAI SSE stream from src and writes the
// translated Anthropic event sequence to w. It mirrors streamCopyWithUsage:
// usage extraction, TTFT, terminal detection, and per-event flush behavior.
// Returns the same error contract as streamCopyWithUsage (nil on success,
// streamClientError on client write failure, io.ErrUnexpectedEOF on truncation).
func streamCopyAnthropic(w http.ResponseWriter, src io.Reader, model string, onUsage func(in, out, cache uint64), onFirst func()) error {
	state := newAnthropicStreamState(model)
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	terminal, finished := false, false
	flusher, _ := w.(http.Flusher)
	// lastPing bounds the gap between client-visible events so Claude
	// Code's streaming idle watchdog is not tripped by long thinking
	// pauses upstream. 15s is comfortably under the 60s default watchdog.
	lastPing := time.Now()
	maybePing := func() error {
		if time.Since(lastPing) < 15*time.Second {
			return nil
		}
		lastPing = time.Now()
		return emitPing(w)
	}
	flush := func() error {
		if f, ok := w.(interface{ FlushError() error }); ok {
			if err := f.FlushError(); err != nil {
				return &streamClientError{err}
			}
		} else if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				terminal = true
			} else if payload != "" {
				data := []byte(payload)
				var event struct {
					Type    string          `json:"type"`
					Error   json.RawMessage `json:"error"`
					Choices []struct {
						Finish *string `json:"finish_reason"`
					} `json:"choices"`
				}
				if json.Unmarshal(data, &event) == nil {
					if event.Type == "response.completed" {
						terminal = true
					}
					if len(event.Error) > 0 && string(event.Error) != "null" {
						terminal = true
					}
					for _, c := range event.Choices {
						if c.Finish != nil {
							finished = true
						}
					}
				}
				if onUsage != nil && bytes.Contains(data, usageMarker) {
					if in, out, cache, known := usageOf(data); known {
						onUsage(in, out, cache)
					}
				}
				if !terminal {
					if err := state.ingestChunk(w, data, model); err != nil {
						return err
					}
				}
			}
			if onFirst != nil {
				onFirst()
				onFirst = nil
			}
			if err := flush(); err != nil {
				return err
			}
			// Idle-gap pings: only meaningful once the message has started
			// (a ping before message_start is harmless but noisy).
			if state.started {
				if err := maybePing(); err != nil {
					return err
				}
			}
		}
		if terminal {
			// Drain complete: close out the message.
			if err := state.finish(w); err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			return nil
		}
	}
	if err := state.finish(w); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !terminal && !finished {
		return io.ErrUnexpectedEOF
	}
	return nil
}
