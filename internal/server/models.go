package server

import (
	_ "embed"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"time"
)

//go:embed models.json
var modelsJSON []byte

// modelInfo is the per-model record embedded from models.json. Two pricing
// representations are stored:
//   - Pricing (OpenRouter-compatible): string values in $/token, with fields
//     prompt / completion / input_cache_read / image / request / web_search.
//     This is what /v1/models exposes so OpenAI/OpenRouter-style clients
//     (e.g. hermes) can read it without unit confusion.
//   - PricingPerMillion: numeric values in $/1M tokens (with currency/per
//     metadata + optional tiers). This is what /v1/pricing exposes; the
//     dashboard collector reads from it.
type modelInfo struct {
	ID                string         `json:"id"`
	Object            string         `json:"object"`
	Created           int64          `json:"created"`
	OwnedBy           string         `json:"owned_by"`
	Upstream          string         `json:"upstream"`
	ContextWindow     int            `json:"context_window"`
	MaxTokens         int            `json:"max_tokens"`
	Type              string         `json:"type"`
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	Tags              []string       `json:"tags,omitempty"`
	Pricing           pricingOR      `json:"pricing"`
	PricingPerMillion pricingPer1M   `json:"pricing_per_million"`
	PricingKnown      bool           `json:"pricing_known"`
	Released          int64          `json:"released,omitempty"`
	ReleaseDate       string         `json:"release_date,omitempty"`
	Attachment        *bool          `json:"attachment,omitempty"`
	Reasoning         *bool          `json:"reasoning,omitempty"`
	Temperature       *bool          `json:"temperature,omitempty"`
	Knowledge         string         `json:"knowledge,omitempty"`
	Status            string         `json:"status,omitempty"`
	Modalities        map[string]any `json:"modalities,omitempty"`
}

// pricingOR is the OpenRouter-compatible per-token pricing. Values are JSON
// strings (per OpenRouter's schema) to make the unit unambiguous.
type pricingOR struct {
	Prompt          string `json:"prompt"`
	Completion      string `json:"completion"`
	InputCacheRead  string `json:"input_cache_read,omitempty"`
	InputCacheWrite string `json:"input_cache_write,omitempty"`
	Request         string `json:"request,omitempty"`
	Image           string `json:"image,omitempty"`
	WebSearch       string `json:"web_search,omitempty"`
}

// pricingPer1M is the per-1M-token pricing used by /v1/pricing and the
// dashboard's cost calculator. Includes optional tier arrays for models
// whose rate changes after a token threshold.
type pricingPer1M struct {
	Input               float64 `json:"input"`
	Output              float64 `json:"output"`
	CacheRead           float64 `json:"cache_read"`
	Currency            string  `json:"currency"`
	Per                 string  `json:"per"`
	InputTiers          []any   `json:"input_tiers,omitempty"`
	OutputTiers         []any   `json:"output_tiers,omitempty"`
	InputCacheReadTiers []any   `json:"input_cache_read_tiers,omitempty"`
	WebSearch           float64 `json:"web_search,omitempty"`
}

type modelCatalog struct {
	mu       sync.RWMutex
	embedded []modelInfo           // immutable seed from models.json
	all      []modelInfo           // embedded + runtime overlays
	byID     map[string]*modelInfo // keyed by verloc model id
	pricing  map[string]pricingPer1M
	overlay  map[string]bool // ids that came from model_meta (removable)
	// pricingCopy is PricingMap's return value, rebuilt with the catalog.
	// The only caller marshals it read-only, so sharing it is safe and
	// removes a whole-table copy from every dashboard poll.
	pricingCopy map[string]pricingPer1M
}

var catalog = func() *modelCatalog {
	c := &modelCatalog{
		byID:    map[string]*modelInfo{},
		pricing: map[string]pricingPer1M{},
		overlay: map[string]bool{},
	}
	if err := json.Unmarshal(modelsJSON, &c.embedded); err != nil {
		log.Fatalf("models.json: %v", err)
	}
	c.rebuildLocked()
	log.Printf("loaded model catalog: %d models", len(c.all))
	return c
}()

func (c *modelCatalog) rebuildLocked() {
	c.all = make([]modelInfo, len(c.embedded))
	copy(c.all, c.embedded)
	c.byID = map[string]*modelInfo{}
	c.pricing = map[string]pricingPer1M{}
	for i := range c.all {
		m := &c.all[i]
		m.PricingKnown = true
		c.byID[m.ID] = m
		c.pricing[m.ID] = m.PricingPerMillion
	}
	c.refreshPricingCopyLocked()
}

// refreshPricingCopyLocked snapshots c.pricing for PricingMap callers.
// Caller holds c.mu (write).
func (c *modelCatalog) refreshPricingCopyLocked() {
	c.pricingCopy = make(map[string]pricingPer1M, len(c.pricing))
	for k, v := range c.pricing {
		c.pricingCopy[k] = v
	}
}

func (c *modelCatalog) Lookup(id string) *modelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byID[id]
}

func (c *modelCatalog) All() []modelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]modelInfo, len(c.all))
	copy(out, c.all)
	return out
}

// PricingMap returns the catalog's shared pricing table. Callers must not
// mutate it (the dashboard only marshals it); it is rebuilt atomically
// with the catalog on every SyncFromConfig.
func (c *modelCatalog) PricingMap() map[string]pricingPer1M {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pricingCopy
}

// SyncFromConfig rebuilds the catalog from the embedded seed plus model_meta.
func (c *modelCatalog) SyncFromConfig(cfg *Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rebuildLocked()
	c.overlay = map[string]bool{}
	if cfg == nil {
		return
	}
	for id, meta := range cfg.ModelMeta {
		if meta == nil {
			continue
		}
		m := metaToModelInfo(id, meta, cfg.ModelMap[id])
		if old, ok := c.byID[id]; ok {
			// Runtime pricing/limits must not erase immutable catalog capabilities.
			m.Attachment, m.Reasoning, m.Temperature = old.Attachment, old.Reasoning, old.Temperature
			m.Modalities, m.Knowledge, m.Status = old.Modalities, old.Knowledge, old.Status
			m.Released, m.ReleaseDate = old.Released, old.ReleaseDate
			*old = m
		} else {
			c.all = append(c.all, m)
		}
		c.overlay[id] = true
	}
	// Re-index after possible appends (append may reallocate the slice).
	c.byID = map[string]*modelInfo{}
	c.pricing = map[string]pricingPer1M{}
	for i := range c.all {
		c.byID[c.all[i].ID] = &c.all[i]
		if c.all[i].PricingKnown {
			c.pricing[c.all[i].ID] = c.all[i].PricingPerMillion
		}
	}
	c.refreshPricingCopyLocked()
}

func metaToModelInfo(id string, meta *ModelMeta, mapVal string) modelInfo {
	upstream := meta.Upstream
	if upstream == "" {
		if i := indexByte(mapVal, ':'); i >= 0 {
			upstream = mapVal[i+1:]
		} else {
			upstream = mapVal
		}
	}
	owned := meta.OwnedBy
	if owned == "" {
		owned = providerOf(mapVal)
		if owned == "unknown" {
			if i := indexByte(mapVal, ':'); i >= 0 {
				owned = mapVal[:i]
			}
		}
	}
	name := meta.Name
	if name == "" {
		name = id
	}
	per1M := pricingPer1M{
		Input:     meta.PricingInput,
		Output:    meta.PricingOutput,
		CacheRead: meta.PricingCache,
		Currency:  "USD",
		Per:       "1M tokens",
	}
	return modelInfo{
		ID:            id,
		Object:        "model",
		Created:       time.Now().Unix(),
		OwnedBy:       owned,
		Upstream:      upstream,
		ContextWindow: meta.ContextWindow,
		MaxTokens:     meta.MaxTokens,
		Type:          "language",
		Name:          name,
		Description:   meta.Description,
		Tags:          meta.Tags,
		PricingKnown:  meta.PricingKnown == nil || *meta.PricingKnown,
		Pricing: pricingOR{
			Prompt:         per1MString(meta.PricingInput),
			Completion:     per1MString(meta.PricingOutput),
			InputCacheRead: per1MString(meta.PricingCache),
		},
		PricingPerMillion: per1M,
	}
}

func per1MString(per1M float64) string {
	return strconv.FormatFloat(per1M/1e6, 'f', -1, 64)
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// costOf computes the USD cost of a request given per-1M-token pricing.
// Unknown pricing stays unknown to callers; zero here is only the numeric
// contribution to an explicitly labeled estimated-cost aggregate.
func costOf(modelID string, in, out, cache uint64) float64 {
	catalog.mu.RLock()
	p, ok := catalog.pricing[modelID]
	catalog.mu.RUnlock()
	if !ok {
		return 0
	}
	if cache > in {
		cache = in
	}
	return (float64(in-cache)*p.Input + float64(out)*p.Output + float64(cache)*p.CacheRead) / 1e6
}

// JSON field presence distinguishes an explicitly free model from missing
// pricing during legacy import and management-state cloning.
func (m *ModelMeta) UnmarshalJSON(data []byte) error {
	type metadata ModelMeta
	var decoded metadata
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if decoded.PricingKnown == nil {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		input, hasInput := fields["pricing_input"]
		output, hasOutput := fields["pricing_output"]
		known := hasInput && hasOutput && string(input) != "null" && string(output) != "null"
		decoded.PricingKnown = &known
	}
	*m = ModelMeta(decoded)
	return nil
}
