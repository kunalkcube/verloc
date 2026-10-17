package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var hopByHop = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// maxSSELineBytes bounds a single SSE line. bufio.Scanner returns ErrTooLong
// past the limit, which would abort an otherwise healthy stream, so this is
// generous: providers occasionally coalesce large deltas (base64 images, big
// tool-call arguments) into one event. The buffer grows on demand, so a large
// limit costs nothing for ordinary traffic.
const maxSSELineBytes = 8 << 20

// Matches balance / rate-limit style errors in a 400 body so we can fail over.
var balanceRe = regexp.MustCompile(`(?i)(insufficient|quota|balance|credit|rate.?limit|try again later|payment)`)

// Matches genuine client-side request errors that will fail identically on
// every key, so we must NOT fail over (doing so exhausts the whole pool).
var nonRetryableRe = regexp.MustCompile(`(?i)(context_length_exceeded|maximum context|reduce the length|invalid_request_error|context length|too long)`)

// runtimeState is swapped atomically on live config apply so in-flight
// requests keep a stable cfg + backend map for the duration of the call.
type runtimeState struct {
	cfg          *Config
	backends     map[string]*backend
	modelPrices  map[string]requestPrice
	targetPrices map[RouteTarget]requestPrice
}

type requestPrice struct {
	known                bool
	input, output, cache float64
}

func (p requestPrice) cost(in, out, cache uint64) float64 {
	if !p.known {
		return 0
	}
	cache = min(cache, in)
	return (float64(in-cache)*p.input + float64(out)*p.output + float64(cache)*p.cache) / 1e6
}

func newRuntimeState(cfg *Config, backends map[string]*backend) *runtimeState {
	rt := &runtimeState{cfg: cfg, backends: backends, modelPrices: make(map[string]requestPrice, len(cfg.ModelMap)), targetPrices: make(map[RouteTarget]requestPrice, len(cfg.ModelMap))}
	for id, mapped := range cfg.ModelMap {
		var price requestPrice
		if meta := cfg.ModelMeta[id]; meta != nil {
			price = requestPrice{known: meta.PricingKnown == nil || *meta.PricingKnown, input: meta.PricingInput, output: meta.PricingOutput, cache: meta.PricingCache}
		} else if model := catalog.Lookup(id); model != nil {
			p := model.PricingPerMillion
			price = requestPrice{known: model.PricingKnown, input: p.Input, output: p.Output, cache: p.CacheRead}
		}
		rt.modelPrices[id] = price
		target := primaryTarget(cfg, mapped)
		if previous, exists := rt.targetPrices[target]; exists && previous != price {
			// An ambiguous alias price cannot establish fallback cost coverage.
			rt.targetPrices[target] = requestPrice{}
		} else {
			rt.targetPrices[target] = price
		}
	}
	return rt
}

type Server struct {
	rt            atomic.Pointer[runtimeState]
	metrics       *Metrics
	recent        *RecentLog
	store         *Store
	hub           *Hub
	alerts        *AlertEngine
	ticker        *Ticker
	hints         *hintResolver
	reasonCache   *reasoningCache
	control       *ControlStore
	security      *Security
	shuttingDown  atomic.Bool
	admissionOnce sync.Once
	admissionMu   sync.Mutex
	admissionWake chan struct{}
	active        int
	queued        int
}

func (s *Server) runtime() *runtimeState {
	return s.rt.Load()
}

// SetTelemetry wires the analytics subsystems (store, live hub, alerts,
// recent-request log) into the server. Called once at startup after the
// store is opened; all fields are optional.
func (s *Server) SetTelemetry(store *Store, hub *Hub, alerts *AlertEngine, recent *RecentLog) {
	s.store = store
	s.hub = hub
	s.alerts = alerts
	if recent == nil {
		recent = NewRecentLog(10000)
	}
	s.recent = recent
	s.wirePoolHooks(s.runtime().backends)
}

func (s *Server) wirePoolHooks(backends map[string]*backend) {
	store, hub := s.store, s.hub
	for name, b := range backends {
		name := name
		b.pool.SetEventHook(func(key, event string, status int, msg string) {
			if store != nil {
				// Queued, never blocking: key failures can arrive in storms
				// (e.g. a fresh provider with bad keys) and must not hold
				// up request handling behind SQLite.
				store.QueueKeyEvent(time.Now().UnixMilli(), name, key, event, status, msg)
			}
			if hub != nil {
				hub.Publish(map[string]any{
					"type":  "key_event",
					"event": KeyEvent{TS: time.Now().UnixMilli(), Backend: name, Key: key, Event: event, Status: status, Msg: msg},
				})
			}
		})
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/chat/completions")
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/responses")
}

func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/systemone")
}

func isCloudflareClef(baseURL, upstream string) bool {
	if upstream != "@cf/cloudflare/clef" && upstream != "@cf/cloudflare/clef-flash" {
		return false
	}
	base := strings.TrimRight(baseURL, "/")
	return strings.HasSuffix(base, "/ai/v1") && strings.Contains(base, "/accounts/")
}

// inferenceTarget keeps native API routing separate from chat normalization.
// Account placeholders remain intact until the credential is selected.
func inferenceTarget(baseURL, apiFormat, upstream, endpoint string) (url, model string, cloudflareEnvelope bool, err error) {
	base := strings.TrimRight(baseURL, "/")
	clef := isCloudflareClef(base, upstream)
	native := apiFormat == "systemone" || clef
	if (endpoint == "/systemone") != native {
		return "", "", false, errors.New("model does not support this inference endpoint")
	}
	if clef {
		return strings.TrimSuffix(base, "/v1") + "/run/" + upstream, strings.TrimPrefix(upstream, "@cf/cloudflare/"), true, nil
	}
	return base + endpoint, upstream, false, nil
}

// Native model remapping preserves the raw JSON values of every other field,
// including exact numeric state and arbitrary typed question/image extensions.
func remapSystemOneBody(raw []byte, model string) []byte {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return raw
	}
	body["model"], _ = json.Marshal(model)
	modified, err := json.Marshal(body)
	if err != nil {
		return raw
	}
	return modified
}

// The Workers AI REST API wraps the native result, unlike TypeSafe. Accept a
// direct native result too, but never report an API error envelope as success.
func systemOneResponse(body []byte, cloudflareEnvelope bool) ([]byte, error) {
	if cloudflareEnvelope {
		var envelope struct {
			Success *bool           `json:"success"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(body, &envelope) != nil {
			return nil, errors.New("invalid native response")
		}
		if envelope.Success != nil || envelope.Result != nil {
			if envelope.Success == nil || !*envelope.Success || len(envelope.Result) == 0 {
				return nil, errors.New("unsuccessful native response")
			}
			body = envelope.Result
		}
	}
	var native struct {
		Model   string          `json:"model"`
		Answers json.RawMessage `json:"answers"`
	}
	if json.Unmarshal(body, &native) != nil || native.Model == "" {
		return nil, errors.New("invalid native response")
	}
	answers := bytes.TrimSpace(native.Answers)
	if len(answers) == 0 || answers[0] != '{' {
		return nil, errors.New("invalid native response")
	}
	return body, nil
}

// initGateway is safe for pure runtime constructors and direct handler tests.
func (s *Server) initGateway() {
	s.admissionOnce.Do(func() { s.admissionWake = make(chan struct{}) })
}

// Live settings and shutdown wake queued requests without replacing the gate.
func (s *Server) gatewayConfigChanged() {
	s.initGateway()
	s.admissionMu.Lock()
	close(s.admissionWake)
	s.admissionWake = make(chan struct{})
	s.admissionMu.Unlock()
}

func (s *Server) admissionLimits(fallback GatewayLimits) GatewayLimits {
	if rt := s.runtime(); rt != nil {
		fallback = rt.cfg.Limits
	}
	fallback.applyDefaults()
	return fallback
}

func (s *Server) releaseGateway() {
	s.admissionMu.Lock()
	s.active--
	if s.queued > 0 {
		close(s.admissionWake)
		s.admissionWake = make(chan struct{})
	}
	s.admissionMu.Unlock()
}

func (s *Server) acquireGateway(ctx context.Context, limits GatewayLimits) error {
	s.initGateway()
	s.admissionMu.Lock()
	limits = s.admissionLimits(limits)
	if ctx.Err() != nil {
		s.admissionMu.Unlock()
		return ctx.Err()
	}
	if s.shuttingDown.Load() {
		s.admissionMu.Unlock()
		return errors.New("gateway shutting down")
	}
	if s.active < limits.MaxInFlight {
		s.active++
		s.admissionMu.Unlock()
		return nil
	}
	if s.queued >= limits.MaxQueued {
		s.admissionMu.Unlock()
		return errors.New("gateway queue full")
	}
	s.queued++
	s.admissionMu.Unlock()
	defer func() { s.admissionMu.Lock(); s.queued--; s.admissionMu.Unlock() }()
	timer := time.NewTimer(time.Duration(limits.QueueTimeoutSeconds) * time.Second)
	defer timer.Stop()
	for {
		s.admissionMu.Lock()
		if s.shuttingDown.Load() {
			s.admissionMu.Unlock()
			return errors.New("gateway shutting down")
		}
		if ctx.Err() != nil {
			s.admissionMu.Unlock()
			return ctx.Err()
		}
		limits = s.admissionLimits(limits)
		if s.active < limits.MaxInFlight {
			s.active++
			s.admissionMu.Unlock()
			return nil
		}
		wake := s.admissionWake
		s.admissionMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("gateway queue timeout")
		case <-wake:
		}
	}
}

func proxyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "gateway_error"}})
}

// trackingResponseWriter records whether any bytes were committed to the
// client. The panic recovery in forward needs this: before the first write a
// 500 is still deliverable, after it the only correct action is to abort the
// connection (appending JSON to a half-written body would corrupt it).
type trackingResponseWriter struct {
	http.ResponseWriter
	wrote bool
}

// Unwrap lets http.ResponseController reach SetReadDeadline/SetWriteDeadline
// and Flush on the real writer through this wrapper.
func (w *trackingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackingResponseWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackingResponseWriter) Write(p []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(p)
}

// FlushError keeps the streaming path's interface check working through the
// wrapper; without it a wrapped writer silently loses per-chunk flushing.
func (w *trackingResponseWriter) FlushError() error {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(time.Minute))
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

type requestMetadata struct {
	Model         string `json:"model"`
	Stream        bool   `json:"stream"`
	MaxTokens     uint64 `json:"max_tokens"`
	MaxCompletion uint64 `json:"max_completion_tokens"`
	MaxOutput     uint64 `json:"max_output_tokens"`
}

func tokenReservation(p requestMetadata, bodyBytes int) uint64 {
	output := max(p.MaxTokens, p.MaxCompletion, p.MaxOutput)
	if output == 0 {
		output = 32768
		if m := catalog.Lookup(p.Model); m != nil && m.MaxTokens > 0 {
			output = uint64(m.MaxTokens)
		}
	}
	input := uint64(bodyBytes) // byte count is conservative for text tokenization.
	if output > ^uint64(0)-input {
		return ^uint64(0)
	}
	return input + output
}

// Non-text and referenced inputs cannot be bounded by serialized request bytes.
// Their reservation must use the configured model context upper bound.
func nonTextInput(value any) bool {
	switch value := value.(type) {
	case nil, string:
		return false
	case []any:
		for _, part := range value {
			if nonTextInput(part) {
				return true
			}
		}
		return false
	case map[string]any:
		if content, ok := value["content"]; ok {
			return nonTextInput(content)
		}
		kind, _ := value["type"].(string)
		switch kind {
		case "text", "input_text", "output_text", "refusal", "function_call":
			return false
		case "function_call_output":
			return nonTextInput(value["output"])
		}
		return true
	default:
		return true
	}
}

// Only TPM-limited clients require normalization. Chat output reservations are
// enforced in the forwarded body; native typed APIs reserve their full context.
func prepareTPMRequest(raw []byte, metadata requestMetadata, cfg *Config, endpoint string) ([]byte, uint64, error) {
	maxOutput, contextWindow := 32768, 0
	if model := catalog.Lookup(metadata.Model); model != nil {
		if model.MaxTokens > 0 {
			maxOutput = model.MaxTokens
		}
		contextWindow = model.ContextWindow
	}
	if model := cfg.ModelMeta[metadata.Model]; model != nil {
		if model.MaxTokens > 0 {
			maxOutput = model.MaxTokens
		}
		if model.ContextWindow > 0 {
			contextWindow = model.ContextWindow
		}
	}
	if endpoint == "/systemone" {
		if contextWindow <= 0 {
			return nil, 0, errors.New("TPM-limited System One requests require a configured model context window")
		}
		// Native typed evaluation has no max-token parameter. Reserve the full
		// context plus serialized schema/state rather than injecting chat limits;
		// actual native usage reconciles the lease when the call completes.
		return raw, saturatingTokenAdd(uint64(contextWindow), uint64(len(raw))), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, 0, err
	}
	output := uint64(maxOutput)
	if requested := max(metadata.MaxTokens, metadata.MaxCompletion, metadata.MaxOutput); requested > 0 {
		output = min(output, requested)
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if _, present := body[field]; present {
			body[field] = output
		}
	}
	if endpoint == "/responses" {
		body["max_output_tokens"] = output
	} else if metadata.MaxTokens == 0 && metadata.MaxCompletion == 0 {
		body["max_tokens"] = output
	}
	multimodal := nonTextInput(body["input"])
	if messages, ok := body["messages"].([]any); ok {
		for _, message := range messages {
			if message, ok := message.(map[string]any); ok && nonTextInput(message["content"]) {
				multimodal = true
			}
		}
	}
	if reference, ok := body["previous_response_id"].(string); ok && reference != "" {
		multimodal = true
	}
	if body["conversation"] != nil {
		multimodal = true
	}
	input := uint64(len(raw))
	if multimodal {
		if contextWindow <= 0 {
			return nil, 0, errors.New("TPM-limited non-text input requires a configured model context window")
		}
		input = max(input, uint64(contextWindow))
	}
	reservation := ^uint64(0)
	if output <= ^uint64(0)-input {
		reservation = input + output
	}
	bounded, err := json.Marshal(body)
	return bounded, reservation, err
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, endpoint string) {
	// Wrap first so the panic recovery below can determine whether a response
	// was already committed to the client.
	tw := &trackingResponseWriter{ResponseWriter: w}
	w = tw
	start := time.Now()
	rt := s.runtime()
	limits := rt.cfg.Limits
	limits.applyDefaults()
	id := randomUUID()
	w.Header().Set("X-Request-ID", id)
	rec := ReqRecord{TS: nowMs(), ID: id, IP: clientIP(r), UA: truncate(r.Header.Get("User-Agent"), 60),
		Status: http.StatusBadRequest, Outcome: "invalid_request"}
	if identity := ClientIdentityFromRequest(r); identity != nil {
		rec.ClientKeyID, rec.ClientName = identity.ID, identity.Name
	}
	var lease *ClientLease
	var selectedPrice requestPrice
	defer func() {
		// A panic anywhere in forward() must not take down the process or
		// lose the request record. net/http only recovers the connection, and
		// we have no other recover on the request path (proxy_test.go covers
		// the base64/regex/decoder paths but nothing will ever be complete).
		// The tracking writer tells us whether a 5xx is still sendable: once
		// bytes are committed mid-stream we can only abort the connection.
		if p := recover(); p != nil {
			rec.OK, rec.Status, rec.Outcome = false, 500, "internal_error"
			rec.ErrMsg = "gateway internal error"
			log.Printf("[panic] request=%s model=%s recovered: %v", rec.ID, rec.Model, p)
			if tw, ok := w.(*trackingResponseWriter); ok && !tw.wrote {
				proxyError(w, 500, "gateway internal error")
			}
		}
		if !rec.OK && r.Context().Err() != nil {
			rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = false, 499, "canceled", "client canceled"
		}
		rec.LatMs = float64(time.Since(start).Nanoseconds()) / 1e6
		rec.Failover = max(0, len(rec.AttemptLog)-1)
		rec.CostKnown = rec.UsageKnown && selectedPrice.known
		rec.Cost = selectedPrice.cost(rec.TokIn, rec.TokOut, rec.TokCache)
		if lease != nil {
			// No upstream attempt means no tokens could have been consumed.
			lease.Finish(rec.TokIn+rec.TokOut, rec.UsageKnown || len(rec.AttemptLog) == 0)
		}
		if s.metrics != nil {
			metered := rec
			if _, configured := rt.cfg.ModelMap[metered.Model]; !configured {
				metered.Model = "(unknown)"
			}
			s.metrics.RecordRequest(metered)
		}
		if s.store != nil {
			s.store.QueueRequest(rec)
		} else if s.recent != nil {
			// The in-memory ring is only the fallback source for
			// /dash/api/recent; when the analytics store is present it is the
			// authority and the ring would be a third copy of the same record
			// (plus a crypto/rand shortID) on every request.
			s.recent.Add(rec)
		}
		if s.hub != nil {
			s.hub.Publish(map[string]any{"type": "req", "req": rec})
		}
	}()
	fail := func(status int, outcome, message string) {
		rec.Status, rec.Outcome, rec.ErrMsg = status, outcome, message
		if r.Context().Err() == nil {
			proxyError(w, status, message)
		}
	}
	if s.shuttingDown.Load() {
		fail(503, "unavailable", "gateway shutting down")
		return
	}
	if r.Body == nil {
		fail(400, "invalid_request", "request body required")
		return
	}
	if endpoint == "/systemone" {
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
			fail(http.StatusUnsupportedMediaType, "invalid_request", "System One requests require uncompressed JSON")
			return
		}
	}
	if err := s.acquireGateway(r.Context(), limits); err != nil {
		w.Header().Set("Retry-After", "1")
		fail(503, "overloaded", "gateway capacity unavailable")
		return
	}
	defer s.releaseGateway()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(time.Duration(limits.QueueTimeoutSeconds) * time.Second))
	r.Body = http.MaxBytesReader(tw.ResponseWriter, r.Body, limits.MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(413, "body_too_large", "request body too large")
		} else {
			fail(400, "invalid_request", "failed to read body")
		}
		return
	}
	var metadata requestMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		fail(400, "invalid_request", "invalid request JSON")
		return
	}
	rec.Model, rec.Stream = metadata.Model, metadata.Stream
	mapped, configured := rt.cfg.ModelMap[rec.Model]
	if !configured || rec.Model == "" || rt.cfg.DisabledModels[rec.Model] {
		fail(404, "model_unavailable", "unknown or disabled model")
		return
	}
	primary := primaryTarget(rt.cfg, mapped)
	if endpoint == "/systemone" && metadata.Stream {
		fail(400, "invalid_request", "System One does not support streaming")
		return
	}
	if b := rt.backends[primary.Backend]; b != nil {
		if _, _, _, err := inferenceTarget(b.baseURL, b.apiFormat, primary.Upstream, endpoint); err != nil {
			fail(400, "incompatible_endpoint", err.Error())
			return
		}
	}
	if s.security != nil {
		reservation := tokenReservation(metadata, len(raw))
		if identity := ClientIdentityFromRequest(r); identity != nil && identity.TPM > 0 {
			raw, reservation, err = prepareTPMRequest(raw, metadata, rt.cfg, endpoint)
			if err != nil {
				fail(400, "invalid_request", "TPM-limited input requires known model token bounds")
				return
			}
		}
		var policy *PolicyError
		lease, policy = s.security.AcquireClient(r, rec.Model, reservation)
		if policy != nil {
			if policy.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(policy.RetryAfter))
			}
			fail(policy.Status, "policy_rejected", policy.Message)
			return
		}
		if lease.Identity != nil {
			rec.ClientKeyID, rec.ClientName = lease.Identity.ID, lease.Identity.Name
		}
	}
	// Budget applies until an attempt is ready to flush, never to the whole stream.
	deadline := time.Now().Add(time.Duration(limits.RetryBudgetSeconds) * time.Second)
	routes := rt.cfg.ModelRoutes[rec.Model]
	rec.AttemptLog = make([]RequestAttempt, 0, limits.MaxAttempts)
	attempts := 0
	var previousBackend *backend
	var previousKey *KeyState
	lastStatus := http.StatusServiceUnavailable
	for routeIndex := range len(routes) + 1 {
		target := primary
		if routeIndex > 0 {
			target = routes[routeIndex-1]
		}
		b := rt.backends[target.Backend]
		if b == nil || target.Upstream == "" {
			continue
		}
		if p := rt.cfg.Providers[target.Backend]; p != nil && p.Disabled {
			continue
		}
		upstreamURL, upstreamModel, cloudflareEnvelope, routeErr := inferenceTarget(b.baseURL, b.apiFormat, target.Upstream, endpoint)
		if routeErr != nil {
			continue // Never convert typed evaluation into chat during failover.
		}
		var modified []byte
		if endpoint == "/systemone" {
			modified = remapSystemOneBody(raw, upstreamModel)
		} else {
			modified = remapBody(raw, upstreamModel, b.name == "mistral", b.name)
			modified = stripReasoningContent(modified, b.stripReasoning)
			if !b.stripReasoning {
				modified = ensureReasoningReplay(modified, s.reasonCache)
			}
			modified = capMaxTokens(modified, b.maxTokens)
		}
		tried := make(map[credentialKey]bool, min(b.pool.Count(), limits.MaxAttempts))
		for attempts < limits.MaxAttempts && time.Now().Before(deadline) && r.Context().Err() == nil {
			ks := b.pool.NextUntried(tried)
			if ks == nil {
				break
			}
			tried[keyIdentity(ks)] = true
			if previousKey != nil {
				previousBackend.pool.RecordFailover(previousKey)
				if s.metrics != nil {
					s.metrics.RecordFailover(rec.Model, previousBackend.name)
				}
			}
			previousBackend, previousKey = b, ks
			attempts++
			rec.Backend, rec.Upstream, rec.Key = b.name, target.Upstream, maskKey(ks.Key)
			selectedPrice = rt.modelPrices[rec.Model]
			if routeIndex > 0 {
				selectedPrice = rt.targetPrices[target]
			}
			attemptStart := time.Now()
			ctx, cancel := context.WithCancel(r.Context())
			budgetTimer := time.AfterFunc(time.Until(deadline), cancel)
			upURL := strings.ReplaceAll(upstreamURL, "%ACCOUNT%", ks.AccountID)
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, upURL, bytes.NewReader(modified))
			if err != nil {
				budgetTimer.Stop()
				cancel()
				fail(502, "upstream_error", "invalid upstream configuration")
				return
			}
			copyRequestHeaders(req, r)
			req.Header.Set("X-Request-ID", id)
			req.Header.Set("Content-Type", "application/json")
			if !b.noAuth {
				req.Header.Set(b.authHeader, fmt.Sprintf(b.authFormat, ks.Key))
			}
			if b.synthesizeCLI {
				req.Header.Set("User-Agent", "opencode-cli/1.0.0")
				req.Header.Set("x-opencode-client", "cli")
				req.Header.Set("x-opencode-project", "default")
				req.Header.Set("x-opencode-request", randomUUID())
				req.Header.Set("x-opencode-session", randomUUID())
			}
			attempt := RequestAttempt{Backend: b.name, Upstream: target.Upstream, Outcome: "upstream_error"}
			finishAttempt := func(status int, outcome string) {
				attempt.Status, attempt.Outcome = status, outcome
				attempt.LatMs = float64(time.Since(attemptStart).Nanoseconds()) / 1e6
				rec.AttemptLog = append(rec.AttemptLog, attempt)
			}
			resp, err := b.client.Do(req)
			if err != nil {
				budgetTimer.Stop()
				cancel()
				b.pool.RecordErr(ks)
				if r.Context().Err() != nil {
					finishAttempt(499, "canceled")
					return
				}
				b.pool.MarkFailure(ks, 0)
				finishAttempt(502, "upstream_error")
				lastStatus = 502
				continue
			}
			lastStatus = resp.StatusCode
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
				_ = resp.Body.Close()
				budgetTimer.Stop()
				cancel()
				retryable := isRetryableStatus(resp.StatusCode) ||
					(resp.StatusCode == 400 && balanceRe.Match(body) && !nonRetryableRe.Match(body))
				b.pool.RecordErr(ks)
				finishAttempt(resp.StatusCode, "upstream_rejected")
				if !retryable {
					fail(resp.StatusCode, "upstream_rejected", "upstream rejected request")
					return
				}
				b.pool.MarkFailure(ks, retryAfterDuration(resp))
				continue
			}
			if rec.Stream {
				idleTimeout := time.Duration(limits.StreamIdleTimeoutSeconds) * time.Second
				idle := newIdleReader(resp.Body, idleTimeout, cancel)
				streamWriter := &committedStreamWriter{ResponseWriter: w, response: resp, requestID: id, idle: idleTimeout,
					beforeCommit: func() error {
						if !budgetTimer.Stop() || ctx.Err() != nil {
							return errors.New("retry budget exhausted")
						}
						return nil
					}}
				var acc *sseReasoningAcc
				if !b.stripReasoning {
					acc = &sseReasoningAcc{}
				}
				err = streamCopyWithUsage(streamWriter, idle, func(i, o, c uint64) {
					rec.TokIn, rec.TokOut, rec.TokCache, rec.UsageKnown = i, o, c, true
				}, func() { rec.TTFTMs = float64(time.Since(start).Nanoseconds()) / 1e6 }, streamOptions{
					aliasReasoning: !b.stripReasoning,
					acc:            acc,
				})
				budgetTimer.Stop()
				idle.Close()
				cancel()
				_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
				if err == nil {
					acc.store(s.reasonCache)
				} else {
					outcome := "interrupted"
					if idle.timedOut.Load() {
						outcome = "idle_timeout"
					}
					var clientErr *streamClientError
					if streamWriter.committed && errors.As(err, &clientErr) {
						outcome = "canceled"
					}
					if r.Context().Err() != nil {
						outcome = "canceled"
					}
					rec.Status, rec.Outcome, rec.ErrMsg = 502, outcome, "upstream stream interrupted"
					if outcome == "canceled" {
						rec.Status = 499
					}
					if outcome == "idle_timeout" {
						rec.Status = 504
					}
					b.pool.RecordErr(ks)
					finishAttempt(rec.Status, outcome)
					if !streamWriter.committed && r.Context().Err() == nil {
						lastStatus = 502
						continue
					}
					return // The first client write permanently commits this route.
				}
			} else {
				// The retry budget bounds how long we will keep trying to find a
				// usable upstream, NOT how long a healthy upstream may take to
				// generate a response. Once a 200 is in hand the upstream has
				// accepted the work, so stop the budget here — otherwise a slow
				// reasoning model (or the sum of several retried attempts) gets
				// its own valid response thrown away and reported as 502/504.
				// Mirror of the streaming path's beforeCommit check.
				if !budgetTimer.Stop() || ctx.Err() != nil {
					_ = resp.Body.Close()
					cancel()
					b.pool.RecordErr(ks)
					finishAttempt(502, "interrupted")
					lastStatus = 502
					continue
				}
				// A stall guard still applies: the upstream must keep sending.
				// The idle reader cancels the context on inactivity so a hung
				// connection cannot pin a request forever.
				idleTimeout := time.Duration(limits.StreamIdleTimeoutSeconds) * time.Second
				idle := newIdleReader(resp.Body, idleTimeout, cancel)
				var body []byte
				body, err = io.ReadAll(io.LimitReader(idle, (64<<20)+1))
				_ = idle.Close()
				if err == nil && (len(body) > 64<<20 || !json.Valid(body)) {
					err = errors.New("incomplete upstream response")
				}
				cancel()
				if err == nil && endpoint == "/systemone" {
					body, err = systemOneResponse(body, cloudflareEnvelope)
				}
				if err != nil {
					b.pool.RecordErr(ks)
					finishAttempt(502, "interrupted")
					lastStatus = 502
					continue
				}
				rec.TokIn, rec.TokOut, rec.TokCache, rec.UsageKnown = usageOf(body)
				if endpoint != "/systemone" && !b.stripReasoning {
					body = rememberFromResponse(body, s.reasonCache)
				}
				copyResponseHeaders(w, resp, false)
				w.Header().Del("Content-Length") // Reasoning aliases can change the body length.
				w.Header().Set("X-Request-ID", id)
				w.WriteHeader(http.StatusOK)
				if _, err = w.Write(body); err != nil {
					rec.Status, rec.Outcome, rec.ErrMsg = 499, "canceled", "client write failed"
					b.pool.RecordErr(ks)
					finishAttempt(499, "canceled")
					return
				}
			}
			rec.OK, rec.Status, rec.Outcome, rec.ErrMsg = true, 200, "completed", ""
			rec.CostKnown = rec.UsageKnown && selectedPrice.known
			rec.Cost = selectedPrice.cost(rec.TokIn, rec.TokOut, rec.TokCache)
			b.pool.RecordSuccess(ks, rec.TokIn, rec.TokOut, rec.TokCache, rec.Cost)
			finishAttempt(200, "completed")
			return
		}
	}
	if !time.Now().Before(deadline) {
		fail(504, "retry_budget_exhausted", "upstream retry budget exhausted")
	} else {
		fail(lastStatus, "unavailable", "upstream unavailable")
	}
}

// remapBody rewrites the "model" field to the upstream model name. When
// `isMistral` is true it also normalises request shapes that Mistral
// rejects with 422 but that other backends (vercel, anthropic, ollama,
// nvidia, etc.) accept as-is. The full list of Mistral-specific
// normalisations is in the body below.
func remapBody(raw []byte, upstreamModel string, isMistral bool, backendName string) []byte {
	if len(raw) == 0 {
		return raw
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
						if t == "" || t == "text" {
							if txt, ok := pm["text"].(string); ok {
								textParts = append(textParts, txt)
							}
						} else if t == "tool_use" {
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
func stripReasoningContent(raw []byte, strip bool) []byte {
	if !strip || len(raw) == 0 {
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
func capMaxTokens(raw []byte, limit int) []byte {
	if limit <= 0 || len(raw) == 0 {
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
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func copyRequestHeaders(dst *http.Request, src *http.Request) {
	for k, vals := range src.Header {
		if hopByHop[k] || k == "Authorization" || k == "Content-Length" || k == "Accept-Encoding" ||
			k == "Cookie" || k == "X-Api-Key" || k == "X-Csrf-Token" || connectionHeader(src.Header, k) {
			continue
		}
		for _, v := range vals {
			dst.Header.Add(k, v)
		}
	}
}

func connectionHeader(headers http.Header, name string) bool {
	for _, value := range headers.Values("Connection") {
		for _, nominated := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(nominated), name) {
				return true
			}
		}
	}
	return false
}

func copyResponseHeaders(w http.ResponseWriter, resp *http.Response, streaming bool) {
	for k, vals := range resp.Header {
		if hopByHop[k] || k == "Set-Cookie" || connectionHeader(resp.Header, k) {
			continue
		}
		if streaming && k == "Content-Length" {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	if streaming {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
	}
}

func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusPaymentRequired,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryAfterDuration(resp *http.Response) time.Duration {
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil && secs > 0 {
		return time.Duration(min(secs, 3600)) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return min(d, time.Hour)
		}
	}
	return 0
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	return "..." + k[len(k)-4:]
}

// randomUUID generates a version-4 UUID using crypto/rand. Used for
// x-opencode-request / x-opencode-session headers that Cloudflare expects.
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Upstream error bodies may echo credentials or prompts; only generic metadata
// may reach admin responses, request history, or logs.
func errorMessage(_ []byte) string { return "upstream rejected request" }

// handleMetrics exposes a live, depless JSON snapshot of all proxy activity.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	rt := s.runtime()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.metrics.Snapshot(rt.backends))
}

// usageOf supports Chat Completions, Responses, and native System One usage.
// Explicit zero usage is known; omitted token fields remain unknown.
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
type committedStreamWriter struct {
	http.ResponseWriter
	response     *http.Response
	requestID    string
	idle         time.Duration
	beforeCommit func() error
	committed    bool
}

func (w *committedStreamWriter) Write(p []byte) (int, error) {
	if !w.committed {
		if err := w.beforeCommit(); err != nil {
			return 0, err
		}
		copyResponseHeaders(w.ResponseWriter, w.response, true)
		w.Header().Set("X-Request-ID", w.requestID)
		w.ResponseWriter.WriteHeader(http.StatusOK)
		w.committed = true
	}
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.idle))
	return w.ResponseWriter.Write(p)
}

func (w *committedStreamWriter) FlushError() error {
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.idle))
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

type idleReader struct {
	body     io.ReadCloser
	timer    *time.Timer
	timeout  time.Duration
	timedOut atomic.Bool
}

func newIdleReader(body io.ReadCloser, timeout time.Duration, cancel context.CancelFunc) *idleReader {
	r := &idleReader{body: body, timeout: timeout}
	r.timer = time.AfterFunc(timeout, func() {
		r.timedOut.Store(true)
		cancel()
		_ = body.Close()
	})
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (r *idleReader) Close() error { r.timer.Stop(); return r.body.Close() }

type streamClientError struct{ err error }

func (e *streamClientError) Error() string { return "client stream write failed" }
func (e *streamClientError) Unwrap() error { return e.err }

// streamOptions tunes per-chunk work. The zero value is the correct default
// for a stripping backend: no reasoning alias, no accumulator.
type streamOptions struct {
	// aliasReasoning copies delta.reasoning onto reasoning_content so clients
	// that only read the DeepSeek field still see thinking. Only needed when
	// the backend does NOT strip reasoning; running it unconditionally cost a
	// full JSON decode per chunk for a field that was about to be discarded.
	aliasReasoning bool
	// acc collects reasoning + tool_call ids for the replay cache. nil when the
	// backend strips reasoning.
	acc *sseReasoningAcc
}

// usageMarker gates the per-chunk usage parse. Usage appears only in the final
// chunk of a stream, so scanning for the key first avoids a full JSON decode
// on every chunk. A false negative here only means the terminal usage chunk is
// missed, which is caught by the same parse on the final event anyway.
var usageMarker = []byte(`"usage"`)

// streamCopyWithUsage reports an interrupted stream unless a protocol terminal
// marker was observed. A 200 header alone is never evidence of completion.
//
// Flushing happens once per SSE event (at the blank-line boundary) rather than
// once per line: a standard `data: {...}\n\n` event is two lines, so per-line
// flushing doubled the write syscalls, the Flush calls and the
// SetWriteDeadline calls for every chunk.
func streamCopyWithUsage(w http.ResponseWriter, src io.Reader, onUsage func(in, out, cache uint64), onFirst func(), opts streamOptions) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), maxSSELineBytes)
	terminal, finished, failed := false, false, false
	flusher, _ := w.(http.Flusher)
	var eventBuf []byte
	flushEvent := func() error {
		if len(eventBuf) == 0 {
			return nil
		}
		if _, err := w.Write(eventBuf); err != nil {
			return &streamClientError{err}
		}
		eventBuf = eventBuf[:0]
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
		outLine := line
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
					switch event.Type {
					case "response.completed":
						terminal = true
					case "response.failed", "response.incomplete", "error":
						terminal, failed = true, true
					}
					if len(event.Error) > 0 && string(event.Error) != "null" {
						terminal, failed = true, true
					}
					for _, choice := range event.Choices {
						if choice.Finish != nil {
							finished = true
						}
					}
				}
				if onUsage != nil && bytes.Contains(data, usageMarker) {
					if in, out, cache, known := usageOf(data); known {
						onUsage(in, out, cache)
					}
				}
				if opts.acc != nil {
					opts.acc.ingest(data)
				}
				if opts.aliasReasoning {
					if rewritten, ok := aliasReasoningInChunk(data); ok {
						outLine = "data: " + string(rewritten)
					}
				}
			}
		}
		eventBuf = append(eventBuf, outLine...)
		eventBuf = append(eventBuf, '\n')
		if onFirst != nil && strings.HasPrefix(line, "data:") {
			onFirst()
			onFirst = nil
		}
		// An empty line terminates an SSE event: that is the only point where
		// the client needs the bytes, so it is the only point we flush.
		if line == "" {
			if err := flushEvent(); err != nil {
				return err
			}
		}
		if terminal && line == "" {
			if failed {
				return errors.New("upstream stream failed")
			}
			return nil
		}
	}
	// Trailing event without a blank-line terminator (truncated, or a provider
	// that omits it) must still reach the client.
	if err := flushEvent(); err != nil {
		return err
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if failed {
		return errors.New("upstream stream failed")
	}
	if !terminal && !finished {
		return io.ErrUnexpectedEOF
	}
	return nil
}
