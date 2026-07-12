package controller

import (
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/model"
)

// TestX402OffByDefault pins the most important property of an unconfigured x402: it
// is INERT. The stub in vault/<env>/x402.yml ships with enabled: false and empty
// credentials, so nothing may quote terms or accept payments until it is filled in.
func TestX402OffByDefault(t *testing.T) {
	assert.Equal(t, X402Enabled(), false)

	// with x402 off, terms cannot be quoted at all
	_, err := X402PaymentRequiredFor("/x402/purchase", X402SkuProMonth, "")
	assert.NotEqual(t, err, nil)
}

// TestX402RefusesHalfConfigured pins the guard that a filled-in `enabled: true` with
// missing credentials must NOT go live. A blank pay_to would quote terms that settle
// nowhere; a blank facilitator would accept payments we never verify. Both are worse
// than being off, so the loader turns x402 back off.
func TestX402RefusesHalfConfigured(t *testing.T) {
	// enabled, but no facilitator/pay_to -> must not be usable
	for _, c := range []*X402Config{
		{Enabled: true, PayTo: "0xmerchant", Networks: []string{"base"}},
		{Enabled: true, Facilitator: x402FacilitatorConfig{Url: "https://f", ApiKey: "sk"}, Networks: []string{"base"}},
		{Enabled: true, Facilitator: x402FacilitatorConfig{Url: "https://f"}, PayTo: "0xmerchant", Networks: []string{"base"}},
		{Enabled: true, Facilitator: x402FacilitatorConfig{Url: "https://f", ApiKey: "sk"}, PayTo: "0xmerchant"},
	} {
		assert.Equal(t, x402ConfigUsable(c), false)
	}

	// fully configured -> usable
	assert.Equal(t, x402ConfigUsable(&X402Config{
		Enabled:     true,
		Facilitator: x402FacilitatorConfig{Url: "https://f", ApiKey: "sk"},
		PayTo:       "0xmerchant",
		Networks:    []string{"base"},
	}), true)
}

// TestUsdToAtomic pins the price conversion. Terms quote INTEGER atomic units, so a
// rounding slip here charges the wrong amount -- $5.00 USDC is 5000000, not 5.
func TestUsdToAtomic(t *testing.T) {
	assert.Equal(t, usdToAtomic(5.00), "5000000")
	assert.Equal(t, usdToAtomic(30.00), "30000000")
	assert.Equal(t, usdToAtomic(0.01), "10000")
	assert.Equal(t, usdToAtomic(12.34), "12340000")
	// float noise must not leak into the amount
	assert.Equal(t, usdToAtomic(0.1+0.2), "300000")
}

// TestX402PaymentRequiredQuotesEveryNetwork pins chain-neutrality: we quote one term
// per configured network, all to the same merchant and amount, and the agent picks.
func TestX402PaymentRequiredQuotesEveryNetwork(t *testing.T) {
	c := &X402Config{
		Enabled:       true,
		Facilitator:   x402FacilitatorConfig{Url: "https://f", ApiKey: "sk"},
		PayTo:         "0xmerchant",
		Networks:      []string{"base", "solana", "tempo"},
		Asset:         "usdc",
		MaxPaymentUsd: 100,
		Skus:          map[string]x402SkuConfig{X402SkuData1Tib: {PriceUsd: 5}},
	}

	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	assert.Equal(t, err, nil)
	assert.Equal(t, paymentRequired.X402Version, X402Version)
	assert.Equal(t, len(paymentRequired.Accepts), 3)

	for _, accept := range paymentRequired.Accepts {
		assert.Equal(t, accept.Scheme, "exact")
		assert.Equal(t, accept.PayTo, "0xmerchant")
		assert.Equal(t, accept.Asset, "usdc")
		assert.Equal(t, accept.MaxAmountRequired, "5000000")
		assert.Equal(t, accept.Resource, "/x402/purchase")
	}
	assert.Equal(t, paymentRequired.Accepts[0].Network, "base")
	assert.Equal(t, paymentRequired.Accepts[1].Network, "solana")
	assert.Equal(t, paymentRequired.Accepts[2].Network, "tempo")
}

// TestX402RefusesToQuoteBadSku pins the guards around quoting: we would rather refuse
// than quote something wrong.
//
// x402.yml is a CATALOG now — listing a sku enables it. Prices come from pro.yml, so an
// agent and a human are always quoted the same number for the same thing; there is no
// second price to forget to update.
func TestX402RefusesToQuoteBadSku(t *testing.T) {
	c := &X402Config{
		Enabled:       true,
		Facilitator:   x402FacilitatorConfig{Url: "https://f", ApiKey: "sk"},
		PayTo:         "0xmerchant",
		Networks:      []string{"base"},
		Asset:         "usdc",
		MaxPaymentUsd: 10,
		Skus: map[string]x402SkuConfig{
			// data_10tib IS in the catalog, but pro.yml prices it at $30 -- over the
			// server-side ceiling below
			X402SkuData10Tib: {},
			X402SkuData1Tib:  {},
		},
	}

	// a sku that is not in the catalog is not offered
	_, err := x402PaymentRequiredForConfig(c, "/x402/purchase", "nonsense", "")
	assert.NotEqual(t, err, nil)

	// pro_1month is not in this catalog, so it is not quoted either
	_, err = x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuProMonth, "")
	assert.NotEqual(t, err, nil)

	// over the ceiling -> refuse rather than quote it
	_, err = x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData10Tib, "")
	assert.NotEqual(t, err, nil)

	// in the catalog and in bounds -> quoted, at pro.yml's price
	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	assert.Equal(t, err, nil)
	assert.Equal(t, paymentRequired.Accepts[0].MaxAmountRequired, "5000000") // $5, from pro.yml
}

// TestX402PricesComeFromProYml pins the single source of truth: an agent must never be
// quoted a different price than a human buying the same thing.
func TestX402PricesComeFromProYml(t *testing.T) {
	c := &X402Config{
		Enabled:     true,
		Facilitator: x402FacilitatorConfig{Url: "https://f", ApiKey: "sk"},
		PayTo:       "0xmerchant",
		Networks:    []string{"base"},
		Asset:       "usdc",
		Skus: map[string]x402SkuConfig{
			X402SkuProMonth:  {},
			X402SkuData1Tib:  {},
			X402SkuData10Tib: {},
		},
	}

	skus := x402SkusForConfig(c)
	prices := map[string]float64{}
	for _, sku := range skus {
		prices[sku.SkuId] = sku.PriceUsd
	}

	// exactly the numbers in pro.yml -- the same ones the site shows and Stripe charges
	assert.Equal(t, prices[X402SkuProMonth], model.Pro().PriceMonthlyUsd())
	assert.Equal(t, prices[X402SkuProMonth], float64(5))
	assert.Equal(t, prices[X402SkuData1Tib], float64(5))
	assert.Equal(t, prices[X402SkuData10Tib], float64(30))
}

// TestX402RequirementsForNetwork pins that a payment is always checked against the
// terms WE quoted for the network the agent names -- never against something the
// agent supplied, and never against a network we did not quote.
func TestX402RequirementsForNetwork(t *testing.T) {
	paymentRequired := &X402PaymentRequired{
		Accepts: []X402Accept{
			{Network: "base", MaxAmountRequired: "5000000", PayTo: "0xmerchant"},
			{Network: "solana", MaxAmountRequired: "5000000", PayTo: "0xmerchant"},
		},
	}

	requirements, err := x402RequirementsForNetwork(paymentRequired, "solana")
	assert.Equal(t, err, nil)
	assert.Equal(t, requirements.Network, "solana")
	assert.Equal(t, requirements.PayTo, "0xmerchant")

	// a network we never quoted is refused
	_, err = x402RequirementsForNetwork(paymentRequired, "ethereum")
	assert.NotEqual(t, err, nil)

	// ambiguous when several are quoted and none is named
	_, err = x402RequirementsForNetwork(paymentRequired, "")
	assert.NotEqual(t, err, nil)

	// unambiguous when only one is quoted
	single := &X402PaymentRequired{Accepts: []X402Accept{{Network: "base"}}}
	requirements, err = x402RequirementsForNetwork(single, "")
	assert.Equal(t, err, nil)
	assert.Equal(t, requirements.Network, "base")
}

// TestX402ReceiptTemplateRenders pins that the receipt actually renders through the
// normal email template system. A receipt that panics or renders blank would be
// noticed only after money moved, so it is worth a test.
func TestX402ReceiptTemplateRenders(t *testing.T) {
	// a data purchase: shows a data line, no Pro line
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(&X402ReceiptTemplate{
		Description:      "URnetwork data, 1TiB",
		PriceUsd:         5,
		Asset:            "usdc",
		Network:          "base",
		Transaction:      "0xdeadbeef",
		Pro:              false,
		BalanceByteCount: 1 * model.Tib,
	})
	assert.Equal(t, err, nil)
	assert.NotEqual(t, subject, "")
	assert.Equal(t, strings.Contains(bodyText, "$5.00"), true)
	assert.Equal(t, strings.Contains(bodyText, "base"), true)
	assert.Equal(t, strings.Contains(bodyText, "0xdeadbeef"), true)
	assert.Equal(t, strings.Contains(bodyText, "1TiB"), true)
	// not a Pro purchase -> no Pro line
	assert.Equal(t, strings.Contains(bodyText, "UR Pro is active"), false)
	assert.Equal(t, strings.Contains(bodyHtml, "$5.00"), true)

	// a Pro purchase: shows the Pro line
	_, _, bodyText, err = RenderEmailTemplate(&X402ReceiptTemplate{
		Description:      "UR Pro, 1 month",
		PriceUsd:         12.34,
		Asset:            "usdc",
		Network:          "solana",
		Transaction:      "0xfeed",
		Pro:              true,
		BalanceByteCount: 10 * model.Tib,
	})
	assert.Equal(t, err, nil)
	assert.Equal(t, strings.Contains(bodyText, "$12.34"), true)
	assert.Equal(t, strings.Contains(bodyText, "UR Pro is active"), true)
}
