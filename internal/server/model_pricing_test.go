package server

import (
	"math"
	"testing"
)

func TestModelPricingCacheDiscountRequiresExplicitRate(t *testing.T) {
	input, output, freeCache, discountedCache := 1.0, 2.0, 0.0, 0.25
	for _, tc := range []struct {
		name  string
		cache *float64
		want  float64
	}{
		{name: "unpublished discount bills input rate", want: 0.0012},
		{name: "explicit free cache", cache: &freeCache, want: 0.0004},
		{name: "published discount", cache: &discountedCache, want: 0.0006},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Providers: map[string]*ProviderConfig{"upstream": {BaseURL: "http://127.0.0.1/v1", NoAuth: true}}}
			cfg.applyDefaults()
			err := applyModelRequest(cfg, modelUpsertReq{
				ID: "priced", Backend: "upstream", Upstream: "priced-model",
				PricingInput: &input, PricingOutput: &output, PricingCache: tc.cache,
			}, false)
			if err != nil {
				t.Fatal(err)
			}
			price := newRuntimeState(cfg, nil).modelPrices["priced"]
			if got := price.cost(1000, 100, 800); !price.known || math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("cached request cost=%g known=%v want=%g", got, price.known, tc.want)
			}
		})
	}
}
