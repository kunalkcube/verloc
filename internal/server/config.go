package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

type ProviderConfig struct {
	Disabled         bool       `json:"disabled,omitempty"`
	BaseURL          string     `json:"base_url"`
	APIFormat        string     `json:"api_format,omitempty"`
	AuthHeader       string     `json:"auth_header"`
	AuthHeaderFormat string     `json:"auth_header_format"`
	Keys             []KeyEntry `json:"keys"`
	CooldownSeconds  int        `json:"cooldown_seconds"`
	RequestTimeout   int        `json:"request_timeout_seconds"`
	// NoAuth marks a keyless provider (e.g. opencode free tier). When true,
	// no Authorization header is sent and the keys array may be empty (a
	// single dummy slot is created so the key pool has one entry).
	NoAuth bool `json:"no_auth"`
	// SynthesizeCLIHeaders injects opencode CLI identity headers that
	// Cloudflare in front of opencode.ai requires on server-side egress.
	// Sets User-Agent, x-opencode-client/project/request/session.
	SynthesizeCLIHeaders bool `json:"synthesize_cli_headers"`
	// StripReasoningContent removes non-standard reasoning fields
	// (reasoning_content / reasoning) from assistant messages before
	// forwarding. Some upstreams (NVIDIA NIM, Stepfun, etc.) reject these
	// and return 400. Defaults to true when unset.
	StripReasoningContent *bool `json:"strip_reasoning_content"`
	// MaxTokens caps the request's max_tokens before forwarding. Some
	// upstreams enforce a lower output limit than the model's nominal max
	// (e.g. Poolside caps at 32768). 0 = no cap.
	MaxTokens int `json:"max_tokens"`
}

// KeyEntry is either a plain API key string (single-account providers like
// Vercel) or an object {account_id, api_key} for providers where the
// account id is part of the request URL (e.g. Cloudflare Workers AI:
// base_url contains %ACCOUNT% which is replaced per key).
type KeyEntry struct {
	ID        string
	Key       string
	AccountID string
}

func (k *KeyEntry) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*k = KeyEntry{Key: s}
		return nil
	}
	var o struct {
		ID        string `json:"id,omitempty"`
		AccountID string `json:"account_id"`
		APIKey    string `json:"api_key"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("invalid key entry: %w", err)
	}
	k.AccountID = o.AccountID
	k.ID = o.ID
	k.Key = o.APIKey
	return nil
}

func (k KeyEntry) MarshalJSON() ([]byte, error) {
	if k.AccountID == "" && k.ID == "" {
		return json.Marshal(k.Key)
	}
	return json.Marshal(struct {
		ID        string `json:"id,omitempty"`
		AccountID string `json:"account_id"`
		APIKey    string `json:"api_key"`
	}{ID: k.ID, AccountID: k.AccountID, APIKey: k.Key})
}

type Config struct {
	ManagementDB     string                   `json:"management_db,omitempty"`
	MasterKeyFile    string                   `json:"master_key_file,omitempty"`
	SetupTokenFile   string                   `json:"setup_token_file,omitempty"`
	SecureCookies    bool                     `json:"secure_cookies,omitempty"`
	DisabledModels   map[string]bool          `json:"disabled_models,omitempty"`
	ModelRoutes      map[string][]RouteTarget `json:"model_routes,omitempty"`
	Limits           GatewayLimits            `json:"limits,omitempty"`
	Listen           string                   `json:"listen"`
	UpstreamBase     string                   `json:"upstream_base"`
	AuthHeader       string                   `json:"auth_header"`
	AuthHeaderFormat string                   `json:"auth_header_format"`
	Keys             []string                 `json:"keys"`
	DefaultProvider  string                   `json:"default_provider"`
	ModelMap         map[string]string        `json:"model_map"`
	// ModelMeta holds runtime catalog overlays (pricing + limits) for models
	// added/edited via the dashboard. Merged over the embedded models.json.
	ModelMeta       map[string]*ModelMeta      `json:"model_meta,omitempty"`
	CooldownSeconds int                        `json:"cooldown_seconds"`
	ProxyKey        string                     `json:"proxy_key"`
	RequestTimeout  int                        `json:"request_timeout_seconds"`
	Providers       map[string]*ProviderConfig `json:"providers"`
	Dashboard       *DashboardConfig           `json:"dashboard"`
}

type RouteTarget struct {
	Backend  string `json:"backend"`
	Upstream string `json:"upstream"`
}

type GatewayLimits struct {
	MaxBodyBytes             int64 `json:"max_body_bytes"`
	MaxAttempts              int   `json:"max_attempts"`
	RetryBudgetSeconds       int   `json:"retry_budget_seconds"`
	QueueTimeoutSeconds      int   `json:"queue_timeout_seconds"`
	MaxInFlight              int   `json:"max_in_flight"`
	MaxQueued                int   `json:"max_queued"`
	StreamIdleTimeoutSeconds int   `json:"stream_idle_timeout_seconds"`
}

func (l *GatewayLimits) applyDefaults() {
	if l.MaxBodyBytes == 0 {
		l.MaxBodyBytes = 8 << 20
	}
	if l.MaxAttempts == 0 {
		l.MaxAttempts = 4
	}
	if l.RetryBudgetSeconds == 0 {
		l.RetryBudgetSeconds = 30
	}
	if l.QueueTimeoutSeconds == 0 {
		l.QueueTimeoutSeconds = 10
	}
	if l.MaxInFlight == 0 {
		l.MaxInFlight = 64
	}
	if l.MaxQueued == 0 {
		l.MaxQueued = 128
	}
	if l.StreamIdleTimeoutSeconds == 0 {
		l.StreamIdleTimeoutSeconds = 60
	}
}

// Validate accepts omitted (zero) defaults but rejects unsafe explicit values.
func (l GatewayLimits) Validate() error {
	l.applyDefaults()
	for _, bound := range []struct {
		name   string
		n, max int64
	}{
		{"max_body_bytes", l.MaxBodyBytes, 64 << 20},
		{"max_attempts", int64(l.MaxAttempts), 32},
		{"retry_budget_seconds", int64(l.RetryBudgetSeconds), 300},
		{"queue_timeout_seconds", int64(l.QueueTimeoutSeconds), 120},
		{"max_in_flight", int64(l.MaxInFlight), 4096},
		{"max_queued", int64(l.MaxQueued), 16384},
		{"stream_idle_timeout_seconds", int64(l.StreamIdleTimeoutSeconds), 600},
	} {
		if bound.n < 1 || bound.n > bound.max {
			return fmt.Errorf("%s must be between 1 and %d", bound.name, bound.max)
		}
	}
	return nil
}

// ModelMeta is dashboard-editable catalog metadata. Pricing fields are USD
// per 1M tokens (same unit as /v1/pricing and the analytics dashboard).
type ModelMeta struct {
	Name          string   `json:"name,omitempty"`
	Description   string   `json:"description,omitempty"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	Upstream      string   `json:"upstream,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	PricingInput  float64  `json:"pricing_input"`
	PricingOutput float64  `json:"pricing_output"`
	PricingCache  float64  `json:"pricing_cache"`
	PricingKnown  *bool    `json:"pricing_known,omitempty"`
}

// DashboardConfig controls the embedded analytics dashboard + persistence.
type DashboardConfig struct {
	// DBPath is the SQLite file for historical deltas + alert history.
	// Defaults to "verloc.db" next to the binary. Point it at the old
	// Node collector's verloc.db to keep existing history.
	DBPath string `json:"db_path"`
	// RecentRequests is the in-memory ring size for the live request log.
	// Default 10000. Never touches disk on the hot path.
	RecentRequests int `json:"recent_requests"`
	// Alerts are the in-app rules evaluated against rolling aggregates.
	// See AlertRule for the schema.
	Alerts []AlertRule `json:"alerts"`
}

// AlertRule triggers an alert event when `metric` exceeds `threshold`
// over `window_sec` (0 = latest tick only). Rules are global unless
// `scope` is "backend:<name>" or "model:<id>".
type AlertRule struct {
	ID        string  `json:"id"`
	Metric    string  `json:"metric"` // error_rate | cost_per_hour | rps | tokens_per_hour | failover_rate | keys_in_cooldown | latency_p95 | ttft_p95
	Scope     string  `json:"scope"`  // global | backend:<name> | model:<id>
	Threshold float64 `json:"threshold"`
	WindowSec int     `json:"window_sec"`
	// CooldownSec suppresses re-triggering the same rule within the window.
	// Default 900 (15 min).
	CooldownSec int    `json:"cooldown_sec"`
	Severity    string `json:"severity"` // info | warning | critical. Default warning
}

// backend is a resolved, ready-to-use upstream with its own key pool.
type backend struct {
	name           string
	baseURL        string
	apiFormat      string
	authHeader     string
	authFormat     string
	pool           *KeyPool
	client         *http.Client
	stripReasoning bool
	noAuth         bool
	synthesizeCLI  bool
	maxTokens      int
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	c.Limits.applyDefaults()
	if c.ManagementDB == "" {
		c.ManagementDB = "verloc-data/control.db"
	}
	if c.MasterKeyFile == "" {
		c.MasterKeyFile = "verloc-data/master.key"
	}
	if c.SetupTokenFile == "" {
		c.SetupTokenFile = "verloc-data/setup.token"
	}
	if c.DisabledModels == nil {
		c.DisabledModels = map[string]bool{}
	}
	if c.ModelRoutes == nil {
		c.ModelRoutes = map[string][]RouteTarget{}
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.AuthHeader == "" {
		c.AuthHeader = "Authorization"
	}
	if c.AuthHeaderFormat == "" {
		c.AuthHeaderFormat = "Bearer %s"
	}
	if c.CooldownSeconds <= 0 {
		c.CooldownSeconds = 30
	}
	if c.CooldownSeconds > 3600 {
		c.CooldownSeconds = 3600
	}
	if c.DefaultProvider == "" {
		c.DefaultProvider = "vercel"
	}
	if c.Keys == nil {
		c.Keys = []string{}
	}
	if c.ModelMap == nil {
		c.ModelMap = map[string]string{}
	}
	if c.ModelMeta == nil {
		c.ModelMeta = map[string]*ModelMeta{}
	}
	if c.Providers == nil {
		c.Providers = map[string]*ProviderConfig{}
	}
	if c.Dashboard == nil {
		c.Dashboard = &DashboardConfig{}
	}
	if c.Dashboard.DBPath == "" {
		c.Dashboard.DBPath = "verloc.db"
	}
	if c.Dashboard.RecentRequests <= 0 {
		c.Dashboard.RecentRequests = 10000
	}
	for i := range c.Dashboard.Alerts {
		a := &c.Dashboard.Alerts[i]
		if a.ID == "" {
			a.ID = a.Metric + "-" + a.Scope
		}
		if a.Severity == "" {
			a.Severity = "warning"
		}
		if a.CooldownSec == 0 {
			a.CooldownSec = 900
		}
	}
}

// resolveBackends builds the set of upstreams. If `providers` is set it is
// used (multi-backend); otherwise the legacy top-level fields are treated as a
// single provider named by `default_provider`.
func (c *Config) resolveBackends() (map[string]*backend, error) {
	if err := c.Limits.Validate(); err != nil {
		return nil, err
	}
	out := map[string]*backend{}

	if len(c.Providers) > 0 {
		for name, p := range c.Providers {
			if p == nil {
				continue
			}
			if p.APIFormat != "" && p.APIFormat != "openai" && p.APIFormat != "systemone" {
				return nil, fmt.Errorf("provider %q api_format must be openai or systemone", name)
			}
			if len(p.Keys) == 0 && !p.NoAuth && !p.Disabled {
				return nil, fmt.Errorf("provider %q has no keys", name)
			}
			b := &backend{
				name:          name,
				baseURL:       p.BaseURL,
				apiFormat:     p.APIFormat,
				authHeader:    p.AuthHeader,
				authFormat:    p.AuthHeaderFormat,
				noAuth:        p.NoAuth,
				synthesizeCLI: p.SynthesizeCLIHeaders,
				maxTokens:     p.MaxTokens,
			}
			if b.authHeader == "" {
				b.authHeader = "Authorization"
			}
			if b.authFormat == "" {
				b.authFormat = "Bearer %s"
			}
			strip := true
			if p.StripReasoningContent != nil {
				strip = *p.StripReasoningContent
			}
			b.stripReasoning = strip
			cd := p.CooldownSeconds
			if cd <= 0 {
				cd = c.CooldownSeconds
			}
			if cd <= 0 {
				cd = 30
			}
			// Clamp the base: an absurd value (hand-typed via Admin) would
			// overflow the backoff multiply in MarkFailure and silently
			// disable cooldowns, turning a bad provider into a retry storm.
			if cd > 3600 {
				cd = 3600
			}
			keys := p.Keys
			if p.NoAuth && len(keys) == 0 {
				keys = []KeyEntry{{Key: ""}}
			}
			b.pool = NewKeyPool(keys, time.Duration(cd)*time.Second)
			b.client = newClient(p.RequestTimeout)
			out[name] = b
		}
	} else {
		if len(c.Keys) == 0 {
			if len(c.ModelMap) == 0 {
				return out, nil
			}
			return nil, fmt.Errorf("no keys configured: set keys or providers")
		}
		name := c.DefaultProvider
		if name == "" {
			name = "vercel"
		}
		entries := make([]KeyEntry, len(c.Keys))
		for i, s := range c.Keys {
			entries[i] = KeyEntry{Key: s}
		}
		b := &backend{
			name:       name,
			baseURL:    c.UpstreamBase,
			authHeader: c.AuthHeader,
			authFormat: c.AuthHeaderFormat,
			pool:       NewKeyPool(entries, time.Duration(c.CooldownSeconds)*time.Second),
			client:     newClient(c.RequestTimeout),
		}
		if b.authHeader == "" {
			b.authHeader = "Authorization"
		}
		if b.authFormat == "" {
			b.authFormat = "Bearer %s"
		}
		out[name] = b
	}

	// Disabled providers remain resolved so disabling never destroys routing data.
	for id, v := range c.ModelMap {
		target := primaryTarget(c, v)
		if target.Upstream == "" || out[target.Backend] == nil {
			return nil, fmt.Errorf("model %q references an invalid primary route", id)
		}
		for _, route := range c.ModelRoutes[id] {
			if route.Upstream == "" || out[route.Backend] == nil {
				return nil, fmt.Errorf("model %q references an invalid fallback route", id)
			}
		}
	}

	return out, nil
}

func primaryTarget(c *Config, mapped string) RouteTarget {
	if i := strings.Index(mapped, ":"); i >= 0 {
		return RouteTarget{Backend: mapped[:i], Upstream: mapped[i+1:]}
	}
	return RouteTarget{Backend: c.DefaultProvider, Upstream: mapped}
}

func newClient(timeoutSeconds int) *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   1000,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if timeoutSeconds > 0 {
		transport.ResponseHeaderTimeout = time.Duration(min(timeoutSeconds, 300)) * time.Second
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return client
}

// cloneConfig deep-copies via JSON so map mutations never race with readers
// holding a previous runtime snapshot.
func cloneConfig(c *Config) (*Config, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	out.applyDefaults()
	return &out, nil
}

// providerFingerprint is equal when live-apply can reuse the existing key pool.
func providerFingerprint(p *ProviderConfig) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(struct {
		BaseURL              string     `json:"base_url"`
		APIFormat            string     `json:"api_format,omitempty"`
		AuthHeader           string     `json:"auth_header"`
		AuthHeaderFormat     string     `json:"auth_header_format"`
		Keys                 []KeyEntry `json:"keys"`
		CooldownSeconds      int        `json:"cooldown_seconds"`
		RequestTimeout       int        `json:"request_timeout_seconds"`
		NoAuth               bool       `json:"no_auth"`
		SynthesizeCLIHeaders bool       `json:"synthesize_cli_headers"`
		StripReasoning       *bool      `json:"strip_reasoning_content"`
		MaxTokens            int        `json:"max_tokens"`
	}{
		BaseURL: p.BaseURL, APIFormat: p.APIFormat, AuthHeader: p.AuthHeader, AuthHeaderFormat: p.AuthHeaderFormat,
		Keys: p.Keys, CooldownSeconds: p.CooldownSeconds, RequestTimeout: p.RequestTimeout,
		NoAuth: p.NoAuth, SynthesizeCLIHeaders: p.SynthesizeCLIHeaders,
		StripReasoning: p.StripReasoningContent, MaxTokens: p.MaxTokens,
	})
	return string(b)
}
