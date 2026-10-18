package server

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

func remapBody(raw []byte, upstreamModel string, isMistral bool, backendName string) []byte {
	if len(raw) == 0 {
		return raw
	}
	// Fast path for the overwhelming majority of traffic (every backend
	// except Mistral/freeinference): only the model field changes, so
	// decode the top level as raw messages and leave the messages tree —
	// usually 99% of the body — completely untouched. No number is ever
	// reformatted and no nested structure is rebuilt.
	if !isMistral && backendName != "freeinference" {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return raw
		}
		modelJSON, err := json.Marshal(upstreamModel)
		if err != nil {
			return raw
		}
		top["model"] = modelJSON
		out, err := json.Marshal(top)
		if err != nil {
			return raw
		}
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed map[string]any
	if err := dec.Decode(&parsed); err != nil {
		return raw
	}
	parsed["model"] = upstreamModel
	// Remap "developer" role to "system" for backends that reject it.
	// Mistral and freeinference both 400 on role: "developer".
	if isMistral || backendName == "freeinference" {
		if msgs, ok := parsed["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					if role, _ := mm["role"].(string); role == "developer" {
						mm["role"] = "system"
					}
				}
			}
		}
	}
	if isMistral {
		// --- Message rebuild: role + content translation ---
		// Mistral rejects several request shapes that modern clients send:
		//   - role: "developer" (Mistral only accepts system/user/assistant/tool)
		//   - content: [{type:"text",...}] arrays (Mistral expects strings for
		//     text-only messages)
		//   - Anthropic-style tool calls embedded in content arrays:
		//       assistant content: [{type:"tool_use", id, name, input}]
		//       user content:      [{type:"tool_result", tool_use_id, content}]
		//     Mistral (OpenAI-compat) expects:
		//       assistant: {tool_calls: [{id, type:"function", function:{name, arguments}}]}
		//       tool:      {role:"tool", tool_call_id, content}
		//
		// We rebuild the messages array in a single pass, translating each
		// message as needed. Non-translatable content (images, unknown
		// types) is left intact.
		if msgs, ok := parsed["messages"].([]any); ok {
			rebuilt := make([]any, 0, len(msgs))
			for _, m := range msgs {
				mm, ok := m.(map[string]any)
				if !ok {
					rebuilt = append(rebuilt, m)
					continue
				}
				role, _ := mm["role"].(string)
				parts, ok := mm["content"].([]any)
				if !ok {
					rebuilt = append(rebuilt, mm)
					continue
				}
				hasToolUse := false
				hasToolResult := false
				allText := true
				for _, p := range parts {
					pm, ok := p.(map[string]any)
					if !ok {
						allText = false
						continue
					}
					switch t, _ := pm["type"].(string); t {
					case "", "text":
					case "tool_use":
						hasToolUse = true
						allText = false
					case "tool_result":
						hasToolResult = true
						allText = false
					default:
						allText = false
					}
				}
				switch {
				case allText:
					var sb strings.Builder
					for _, p := range parts {
						if pm, ok := p.(map[string]any); ok {
							if txt, ok := pm["text"].(string); ok {
								sb.WriteString(txt)
							}
						}
					}
					mm["content"] = sb.String()
					rebuilt = append(rebuilt, mm)
				case hasToolUse && role == "assistant":
					var textParts []string
					var toolCalls []any
					for _, p := range parts {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						t, _ := pm["type"].(string)
						switch t {
						case "", "text":
							if txt, ok := pm["text"].(string); ok {
								textParts = append(textParts, txt)
							}
						case "tool_use":
							id, _ := pm["id"].(string)
							name, _ := pm["name"].(string)
							input := pm["input"]
							var argsBytes []byte
							if input != nil {
								argsBytes, _ = json.Marshal(input)
							} else {
								argsBytes = []byte("{}")
							}
							toolCalls = append(toolCalls, map[string]any{
								"id":   id,
								"type": "function",
								"function": map[string]any{
									"name":      name,
									"arguments": string(argsBytes),
								},
							})
						}
					}
					if len(textParts) > 0 {
						mm["content"] = strings.Join(textParts, "")
					} else {
						delete(mm, "content")
					}
					if len(toolCalls) > 0 {
						if existing, ok := mm["tool_calls"].([]any); ok {
							mm["tool_calls"] = append(existing, toolCalls...)
						} else {
							mm["tool_calls"] = toolCalls
						}
					}
					rebuilt = append(rebuilt, mm)
				case hasToolResult && role == "user":
					for _, p := range parts {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						if t, _ := pm["type"].(string); t != "tool_result" {
							continue
						}
						toolUseID, _ := pm["tool_use_id"].(string)
						content := ""
						switch c := pm["content"].(type) {
						case string:
							content = c
						case []any:
							var sb strings.Builder
							for _, cp := range c {
								if cpm, ok := cp.(map[string]any); ok {
									if txt, ok := cpm["text"].(string); ok {
										sb.WriteString(txt)
									}
								}
							}
							content = sb.String()
						}
						rebuilt = append(rebuilt, map[string]any{
							"role":         "tool",
							"tool_call_id": toolUseID,
							"content":      content,
						})
					}
					var textParts []string
					for _, p := range parts {
						pm, ok := p.(map[string]any)
						if !ok {
							continue
						}
						t, _ := pm["type"].(string)
						if t == "" || t == "text" {
							if txt, ok := pm["text"].(string); ok {
								textParts = append(textParts, txt)
							}
						}
					}
					if len(textParts) > 0 {
						rebuilt = append(rebuilt, map[string]any{
							"role":    "user",
							"content": strings.Join(textParts, ""),
						})
					}
				default:
					rebuilt = append(rebuilt, mm)
				}
			}
			parsed["messages"] = rebuilt
		}
		// --- Field cleanup ---
		delete(parsed, "reasoning_effort")
		delete(parsed, "thinking")
		if v, ok := parsed["max_completion_tokens"]; ok {
			if _, hasMax := parsed["max_tokens"]; !hasMax {
				parsed["max_tokens"] = v
			}
			delete(parsed, "max_completion_tokens")
		}
		delete(parsed, "store")
		if tools, ok := parsed["tools"].([]any); ok {
			for _, t := range tools {
				if mm, ok := t.(map[string]any); ok {
					if fn, ok := mm["function"].(map[string]any); ok {
						delete(fn, "strict")
					}
				}
			}
		}
		// --- Tool_call_id dedup (FIFO queue) ---
		// Some clients (notably opencode) emit every tool_call id as the
		// literal string "chatcmplt" instead of a unique id. Mistral
		// rejects this with "Duplicate tool call id in assistant message".
		//
		// Algorithm: single pass over messages. For each old id, track
		// callCount and resultCount. The first call keeps the original
		// id; each subsequent call gets a fresh unique id (pushed to a
		// FIFO queue). The first result keeps the original id (matching
		// the first call); each subsequent result dequeues the oldest
		// fresh id (matching the corresponding duplicate call in order).
		if msgs, ok := parsed["messages"].([]any); ok {
			counter := 0
			freshID := func() string {
				counter++
				return "verloc_tc_" + strconv.Itoa(counter)
			}
			type idState struct {
				callCount   int
				resultCount int
				queue       []string
			}
			states := map[string]*idState{}
			for _, m := range msgs {
				mm, ok := m.(map[string]any)
				if !ok {
					continue
				}
				if tcs, ok := mm["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						id, _ := tcm["id"].(string)
						if id == "" {
							continue
						}
						s := states[id]
						if s == nil {
							s = &idState{}
							states[id] = s
						}
						s.callCount++
						if s.callCount > 1 {
							fresh := freshID()
							s.queue = append(s.queue, fresh)
							tcm["id"] = fresh
						}
					}
				}
				if role, _ := mm["role"].(string); role == "tool" {
					if id, ok := mm["tool_call_id"].(string); ok && id != "" {
						s := states[id]
						if s == nil {
							continue
						}
						s.resultCount++
						if s.resultCount > 1 && len(s.queue) > 0 {
							fresh := s.queue[0]
							s.queue = s.queue[1:]
							mm["tool_call_id"] = fresh
						}
					}
				}
			}
		}
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}
	return out
}

// stripReasoningContent removes non-standard reasoning fields
// (reasoning_content / reasoning) from message objects. Some upstreams
// reject these and return 400 (e.g. "messages.N.assistant.reasoning_content:
// property ... is unsupported"). opencode stores reasoning in prior assistant
// turns, so the field can leak into the forwarded conversation history.
var reasoningMarkers = [][]byte{[]byte(`"reasoning_content"`), []byte(`"reasoning"`)}

func stripReasoningContent(raw []byte, strip bool) []byte {
	if !strip || len(raw) == 0 {
		return raw
	}
	// Guard: without either field name anywhere in the body there is
	// nothing to strip — skip the full decode/re-encode entirely.
	if !bytes.Contains(raw, reasoningMarkers[0]) && !bytes.Contains(raw, reasoningMarkers[1]) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed map[string]any
	if err := dec.Decode(&parsed); err != nil {
		return raw
	}
	msgs, ok := parsed["messages"].([]any)
	if !ok {
		return raw
	}
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"reasoning_content", "reasoning"} {
			if _, has := mm[field]; has {
				delete(mm, field)
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}
	return out
}

// capMaxTokens clamps Chat and Responses output limits
// to the backend's limit. Some upstreams enforce a lower output cap than the
// model's nominal max (e.g. Poolside caps at 32768). limit <= 0 is a no-op.
var maxTokenMarkers = [][]byte{[]byte(`"max_tokens"`), []byte(`"max_completion_tokens"`), []byte(`"max_output_tokens"`)}

func capMaxTokens(raw []byte, limit int) []byte {
	if limit <= 0 || len(raw) == 0 {
		return raw
	}
	// Guard: no output-limit field anywhere → nothing to clamp.
	found := false
	for _, m := range maxTokenMarkers {
		if bytes.Contains(raw, m) {
			found = true
			break
		}
	}
	if !found {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var parsed map[string]any
	if err := dec.Decode(&parsed); err != nil {
		return raw
	}
	changed := false
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		v, ok := parsed[field]
		if !ok {
			continue
		}
		var n float64
		switch x := v.(type) {
		case float64:
			n = x
		case json.Number:
			n, _ = x.Float64()
		default:
			continue
		}
		if n > float64(limit) {
			parsed[field] = limit
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}
	return out
}

// Attribution uses the connected peer, never spoofable forwarding headers.
func usageFromReader(r io.Reader) (in, out, cache uint64, ok bool) {
	var p struct {
		Usage    *usageJSON `json:"usage"`
		Response struct {
			Usage *usageJSON `json:"usage"`
		} `json:"response"`
	}
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return 0, 0, 0, false
	}
	u := p.Usage
	if u == nil {
		u = p.Response.Usage
	}
	return extractUsage(u)
}

type usageJSON = struct {
	Prompt        *uint64 `json:"prompt_tokens"`
	Completion    *uint64 `json:"completion_tokens"`
	Input         *uint64 `json:"input_tokens"`
	Output        *uint64 `json:"output_tokens"`
	PromptDetails struct {
		Cached uint64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputDetails struct {
		Cached uint64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func extractUsage(u *usageJSON) (in, out, cache uint64, ok bool) {
	if u == nil {
		return 0, 0, 0, false
	}
	if u.Prompt != nil && u.Completion != nil {
		return *u.Prompt, *u.Completion, u.PromptDetails.Cached, true
	}
	if u.Input != nil && u.Output != nil {
		return *u.Input, *u.Output, u.InputDetails.Cached, true
	}
	return 0, 0, 0, false
}

func usageOf(body []byte) (in, out, cache uint64, ok bool) {
	type usage struct {
		Prompt        *uint64 `json:"prompt_tokens"`
		Completion    *uint64 `json:"completion_tokens"`
		Input         *uint64 `json:"input_tokens"`
		Output        *uint64 `json:"output_tokens"`
		PromptDetails struct {
			Cached uint64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		InputDetails struct {
			Cached uint64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	}
	var p struct {
		Usage    *usage `json:"usage"`
		Response struct {
			Usage *usage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &p) != nil {
		return 0, 0, 0, false
	}
	u := p.Usage
	if u == nil {
		u = p.Response.Usage
	}
	if u == nil {
		return 0, 0, 0, false
	}
	if u.Prompt != nil && u.Completion != nil {
		return *u.Prompt, *u.Completion, u.PromptDetails.Cached, true
	}
	if u.Input != nil && u.Output != nil {
		return *u.Input, *u.Output, u.InputDetails.Cached, true
	}
	return 0, 0, 0, false
}

// A route remains replaceable until the first stream write commits headers.
