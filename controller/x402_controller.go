package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
// We settle through Stripe's x402 facilitator, which keeps us CHAIN-NEUTRAL: we
// quote one term per supported network and the agent pays on whichever chain it
// holds funds. See vault/<env>/x402.yml for the config (facilitator, pay-to,
// networks, prices).
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
	Url    string `yaml:"url"`
	ApiKey string `yaml:"api_key"`
}

type X402Config struct {
	Enabled       bool                     `yaml:"enabled"`
	Facilitator   x402FacilitatorConfig    `yaml:"facilitator"`
	PayTo         string                   `yaml:"pay_to"`
	Networks      []string                 `yaml:"networks"`
	Asset         string                   `yaml:"asset"`
	Skus          map[string]x402SkuConfig `yaml:"skus"`
	MaxPaymentUsd float64                  `yaml:"max_payment_usd"`
	Receipt       x402ReceiptConfig        `yaml:"receipt"`
}

// x402ConfigUsable reports whether a config is complete enough to actually take
// money. Being HALF configured is worse than being off: a blank pay_to would quote
// terms that settle nowhere, and a blank facilitator would accept payments we never
// verify. So anything missing means x402 stays off.
func x402ConfigUsable(c *X402Config) bool {
	if !c.Enabled {
		return false
	}
	if c.Facilitator.Url == "" || c.Facilitator.ApiKey == "" || c.PayTo == "" {
		return false
	}
	if len(c.Networks) == 0 {
		return false
	}
	return true
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

	if c.Enabled && !x402ConfigUsable(c) {
		glog.Errorf(
			"[x402]enabled but not fully configured " +
				"(facilitator.url, facilitator.api_key, pay_to, networks); x402 is off\n",
		)
		c.Enabled = false
	}

	return c
})

func X402() *X402Config {
	return x402Config()
}

// X402Enabled reports whether the x402 flow is live. Every x402 entry point checks
// this first, so the feature is a no-op until the config is filled in.
func X402Enabled() bool {
	return X402().Enabled
}

// ----- payment terms -----

// X402Accept is one quoted way to pay: a scheme+network+amount+destination. We quote
// one per supported network and let the agent choose.
type X402Accept struct {
	Scheme            string `json:"scheme"`
	Network           string `json:"network"`
	MaxAmountRequired string `json:"maxAmountRequired"`
	Resource          string `json:"resource"`
	Description       string `json:"description"`
	MimeType          string `json:"mimeType"`
	PayTo             string `json:"payTo"`
	Asset             string `json:"asset"`
	MaxTimeoutSeconds int    `json:"maxTimeoutSeconds"`
}

// X402PaymentRequired is the 402 body: what the agent must pay to proceed.
type X402PaymentRequired struct {
	X402Version int          `json:"x402Version"`
	Error       string       `json:"error"`
	Accepts     []X402Accept `json:"accepts"`
}

// X402Sku is a thing an agent can buy, resolved from pro.yml (amount) + x402.yml
// (price).
type X402Sku struct {
	SkuId       string  `json:"sku_id"`
	Description string  `json:"description"`
	PriceUsd    float64 `json:"price_usd"`
	// Pro is true for the Pro-month sku: it grants the Pro entitlement. The data
	// skus grant data only and never confer Pro.
	Pro       bool            `json:"pro"`
	ByteCount model.ByteCount `json:"byte_count,omitempty"`
}

// X402Skus is everything purchasable over x402, with amounts from pro.yml so the
// product spec stays in one place.
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

	maxAmountRequired := usdToAtomic(sku.PriceUsd)

	accepts := []X402Accept{}
	for _, network := range c.Networks {
		accepts = append(accepts, X402Accept{
			Scheme:            "exact",
			Network:           network,
			MaxAmountRequired: maxAmountRequired,
			Resource:          resource,
			Description:       sku.Description,
			MimeType:          "application/json",
			PayTo:             c.PayTo,
			Asset:             c.Asset,
			MaxTimeoutSeconds: 300,
		})
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
// matches what we asked for.
type x402FacilitatorRequest struct {
	X402Version         int        `json:"x402Version"`
	PaymentPayload      string     `json:"paymentPayload"`
	PaymentRequirements X402Accept `json:"paymentRequirements"`
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

func x402FacilitatorPost(ctx context.Context, path string, request any, response any) error {
	c := X402()

	// Refuse before any http. This is unreachable today -- every entry point is behind
	// X402Enabled(), and the loader forces enabled = false when these are blank -- but
	// this is the function that VERIFIES AND SETTLES MONEY, so it does not get to rely on
	// a caller upstream having checked. With a blank url the request would otherwise go
	// out as a relative "/settle" and come back "unsupported protocol scheme", which is a
	// confusing way to learn that a payment was never actually settled.
	if c.Facilitator.Url == "" || c.Facilitator.ApiKey == "" {
		return fmt.Errorf("x402 facilitator is not configured (facilitator.url, facilitator.api_key)")
	}

	body, err := json.Marshal(request)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		"POST",
		fmt.Sprintf("%s%s", c.Facilitator.Url, path),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.Facilitator.ApiKey))

	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || 300 <= res.StatusCode {
		return fmt.Errorf("facilitator %s: %d %s", path, res.StatusCode, string(resBody))
	}

	return json.Unmarshal(resBody, response)
}

// X402Verify asks the facilitator whether a signed payment is valid for the terms we
// quoted. Verify BEFORE settling so a bad payment costs nothing.
func X402Verify(ctx context.Context, payment string, requirements X402Accept) (*X402VerifyResponse, error) {
	verifyResponse := &X402VerifyResponse{}
	err := x402FacilitatorPost(ctx, "/verify", &x402FacilitatorRequest{
		X402Version:         X402Version,
		PaymentPayload:      payment,
		PaymentRequirements: requirements,
	}, verifyResponse)
	if err != nil {
		return nil, err
	}
	return verifyResponse, nil
}

// X402Settle moves the funds. Only call this after X402Verify succeeds, and only
// grant the entitlement after this succeeds.
func X402Settle(ctx context.Context, payment string, requirements X402Accept) (*X402SettleResponse, error) {
	settleResponse := &X402SettleResponse{}
	err := x402FacilitatorPost(ctx, "/settle", &x402FacilitatorRequest{
		X402Version:         X402Version,
		PaymentPayload:      payment,
		PaymentRequirements: requirements,
	}, settleResponse)
	if err != nil {
		return nil, err
	}
	return settleResponse, nil
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

	// the agent may name the network explicitly; otherwise read it out of the signed
	// payload, which is where the x402 spec puts it
	network := purchase.Network
	if network == "" {
		network = x402PaymentNetwork(payment)
	}

	requirements, err := x402RequirementsForNetwork(paymentRequired, network)
	if err != nil {
		return nil, err
	}

	verifyResponse, err := X402Verify(ctx, payment, *requirements)
	if err != nil {
		return nil, fmt.Errorf("x402 verify failed: %w", err)
	}
	if !verifyResponse.IsValid {
		return nil, fmt.Errorf("x402 payment invalid: %s", verifyResponse.InvalidReason)
	}

	settleResponse, err := X402Settle(ctx, payment, *requirements)
	if err != nil {
		return nil, fmt.Errorf("x402 settle failed: %w", err)
	}
	if !settleResponse.Success {
		return nil, fmt.Errorf("x402 settlement failed: %s", settleResponse.ErrorReason)
	}

	// paid. grant it.
	netRevenue := model.UsdToNanoCents(sku.PriceUsd)

	if sku.Pro {
		err = x402GrantProMonth(ctx, networkId, sku, netRevenue, settleResponse)
	} else {
		err = x402GrantData(ctx, networkId, sku, netRevenue, settleResponse)
	}
	if err != nil {
		// The money moved but the grant did not. Loudly: this needs manual repair,
		// and the transaction id is the thread to pull.
		glog.Errorf(
			"[x402]SETTLED BUT NOT GRANTED network=%s sku=%s tx=%s err=%s\n",
			networkId, sku.SkuId, settleResponse.Transaction, err,
		)
		return nil, fmt.Errorf("x402 grant failed after settlement: %w", err)
	}

	if c.Receipt.Enabled && purchase.Email != "" {
		x402SendReceipt(ctx, purchase.Email, sku, settleResponse)
	}

	glog.Infof(
		"[x402]granted network=%s sku=%s tx=%s network=%s\n",
		networkId, sku.SkuId, settleResponse.Transaction, settleResponse.Network,
	)

	return &X402PurchaseResult{
		Complete:    true,
		SkuId:       sku.SkuId,
		Pro:         sku.Pro,
		ByteCount:   sku.ByteCount,
		Transaction: settleResponse.Transaction,
		Network:     settleResponse.Network,
	}, nil
}

// x402PaymentNetwork reads the network out of the signed payment payload. Per the
// x402 spec the X-PAYMENT header is base64(JSON) and the payload names the network
// it was signed for; we use that to pick which quoted terms to check it against.
//
// Returns "" when it cannot be read, in which case the caller falls back to the
// single quoted network (and refuses if several were quoted, rather than guessing).
func x402PaymentNetwork(payment string) string {
	raw, err := base64.StdEncoding.DecodeString(payment)
	if err != nil {
		return ""
	}
	var payload struct {
		Network string `json:"network"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.Network
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	if receipt, err := json.Marshal(result); err == nil {
		w.Header().Set(X402PaymentResponseHeader, string(receipt))
	}

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

// x402GrantProMonth grants Pro for one month: a subscription renewal (so the monthly
// Pro grant task keeps renewing the data allowance) plus the Pro balance itself
// (pro = true), which is what actually confers the entitlement.
func x402GrantProMonth(
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
) (returnErr error) {
	startTime, endTime := ProGrantWindow(server.NowUtc())

	server.Tx(ctx, func(tx server.PgTx) {
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
			returnErr = err
			return
		}

		returnErr = model.AddProTransferBalanceInTx(
			tx,
			ctx,
			networkId,
			sku.ByteCount,
			startTime,
			endTime,
		)
	})

	if returnErr != nil {
		return
	}

	// the upgrade takes effect immediately rather than after ProCacheTtl
	model.UpdateProNetwork(ctx, networkId)

	return
}

// x402GrantData adds a data balance. pro = false: buying data never grants Pro. The
// balance is valid for pro.yml data_code.duration (1 year), the same as a data code.
func x402GrantData(
	ctx context.Context,
	networkId server.Id,
	sku *X402Sku,
	netRevenue model.NanoCents,
	settleResponse *X402SettleResponse,
) error {
	startTime := server.NowUtc()
	endTime := startTime.Add(model.Pro().DataCodeDuration)

	model.AddTransferBalance(ctx, &model.TransferBalance{
		NetworkId:             networkId,
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: sku.ByteCount,
		BalanceByteCount:      sku.ByteCount,
		NetRevenue:            netRevenue,
		PurchaseToken:         settleResponse.Transaction,
	})

	return nil
}

// x402SendReceipt emails a receipt for a settled purchase, through the normal
// account message template system (see X402ReceiptTemplate in aws_controller).
//
// Only sent when the caller supplied an email. A failed receipt must NEVER fail the
// purchase: the money already moved and the grant already landed, so the receipt is
// a courtesy and its failure is logged, not propagated.
func x402SendReceipt(ctx context.Context, email string, sku *X402Sku, settleResponse *X402SettleResponse) {
	defer func() {
		if err := recover(); err != nil {
			glog.Errorf("[x402]receipt panicked for %s: %v\n", email, err)
		}
	}()

	network := settleResponse.Network
	if network == "" {
		network = "chain"
	}

	awsMessageSender := GetAWSMessageSender()
	err := awsMessageSender.SendAccountMessageTemplate(
		email,
		&X402ReceiptTemplate{
			Description:      sku.Description,
			PriceUsd:         sku.PriceUsd,
			Asset:            X402().Asset,
			Network:          network,
			Transaction:      settleResponse.Transaction,
			Pro:              sku.Pro,
			BalanceByteCount: sku.ByteCount,
		},
	)
	if err != nil {
		glog.Errorf("[x402]receipt failed for %s: %s\n", email, err)
	}
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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

	receipt, err := json.Marshal(result)
	if err == nil {
		w.Header().Set(X402PaymentResponseHeader, string(receipt))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(receipt)
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
