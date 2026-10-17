package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const systemOneRequestFixture = `{"model":"typed","state":{"record_id":9007199254740993,"messages":[{"role":"developer","content":"private-state"}],"reasoning":{"preserve":true}},"questions":{"urgent":{"type":"noul","instructions":{"question":"Urgent?","context":["customer",{"priority":2}]}},"team":{"type":"choice","instructions":"Choose a team","criteria":{"billing":{"description":"Payments"},"technical":null}},"severity":{"type":"score","instructions":"Impact?","criteria":["Minor",{"description":"Major"}]} }}`
const systemOneResponseFixture = `{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.95},"team":{"type":"choice","choice":"billing","probabilities":{"billing":0.88,"technical":0.12},"confidence":0.81},"severity":{"type":"score","score":0.7,"legend":{"0":"Minor","1":{"description":"Major"}},"probabilities":{"0":0.3,"1":0.7},"confidence":0.4}},"usage":{"input_tokens":296,"output_tokens":20}}`

func systemOneJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		t.Error(err)
		return nil
	}
	return value
}

func systemOneCall(s *Server, path, key, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestSystemOneNativeContractsAndAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, apiFormat, basePath, upstream, requestPath, bodyModel string
		cloudflare                                                  bool
		inputPrice                                                  float64
	}{
		{name: "typesafe", apiFormat: "systemone", basePath: "/v1", upstream: "jev-latest", requestPath: "/v1/systemone", bodyModel: "jev-latest", inputPrice: 0.042},
		{name: "clef", basePath: "/client/v4/accounts/%ACCOUNT%/ai/v1", upstream: "@cf/cloudflare/clef", requestPath: "/client/v4/accounts/account-one/ai/run/@cf/cloudflare/clef", bodyModel: "clef", cloudflare: true, inputPrice: 0.24},
		{name: "clef-flash", basePath: "/client/v4/accounts/%ACCOUNT%/ai/v1", upstream: "@cf/cloudflare/clef-flash", requestPath: "/client/v4/accounts/account-one/ai/run/@cf/cloudflare/clef-flash", bodyModel: "clef-flash", cloudflare: true, inputPrice: 0.038},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := systemOneRequestFixture
			responseBody := systemOneResponseFixture
			if tc.cloudflare {
				body = strings.TrimSuffix(body, "}") + `,"images":[{"content_type":"image/png","base64":"iVBORw0KGgo="}]}`
				responseBody = strings.Replace(responseBody, "jev-1.13.0", tc.bodyModel, 1)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != tc.requestPath || r.Method != http.MethodPost {
					t.Errorf("native target %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer upstream-secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("native upstream authorization/content type missing")
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				got := systemOneJSON(t, raw)
				want := systemOneJSON(t, []byte(body))
				want["model"] = tc.bodyModel
				if !reflect.DeepEqual(got, want) {
					t.Error("native state/questions/images changed or chat fields injected")
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.cloudflare {
					io.WriteString(w, `{"success":true,"errors":[],"messages":[],"result":`+responseBody+`}`)
				} else {
					io.WriteString(w, responseBody)
				}
			}))
			defer upstream.Close()
			noStrip := false
			known := true
			cfg := &Config{DefaultProvider: "mistral", Providers: map[string]*ProviderConfig{"mistral": {
				BaseURL: upstream.URL + tc.basePath, APIFormat: tc.apiFormat, StripReasoningContent: &noStrip, MaxTokens: 1,
				Keys: []KeyEntry{{Key: "upstream-secret", AccountID: "account-one"}},
			}}, ModelMap: map[string]string{"typed": "mistral:" + tc.upstream}, ModelMeta: map[string]*ModelMeta{"typed": {
				ContextWindow: 65536, PricingInput: tc.inputPrice, PricingOutput: 0, PricingKnown: &known,
			}}}
			s := runtimeTestServer(t, cfg)
			e := newSecurityTestEnv(t)
			cookie, csrf := e.seedSession(t)
			key, view := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "typed-client", AllowedModels: []string{"typed"}, TPM: 200000, MaxConcurrent: 1})
			s.security = e.security
			w := systemOneCall(s, "/v1/systemone", key, body)
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("native status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
			}
			if !reflect.DeepEqual(systemOneJSON(t, w.Body.Bytes()), systemOneJSON(t, []byte(responseBody))) {
				t.Fatal("typed answers, distributions, or actual model ID were converted")
			}
			rec := runtimeRecord(t, s)
			if !rec.OK || rec.Stream || !rec.UsageKnown || !rec.CostKnown || rec.TokIn != 296 || rec.TokOut != 20 || rec.TokCache != 0 || rec.ClientKeyID != view.ID || rec.Upstream != tc.upstream || len(rec.AttemptLog) != 1 {
				t.Fatalf("native accounting=%+v", rec)
			}
			if math.Abs(rec.Cost-296*tc.inputPrice/1e6) > 1e-12 {
				t.Fatalf("native cost=%g", rec.Cost)
			}
			e.security.mu.Lock()
			usage := *e.security.clients[view.ID].usage
			e.security.mu.Unlock()
			if usage.active != 0 || usage.tokens != 316 {
				t.Fatalf("native client lease active=%d tokens=%d", usage.active, usage.tokens)
			}
			encoded, _ := json.Marshal(rec)
			if bytes.Contains(encoded, []byte("private-state")) || bytes.Contains(encoded, []byte("upstream-secret")) {
				t.Fatal("native inference leaked state or credentials into accounting")
			}
		})
	}
}

func TestSystemOneAuthorizationAndAliases(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, systemOneResponseFixture)
	}))
	defer upstream.Close()
	cfg := runtimeConfig(upstream.URL + "/v1")
	cfg.Providers["primary"].APIFormat = "systemone"
	cfg.ModelMap = map[string]string{"typed": "primary:jev-latest", "forbidden": "primary:jev-preview"}
	s := runtimeTestServer(t, cfg)
	e := newSecurityTestEnv(t)
	cookie, csrf := e.seedSession(t)
	key, _ := createSecurityTestKey(t, e, cookie, csrf, clientKeyRequest{Name: "scoped", AllowedModels: []string{"typed"}, RPM: 2})
	s.security = e.security
	for _, path := range []string{"/v1/systemone", "/systemone"} {
		if w := systemOneCall(s, path, "", systemOneRequestFixture); w.Code != 401 {
			t.Fatalf("anonymous native status=%d", w.Code)
		}
		forbidden := strings.Replace(systemOneRequestFixture, `"model":"typed"`, `"model":"forbidden"`, 1)
		if w := systemOneCall(s, path, key, forbidden); w.Code != 403 {
			t.Fatalf("out-of-scope native status=%d", w.Code)
		}
		if w := systemOneCall(s, path, key, systemOneRequestFixture); w.Code != 200 {
			t.Fatalf("authenticated native alias status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if w := systemOneCall(s, "/systemone", key, systemOneRequestFixture); w.Code != 429 {
		t.Fatalf("native RPM policy status=%d", w.Code)
	}
	if calls.Load() != 2 {
		t.Fatalf("unauthorized/policy-rejected requests forwarded: calls=%d", calls.Load())
	}
}

func TestSystemOneEndpointCompatibility(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, systemOneResponseFixture)
	}))
	defer upstream.Close()
	for _, tc := range []struct {
		name, format, basePath, model, endpoint, extra string
	}{
		{name: "chat to typesafe", format: "systemone", model: "jev-latest", endpoint: "/chat/completions"},
		{name: "responses to typesafe", format: "systemone", model: "jev-latest", endpoint: "/responses"},
		{name: "chat to clef", basePath: "/accounts/a/ai/v1", model: "@cf/cloudflare/clef", endpoint: "/chat/completions"},
		{name: "responses to clef-flash", basePath: "/accounts/a/ai/v1", model: "@cf/cloudflare/clef-flash", endpoint: "/responses"},
		{name: "systemone to cloudflare chat", basePath: "/accounts/a/ai/v1", model: "@cf/meta/llama-3.1-8b-instruct", endpoint: "/systemone"},
		{name: "systemone to ordinary chat", model: "chat-model", endpoint: "/systemone"},
		{name: "native stream rejected", format: "systemone", model: "jev-latest", endpoint: "/systemone", extra: `,"stream":true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := runtimeConfig(upstream.URL + tc.basePath)
			cfg.Providers["primary"].APIFormat = tc.format
			cfg.ModelMap["test"] = "primary:" + tc.model
			s := runtimeTestServer(t, cfg)
			w := httptest.NewRecorder()
			body := `{"model":"test","state":"private-state","questions":{}` + tc.extra + `}`
			s.forward(w, httptest.NewRequest(http.MethodPost, "/v1"+tc.endpoint, strings.NewReader(body)), tc.endpoint)
			if w.Code != 400 || len(runtimeRecord(t, s).AttemptLog) != 0 {
				t.Fatalf("incompatible request status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("incompatible inference reached upstream")
	}
	// Cloudflare's ordinary models retain their existing OpenAI chat endpoint.
	cfg := runtimeConfig(upstream.URL + "/accounts/a/ai/v1")
	cfg.ModelMap["test"] = "primary:@cf/meta/llama-3.1-8b-instruct"
	if w := runtimeCall(runtimeTestServer(t, cfg), `{"model":"test","messages":[]}`); w.Code != 200 || calls.Load() != 1 {
		t.Fatal("native integration disabled Cloudflare chat models")
	}
}

func TestSystemOneFailoverBeforeFlushAndPerKeyAccounts(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		status                int
		response              string
		wantCalls, wantStatus int
	}{
		{name: "rate limit", status: 429, response: `{"error":"quota"}`, wantCalls: 2, wantStatus: 200},
		{name: "native envelope error", status: 200, response: `{"success":false,"errors":[{"message":"private-error"}],"result":null}`, wantCalls: 2, wantStatus: 200},
		{name: "truncated JSON", status: 200, response: `{"success":true,"result":`, wantCalls: 2, wantStatus: 200},
		{name: "chat shaped response", status: 200, response: `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}`, wantCalls: 2, wantStatus: 200},
		{name: "client error stops", status: 400, response: `{"error":"invalid typed question private-state"}`, wantCalls: 1, wantStatus: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			accounts := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				accounts <- r.URL.Path
				if n == 1 {
					w.Header().Set("X-Rejected-Upstream", "must-not-flush")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.response)
					return
				}
				io.WriteString(w, `{"success":true,"result":`+systemOneResponseFixture+`}`)
			}))
			defer upstream.Close()
			cfg := runtimeConfig(upstream.URL + "/accounts/%ACCOUNT%/ai/v1")
			cfg.ModelMap["test"] = "primary:@cf/cloudflare/clef"
			cfg.Providers["primary"].Keys = []KeyEntry{{Key: "credential-a", AccountID: "account-a"}, {Key: "credential-b", AccountID: "account-b"}}
			cfg.Limits.MaxAttempts = 2
			s := runtimeTestServer(t, cfg)
			w := httptest.NewRecorder()
			s.handleSystemOne(w, httptest.NewRequest(http.MethodPost, "/systemone", strings.NewReader(`{"model":"test","state":"private-state","questions":{"q":{"type":"noul","instructions":"Question?"}}}`)))
			if w.Code != tc.wantStatus || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("native retry status=%d calls=%d", w.Code, calls.Load())
			}
			if w.Header().Get("X-Rejected-Upstream") != "" || strings.Contains(w.Body.String(), "private-") {
				t.Fatal("failed native response flushed before failover")
			}
			rec := runtimeRecord(t, s)
			if len(rec.AttemptLog) != tc.wantCalls || rec.Failover != tc.wantCalls-1 {
				t.Fatalf("native attempts=%+v", rec.AttemptLog)
			}
			if tc.wantCalls == 2 {
				firstAccount, secondAccount := <-accounts, <-accounts
				if firstAccount == secondAccount || !strings.Contains(firstAccount, "/ai/run/@cf/cloudflare/clef") || !strings.Contains(secondAccount, "/ai/run/@cf/cloudflare/clef") {
					t.Fatalf("native accounts not substituted per credential: %s, %s", firstAccount, secondAccount)
				}
				if !rec.OK || !rec.UsageKnown || rec.CostKnown || rec.TokIn != 296 || rec.TokOut != 20 {
					t.Fatalf("fallback native usage/cost=%+v", rec)
				}
			}
		})
	}
}

func TestSystemOneFallbackCompatibilityAndAttemptLimit(t *testing.T) {
	var nativeCalls, chatCalls atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		w.WriteHeader(503)
	}))
	defer native.Close()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer chat.Close()
	for _, limit := range []int{1, 2} {
		cfg := runtimeConfig(native.URL)
		cfg.Providers["primary"].APIFormat = "systemone"
		cfg.Providers["primary"].Keys = []KeyEntry{{Key: "a"}, {Key: "b"}, {Key: "c"}}
		cfg.Providers["chat"] = &ProviderConfig{BaseURL: chat.URL, NoAuth: true}
		cfg.ModelMap["test"] = "primary:jev-latest"
		cfg.ModelRoutes = map[string][]RouteTarget{"test": {{Backend: "chat", Upstream: "chat-model"}}}
		cfg.Limits.MaxAttempts = limit
		s := runtimeTestServer(t, cfg)
		w := httptest.NewRecorder()
		s.handleSystemOne(w, httptest.NewRequest(http.MethodPost, "/systemone", strings.NewReader(`{"model":"test","state":[],"questions":{}}`)))
		if w.Code != 503 || len(runtimeRecord(t, s).AttemptLog) != limit {
			t.Fatalf("native attempt limit=%d status=%d", limit, w.Code)
		}
	}
	if nativeCalls.Load() != 3 || chatCalls.Load() != 0 {
		t.Fatalf("native attempts=%d incompatible fallback attempts=%d", nativeCalls.Load(), chatCalls.Load())
	}
}

func TestSystemOneUnknownUsageRemainsUnknown(t *testing.T) {
	for _, usage := range []string{"", `,"usage":{"input_tokens":12}`, `,"usage":{"input_tokens":0,"output_tokens":0}`} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.5}}`+usage+`}`)
		}))
		cfg := runtimeConfig(upstream.URL)
		cfg.Providers["primary"].APIFormat = "systemone"
		cfg.ModelMeta = map[string]*ModelMeta{"test": {PricingInput: 0.042}}
		s := runtimeTestServer(t, cfg)
		w := httptest.NewRecorder()
		s.handleSystemOne(w, httptest.NewRequest(http.MethodPost, "/systemone", strings.NewReader(`{"model":"test","state":"state","questions":{}}`)))
		upstream.Close()
		rec := runtimeRecord(t, s)
		known := strings.Contains(usage, "output_tokens")
		if w.Code != 200 || !rec.OK || rec.UsageKnown != known || rec.CostKnown != known {
			t.Fatalf("native usage=%s record=%+v", usage, rec)
		}
	}
}

func TestSystemOneProviderFormatPersistsAndValidates(t *testing.T) {
	store, _, _ := controlTestStore(t)
	cfg := controlTestConfig()
	cfg.Providers["primary"].APIFormat = "systemone"
	loaded, err := store.LoadOrImportConfig(cfg)
	if err != nil || loaded.Providers["primary"].APIFormat != "systemone" {
		t.Fatalf("native provider import failed: %v", err)
	}
	clone, err := cloneConfig(loaded)
	if err != nil || clone.Providers["primary"].APIFormat != "systemone" {
		t.Fatalf("native provider clone failed: %v", err)
	}
	if view := (&Server{}).providerView("primary", clone.Providers["primary"], clone); view.APIFormat != "systemone" {
		t.Fatal("native provider format missing from management response")
	}
	req := providerUpsertReq{Name: "primary", BaseURL: "https://api.typesafe.ai/v1", APIFormat: "systemone"}
	if err := applyProviderRequest(clone, req); err != nil || clone.Providers["primary"].APIFormat != "systemone" {
		t.Fatalf("native provider management assignment failed: %v", err)
	}
	if providerFingerprint(clone.Providers["primary"]) == providerFingerprint(&ProviderConfig{BaseURL: req.BaseURL, Keys: clone.Providers["primary"].Keys}) {
		t.Fatal("native format change did not invalidate backend fingerprint")
	}
	if err := store.SaveConfig(clone); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.LoadOrImportConfig(&Config{})
	if err != nil || reloaded.Providers["primary"].APIFormat != "systemone" {
		t.Fatalf("native provider persistence failed: %v", err)
	}
	req.APIFormat = "invalid"
	if err := applyProviderRequest(clone, req); err == nil {
		t.Fatal("management accepted invalid native API format")
	}
	clone.Providers["primary"].APIFormat = "invalid"
	if _, err := clone.resolveBackends(); err == nil {
		t.Fatal("runtime accepted invalid native API format")
	}
}

func TestSystemOneCrossProviderFallbackUsesNativeContractAndPrice(t *testing.T) {
	var chatCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("TypeSafe fallback primary path=%s", r.URL.Path)
		}
		w.Header().Set("X-Failed-Primary", "must-not-flush")
		w.WriteHeader(503)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/fallback-account/ai/run/@cf/cloudflare/clef" {
			t.Errorf("native fallback path=%s", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		got := systemOneJSON(t, raw)
		want := systemOneJSON(t, []byte(systemOneRequestFixture))
		want["model"] = "clef"
		if !reflect.DeepEqual(got, want) {
			t.Error("cross-provider fallback changed native structured input")
		}
		io.WriteString(w, `{"success":true,"result":`+strings.Replace(systemOneResponseFixture, "jev-1.13.0", "clef", 1)+`}`)
	}))
	defer fallback.Close()
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer chat.Close()
	cfg := runtimeConfig(primary.URL + "/v1")
	cfg.Providers["primary"].APIFormat = "systemone"
	cfg.Providers["chat"] = &ProviderConfig{BaseURL: chat.URL, NoAuth: true}
	cfg.Providers["cf"] = &ProviderConfig{BaseURL: fallback.URL + "/accounts/%ACCOUNT%/ai/v1", Keys: []KeyEntry{{Key: "cf-key", AccountID: "fallback-account"}}}
	cfg.ModelMap = map[string]string{"typed": "primary:jev-latest", "clef-price": "cf:@cf/cloudflare/clef"}
	cfg.ModelMeta = map[string]*ModelMeta{"typed": {PricingInput: 0.042}, "clef-price": {PricingInput: 0.24}}
	cfg.ModelRoutes = map[string][]RouteTarget{"typed": {{Backend: "chat", Upstream: "chat-only"}, {Backend: "cf", Upstream: "@cf/cloudflare/clef"}}}
	cfg.Limits.MaxAttempts = 2
	s := runtimeTestServer(t, cfg)
	w := httptest.NewRecorder()
	s.handleSystemOne(w, httptest.NewRequest(http.MethodPost, "/systemone", strings.NewReader(systemOneRequestFixture)))
	rec := runtimeRecord(t, s)
	if w.Code != 200 || !rec.OK || rec.Backend != "cf" || len(rec.AttemptLog) != 2 || chatCalls.Load() != 0 || !rec.CostKnown || math.Abs(rec.Cost-296*0.24/1e6) > 1e-12 {
		t.Fatalf("native cross-provider failover status=%d record=%+v chat=%d", w.Code, rec, chatCalls.Load())
	}
	if w.Header().Get("X-Failed-Primary") != "" {
		t.Fatal("primary headers flushed before native fallback")
	}
}

func TestSystemOneDoesNotRewriteCloudflareChatResponses(t *testing.T) {
	const response = `{"success":true,"result":{"response":"ordinary chat result"},"errors":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/chat-account/ai/v1/chat/completions" {
			t.Errorf("Cloudflare chat path=%s", r.URL.Path)
		}
		io.WriteString(w, response)
	}))
	defer upstream.Close()
	cfg := runtimeConfig(upstream.URL + "/accounts/%ACCOUNT%/ai/v1")
	cfg.Providers["primary"].Keys[0].AccountID = "chat-account"
	cfg.ModelMap["test"] = "primary:@cf/meta/llama-3.1-8b-instruct"
	s := runtimeTestServer(t, cfg)
	w := runtimeCall(s, `{"model":"test","messages":[{"role":"user","content":"chat input"}]}`)
	if w.Code != 200 || w.Body.String() != response {
		t.Fatalf("Cloudflare chat response rewritten: %d %s", w.Code, w.Body.String())
	}
}
