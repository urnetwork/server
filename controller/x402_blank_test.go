package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
)

// pushBlankX402Config stands in for a vault/<env>/x402.yml that someone flipped to
// enabled: true before filling in the keys -- the exact half-configured state the stub
// warns about.
func pushBlankX402Config() func() {
	return server.Vault.PushSimpleResource("x402.yml", []byte(`
enabled: true

facilitator:
  url: ""
  api_key: ""

pay_to: ""

networks:
  - base

asset: usdc

skus:
  pro_1month: {}
  data_1tib: {}
  data_10tib: {}

max_payment_usd: 100.00

receipt:
  enabled: true
`))
}

// TestX402BlankConfigNoops proves that a half-filled x402.yml -- `enabled: true` but a
// blank facilitator and a blank pay_to, which is exactly the state of the checked-in stub
// with someone having flipped the switch early -- does not crash and does not take money.
// It noops.
//
// Being HALF configured is the dangerous state, worse than being off: a blank pay_to would
// quote payment terms that settle NOWHERE (the agent's money leaves and lands nothing),
// and a blank facilitator would mean we never actually verify or settle what we accept.
//
// This drives the real HTTP handlers, not the config struct, because "the handler is
// behind a gate" is a claim about the wiring and the wiring is what can rot.
func TestX402BlankConfigNoops(t *testing.T) {
	// Stand in for vault/<env>/x402.yml. This must happen before anything reads the
	// config -- x402Config is a sync.OnceValue, so the first read wins for the process.
	pop := pushBlankX402Config()
	defer pop()

	c := X402()

	// the loader must have refused to enable a half-configured x402, and said so
	assert.Equal(t, false, c.Enabled)
	assert.Equal(t, false, X402Enabled())
	assert.Equal(t, "", c.Facilitator.Url)
	assert.Equal(t, "", c.PayTo)

	ctx := context.Background()

	// 1. GET /x402/skus -> 404. Nothing is advertised.
	rec := httptest.NewRecorder()
	X402SkusHandler(rec, httptest.NewRequest("GET", "/x402/skus", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// 2. POST /x402/purchase -> 404. No terms quoted, so no agent ever pays.
	rec = httptest.NewRecorder()
	X402PurchaseHandler(rec, httptest.NewRequest(
		"POST", "/x402/purchase", strings.NewReader(`{"sku_id":"pro_1month"}`),
	))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// 3. the inline-upgrade settle path declines to handle the request at all
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/network/auth-client", strings.NewReader(`{}`))
	req.Header.Set(X402PaymentHeader, "a-signed-payment")
	assert.Equal(t, false, X402SettleInlineUpgrade(rec, req))

	// 4. no terms are quoted for anything
	_, err := X402PaymentRequiredFor("/network/auth-client", X402SkuProMonth, "limit")
	assert.NotEqual(t, nil, err)

	// 5. THE MONEY PATH. Even called directly -- bypassing every gate above -- verify and
	//    settle refuse before any http leaves the process. A blank facilitator url would
	//    otherwise send the request to a relative "/settle" and return a confusing
	//    "unsupported protocol scheme", which is a terrible way to find out that a payment
	//    was accepted but never actually settled.
	_, err = X402Verify(ctx, "a-signed-payment", X402Accept{})
	assert.NotEqual(t, nil, err)
	assert.Equal(t, true, strings.Contains(err.Error(), "facilitator is not configured"))

	_, err = X402Settle(ctx, "a-signed-payment", X402Accept{})
	assert.NotEqual(t, nil, err)
	assert.Equal(t, true, strings.Contains(err.Error(), "facilitator is not configured"))
}

// TestX402ConfigUsable is the deterministic half of the above.
//
// TestX402BlankConfigNoops drives the real handlers, but it can only observe x402 in
// whatever state the process loaded it (x402Config is a sync.OnceValue, so the first read
// wins and a test cannot count on being first). This one takes the decision function
// directly and pins it against every half-configured shape, with no ordering to depend on.
//
// The rule: a MISSING piece means off. Not "off for the missing part" -- off entirely.
// Half configured is worse than off, because it looks live.
func TestX402ConfigUsable(t *testing.T) {
	full := func() *X402Config {
		return &X402Config{
			Enabled:     true,
			Facilitator: x402FacilitatorConfig{Url: "https://x402.stripe.com", ApiKey: "sk_test_x"},
			PayTo:       "0xmerchant",
			Networks:    []string{"base"},
		}
	}

	// the fully-filled config is the only one that may take money
	assert.Equal(t, true, x402ConfigUsable(full()))

	// ...and every way of being half-filled turns it off
	for name, breakIt := range map[string]func(*X402Config){
		"blank facilitator url":     func(c *X402Config) { c.Facilitator.Url = "" },
		"blank facilitator api key": func(c *X402Config) { c.Facilitator.ApiKey = "" },
		"blank pay_to":              func(c *X402Config) { c.PayTo = "" },
		"no networks":               func(c *X402Config) { c.Networks = nil },
		"the checked-in stub":       func(c *X402Config) { *c = X402Config{Enabled: true} },
	} {
		c := full()
		breakIt(c)
		if x402ConfigUsable(c) {
			t.Errorf("x402 must be off with %s -- half configured takes money and delivers nothing", name)
		}
	}

	// not enabled is not usable, however complete the rest of it is
	c := full()
	c.Enabled = false
	assert.Equal(t, false, x402ConfigUsable(c))
}

// TestX402SkuCatalogRefusesFreeProducts pins the rule that nothing is ever offered for
// $0.00. The prices all come from pro.yml; if pro.yml is missing, every price is zero, and
// a sku quoted at zero would settle against a payment of nothing -- an agent would get a
// Pro month, or 10 TiB, for free. An unpriced sku is simply not for sale.
func TestX402SkuCatalogRefusesFreeProducts(t *testing.T) {
	// a config that offers everything, with no price backing any of it
	c := &X402Config{
		Enabled:  true,
		Networks: []string{"base"},
		Skus: map[string]x402SkuConfig{
			X402SkuProMonth:  {},
			X402SkuData1Tib:  {},
			X402SkuData10Tib: {},
		},
	}

	for _, sku := range x402SkusForConfig(c) {
		// whatever survives into the catalog must carry a real price and real goods
		assert.Equal(t, true, 0 < sku.PriceUsd)
		assert.Equal(t, true, 0 < sku.ByteCount)
	}
}
