package server

import (
	"context"
	"net/http"
	"sort"
	"strings"
)

// handleModels is public: anonymous callers see every enabled model, while a
// valid client key narrows the list to that key's allowed model scope.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.runtime().cfg
	identity := ClientIdentityFromRequest(r)
	now := nowUnix()
	data := make([]modelInfo, 0, len(cfg.ModelMap))
	for id, mapping := range cfg.ModelMap {
		if !managementModelAvailable(cfg, id) || identity != nil && !identity.AllowsModel(id) {
			continue
		}
		var model modelInfo
		target := managementTarget(cfg, mapping)
		if m := catalog.Lookup(id); m != nil {
			model = *m
		} else {
			model = modelInfo{ID: id, Object: "model", Name: id, OwnedBy: target.Backend, Upstream: target.Upstream}
		}
		// display_name feeds Anthropic gateway model discovery (Claude
		// Code's /model picker reads id + display_name + description).
		if model.DisplayName == "" && model.Name != "" && model.Name != id {
			model.DisplayName = model.Name
		}
		if model.Created == 0 {
			model.Created = now
		}
		if provider := cfg.Providers[target.Backend]; provider != nil &&
			(provider.APIFormat == "systemone" || isCloudflareClef(provider.BaseURL, target.Upstream)) {
			model.Type = "systemone"
		}
		data = append(data, model)
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handlePricing never exposes catalog entries outside this client's model scope.
func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	cfg := s.runtime().cfg
	identity := ClientIdentityFromRequest(r)
	pricing := make(map[string]pricingPer1M, len(cfg.ModelMap))
	for id := range cfg.ModelMap {
		if !managementModelAvailable(cfg, id) || identity != nil && !identity.AllowsModel(id) {
			continue
		}
		if model := catalog.Lookup(id); model != nil {
			pricing[id] = model.PricingPerMillion
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "per": "1M tokens", "currency": "USD", "data": pricing})
}

// Liveness is public and deliberately contains no provider or credential data.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if s.shuttingDown.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "draining"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readiness(ctx context.Context) (bool, int) {
	ready := !s.shuttingDown.Load() && s.control != nil && s.store != nil && s.security != nil
	var owner bool
	if ready {
		ready = s.control.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM admin_auth WHERE id=1)").Scan(&owner) == nil && owner
		ready = ready && s.store.db.PingContext(ctx) == nil
	}
	cfg := s.runtime().cfg
	available := 0
	for id := range cfg.ModelMap {
		if managementModelAvailable(cfg, id) {
			available++
		}
	}
	return ready && available > 0, available
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ready, available := s.readiness(r.Context())
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "available_models": available, "draining": s.shuttingDown.Load()})
}

func managementModelAvailable(cfg *Config, id string) bool {
	mapping, exists := cfg.ModelMap[id]
	if !exists || cfg.DisabledModels[id] {
		return false
	}
	primary := managementTarget(cfg, mapping)
	if provider := cfg.Providers[primary.Backend]; provider != nil && !provider.Disabled {
		return true
	}
	for _, route := range cfg.ModelRoutes[id] {
		if provider := cfg.Providers[route.Backend]; provider != nil && !provider.Disabled {
			return true
		}
	}
	return false
}

// providerOf extracts the upstream provider name from a model_map value,
// which may be "backend:provider/model" or "provider/model".
func providerOf(v string) string {
	if i := strings.Index(v, ":"); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.Index(v, "/"); i >= 0 {
		return v[:i]
	}
	return "unknown"
}
