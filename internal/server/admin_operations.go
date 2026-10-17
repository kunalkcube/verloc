package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type managementConflict struct{ message string }

func (e *managementConflict) Error() string { return e.message }

func decodeAdminJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dest); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "management request exceeds 2 MiB")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON request")
		}
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "request must contain one JSON object")
		return false
	}
	return true
}

func writeMutationError(w http.ResponseWriter, err error) {
	var conflict *managementConflict
	if errors.As(err, &conflict) {
		writeErr(w, http.StatusConflict, conflict.Error())
		return
	}
	writeErr(w, http.StatusBadRequest, err.Error())
}

func managementTarget(cfg *Config, mapping string) RouteTarget {
	if backend, upstream, ok := strings.Cut(mapping, ":"); ok {
		return RouteTarget{Backend: backend, Upstream: upstream}
	}
	return RouteTarget{Backend: cfg.DefaultProvider, Upstream: mapping}
}

func applyProviderRequest(cfg *Config, req providerUpsertReq) error {
	if req.Name == "" || len(req.Name) > 100 || strings.TrimSpace(req.Name) != req.Name || strings.ContainsAny(req.Name, "/: \\?&#\t\r\n") {
		return fmt.Errorf("provider name must be 1–100 characters without spaces or URL separators")
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	parsed, err := url.Parse(strings.ReplaceAll(base, "%ACCOUNT%", "account"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("base_url must be an HTTP(S) endpoint without credentials, query, or fragment")
	}
	if req.APIFormat != "" && req.APIFormat != "openai" && req.APIFormat != "systemone" {
		return fmt.Errorf("api_format must be openai or systemone")
	}
	if strings.ContainsAny(req.AuthHeader+req.AuthHeaderFormat, "\r\n") || (req.AuthHeader != "" && !validAuthHeaderName(req.AuthHeader)) {
		return fmt.Errorf("invalid authentication header")
	}
	if req.AuthHeaderFormat != "" && (strings.Count(req.AuthHeaderFormat, "%s") != 1 || strings.Count(req.AuthHeaderFormat, "%") != 1) {
		return fmt.Errorf("auth_header_format must contain exactly one %%s")
	}
	if req.CooldownSeconds < 0 || req.CooldownSeconds > 3600 || req.RequestTimeout < 0 || req.RequestTimeout > 3600 || req.MaxTokens < 0 || req.MaxTokens > 16_777_216 {
		return fmt.Errorf("provider timeout, cooldown, or output limit is out of range")
	}
	p := &ProviderConfig{BaseURL: base, APIFormat: req.APIFormat, AuthHeader: req.AuthHeader, AuthHeaderFormat: req.AuthHeaderFormat, CooldownSeconds: req.CooldownSeconds, RequestTimeout: req.RequestTimeout, NoAuth: req.NoAuth, SynthesizeCLIHeaders: req.SynthesizeCLIHeaders, StripReasoningContent: req.StripReasoning, MaxTokens: req.MaxTokens, Disabled: req.Disabled}
	if req.Keys != nil {
		p.Keys = append([]KeyEntry(nil), (*req.Keys)...)
	} else if old := cfg.Providers[req.Name]; old != nil {
		p.Keys = append([]KeyEntry(nil), old.Keys...)
	}
	p.Keys = append(p.Keys, req.AppendKeys...)
	seen := make(map[string]bool, len(p.Keys))
	for _, key := range p.Keys {
		if strings.TrimSpace(key.Key) == "" || strings.ContainsAny(key.Key, "\r\n") {
			return fmt.Errorf("provider credentials cannot be empty or contain line breaks")
		}
		if seen[key.Key] {
			return fmt.Errorf("duplicate provider credential")
		}
		seen[key.Key] = true
		if strings.ContainsAny(key.AccountID, "/\\?#% \t\r\n") {
			return fmt.Errorf("invalid credential account identifier")
		}
		if strings.Contains(base, "%ACCOUNT%") && key.AccountID == "" {
			return fmt.Errorf("this provider requires an account_id for each credential")
		}
	}
	if len(p.Keys) == 0 && !p.NoAuth && !p.Disabled {
		return fmt.Errorf("provider needs a credential or keyless authentication")
	}
	cfg.Providers[req.Name] = p
	if len(cfg.Providers) == 1 {
		cfg.DefaultProvider = req.Name
	}
	return nil
}

func validAuthHeaderName(name string) bool {
	for _, ch := range name {
		if ch > 127 || !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", ch)) {
			return false
		}
	}
	return name != ""
}

func applyModelRequest(cfg *Config, req modelUpsertReq, overwrite bool) error {
	if req.ID == "" || len(req.ID) > 256 || strings.TrimSpace(req.ID) != req.ID || strings.HasPrefix(req.ID, "/") || strings.ContainsAny(req.ID, "?#\r\n\t ") || req.Backend == "" || strings.TrimSpace(req.Upstream) == "" || strings.ContainsAny(req.Upstream, "\r\n") {
		return fmt.Errorf("valid id, backend, and upstream are required")
	}
	if _, exists := cfg.ModelMap[req.ID]; exists && !overwrite {
		return &managementConflict{message: fmt.Sprintf("model %q already exists; explicitly confirm overwrite", req.ID)}
	}
	if cfg.Providers[req.Backend] == nil {
		return fmt.Errorf("unknown provider %q", req.Backend)
	}
	if req.ContextWindow < 0 || req.MaxTokens < 0 || req.ContextWindow > 16_777_216 || req.MaxTokens > 16_777_216 {
		return fmt.Errorf("model limits must be between 0 and 16777216 tokens")
	}
	for _, price := range []*float64{req.PricingInput, req.PricingOutput, req.PricingCache} {
		if price != nil && (math.IsNaN(*price) || math.IsInf(*price, 0) || *price < 0) {
			return fmt.Errorf("pricing must contain finite, non-negative USD per million token values")
		}
	}
	seen := map[string]bool{req.Backend + ":" + req.Upstream: true}
	for _, target := range req.Routes {
		if cfg.Providers[target.Backend] == nil || strings.TrimSpace(target.Upstream) == "" || strings.ContainsAny(target.Upstream, "\r\n") {
			return fmt.Errorf("each fallback must reference an existing provider and upstream model")
		}
		name := target.Backend + ":" + target.Upstream
		if seen[name] {
			return fmt.Errorf("duplicate routing target")
		}
		seen[name] = true
	}
	cfg.ModelMap[req.ID] = req.Backend + ":" + req.Upstream
	price := func(value *float64) float64 {
		if value == nil {
			return 0
		}
		return *value
	}
	pricingKnown := req.PricingInput != nil && req.PricingOutput != nil
	cachePrice := req.PricingCache
	if cachePrice == nil {
		// No published discount means cached input is billed at the input rate,
		// not free. An explicitly supplied zero remains a real free-cache rate.
		cachePrice = req.PricingInput
	}
	cfg.ModelMeta[req.ID] = &ModelMeta{Name: req.Name, Description: req.Description, OwnedBy: req.OwnedBy, Upstream: req.Upstream, ContextWindow: req.ContextWindow, MaxTokens: req.MaxTokens, Tags: req.Tags, PricingInput: price(req.PricingInput), PricingOutput: price(req.PricingOutput), PricingCache: price(cachePrice), PricingKnown: &pricingKnown}
	cfg.DisabledModels[req.ID] = req.Disabled
	cfg.ModelRoutes[req.ID] = append([]RouteTarget(nil), req.Routes...)
	return nil
}

func (s *Server) handleAdminOnboard(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Provider  *providerUpsertReq `json:"provider"`
		Models    []modelUpsertReq   `json:"models"`
		Overwrite bool               `json:"overwrite"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if len(req.Models) == 0 || len(req.Models) > 1000 {
		writeErr(w, http.StatusBadRequest, "select between 1 and 1000 models")
		return
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot copy management state")
		return
	}
	if req.Provider != nil {
		if err := applyProviderRequest(cfg, *req.Provider); err != nil {
			writeMutationError(w, err)
			return
		}
	}
	seen := make(map[string]bool, len(req.Models))
	for _, model := range req.Models {
		if seen[model.ID] {
			writeErr(w, http.StatusBadRequest, "duplicate model id in onboarding request")
			return
		}
		seen[model.ID] = true
		if err := applyModelRequest(cfg, model, req.Overwrite || model.Overwrite); err != nil {
			writeMutationError(w, err)
			return
		}
	}
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	providers := 0
	if req.Provider != nil {
		providers = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "providers": providers, "models": len(req.Models)})
}

func (s *Server) handleAdminModelBulk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 1000 || (req.Action != "enable" && req.Action != "disable" && req.Action != "delete") {
		writeErr(w, http.StatusBadRequest, "choose 1–1000 models and enable, disable, or delete")
		return
	}
	adminMu.Lock()
	defer adminMu.Unlock()
	cfg, err := cloneConfig(s.runtime().cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot copy management state")
		return
	}
	unique := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		if _, exists := cfg.ModelMap[id]; !exists {
			writeErr(w, http.StatusNotFound, fmt.Sprintf("model %q not found", id))
			return
		}
		unique[id] = true
	}
	for id := range unique {
		if req.Action == "delete" {
			delete(cfg.ModelMap, id)
			delete(cfg.ModelMeta, id)
			delete(cfg.ModelRoutes, id)
			delete(cfg.DisabledModels, id)
		} else {
			cfg.DisabledModels[id] = req.Action == "disable"
		}
	}
	if err := s.applyLive(cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(unique)})
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Limits *GatewayLimits `json:"limits"`
			Alerts *[]AlertRule   `json:"alerts"`
		}
		if !decodeAdminJSON(w, r, &req) {
			return
		}
		adminMu.Lock()
		defer adminMu.Unlock()
		cfg, err := cloneConfig(s.runtime().cfg)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "cannot copy management state")
			return
		}
		if req.Limits != nil {
			cfg.Limits = *req.Limits
		}
		if req.Alerts != nil {
			if err := validateAlertRules(*req.Alerts, cfg); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			cfg.Dashboard.Alerts = *req.Alerts
		}
		if err := s.applyLive(cfg); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	cfg := s.runtime().cfg
	writeJSON(w, http.StatusOK, map[string]any{"limits": cfg.Limits, "secure_cookies": cfg.SecureCookies, "management_db": cfg.ManagementDB, "analytics_db": cfg.Dashboard.DBPath, "provider_count": len(cfg.Providers), "model_count": len(cfg.ModelMap)})
}

func validateAlertRules(rules []AlertRule, cfg *Config) error {
	if len(rules) > 100 {
		return fmt.Errorf("at most 100 alert rules are supported")
	}
	metrics := map[string]bool{"error_rate": true, "failover_rate": true, "cost_per_hour": true, "rps": true, "tokens_per_hour": true, "latency_p95": true, "ttft_avg": true, "ttft_p95": true, "keys_in_cooldown": true, "telemetry_errors": true, "telemetry_queued": true}
	ids := map[string]bool{}
	for _, rule := range rules {
		if rule.ID == "" || ids[rule.ID] || !metrics[rule.Metric] || math.IsNaN(rule.Threshold) || math.IsInf(rule.Threshold, 0) || rule.Threshold < 0 || rule.WindowSec < 0 || rule.WindowSec > 86400 || rule.CooldownSec < 0 || rule.CooldownSec > 604800 {
			return fmt.Errorf("alert rules need unique IDs, supported metrics, non-negative thresholds, windows up to one day and cooldowns up to seven days")
		}
		ids[rule.ID] = true
		if rule.Severity != "" && rule.Severity != "info" && rule.Severity != "warning" && rule.Severity != "critical" {
			return fmt.Errorf("invalid alert severity")
		}
		if rule.Scope != "" && rule.Scope != "global" {
			kind, id, ok := strings.Cut(rule.Scope, ":")
			if !ok || kind == "backend" && cfg.Providers[id] == nil || kind == "model" && cfg.ModelMap[id] == "" || kind != "backend" && kind != "model" {
				return fmt.Errorf("invalid alert scope")
			}
		}
	}
	return nil
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	events, err := s.control.AuditEvents(r.Context(), 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "cannot load audit history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func managementProviderNames(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
