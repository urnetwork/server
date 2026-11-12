package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// x402 is the HTTP 402 "Payment Required" purchase flow, for agents.
//
// An agent requests something it cannot have yet. Instead of a checkout page, the
// server answers 402 with machine-readable payment terms. The agent signs a
// payment for one of the quoted terms and retries the SAME request with it in a
// header. The server verifies and settles that payment through the facilitator,
// then fulfills. No human in the loop.
//
// Both protocol versions are spoken, on every entry point:
//   - x402 v2 (specs/x402-specification-v2.md and specs/transports-v2/http.md,
//     2025-12-09): the terms are base64 JSON in the PAYMENT-REQUIRED response
//     header, networks are CAIP-2 ids ("eip155:8453"), each term quotes
//     `amount`, the resource is described once at the top, the payment comes
//     back in PAYMENT-SIGNATURE and names the terms it `accepted`, and the
//     receipt is PAYMENT-RESPONSE.
//   - x402 v1 (specs/x402-specification-v1.md, specs/transports-v1/http.md):
//     the terms are the JSON body, networks are names ("base"), each term
//     quotes `maxAmountRequired` and its own `resource`, the payment comes back
//     in X-PAYMENT, and the receipt is X-PAYMENT-RESPONSE.
//
// One 402 carries both: the v2 header and the v1 body. A v2 client reads the
// header and only falls back to the body when there is none; a v1 client never
// looks at the header (x402-foundation/x402 go/http/client.go
// GetPaymentRequiredResponse, typescript/packages/core/src/http/x402HTTPClient.ts
// getPaymentRequiredResponse). The facilitator verifies and settles either
// version (docs.x402.org migration guide: "The facilitator supports both V1 and
// V2 protocols"), and the payment's own x402Version decides which one it is
// checked and answered in. So v1 clients keep working unchanged while v2
// clients get v2.
//
// The money lands in Stripe crypto deposit addresses, and settles through the
// Coinbase Developer Platform (CDP) facilitator, the one Stripe's x402 guide uses
// for mainnet (docs.stripe.com/payments/machine/x402); after settlement the
// payment is recorded in Stripe (x402_stripe_record.go). We quote one term per
// configured network and the agent pays on whichever chain it holds funds. See
// vault/<env>/x402.yml for the config (facilitator, pay-to, networks, prices),
// x402_networks.go for the networks and tokens, and x402_facilitator.go for the
// facilitator protocol.
//
// The whole surface is inert until x402.yml exists AND sets enabled: true.

// The protocol versions this server speaks.
const (
	X402VersionV1 = 1
	X402VersionV2 = 2
)

// X402Versions are the versions this server speaks, newest first.
var X402Versions = []int{X402VersionV2, X402VersionV1}

// X402 sku ids. Prices and amounts come from pro.yml. x402.yml turns on the
// Pro month; the data skus are pro.yml's data packs sold over x402
// (data_code.skus with rails x402): data_1tib, data_10tib and data_256gib.
const (
	X402SkuProMonth  = "pro_1month"
	X402SkuData1Tib  = "data_1tib"
	X402SkuData10Tib = "data_10tib"
	// 256 GiB for $0.75: the pack for clients with a $1 per-payment cap, sold
	// over x402 and Gamma only (pro.yml)
	X402SkuData256Gib = "data_256gib"
)

// UsdcDecimals is the atomic-unit scale for USDC. Terms quote integer atomic units,
// so $5.00 is "5000000".
const UsdcDecimals = 6

// The x402 v2 HTTP headers (specs/transports-v2/http.md, "Header Summary"),
// each carrying base64 of a JSON object.
const (
	// server to client, with the 402: the PaymentRequired
	X402V2PaymentRequiredHeader = "PAYMENT-REQUIRED"
	// client to server, on the paid retry: the PaymentPayload
	X402V2PaymentSignatureHeader = "PAYMENT-SIGNATURE"
	// server to client, after settlement: the SettlementResponse
	X402V2PaymentResponseHeader = "PAYMENT-RESPONSE"
)

// The x402 v1 HTTP headers (specs/transports-v1/http.md). v1 carries its
// PaymentRequired in the 402 body.
const (
	// client to server, on the paid retry
	X402V1PaymentHeader = "X-PAYMENT"
	// server to client, after settlement
	X402V1PaymentResponseHeader = "X-PAYMENT-RESPONSE"
)

// X402QuoteHeader is the request header with which a caller asks auth-client to
// answer a refusal an upgrade lifts with x402 payment terms (`X-UR-X402: 1`).
// See X402QuotesClient.
const X402QuoteHeader = "X-UR-X402"

// x402TermsCacheControl keeps a 402 out of shared caches: its terms are
// per caller and per moment (specs/transports-v2/http.md, "Browser clients";
// the reference servers send no-store, x402-foundation/x402 go/http/server.go
// PaymentRequiredCacheControl).
const x402TermsCacheControl = "no-store"

type x402SkuConfig struct {
	PriceUsd float64 `yaml:"price_usd"`
}

type x402ReceiptConfig struct {
	Enabled bool `yaml:"enabled"`
}

type x402FacilitatorConfig struct {
	// The facilitator's base url; /verify, /settle and /supported are under
	// it. CDP: https://api.cdp.coinbase.com/platform/v2/x402
	Url string `yaml:"url"`
	// The CDP secret API key (portal.cdp.coinbase.com): its id, and its
	// secret, an Ed25519 key in base64 or an EC private key in PEM. Each
	// request carries a JWT signed with it, minted for that request
	// (x402_facilitator.go). There is no static bearer: the CDP facilitator
	// accepts only these JWTs.
	ApiKeyId     string `yaml:"api_key_id"`
	ApiKeySecret string `yaml:"api_key_secret"`
}

type X402Config struct {
	Enabled     bool                  `yaml:"enabled"`
	Facilitator x402FacilitatorConfig `yaml:"facilitator"`
	// PayTo is the settlement address PER NETWORK, keyed by the same names as
	// Networks: the Stripe crypto deposit address created for that network.
	//
	// It is a map and not one string because the address is network-specific
	// and the forms are not interchangeable: Base settles to an EVM 0x
	// address, Solana to a base58 account. Quoting one string across both
	// would hand an agent an address that does not exist on one of the
	// chains it was offered, and USDC sent there is gone.
	PayTo map[string]string `yaml:"pay_to"`
	// Networks are named as x402 v1 names them ("base", "solana"). The v2
	// terms quote the same networks by their CAIP-2 ids (x402_networks.go).
	Networks []string `yaml:"networks"`
	// Asset is the token symbol, "usdc". The terms quote the token's address
	// on each network, from x402_networks.go.
	Asset         string                   `yaml:"asset"`
	Skus          map[string]x402SkuConfig `yaml:"skus"`
	MaxPaymentUsd float64                  `yaml:"max_payment_usd"`
	Receipt       x402ReceiptConfig        `yaml:"receipt"`
}

// x402ConfigProblem says why a config cannot take money, or nil when it can.
// Being HALF configured is worse than being off: a blank pay_to would quote
// terms that settle nowhere, a facilitator we cannot authenticate to would
// verify nothing, and a network without a known token address would be quoted
// a guess. So anything missing or wrong means x402 stays off.
//
// The reasons name fields, never their values.
func x402ConfigProblem(c *X402Config) error {
	if !c.Enabled {
		return errors.New("enabled is false")
	}

	facilitatorUrl, err := url.Parse(c.Facilitator.Url)
	if c.Facilitator.Url == "" || err != nil || facilitatorUrl.Scheme != "https" || facilitatorUrl.Host == "" {
		return errors.New(
			"facilitator.url must be the facilitator's https url (CDP: https://api.cdp.coinbase.com/platform/v2/x402)",
		)
	}
	if c.Facilitator.ApiKeyId == "" || c.Facilitator.ApiKeySecret == "" {
		return errors.New(
			"facilitator.api_key_id and facilitator.api_key_secret, a CDP secret API key, are required " +
				"(the static facilitator.api_key is no longer used)",
		)
	}
	// the secret must be a key CDP's JWT signing takes, or every request
	// would fail at payment time instead of here
	supportedUrl, err := x402FacilitatorEndpoint(c, "/supported")
	if err != nil {
		return err
	}
	if _, err := x402FacilitatorAuthorization(c, http.MethodGet, supportedUrl); err != nil {
		return errors.New(
			"facilitator.api_key_secret is not a CDP API key secret " +
				"(an Ed25519 key in base64, or an EC private key in PEM with real newlines)",
		)
	}

	if len(c.Networks) == 0 {
		return errors.New("networks is empty")
	}
	// Every listed network needs a token address for the asset and its own
	// settlement address in that network's form. A network that fails either
	// is not one we quietly drop -- it means the operator believes they
	// configured a chain they did not, so the whole feature stays off rather
	// than silently offering fewer chains than the vault lists.
	for _, network := range c.Networks {
		token, err := x402TokenFor(network, c.Asset)
		if err != nil {
			return err
		}
		payTo := c.PayTo[network]
		if payTo == "" {
			return fmt.Errorf("pay_to.%s (the Stripe deposit address for %s) is missing", network, network)
		}
		if !x402AddressValid(token.addressKind, payTo) {
			return fmt.Errorf("pay_to.%s is not an address of the %s network's form", network, network)
		}
	}
	return nil
}

// x402ConfigUsable reports whether a config is complete enough to actually take
// money. See x402ConfigProblem.
func x402ConfigUsable(c *X402Config) bool {
	return x402ConfigProblem(c) == nil
}

// X402PayTo is the settlement address for one network, or empty when that
// network has none.
func (c *X402Config) X402PayTo(network string) string {
	return c.PayTo[network]
}

// x402Config loads vault/<env>/x402.yml. The resource is OPTIONAL: an env with no
// x402.yml simply has x402 turned off, rather than failing to boot.
var x402Config = sync.OnceValue(func() *X402Config {
	c := &X402Config{}

	resource, err := server.Vault.SimpleResource("x402.yml")
	if err != nil {
		glog.Infof("[x402]not configured (no x402.yml); x402 is off\n")
		return c
	}
	resource.UnmarshalYaml(c)

	if c.Enabled {
		if err := x402ConfigProblem(c); err != nil {
			glog.Errorf("[x402]enabled but not usable, so x402 is off: %s\n", err)
			c.Enabled = false
		} else if _, err := server.Domain(); err != nil {
			// the terms name their resource as a url on this api's domain
			glog.Errorf("[x402]enabled but WARP_DOMAIN is not set, so terms cannot name a resource url; x402 is off\n")
			c.Enabled = false
		}
	}

	return c
})

// x402ConfigInstance, when set, overrides the vault config (the swappable
// instance pattern of `SetStConfig`, for tests).
var x402ConfigInstance *X402Config

func SetX402Config(c *X402Config) {
	x402ConfigInstance = c
}

func X402() *X402Config {
	if x402ConfigInstance != nil {
		return x402ConfigInstance
	}
	return x402Config()
}

// X402Enabled reports whether the x402 flow is live. Every x402 entry point checks
// this first, so the feature is a no-op until the config is filled in.
func X402Enabled() bool {
	return X402().Enabled
}

// ----- payment terms -----

// X402Accept is one quoted way to pay: a scheme+network+amount+destination, the
// x402 v1 PaymentRequirements (specs/x402-specification-v1.md 5.1.2). We quote one
// per supported network and let the agent choose.
type X402Accept struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`
	MaxAmountRequired string `json:"maxAmountRequired"`
	// Resource is a url: v1 clients refuse terms whose resource is not one
	Resource    string `json:"resource"`
	Description string `json:"description"`
	MimeType    string `json:"mimeType"`
	PayTo       string `json:"payTo"`
	// Asset is the token's address on the network (contract or mint), never
	// its symbol
	Asset             string `json:"asset"`
	MaxTimeoutSeconds int    `json:"maxTimeoutSeconds"`
	// Extra is what the scheme needs to build the payment on this network:
	// the token's EIP-712 domain {name, version} for EVM exact, the
	// facilitator's {feePayer} for Solana exact
	Extra map[string]any `json:"extra,omitempty"`
}

// X402PaymentRequired is the x402 v1 402 body: what the agent must pay to proceed.
type X402PaymentRequired struct {
	X402Version int          `json:"x402Version"`
	Error       string       `json:"error"`
	Accepts     []X402Accept `json:"accepts"`
}

// X402ResourceInfo describes what is being paid for, once per x402 v2
// PaymentRequired (specs/x402-specification-v2.md 5.1.2, ResourceInfo).
type X402ResourceInfo struct {
	Url         string `json:"url"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// X402AcceptV2 is one quoted way to pay in x402 v2, its PaymentRequirements
// (specs/x402-specification-v2.md 5.1.2): the network is a CAIP-2 id, the
// price is `amount`, and the resource is described in the PaymentRequired
// instead of here. The scheme's extra is the same as in v1.
type X402AcceptV2 struct {
	Scheme            string         `json:"scheme"`
	Network           string         `json:"network"`
	Amount            string         `json:"amount"`
	Asset             string         `json:"asset"`
	PayTo             string         `json:"payTo"`
	MaxTimeoutSeconds int            `json:"maxTimeoutSeconds"`
	Extra             map[string]any `json:"extra,omitempty"`
}

// X402PaymentRequiredV2 is the x402 v2 PaymentRequired, which a 402 carries
// base64-encoded in the PAYMENT-REQUIRED header (specs/transports-v2/http.md).
type X402PaymentRequiredV2 struct {
	X402Version int              `json:"x402Version"`
	Error       string           `json:"error,omitempty"`
	Resource    X402ResourceInfo `json:"resource"`
	Accepts     []X402AcceptV2   `json:"accepts"`
}

// X402Terms are the terms of one quote in both protocol versions: the same
// price for the same sku, to the same addresses, in the same token, written
// the way each version writes it. A version is nil when it has no network it
// can quote now (only a Solana network can sit out in one version, while the
// facilitator has not named that version's fee payer).
type X402Terms struct {
	V1 *X402PaymentRequired
	V2 *X402PaymentRequiredV2
	// what the terms sell
	sku *X402Sku
}

// Testing_NewX402Terms builds terms for a sku from parts, for tests outside
// this package that cannot build a quote from a config.
func Testing_NewX402Terms(
	v1 *X402PaymentRequired,
	v2 *X402PaymentRequiredV2,
	description string,
	priceUsd float64,
) *X402Terms {
	return &X402Terms{
		V1:  v1,
		V2:  v2,
		sku: &X402Sku{Description: description, PriceUsd: priceUsd, Amount: usdToAtomic(priceUsd)},
	}
}

// PriceUsd is what the terms charge.
func (self *X402Terms) PriceUsd() float64 {
	if self.sku == nil {
		return 0
	}
	return self.sku.PriceUsd
}

// Description is what the terms sell.
func (self *X402Terms) Description() string {
	if self.sku == nil {
		return ""
	}
	return self.sku.Description
}

// WithError is a copy of the terms with their error replaced, in both
// versions: the reason a payment failed, when they are quoted again.
func (self *X402Terms) WithError(message string, reason string) *X402Terms {
	if reason == "" {
		reason = message
	}
	return self.withError(message, reason)
}

// withError is a copy of the terms with their errors replaced: v1's (free
// text) and v2's (an x402 error code when there is one).
func (self *X402Terms) withError(v1Error string, v2Error string) *X402Terms {
	terms := &X402Terms{sku: self.sku}
	if self.V1 != nil {
		v1 := *self.V1
		v1.Error = v1Error
		terms.V1 = &v1
	}
	if self.V2 != nil {
		v2 := *self.V2
		v2.Error = v2Error
		terms.V2 = &v2
	}
	return terms
}

// X402Sku is a thing an agent can buy, with price and amount from pro.yml. The Pro
// month is on when x402.yml lists it; a data sku is on when pro.yml sells its pack over
// x402.
type X402Sku struct {
	SkuId       string  `json:"sku_id"`
	Description string  `json:"description"`
	PriceUsd    float64 `json:"price_usd"`
	// Amount is the price in atomic units of the asset, exactly what the terms
	// quote: USDC has 6 decimals, so $5.00 is "5000000". An agent sets its
	// client's per-payment cap from it.
	Amount string `json:"amount,omitempty"`
	// Pro is true for the Pro-month sku: it grants the Pro entitlement. The data
	// skus grant data only and never confer Pro.
	Pro       bool            `json:"pro"`
	ByteCount model.ByteCount `json:"byte_count,omitempty"`
}

// X402Skus is everything purchasable over x402, with prices and amounts from pro.yml
// so the product spec stays in one place.
func X402Skus() []*X402Sku {
	return x402SkusForConfig(X402())
}

func X402SkuById(skuId string) *X402Sku {
	return x402SkuByIdForConfig(X402(), skuId)
}

func x402SkusForConfig(c *X402Config) []*X402Sku {
	skus := []*X402Sku{}

	// x402.yml turns on the Pro month; pro.yml is the only place a PRICE lives, and the
	// only list of data packs and where each is sold.
	//
	// The price used to be duplicated in x402.yml, so it could drift from pro.yml and an
	// agent would be quoted a different number than a human paying by card for the same
	// thing. There is one price.
	if _, ok := c.Skus[X402SkuProMonth]; ok {
		priceUsd := model.Pro().PriceMonthlyUsd()
		byteCount := model.Pro().DataAmount(true)
		// Never offer it for nothing. With no pro.yml both of these are zero, and a sku
		// quoted at $0.00 is not merely odd -- the terms would settle against a payment of
		// nothing, and an agent would get a Pro month free. An unpriced sku is not for
		// sale. (The data skus below already do this; the Pro one used to not.)
		if priceUsd <= 0 || byteCount <= 0 {
			glog.Errorf("[x402]no price/amount in pro.yml for %s; not offering it\n", X402SkuProMonth)
		} else {
			skus = append(skus, &X402Sku{
				SkuId:       X402SkuProMonth,
				Description: "UR Pro, 1 month",
				PriceUsd:    priceUsd,
				Amount:      usdToAtomic(priceUsd),
				Pro:         true,
				ByteCount:   byteCount,
			})
		}
	}

	// The data skus are pro.yml's data packs that its rails sell over x402
	// (data_code.skus, model.DataRailX402), in pro.yml order, so the catalog is stable
	// across calls: data_1tib, data_10tib and data_256gib, the 256 GiB pack at $0.75 for
	// clients with a $1 per-payment cap. A pack pro.yml does not sell over x402 is not
	// in the catalog, so it is never quoted or settled. x402.yml does not list them: an
	// x402.yml written before rails still names data_1tib and data_10tib, which change
	// nothing.
	for _, dataSku := range model.Pro().DataCodeSkusOn(model.DataRailX402) {
		// the price comes from pro.yml data_code.skus -- the same number every rail
		// that sells the pack quotes
		if dataSku.PriceUsd <= 0 || dataSku.Data <= 0 {
			glog.Errorf("[x402]no price/amount in pro.yml for %s; not offering it\n", dataSku.Sku)
			continue
		}
		skus = append(skus, &X402Sku{
			SkuId:       dataSku.Sku,
			Description: fmt.Sprintf("URnetwork data, %s", model.ByteCountHumanReadable(dataSku.Data)),
			PriceUsd:    dataSku.PriceUsd,
			Amount:      usdToAtomic(dataSku.PriceUsd),
			Pro:         false,
			ByteCount:   dataSku.Data,
		})
	}

	return skus
}

func x402SkuByIdForConfig(c *X402Config, skuId string) *X402Sku {
	for _, sku := range x402SkusForConfig(c) {
		if sku.SkuId == skuId {
			return sku
		}
	}
	return nil
}

// usdToAtomic converts a USD price to the integer atomic units the terms quote.
func usdToAtomic(priceUsd float64) string {
	atomic := int64(math.Round(priceUsd * math.Pow10(UsdcDecimals)))
	return strconv.FormatInt(atomic, 10)
}

// x402DefaultClientCapUsd is the most a default x402 client pays without its
// owner raising the cap. The x402 SDKs' clients (TypeScript @x402/core and
// Go x402-foundation/x402/go/v2 since 2.23.0, and the Python SDK) refuse any
// payment of a USD-pegged asset over "$1" unless spend controls are raised
// (docs.x402.org quickstart for buyers, "Spend Controls";
// DefaultMaxAmountPerPayment = "$1"). Coinbase AgentKit's x402 action
// provider defaults maxPaymentUsdc to 1.0, and the older clients cap lower:
// the v1 x402-fetch and x402-axios at 0.10 USDC (maxValue), Cloudflare
// Agents' withX402Client at 0.10 USDC (maxPaymentValue). Such a client refuses
// before it signs, so it never sends a payment we could answer.
const x402DefaultClientCapUsd = 1.0

// x402TermsError is the terms' error: why payment is required, then the
// price, and, when the price is over what a default x402 client pays, how to
// let it pay. The error is the one field of the terms a person or a model
// reads (specs/x402-specification-v2.md 5.1.2: "Human-readable error message
// explaining why payment is required").
func x402TermsError(reason string, sku *X402Sku) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "payment required"
	}
	if !strings.HasSuffix(reason, ".") {
		reason += "."
	}
	text := fmt.Sprintf("%s %s costs $%.2f in USDC.", reason, sku.Description, sku.PriceUsd)
	if x402DefaultClientCapUsd < sku.PriceUsd {
		text += fmt.Sprintf(
			" x402 clients refuse a payment over $1 by default: raise your client's per-payment cap to $%.2f or more to pay (see https://ur.io/agents.md).",
			sku.PriceUsd,
		)
	}
	return text
}

// X402TermsFor builds the terms for a sku at a resource, in both protocol
// versions: one term per configured network, all settling to our merchant
// address.
//
// Returns an error when the sku is unknown or its price exceeds max_payment_usd --
// we would rather refuse to quote than quote something wrong.
func X402TermsFor(resource string, skuId string, reason string) (*X402Terms, error) {
	return x402TermsForConfig(X402(), resource, skuId, reason)
}

func x402TermsForConfig(
	c *X402Config,
	resource string,
	skuId string,
	reason string,
) (*X402Terms, error) {
	if !c.Enabled {
		return nil, fmt.Errorf("x402 is not enabled")
	}

	sku := x402SkuByIdForConfig(c, skuId)
	if sku == nil {
		return nil, fmt.Errorf("unknown x402 sku: %s", skuId)
	}
	if sku.PriceUsd <= 0 {
		return nil, fmt.Errorf("x402 sku %s has no price set", skuId)
	}
	if 0 < c.MaxPaymentUsd && c.MaxPaymentUsd < sku.PriceUsd {
		return nil, fmt.Errorf(
			"x402 sku %s price %.2f exceeds max_payment_usd %.2f",
			skuId, sku.PriceUsd, c.MaxPaymentUsd,
		)
	}

	resourceUrl, err := x402ResourceUrl(resource)
	if err != nil {
		return nil, err
	}

	amount := usdToAtomic(sku.PriceUsd)

	accepts := []X402Accept{}
	acceptsV2 := []X402AcceptV2{}
	for _, network := range c.Networks {
		token, err := x402TokenFor(network, c.Asset)
		if err != nil {
			// The loader refuses such a config (x402ConfigProblem). One set
			// some other way is refused here rather than quoted a guess.
			return nil, err
		}
		caip2, _ := x402Caip2Network(network)
		for _, version := range X402Versions {
			extra := map[string]any{}
			if token.eip712Name != "" {
				extra["name"] = token.eip712Name
				extra["version"] = token.eip712Version
			}
			if token.feePayerRequired {
				svm, ok := x402SupportedSvmFor(c, version, network)
				if !ok {
					// no client can build this payment without the fee
					// payer, so this network sits out of this version until
					// the facilitator names one
					continue
				}
				for key, value := range svm.extra() {
					extra[key] = value
				}
			}
			switch version {
			case X402VersionV1:
				accepts = append(accepts, X402Accept{
					Scheme:            "exact",
					Network:           network,
					MaxAmountRequired: amount,
					Resource:          resourceUrl,
					Description:       sku.Description,
					MimeType:          "application/json",
					PayTo:             c.PayTo[network],
					Asset:             token.address,
					MaxTimeoutSeconds: 300,
					Extra:             extra,
				})
			case X402VersionV2:
				acceptsV2 = append(acceptsV2, X402AcceptV2{
					Scheme:            "exact",
					Network:           caip2,
					Amount:            amount,
					Asset:             token.address,
					PayTo:             c.PayTo[network],
					MaxTimeoutSeconds: 300,
					Extra:             extra,
				})
			}
		}
	}
	if len(accepts) == 0 && len(acceptsV2) == 0 {
		return nil, fmt.Errorf("x402 has no network it can quote now")
	}

	termsError := x402TermsError(reason, sku)
	terms := &X402Terms{sku: sku}
	if 0 < len(accepts) {
		terms.V1 = &X402PaymentRequired{
			X402Version: X402VersionV1,
			Error:       termsError,
			Accepts:     accepts,
		}
	}
	if 0 < len(acceptsV2) {
		terms.V2 = &X402PaymentRequiredV2{
			X402Version: X402VersionV2,
			Error:       termsError,
			Resource: X402ResourceInfo{
				Url:         resourceUrl,
				Description: sku.Description,
				MimeType:    "application/json",
			},
			Accepts: acceptsV2,
		}
	}
	return terms, nil
}

// X402PaymentRequiredFor is the x402 v1 terms for a sku (X402TermsFor's V1).
func X402PaymentRequiredFor(resource string, skuId string, reason string) (*X402PaymentRequired, error) {
	return x402PaymentRequiredForConfig(X402(), resource, skuId, reason)
}

func x402PaymentRequiredForConfig(
	c *X402Config,
	resource string,
	skuId string,
	reason string,
) (*X402PaymentRequired, error) {
	terms, err := x402TermsForConfig(c, resource, skuId, reason)
	if err != nil {
		return nil, err
	}
	if terms.V1 == nil {
		return nil, fmt.Errorf("x402 has no network it can quote in x402 v1 now")
	}
	return terms.V1, nil
}

// x402ResourceUrl is the resource the terms quote: each v1 term's `resource`,
// and the v2 ResourceInfo's url. x402 v1 requires a url there
// (specs/x402-specification-v1.md 5.1.2), and v1 clients check that it is one
// before they pay: the legacy fetch and axios clients parse each term with a
// zod schema whose resource is z.string().url(), which refuses a bare path. A
// path is served by this API, so it gets the API's public origin,
// https://api.<WARP_DOMAIN>, the same origin the other services reach the API
// on. A resource that is already a url (the MCP tool's) is quoted as it is.
func x402ResourceUrl(resource string) (string, error) {
	if strings.HasPrefix(resource, "/") {
		domain, err := server.Domain()
		if err != nil {
			return "", fmt.Errorf("x402 cannot quote %s as a url: %w", resource, err)
		}
		return "https://api." + domain + resource, nil
	}
	parsed, err := url.Parse(resource)
	if err != nil || parsed.Scheme == "" {
		return "", fmt.Errorf("x402 resource %q is not a url", resource)
	}
	return resource, nil
}

// x402Base64Json is base64 of an object's JSON, the encoding of every x402 v2
// header (and of the v1 payment response).
func x402Base64Json(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encoded), nil
}

// WriteX402PaymentRequired writes a 402 with payment terms. This is the one place
// a 402 is produced, so every gated endpoint quotes terms the same way: the v2
// PaymentRequired in the PAYMENT-REQUIRED header, the canonical place for it
// (specs/transports-v2/http.md), and the v1 PaymentRequired as the body, where
// v1 clients read it. A v2-only quote (no v1 network) writes its terms as the
// body too, for a reader without a header decoder.
func WriteX402PaymentRequired(w http.ResponseWriter, terms *X402Terms) {
	var body []byte
	var err error
	switch {
	case terms.V1 != nil:
		body, err = json.Marshal(terms.V1)
	case terms.V2 != nil:
		body, err = json.Marshal(terms.V2)
	default:
		err = errors.New("no terms")
	}
	if err != nil {
		http.Error(w, "Could not encode payment terms.", http.StatusInternalServerError)
		return
	}
	if terms.V2 != nil {
		header, err := x402Base64Json(terms.V2)
		if err != nil {
			http.Error(w, "Could not encode payment terms.", http.StatusInternalServerError)
			return
		}
		w.Header().Set(X402V2PaymentRequiredHeader, header)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", x402TermsCacheControl)
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write(body)
}

// ----- facilitator (verify + settle) -----

type X402VerifyResponse struct {
	IsValid       bool   `json:"isValid"`
	InvalidReason string `json:"invalidReason,omitempty"`
	Payer         string `json:"payer,omitempty"`
}

type X402SettleResponse struct {
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	Transaction string `json:"transaction,omitempty"`
	Network     string `json:"network,omitempty"`
	Payer       string `json:"payer,omitempty"`
}

// x402ReasonPattern is the form of an x402 error code
// (specs/x402-specification-v1.md 9, specs/x402-specification-v2.md 9).
var x402ReasonPattern = regexp.MustCompile(`^[a-z0-9_]{1,128}$`)

// x402ReasonText is a facilitator's rejection reason as the agent may see it: an
// x402 error code passes, anything else from the facilitator stays in the log.
func x402ReasonText(reason string) string {
	if x402ReasonPattern.MatchString(reason) {
		return reason
	}
	return "rejected"
}

// x402SettlementResponse is what the payment response header carries: the
// SettlementResponse (specs/x402-specification-v1.md 5.3,
// specs/x402-specification-v2.md 5.3), as base64 of its JSON, in
// X-PAYMENT-RESPONSE for a v1 payment and PAYMENT-RESPONSE for a v2 one. Its
// network is named the way the payment's version names networks. A failed
// settlement carries its errorReason and the transaction, when there is one.
type x402SettlementResponse struct {
	Success     bool   `json:"success"`
	ErrorReason string `json:"errorReason,omitempty"`
	Transaction string `json:"transaction"`
	Network     string `json:"network"`
	Payer       string `json:"payer"`
}

// x402SettlementFor is the SettlementResponse of a settlement on one of our
// networks, in a protocol version.
func x402SettlementFor(version int, settleResponse *X402SettleResponse, network string, payer string) *x402SettlementResponse {
	wireNetwork := network
	if version == X402VersionV2 {
		if caip2, ok := x402Caip2Network(network); ok {
			wireNetwork = caip2
		}
	}
	if settleResponse.Payer != "" {
		payer = settleResponse.Payer
	}
	return &x402SettlementResponse{
		Success:     settleResponse.Success,
		ErrorReason: settleResponse.ErrorReason,
		Transaction: settleResponse.Transaction,
		Network:     wireNetwork,
		Payer:       payer,
	}
}

// x402PaymentResponseHeaderName is the header a version's settlement response
// goes in.
func x402PaymentResponseHeaderName(version int) string {
	if version == X402VersionV2 {
		return X402V2PaymentResponseHeader
	}
	return X402V1PaymentResponseHeader
}

// writeX402Settlement sets the payment response header for a settlement.
func writeX402Settlement(w http.ResponseWriter, version int, settlement *x402SettlementResponse) {
	header, err := x402Base64Json(settlement)
	if err != nil {
		return
	}
	w.Header().Set(x402PaymentResponseHeaderName(version), header)
}

// ----- purchase -----

type X402PurchaseArgs struct {
	SkuId string `json:"sku_id"`
	// Network the agent intends to pay on, as /x402/skus names it ("base").
	// Must be one we quoted. Optional when only one network is configured,
	// and for an x402 v2 payment, which names its network itself.
	Network string `json:"network,omitempty"`
	// Email to send the receipt to. Optional.
	Email string `json:"email,omitempty"`
}

type X402PurchaseResult struct {
	Complete    bool            `json:"complete"`
	SkuId       string          `json:"sku_id"`
	Pro         bool            `json:"pro"`
	ByteCount   model.ByteCount `json:"byte_count,omitempty"`
	Transaction string          `json:"transaction,omitempty"`
	// as /x402/skus names it ("base"), whichever protocol version paid
	Network string `json:"network,omitempty"`
	// the paying wallet, as the facilitator reported it
	Payer string `json:"payer,omitempty"`
	// the protocol version of the payment, which its receipt is written in
	X402Version int `json:"x402_version,omitempty"`
}

// SettlementResponse is the purchase's SettlementResponse in the version the
// payment was made in: the payment response header's content, and what the
// MCP transport puts in _meta["x402/payment-response"].
func (self *X402PurchaseResult) SettlementResponse() any {
	return x402SettlementFor(
		self.X402Version,
		&X402SettleResponse{Success: true, Transaction: self.Transaction},
		self.Network,
		self.Payer,
	)
}

// writeX402PaymentResponse sets the payment response header of a settled
// purchase.
func writeX402PaymentResponse(w http.ResponseWriter, result *X402PurchaseResult) {
	writeX402Settlement(w, result.X402Version, result.SettlementResponse().(*x402SettlementResponse))
}

// X402PaymentError is why a presented payment bought nothing, with what the
// agent is answered, per the x402 HTTP transport's error handling
// (specs/transports-v2/http.md and specs/transports-v1/http.md):
//   - 400, Invalid Payment: the payment is not a well-formed x402 payment.
//   - 402, Payment Failed: it does not match the terms, or the facilitator
//     refused it at verify or settle; the answer quotes the terms again, with
//     the reason as their error, and a failed settlement's response in the
//     payment response header.
//   - 502: the facilitator could not be asked, or answered with an error, or
//     the grant failed after the money moved (recorded for the reconciler).
//     The cause stays in the log.
type X402PaymentError struct {
	Status int
	// what the agent is told, and the v1 terms' error
	Message string
	// the x402 error code, the v2 terms' error; empty for the message
	Reason string
	// for a 402: the terms to quote again
	Terms *X402Terms
	// for a failed settlement: its response, in the payment's version
	version    int
	settlement *x402SettlementResponse
}

func (self *X402PaymentError) Error() string {
	return self.Message
}

// SettlementResponse is the failed settlement's SettlementResponse, in the
// version the payment was made in, or nil when the failure came before
// settlement. The MCP transport puts it in _meta["x402/payment-response"].
func (self *X402PaymentError) SettlementResponse() any {
	if self.settlement == nil {
		return nil
	}
	return self.settlement
}

// QuotedTerms are the terms to quote again for a failed payment, with the
// failure as their error, or nil when the failure is not answered with terms.
func (self *X402PaymentError) QuotedTerms() *X402Terms {
	if self.Status != http.StatusPaymentRequired || self.Terms == nil {
		return nil
	}
	return self.Terms.WithError(self.Message, self.Reason)
}

// writeX402PurchaseError answers a purchase that failed.
func writeX402PurchaseError(w http.ResponseWriter, err error) {
	paymentError := &X402PaymentError{}
	if !errors.As(err, &paymentError) {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	terms := paymentError.QuotedTerms()
	if terms == nil {
		http.Error(w, paymentError.Message, paymentError.Status)
		return
	}
	if paymentError.settlement != nil {
		writeX402Settlement(w, paymentError.version, paymentError.settlement)
	}
	WriteX402PaymentRequired(w, terms)
}

// The outcomes of a presented x402 payment, by protocol version, so the share
// of v1 payments is known before v1 is retired.
var x402PaymentsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "x402",
		Name:      "payments_total",
		Help:      "x402 payments presented, by protocol version and outcome: granted, malformed, unmatched, invalid (verify refused), settle_refused, pending (settlement_pending), facilitator_error, grant_failed",
	},
	[]string{"version", "result"},
)

func x402CountPayment(version int, result string) {
	label := "unknown"
	switch version {
	case X402VersionV1, X402VersionV2:
		label = strconv.Itoa(version)
	}
	x402PaymentsCounter.WithLabelValues(label, result).Inc()
}

// x402PresentedPayment is the payment a request presents, in PAYMENT-SIGNATURE
// (v2) or X-PAYMENT (v1), or "" when it presents none. A request that
// presents both is refused: which one would be settled is not the server's
// guess to make.
func x402PresentedPayment(header http.Header) (string, error) {
	v2 := strings.TrimSpace(header.Get(X402V2PaymentSignatureHeader))
	v1 := strings.TrimSpace(header.Get(X402V1PaymentHeader))
	if v2 != "" && v1 != "" {
		return "", fmt.Errorf("send one payment, in %s (x402 v2) or %s (x402 v1), not both", X402V2PaymentSignatureHeader, X402V1PaymentHeader)
	}
	if v2 != "" {
		return v2, nil
	}
	return v1, nil
}

// x402MatchPayment picks the quoted term a payment was made for, in the
// payment's protocol version, and prepares it for the facilitator. The
// payment is checked against our terms, never the other way round.
//
// network is the one the agent named (X402PurchaseArgs.Network), or "".
func x402MatchPayment(terms *X402Terms, payment *x402Payment, network string) (*x402PreparedPayment, error) {
	switch payment.version {
	case X402VersionV1:
		if terms.V1 == nil {
			return nil, fmt.Errorf("x402 v1 terms are not quoted now; pay in x402 v2")
		}
		// the agent may name the network explicitly; otherwise it is the one
		// the payment was signed for, which is where the x402 spec puts it
		if network == "" {
			network = payment.network
		}
		requirements, err := x402RequirementsForNetwork(terms.V1, network)
		if err != nil {
			return nil, err
		}
		if payment.network != requirements.Network {
			return nil, fmt.Errorf("x402 payment is for network %s, not %s", payment.network, requirements.Network)
		}
		return &x402PreparedPayment{
			version:      X402VersionV1,
			network:      requirements.Network,
			amount:       requirements.MaxAmountRequired,
			payload:      payment.v1,
			requirements: *requirements,
		}, nil
	case X402VersionV2:
		if terms.V2 == nil {
			return nil, fmt.Errorf("x402 v2 terms are not quoted now; pay in x402 v1")
		}
		if network != "" {
			if caip2, ok := x402Caip2Network(network); !ok || caip2 != payment.network {
				return nil, fmt.Errorf("x402 payment is for network %s, not %s", payment.network, network)
			}
		}
		requirements := x402MatchAcceptedV2(terms.V2.Accepts, payment.accepted)
		if requirements == nil {
			return nil, fmt.Errorf("x402 payment's accepted terms match none of the quoted terms")
		}
		ourNetwork, _ := x402NetworkForCaip2(requirements.Network)
		return &x402PreparedPayment{
			version:      X402VersionV2,
			network:      ourNetwork,
			amount:       requirements.Amount,
			payload:      payment.v2,
			requirements: *requirements,
		}, nil
	default:
		return nil, fmt.Errorf("x402 payment is for x402 version %d", payment.version)
	}
}

// x402MatchAcceptedV2 is the quoted v2 term a payment's `accepted` names, or
// nil. The rule is the reference resource server's (x402-foundation/x402
// go/server.go FindMatchingRequirements): every field but extra is equal, and
// accepted.extra holds every key of the quoted extra with the same value (a
// client may add keys, such as the default assetTransferMethod). On top of it
// the payment may not choose a transfer method or payment flow this server
// does not quote: "Resource servers MUST reject unsupported
// assetTransferMethod / payment flow combinations" (specs/x402-specification-v2.md
// 6.1). We quote EIP-3009 transfers (the EVM default) and the authorization
// flow (the default) only.
func x402MatchAcceptedV2(accepts []X402AcceptV2, accepted *X402AcceptV2) *X402AcceptV2 {
	if accepted == nil {
		return nil
	}
	for i := range accepts {
		requirements := &accepts[i]
		if requirements.Scheme != accepted.Scheme ||
			requirements.Network != accepted.Network ||
			requirements.Amount != accepted.Amount ||
			requirements.Asset != accepted.Asset ||
			requirements.PayTo != accepted.PayTo ||
			requirements.MaxTimeoutSeconds != accepted.MaxTimeoutSeconds {
			continue
		}
		if !x402ExtraContains(accepted.Extra, requirements.Extra) {
			continue
		}
		if method, ok := accepted.Extra["assetTransferMethod"]; ok && method != "eip3009" {
			continue
		}
		if flow, ok := accepted.Extra["paymentFlow"]; ok && flow != "authorization" {
			continue
		}
		return requirements
	}
	return nil
}

// x402ExtraContains reports whether extra holds every key of want with the
// same JSON value. Values are compared as JSON, since a decoded client extra
// holds float64 and []any where ours holds int and []int.
func x402ExtraContains(extra map[string]any, want map[string]any) bool {
	for key, wantValue := range want {
		value, ok := extra[key]
		if !ok {
			return false
		}
		valueJson, err := json.Marshal(value)
		if err != nil {
			return false
		}
		wantJson, err := json.Marshal(wantValue)
		if err != nil {
			return false
		}
		if string(valueJson) != string(wantJson) {
			return false
		}
	}
	return true
}

// X402Purchase settles a signed payment and grants what was bought.
//
// payment is the PAYMENT-SIGNATURE or X-PAYMENT header value (base64 of the
// PaymentPayload JSON, in either protocol version), and resource is the
// resource the terms were quoted for (x402PurchaseResource, the gated route, or
// the MCP tool). A purchase that fails returns an *X402PaymentError.
//
// It is the ONLY place an x402 payment turns into an entitlement, and it grants only
// after the facilitator confirms settlement. The grants reuse the same seams as the
// card/crypto flows, so an agent's Pro month is indistinguishable from a human's:
//   - pro_1month -> a subscription renewal + a Pro balance (pro = true) for the month
//   - data_*     -> a data balance (pro = false), valid for pro.yml data_code.duration
func X402Purchase(
	ctx context.Context,
	networkId server.Id,
	payment string,
	purchase *X402PurchaseArgs,
	resource string,
) (*X402PurchaseResult, error) {
	c := X402()
	if !c.Enabled {
		return nil, fmt.Errorf("x402 is not enabled")
	}

	sku := X402SkuById(purchase.SkuId)
	if sku == nil {
		return nil, &X402PaymentError{Status: http.StatusBadRequest, Message: fmt.Sprintf("unknown x402 sku: %s", purchase.SkuId)}
	}

	// rebuild the terms we would have quoted, so the payment is checked against OUR
	// price and OUR merchant address -- never against anything the agent supplied
	terms, err := X402TermsFor(resource, sku.SkuId, "")
	if err != nil {
		return nil, err
	}

	// the header is base64 of the signed PaymentPayload; one that is not an x402
	// exact payment never reaches the facilitator
	decoded, err := x402DecodePayment(payment)
	if err != nil {
		x402CountPayment(0, "malformed")
		return nil, &X402PaymentError{Status: http.StatusBadRequest, Message: x402MalformedText(err)}
	}

	prepared, err := x402MatchPayment(terms, decoded, purchase.Network)
	if err != nil {
		x402CountPayment(decoded.version, "unmatched")
		return nil, &X402PaymentError{
			Status:  http.StatusPaymentRequired,
			Message: err.Error(),
			Terms:   terms,
		}
	}

	verifyResponse, err := x402Verify(ctx, prepared)
	if err != nil {
		// the facilitator's error quotes its answer, which stays in the log
		glog.Errorf("[x402]verify failed network=%s sku=%s v%d: %s\n", networkId, sku.SkuId, prepared.version, err)
		x402CountPayment(prepared.version, "facilitator_error")
		return nil, &X402PaymentError{Status: http.StatusBadGateway, Message: "x402 verify failed"}
	}
	if !verifyResponse.IsValid {
		reason := x402ReasonText(verifyResponse.InvalidReason)
		x402CountPayment(prepared.version, "invalid")
		return nil, &X402PaymentError{
			Status:  http.StatusPaymentRequired,
			Message: fmt.Sprintf("x402 payment invalid: %s", reason),
			Reason:  reason,
			Terms:   terms,
		}
	}

	settleResponse, err := x402Settle(ctx, prepared)
	if err != nil {
		glog.Errorf("[x402]settle failed network=%s sku=%s v%d: %s\n", networkId, sku.SkuId, prepared.version, err)
		x402CountPayment(prepared.version, "facilitator_error")
		return nil, &X402PaymentError{Status: http.StatusBadGateway, Message: "x402 settle failed"}
	}
	// our name for the network the money is on, whichever way the
	// facilitator spelled it: the grant, receipt, sales notice and Stripe
	// record all name networks this way
	settleResponse.Network = x402NetworkName(settleResponse.Network)
	if settleResponse.Network == "" {
		settleResponse.Network = prepared.network
	}
	if settleResponse.pending() {
		x402CountPayment(prepared.version, "pending")
		x402RecordSettlementPending(ctx, networkId, sku, prepared, settleResponse)
		reason := x402SettlementPending
		return nil, &X402PaymentError{
			Status: http.StatusPaymentRequired,
			Message: fmt.Sprintf(
				"x402 settlement pending: transaction %s was broadcast on %s and is not confirmed yet. Do not sign another payment for it: if it confirms, it is granted after review; write to support@ur.io with the transaction if it does not show up",
				settleResponse.Transaction, settleResponse.Network,
			),
			Reason:     reason,
			Terms:      terms,
			version:    prepared.version,
			settlement: x402SettlementFor(prepared.version, settleResponse, settleResponse.Network, verifyResponse.Payer),
		}
	}
	if !settleResponse.Success {
		reason := x402ReasonText(settleResponse.ErrorReason)
		x402CountPayment(prepared.version, "settle_refused")
		failed := *settleResponse
		failed.ErrorReason = reason
		return nil, &X402PaymentError{
			Status:     http.StatusPaymentRequired,
			Message:    fmt.Sprintf("x402 settlement failed: %s", reason),
			Reason:     reason,
			Terms:      terms,
			version:    prepared.version,
			settlement: x402SettlementFor(prepared.version, &failed, settleResponse.Network, verifyResponse.Payer),
		}
	}

	// paid. grant it. The receipt, when one was asked for, commits with the
	// grant in the account message outbox.
	var receipt *x402Receipt
	if c.Receipt.Enabled && purchase.Email != "" {
		receipt = &x402Receipt{
			email: purchase.Email,
			asset: c.Asset,
		}
	}
	if err := x402GrantSettled(ctx, networkId, sku, settleResponse, receipt); err != nil {
		// x402GrantSettled logged the cause and recorded it for the reconciler
		x402CountPayment(prepared.version, "grant_failed")
		return nil, &X402PaymentError{Status: http.StatusBadGateway, Message: "x402 grant failed after settlement"}
	}
	x402CountPayment(prepared.version, "granted")

	// Record the settled transfer in Stripe. The money is already on chain and
	// the entitlement is already granted, so this is bookkeeping: a failure is
	// logged loudly and never propagated (see x402RecordStripePayment).
	x402RecordStripePayment(ctx, sku, &X402Accept{
		Network:           prepared.network,
		MaxAmountRequired: prepared.amount,
	}, settleResponse)

	glog.Infof(
		"[x402]granted network=%s sku=%s tx=%s network=%s v%d\n",
		networkId, sku.SkuId, settleResponse.Transaction, settleResponse.Network, prepared.version,
	)

	payer := settleResponse.Payer
	if payer == "" {
		payer = verifyResponse.Payer
	}
	return &X402PurchaseResult{
		Complete:    true,
		SkuId:       sku.SkuId,
		Pro:         sku.Pro,
		ByteCount:   sku.ByteCount,
		Transaction: settleResponse.Transaction,
		Network:     settleResponse.Network,
		Payer:       payer,
		X402Version: prepared.version,
	}, nil
}

// x402RecordSettlementPending records a settlement the facilitator broadcast
// but could not confirm, keyed by its transaction, for the operator: the money
// may still move, and nothing is granted for it. The facilitator was already
// asked twice (x402Settle). A record that cannot be written leaves the log
// line as the only trace.
func x402RecordSettlementPending(
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	prepared *x402PreparedPayment,
	settleResponse *X402SettleResponse,
) {
	glog.Errorf(
		"[x402]SETTLEMENT PENDING network=%s sku=%s tx=%s network=%s v%d: not granted; check the transaction on chain\n",
		networkId, sku.SkuId, settleResponse.Transaction, settleResponse.Network, prepared.version,
	)
	details := x402SettledNotGrantedDetails(sku, settleResponse, errors.New(x402SettlementPending))
	details["x402_version"] = prepared.version
	details["amount"] = prepared.amount
	if err := addPaymentReconciliationEvent(nil, ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     model.SubscriptionMarketX402,
		NetworkId: &networkId,
		Action:    model.PaymentReconcileActionSettlementPending,
		Evidence:  settleResponse.Transaction,
		Details:   details,
	}); err != nil {
		glog.Errorf(
			"[x402]SETTLEMENT PENDING tx=%s could not be recorded: %s\n",
			settleResponse.Transaction, err,
		)
	}
}

// x402 grant seams, so the settle->grant failure path is testable without Postgres
var (
	x402GrantProMonthFunc = x402GrantProMonthInConn
	x402GrantDataFunc     = x402GrantDataInConn
)

// x402GrantSettled grants a settled purchase. The money has already moved, so a
// grant failure is recorded durably as a settled_not_granted reconciliation event
// keyed by the settle transaction; the payment reconciler retries the grant from
// it (the grant is idempotent on that transaction), so repair no longer depends
// on someone reading the logs. A receipt, when the purchase asked for one, is
// owed with the grant (see addX402ReceiptInTx).
func x402GrantSettled(
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
) error {
	netRevenue := model.UsdToNanoCents(sku.PriceUsd)

	var err error
	if sku.Pro {
		err = x402GrantProMonthFunc(nil, ctx, networkId, sku, netRevenue, settleResponse, receipt)
	} else {
		err = x402GrantDataFunc(nil, ctx, networkId, sku, netRevenue, settleResponse, receipt)
	}
	if err != nil {
		glog.Errorf(
			"[x402]SETTLED BUT NOT GRANTED network=%s sku=%s tx=%s err=%s\n",
			networkId, sku.SkuId, settleResponse.Transaction, err,
		)
		recordErr := addPaymentReconciliationEvent(nil, ctx, &model.PaymentReconciliationEvent{
			// a fresh provenance id, like the other webhook-time events
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketX402,
			NetworkId: &networkId,
			Action:    model.PaymentReconcileActionSettledNotGranted,
			Evidence:  settleResponse.Transaction,
			Details:   x402SettledNotGrantedDetails(sku, settleResponse, err),
		})
		if recordErr != nil {
			// the log line above is now the only record
			glog.Errorf(
				"[x402]SETTLED BUT NOT GRANTED tx=%s could not be recorded for the reconciler: %s\n",
				settleResponse.Transaction, recordErr,
			)
		}
		return fmt.Errorf("x402 grant failed after settlement: %w", err)
	}
	return nil
}

// x402SettledNotGrantedDetails is what the reconciler needs to retry the grant
// exactly as bought: the price paid and the amount, not the current config.
func x402SettledNotGrantedDetails(sku *X402Sku, settleResponse *X402SettleResponse, err error) map[string]any {
	return map[string]any{
		"sku_id":     sku.SkuId,
		"pro":        sku.Pro,
		"byte_count": int64(sku.ByteCount),
		"price_usd":  sku.PriceUsd,
		"network":    settleResponse.Network,
		"error":      err.Error(),
	}
}

// x402SkuFromSettledNotGrantedDetails reverses x402SettledNotGrantedDetails
// (json numbers decode as float64).
func x402SkuFromSettledNotGrantedDetails(details map[string]any) (*X402Sku, string, error) {
	skuId, _ := details["sku_id"].(string)
	pro, proOk := details["pro"].(bool)
	byteCount, byteCountOk := details["byte_count"].(float64)
	priceUsd, priceUsdOk := details["price_usd"].(float64)
	network, _ := details["network"].(string)
	if skuId == "" || !proOk || !byteCountOk || !priceUsdOk {
		return nil, "", fmt.Errorf("settled_not_granted details are incomplete: %v", details)
	}
	return &X402Sku{
		SkuId:     skuId,
		PriceUsd:  priceUsd,
		Pro:       pro,
		ByteCount: model.ByteCount(byteCount),
	}, network, nil
}

// x402TransactionGranted is x402AlreadyGrantedForTransaction in its own read.
func x402TransactionGranted(ctx context.Context, networkId server.Id, transaction string) (granted bool) {
	return x402TransactionGrantedInConn(nil, ctx, networkId, transaction)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func x402TransactionGrantedInConn(connOwner server.PgConn, ctx context.Context, networkId server.Id, transaction string) (granted bool) {
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		granted = x402AlreadyGrantedForTransaction(ctx, tx, networkId, transaction)
	}, server.TxReadCommitted)
	return
}

// WriteX402UpgradeRequired turns an "upgrade required" auth result into a 402 with
// payment terms, so an agent that asked for them can pay for a Pro month inline
// and retry the SAME request. Returns true when it wrote the response.
//
// It is a no-op (false) when the result is not upgrade-required, when the caller
// did not ask for terms (X402QuotesClient), or when x402 is off or cannot quote --
// and then the normal JSON body with `upgrade_required` is written instead, which
// is what the apps, the browser extensions and ur.io show an upgrade prompt from.
func WriteX402UpgradeRequired(w http.ResponseWriter, r *http.Request, result *model.AuthNetworkClientResult) bool {
	if result == nil || result.Error == nil || !result.Error.UpgradeRequired {
		return false
	}
	if !X402QuotesClient(r.Header) {
		return false
	}
	if !X402Enabled() {
		return false
	}

	terms, err := X402TermsFor(
		x402AuthClientResource,
		X402SkuProMonth,
		result.Error.Message,
	)
	if err != nil {
		// cannot quote (e.g. the Pro price is not filled in) -- fall back to the
		// normal error body rather than emitting broken terms
		glog.Errorf("[x402]could not quote upgrade terms: %s\n", err)
		return false
	}

	WriteX402PaymentRequired(w, terms)
	return true
}

// X402QuotesClient reports whether a refusal that an upgrade lifts is answered
// to this caller with x402 payment terms (402) rather than the JSON body with
// upgrade_required. Only a caller that asks is quoted: it sends
// `X-UR-X402: 1` (X402QuoteHeader).
//
// x402 has no header a client sends before it holds terms, in either version:
// the reference clients send the first request unchanged and add
// PAYMENT-SIGNATURE (v2) or X-PAYMENT (v1) only to the paid retry. So the
// server cannot pick out an agent that can pay by anything but its own
// request, and choosing by who is not an agent misses clients. Until
// 2026-11-10 every caller whose client info named no app was quoted, among them
// the published browser extensions (no client info at all), which showed
// "HTTP error! status: 402" at the free plan's hosted proxy cap (support inbox
// 8244). Every client reads the JSON refusal, and an agent that did not ask can
// still buy Pro at /x402/purchase and pay on its retry here.
//
// A request that already carries a payment (settled before the auth ran) is
// never quoted again, so a refusal after a payment never asks for a second one.
func X402QuotesClient(header http.Header) bool {
	if header.Get(X402V2PaymentSignatureHeader) != "" || header.Get(X402V1PaymentHeader) != "" {
		return false
	}
	return strings.TrimSpace(header.Get(X402QuoteHeader)) == "1"
}

// x402AuthClientResource is the gated route the inline upgrade is quoted for.
const x402AuthClientResource = "/network/auth-client"

// X402SettleInlineUpgrade settles a payment presented on a gated request: the
// agent was quoted 402, signed a payment, and retried the same request with it
// in PAYMENT-SIGNATURE (v2) or X-PAYMENT (v1). Settling grants Pro, so the
// handler that runs next sees an upgraded network and simply succeeds.
//
// Returns true to continue handling the request (no payment presented, or the payment
// settled). Returns false when it has already written an error response.
func X402SettleInlineUpgrade(w http.ResponseWriter, r *http.Request) bool {
	payment, err := x402PresentedPayment(r.Header)
	if err != nil {
		if !X402Enabled() {
			http.Error(w, "x402 is not enabled.", http.StatusNotFound)
			return false
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if payment == "" {
		// nothing to settle -- the request proceeds and may be quoted a 402
		return true
	}

	if !X402Enabled() {
		http.Error(w, "x402 is not enabled.", http.StatusNotFound)
		return false
	}

	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		glog.Infof("[x402]client session error: %s\n", err)
		http.Error(w, router.UnexpectedErrorMessage, http.StatusInternalServerError)
		return false
	}
	if err := clientSession.Auth(r); err != nil {
		http.Error(w, "Not authorized.", http.StatusUnauthorized)
		return false
	}

	// nothing is verified or settled for a guest network
	// (purchase_guest_guard.go)
	if err := X402RefuseGuest(clientSession); err != nil {
		writeX402PurchaseError(w, err)
		return false
	}

	result, err := X402Purchase(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		payment,
		&X402PurchaseArgs{SkuId: X402SkuProMonth},
		x402AuthClientResource,
	)
	if err != nil {
		glog.Errorf("[x402]inline upgrade failed: %s\n", err)
		writeX402PurchaseError(w, err)
		return false
	}

	writeX402PaymentResponse(w, result)

	return true
}

// x402RequirementsForNetwork picks the quoted v1 term the agent is paying against.
func x402RequirementsForNetwork(paymentRequired *X402PaymentRequired, network string) (*X402Accept, error) {
	if network == "" {
		if len(paymentRequired.Accepts) == 1 {
			return &paymentRequired.Accepts[0], nil
		}
		return nil, fmt.Errorf("network is required; quoted: %d networks", len(paymentRequired.Accepts))
	}
	for i := range paymentRequired.Accepts {
		if paymentRequired.Accepts[i].Network == network {
			return &paymentRequired.Accepts[i], nil
		}
	}
	return nil, fmt.Errorf("network not quoted: %s", network)
}

// x402ProMonthDuration is what a pro_1month purchase buys: a ROLLING month from the
// moment of purchase, the same shape as the Solana monthly plan. It used to be
// ProGrantWindow (the CALENDAR month), so paying full price on the 28th bought ~3
// days -- and a second purchase in the same month collapsed onto the same renewal
// row and extended nothing.
const x402ProMonthDuration = 30 * 24 * time.Hour

// x402AlreadyGrantedForTransaction reports whether a settle transaction has already
// been granted, in either grant shape (a renewal row for Pro, a purchase-token
// balance for data). This is what makes settle->grant IDEMPOTENT on the settle tx:
// an agent retrying a request whose settlement succeeded (the facilitator settles
// the same signed payment to the same transaction) must not be granted twice.
func x402AlreadyGrantedForTransaction(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	transaction string,
) (granted bool) {
	if transaction == "" {
		return false
	}
	result, err := tx.Query(
		ctx,
		`
		SELECT 1 FROM subscription_renewal
		WHERE network_id = $1 AND market = $2 AND transaction_id = $3
		UNION ALL
		SELECT 1 FROM transfer_balance
		WHERE network_id = $1 AND purchase_token = $3
		LIMIT 1
		`,
		networkId,
		model.SubscriptionMarketX402,
		transaction,
	)
	server.WithPgResult(result, err, func() {
		granted = result.Next()
	})
	return
}

// x402GrantProMonth grants Pro for one ROLLING month from the purchase: a
// subscription renewal plus the Pro balance itself (pro = true), which confers the
// entitlement and holds the month's full data (pro.data). An agent buys data where
// it pays, so each purchase carries its own month's data: x402 months are not Pro
// grant cycles (model.GrantProCycleInTx leaves the x402 market out), and a second
// purchase in the same month gets its data at once.
//
// Each purchase gets its own window starting at ITS purchase time, so a second
// purchase lands a second renewal row (the renewal key includes market and the
// windows are distinct) and EXTENDS the entitlement rather than collapsing onto
// the first. The grant is idempotent on the settle transaction: a retry after a
// lost response grants nothing twice, and owes no second receipt.
func x402GrantProMonth(
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
) (returnErr error) {
	return x402GrantProMonthInConn(nil, ctx, networkId, sku, netRevenue, settleResponse, receipt)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func x402GrantProMonthInConn(connOwner server.PgConn,
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
) (returnErr error) {
	startTime := server.NowUtc()
	endTime := startTime.Add(x402ProMonthDuration + manualPaymentGracePeriod)

	granted := false
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		granted, returnErr = x402GrantProMonthInTx(tx, ctx, networkId, sku, netRevenue, settleResponse, receipt, startTime, endTime)
	}, server.TxReadCommitted)

	if returnErr != nil {
		return
	}

	if granted {
		// the upgrade takes effect immediately rather than after ProCacheTtl
		model.UpdateProNetworkInConn(connOwner, ctx, networkId)
	}

	return
}

// x402GrantProMonthInTx is one attempt of the Pro month grant, in the
// caller's ReadCommitted transaction: the settle-transaction gate, then the
// renewal, the pro balance, the receipt and the sales notice of the new
// payment.
func x402GrantProMonthInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
	startTime time.Time,
	endTime time.Time,
) (granted bool, returnErr error) {
	if err := model.LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
		return false, err
	}
	x402LockSettleTransactionInTx(ctx, tx, settleResponse.Transaction)
	if x402AlreadyGrantedForTransaction(ctx, tx, networkId, settleResponse.Transaction) {
		// this settle tx already granted -- an agent retry, not a new purchase
		glog.Infof(
			"[x402]tx %s already granted for network %s; ignoring retry\n",
			settleResponse.Transaction, networkId,
		)
		return false, nil
	}

	// the paid month, without the manual-payment grace the caller added to
	// endTime (x402GrantProMonthInConn)
	paidEndTime := endTime.Add(-manualPaymentGracePeriod)
	err := model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         netRevenue,
		SubscriptionMarket: model.SubscriptionMarketX402,
		TransactionId:      settleResponse.Transaction,
		PaidEndTime:        &paidEndTime,
	})
	if err != nil {
		return false, err
	}

	// the month's Pro balance carries its own data, the full amount
	returnErr = model.AddProTransferBalanceInTx(
		tx,
		ctx,
		networkId,
		sku.ByteCount,
		startTime,
		endTime,
	)
	if returnErr != nil {
		return false, returnErr
	}
	addX402ReceiptInTx(ctx, tx, networkId, receipt, sku, settleResponse)
	// the settle transaction had no grant: a new payment, posted once the
	// transaction commits
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		return x402SalesNotice(networkId, sku, settleResponse)
	})
	return true, nil
}

// x402LockSettleTransactionInTx serializes the grants of one settle
// transaction until the caller's transaction ends. The network lock is a
// shared lock, so without this two concurrent grants of the same settle
// transaction (an agent retry on another api instance, or the reconciler)
// could both pass the already-granted check. Under ReadCommitted the check
// after the lock sees the first grant once it commits.
func x402LockSettleTransactionInTx(ctx context.Context, tx server.PgTx, transaction string) {
	if transaction == "" {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"x402_settle/"+transaction,
	))
}

// x402GrantData adds a data balance. pro = false: buying data never grants Pro. The
// balance is valid for pro.yml data_code.duration (1 year), the same as a data code.
// Idempotent on the settle transaction (carried as the balance's purchase token),
// so an agent retry of a settled purchase does not credit twice or owe a second
// receipt.
func x402GrantData(
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
) error {
	return x402GrantDataInConn(nil, ctx, networkId, sku, netRevenue, settleResponse, receipt)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func x402GrantDataInConn(connOwner server.PgConn,
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
) error {
	startTime := server.NowUtc()
	endTime := startTime.Add(model.Pro().DataCodeDuration)

	var returnErr error
	server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
		_, returnErr = x402GrantDataInTx(tx, ctx, networkId, sku, netRevenue, settleResponse, receipt, startTime, endTime)
	}, server.TxReadCommitted)

	return returnErr
}

// x402GrantDataInTx is one attempt of the data grant, in the caller's
// ReadCommitted transaction: the settle-transaction gate, then the data
// balance, the receipt and the sales notice of the new payment.
func x402GrantDataInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
	receipt *x402Receipt,
	startTime time.Time,
	endTime time.Time,
) (granted bool, returnErr error) {
	if err := model.LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
		return false, err
	}
	x402LockSettleTransactionInTx(ctx, tx, settleResponse.Transaction)
	if x402AlreadyGrantedForTransaction(ctx, tx, networkId, settleResponse.Transaction) {
		glog.Infof(
			"[x402]tx %s already granted for network %s; ignoring retry\n",
			settleResponse.Transaction, networkId,
		)
		return false, nil
	}

	model.AddTransferBalanceInTx(ctx, tx, &model.TransferBalance{
		NetworkId:             networkId,
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: sku.ByteCount,
		BalanceByteCount:      sku.ByteCount,
		NetRevenue:            netRevenue,
		PurchaseToken:         settleResponse.Transaction,
	})
	addX402ReceiptInTx(ctx, tx, networkId, receipt, sku, settleResponse)
	// the settle transaction had no grant: a new payment, posted once the
	// transaction commits
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		return x402SalesNotice(networkId, sku, settleResponse)
	})
	return true, nil
}

// The longest receipt address the account message outbox stores.
const x402ReceiptEmailMaxLength = 256

// The receipt a purchase asked for: where to send it, and the asset it was
// paid in (from the x402 config the purchase was checked against).
type x402Receipt struct {
	email string
	asset string
}

// Counts x402 receipts not owed because the purchase's email is not one an
// account message can go to.
var x402ReceiptRefusedCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "x402",
		Name:      "receipts_refused_total",
		Help:      "x402 receipts skipped because the purchase's email is not a deliverable address",
	},
)

// Registers the x402 metrics with the default registry.
func init() {
	prometheus.MustRegister(x402ReceiptRefusedCounter)
	prometheus.MustRegister(x402PaymentsCounter)
}

// Adds the receipt for a granted purchase to the account message outbox, in the
// grant's transaction, through the normal account message template system (see
// X402ReceiptTemplate in aws_controller). One receipt per settle transaction.
//
// Only when the caller supplied an email (a nil receipt is none). The receipt
// must never fail the purchase: the email is the agent's input, so one that is
// not an address an account message can go to, or does not fit the outbox, is
// counted and skipped, never written. The delivery task retries a failed send
// after the commit.
func addX402ReceiptInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	receipt *x402Receipt,
	sku *X402Sku,
	settleResponse *X402SettleResponse,
) {
	if receipt == nil {
		return
	}
	normalEmail, userAuthType := model.NormalUserAuth(receipt.email)
	if userAuthType != model.UserAuthTypeEmail || x402ReceiptEmailMaxLength < len(normalEmail) {
		// agent input: counted, and detailed only at verbose level
		x402ReceiptRefusedCounter.Inc()
		if glog.V(1) {
			glog.Infof("[x402]no receipt for tx %s: the email is not a deliverable address\n", settleResponse.Transaction)
		}
		return
	}

	network := settleResponse.Network
	if network == "" {
		network = "chain"
	}

	addAccountMessageInTx(ctx, tx, &accountMessage{
		key:       settleResponse.Transaction,
		networkId: &networkId,
		userAuth:  normalEmail,
		template: &X402ReceiptTemplate{
			Description:      sku.Description,
			PriceUsd:         sku.PriceUsd,
			Asset:            receipt.asset,
			Network:          network,
			Transaction:      settleResponse.Transaction,
			Pro:              sku.Pro,
			BalanceByteCount: sku.ByteCount,
		},
	})
}

// x402PurchaseResource is the resource the terms are bound to.
const x402PurchaseResource = "/x402/purchase"

// X402PurchaseHandler is the raw HTTP entry point, because x402 is a header protocol:
// the signed payment arrives in a header and the "not paid yet" answer is a 402
// status with terms in a header (v2) and the body (v1).
//
// No payment                         -> 402 + terms for the requested sku
// PAYMENT-SIGNATURE or X-PAYMENT     -> verify, settle, grant, 200 + receipt header
func X402PurchaseHandler(w http.ResponseWriter, r *http.Request) {
	if !X402Enabled() {
		http.Error(w, "x402 is not enabled.", http.StatusNotFound)
		return
	}

	clientSession, err := session.NewClientSessionFromRequest(r)
	if err != nil {
		glog.Infof("[x402]client session error: %s\n", err)
		http.Error(w, router.UnexpectedErrorMessage, http.StatusInternalServerError)
		return
	}
	if err := clientSession.Auth(r); err != nil {
		http.Error(w, "Not authorized.", http.StatusUnauthorized)
		return
	}

	purchase := &X402PurchaseArgs{}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Could not read body.", http.StatusBadRequest)
		return
	}
	if 0 < len(body) {
		if err := json.Unmarshal(body, purchase); err != nil {
			http.Error(w, "Could not parse body.", http.StatusBadRequest)
			return
		}
	}
	if purchase.SkuId == "" {
		http.Error(w, "sku_id is required.", http.StatusBadRequest)
		return
	}

	payment, err := x402PresentedPayment(r.Header)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payment == "" {
		// not paid yet: quote the terms
		terms, err := X402TermsFor(
			x402PurchaseResource,
			purchase.SkuId,
			fmt.Sprintf("payment required for %s", purchase.SkuId),
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		WriteX402PaymentRequired(w, terms)
		return
	}

	// nothing is verified or settled for a guest network
	// (purchase_guest_guard.go)
	if err := X402RefuseGuest(clientSession); err != nil {
		writeX402PurchaseError(w, err)
		return
	}

	result, err := X402Purchase(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		payment,
		purchase,
		x402PurchaseResource,
	)
	if err != nil {
		glog.Errorf("[x402]purchase failed: %s\n", err)
		writeX402PurchaseError(w, err)
		return
	}

	// paid and granted by now, so the answer is 200 whatever else happens
	writeX402PaymentResponse(w, result)
	w.Header().Set("Content-Type", "application/json")
	if body, err := json.Marshal(result); err == nil {
		w.Write(body)
	}
}

// X402SkusResult is the public catalog an agent can buy from.
type X402SkusResult struct {
	// the newest protocol version this server speaks
	X402Version int `json:"x402_version"`
	// every version it speaks, newest first
	X402Versions []int `json:"x402_versions"`
	// the networks, as x402 v1 and X402PurchaseArgs name them
	Networks []string `json:"networks"`
	// each network's x402 v2 (CAIP-2) id
	NetworkIds map[string]string `json:"network_ids"`
	Asset      string            `json:"asset"`
	Skus       []*X402Sku        `json:"skus"`
}

// X402SkusHandler lists what an agent can buy and for how much, so a skill can
// discover the catalog without triggering a 402.
func X402SkusHandler(w http.ResponseWriter, r *http.Request) {
	if !X402Enabled() {
		http.Error(w, "x402 is not enabled.", http.StatusNotFound)
		return
	}

	c := X402()
	networkIds := map[string]string{}
	for _, network := range c.Networks {
		if caip2, ok := x402Caip2Network(network); ok {
			networkIds[network] = caip2
		}
	}
	body, err := json.Marshal(&X402SkusResult{
		X402Version:  X402Versions[0],
		X402Versions: X402Versions,
		Networks:     c.Networks,
		NetworkIds:   networkIds,
		Asset:        c.Asset,
		Skus:         X402Skus(),
	})
	if err != nil {
		http.Error(w, "Could not encode skus.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
