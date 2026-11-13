package controller

// Pay with Gamma: the API (mmm/BTCPAY.md §18.3, connect/api/bringyour.yml
// "Gamma"). Every step but the customer's one mint is an API call:
//
//	GET  /pay/gamma/services.json  the services document set_service publishes (CORS *)
//	GET  /pay/gamma/terms          netuid, service hotkey, spender, prices, minimum caps, minting now
//	POST /pay/gamma/intent         bearer: the account's deposit hotkey, the price and the mint command
//	POST /pay/gamma/status         status token: pending, seen, confirming, paid, expired, review
//	GET  /pay/gamma/settings       bearer: the automatic renewal and top-up switches
//	POST /pay/gamma/settings       bearer: set them, the top-up item and the monthly limit; a switch
//	                               turned off cancels the charges it governs not yet sent, and
//	                               lists those already sent (charging)
//
// The API reads Postgres only: the biller (gamma_biller.go) keeps the chain's
// view there, so a request never waits on a node. The bearer token is the only
// authentication: we assigned the deposit hotkey to the account, so whoever
// mints into it pays for that account. A deposit hotkey is not a pay-to
// address and there is no wallet URI: the customer's coldkey mints with
// `btcli tx mint-gamma`, and we charge the price with spend_gamma.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/vedhavyas/go-subkey/v2"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The approved wording (the owner, 2026-11-11). Do not edit; renewals and
// top-ups are described in a sentence of their own.
const GammaDisclaimer = "Gamma can't be refunded or transferred. We charge only the price of what you buy. Unspent Gamma stays in your deposit and works only on SN25."

// GammaAutomaticNotice describes the automatic charges.
const GammaAutomaticNotice = "Unless you turn them off, we charge the same deposit automatically: plans paid with Gamma renew at the end of each term, a data pack (1 TiB unless you choose another) is added when your data runs out, and a free account that reaches its client limit is upgraded to Pro."

// GammaSubnetCaveat is the separate caveat of the /gamma page.
const GammaSubnetCaveat = "Dissolving a subnet destroys its gamma."

// Error codes of the Gamma endpoints, beside the shared PurchaseErrorCode*
// ones (asset_unavailable, invalid_request, plan_unavailable,
// item_unavailable, offer_unavailable, guest_sign_in_required, rate_limited,
// start_failed, invalid_email).
const (
	// the deposit holds less gamma than the price (also a skipped spend)
	GammaErrorCodeInsufficientGamma = "insufficient_gamma"
	// the deposit's daily spend cap did not allow the price when we spent
	GammaErrorCodeDailyCapExceeded = "daily_cap_exceeded"
	// the deposit's daily spend cap is below the price, so no spend can pass
	GammaErrorCodeDailyCapTooLow = "daily_cap_too_low"
	// the deposit names another spender (also a skipped spend)
	GammaErrorCodeWrongSpender = "wrong_spender"
	// there is no deposit at the hotkey (a skipped spend)
	GammaErrorCodeNoDeposit = "no_deposit"
	// the chain refused the amount (a skipped spend)
	GammaErrorCodeInvalidAmount = "invalid_amount"
	// a mint below 0.1 gamma fails (AmountTooLow)
	GammaErrorCodeMintTooSmall = "mint_too_small"
	// the hotkey's deposit or the hotkey belongs to another coldkey
	// (NonAssociatedColdKey): ask for the intent again with `coldkey`
	GammaErrorCodeDepositColdkeyMismatch = "deposit_coldkey_mismatch"
	// a mint above the pool's swap input limit fails (InsufficientLiquidity)
	GammaErrorCodeMintLiquidityExceeded = "mint_liquidity_exceeded"
	// a mint that would mint less than its minimum fails (SlippageTooHigh)
	GammaErrorCodeMintSlippage = "mint_slippage"
)

// The plans and data items gamma sells. Monthly too: gamma can renew.
var gammaKnownPlans = []string{model.SolanaPlanMonthly, model.SolanaPlanYearly, model.SolanaPlanYearlyOnboarding}

// gammaKnownItems are the data packs pro.yml sells with Gamma, in its order:
// data_1tib, data_10tib and the 256 GiB pack.
func gammaKnownItems() []string {
	return dataPackIdsOn(model.DataRailGamma)
}

// The biller writes its state every run; older than this, it is not running
// and nothing is sold.
const gammaStateFreshness = 3 * time.Minute

// The most open purchases one account may have.
const gammaMaxOpenIntentsPerNetwork = 10

// ----- the services document -----

// GammaServicesDocument is the document set_service publishes the url of
// (the Bittensor services guide's format; readers skip what they do not
// know).
type GammaServicesDocument struct {
	Version     int                       `json:"version"`
	Name        string                    `json:"name"`
	Description string                    `json:"description"`
	Publishers  []*GammaServicesPublisher `json:"publishers"`
	Services    []*GammaServicesService   `json:"services"`
	Payments    []*GammaServicesPayment   `json:"payments"`
}

type GammaServicesPublisher struct {
	Netuid uint16 `json:"netuid"`
	Hotkey string `json:"hotkey"`
}

type GammaServicesService struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	Spec     string `json:"spec,omitempty"`
}

type GammaServicesPayment struct {
	Type string `json:"type"`
	// gamma: the most one spend takes, so a deposit keeps at least this much
	MaxSpend string `json:"max_spend,omitempty"`
	// gamma: where the prices, the spender and the minting state are
	Terms string `json:"terms,omitempty"`
	// x402: the catalog
	Skus string `json:"skus,omitempty"`
}

const gammaApiEndpoint = "https://api.bringyour.com"

// the spec as ur.io publishes it (connect/api/bringyour.yml without its
// x-internal operations); ur.io serves no /api/bringyour.yml
const gammaApiSpecUrl = "https://ur.io/openapi.yml"

// GammaServices answers GET /pay/gamma/services.json.
func GammaServices(clientSession *session.ClientSession) (*GammaServicesDocument, error) {
	document := &GammaServicesDocument{
		Version:     1,
		Name:        "URnetwork",
		Description: "A decentralized VPN and proxy network. UR Pro plans and data, paid from an SN25 gamma deposit or over x402, entirely by API.",
		Publishers:  []*GammaServicesPublisher{},
		Services: []*GammaServicesService{
			{Type: "openapi", Endpoint: gammaApiEndpoint, Spec: gammaApiSpecUrl},
		},
		Payments: []*GammaServicesPayment{
			{Type: "gamma", MaxSpend: gammaFormatUnits(gammaMaxSpend()), Terms: gammaApiEndpoint + "/pay/gamma/terms"},
			{Type: "x402", Skus: gammaApiEndpoint + "/x402/skus"},
		},
	}
	if settings, err := gammaLoadSettings(); err == nil {
		document.Publishers = append(document.Publishers, &GammaServicesPublisher{Netuid: settings.Netuid, Hotkey: settings.ServiceAddress})
	}
	return document, nil
}

// gammaMaxSpend is the largest single charge at the standard tier: a year.
func gammaMaxSpend() int64 {
	largest := int64(0)
	tier := model.Pro().DefaultPriceTier()
	for _, plan := range []string{model.SolanaPlanMonthly, model.SolanaPlanYearly} {
		if priceUsd, ok := solanaPlanPriceUsdForTier(plan, tier); ok {
			largest = max(largest, model.UsdToNanoCents(priceUsd))
		}
	}
	for _, item := range gammaKnownItems() {
		if priceUsd, ok := dataPackPriceUsd(item, model.DataRailGamma); ok {
			largest = max(largest, model.UsdToNanoCents(priceUsd))
		}
	}
	return largest
}

// GammaServicesHandler serves the services document with
// Access-Control-Allow-Origin: *, as the services guide requires.
func GammaServicesCors(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

// ----- terms -----

type GammaTermsItem struct {
	Plan             string  `json:"plan,omitempty"`
	ItemId           string  `json:"item_id,omitempty"`
	PriceGamma       string  `json:"price_gamma"`
	PriceUsd         float64 `json:"price_usd"`
	MinDailyCapGamma string  `json:"min_daily_cap_gamma"`
	// plans: renewed automatically at the end of each term unless turned off
	Renews bool `json:"renews"`
	// TAO that mints about the price now; "" while minting is unavailable
	IndicativeTao string `json:"indicative_tao"`
}

type GammaTermsResult struct {
	Available             bool   `json:"available"`
	UnavailableReason     string `json:"unavailable_reason,omitempty"`
	MintAvailable         bool   `json:"mint_available"`
	MintUnavailableReason string `json:"mint_unavailable_reason,omitempty"`
	Netuid                uint16 `json:"netuid"`
	ServiceHotkey         string `json:"service_hotkey"`
	Spender               string `json:"spender"`
	ServicesUrl           string `json:"services_url"`
	PriceTier             string `json:"price_tier"`
	// the oracle's accepted TAO/USD price, in dollars; "" before any feed
	TaoUsd            string            `json:"tao_usd"`
	PriceAgeBlocks    uint64            `json:"price_age_blocks"`
	MaxPriceAgeBlocks uint64            `json:"max_price_age_blocks"`
	MinMintGamma      string            `json:"min_mint_gamma"`
	FinalizedBlock    uint64            `json:"finalized_block"`
	Items             []*GammaTermsItem `json:"items"`
	// the daily cap that lets every automatic charge (a yearly renewal, a
	// seat upgrade, a top-up) be spent
	AutoMinDailyCapGamma string     `json:"auto_min_daily_cap_gamma"`
	Disclaimer           string     `json:"disclaimer"`
	AutomaticNotice      string     `json:"automatic_notice"`
	UpdatedAt            *time.Time `json:"updated_at"`
}

// gammaView is what the API knows of Gamma: the settings and the biller's
// last state.
type gammaView struct {
	settings *gammaSettings
	state    *model.GammaChainState
	terms    *gammaTerms
	// "ready" or why not
	readiness string
	mint      string
}

func gammaLoadView(clientSession *session.ClientSession) *gammaView {
	view := &gammaView{readiness: gammaNotConfigured, mint: gammaNotConfigured}
	settings, err := gammaLoadSettings()
	if err != nil {
		return view
	}
	view.settings = settings
	state := model.GetGammaChainState(clientSession.Ctx)
	view.state = state
	if state == nil {
		view.readiness, view.mint = gammaChainUnreachable, gammaChainUnreachable
		return view
	}
	view.readiness, view.mint = state.Readiness, state.MintReadiness
	if gammaNow().Sub(state.UpdateTime) > gammaStateFreshness {
		view.readiness, view.mint = gammaChainUnreachable, gammaChainUnreachable
	}
	terms := &gammaTerms{}
	if 0 < len(state.Terms) && json.Unmarshal(state.Terms, terms) == nil {
		view.terms = terms
	}
	return view
}

// GammaTerms answers GET /pay/gamma/terms.
func GammaTerms(clientSession *session.ClientSession) (*GammaTermsResult, error) {
	view := gammaLoadView(clientSession)
	result := &GammaTermsResult{
		MaxPriceAgeBlocks: gammaPriceMaxAgeBlocks,
		MinMintGamma:      gammaFormatUnits(gammaMinMintUnits),
		Items:             []*GammaTermsItem{},
		Disclaimer:        GammaDisclaimer,
		AutomaticNotice:   GammaAutomaticNotice,
	}
	result.Available = view.readiness == gammaReady
	if !result.Available {
		result.UnavailableReason = view.readiness
	}
	result.MintAvailable = view.mint == gammaReady
	if !result.MintAvailable {
		result.MintUnavailableReason = view.mint
	}
	if view.settings != nil {
		result.Netuid = view.settings.Netuid
		result.ServiceHotkey = view.settings.ServiceAddress
		result.Spender = view.settings.SpenderAddress
		result.ServicesUrl = view.settings.ServicesUrl
	}
	if view.state != nil {
		updated := view.state.UpdateTime
		result.UpdatedAt = &updated
	}
	gammaPerTao := uint64(0)
	if view.terms != nil {
		result.PriceAgeBlocks = view.terms.PriceAgeBlocks
		result.FinalizedBlock = view.terms.FinalizedBlock
		if 0 < view.terms.TaoUsdAccepted {
			result.TaoUsd = gammaFormatUnits(int64(view.terms.TaoUsdAccepted))
		}
		if result.MintAvailable {
			gammaPerTao = view.terms.GammaPerTao
		}
	}
	tier := ResolvePriceTier(clientSession, "")
	result.PriceTier = tier.Tier.Name
	largest := int64(0)
	for _, plan := range []string{model.SolanaPlanMonthly, model.SolanaPlanYearly} {
		priceUsd, ok := solanaPlanPriceUsdForTier(plan, tier.Tier)
		if !ok {
			continue
		}
		units := model.UsdToNanoCents(priceUsd)
		largest = max(largest, units)
		result.Items = append(result.Items, &GammaTermsItem{
			Plan:             plan,
			PriceGamma:       gammaFormatUnits(units),
			PriceUsd:         priceUsd,
			MinDailyCapGamma: gammaFormatUnits(units),
			Renews:           true,
			IndicativeTao:    gammaIndicativeTao(units, gammaPerTao),
		})
	}
	for _, item := range gammaKnownItems() {
		priceUsd, ok := dataPackPriceUsd(item, model.DataRailGamma)
		if !ok {
			continue
		}
		units := model.UsdToNanoCents(priceUsd)
		largest = max(largest, units)
		result.Items = append(result.Items, &GammaTermsItem{
			ItemId:           item,
			PriceGamma:       gammaFormatUnits(units),
			PriceUsd:         priceUsd,
			MinDailyCapGamma: gammaFormatUnits(units),
			IndicativeTao:    gammaIndicativeTao(units, gammaPerTao),
		})
	}
	result.AutoMinDailyCapGamma = gammaFormatUnits(largest)
	return result, nil
}

// gammaIndicativeTao is about the TAO that mints units of gamma now, with 3%
// for slippage and the gamma price's lag, rounded up to 6 decimals; "" when
// minting is unavailable.
func gammaIndicativeTao(units int64, gammaPerTao uint64) string {
	if units <= 0 || gammaPerTao == 0 {
		return ""
	}
	tao := float64(units) / float64(gammaPerTao) * 1.03
	micro := math.Ceil(tao * 1e6)
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", micro/1e6), "0"), ".")
}

// ----- intents -----

type CreateGammaPaymentIntentArgs struct {
	// monthly | yearly | yearly_onboarding
	Plan string `json:"plan,omitempty"`
	// a pack pro.yml sells with Gamma: data_1tib | data_10tib | data_256gib
	ItemId string `json:"item_id,omitempty"`
	// account (default): the plan or data lands on the caller's network;
	// code: a data pack as a balance code
	Delivery string `json:"delivery,omitempty"`
	// delivery code: where the code is also emailed
	Email             string `json:"email,omitempty"`
	StorefrontCountry string `json:"storefront_country,omitempty"`
	// the SS58 coldkey that will mint: picks a hotkey whose deposit that
	// coldkey can open or already owns
	Coldkey string `json:"coldkey,omitempty"`
}

type GammaPaymentIntentResult struct {
	IntentId        string  `json:"intent_id"`
	StatusToken     string  `json:"status_token"`
	Plan            string  `json:"plan"`
	ItemId          string  `json:"item_id"`
	Delivery        string  `json:"delivery"`
	Tier            string  `json:"tier"`
	OfferApplied    bool    `json:"offer_applied"`
	PriceUsd        float64 `json:"price_usd"`
	RegularPriceUsd float64 `json:"regular_price_usd"`
	AmountGamma     string  `json:"amount_gamma"`
	// the public 32-byte spend reference our GammaSpent will carry
	Reference     string `json:"reference"`
	Netuid        uint16 `json:"netuid"`
	DepositHotkey string `json:"deposit_hotkey"`
	ServiceHotkey string `json:"service_hotkey"`
	Spender       string `json:"spender"`
	// the daily cap the deposit needs for this purchase, and for every
	// automatic charge after it
	MinDailyCapGamma      string                    `json:"min_daily_cap_gamma"`
	AutoMinDailyCapGamma  string                    `json:"auto_min_daily_cap_gamma"`
	MinMintGamma          string                    `json:"min_mint_gamma"`
	IndicativeTao         string                    `json:"indicative_tao"`
	MintAvailable         bool                      `json:"mint_available"`
	MintUnavailableReason string                    `json:"mint_unavailable_reason,omitempty"`
	MintDryRunCommand     string                    `json:"mint_dry_run_command"`
	MintCommand           string                    `json:"mint_command"`
	Disclaimer            string                    `json:"disclaimer"`
	AutomaticNotice       string                    `json:"automatic_notice"`
	ExpiresAt             *time.Time                `json:"expires_at"`
	Status                *GammaPaymentStatusResult `json:"status,omitempty"`
	Error                 *CryptoPaymentError       `json:"error,omitempty"`
}

func gammaIntentError(code string, message string) *GammaPaymentIntentResult {
	return &GammaPaymentIntentResult{Error: &CryptoPaymentError{Code: code, Message: message}}
}

const gammaUnavailableMessage = "Paying with Gamma is temporarily unavailable."

// A plan paid with Gamma never charges for days another Pro subscription
// pays for (the owner, 2026-11-11): beside one that renews, a Gamma Pro
// purchase and Gamma renewal are refused, as card checkout is refused beside a
// renewing Gamma plan (stripeNewSubscriptionRefusal).
const gammaOtherProRenewsPurchaseMessage = "You already have a Pro subscription that renews, so no Gamma payment was started: paying with Gamma as well would charge you twice for the same days. Cancel that subscription, then pay with Gamma once it has ended."
const gammaOtherProRenewsSettingsMessage = "You have another Pro subscription that renews, so Gamma renewal was not turned on: both would charge you for the same days. Cancel that subscription, then turn Gamma renewal on once it has ended."

// a plan intent the biller holds while another Pro subscription pays for the
// days it would buy (gammaPlanHold)
const gammaPlanCoveredMessage = "Another Pro subscription pays for these days, so this Gamma payment is not charged while it runs. It is charged only if that subscription ends before this payment expires."

// A Gamma plan is never charged beside another Gamma plan charge for the same
// days (the owner, 2026-11-11). A plan purchase is refused while another Gamma
// plan charge of the network is being made: a renewal, a seat upgrade or
// another plan purchase in a spend batch cannot be voided safely, since its
// spend may still land. Once it is paid, a plan can be bought again (an
// upgrade starts at once, as before).
const gammaPlanChargingPurchaseMessage = "A Gamma payment for your Pro plan is being charged now (its renewal, an upgrade at your client limit, or a plan you bought), so no Gamma payment was started: paying for a plan as well would charge you twice for the same days. Check again in a few minutes, once it is paid."

// the status of a plan intent held while another Gamma plan charge of the
// network is in flight (gammaPlanHold)
const gammaPlanChargingMessage = "Another Gamma payment for your Pro plan is being charged now, so this one waits: if that one is paid, this one is not charged, since both would pay for the same days."

// the status of a plan intent made before another Gamma plan of the network
// was paid (gammaPlanHold)
const gammaPlanPaidSinceMessage = "Another Gamma payment for your Pro plan was paid after this one was started, so this one is not charged: both would pay for the same days."

// the status of a renewal or seat upgrade while a plan purchase of the
// network is open (gammaPlanHold); automatic intents hand out no status
// token, so this is for operators
const gammaPlanReplacedMessage = "A Gamma plan purchase of this account is open and replaces this automatic charge: it waits while the purchase is open, is voided once the purchase is paid, and goes ahead only if the purchase expires unpaid."

// gammaOtherProRenews reports whether a Pro subscription of the network other
// than a plan paid with gamma renews or still bills, by card checkout's own
// rule (stripeNewSubscriptionRefusal) with gamma left out: a recorded window
// of Stripe, the App Store or Google Play that covers now (a manual grant, an
// x402 month or a one-time crypto year never renews), the subscription behind
// its last Stripe window still billing, or a live subscription of its Stripe
// customer.
func gammaOtherProRenews(clientSession *session.ClientSession) bool {
	markets := model.GetActiveSubscriptionRenewalMarkets(clientSession.Ctx, clientSession.ByJwt.NetworkId, model.SubscriptionTypeSupporter)
	if subscriptionMarketsBlockNewPurchase(markets, false) {
		return true
	}
	return stripeNetworkHasBillingSubscription(clientSession)
}

// CreateGammaPaymentIntent answers POST /pay/gamma/intent.
func CreateGammaPaymentIntent(args *CreateGammaPaymentIntentArgs, clientSession *session.ClientSession) (*GammaPaymentIntentResult, error) {
	ctx := clientSession.Ctx
	view := gammaLoadView(clientSession)
	if view.settings == nil || view.readiness != gammaReady {
		return gammaIntentError(PurchaseErrorCodeAssetUnavailable, gammaUnavailableMessage), nil
	}
	settings := view.settings
	networkId := clientSession.ByJwt.NetworkId

	plan := strings.TrimSpace(args.Plan)
	itemId := strings.TrimSpace(args.ItemId)
	if (plan == "") == (itemId == "") {
		return gammaIntentError(PurchaseErrorCodeInvalidRequest, "Name a plan or a data pack."), nil
	}
	if plan != "" && !slices.Contains(gammaKnownPlans, plan) {
		return gammaIntentError(PurchaseErrorCodePlanUnavailable, "That plan is not available with Gamma."), nil
	}
	if itemId != "" && !slices.Contains(gammaKnownItems(), itemId) {
		if refusal := dataPackRefusal(itemId, model.DataRailGamma); refusal != "" {
			return gammaIntentError(PurchaseErrorCodeItemUnavailable, refusal), nil
		}
		return gammaIntentError(PurchaseErrorCodeItemUnavailable, "That data pack is not available with Gamma."), nil
	}
	delivery := strings.TrimSpace(args.Delivery)
	switch delivery {
	case "", model.CryptoDeliveryAccount:
		delivery = model.CryptoDeliveryAccount
	case model.CryptoDeliveryCode:
		if plan != "" {
			return gammaIntentError(PurchaseErrorCodeInvalidRequest, "Only a data pack can be bought as a code."), nil
		}
	default:
		return gammaIntentError(PurchaseErrorCodeInvalidRequest, "Delivery is account or code."), nil
	}
	email := strings.TrimSpace(args.Email)
	if email != "" && (delivery != model.CryptoDeliveryCode || !payDataValidEmail(email)) {
		return gammaIntentError(PurchaseErrorCodeInvalidEmail, "That email address does not look right."), nil
	}
	coldkey := strings.TrimSpace(args.Coldkey)
	if coldkey != "" && !model.IsValidBittensorAddress(coldkey) {
		return gammaIntentError(PurchaseErrorCodeInvalidRequest, "The coldkey is not an SS58 address."), nil
	}

	offerApplied := false
	// a plan, and a data pack delivered to the account, land on the caller's
	// network (purchase_guest_guard.go); a code is redeemed wherever its
	// buyer chooses
	if (plan != "" || delivery == model.CryptoDeliveryAccount) && refuseGuestPurchase(clientSession) {
		return gammaIntentError(PurchaseErrorCodeGuestSignInRequired, purchaseGuestSignInRequiredMessage), nil
	}
	if plan != "" {
		if plan == model.SolanaPlanYearlyOnboarding {
			if EligibleOnboardingOffer(clientSession, networkId) == nil {
				return gammaIntentError(PurchaseErrorCodeOfferUnavailable, "The welcome offer is not available."), nil
			}
			offerApplied = true
		}
	}
	if !allowNetworkIntent(networkId) {
		return gammaIntentError(PurchaseErrorCodeRateLimited, "Too many payment attempts. Try again in a minute."), nil
	}
	if plan != "" && gammaOtherProRenews(clientSession) {
		// Pro paid elsewhere renews: a Gamma plan beside it would charge the
		// customer twice for the same days (an open one is not handed out
		// again either)
		return gammaIntentError(PurchaseErrorCodeAlreadySubscribed, gammaOtherProRenewsPurchaseMessage), nil
	}

	var priceUsd, regularUsd float64
	tierName := ""
	if plan != "" {
		tier := ResolvePriceTier(clientSession, args.StorefrontCountry)
		var ok bool
		priceUsd, ok = solanaPlanPriceUsdForTier(plan, tier.Tier)
		if !ok || priceUsd <= 0 {
			return gammaIntentError(PurchaseErrorCodePlanUnavailable, "That plan is not available."), nil
		}
		regularUsd = priceUsd
		if offerApplied {
			regularUsd = tier.Tier.YearlyUsd
		}
		tierName = tier.Tier.Name
	} else {
		var ok bool
		priceUsd, ok = dataPackPriceUsd(itemId, model.DataRailGamma)
		if !ok {
			return gammaIntentError(PurchaseErrorCodeItemUnavailable, "That data pack is not available."), nil
		}
		regularUsd = priceUsd
	}
	item := plan
	if item == "" {
		item = itemId
	}

	hotkey, err := gammaPickDepositHotkey(clientSession, settings, networkId, coldkey)
	if err != nil {
		if err == errGammaNoFreeHotkey {
			return gammaIntentError(GammaErrorCodeDepositColdkeyMismatch, "Every deposit hotkey of this account belongs to another coldkey."), nil
		}
		glog.Errorf("[gamma]could not assign a deposit hotkey: %s\n", err)
		return gammaIntentError(PurchaseErrorCodeStartFailed, purchaseStartFailedMessage), nil
	}

	now := gammaNow()
	amount := model.UsdToNanoCents(priceUsd)
	// with minting unavailable, only a deposit that already covers the price
	// can pay
	if view.mint != gammaReady && (hotkey.DepositSpendable == nil || *hotkey.DepositSpendable < amount) {
		result := gammaIntentError(PurchaseErrorCodeAssetUnavailable, gammaMintUnavailableMessage(view.mint))
		result.MintUnavailableReason = view.mint
		return result, nil
	}

	var open *model.GammaPaymentIntent
	if delivery == model.CryptoDeliveryAccount {
		open = model.GetOpenGammaPaymentIntentForReuse(ctx, networkId, hotkey.Hotkey, item, delivery, now)
	}
	if plan != "" {
		// another Gamma plan charge of the network in flight (a renewal, a
		// seat upgrade, another plan) cannot be voided: its spend may land.
		// Asking again for the purchase being charged hands it back.
		var exclude *server.Id
		var since *time.Time
		if open != nil {
			exclude, since = &open.IntentId, &open.CreateTime
		}
		charging, paidSince := model.GetGammaPlanChargeState(ctx, networkId, gammaKnownPlans, exclude, since)
		if charging {
			return gammaIntentError(PurchaseErrorCodeAlreadySubscribed, gammaPlanChargingPurchaseMessage), nil
		}
		if paidSince {
			// another plan was paid after the open one was made, so that one
			// is never charged: this is a new purchase
			open = nil
		}
	}
	if open != nil {
		token, tokenHash := cryptoNewStatusToken()
		model.AddGammaPaymentIntentStatusToken(ctx, open.IntentId, tokenHash)
		return gammaIntentResult(clientSession, view, open, hotkey, token, now), nil
	}
	if gammaMaxOpenIntentsPerNetwork <= model.CountOpenGammaPaymentIntents(ctx, networkId, now) {
		return gammaIntentError(PurchaseErrorCodeRateLimited, "Too many open Gamma payments. Pay or let one expire first."), nil
	}

	token, tokenHash := cryptoNewStatusToken()
	intent := &model.GammaPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		NetworkId:                networkId,
		Hotkey:                   hotkey.Hotkey,
		Purpose:                  model.GammaPurposePurchase,
		Reference:                gammaNewReference(),
		Delivery:                 delivery,
		Item:                     item,
		Tier:                     tierName,
		OfferApplied:             offerApplied,
		PriceUsdNanoCents:        amount,
		RegularPriceUsdNanoCents: model.UsdToNanoCents(regularUsd),
		Email:                    email,
		CreateTime:               now,
		ExpiresAt:                now.Add(settings.IntentTtl),
	}
	model.CreateGammaPaymentIntent(ctx, intent)
	gammaIntentsCreated.WithLabelValues(model.GammaPurposePurchase).Inc()
	glog.Infof("[gamma]intent %s: %s, delivery %s, hotkey %s\n", intent.IntentId, item, delivery, hotkey.Hotkey)
	return gammaIntentResult(clientSession, view, intent, hotkey, token, now), nil
}

func gammaMintUnavailableMessage(reason string) string {
	switch reason {
	case gammaMintPriceStale:
		return "Minting gamma is paused: the chain's TAO/USD price is more than 300 blocks old. Try again later, or pay with TAO or x402."
	case gammaMintSubnetDisabled, gammaMintDisabled:
		return "Minting SN25 gamma is not switched on yet."
	}
	return gammaUnavailableMessage
}

var errGammaNoFreeHotkey = fmt.Errorf("no deposit hotkey of the account can take this coldkey's deposit")

// gammaPickDepositHotkey is the deposit hotkey a payer mints into: the
// account's first hotkey, or with a coldkey, the first whose deposit that
// coldkey owns or can open (no deposit there, and the hotkey unassociated or
// associated with it). An account with none gets its first; one whose every
// hotkey is taken gets another.
func gammaPickDepositHotkey(clientSession *session.ClientSession, settings *gammaSettings, networkId server.Id, coldkey string) (*model.GammaDepositHotkey, error) {
	ctx := clientSession.Ctx
	hotkeys := model.GetGammaDepositHotkeys(ctx, networkId)
	for _, hotkey := range hotkeys {
		if coldkey == "" {
			return hotkey, nil
		}
		depositOk := hotkey.DepositColdkey == nil || *hotkey.DepositColdkey == coldkey
		ownerOk := hotkey.HotkeyOwner == nil || *hotkey.HotkeyOwner == coldkey
		if depositOk && ownerOk {
			return hotkey, nil
		}
	}
	if gammaMaxDepositHotkeysPerNetwork <= len(hotkeys) {
		return nil, errGammaNoFreeHotkey
	}
	return model.AllocateGammaDepositHotkey(ctx, networkId, func(index int64) (string, error) {
		_, address, err := gammaDepositHotkeyAt(settings.DepositHotkeyMaster, index)
		return address, err
	}, gammaNow())
}

func gammaIntentResult(
	clientSession *session.ClientSession,
	view *gammaView,
	intent *model.GammaPaymentIntent,
	hotkey *model.GammaDepositHotkey,
	token string,
	now time.Time,
) *GammaPaymentIntentResult {
	settings := view.settings
	plan, itemId := cryptoSplitItem(intent.Item)
	if intent.Item == model.SolanaPlanMonthly {
		plan, itemId = intent.Item, ""
	}
	amount := intent.AmountGamma()
	status := gammaStatusResult(clientSession, view, intent, now)
	remaining := amount
	if hotkey.DepositBalance != nil {
		remaining = max(0, amount-*hotkey.DepositBalance)
	}
	gammaPerTao := uint64(0)
	if view.terms != nil && view.mint == gammaReady {
		gammaPerTao = view.terms.GammaPerTao
	}
	// a mint of the remainder, at least the chain's minimum
	mintUnits := max(remaining, gammaMinMintUnits)
	indicative := gammaIndicativeTao(mintUnits, gammaPerTao)
	amountArg := indicative
	if amountArg == "" {
		amountArg = "<TAO>"
	}
	command := fmt.Sprintf(
		"btcli tx mint-gamma --netuid %d --hotkey %s --amount-tao %s --service-hotkey %s",
		settings.Netuid, hotkey.Hotkey, amountArg, settings.ServiceAddress,
	)
	expiresAt := intent.ExpiresAt
	result := &GammaPaymentIntentResult{
		IntentId:             intent.IntentId.String(),
		StatusToken:          token,
		Plan:                 plan,
		ItemId:               itemId,
		Delivery:             intent.Delivery,
		Tier:                 intent.Tier,
		OfferApplied:         intent.OfferApplied,
		PriceUsd:             model.NanoCentsToUsd(intent.PriceUsdNanoCents),
		RegularPriceUsd:      model.NanoCentsToUsd(intent.RegularPriceUsdNanoCents),
		AmountGamma:          gammaFormatUnits(amount),
		Reference:            gammaHex(intent.Reference),
		Netuid:               settings.Netuid,
		DepositHotkey:        hotkey.Hotkey,
		ServiceHotkey:        settings.ServiceAddress,
		Spender:              settings.SpenderAddress,
		MinDailyCapGamma:     gammaFormatUnits(amount),
		AutoMinDailyCapGamma: gammaFormatUnits(max(amount, gammaMaxSpend())),
		MinMintGamma:         gammaFormatUnits(gammaMinMintUnits),
		IndicativeTao:        indicative,
		MintAvailable:        view.mint == gammaReady,
		MintDryRunCommand:    command + " --dry-run",
		MintCommand:          command + " -w <your_coldkey>",
		Disclaimer:           GammaDisclaimer,
		AutomaticNotice:      GammaAutomaticNotice,
		ExpiresAt:            &expiresAt,
		Status:               status,
	}
	if !result.MintAvailable {
		result.MintUnavailableReason = view.mint
	}
	return result
}

// ----- status -----

type GammaPaymentStatusArgs struct {
	IntentId    string `json:"intent_id"`
	StatusToken string `json:"status_token"`
}

type GammaPaymentStatusResult struct {
	IntentId      string `json:"intent_id"`
	Purpose       string `json:"purpose"`
	Plan          string `json:"plan"`
	ItemId        string `json:"item_id"`
	Delivery      string `json:"delivery"`
	Status        string `json:"status"`
	ReviewReason  string `json:"review_reason"`
	AmountGamma   string `json:"amount_gamma"`
	Reference     string `json:"reference"`
	DepositHotkey string `json:"deposit_hotkey"`
	// as the biller last saw the deposit; "" when there is none
	DepositBalanceGamma string `json:"deposit_balance_gamma"`
	SpendableGamma      string `json:"spendable_gamma"`
	RemainingGamma      string `json:"remaining_gamma"`
	// "" with no cap
	DailyCapGamma    string     `json:"daily_cap_gamma"`
	MinDailyCapGamma string     `json:"min_daily_cap_gamma"`
	ObservedBlock    *int64     `json:"observed_block"`
	SpentBlock       *int64     `json:"spent_block"`
	ExpiresAt        *time.Time `json:"expires_at"`
	CreditedAt       *time.Time `json:"credited_at"`
	BalanceCode      string     `json:"balance_code"`
	// stray plain TAO seen at the deposit hotkey (a mistake we can return)
	StrayTaoRao int64               `json:"stray_tao_rao"`
	Error       *CryptoPaymentError `json:"error,omitempty"`
}

// The status words, as /pay/crypto/status uses them.
const (
	gammaStatusUnknown    = "unknown"
	gammaStatusPending    = "pending"
	gammaStatusSeen       = "seen"
	gammaStatusConfirming = "confirming"
	gammaStatusPaid       = "paid"
	gammaStatusExpired    = "expired"
	gammaStatusReview     = "review"
)

// GammaPaymentStatus answers POST /pay/gamma/status.
func GammaPaymentStatus(args *GammaPaymentStatusArgs, clientSession *session.ClientSession) (*GammaPaymentStatusResult, error) {
	if !payDataLookupLimiter.allow(clientSession) {
		return nil, errors.New("429 Too many status checks from this address. Try again in a minute.")
	}
	unknown := &GammaPaymentStatusResult{IntentId: args.IntentId, Status: gammaStatusUnknown}
	intentId, err := server.ParseId(strings.TrimSpace(args.IntentId))
	if err != nil {
		return unknown, nil
	}
	tokenHash, ok := cryptoStatusTokenHash(args.StatusToken)
	if !ok {
		return unknown, nil
	}
	intent := model.GetGammaPaymentIntent(clientSession.Ctx, intentId)
	if intent == nil || !intent.MatchesStatusToken(tokenHash) {
		return unknown, nil
	}
	return gammaStatusResult(clientSession, gammaLoadView(clientSession), intent, gammaNow()), nil
}

func gammaStatusResult(clientSession *session.ClientSession, view *gammaView, intent *model.GammaPaymentIntent, now time.Time) *GammaPaymentStatusResult {
	ctx := clientSession.Ctx
	plan, itemId := cryptoSplitItem(intent.Item)
	if intent.Item == model.SolanaPlanMonthly {
		plan, itemId = intent.Item, ""
	}
	amount := intent.AmountGamma()
	expiresAt := intent.ExpiresAt
	result := &GammaPaymentStatusResult{
		IntentId:         intent.IntentId.String(),
		Purpose:          intent.Purpose,
		Plan:             plan,
		ItemId:           itemId,
		Delivery:         intent.Delivery,
		AmountGamma:      gammaFormatUnits(amount),
		Reference:        gammaHex(intent.Reference),
		DepositHotkey:    intent.Hotkey,
		MinDailyCapGamma: gammaFormatUnits(amount),
		ExpiresAt:        &expiresAt,
		CreditedAt:       intent.CreditedAt,
	}
	hotkey := model.GetGammaDepositHotkey(ctx, intent.Hotkey)
	balance := int64(-1)
	spendable := int64(-1)
	if hotkey != nil {
		result.ObservedBlock = hotkey.ObservedBlock
		result.StrayTaoRao = hotkey.StrayTaoRao
		if hotkey.DepositBalance != nil {
			balance = *hotkey.DepositBalance
			result.DepositBalanceGamma = gammaFormatUnits(balance)
		}
		if hotkey.DepositSpendable != nil {
			spendable = *hotkey.DepositSpendable
			result.SpendableGamma = gammaFormatUnits(spendable)
		}
		if hotkey.DepositDailyCap != nil {
			result.DailyCapGamma = gammaFormatUnits(*hotkey.DepositDailyCap)
		}
	}
	if 0 <= balance {
		result.RemainingGamma = gammaFormatUnits(max(0, amount-balance))
	} else {
		result.RemainingGamma = gammaFormatUnits(amount)
	}
	charge := model.GetGammaChargeForIntent(ctx, intent.IntentId)
	if charge != nil && charge.State == model.GammaChargeSpent {
		result.SpentBlock = charge.BlockNumber
	}

	switch {
	case intent.CreditedAt != nil:
		result.Status = gammaStatusPaid
		if intent.BalanceCodeId != nil {
			result.BalanceCode = model.GetBalanceCodeSecretInConn(nil, ctx, *intent.BalanceCodeId)
		}
		return result
	case intent.VoidedAt != nil:
		result.Status, result.ReviewReason = gammaStatusReview, "voided"
		return result
	case charge != nil && charge.State == model.GammaChargeSpent:
		result.Status = gammaStatusConfirming
		if gammaMaxCreditFailures <= intent.CreditFailureCount {
			result.Status, result.ReviewReason = gammaStatusReview, "credit_failed"
		}
		return result
	case charge != nil && charge.State == model.GammaChargePending:
		result.Status = gammaStatusConfirming
		return result
	}
	if !intent.ExpiresAt.After(now) {
		result.Status = gammaStatusExpired
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeQuoteExpired, Message: "The payment expired before the deposit could pay it."}
		return result
	}
	if hold := gammaPlanHold(ctx, intent, now); hold != "" {
		// covered: the biller does not charge it while another Pro
		// subscription or another Gamma plan pays for the same days
		// (gamma_auto.go)
		result.Status = gammaStatusSeen
		if balance < 0 {
			result.Status = gammaStatusPending
		}
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeAlreadySubscribed, Message: hold}
		return result
	}
	if charge != nil && (charge.State == model.GammaChargeSkipped || charge.State == model.GammaChargeFailed) {
		result.Status = gammaStatusSeen
		if balance < 0 {
			result.Status = gammaStatusPending
		}
		result.Error = gammaSkipError(charge.SkipReason, amount)
		return result
	}
	switch {
	case hotkey == nil || balance <= 0:
		result.Status = gammaStatusPending
	case hotkey.DepositSpender != nil && view.settings != nil && *hotkey.DepositSpender != view.settings.SpenderAddress:
		result.Status = gammaStatusSeen
		result.Error = &CryptoPaymentError{Code: GammaErrorCodeWrongSpender, Message: "The deposit names another spender. Point it at URnetwork's service with btcli gamma set-spender --service-hotkey."}
	case balance < amount:
		result.Status = gammaStatusSeen
	case hotkey.DepositDailyCap != nil && *hotkey.DepositDailyCap < amount:
		result.Status = gammaStatusSeen
		result.Error = &CryptoPaymentError{Code: GammaErrorCodeDailyCapTooLow, Message: fmt.Sprintf("The deposit's daily spend cap is below the price. Raise it to at least %s gamma.", gammaFormatUnits(amount))}
	case spendable < amount:
		// the cap refills; the charge goes when it allows the price
		result.Status = gammaStatusSeen
	default:
		// covered: the next biller run charges it
		result.Status = gammaStatusConfirming
	}
	return result
}

func gammaSkipError(reason string, amount int64) *CryptoPaymentError {
	switch reason {
	case GammaErrorCodeInsufficientGamma:
		return &CryptoPaymentError{Code: reason, Message: "The deposit held less gamma than the price when we spent. Mint more; we try again once the deposit changes."}
	case GammaErrorCodeDailyCapExceeded:
		return &CryptoPaymentError{Code: reason, Message: fmt.Sprintf("The deposit's daily spend cap did not allow the price. Raise it to at least %s gamma.", gammaFormatUnits(amount))}
	case GammaErrorCodeWrongSpender:
		return &CryptoPaymentError{Code: reason, Message: "The deposit names another spender. Point it at URnetwork's service with btcli gamma set-spender --service-hotkey."}
	case GammaErrorCodeNoDeposit:
		return &CryptoPaymentError{Code: reason, Message: "There was no deposit at the hotkey when we spent."}
	}
	return &CryptoPaymentError{Code: GammaErrorCodeInvalidAmount, Message: "The spend did not go through; we try again once the deposit changes."}
}

// ----- settings -----

type GammaSettingsArgs struct {
	AutoRenew   *bool   `json:"auto_renew,omitempty"`
	AutoTopup   *bool   `json:"auto_topup,omitempty"`
	TopupItemId *string `json:"topup_item_id,omitempty"`
	// decimal gamma; "0" or "" removes the limit
	MonthlyLimitGamma *string `json:"monthly_limit_gamma,omitempty"`
}

type GammaSettingsResult struct {
	AutoRenew   bool   `json:"auto_renew"`
	AutoTopup   bool   `json:"auto_topup"`
	TopupItemId string `json:"topup_item_id"`
	// "" with no limit
	MonthlyLimitGamma string `json:"monthly_limit_gamma"`
	MonthSpentGamma   string `json:"month_spent_gamma"`
	// the plan a renewal or a seat upgrade buys; "" before a plan was paid
	RenewPlan string `json:"renew_plan"`
	// the deposit hotkey automatic charges draw from; "" before a payment
	DepositHotkey  string   `json:"deposit_hotkey"`
	DepositHotkeys []string `json:"deposit_hotkeys"`
	// the automatic charges whose switch is off that were already being
	// charged when it was turned off: a payment already sent to the chain
	// cannot be recalled, so each can still be charged this once; empty when
	// there is none
	Charging []*GammaSettingsCharge `json:"charging"`
	// Charging in a sentence; "" when it is empty
	Notice string              `json:"notice,omitempty"`
	Error  *CryptoPaymentError `json:"error,omitempty"`
}

// GammaSettingsCharge is an automatic charge already being made when its
// switch was turned off (model.GammaAutomaticChargeInFlight).
type GammaSettingsCharge struct {
	IntentId string `json:"intent_id"`
	// renew, topup or seat
	Purpose     string `json:"purpose"`
	Plan        string `json:"plan"`
	ItemId      string `json:"item_id"`
	AmountGamma string `json:"amount_gamma"`
	// confirming: its spend was sent, or landed and is being credited
	Status string `json:"status"`
}

// GetGammaSettings answers GET /pay/gamma/settings.
func GetGammaSettings(clientSession *session.ClientSession) (*GammaSettingsResult, error) {
	return gammaSettingsResult(clientSession, model.GetGammaSwitchedOffChargesInFlight(clientSession.Ctx, clientSession.ByJwt.NetworkId)), nil
}

// SetGammaSettings answers POST /pay/gamma/settings. A switch turned off
// (auto_renew: the renewal and the upgrade at the client limit; auto_topup:
// the data top-up and that upgrade) cancels the open charges it governs that
// are not yet sent: the biller voids them in its own step, and no spend batch
// takes them meanwhile. One already sent to the chain cannot be recalled, and
// voiding it would leave a payment that lands uncredited, so the switch is
// still turned off and the answer lists that charge (charging) and says
// plainly that it can still be charged this once (notice). Refusing instead
// would spare the customer nothing, since the charge lands either way, and
// would leave the switch on for the charges after it.
func SetGammaSettings(args *GammaSettingsArgs, clientSession *session.ClientSession) (*GammaSettingsResult, error) {
	settings := &model.GammaAccountSettings{AutoRenew: args.AutoRenew, AutoTopup: args.AutoTopup}
	if args.TopupItemId != nil {
		item := strings.TrimSpace(*args.TopupItemId)
		if !slices.Contains(gammaTopupItems(), item) {
			return &GammaSettingsResult{Error: &CryptoPaymentError{Code: PurchaseErrorCodeItemUnavailable, Message: gammaTopupItemMessage()}}, nil
		}
		settings.TopupItem = &item
	}
	if args.MonthlyLimitGamma != nil {
		units, ok := gammaParseUnits(*args.MonthlyLimitGamma)
		if !ok {
			return &GammaSettingsResult{Error: &CryptoPaymentError{Code: PurchaseErrorCodeInvalidRequest, Message: "The monthly limit is a decimal amount of gamma, or \"0\" for none."}}, nil
		}
		settings.MonthlyLimit = &units
	}
	if args.AutoRenew != nil && *args.AutoRenew && gammaOtherProRenews(clientSession) {
		// a Gamma plan renewing beside Pro paid elsewhere would charge the
		// customer twice for the same days; nothing in the request is applied
		result := gammaSettingsResult(clientSession, model.GetGammaSwitchedOffChargesInFlight(clientSession.Ctx, clientSession.ByJwt.NetworkId))
		result.Error = &CryptoPaymentError{Code: PurchaseErrorCodeAlreadySubscribed, Message: gammaOtherProRenewsSettingsMessage}
		return result, nil
	}
	// what was already being charged, read under the account's row lock: a
	// spend batch made after this sees the switch off
	inFlight := model.SetGammaAccountSettings(clientSession.Ctx, clientSession.ByJwt.NetworkId, settings, gammaNow())
	return gammaSettingsResult(clientSession, inFlight), nil
}

func gammaSettingsResult(clientSession *session.ClientSession, inFlight []*model.GammaAutomaticChargeInFlight) *GammaSettingsResult {
	ctx := clientSession.Ctx
	networkId := clientSession.ByJwt.NetworkId
	result := &GammaSettingsResult{
		AutoRenew:      true,
		AutoTopup:      true,
		TopupItemId:    StripeItemData1Tib,
		DepositHotkeys: []string{},
		Charging:       []*GammaSettingsCharge{},
	}
	for _, charge := range inFlight {
		plan, itemId := cryptoSplitItem(charge.Item)
		if charge.Item == model.SolanaPlanMonthly {
			plan, itemId = charge.Item, ""
		}
		result.Charging = append(result.Charging, &GammaSettingsCharge{
			IntentId:    charge.IntentId.String(),
			Purpose:     charge.Purpose,
			Plan:        plan,
			ItemId:      itemId,
			AmountGamma: gammaFormatUnits(charge.Amount),
			Status:      gammaStatusConfirming,
		})
	}
	result.Notice = gammaSettingsChargingNotice(inFlight)
	for _, hotkey := range model.GetGammaDepositHotkeys(ctx, networkId) {
		result.DepositHotkeys = append(result.DepositHotkeys, hotkey.Hotkey)
	}
	now := gammaNow()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	result.MonthSpentGamma = gammaFormatUnits(model.SumGammaAutomaticSpend(ctx, networkId, monthStart))
	account := model.GetGammaAccount(ctx, networkId)
	if account == nil {
		return result
	}
	result.AutoRenew = account.AutoRenew
	result.AutoTopup = account.AutoTopup
	result.TopupItemId = account.TopupItem
	if account.MonthlyLimit != nil {
		result.MonthlyLimitGamma = gammaFormatUnits(*account.MonthlyLimit)
	}
	result.RenewPlan = account.RenewPlan
	result.DepositHotkey = account.Hotkey
	return result
}

// gammaSettingsChargingNotice says, in plain words, which automatic charges
// were already being made when their switch was turned off, and that each can
// still be charged this once; "" when there is none.
func gammaSettingsChargingNotice(inFlight []*model.GammaAutomaticChargeInFlight) string {
	descriptions := []string{}
	for _, charge := range inFlight {
		what := fmt.Sprintf("(%s, %s gamma)", gammaItemDescription(charge.Item), gammaFormatUnits(charge.Amount))
		switch charge.Purpose {
		case model.GammaPurposeRenew:
			descriptions = append(descriptions, "the renewal of your plan "+what)
		case model.GammaPurposeSeat:
			descriptions = append(descriptions, "the upgrade to Pro at your client limit "+what)
		case model.GammaPurposeTopup:
			descriptions = append(descriptions, "the data top-up "+what)
		default:
			descriptions = append(descriptions, "an automatic charge "+what)
		}
	}
	switch len(descriptions) {
	case 0:
		return ""
	case 1:
		text := descriptions[0]
		return strings.ToUpper(text[:1]) + text[1:] + " was already being charged when you turned this off. A payment already sent to the chain can't be recalled, so it can still be charged this once, but none after it while the switch is off."
	}
	list := strings.Join(descriptions[:len(descriptions)-1], ", ") + " and " + descriptions[len(descriptions)-1]
	return "These automatic charges were already being made when you turned them off: " + list + ". A payment already sent to the chain can't be recalled, so each can still be charged this once, but none after it while its switch is off."
}

// gammaParseUnits parses a decimal amount of gamma into units (at most 9
// decimals). "" is 0.
func gammaParseUnits(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, true
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok || value.Sign() < 0 {
		return 0, false
	}
	value.Mul(value, big.NewRat(gammaUnitsPerGamma, 1))
	if !value.IsInt() || !value.Num().IsInt64() {
		return 0, false
	}
	return value.Num().Int64(), true
}

// gammaSS58 is a public key's SS58 address (prefix 42).
func gammaSS58(publicKey [32]byte) string {
	return subkey.SS58Encode(publicKey[:], model.BittensorSS58Prefix)
}
