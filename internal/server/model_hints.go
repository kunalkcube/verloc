package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Catalog prices are normalized to USD per million tokens. Missing prices
// remain distinct from explicit zero (free); sources fill gaps per field.
const (
	defaultModelsDevURL   = "https://models.dev/api.json?type=all"
	defaultOpenRouterURL  = "https://openrouter.ai/api/v1/models"
	defaultVercelURL      = "https://ai-gateway.vercel.sh/v1"
	defaultLiteLLMURL     = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	vercelHintTTL         = time.Hour
	catalogHintTTL        = 24 * time.Hour
	hintFetchOverallLimit = 30 * time.Second
)

type modelHint struct {
	Name          string   `json:"name,omitempty"`
	Description   string   `json:"description,omitempty"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	PricingInput  float64  `json:"-"`
	PricingOutput float64  `json:"-"`
	PricingCache  float64  `json:"-"`
	PricingKnown  bool     `json:"pricing_known"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
	Source        string   `json:"source,omitempty"`
	inputKnown    bool
	outputKnown   bool
	cacheKnown    bool
}

// Optional JSON numbers preserve partial pricing and explicitly free prices.
func (h modelHint) MarshalJSON() ([]byte, error) {
	type fields modelHint
	v := struct {
		fields
		Input  *float64 `json:"pricing_input,omitempty"`
		Output *float64 `json:"pricing_output,omitempty"`
		Cache  *float64 `json:"pricing_cache,omitempty"`
	}{fields: fields(h)}
	if h.inputKnown {
		v.Input = &h.PricingInput
	}
	if h.outputKnown {
		v.Output = &h.PricingOutput
	}
	if h.cacheKnown {
		v.Cache = &h.PricingCache
	}
	return json.Marshal(v)
}

func (h *modelHint) UnmarshalJSON(body []byte) error {
	type fields modelHint
	var v struct {
		fields
		Input  *float64 `json:"pricing_input"`
		Output *float64 `json:"pricing_output"`
		Cache  *float64 `json:"pricing_cache"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	*h = modelHint(v.fields)
	h.inputKnown, h.outputKnown, h.cacheKnown = v.Input != nil, v.Output != nil, v.Cache != nil
	if v.Input != nil {
		h.PricingInput = *v.Input
	}
	if v.Output != nil {
		h.PricingOutput = *v.Output
	}
	if v.Cache != nil {
		h.PricingCache = *v.Cache
	}
	h.PricingKnown = h.inputKnown && h.outputKnown
	return nil
}

type catalogEntry struct {
	ID string
	modelHint
}

type catalogIndex struct {
	byID  map[string]*catalogEntry
	tails map[string][]*catalogEntry
}

func indexCatalog(list []catalogEntry) catalogIndex {
	idx := catalogIndex{byID: map[string]*catalogEntry{}, tails: map[string][]*catalogEntry{}}
	grouped := make(map[string][]*catalogEntry)
	for i := range list {
		e := &list[i]
		if id := normHintID(e.ID); id != "" {
			grouped[id] = append(grouped[id], e)
		}
	}
	for id, entries := range grouped {
		// models.dev lists the same model through multiple providers. Only
		// use fields those providers agree on, never map iteration order.
		e := catalogEntry{ID: id, modelHint: consensusHints(entries)}
		idx.byID[id] = &e
		tail := tailHintID(id)
		idx.tails[tail] = append(idx.tails[tail], &e)
	}
	return idx
}

// Missing values do not vote; disagreement makes a field unavailable.
// This includes price disagreements between models.dev resellers.
func agreedHintField[T comparable](entries []*catalogEntry, field func(*modelHint) (T, bool)) (T, bool) {
	var value, zero T
	known := false
	for i := range entries {
		v, ok := field(&entries[i].modelHint)
		if !ok {
			continue
		}
		if known && v != value {
			return zero, false
		}
		value, known = v, true
	}
	return value, known
}

func consensusHints(entries []*catalogEntry) modelHint {
	if len(entries) == 1 {
		return entries[0].modelHint
	}
	h := modelHint{Source: entries[0].Source}
	h.Name, _ = agreedHintField(entries, func(e *modelHint) (string, bool) { return e.Name, e.Name != "" })
	h.Description, _ = agreedHintField(entries, func(e *modelHint) (string, bool) { return e.Description, e.Description != "" })
	h.OwnedBy, _ = agreedHintField(entries, func(e *modelHint) (string, bool) { return e.OwnedBy, e.OwnedBy != "" })
	h.ContextWindow, _ = agreedHintField(entries, func(e *modelHint) (int, bool) { return e.ContextWindow, e.ContextWindow > 0 })
	h.MaxTokens, _ = agreedHintField(entries, func(e *modelHint) (int, bool) { return e.MaxTokens, e.MaxTokens > 0 })
	h.PricingInput, h.inputKnown = agreedHintField(entries, func(e *modelHint) (float64, bool) { return e.PricingInput, e.inputKnown })
	h.PricingOutput, h.outputKnown = agreedHintField(entries, func(e *modelHint) (float64, bool) { return e.PricingOutput, e.outputKnown })
	h.PricingCache, h.cacheKnown = agreedHintField(entries, func(e *modelHint) (float64, bool) { return e.PricingCache, e.cacheKnown })
	for _, e := range entries {
		if len(e.Tags) == 0 {
			continue
		}
		if len(h.Tags) > 0 && !slices.Equal(h.Tags, e.Tags) {
			h.Tags = nil
			break
		}
		h.Tags = e.Tags
	}
	h.PricingKnown = h.inputKnown && h.outputKnown
	return h
}

func (c catalogIndex) lookup(upstream string) *catalogEntry {
	id := normHintID(upstream)
	if e := c.byID[id]; e != nil {
		return e
	}
	// Qualified identities must never cross organizations. Bare names may
	// match a unique catalog identity, but ambiguous tails are not evidence.
	if strings.Contains(id, "/") {
		return nil
	}
	if matches := c.tails[id]; len(matches) == 1 {
		return matches[0]
	}
	return nil
}

func normHintID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

func tailHintID(id string) string {
	n := normHintID(id)
	if i := strings.LastIndex(n, "/"); i >= 0 {
		return n[i+1:]
	}
	return n
}

// Numeric strings and numbers occur interchangeably in public catalogs.
// Null, structured/tiered prices, negative sentinels and non-finite values
// are unavailable, not free.
func numberField(m map[string]json.RawMessage, key string) (float64, bool) {
	raw := m[key]
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return 0, false
		}
		var err error
		n, err = strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, false
		}
	}
	return n, n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0)
}

func strField(m map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(m[key], &s)
	return s
}

func objectField(m map[string]json.RawMessage, key string) map[string]json.RawMessage {
	var v map[string]json.RawMessage
	_ = json.Unmarshal(m[key], &v)
	return v
}

func priceField(m map[string]json.RawMessage, scale float64, keys ...string) (float64, bool) {
	for _, key := range keys {
		if n, ok := numberField(m, key); ok && !math.IsInf(n*scale, 0) {
			return n * scale, true
		}
	}
	return 0, false
}

func positiveLimit(m map[string]json.RawMessage, keys ...string) int {
	for _, key := range keys {
		if n, ok := numberField(m, key); ok && n > 0 && n < float64(int(^uint(0)>>1)) && math.Trunc(n) == n {
			return int(n)
		}
	}
	return 0
}

func hintTags(m map[string]json.RawMessage) []string {
	var tags, params []string
	_ = json.Unmarshal(m["tags"], &tags)
	_ = json.Unmarshal(m["supported_parameters"], &params)
	add := func(tag string) {
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	for _, p := range params {
		switch p {
		case "tools", "tool_choice":
			add("tool-use")
		case "reasoning", "include_reasoning":
			add("reasoning")
		case "response_format", "structured_outputs":
			add("structured-output")
		}
	}
	for _, field := range []struct{ key, tag string }{{"reasoning", "reasoning"}, {"tool_call", "tool-use"}, {"structured_output", "structured-output"}} {
		if string(m[field.key]) == "true" {
			add(field.tag)
		}
	}
	modalities := objectField(m, "modalities")
	if modalities == nil {
		arch := objectField(m, "architecture")
		modalities = map[string]json.RawMessage{"input": arch["input_modalities"], "output": arch["output_modalities"]}
	}
	var input, output []string
	_ = json.Unmarshal(modalities["input"], &input)
	_ = json.Unmarshal(modalities["output"], &output)
	for _, mode := range input {
		switch mode {
		case "image":
			add("vision")
		case "audio":
			add("audio-input")
		case "video":
			add("video-input")
		case "pdf":
			add("pdf-input")
		}
	}
	for _, mode := range output {
		switch mode {
		case "image":
			add("image-generation")
		case "audio":
			add("audio-output")
		case "video":
			add("video-generation")
		}
	}
	if kind := strField(m, "type"); kind != "" && kind != "language" {
		add(kind)
	}
	slices.Sort(tags)
	return slices.Compact(tags)
}

func decodeCatalogEntry(raw json.RawMessage, id, source string) (catalogEntry, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return catalogEntry{}, false
	}
	return decodeCatalogObject(m, id, source)
}

func decodeCatalogObject(m map[string]json.RawMessage, id, source string) (catalogEntry, bool) {
	if found := strField(m, "id"); found != "" {
		id = found
	}
	if id == "" {
		return catalogEntry{}, false
	}
	h := modelHint{Name: strField(m, "name"), Description: strField(m, "description"), OwnedBy: strField(m, "owned_by"), Tags: hintTags(m), Source: source}
	if h.OwnedBy == "" {
		h.OwnedBy = strField(m, "owner")
	}
	h.ContextWindow = positiveLimit(m, "context_window", "context_length")
	h.MaxTokens = positiveLimit(m, "max_output_tokens", "max_completion_tokens", "max_tokens")
	limit := objectField(m, "limit")
	if h.ContextWindow == 0 {
		h.ContextWindow = positiveLimit(limit, "context")
	}
	if h.MaxTokens == 0 {
		h.MaxTokens = positiveLimit(limit, "output")
	}
	tp := objectField(m, "top_provider")
	if h.ContextWindow == 0 {
		h.ContextWindow = positiveLimit(tp, "context_length")
	}
	if h.MaxTokens == 0 {
		h.MaxTokens = positiveLimit(tp, "max_completion_tokens")
	}
	prices := objectField(m, "pricing")
	h.PricingInput, h.inputKnown = priceField(prices, 1e6, "input", "prompt")
	h.PricingOutput, h.outputKnown = priceField(prices, 1e6, "output", "completion")
	h.PricingCache, h.cacheKnown = priceField(prices, 1e6, "input_cache_read", "cache_read")
	cost := objectField(m, "cost")
	if n, ok := priceField(cost, 1, "input"); ok {
		h.PricingInput, h.inputKnown = n, true
	}
	if n, ok := priceField(cost, 1, "output"); ok {
		h.PricingOutput, h.outputKnown = n, true
	}
	if n, ok := priceField(cost, 1, "cache_read"); ok {
		h.PricingCache, h.cacheKnown = n, true
	}
	if n, ok := priceField(m, 1, "pricing_input"); ok {
		h.PricingInput, h.inputKnown = n, true
	}
	if n, ok := priceField(m, 1, "pricing_output"); ok {
		h.PricingOutput, h.outputKnown = n, true
	}
	if n, ok := priceField(m, 1, "pricing_cache"); ok {
		h.PricingCache, h.cacheKnown = n, true
	}
	if string(m["pricing_known"]) == "false" {
		h.inputKnown, h.outputKnown, h.cacheKnown = false, false, false
	}
	h.PricingKnown = h.inputKnown && h.outputKnown
	return catalogEntry{ID: id, modelHint: h}, true
}

// hintForDiscoveredModel keeps rich native /models metadata. source labels
// provenance only; pricing/cost field shapes determine the units.
func hintForDiscoveredModel(raw json.RawMessage, source string) *modelHint {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return nil
	}
	e, ok := decodeCatalogObject(m, strField(m, "name"), source)
	if !ok {
		return nil
	}
	return mergeDiscoveredModelHint(&e.modelHint, nil)
}

func modelListRows(body []byte) ([]json.RawMessage, error) {
	var root map[string]json.RawMessage
	var rows []json.RawMessage
	if json.Unmarshal(body, &rows) != nil {
		if err := json.Unmarshal(body, &root); err != nil {
			return nil, err
		}
		if json.Unmarshal(root["data"], &rows) != nil {
			if err := json.Unmarshal(root["models"], &rows); err != nil {
				return nil, fmt.Errorf("catalog has no model list")
			}
		}
	}
	return rows, nil
}

func decodeModelRows(rows []json.RawMessage, source string) []catalogEntry {
	out := make([]catalogEntry, 0, len(rows))
	for _, row := range rows {
		if e, ok := decodeCatalogEntry(row, "", source); ok {
			out = append(out, e)
		}
	}
	return out
}

func decodeModelList(body []byte, source string) ([]catalogEntry, error) {
	rows, err := modelListRows(body)
	if err != nil {
		return nil, err
	}
	return decodeModelRows(rows, source), nil
}

func getJSON(ctx context.Context, url string, limit int64, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// A fresh client per fetch: close its idle connections on the way out
	// or every catalog refresh abandons sockets to the 90s IdleConnTimeout.
	client := newClient(int(timeout.Seconds()) + 5)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog status %d", resp.StatusCode)
	}
	// Read one byte past the limit so truncation is an error, not silent
	// partial data that decodes into a plausible-but-wrong catalog.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("catalog response exceeds %d bytes", limit)
	}
	return body, nil
}

// Catalog pagination stays on the original origin, including credentialed
// Vercel connections. Bounds cover the whole source, not each page.
func fetchModelPages(ctx context.Context, endpoint, source string, headers http.Header, timeout time.Duration) ([]catalogEntry, error) {
	origin, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := newClient(int(timeout.Seconds()) + 5)
	defer client.CloseIdleConnections()
	remaining := int64(8 << 20)
	seen := make(map[string]bool)
	var out []catalogEntry
	rowsSeen, totalCount := 0, 0
	for endpoint != "" {
		if seen[endpoint] {
			return nil, fmt.Errorf("%s catalog pagination cycle", source)
		}
		seen[endpoint] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		for name, values := range headers {
			req.Header[name] = values
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, remaining+1))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s catalog status %d", source, resp.StatusCode)
		}
		remaining -= int64(len(body))
		if remaining < 0 {
			return nil, fmt.Errorf("%s catalog exceeds size limit", source)
		}
		rows, err := modelListRows(body)
		if err != nil {
			return nil, err
		}
		rowsSeen += len(rows)
		out = append(out, decodeModelRows(rows, source)...)
		var page struct {
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
			TotalCount int `json:"total_count"`
		}
		// Bare lists have no envelope or pagination metadata.
		if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '{' {
			if err := json.Unmarshal(body, &page); err != nil {
				return nil, err
			}
		}
		totalCount = max(totalCount, page.TotalCount)
		if page.Links.Next == "" {
			if totalCount > rowsSeen {
				return nil, fmt.Errorf("%s catalog missing next page", source)
			}
			break
		}
		next, err := url.Parse(page.Links.Next)
		if err != nil {
			return nil, err
		}
		next = req.URL.ResolveReference(next)
		if !strings.EqualFold(next.Scheme, origin.Scheme) || !strings.EqualFold(next.Host, origin.Host) || next.User != nil {
			return nil, fmt.Errorf("%s catalog pagination changed origin", source)
		}
		endpoint = next.String()
	}
	return out, nil
}

// Vercel's catalog is public; configured credentials are optional.
func fetchVercelModels(ctx context.Context, baseURL, key, authHeader, authFormat string) ([]catalogEntry, error) {
	headers := make(http.Header)
	if authHeader == "" {
		authHeader = "Authorization"
	}
	if authFormat == "" {
		authFormat = "Bearer %s"
	}
	if key != "" {
		headers.Set(authHeader, fmt.Sprintf(authFormat, key))
	}
	return fetchModelPages(ctx, strings.TrimRight(baseURL, "/")+"/models", "vercel", headers, 15*time.Second)
}

func fetchOpenRouter(ctx context.Context, endpoint string) ([]catalogEntry, error) {
	return fetchModelPages(ctx, endpoint, "openrouter", nil, 20*time.Second)
}

// Accept api.json's provider map, catalog.json's providers/models wrapper,
// and models.json's canonical model map. Bad individual rows are isolated.
func decodeModelsDev(body []byte) ([]catalogEntry, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	var out []catalogEntry
	// The same model legitimately appears under several providers (and the
	// canonical map) with possibly disagreeing fields: those duplicates are
	// the INPUT to consensusHints, which drops fields the sources disagree
	// on. Deduping here would silently crown the first source instead.
	// Iteration is sorted only so decoding is deterministic.
	appendModels := func(models map[string]json.RawMessage) {
		ids := make([]string, 0, len(models))
		for id := range models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if e, ok := decodeCatalogEntry(models[id], id, "models.dev"); ok {
				out = append(out, e)
			}
		}
	}
	providers := root
	if p := objectField(root, "providers"); p != nil {
		providers = p
	}
	for id, raw := range providers {
		var provider map[string]json.RawMessage
		if json.Unmarshal(raw, &provider) != nil {
			continue
		}
		if models := objectField(provider, "models"); models != nil {
			appendModels(models)
		} else if provider["limit"] != nil || provider["cost"] != nil || provider["description"] != nil || provider["modalities"] != nil {
			if e, ok := decodeCatalogEntry(raw, id, "models.dev"); ok {
				out = append(out, e)
			}
		}
	}
	if canonical := objectField(root, "models"); canonical != nil {
		appendModels(canonical)
	}
	return out, nil
}

func fetchModelsDev(ctx context.Context, url string) ([]catalogEntry, error) {
	body, err := getJSON(ctx, url, 32<<20, 25*time.Second)
	if err != nil {
		return nil, err
	}
	return decodeModelsDev(body)
}

func fetchLiteLLM(ctx context.Context, url string) ([]catalogEntry, error) {
	body, err := getJSON(ctx, url, 16<<20, 25*time.Second)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make([]catalogEntry, 0, len(raw))
	for id, row := range raw {
		var m map[string]json.RawMessage
		if json.Unmarshal(row, &m) != nil || m == nil {
			continue
		}
		h := modelHint{Source: "litellm", ContextWindow: positiveLimit(m, "max_input_tokens"), MaxTokens: positiveLimit(m, "max_output_tokens", "max_tokens")}
		h.PricingInput, h.inputKnown = priceField(m, 1e6, "input_cost_per_token")
		h.PricingOutput, h.outputKnown = priceField(m, 1e6, "output_cost_per_token")
		h.PricingCache, h.cacheKnown = priceField(m, 1e6, "input_cost_per_token_cache_hit")
		h.PricingKnown = h.inputKnown && h.outputKnown
		out = append(out, catalogEntry{ID: id, modelHint: h})
	}
	return out, nil
}

func fillModelHint(h, e *modelHint) {
	if e == nil {
		return
	}
	if h.Source == "" {
		h.Source = e.Source
	}
	if h.Name == "" {
		h.Name = e.Name
	}
	if h.Description == "" {
		h.Description = e.Description
	}
	if h.OwnedBy == "" {
		h.OwnedBy = e.OwnedBy
	}
	if len(h.Tags) == 0 {
		h.Tags = e.Tags
	}
	if !h.inputKnown && e.inputKnown {
		if !h.outputKnown {
			h.Source = e.Source
		}
		h.PricingInput, h.inputKnown = e.PricingInput, true
	}
	if !h.outputKnown && e.outputKnown {
		if !h.inputKnown {
			h.Source = e.Source
		}
		h.PricingOutput, h.outputKnown = e.PricingOutput, true
	}
	if !h.cacheKnown && e.cacheKnown {
		h.PricingCache, h.cacheKnown = e.PricingCache, true
	}
	if h.ContextWindow == 0 {
		h.ContextWindow = e.ContextWindow
	}
	if h.MaxTokens == 0 {
		h.MaxTokens = e.MaxTokens
	}
	h.PricingKnown = h.inputKnown && h.outputKnown
}

func nonemptyHint(h *modelHint) *modelHint {
	if h.Name == "" && h.Description == "" && h.OwnedBy == "" && len(h.Tags) == 0 && !h.inputKnown && !h.outputKnown && !h.cacheKnown && h.ContextWindow == 0 && h.MaxTokens == 0 {
		return nil
	}
	return h
}

func mergeDiscoveredModelHint(native, catalog *modelHint) *modelHint {
	h := &modelHint{}
	fillModelHint(h, native)
	fillModelHint(h, catalog)
	return nonemptyHint(h)
}

func mergeHint(upstream string, idxs []catalogIndex) *modelHint {
	h := &modelHint{}
	if id := normHintID(upstream); !strings.Contains(id, "/") {
		identity := ""
		for _, idx := range idxs {
			for _, e := range idx.tails[id] {
				if strings.Contains(e.ID, "/") {
					if identity != "" && identity != e.ID {
						return nil
					}
					identity = e.ID
				}
			}
		}
	}
	for _, idx := range idxs {
		if e := idx.lookup(upstream); e != nil {
			fillModelHint(h, &e.modelHint)
		}
	}
	return nonemptyHint(h)
}

// ---- caching resolver ----

type vercelConn struct {
	baseURL    string
	key        string
	authHeader string
	authFormat string
}

type cachedIndex struct {
	at  time.Time
	idx catalogIndex
}

type hintResolver struct {
	mu            sync.Mutex
	modelsDevURL  string
	vercelURL     string
	openRouterURL string
	litellmURL    string

	vercel  cachedIndex
	or      cachedIndex
	md      cachedIndex
	litellm cachedIndex

	// fetching marks sources with a refresh already in flight, so N
	// concurrent callers cannot each launch their own multi-MB fetch.
	fetching map[string]bool
}

func newHintResolver() *hintResolver {
	return &hintResolver{
		modelsDevURL:  defaultModelsDevURL,
		vercelURL:     defaultVercelURL,
		openRouterURL: defaultOpenRouterURL,
		litellmURL:    defaultLiteLLMURL,
	}
}

func (h *hintResolver) vercelConnFrom(cfg *Config) *vercelConn {
	vc := &vercelConn{baseURL: h.vercelURL}
	if vc.baseURL == "" {
		vc.baseURL = defaultVercelURL
	}
	if cfg == nil {
		return vc
	}
	p := cfg.Providers["vercel"]
	if p == nil {
		return vc
	}
	if p.BaseURL != "" {
		vc.baseURL = p.BaseURL
	}
	if len(p.Keys) > 0 {
		vc.key = p.Keys[0].Key
	}
	vc.authHeader = p.AuthHeader
	if vc.authHeader == "" {
		vc.authHeader = cfg.AuthHeader
	}
	vc.authFormat = p.AuthHeaderFormat
	if vc.authFormat == "" {
		vc.authFormat = cfg.AuthHeaderFormat
	}
	return vc
}

func (h *hintResolver) fresh(c cachedIndex, ttl time.Duration) bool {
	return !c.at.IsZero() && time.Since(c.at) < ttl
}

// hintsFor returns per-upstream merged hints, refreshing stale catalogs
// concurrently. Any source failure only drops that source.
func (h *hintResolver) hintsFor(ctx context.Context, cfg *Config, upstreams []string) map[string]*modelHint {
	var wg sync.WaitGroup
	refresh := func(cur cachedIndex, ttl time.Duration, set func(cachedIndex), fetch func(context.Context) ([]catalogEntry, error), label string) {
		h.mu.Lock()
		stale := !h.fresh(cur, ttl)
		if h.fetching == nil {
			h.fetching = map[string]bool{}
		}
		if !stale || h.fetching[label] {
			h.mu.Unlock()
			return
		}
		h.fetching[label] = true
		h.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				h.mu.Lock()
				delete(h.fetching, label)
				h.mu.Unlock()
			}()
			// WithoutCancel: with cache-first serving the caller may
			// return (and cancel) while this refresh is still running.
			list, err := fetch(context.WithoutCancel(ctx))
			if err != nil {
				log.Printf("[admin] %s hint fetch failed: %v", label, err)
				return
			}
			// D9: an empty-but-successful fetch must not blank every hint
			// for the whole TTL — keep the previous index.
			if len(list) == 0 {
				log.Printf("[admin] %s hint fetch returned no entries; keeping previous hints", label)
				return
			}
			set(cachedIndex{at: time.Now(), idx: indexCatalog(list)})
		}()
	}
	h.mu.Lock()
	vercel, or, md, litellm := h.vercel, h.or, h.md, h.litellm
	mdURL, orURL, litellmURL := h.modelsDevURL, h.openRouterURL, h.litellmURL
	h.mu.Unlock()

	if vc := h.vercelConnFrom(cfg); vc != nil {
		refresh(vercel, vercelHintTTL, func(c cachedIndex) {
			h.mu.Lock()
			h.vercel = c
			h.mu.Unlock()
		}, func(ctx context.Context) ([]catalogEntry, error) {
			return fetchVercelModels(ctx, vc.baseURL, vc.key, vc.authHeader, vc.authFormat)
		}, "vercel")
	}
	refresh(or, catalogHintTTL, func(c cachedIndex) {
		h.mu.Lock()
		h.or = c
		h.mu.Unlock()
	}, func(ctx context.Context) ([]catalogEntry, error) { return fetchOpenRouter(ctx, orURL) }, "openrouter")
	refresh(md, catalogHintTTL, func(c cachedIndex) {
		h.mu.Lock()
		h.md = c
		h.mu.Unlock()
	}, func(ctx context.Context) ([]catalogEntry, error) { return fetchModelsDev(ctx, mdURL) }, "models.dev")
	refresh(litellm, catalogHintTTL, func(c cachedIndex) {
		h.mu.Lock()
		h.litellm = c
		h.mu.Unlock()
	}, func(ctx context.Context) ([]catalogEntry, error) { return fetchLiteLLM(ctx, litellmURL) }, "litellm")

	// Cache-first: when any catalog is already cached (even stale), serve
	// it immediately and let the refresh finish in the background — the
	// caller used to block up to hintFetchOverallLimit on the slowest of
	// four network fetches. Only a cold resolver waits.
	h.mu.Lock()
	haveCache := h.vercel.idx.byID != nil || h.or.idx.byID != nil || h.md.idx.byID != nil || h.litellm.idx.byID != nil
	h.mu.Unlock()
	if !haveCache {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(hintFetchOverallLimit):
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	idxs := []catalogIndex{h.vercel.idx, h.or.idx, h.md.idx, h.litellm.idx}
	out := make(map[string]*modelHint, len(upstreams))
	for _, u := range upstreams {
		if hint := mergeHint(u, idxs); hint != nil {
			out[u] = hint
		}
	}
	return out
}
