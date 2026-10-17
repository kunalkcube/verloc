package server

import (
	"encoding/json"
	"log"
	"strings"
	"sync"
)

const (
	maxReasoningEntries = 256
	maxReasoningBytes   = 512 * 1024
)

// reasoningCache remembers reasoning_content from a thinking-mode response,
// keyed by the tool_call ids in that turn, so a later request that dropped
// the field can still replay it. DeepSeek (and OpenCode Zen in front of it)
// 400s tool-call follow-ups that omit reasoning_content.
type reasoningCache struct {
	mu    sync.Mutex
	m     map[string]string
	order []string
	max   int
}

func newReasoningCache(max int) *reasoningCache {
	if max <= 0 {
		max = maxReasoningEntries
	}
	return &reasoningCache{m: make(map[string]string, max), max: max}
}

func (c *reasoningCache) put(id, text string) {
	if c == nil || id == "" || text == "" {
		return
	}
	if len(text) > maxReasoningBytes {
		text = text[:maxReasoningBytes]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; !ok {
		if len(c.order) >= c.max {
			old := c.order[0]
			c.order = c.order[1:]
			delete(c.m, old)
		}
		c.order = append(c.order, id)
	}
	c.m[id] = text
}

func (c *reasoningCache) get(id string) string {
	if c == nil || id == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[id]
}

func (c *reasoningCache) putMany(ids []string, text string) {
	for _, id := range ids {
		c.put(id, text)
	}
}

// ensureReasoningReplay makes thinking-mode tool-call history acceptable to
// DeepSeek-style upstreams:
//  1. copy `reasoning` → `reasoning_content` when the latter is missing
//  2. fill from the replay cache using tool_call ids
//  3. if the assistant message has tool_calls and still no reasoning, set ""
func ensureReasoningReplay(raw []byte, cache *reasoningCache) []byte {
	if len(raw) == 0 {
		return raw
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return raw
	}
	msgs, ok := parsed["messages"].([]any)
	if !ok {
		return raw
	}
	var nCopy, nCache, nEmpty int
	changed := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := mm["role"].(string)
		if role != "assistant" {
			continue
		}
		cur := stringField(mm, "reasoning_content")
		if cur == "" {
			if alt := stringField(mm, "reasoning"); alt != "" {
				mm["reasoning_content"] = alt
				cur = alt
				nCopy++
				changed = true
			}
		}
		if cur != "" || !hasToolCalls(mm) {
			continue
		}
		filled := ""
		for _, id := range toolCallIDs(mm) {
			if got := cache.get(id); got != "" {
				filled = got
				break
			}
		}
		if filled != "" {
			mm["reasoning_content"] = filled
			nCache++
		} else {
			mm["reasoning_content"] = ""
			nEmpty++
		}
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return raw
	}
	if nCopy+nCache+nEmpty > 0 {
		log.Printf("[reasoning] replay copy=%d cache=%d empty=%d", nCopy, nCache, nEmpty)
	}
	return out
}

func stringField(mm map[string]any, key string) string {
	s, _ := mm[key].(string)
	return s
}

func hasToolCalls(mm map[string]any) bool {
	tcs, ok := mm["tool_calls"].([]any)
	return ok && len(tcs) > 0
}

func toolCallIDs(mm map[string]any) []string {
	tcs, ok := mm["tool_calls"].([]any)
	if !ok {
		return nil
	}
	var ids []string
	for _, tc := range tcs {
		tcm, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		id, _ := tcm["id"].(string)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// sseReasoningAcc collects reasoning + tool_call ids from an upstream SSE
// (or JSON) completion so the next turn can replay them.
type sseReasoningAcc struct {
	buf  strings.Builder
	ids  []string
	seen map[string]bool
}

func (a *sseReasoningAcc) ingest(payload []byte) {
	if a == nil || len(payload) == 0 {
		return
	}
	var chunk map[string]any
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}
	choices, _ := chunk["choices"].([]any)
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		a.ingestObj(cm["delta"])
		a.ingestObj(cm["message"])
	}
}

func (a *sseReasoningAcc) ingestObj(v any) {
	mm, ok := v.(map[string]any)
	if !ok {
		return
	}
	if s := stringField(mm, "reasoning_content"); s != "" {
		a.buf.WriteString(s)
	} else if s := stringField(mm, "reasoning"); s != "" {
		a.buf.WriteString(s)
	}
	for _, id := range toolCallIDs(mm) {
		if a.seen == nil {
			a.seen = map[string]bool{}
		}
		if a.seen[id] {
			continue
		}
		a.seen[id] = true
		a.ids = append(a.ids, id)
	}
}

func (a *sseReasoningAcc) store(cache *reasoningCache) {
	if a == nil {
		return
	}
	text := a.buf.String()
	if text == "" || len(a.ids) == 0 {
		return
	}
	cache.putMany(a.ids, text)
}

// aliasReasoningInChunk copies delta/message.reasoning onto reasoning_content
// so clients that only look for the DeepSeek field still capture thinking.
func aliasReasoningInChunk(payload []byte) ([]byte, bool) {
	var chunk map[string]any
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return payload, false
	}
	choices, ok := chunk["choices"].([]any)
	if !ok {
		return payload, false
	}
	changed := false
	for _, c := range choices {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			mm, ok := cm[key].(map[string]any)
			if !ok {
				continue
			}
			if stringField(mm, "reasoning_content") == "" {
				if alt := stringField(mm, "reasoning"); alt != "" {
					mm["reasoning_content"] = alt
					changed = true
				}
			}
		}
	}
	if !changed {
		return payload, false
	}
	out, err := json.Marshal(chunk)
	if err != nil {
		return payload, false
	}
	return out, true
}

func rememberFromResponse(body []byte, cache *reasoningCache) []byte {
	acc := &sseReasoningAcc{}
	acc.ingest(body)
	acc.store(cache)
	if rewritten, ok := aliasReasoningInChunk(body); ok {
		return rewritten
	}
	return body
}
