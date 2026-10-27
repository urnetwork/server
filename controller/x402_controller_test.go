package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

// TestX402OffByDefault pins the most important property of an unconfigured x402: it
// is INERT. The stub in vault/<env>/x402.yml ships with enabled: false and empty
// credentials, so nothing may quote terms or accept payments until it is filled in.
func TestX402OffByDefault(t *testing.T) {
	connect.AssertEqual(t, X402Enabled(), false)

	// with x402 off, terms cannot be quoted at all
	_, err := X402PaymentRequiredFor("/x402/purchase", X402SkuProMonth, "")
	connect.AssertNotEqual(t, err, nil)
}

// x402TestFacilitator is a facilitator config with a real (throwaway) CDP key, so
// the config checks that sign with it pass.
func x402TestFacilitator(t testing.TB) x402FacilitatorConfig {
	key := newX402CdpTestEd25519Key(t)
	return x402FacilitatorConfig{
		Url:          "https://api.cdp.coinbase.com/platform/v2/x402",
		ApiKeyId:     key.id,
		ApiKeySecret: key.secret,
	}
}

// TestX402RefusesHalfConfigured pins the guard that a filled-in `enabled: true` with
// missing credentials must NOT go live. A blank pay_to would quote terms that settle
// nowhere; a blank facilitator would accept payments we never verify. Both are worse
// than being off, so the loader turns x402 back off.
func TestX402RefusesHalfConfigured(t *testing.T) {
	facilitator := x402TestFacilitator(t)
	noKey := facilitator
	noKey.ApiKeySecret = ""

	// enabled, but no facilitator/pay_to -> must not be usable
	for _, c := range []*X402Config{
		{Enabled: true, Asset: "usdc", PayTo: map[string]string{"base": x402CdpTestPayToBase}, Networks: []string{"base"}},
		{Enabled: true, Asset: "usdc", Facilitator: facilitator, Networks: []string{"base"}},
		{Enabled: true, Asset: "usdc", Facilitator: noKey, PayTo: map[string]string{"base": x402CdpTestPayToBase}, Networks: []string{"base"}},
		{Enabled: true, Asset: "usdc", Facilitator: facilitator, PayTo: map[string]string{"base": x402CdpTestPayToBase}},
		// a quoted network with no settlement address of its own: quoting it
		// would hand an agent an address that does not exist on that chain
		{
			Enabled:     true,
			Asset:       "usdc",
			Facilitator: facilitator,
			PayTo:       map[string]string{"base": x402CdpTestPayToBase},
			Networks:    []string{"base", "solana"},
		},
	} {
		connect.AssertEqual(t, x402ConfigUsable(c), false)
	}

	// fully configured -> usable
	connect.AssertEqual(t, x402ConfigUsable(&X402Config{
		Enabled:     true,
		Asset:       "usdc",
		Facilitator: facilitator,
		PayTo:       map[string]string{"base": x402CdpTestPayToBase, "solana": x402CdpTestPayToSolana},
		Networks:    []string{"base"},
	}), true)
}

// TestUsdToAtomic pins the price conversion. Terms quote INTEGER atomic units, so a
// rounding slip here charges the wrong amount -- $5.00 USDC is 5000000, not 5.
func TestUsdToAtomic(t *testing.T) {
	connect.AssertEqual(t, usdToAtomic(5.00), "5000000")
	connect.AssertEqual(t, usdToAtomic(30.00), "30000000")
	connect.AssertEqual(t, usdToAtomic(0.01), "10000")
	connect.AssertEqual(t, usdToAtomic(12.34), "12340000")
	// float noise must not leak into the amount
	connect.AssertEqual(t, usdToAtomic(0.1+0.2), "300000")
}

// TestX402PaymentRequiredQuotesEveryNetwork pins chain-neutrality: we quote one term
// per configured network, each to ITS OWN settlement address and in its own token,
// at the same amount, and the agent picks.
//
// The per-network address is the load-bearing part. Base settles to an EVM 0x
// address and Solana to a base58 account, so quoting one address across both hands
// an agent an address that does not exist on one of the chains it was offered. USDC
// sent there is unrecoverable.
func TestX402PaymentRequiredQuotesEveryNetwork(t *testing.T) {
	x402CdpTestPrices(t)
	t.Setenv("WARP_DOMAIN", "x402.example")

	key := newX402CdpTestEd25519Key(t)
	facilitator := newX402CdpTestFacilitator(t, key)
	c := x402CdpTestConfig(t, x402CdpTestDocument(facilitator.url, key, "base", "solana"))
	payTo := map[string]string{
		"base":   x402CdpTestPayToBase,
		"solana": x402CdpTestPayToSolana,
	}
	asset := map[string]string{
		"base":   x402CdpTestUsdcBase,
		"solana": x402CdpTestUsdcSolana,
	}

	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, paymentRequired.X402Version, X402Version)
	connect.AssertEqual(t, len(paymentRequired.Accepts), 2)

	for _, accept := range paymentRequired.Accepts {
		connect.AssertEqual(t, accept.Scheme, "exact")
		connect.AssertEqual(t, accept.PayTo, payTo[accept.Network])
		connect.AssertEqual(t, accept.Asset, asset[accept.Network])
		connect.AssertEqual(t, accept.MaxAmountRequired, "3000000")
		connect.AssertEqual(t, accept.Resource, "https://api.x402.example/x402/purchase")
	}
	// Each address really is distinct, so the assertion above cannot pass by
	// every network sharing one string again.
	connect.AssertNotEqual(t, paymentRequired.Accepts[0].PayTo, paymentRequired.Accepts[1].PayTo)
	connect.AssertEqual(t, paymentRequired.Accepts[0].Network, "base")
	connect.AssertEqual(t, paymentRequired.Accepts[1].Network, "solana")
}

// TestX402RefusesToQuoteBadSku pins the guards around quoting: we would rather refuse
// than quote something wrong.
//
// x402.yml is a CATALOG now — listing a sku enables it. Prices come from pro.yml, so an
// agent and a human are always quoted the same number for the same thing; there is no
// second price to forget to update.
func TestX402RefusesToQuoteBadSku(t *testing.T) {
	skipWithoutProYml(t)
	t.Setenv("WARP_DOMAIN", "x402.example")

	c := &X402Config{
		Enabled:       true,
		Facilitator:   x402TestFacilitator(t),
		PayTo:         map[string]string{"base": x402CdpTestPayToBase, "solana": x402CdpTestPayToSolana},
		Networks:      []string{"base"},
		Asset:         "usdc",
		MaxPaymentUsd: 10,
		Skus: map[string]x402SkuConfig{
			// data_10tib IS in the catalog, but pro.yml prices it at $20 -- over the
			// server-side ceiling below
			X402SkuData10Tib: {},
			X402SkuData1Tib:  {},
		},
	}

	// a sku that is not in the catalog is not offered
	_, err := x402PaymentRequiredForConfig(c, "/x402/purchase", "nonsense", "")
	connect.AssertNotEqual(t, err, nil)

	// pro_1month is not in this catalog, so it is not quoted either
	_, err = x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuProMonth, "")
	connect.AssertNotEqual(t, err, nil)

	// over the ceiling -> refuse rather than quote it
	_, err = x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData10Tib, "")
	connect.AssertNotEqual(t, err, nil)

	// in the catalog and in bounds -> quoted, at pro.yml's price
	paymentRequired, err := x402PaymentRequiredForConfig(c, "/x402/purchase", X402SkuData1Tib, "")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, paymentRequired.Accepts[0].MaxAmountRequired, "3000000") // $3, from pro.yml
}

// TestX402PricesComeFromProYml pins the single source of truth: an agent must never be
// quoted a different price than a human buying the same thing.
func TestX402PricesComeFromProYml(t *testing.T) {
	skipWithoutProYml(t)

	c := &X402Config{
		Enabled:     true,
		Facilitator: x402TestFacilitator(t),
		PayTo:       map[string]string{"base": x402CdpTestPayToBase, "solana": x402CdpTestPayToSolana},
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
	connect.AssertEqual(t, prices[X402SkuProMonth], model.Pro().PriceMonthlyUsd())
	connect.AssertEqual(t, prices[X402SkuProMonth], float64(5))
	connect.AssertEqual(t, prices[X402SkuData1Tib], float64(3))
	connect.AssertEqual(t, prices[X402SkuData10Tib], float64(20))
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
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, requirements.Network, "solana")
	connect.AssertEqual(t, requirements.PayTo, "0xmerchant")

	// a network we never quoted is refused
	_, err = x402RequirementsForNetwork(paymentRequired, "ethereum")
	connect.AssertNotEqual(t, err, nil)

	// ambiguous when several are quoted and none is named
	_, err = x402RequirementsForNetwork(paymentRequired, "")
	connect.AssertNotEqual(t, err, nil)

	// unambiguous when only one is quoted
	single := &X402PaymentRequired{Accepts: []X402Accept{{Network: "base"}}}
	requirements, err = x402RequirementsForNetwork(single, "")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, requirements.Network, "base")
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
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, subject, "")
	connect.AssertEqual(t, strings.Contains(bodyText, "$5.00"), true)
	connect.AssertEqual(t, strings.Contains(bodyText, "base"), true)
	connect.AssertEqual(t, strings.Contains(bodyText, "0xdeadbeef"), true)
	connect.AssertEqual(t, strings.Contains(bodyText, "1TiB"), true)
	// not a Pro purchase -> no Pro line
	connect.AssertEqual(t, strings.Contains(bodyText, "UR Pro is active"), false)
	connect.AssertEqual(t, strings.Contains(bodyHtml, "$5.00"), true)

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
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(bodyText, "$12.34"), true)
	connect.AssertEqual(t, strings.Contains(bodyText, "UR Pro is active"), true)
}
