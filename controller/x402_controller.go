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
// server answers 402 with machine-readable payment terms (X402PaymentRequired). The
// agent signs a payment for one of the quoted terms and retries the SAME request
// with an `X-PAYMENT` header. The server verifies and settles that payment through
// the facilitator, then fulfills. No human in the loop.
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

const X402Version = 1

// X402 sku ids. Amounts come from pro.yml; only the prices live in x402.yml.
const (
	X402SkuProMonth  = "pro_1month"
	X402SkuData1Tib  = "data_1tib"
	X402SkuData10Tib = "data_10tib"
)

// UsdcDecimals is the atomic-unit scale for USDC. Terms quote integer atomic units,
// so $5.00 is "5000000".
const UsdcDecimals = 6

// X402PaymentHeader is the header an agent retries with, carrying the signed payment.
const X402PaymentHeader = "X-PAYMENT"

// X402PaymentResponseHeader carries the settlement receipt back to the agent.
const X402PaymentResponseHeader = "X-PAYMENT-RESPONSE"

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
	PayTo    map[string]string `yaml:"pay_to"`
	Networks []string          `yaml:"networks"`
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

// X402PaymentRequired is the 402 body: what the agent must pay to proceed.
type X402PaymentRequired struct {
	X402Version int          `json:"x402Version"`
	Error       string       `json:"error"`
	Accepts     []X402Accept `json:"accepts"`
}

// X402Sku is a thing an agent can buy, with availability from x402.yml and price and
// amount from pro.yml.
type X402Sku struct {
	SkuId       string  `json:"sku_id"`
	Description string  `json:"description"`
	PriceUsd    float64 `json:"price_usd"`
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

	// x402.yml declares WHICH skus are offered; pro.yml is the only place a PRICE lives.
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
				Pro:         true,
				ByteCount:   byteCount,
			})
		}
	}

	// the data skus mirror pro.yml data_code.skus (1 TiB, 10 TiB). Kept as an ordered
	// slice, not a map, so the catalog we serve is stable across calls.
	for _, dataSku := range []struct {
		SkuId     string
		ByteCount model.ByteCount
	}{
		{X402SkuData1Tib, 1 * model.Tib},
		{X402SkuData10Tib, 10 * model.Tib},
	} {
		if _, ok := c.Skus[dataSku.SkuId]; ok {
			// the price comes from pro.yml data_code.skus -- the same number the site
			// quotes and the same number Stripe charges
			priceUsd, ok := proDataCodePriceUsd(dataSku.ByteCount)
			if !ok || priceUsd <= 0 {
				glog.Errorf("[x402]no price in pro.yml for %s; not offering it\n", dataSku.SkuId)
				continue
			}
			skus = append(skus, &X402Sku{
				SkuId:       dataSku.SkuId,
				Description: fmt.Sprintf("URnetwork data, %s", model.ByteCountHumanReadable(dataSku.ByteCount)),
				PriceUsd:    priceUsd,
				Pro:         false,
				ByteCount:   dataSku.ByteCount,
			})
		}
	}

	return skus
}

// proDataCodePriceUsd is the price of a data amount, from pro.yml. One source of truth,
// shared by the site, Stripe checkout and x402.
func proDataCodePriceUsd(byteCount model.ByteCount) (float64, bool) {
	for _, sku := range model.Pro().DataCodeSkus {
		if sku.Data == byteCount {
			return sku.PriceUsd, true
		}
	}
	return 0, false
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

// X402PaymentRequiredFor builds the 402 body for a sku: one term per supported
// network, all settling to our merchant address.
//
// Returns an error when the sku is unknown or its price exceeds max_payment_usd --
// we would rather refuse to quote than quote something wrong.
func X402PaymentRequiredFor(resource string, skuId string, reason string) (*X402PaymentRequired, error) {
	return x402PaymentRequiredForConfig(X402(), resource, skuId, reason)
}

func x402PaymentRequiredForConfig(
	c *X402Config,
	resource string,
	skuId string,
	reason string,
) (*X402PaymentRequired, error) {
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

	maxAmountRequired := usdToAtomic(sku.PriceUsd)

	accepts := []X402Accept{}
	for _, network := range c.Networks {
		token, err := x402TokenFor(network, c.Asset)
		if err != nil {
			// The loader refuses such a config (x402ConfigProblem). One set
			// some other way is refused here rather than quoted a guess.
			return nil, err
		}
		extra := map[string]any{}
		if token.eip712Name != "" {
			extra["name"] = token.eip712Name
			extra["version"] = token.eip712Version
		}
		if token.feePayerRequired {
			feePayer, ok := x402FeePayer(c, network)
			if !ok {
				// no client can build this payment without the fee payer, so
				// this network sits out until the facilitator names one
				continue
			}
			extra["feePayer"] = feePayer
		}
		accepts = append(accepts, X402Accept{
			Scheme:            "exact",
			Network:           network,
			MaxAmountRequired: maxAmountRequired,
			Resource:          resourceUrl,
			Description:       sku.Description,
			MimeType:          "application/json",
			PayTo:             c.PayTo[network],
			Asset:             token.address,
			MaxTimeoutSeconds: 300,
			Extra:             extra,
		})
	}
	if len(accepts) == 0 {
		return nil, fmt.Errorf("x402 has no network it can quote now")
	}

	if reason == "" {
		reason = "payment required"
	}

	return &X402PaymentRequired{
		X402Version: X402Version,
		Error:       reason,
		Accepts:     accepts,
	}, nil
}

// x402ResourceUrl is the `resource` the terms quote. x402 v1 requires a url there
// (specs/x402-specification-v1.md 5.1.2), and v1 clients check that it is one
// before they pay: the legacy fetch and axios clients parse each term with a zod
// schema whose resource is z.string().url(), which refuses a bare path. A path is
// served by this API, so it gets the API's public origin, https://api.<WARP_DOMAIN>,
// the same origin the other services reach the API on. A resource that is already
// a url (mcp:fetch) is quoted as it is.
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

// WriteX402PaymentRequired writes a 402 response with payment terms. This is the one
// place a 402 body is produced, so every gated endpoint quotes terms the same way.
func WriteX402PaymentRequired(w http.ResponseWriter, paymentRequired *X402PaymentRequired) {
	body, err := json.Marshal(paymentRequired)
	if err != nil {
		http.Error(w, "Could not encode payment terms.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write(body)
}

// ----- facilitator (verify + settle) -----

// x402FacilitatorRequest is what we send to /verify and /settle: the agent's signed
// payment plus the exact terms we quoted, so the facilitator can check the payment
// matches what we asked for (specs/x402-specification-v1.md 7.1). The payment is the
// decoded PaymentPayload object, not the X-PAYMENT header string.
type x402FacilitatorRequest struct {
	X402Version         int                 `json:"x402Version"`
	PaymentPayload      *x402PaymentPayload `json:"paymentPayload"`
	PaymentRequirements X402Accept          `json:"paymentRequirements"`
}

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

// X402Verify asks the facilitator whether a signed payment (an X-PAYMENT header) is
// valid for the terms we quoted. Verify BEFORE settling so a bad payment costs
// nothing. A payment the facilitator rejects is an answer (IsValid false, with its
// reason), not an error.
func X402Verify(ctx context.Context, payment string, requirements X402Accept) (*X402VerifyResponse, error) {
	if !x402FacilitatorConfigured(X402()) {
		return nil, errX402FacilitatorNotConfigured
	}
	paymentPayload, err := x402DecodePaymentFor(payment, requirements)
	if err != nil {
		return nil, err
	}
	return x402Verify(ctx, paymentPayload, requirements)
}

func x402Verify(ctx context.Context, paymentPayload *x402PaymentPayload, requirements X402Accept) (*X402VerifyResponse, error) {
	verifyResponse := &X402VerifyResponse{}
	err := x402FacilitatorPost(ctx, "/verify", &x402FacilitatorRequest{
		X402Version:         X402Version,
		PaymentPayload:      paymentPayload,
		PaymentRequirements: requirements,
	}, verifyResponse)
	if err != nil {
		return nil, err
	}
	return verifyResponse, nil
}

// X402Settle moves the funds. Only call this after X402Verify succeeds, and only
// grant the entitlement after this succeeds. A settlement the facilitator rejects is
// an answer (Success false, with its reason), not an error.
func X402Settle(ctx context.Context, payment string, requirements X402Accept) (*X402SettleResponse, error) {
	if !x402FacilitatorConfigured(X402()) {
		return nil, errX402FacilitatorNotConfigured
	}
	paymentPayload, err := x402DecodePaymentFor(payment, requirements)
	if err != nil {
		return nil, err
	}
	return x402Settle(ctx, paymentPayload, requirements)
}

func x402Settle(ctx context.Context, paymentPayload *x402PaymentPayload, requirements X402Accept) (*X402SettleResponse, error) {
	settleResponse := &X402SettleResponse{}
	err := x402FacilitatorPost(ctx, "/settle", &x402FacilitatorRequest{
		X402Version:         X402Version,
		PaymentPayload:      paymentPayload,
		PaymentRequirements: requirements,
	}, settleResponse)
	if err != nil {
		return nil, err
	}
	return settleResponse, nil
}

// x402ReasonPattern is the form of an x402 error code (specs/x402-specification-v1.md 9).
var x402ReasonPattern = regexp.MustCompile(`^[a-z0-9_]{1,128}$`)

// x402ReasonText is a facilitator's rejection reason as the agent may see it: an
// x402 error code passes, anything else from the facilitator stays in the log.
func x402ReasonText(reason string) string {
	if x402ReasonPattern.MatchString(reason) {
		return reason
	}
	return "rejected"
}

// x402SettlementResponse is what X-PAYMENT-RESPONSE carries after a payment settles:
// the x402 v1 SettlementResponse (specs/x402-specification-v1.md 5.3), as base64 of
// its JSON (specs/transports-v1/http.md, "Settlement Response Delivery").
type x402SettlementResponse struct {
	Success     bool   `json:"success"`
	Transaction string `json:"transaction"`
	Network     string `json:"network"`
	Payer       string `json:"payer"`
}

// x402PaymentResponseHeader is the X-PAYMENT-RESPONSE value for a settled purchase.
func x402PaymentResponseHeader(result *X402PurchaseResult) string {
	settlement, err := json.Marshal(&x402SettlementResponse{
		Success:     true,
		Transaction: result.Transaction,
		Network:     result.Network,
		Payer:       result.Payer,
	})
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(settlement)
}

// ----- purchase -----

type X402PurchaseArgs struct {
	SkuId string `json:"sku_id"`
	// Network the agent intends to pay on. Must be one we quoted. Optional when only
	// one network is configured.
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
	Network     string          `json:"network,omitempty"`
	// the paying wallet, as the facilitator reported it
	Payer string `json:"payer,omitempty"`
}

// X402Purchase settles a signed payment and grants what was bought.
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
) (*X402PurchaseResult, error) {
	c := X402()
	if !c.Enabled {
		return nil, fmt.Errorf("x402 is not enabled")
	}

	sku := X402SkuById(purchase.SkuId)
	if sku == nil {
		return nil, fmt.Errorf("unknown x402 sku: %s", purchase.SkuId)
	}

	// rebuild the terms we would have quoted, so the payment is checked against OUR
	// price and OUR merchant address -- never against anything the agent supplied
	paymentRequired, err := X402PaymentRequiredFor(x402PurchaseResource, sku.SkuId, "")
	if err != nil {
		return nil, err
	}

	// the X-PAYMENT header is base64 of the signed PaymentPayload; one that is not an
	// x402 v1 exact payment never reaches the facilitator
	paymentPayload, err := x402DecodePayment(payment)
	if err != nil {
		return nil, err
	}

	// the agent may name the network explicitly; otherwise it is the one the payment
	// was signed for, which is where the x402 spec puts it
	network := purchase.Network
	if network == "" {
		network = paymentPayload.Network
	}

	requirements, err := x402RequirementsForNetwork(paymentRequired, network)
	if err != nil {
		return nil, err
	}
	if paymentPayload.Network != requirements.Network {
		return nil, fmt.Errorf("x402 payment is for network %s, not %s", paymentPayload.Network, requirements.Network)
	}

	verifyResponse, err := x402Verify(ctx, paymentPayload, *requirements)
	if err != nil {
		// the facilitator's error quotes its answer, which stays in the log
		glog.Errorf("[x402]verify failed network=%s sku=%s: %s\n", networkId, sku.SkuId, err)
		return nil, errors.New("x402 verify failed")
	}
	if !verifyResponse.IsValid {
		return nil, fmt.Errorf("x402 payment invalid: %s", x402ReasonText(verifyResponse.InvalidReason))
	}

	settleResponse, err := x402Settle(ctx, paymentPayload, *requirements)
	if err != nil {
		glog.Errorf("[x402]settle failed network=%s sku=%s: %s\n", networkId, sku.SkuId, err)
		return nil, errors.New("x402 settle failed")
	}
	if !settleResponse.Success {
		return nil, fmt.Errorf("x402 settlement failed: %s", x402ReasonText(settleResponse.ErrorReason))
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
		return nil, errors.New("x402 grant failed after settlement")
	}

	// Record the settled transfer in Stripe. The money is already on chain and
	// the entitlement is already granted, so this is bookkeeping: a failure is
	// logged loudly and never propagated (see x402RecordStripePayment).
	x402RecordStripePayment(ctx, sku, requirements, settleResponse)

	glog.Infof(
		"[x402]granted network=%s sku=%s tx=%s network=%s\n",
		networkId, sku.SkuId, settleResponse.Transaction, settleResponse.Network,
	)

	settledNetwork := settleResponse.Network
	if settledNetwork == "" {
		settledNetwork = requirements.Network
	}
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
		Network:     settledNetwork,
		Payer:       payer,
	}, nil
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
// payment terms, so an agent can pay for a Pro month inline and retry the SAME
// request. Returns true when it wrote the response.
//
// It is a no-op (false) when the result is not upgrade-required, or when x402 is off
// or cannot quote -- and then the normal JSON body with `upgrade_required` is written
// instead, which is what a human client shows an upgrade prompt from.
func WriteX402UpgradeRequired(w http.ResponseWriter, result *model.AuthNetworkClientResult) bool {
	if result == nil || result.Error == nil || !result.Error.UpgradeRequired {
		return false
	}
	if !X402Enabled() {
		return false
	}

	paymentRequired, err := X402PaymentRequiredFor(
		"/network/auth-client",
		X402SkuProMonth,
		result.Error.Message,
	)
	if err != nil {
		// cannot quote (e.g. the Pro price is not filled in) -- fall back to the
		// normal error body rather than emitting broken terms
		glog.Errorf("[x402]could not quote upgrade terms: %s\n", err)
		return false
	}

	WriteX402PaymentRequired(w, paymentRequired)
	return true
}

// X402SettleInlineUpgrade settles an X-PAYMENT presented on a gated request: the
// agent was quoted 402, signed a payment, and retried the same request. Settling
// grants Pro, so the handler that runs next sees an upgraded network and simply
// succeeds.
//
// Returns true to continue handling the request (no payment presented, or the payment
// settled). Returns false when it has already written an error response.
func X402SettleInlineUpgrade(w http.ResponseWriter, r *http.Request) bool {
	payment := r.Header.Get(X402PaymentHeader)
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

	result, err := X402Purchase(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		payment,
		&X402PurchaseArgs{SkuId: X402SkuProMonth},
	)
	if err != nil {
		glog.Errorf("[x402]inline upgrade failed: %s\n", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return false
	}

	w.Header().Set(X402PaymentResponseHeader, x402PaymentResponseHeader(result))

	return true
}

// x402RequirementsForNetwork picks the quoted term the agent is paying against.
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
// subscription renewal (so the monthly Pro grant task keeps renewing the data
// allowance) plus the Pro balance itself (pro = true), which is what actually
// confers the entitlement.
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

	err := model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
		NetworkId:          networkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         netRevenue,
		SubscriptionMarket: model.SubscriptionMarketX402,
		TransactionId:      settleResponse.Transaction,
	})
	if err != nil {
		return false, err
	}

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

// Registers the receipt metric with the default registry.
func init() {
	prometheus.MustRegister(x402ReceiptRefusedCounter)
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
// the signed payment arrives in `X-PAYMENT` and the "not paid yet" answer is a 402
// status with terms in the body.
//
// Without X-PAYMENT  -> 402 + terms for the requested sku
// With    X-PAYMENT  -> verify, settle, grant, 200 + receipt header
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

	payment := r.Header.Get(X402PaymentHeader)

	if payment == "" {
		// not paid yet: quote the terms
		paymentRequired, err := X402PaymentRequiredFor(
			x402PurchaseResource,
			purchase.SkuId,
			fmt.Sprintf("payment required for %s", purchase.SkuId),
		)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		WriteX402PaymentRequired(w, paymentRequired)
		return
	}

	result, err := X402Purchase(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		payment,
		purchase,
	)
	if err != nil {
		glog.Errorf("[x402]purchase failed: %s\n", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// paid and granted by now, so the answer is 200 whatever else happens
	w.Header().Set(X402PaymentResponseHeader, x402PaymentResponseHeader(result))
	w.Header().Set("Content-Type", "application/json")
	if body, err := json.Marshal(result); err == nil {
		w.Write(body)
	}
}

// X402SkusHandler lists what an agent can buy and for how much, so a skill can
// discover the catalog without triggering a 402.
func X402SkusHandler(w http.ResponseWriter, r *http.Request) {
	if !X402Enabled() {
		http.Error(w, "x402 is not enabled.", http.StatusNotFound)
		return
	}

	body, err := json.Marshal(map[string]any{
		"x402_version": X402Version,
		"networks":     X402().Networks,
		"asset":        X402().Asset,
		"skus":         X402Skus(),
	})
	if err != nil {
		http.Error(w, "Could not encode skus.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
