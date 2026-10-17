package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// adminMu serialises config mutations so two dashboard writers cannot interleave.
var adminMu sync.Mutex

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.security == nil {
		writeErr(w, http.StatusUnauthorized, "administrator session required")
		return false
	}
	return s.security.CheckAdmin(w, r)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": "invalid_request"},
	})
}

// applyLive validates and commits a complete management transaction before
// publishing its immutable runtime snapshot. No live change writes config.json.
func (s *Server) applyLive(newCfg *Config) error {
	newCfg.applyDefaults()
	if err := newCfg.Limits.Validate(); err != nil {
		return err
	}
	backends, err := newCfg.resolveBackends()
	if err != nil {
		return err
	}
	if s.control == nil {
		return fmt.Errorf("management database is unavailable")
	}
	old := s.runtime()
	for name, nb := range backends {
		op := old.cfg.Providers[name]
		np := newCfg.Providers[name]
		if ob, ok := old.backends[name]; ok && providerFingerprint(op) == providerFingerprint(np) {
			nb.pool = ob.pool
			nb.client = ob.client
		}
	}
	if err := s.control.SaveConfig(newCfg); err != nil {
		return fmt.Errorf("persist management state: %w", err)
	}
	// Past this point the change is COMMITTED. Every remaining step is
	// publication, and none of them returns an error — so a panic here would
	// leave control.db holding the new config while s.rt still serves the old
	// one, and the next restart would silently "jump" to the committed value
	// the UI was told had failed. Recover, log loudly, and continue to publish:
	// an inconsistent-but-published runtime is strictly better than a silent
	// divergence, and the failure is surfaced rather than swallowed.
	var publishErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("[admin] PANIC while publishing committed config (db and runtime may diverge until the next apply): %v", p)
				// The commit stands, but telling the client "applied" would
				// be false: the runtime may still serve the old config until
				// restart. Surface it so the operator reloads deliberately.
				publishErr = fmt.Errorf("config committed but publication failed (%v); restart verloc to apply it", p)
			}
		}()
		s.wirePoolHooks(backends)
		catalog.SyncFromConfig(newCfg)
		s.rt.Store(newRuntimeState(newCfg, backends))
		s.gatewayConfigChanged()
		if err := s.updateTelemetrySettings(newCfg); err != nil {
			log.Printf("[admin] telemetry settings not applied after commit: %v", err)
		}
	}()
	if publishErr != nil {
		return publishErr
	}
	for name, ob := range old.backends {
		if nb := backends[name]; nb == nil || nb.client != ob.client {
			ob.client.CloseIdleConnections()
		}
	}
	log.Printf("[admin] state applied: providers=%d models=%d", len(newCfg.Providers), len(newCfg.ModelMap))
	return nil
}

// ---- providers ----

type adminProviderView struct {
	Name                 string         `json:"name"`
	BaseURL              string         `json:"base_url"`
	APIFormat            string         `json:"api_format"`
	AuthHeader           string         `json:"auth_header"`
	AuthHeaderFormat     string         `json:"auth_header_format"`
	CooldownSeconds      int            `json:"cooldown_seconds"`
	RequestTimeout       int            `json:"request_timeout_seconds"`
	NoAuth               bool           `json:"no_auth"`
	SynthesizeCLIHeaders bool           `json:"synthesize_cli_headers"`
	StripReasoning       *bool          `json:"strip_reasoning_content"`
	MaxTokens            int            `json:"max_tokens"`
	Keys                 []adminKeyView `json:"keys"`
	Disabled             bool           `json:"disabled"`
	ModelCount           int            `json:"model_count"`
}

type adminKeyView struct {
	ID        string `json:"id"`
	Masked    string `json:"masked"`
	AccountID string `json:"account_id,omitempty"`
}

type providerUpsertReq struct {
	Name                 string `json:"name"`
	BaseURL              string `json:"base_url"`
	APIFormat            string `json:"api_format"`
	AuthHeader           string `json:"auth_header"`
	AuthHeaderFormat     string `json:"auth_header_format"`
	CooldownSeconds      int    `json:"cooldown_seconds"`
	RequestTimeout       int    `json:"request_timeout_seconds"`
	NoAuth               bool   `json:"no_auth"`
	SynthesizeCLIHeaders bool   `json:"synthesize_cli_headers"`
	StripReasoning       *bool  `json:"strip_reasoning_content"`
	MaxTokens            int    `json:"max_tokens"`
	// AllowPrivateUpstreams opts this provider into targeting a loopback /
	// link-local / private address (see the wizard probe guard).
	AllowPrivateUpstreams bool `json:"allow_private_upstreams"`
	Disabled              bool `json:"disabled"`
	// Keys replaces the provider key list when non-nil (including empty).
	Keys *[]KeyEntry `json:"keys"`
	// AppendKeys adds keys without clearing existing ones.
	AppendKeys []KeyEntry `json:"append_keys"`
}

func (s *Server) handleAdminProvidersList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rt := s.runtime()
	names := managementProviderNames(rt.cfg)
	out := make([]adminProviderView, 0, len(names))
	for _, name := range names {
		p := rt.cfg.Providers[name]
		if p == nil {
			continue
		}
		out = append(out, s.providerView(name, p, rt.cfg))
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (s *Server) providerView(name string, p *ProviderConfig, cfg *Config) adminProviderView {
	keys := make([]adminKeyView, len(p.Keys))
	for i, k := range p.Keys {
		keys[i] = adminKeyView{ID: k.ID, Masked: maskKey(k.Key), AccountID: k.AccountID}
	}
	nModels := 0
	prefix := name + ":"
	for _, v := range cfg.ModelMap {
		if strings.HasPrefix(v, prefix) || (!strings.Contains(v, ":") && cfg.DefaultProvider == name) {
			nModels++
		}
	}
	return adminProviderView{
		Name: name, BaseURL: p.BaseURL, APIFormat: p.APIFormat, AuthHeader: p.AuthHeader, AuthHeaderFormat: p.AuthHeaderFormat,
		CooldownSeconds: p.CooldownSeconds, RequestTimeout: p.RequestTimeout,
		NoAuth: p.NoAuth, SynthesizeCLIHeaders: p.SynthesizeCLIHeaders,
		StripReasoning: p.StripReasoningContent, MaxTokens: p.MaxTokens,
		Keys: keys, ModelCount: nModels, Disabled: p.Disabled,
	}
}

func (s *Server) handleAdminProviderPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req providerUpsertReq
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if req.Name == "" {
		req.Name = r.PathValue("name")
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	cfg, err := cloneConfig(s.runtime().cfg)
	if err == nil {
		err = applyProviderRequest(cfg, req)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": s.providerView(req.Name, cfg.Providers[req.Name], cfg)})
}

func (s *Server) handleAdminProviderDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}

	adminMu.Lock()
	defer adminMu.Unlock()

	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, ok := cfg.Providers[name]; !ok {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	for id, v := range cfg.ModelMap {
		target := managementTarget(cfg, v)
		if target.Backend == name {
			writeErr(w, http.StatusConflict, fmt.Sprintf("provider is used by model %q", id))
			return
		}
		for _, fallback := range cfg.ModelRoutes[id] {
			if fallback.Backend == name {
				writeErr(w, http.StatusConflict, fmt.Sprintf("provider is a fallback for model %q", id))
				return
			}
		}
	}
	delete(cfg.Providers, name)
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": name})
}

func (s *Server) handleAdminProviderKeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	credentialID := r.PathValue("id")
	if credentialID == "" {
		writeErr(w, http.StatusBadRequest, "credential id required")
		return
	}

	adminMu.Lock()
	defer adminMu.Unlock()

	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := cfg.Providers[name]
	if p == nil {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	idx := -1
	for i, credential := range p.Keys {
		if credential.ID == credentialID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "credential not found")
		return
	}
	p.Keys = append(p.Keys[:idx], p.Keys[idx+1:]...)
	if len(p.Keys) == 0 && !p.NoAuth && !p.Disabled {
		writeErr(w, http.StatusBadRequest, "cannot remove last key")
		return
	}
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "provider": s.providerView(name, s.runtime().cfg.Providers[name], s.runtime().cfg)})
}

// ---- models ----

type adminModelView struct {
	ID            string        `json:"id"`
	Backend       string        `json:"backend"`
	Upstream      string        `json:"upstream"`
	Name          string        `json:"name"`
	Description   string        `json:"description,omitempty"`
	OwnedBy       string        `json:"owned_by,omitempty"`
	ContextWindow int           `json:"context_window"`
	MaxTokens     int           `json:"max_tokens"`
	Tags          []string      `json:"tags,omitempty"`
	PricingInput  float64       `json:"pricing_input"`
	PricingOutput float64       `json:"pricing_output"`
	PricingCache  float64       `json:"pricing_cache"`
	PricingKnown  bool          `json:"pricing_known"`
	HasMeta       bool          `json:"has_meta"`
	Embedded      bool          `json:"embedded"`
	Disabled      bool          `json:"disabled"`
	Routes        []RouteTarget `json:"routes"`
}

type modelUpsertReq struct {
	ID            string        `json:"id"`
	Backend       string        `json:"backend"`
	Upstream      string        `json:"upstream"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	OwnedBy       string        `json:"owned_by"`
	ContextWindow int           `json:"context_window"`
	MaxTokens     int           `json:"max_tokens"`
	Tags          []string      `json:"tags"`
	PricingInput  *float64      `json:"pricing_input"`
	PricingOutput *float64      `json:"pricing_output"`
	PricingCache  *float64      `json:"pricing_cache"`
	Disabled      bool          `json:"disabled"`
	Routes        []RouteTarget `json:"routes"`
	Overwrite     bool          `json:"overwrite"`
}

func (s *Server) handleAdminModelsList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	rt := s.runtime()
	ids := make([]string, 0, len(rt.cfg.ModelMap))
	for id := range rt.cfg.ModelMap {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]adminModelView, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.modelView(id, rt.cfg))
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func (s *Server) modelView(id string, cfg *Config) adminModelView {
	mapVal := cfg.ModelMap[id]
	backend, upstream := "", mapVal
	if i := strings.Index(mapVal, ":"); i >= 0 {
		backend, upstream = mapVal[:i], mapVal[i+1:]
	} else {
		backend = cfg.DefaultProvider
	}
	v := adminModelView{ID: id, Backend: backend, Upstream: upstream, Disabled: cfg.DisabledModels[id], Routes: cfg.ModelRoutes[id]}
	if meta := cfg.ModelMeta[id]; meta != nil {
		v.HasMeta = true
		v.Name = meta.Name
		v.Description = meta.Description
		v.OwnedBy = meta.OwnedBy
		v.ContextWindow = meta.ContextWindow
		v.MaxTokens = meta.MaxTokens
		v.Tags = meta.Tags
		v.PricingInput = meta.PricingInput
		v.PricingOutput = meta.PricingOutput
		v.PricingCache = meta.PricingCache
		v.PricingKnown = meta.PricingKnown == nil || *meta.PricingKnown
		if meta.Upstream != "" {
			v.Upstream = meta.Upstream
		}
	}
	if m := catalog.Lookup(id); m != nil {
		v.Embedded = !v.HasMeta
		if v.Name == "" {
			v.Name = m.Name
		}
		if v.Description == "" {
			v.Description = m.Description
		}
		if v.OwnedBy == "" {
			v.OwnedBy = m.OwnedBy
		}
		if v.ContextWindow == 0 {
			v.ContextWindow = m.ContextWindow
		}
		if v.MaxTokens == 0 {
			v.MaxTokens = m.MaxTokens
		}
		if !v.HasMeta {
			v.PricingKnown = m.PricingKnown
			v.PricingInput = m.PricingPerMillion.Input
			v.PricingOutput = m.PricingPerMillion.Output
			v.PricingCache = m.PricingPerMillion.CacheRead
		}
		if len(v.Tags) == 0 {
			v.Tags = m.Tags
		}
	}
	if v.Name == "" {
		v.Name = id
	}
	return v
}

func (s *Server) handleAdminModelPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req modelUpsertReq
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	pathID := strings.TrimPrefix(r.PathValue("id"), "/")
	if req.ID == "" {
		req.ID = pathID
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot copy management state")
		return
	}
	editingSameRoute := pathID == req.ID && cfg.ModelMap[req.ID] == req.Backend+":"+req.Upstream
	if err := applyModelRequest(cfg, req, req.Overwrite || editingSameRoute); err != nil {
		writeMutationError(w, err)
		return
	}
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": s.modelView(req.ID, cfg)})
}

func (s *Server) handleAdminModelDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := strings.TrimPrefix(strings.TrimSpace(r.PathValue("id")), "/")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}

	adminMu.Lock()
	defer adminMu.Unlock()

	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, ok := cfg.ModelMap[id]; !ok {
		writeErr(w, http.StatusNotFound, "model not found")
		return
	}
	delete(cfg.ModelMap, id)
	delete(cfg.ModelMeta, id)
	delete(cfg.ModelRoutes, id)
	delete(cfg.DisabledModels, id)
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}
