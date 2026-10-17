package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
)

// probeConn holds the connection details for an ephemeral upstream probe
// (discover / test). It works pre-save (explicit base_url + key from the
// wizard form) or against a saved provider (provider name, server-side key).
type probeConn struct {
	baseURL      string
	apiFormat    string
	authHeader   string
	authFormat   string
	key          string
	accountID    string
	noAuth       bool
	synthCLI     bool
	allowPrivate bool
}

// probeClient returns the HTTP client for wizard probes. Unless private
// upstreams are explicitly allowed, the dialer re-validates every resolved
// IP at connect time and pins the checked address — closing the
// DNS-rebinding gap between guardProbeHost's lookup and the real dial,
// where a low-TTL name could return a public IP first and a metadata IP
// second.
func probeClient(timeoutSec int, allowPrivate bool) *http.Client {
	c := newClient(timeoutSec)
	if allowPrivate {
		return c
	}
	if tr, ok := c.Transport.(*http.Transport); ok {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if ip, err := netip.ParseAddr(host); err == nil {
				if isPrivateIP(ip.Unmap()) {
					return nil, fmt.Errorf("upstream resolves to a private or local address (%s)", ip)
				}
				return dialer.DialContext(ctx, network, addr)
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				// Same rule as the guard: only a positive private match
				// blocks; resolution failure surfaces as a dial error.
				return dialer.DialContext(ctx, network, addr)
			}
			for _, ip := range ips {
				if isPrivateIP(ip.Unmap()) {
					return nil, fmt.Errorf("upstream resolves to a private or local address (%s)", ip)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		}
	}
	return c
}

func isPrivateIP(ip netip.Addr) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast()
}

type probeReq struct {
	Provider         string `json:"provider"`
	Backend          string `json:"backend"`
	BaseURL          string `json:"base_url"`
	APIFormat        string `json:"api_format"`
	APIKey           string `json:"api_key"`
	AccountID        string `json:"account_id"`
	AuthHeader       string `json:"auth_header"`
	AuthHeaderFormat string `json:"auth_header_format"`
	NoAuth           bool   `json:"no_auth"`
	SynthesizeCLI    bool   `json:"synthesize_cli_headers"`
	AllowPrivate     bool   `json:"allow_private_upstreams"`
}

// guardProbeHost blocks the probe from reaching loopback/link-local/private
// addresses unless the caller explicitly opts in. The probes are owner-only and
// never follow redirects, and a stored credential is only attached when no
// base_url is supplied — so this is defence-in-depth against using the gateway
// as a blind SSRF relay to internal metadata services, not a credential leak.
//
// The opt-in matters: a self-hosted gateway legitimately points at a LAN
// upstream (Ollama, llama.cpp, vLLM), so this must not be an unconditional
// refusal. A literal IP is checked directly; a hostname is resolved, because
// "localhost" and internal DNS names must not slip through.
func guardProbeHost(ctx context.Context, baseURL string, allowPrivate bool) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("base_url is not a valid URL")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("base_url has no host")
	}
	if allowPrivate {
		return nil
	}
	check := func(ip netip.Addr) error {
		if isPrivateIP(ip) {
			return fmt.Errorf("base_url points at a private or local address (%s); set allow_private_upstreams to permit it", ip)
		}
		return nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return check(addr.Unmap())
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		// Resolution failure must NOT block: a transient resolver blip or an
		// air-gapped host would otherwise make the admin API unusable, and the
		// probe's own connection fails anyway if the host truly cannot be
		// reached. Only a POSITIVE private-address match is grounds to refuse.
		return nil
	}
	for _, ip := range ips {
		if err := check(ip.Unmap()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) resolveProbeConn(req probeReq) (probeConn, error) {
	allowPrivate := req.AllowPrivate
	c := probeConn{
		apiFormat:  strings.TrimSpace(req.APIFormat),
		authHeader: strings.TrimSpace(req.AuthHeader),
		authFormat: strings.TrimSpace(req.AuthHeaderFormat),
		key:        strings.TrimSpace(req.APIKey),
		accountID:  strings.TrimSpace(req.AccountID),
		noAuth:     req.NoAuth,
		synthCLI:   req.SynthesizeCLI,
	}
	base := strings.TrimSpace(req.BaseURL)
	name := strings.TrimSpace(req.Provider)
	if name == "" {
		name = strings.TrimSpace(req.Backend)
	}
	if base == "" && name != "" {
		rt := s.runtime()
		p := rt.cfg.Providers[name]
		if p == nil {
			return c, fmt.Errorf("unknown provider %q", name)
		}
		base = p.BaseURL
		if c.apiFormat == "" {
			c.apiFormat = p.APIFormat
		}
		if c.authHeader == "" {
			c.authHeader = p.AuthHeader
		}
		if c.authFormat == "" {
			c.authFormat = p.AuthHeaderFormat
		}
		c.noAuth = c.noAuth || p.NoAuth
		c.synthCLI = c.synthCLI || p.SynthesizeCLIHeaders
		allowPrivate = allowPrivate || p.AllowPrivateUpstreams
		if c.key == "" && len(p.Keys) > 0 {
			c.key = p.Keys[0].Key
			if c.accountID == "" {
				c.accountID = p.Keys[0].AccountID
			}
		}
	}
	c.baseURL = strings.TrimRight(base, "/")
	if c.baseURL == "" {
		return c, fmt.Errorf("base_url is required")
	}
	if !strings.HasPrefix(c.baseURL, "http://") && !strings.HasPrefix(c.baseURL, "https://") {
		return c, fmt.Errorf("base_url must start with http:// or https://")
	}
	c.allowPrivate = allowPrivate
	// Scheme is not enough: refuse loopback/link-local/private targets unless
	// the caller opted in (a LAN upstream is a normal self-hosted setup). A
	// literal %ACCOUNT% placeholder would defeat URL parsing, so substitute
	// a stand-in for the guard only.
	guardURL := strings.ReplaceAll(c.baseURL, "%ACCOUNT%", "account")
	if c.accountID != "" {
		guardURL = strings.ReplaceAll(c.baseURL, "%ACCOUNT%", c.accountID)
	}
	if err := guardProbeHost(context.Background(), guardURL, allowPrivate); err != nil {
		return c, err
	}
	if c.apiFormat != "" && c.apiFormat != "openai" && c.apiFormat != "systemone" {
		return c, fmt.Errorf("api_format must be openai or systemone")
	}
	if c.authHeader == "" {
		c.authHeader = "Authorization"
	}
	if c.authFormat == "" {
		c.authFormat = "Bearer %s"
	}
	if c.key == "" && !c.noAuth {
		return c, fmt.Errorf("api key required (or no_auth for keyless providers)")
	}
	return c, nil
}

func (c probeConn) url(path string) string {
	u := strings.ReplaceAll(c.baseURL, "%ACCOUNT%", c.accountID)
	return u + path
}

func (c probeConn) setHeaders(r *http.Request) {
	if !c.noAuth {
		r.Header.Set(c.authHeader, fmt.Sprintf(c.authFormat, c.key))
	}
	if c.synthCLI {
		r.Header.Set("User-Agent", "opencode-cli/1.0.0")
		r.Header.Set("x-opencode-client", "cli")
		r.Header.Set("x-opencode-project", "default")
		r.Header.Set("x-opencode-request", randomUUID())
		r.Header.Set("x-opencode-session", randomUUID())
	}
	r.Header.Set("Accept", "application/json")
}

type discoveredModel struct {
	ID      string     `json:"id"`
	OwnedBy string     `json:"owned_by,omitempty"`
	Hint    *modelHint `json:"hint,omitempty"`
}

func (s *Server) handleAdminDiscover(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req probeReq
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	conn, err := s.resolveProbeConn(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	client := probeClient(15, conn.allowPrivate)
	defer client.CloseIdleConnections()
	models := []discoveredModel{}
	start := time.Now()
	cloudflare := strings.HasSuffix(conn.baseURL, "/ai/v1") && strings.Contains(conn.baseURL, "/accounts/")
	for page := 1; ; page++ {
		path := "/models"
		if cloudflare {
			path = fmt.Sprintf("/models/search?per_page=100&page=%d", page)
		}
		modelsURL := conn.url(path)
		if cloudflare {
			modelsURL = strings.ReplaceAll(strings.TrimSuffix(conn.baseURL, "/v1")+path, "%ACCOUNT%", conn.accountID)
		}
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, modelsURL, nil)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad upstream request")
			return
		}
		conn.setHeaders(upReq)
		resp, err := client.Do(upReq)
		if err != nil {
			log.Printf("[admin] discover failed")
			writeErr(w, http.StatusBadGateway, "upstream unreachable")
			return
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		resp.Body.Close()
		if readErr != nil || len(body) > 16<<20 {
			writeErr(w, http.StatusBadGateway, "upstream model list is unreadable or too large")
			return
		}
		if resp.StatusCode != http.StatusOK {
			writeErr(w, http.StatusBadGateway, errorMessage(body))
			return
		}
		pageModels := parseModelsList(body)
		models = append(models, pageModels...)
		var pagination struct {
			Success    *bool `json:"success"`
			ResultInfo struct {
				TotalPages int `json:"total_pages"`
				TotalCount int `json:"total_count"`
			} `json:"result_info"`
		}
		_ = json.Unmarshal(body, &pagination)
		if pagination.Success != nil && !*pagination.Success {
			writeErr(w, http.StatusBadGateway, "upstream model discovery failed")
			return
		}
		finished := page >= pagination.ResultInfo.TotalPages
		if pagination.ResultInfo.TotalPages == 0 {
			finished = len(pageModels) < 100
			if pagination.ResultInfo.TotalCount > 0 {
				finished = len(models) >= pagination.ResultInfo.TotalCount
			}
		}
		if !cloudflare || finished {
			break
		}
		if page >= 100 {
			writeErr(w, http.StatusBadGateway, "upstream model list exceeds discovery page limit")
			return
		}
	}
	// s.hints is set by NewServer. The previous nil-guarded lazy init was a
	// data race (two concurrent discover requests both writing the field) that
	// a -race probe confirms; the guard only looked safe because production
	// always constructs the resolver.
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	hints := s.hints.hintsFor(r.Context(), s.runtime().cfg, ids)
	for i := range models {
		models[i].Hint = mergeDiscoveredModelHint(models[i].Hint, hints[models[i].ID])
	}
	log.Printf("[admin] discover models=%d dur=%v", len(models), time.Since(start))
	writeJSON(w, http.StatusOK, map[string]any{"models": models, "count": len(models)})
}

// Keep each native model object intact for metadata decoding. TypeSafe names
// its IDs "name"; Workers AI returns API names beside internal UUIDs.
func parseModelsList(body []byte) []discoveredModel {
	var envelope struct {
		Data    []json.RawMessage `json:"data"`
		Models  []json.RawMessage `json:"models"`
		Result  []json.RawMessage `json:"result"`
		Success *bool             `json:"success"`
	}
	var rows []json.RawMessage
	cloudflare := false
	if json.Unmarshal(body, &envelope) == nil {
		if envelope.Success != nil && !*envelope.Success {
			return []discoveredModel{}
		}
		switch {
		case envelope.Data != nil:
			rows = envelope.Data
		case envelope.Models != nil:
			rows = envelope.Models
		case envelope.Result != nil:
			rows, cloudflare = envelope.Result, true
		}
	}
	if rows == nil {
		_ = json.Unmarshal(body, &rows)
	}
	out := make([]discoveredModel, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, raw := range rows {
		var model struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			OwnedBy string `json:"owned_by"`
			Owner   string `json:"owner"`
		}
		if json.Unmarshal(raw, &model) != nil {
			continue
		}
		id := model.ID
		if id == "" || cloudflare {
			id = model.Name
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		owner := model.OwnedBy
		if owner == "" {
			owner = model.Owner
		}
		out = append(out, discoveredModel{ID: id, OwnedBy: owner, Hint: hintForDiscoveredModel(raw, "upstream")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) handleAdminModelHints(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Upstreams []string `json:"upstreams"`
	}
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	if len(req.Upstreams) == 0 || len(req.Upstreams) > 256 {
		writeErr(w, http.StatusBadRequest, "supply 1–256 upstream model IDs")
		return
	}
	for _, upstream := range req.Upstreams {
		if strings.TrimSpace(upstream) == "" || len(upstream) > 256 {
			writeErr(w, http.StatusBadRequest, "invalid upstream model ID")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hints": s.hints.hintsFor(r.Context(), s.runtime().cfg, req.Upstreams)})
}

type testReq struct {
	probeReq
	Upstream  string `json:"upstream"`
	MaxTokens int    `json:"max_tokens"`
}

func (s *Server) handleAdminTest(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req testReq
	if !decodeAdminJSON(w, r, &req) {
		return
	}
	upstream := strings.TrimSpace(req.Upstream)
	if upstream == "" {
		writeErr(w, http.StatusBadRequest, "upstream is required")
		return
	}
	conn, err := s.resolveProbeConn(req.probeReq)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16
	}
	if maxTokens > 64 {
		maxTokens = 64
	}
	endpoint := "/chat/completions"
	if conn.apiFormat == "systemone" || isCloudflareClef(conn.baseURL, upstream) {
		endpoint = "/systemone"
	}
	targetURL, model, cloudflareEnvelope, err := inferenceTarget(conn.baseURL, conn.apiFormat, upstream, endpoint)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var payload []byte
	if endpoint == "/systemone" {
		payload, _ = json.Marshal(map[string]any{
			"model": model, "state": "This is a connectivity check.",
			"questions": map[string]any{"reachable": map[string]string{
				"type": "noul", "instructions": "Is this a connectivity check?",
			}},
		})
	} else {
		payload, _ = json.Marshal(map[string]any{
			"model": model, "messages": []map[string]string{{"role": "user", "content": "Reply with the word PONG"}},
			"max_tokens": maxTokens, "stream": false,
		})
	}
	client := probeClient(30, conn.allowPrivate)
	defer client.CloseIdleConnections()
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.ReplaceAll(targetURL, "%ACCOUNT%", conn.accountID), bytes.NewReader(payload))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad upstream request")
		return
	}
	conn.setHeaders(upReq)
	upReq.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(upReq)
	if err != nil {
		log.Printf("[admin] test upstream=%s err=%v", upstream, err)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": 0, "latency_ms": time.Since(start).Milliseconds(),
			"error": "upstream unreachable",
			"hint":  "verloc cannot reach the base_url — check the URL and network",
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	latMs := time.Since(start).Milliseconds()
	if resp.StatusCode != http.StatusOK {
		msg := errorMessage(body)
		if msg == "" {
			msg = fmt.Sprintf("upstream status %d", resp.StatusCode)
		}
		log.Printf("[admin] test upstream=%s status=%d", upstream, resp.StatusCode)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "status": resp.StatusCode, "latency_ms": latMs,
			"error": truncate(msg, 300), "hint": testHint(resp.StatusCode, string(body)),
		})
		return
	}
	reply := chatReply(body)
	if endpoint == "/systemone" {
		native, err := systemOneResponse(body, cloudflareEnvelope)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "status": http.StatusBadGateway, "latency_ms": latMs,
				"error": "upstream returned an invalid System One response",
			})
			return
		}
		var result struct {
			Answers json.RawMessage `json:"answers"`
		}
		_ = json.Unmarshal(native, &result)
		reply = string(result.Answers)
	}
	log.Printf("[admin] test upstream=%s ok lat=%dms", upstream, latMs)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": 200, "latency_ms": latMs,
		"reply": truncate(reply, 200),
	})
}

func chatReply(body []byte) string {
	var p struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &p); err != nil || len(p.Choices) == 0 {
		return ""
	}
	return strings.TrimSpace(p.Choices[0].Message.Content)
}

// testHint maps common failure shapes to the next action, covering the
// known bug class of wrong output limits (hy 1M, oc 131k, poolside 32k).
func testHint(status int, msg string) string {
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusNotFound || strings.Contains(lower, "model") && strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist"):
		return "upstream does not know this model id — re-check the upstream id from discovery"
	case strings.Contains(lower, "max_tokens") || strings.Contains(lower, "max output") || strings.Contains(lower, "too large"):
		return "output limit rejected — lower max output tokens / provider max_tokens cap"
	case status == http.StatusUnauthorized || status == http.StatusForbidden || strings.Contains(lower, "balance") || strings.Contains(lower, "insufficient") || strings.Contains(lower, "quota"):
		return "key or balance issue — check the API key and account credit"
	case status == http.StatusTooManyRequests:
		return "rate limited — retry in a bit"
	default:
		return ""
	}
}
