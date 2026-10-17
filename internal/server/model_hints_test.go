package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func catalogFixture(t *testing.T, body, source string) catalogIndex {
	t.Helper()
	entries, err := decodeModelList([]byte(body), source)
	if err != nil {
		t.Fatal(err)
	}
	return indexCatalog(entries)
}

func TestHintPriceAvailability(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
		known bool
	}{
		{`"0.00000012"`, 0.12, true},
		{`0.00000012`, 0.12, true},
		{`"0"`, 0, true},
		{`0`, 0, true},
		{`null`, 0, false},
		{`""`, 0, false},
		{`"NaN"`, 0, false},
		{`"Inf"`, 0, false},
		{`-1`, 0, false},
		{`[{"price":1}]`, 0, false},
		{`{"tier":1}`, 0, false},
		{`true`, 0, false},
		{`1e308`, 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			m := map[string]json.RawMessage{"input": json.RawMessage(tc.value)}
			got, known := priceField(m, 1e6, "input")
			if known != tc.known || got != tc.want {
				t.Fatalf("price=(%v,%v), want (%v,%v)", got, known, tc.want, tc.known)
			}
		})
	}
	if _, known := priceField(nil, 1e6, "input"); known {
		t.Fatal("absent price marked known")
	}
}

func TestHintIdentityMatching(t *testing.T) {
	idx := catalogFixture(t, `{"data":[
		{"id":"owner/widget","name":"Paid","pricing":{"input":"0.000002","output":"0.000004"}},
		{"id":"owner/widget:free","name":"Free","pricing":{"input":"0","output":"0"}},
		{"id":"owner/unique","name":"Unique"},
		{"id":"one/dupe","name":"One"},
		{"id":"two/dupe","name":"Two","pricing":{"input":"0.000001"}}
	]}`, "fixture")
	for _, tc := range []struct{ id, name string }{
		{" OWNER/Widget ", "Paid"},
		{"owner/widget:free", "Free"},
		{"widget:free", "Free"},
		{"unique", "Unique"},
		{"dupe", ""},
		{"other/dupe", ""},
		{"other/unique", ""},
		{"owner/unique:free", ""},
		{"owner/widget-free", ""},
	} {
		t.Run(tc.id, func(t *testing.T) {
			e := idx.lookup(tc.id)
			if tc.name == "" {
				if e != nil {
					t.Fatalf("unsafe match %+v", e)
				}
				return
			}
			if e == nil || e.Name != tc.name {
				t.Fatalf("lookup=%+v, want %s", e, tc.name)
			}
		})
	}
	other := catalogFixture(t, `[{"id":"other/unique","name":"Unrelated"}]`, "other")
	if h := mergeHint("unique", []catalogIndex{idx, other}); h != nil {
		t.Fatalf("cross-catalog ambiguity selected %+v", h)
	}
}

func TestMergeHintPreservesFreeAndPartialPrices(t *testing.T) {
	first := catalogFixture(t, `{"data":[
		{"id":"owner/free:free","name":"Free","pricing":{"input":"0","output":"0","input_cache_read":"0"},"context_window":32000},
		{"id":"owner/partial","pricing":{"input":"0"},"context_window":64000},
		{"id":"owner/unknown","name":"Unknown"}
	]}`, "primary")
	second := catalogFixture(t, `{"data":[
		{"id":"owner/free:free","pricing":{"input":"0.000001","output":"0.000002","input_cache_read":"0.0000005"},"max_tokens":4096},
		{"id":"owner/partial","pricing":{"input":"0.000003","output":"0.000004","input_cache_read":"0.0000005"},"max_tokens":8192}
	]}`, "secondary")
	h := mergeHint("owner/free:free", []catalogIndex{first, second})
	if h == nil || !h.PricingKnown || h.PricingInput != 0 || h.PricingOutput != 0 || h.PricingCache != 0 || h.MaxTokens != 4096 || h.Source != "primary" {
		t.Fatalf("free overwritten: %+v", h)
	}
	h = mergeHint("owner/partial", []catalogIndex{first, second})
	if h == nil || !h.PricingKnown || h.PricingInput != 0 || h.PricingOutput != 4 || h.PricingCache != 0.5 || h.ContextWindow != 64000 || h.MaxTokens != 8192 {
		t.Fatalf("partial merge: %+v", h)
	}
	h = mergeHint("owner/unknown", []catalogIndex{first, second})
	if h == nil || h.PricingKnown || h.inputKnown || h.outputKnown {
		t.Fatalf("unknown marked free: %+v", h)
	}
	if mergeHint("missing", []catalogIndex{first, second}) != nil {
		t.Fatal("unknown identity received a hint")
	}
}

func TestHintJSONPreservesPricePresence(t *testing.T) {
	for _, body := range []string{
		`{"id":"owner/free","name":"Free","pricing":{"input":"0","output":"0"}}`,
		`{"id":"owner/partial","name":"Partial","pricing":{"input":"0"}}`,
		`{"id":"owner/unknown","name":"Unknown"}`,
	} {
		h := hintForDiscoveredModel(json.RawMessage(body), "upstream")
		if h == nil {
			t.Fatal("missing native hint")
		}
		encoded, err := json.Marshal(h)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		for _, field := range []struct {
			name  string
			known bool
		}{{"pricing_input", h.inputKnown}, {"pricing_output", h.outputKnown}, {"pricing_cache", h.cacheKnown}} {
			if _, present := fields[field.name]; present != field.known {
				t.Fatalf("%s presence wrong in %s", field.name, encoded)
			}
		}
		if _, present := fields["pricing_known"]; !present {
			t.Fatalf("missing pricing_known: %s", encoded)
		}
		var restored modelHint
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.inputKnown != h.inputKnown || restored.outputKnown != h.outputKnown || restored.PricingKnown != h.PricingKnown || restored.Name != h.Name {
			t.Fatalf("round trip lost metadata: %s -> %+v", encoded, restored)
		}
	}
}

func TestFetchVercelPublicAndAuthenticated(t *testing.T) {
	for _, key := range []string{"", "test-key"} {
		t.Run(key, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("path=%s", r.URL.Path)
				}
				want := ""
				if key != "" {
					want = "Bearer " + key
				}
				if r.Header.Get("Authorization") != want {
					t.Errorf("unexpected authorization header")
				}
				_, _ = w.Write([]byte(`{"data":[
					{"id":"owner/widget","name":"Widget","description":"Native description","owned_by":"owner","context_window":"32000","max_tokens":4096,"pricing":{"input":0.0000005,"output":"0.000002","input_cache_read":"0"},"supported_parameters":["tools","structured_outputs"],"modalities":{"input":["text","image"],"output":["text"]}},
					{"id":3},{"id":"owner/odd","context_window":{},"pricing":{"input":[]}}
				]}`))
			}))
			defer srv.Close()
			entries, err := fetchVercelModels(context.Background(), srv.URL+"/v1", key, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 2 {
				t.Fatalf("bad row poisoned catalog: %+v", entries)
			}
			h := mergeHint("owner/widget", []catalogIndex{indexCatalog(entries)})
			if h == nil || !h.PricingKnown || h.PricingInput != 0.5 || h.PricingOutput != 2 || !h.cacheKnown || h.PricingCache != 0 || h.ContextWindow != 32000 || h.MaxTokens != 4096 || h.Description != "Native description" || h.OwnedBy != "owner" {
				t.Fatalf("vercel hint: %+v", h)
			}
			for _, tag := range []string{"vision", "tool-use", "structured-output"} {
				if !slices.Contains(h.Tags, tag) {
					t.Fatalf("missing %s: %+v", tag, h.Tags)
				}
			}
		})
	}
}

func TestFetchOpenRouterPreservesOutputAndUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"owner/widget:free","name":"Free Widget","context_length":128000,"pricing":{"prompt":"0","completion":"0"},"top_provider":{"context_length":64000,"max_completion_tokens":8192}},
			{"id":"owner/widget","context_length":128000,"pricing":{"prompt":"0.0000005","completion":"0.0000015","input_cache_read":"0.00000005"},"top_provider":[{"max_completion_tokens":4096}]},
			{"id":"owner/unknown","pricing":{"prompt":null,"completion":-1},"top_provider":{"max_completion_tokens":4096}}
		]}`))
	}))
	defer srv.Close()
	entries, err := fetchOpenRouter(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	idx := indexCatalog(entries)
	free := mergeHint("owner/widget:free", []catalogIndex{idx})
	if free == nil || !free.PricingKnown || free.PricingInput != 0 || free.MaxTokens != 8192 || free.ContextWindow != 128000 {
		t.Fatalf("free entry: %+v", free)
	}
	paid := mergeHint("owner/widget", []catalogIndex{idx})
	if paid == nil || !paid.PricingKnown || paid.PricingInput != 0.5 || paid.PricingOutput != 1.5 || paid.MaxTokens != 0 || math.Abs(paid.PricingCache-0.05) > 1e-9 {
		t.Fatalf("paid entry: %+v", paid)
	}
	unknown := mergeHint("owner/unknown", []catalogIndex{idx})
	if unknown == nil || unknown.PricingKnown || unknown.inputKnown || unknown.outputKnown || unknown.MaxTokens != 4096 {
		t.Fatalf("unknown pricing: %+v", unknown)
	}
}

func TestModelsDevEndpointShapes(t *testing.T) {
	model := `{"name":"Widget","description":"Metadata","reasoning":true,"tool_call":true,"limit":{"context":"131072","input":100000,"output":8192},"cost":{"input":"0.5","output":1.5,"cache_read":0}}`
	for _, body := range []string{
		`{"provider":{"models":{"owner/widget":` + model + `}}}`,
		`{"providers":{"provider":{"models":{"owner/widget":` + model + `}}},"models":{"owner/widget":{"description":"Metadata","limit":{"context":131072,"output":8192}}}}`,
		`{"owner/widget":` + model + `}`,
	} {
		entries, err := decodeModelsDev([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		h := mergeHint("owner/widget", []catalogIndex{indexCatalog(entries)})
		if h == nil || h.Name != "Widget" || !h.PricingKnown || h.PricingInput != 0.5 || h.PricingOutput != 1.5 || !h.cacheKnown || h.PricingCache != 0 || h.ContextWindow != 131072 || h.MaxTokens != 8192 {
			t.Fatalf("models.dev shape: %+v", h)
		}
	}
	entries, err := decodeModelsDev([]byte(`{"owner/widget":{"id":"owner/widget","name":"Widget","limit":{"context":64000,"output":4096},"cost":{"input":[{"tier":1}],"output":{"complex":true}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	h := mergeHint("owner/widget", []catalogIndex{indexCatalog(entries)})
	if h == nil || h.PricingKnown || h.inputKnown || h.outputKnown || h.ContextWindow != 64000 {
		t.Fatalf("tiered prices marked free: %+v", h)
	}
}

func TestModelsDevProviderConflictsAreNotRandomPrices(t *testing.T) {
	entries, err := decodeModelsDev([]byte(`{
		"a":{"models":{"widget":{"id":"owner/widget","name":"Widget","limit":{"context":64000,"output":4096},"cost":{"input":0.5,"output":2}}}},
		"b":{"models":{"widget":{"id":"owner/widget","name":"Widget","limit":{"context":128000,"output":4096},"cost":{"input":0,"output":2}}}},
		"c":{"models":{"widget":{"id":"owner/widget","name":"Widget","cost":{"output":null}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, reversed := range []bool{false, true} {
		if reversed {
			slices.Reverse(entries)
		}
		h := mergeHint("owner/widget", []catalogIndex{indexCatalog(entries)})
		if h == nil || h.inputKnown || h.PricingKnown || !h.outputKnown || h.PricingOutput != 2 || h.ContextWindow != 0 || h.MaxTokens != 4096 || h.Name != "Widget" {
			t.Fatalf("provider ambiguity leaked: %+v", h)
		}
	}
}

func TestNativeDiscoveredHintMergesEachField(t *testing.T) {
	native := hintForDiscoveredModel(json.RawMessage(`{"id":"owner/widget","name":"Native","description":"Authoritative","owned_by":"owner","context_window":262144,"pricing":{"input":"0","output":null},"cost":{"cache_read":0},"pricing_output":3,"tags":["reasoning"],"modalities":{"input":["text","image"],"output":["text"]}}`), "upstream")
	catalog := mergeHint("owner/widget", []catalogIndex{catalogFixture(t, `[{"id":"owner/widget","name":"Catalog","description":"Other","max_tokens":16384,"pricing":{"input":"0.000001","output":"0.000004","input_cache_read":"0.0000005"}}]`, "catalog")})
	h := mergeDiscoveredModelHint(native, catalog)
	if h == nil || h.Name != "Native" || h.Description != "Authoritative" || h.OwnedBy != "owner" || !h.PricingKnown || h.PricingInput != 0 || h.PricingOutput != 3 || h.PricingCache != 0 || h.ContextWindow != 262144 || h.MaxTokens != 16384 || !slices.Contains(h.Tags, "vision") {
		t.Fatalf("native metadata discarded: %+v", h)
	}
	named := hintForDiscoveredModel(json.RawMessage(`{"name":"named-model","description":"Named identity","owner":"owner","limit":{"context":64000},"cost":{"input":0.125,"output":0}}`), "upstream")
	if named == nil || named.Name != "named-model" || named.Description != "Named identity" || named.OwnedBy != "owner" || !named.PricingKnown || named.PricingInput != 0.125 || named.PricingOutput != 0 || named.ContextWindow != 64000 {
		t.Fatalf("name-based discovery metadata discarded: %+v", named)
	}
}

func TestFetchLiteLLMLimitDistinction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"owner/widget":{"max_tokens":128000,"max_output_tokens":16384,"max_input_tokens":262144,"input_cost_per_token":0.0000005,"output_cost_per_token":0.000002,"input_cost_per_token_cache_hit":0},
			"owner/unknown":{"max_tokens":null,"max_input_tokens":null,"input_cost_per_token":null,"output_cost_per_token":null}
		}`))
	}))
	defer srv.Close()
	entries, err := fetchLiteLLM(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := mergeHint("owner/widget", []catalogIndex{indexCatalog(entries)})
	if h == nil || !h.PricingKnown || h.PricingInput != 0.5 || h.PricingOutput != 2 || !h.cacheKnown || h.PricingCache != 0 || h.ContextWindow != 262144 || h.MaxTokens != 16384 {
		t.Fatalf("litellm conversion: %+v", h)
	}
}

func TestHintResolverPublicCatalogAndFailureIsolation(t *testing.T) {
	var vercelCalls atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if r.Header.Get("Authorization") != "" {
				t.Error("public fetch sent authorization")
			}
			if vercelCalls.Add(1) > 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"owner/widget","name":"Widget","pricing":{"input":"0","output":"0"}}]}`))
		case "/md":
			_, _ = w.Write([]byte(`{"owner/widget":{"name":"Widget","limit":{"context":64000,"output":4096}}}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer fixture.Close()
	h := newHintResolver()
	h.vercelURL, h.modelsDevURL = fixture.URL+"/v1", fixture.URL+"/md"
	h.openRouterURL, h.litellmURL = fixture.URL+"/fail", fixture.URL+"/fail"
	for _, stale := range []bool{false, true} {
		if stale {
			h.vercel.at = time.Time{}
		}
		hints := h.hintsFor(context.Background(), &Config{}, []string{"owner/widget"})
		got := hints["owner/widget"]
		if got == nil || !got.PricingKnown || got.PricingInput != 0 || got.PricingOutput != 0 || got.MaxTokens != 4096 || got.ContextWindow != 64000 {
			t.Fatalf("isolated source failure lost cached hints: %+v", got)
		}
	}
	// The stale refresh now runs in the background (cache-first serving):
	// the second fetch must still happen, just not on the caller's path.
	for deadline := time.Now().Add(5 * time.Second); vercelCalls.Load() != 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if vercelCalls.Load() != 2 {
		t.Fatalf("vercel calls=%d", vercelCalls.Load())
	}
}

func TestCatalogPagination(t *testing.T) {
	for _, source := range []string{"openrouter", "vercel"} {
		t.Run(source, func(t *testing.T) {
			var calls atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if source == "vercel" && r.Header.Get("X-Catalog-Key") != "test-key" {
					t.Error("paginated credential missing")
				}
				if r.URL.Query().Get("page") == "2" {
					_, _ = w.Write([]byte(`{"data":[{"id":"owner/second","pricing":{"prompt":"0","completion":"0"}}],"links":{"next":null}}`))
				} else {
					_, _ = w.Write([]byte(`{"data":[{"id":"owner/first","name":"First"},{"id":5}],"links":{"next":"?page=2"},"total_count":3}`))
				}
			}))
			defer fixture.Close()
			var entries []catalogEntry
			var err error
			if source == "vercel" {
				entries, err = fetchVercelModels(context.Background(), fixture.URL+"/v1", "test-key", "X-Catalog-Key", "%s")
			} else {
				entries, err = fetchOpenRouter(context.Background(), fixture.URL+"/models")
			}
			if err != nil {
				t.Fatal(err)
			}
			h := mergeHint("owner/second", []catalogIndex{indexCatalog(entries)})
			if calls.Load() != 2 || len(entries) != 2 || h == nil || !h.PricingKnown || h.PricingInput != 0 || h.PricingOutput != 0 {
				t.Fatalf("second page discarded: calls=%d entries=%+v hint=%+v", calls.Load(), entries, h)
			}
		})
	}
}

func TestCatalogPaginationRejectsIncompleteOrUnsafePages(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"missing", `{"data":[{"id":"owner/one"}],"total_count":2,"links":{"next":null}}`},
		{"foreign", `{"data":[],"links":{"next":"https://foreign.invalid/models"}}`},
		{"cycle", `{"data":[],"links":{"next":"/models"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer fixture.Close()
			if _, err := fetchOpenRouter(context.Background(), fixture.URL+"/models"); err == nil {
				t.Fatal("incomplete/unsafe pagination accepted")
			}
			if calls.Load() != 1 {
				t.Fatalf("unsafe/repeated page requested %d times", calls.Load())
			}
		})
	}
}

func TestAdminDiscoverAttachesHints(t *testing.T) {
	vercelFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"vv/widget-v1","name":"Widget V1","context_window":64000,"max_tokens":4096,"pricing":{"input":"0.00000100","output":"0.00000200"}}]}`))
	}))
	defer vercelFixture.Close()
	mdFixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"mdprov":{"models":{"gadget-v2":{"id":"md/gadget-v2","name":"Gadget V2","limit":{"context":32000,"output":2048},"cost":{"input":0.7,"output":2.1,"cache_read":0.07}}}}}`))
	}))
	defer mdFixture.Close()

	cfg := &Config{
		Listen:          ":0",
		DefaultProvider: "mock",
		ModelMap:        map[string]string{"mock/a": "mock:a"},
		Providers: map[string]*ProviderConfig{
			"mock":   {BaseURL: "http://127.0.0.1:9/v1", Keys: []KeyEntry{{Key: "sk-test"}}},
			"vercel": {BaseURL: vercelFixture.URL + "/v1", Keys: []KeyEntry{{Key: "vk-test"}}},
		},
	}
	cfg.applyDefaults()
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.hints.modelsDevURL = mdFixture.URL
	srv.hints.openRouterURL = "http://127.0.0.1:9/none"
	srv.hints.litellmURL = "http://127.0.0.1:9/none"

	up := mockUpstream(t)
	defer up.Close()
	body, _ := json.Marshal(map[string]any{"base_url": up.URL + "/v1", "allow_private_upstreams": true, "api_key": "good-key"})
	req := httptest.NewRequest(http.MethodPost, "/dash/api/admin/discover", bytes.NewReader(body))
	authorizeAdminTest(t, srv, req)
	rec := httptest.NewRecorder()
	srv.handleAdminDiscover(rec, req)
	if rec.Code != 200 {
		t.Fatalf("discover: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []discoveredModel `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]discoveredModel{}
	for _, m := range out.Models {
		byID[m.ID] = m
	}
	widget := byID["widget-v1"]
	if widget.Hint == nil || widget.Hint.Source != "vercel" {
		t.Fatalf("widget-v1 hint=%+v, want vercel tail hint", widget.Hint)
	}
	if widget.Hint.PricingInput != 1.0 || widget.Hint.PricingOutput != 2.0 || widget.Hint.ContextWindow != 64000 {
		t.Fatalf("widget-v1 bad hint values: %+v", widget.Hint)
	}
	gadget := byID["gadget-v2"]
	if gadget.Hint == nil || gadget.Hint.Source != "models.dev" {
		t.Fatalf("gadget-v2 hint=%+v, want models.dev hint", gadget.Hint)
	}
	if gadget.Hint.PricingInput != 0.7 || gadget.Hint.MaxTokens != 2048 {
		t.Fatalf("gadget-v2 bad hint values: %+v", gadget.Hint)
	}
}
