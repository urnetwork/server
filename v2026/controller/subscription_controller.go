package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

const InitialTransferBalance = 32 * model.Gib

// 30 days
const InitialTransferBalanceDuration = 30 * 24 * time.Hour

// The recurring per-tier data grants come from pro.yml (model.Pro().DataAmount).
// The free grant is daily (model.FreeGrantWindow) and the referral grant runs
// every referral period (model.ReferralGrantWindow). The Pro grant comes at the
// start of each of the network's own subscription cycles
// (model.GrantProCycleInTx, PRO-GRANT-CYCLE.md): at each credit below, and by the
// hourly RefreshProCycleTransferBalances.
//
// SupporterPeriodTransferBalance is what a paid supporter period's own balance
// holds: nothing. That balance is the period's Pro marker, with the period's
// window, revenue and purchase token, which the end paths, Play's overlap gate
// and the sales history match. It replaced the 600 GiB activation credit
// (owner, 2026-11-11); the period's data is its cycle grant.
const SupporterPeriodTransferBalance = model.ByteCount(0)

const SubscriptionGracePeriod = 24 * time.Hour

// manualPaymentGracePeriod keeps an already-paid Stripe, Solana, or x402 supporter
// entitlement available while a renewal can be paid manually. It does not
// record an unpaid renewal as paid or mint another transfer balance.
const manualPaymentGracePeriod = 30 * 24 * time.Hour

const SubscriptionYearDuration = 365 * 24 * time.Hour

type Skus struct {
	Skus map[string]*Sku `yaml:"skus"`
}

type Sku struct {
	// the fees on the payment amount
	FeeFraction                   float64 `yaml:"fee_fraction"`
	PriceAmountUsd                float64 `yaml:"price_amount_usd,omitempty"`
	BalanceByteCountHumanReadable string  `yaml:"balance_byte_count"`
	Supporter                     bool    `yaml:"supporter"`
}

func (self *Sku) BalanceByteCount() model.ByteCount {
	byteCount, err := model.ParseByteCount(self.BalanceByteCountHumanReadable)
	if err != nil {
		panic(err)
	}
	return byteCount
}

// The key Coinbase signs each webhook body with.
func coinbaseWebhookSharedSecret() (string, error) {
	return vaultNonblankString("coinbase.yml", "webhook", "shared_secret")
}

// Why a Coinbase webhook cannot be verified now, or nil.
func CoinbaseWebhookVerifiable() error {
	_, err := coinbaseWebhookSharedSecret()
	return err
}

var coinbaseSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("coinbase.yml").UnmarshalYaml(&skus)
	return skus.Skus
})

// Replaceable only by hermetic fulfillment tests whose environment does not
// carry coinbase.yml. Production always delegates to the cached config.
var coinbaseSkusFunc = func() map[string]*Sku { return coinbaseSkus() }

// The service account whose Google id token authenticates each Play push.
func playPublisherEmail() (string, error) {
	return vaultNonblankString("google.yml", "webhook", "publisher_email")
}

// The exact audience configured on the authenticated Pub/Sub subscription.
func playAudience() (string, error) {
	return vaultNonblankString("google.yml", "webhook", "audience")
}

// Why a Play push cannot be verified now, or nil.
func PlayWebhookVerifiable() error {
	_, publisherErr := playPublisherEmail()
	_, audienceErr := playAudience()
	return errors.Join(publisherErr, audienceErr)
}

var playPackageName = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["webhook"].(map[string]any)["package_name"].(string)
})

var playSkus = sync.OnceValue(func() map[string]*Sku {
	var skus Skus
	server.Config.RequireSimpleResource("play.yml").UnmarshalYaml(&skus)
	return playCreditableSkus(skus.Skus)
})

// playCreditableSkus are the Play products this server credits: play.yml's,
// except any named like a data pack (data_*). Google Play sells UR Pro only; the
// packs are sold on the rails pro.yml names (data_pack_rails.go), and the stores
// are not one, so the 256 GiB pack cannot be sold through Play.
func playCreditableSkus(skus map[string]*Sku) map[string]*Sku {
	creditable := map[string]*Sku{}
	for productId, sku := range skus {
		if isDataPackItem(productId) {
			glog.Errorf("[sub]play.yml lists %s, a data pack; Google Play sells UR Pro only, so it is never credited\n", productId)
			continue
		}
		creditable[productId] = sku
	}
	return creditable
}

// var companySenderEmail = sync.OnceValue(func() string {
// 	c := server.Config.RequireSimpleResource("email.yml").Parse()
// 	return c["company_sender_email"].(string)
// })

var playClientId = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["client_id"].(string)
})

var playClientSecret = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["client_secret"].(string)
})

var playRefreshToken = sync.OnceValue(func() string {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	return c["oauth"].(map[string]any)["refresh_token"].(string)
})

// app initially calls "get info"
// then if no wallet, show a button to initialize wallet
// if wallet, show a button to refresh, and to withdraw

type SubscriptionBalanceResult struct {
	/*
	 * StartBalanceByteCount - The available balance the user starts the day with
	 */
	StartBalanceByteCount model.ByteCount `json:"start_balance_byte_count"`
	/**
	 * BalanceByteCount - The remaining balance the user has available
	 */
	BalanceByteCount model.ByteCount `json:"balance_byte_count"`
	/**
	 * OpenTransferByteCount - The total number of bytes tied up in open transfers
	 */
	OpenTransferByteCount model.ByteCount `json:"open_transfer_byte_count"`
	/**
	 * CurrentSubscription - ONE of the active subscriptions, or nil.
	 *
	 * Shipped apple/android/windows/linux clients read this through the sdk and
	 * treat it as the plan indicator, so it keeps its exact single-value meaning.
	 * It cannot name more than one store; use Subscriptions for the full set.
	 */
	CurrentSubscription *Subscription `json:"current_subscription,omitempty"`
	/**
	 * Subscriptions - EVERY store currently billing this network, one entry per
	 * store, so a caller can offer a cancel path for each. A user subscribed on
	 * two stores is charged by both and has to cancel in both places.
	 *
	 * Not gated on Pro: an active renewal row means a store is taking money now,
	 * and that is exactly when the cancel path must be reachable, entitlement
	 * bookkeeping notwithstanding.
	 */
	Subscriptions          []*Subscription          `json:"subscriptions,omitempty"`
	ActiveTransferBalances []*model.TransferBalance `json:"active_transfer_balances,omitempty"`
	// Always zero: USDC provider payouts ended (owner decisions 2026-11-12 and
	// 2026-11-13), so no payout is pending. Apps built before then parse it.
	PendingPayoutUsdNanoCents model.NanoCents `json:"pending_payout_usd_nano_cents"`
	UpdateTime                time.Time       `json:"update_time"`
	/**
	 * Guest - the network has no login method (no password/phone, SSO, wallet
	 * or seedphrase sign-in): a legacy guest network. Read from the live auth
	 * tables on every call, so it is right after a token refresh (which signs
	 * every jwt without the guest_mode claim) and turns false the moment a
	 * login method is added with AddAuth. A client must not sell a plan to a
	 * guest network, because nothing can sign back in to it.
	 */
	Guest bool `json:"guest"`

	// ----- the onboarding plan fields (onboarding_controller.go DecoratePlan) -----
	// PriceTier is the caller's regional price tier (display estimate unless the
	// source is a storefront or a billing country).
	PriceTier *PriceTierResult `json:"price_tier,omitempty"`
	// OnboardingOffer is the caller's welcome offer, null when none was issued.
	OnboardingOffer *OnboardingOfferResult `json:"onboarding_offer"`
	// WinbackOffer is the caller's active win-back offer (the re-engagement
	// program's W1, winback_offer_controller.go). Omitted when there is none,
	// or it expired or was redeemed, and from an older server.
	WinbackOffer *WinbackOfferResult `json:"winback_offer,omitempty"`
	// Experiments is the caller's variant per experiment surface.
	Experiments map[string]*model.ExperimentAssignment `json:"experiments,omitempty"`

	// ----- crypto payments (crypto_pay_controller.go) -----
	// CryptoAssets are the coins that can be paid with now and what each sells
	// (the assets list of GET /pay/crypto/assets: never a coin whose quotes are
	// refused for now, since every app offers each coin listed here). Omitted
	// when there is none, so a client older than crypto, or a deployment with
	// crypto off, never shows a coin.
	CryptoAssets []*CryptoPaymentAsset `json:"crypto_assets,omitempty"`
	// CryptoUnavailableAssets are the coins that are on but cannot be paid with
	// now (the unavailable_assets list of GET /pay/crypto/assets), each with
	// available false and its unavailable_reason, for an app that shows a coin
	// greyed out (the "More coins" tab while all four of its coins are
	// unavailable). Never offered. Omitted when there is none.
	CryptoUnavailableAssets []*CryptoPaymentAsset `json:"crypto_unavailable_assets,omitempty"`
	// CryptoTerm is the network's latest one-time crypto credit (Solana,
	// Monero, Zcash, Bittensor, Bitcoin or Ethereum) whose grace has not
	// ended. Apps show the renewal notice while term_end_time <= now <
	// grace_end_time, and never before.
	CryptoTerm *CryptoTerm `json:"crypto_term,omitempty"`

	// ----- failed renewals (subscription_stripe_dunning.go) -----
	// PaymentPastDue is set while a store retries a renewal whose payment did
	// not go through. Apps show the past-due notice while it is present, with
	// an action that opens its update_payment_url. Omitted otherwise, and from
	// an older server.
	PaymentPastDue *PaymentPastDue `json:"payment_past_due,omitempty"`
}

type Subscription struct {
	SubscriptionId server.Id `json:"subscription_id"`
	Store          string    `json:"store"`
	Plan           string    `json:"plan"`
}

func SubscriptionBalance(session *session.ClientSession) (*SubscriptionBalanceResult, error) {
	// at a grant boundary the old grant is still active through its grace; the
	// summary counts only the new one (see model.SummarizeTransferBalances)
	transferBalanceSummary := model.GetTransferBalanceSummary(session.Ctx, session.ByJwt.NetworkId)
	if 0 < transferBalanceSummary.StrandedTransferByteCount {
		// Pending that no current grant reserves belongs to contracts past their
		// deadline. Retire them now rather than when a closer reaches them, so
		// Pending returns while the user is looking (gated per payer).
		model.RequestPayerDueContractClose(session.Ctx, session.ByJwt.NetworkId)
	}

	// Pro comes from pro_model, the single place it is tracked. It is NOT "has a
	// paid balance": a data code is paid but data-only, so that test would report
	// a data-code buyer as Pro.
	isPro := model.IsProNetwork(session.Ctx, session.ByJwt.NetworkId)

	var currentSubscription *Subscription

	_, market := model.HasSubscriptionRenewal(session.Ctx, session.ByJwt.NetworkId, model.SubscriptionTypeSupporter)

	if isPro {
		currentSubscription = &Subscription{
			Plan: model.SubscriptionTypeSupporter,
		}

		if market != nil {
			currentSubscription.Store = *market
		}
	}

	// one entry per store billing this network. MIN(market) above can only name
	// one of them, so a second store would otherwise be invisible -- and keep
	// charging, with nowhere in the ui to go and cancel it.
	subscriptions := []*Subscription{}
	for _, market := range model.GetActiveSubscriptionRenewalMarkets(
		session.Ctx,
		session.ByJwt.NetworkId,
		model.SubscriptionTypeSupporter,
	) {
		subscriptions = append(subscriptions, &Subscription{
			Store: market,
			Plan:  model.SubscriptionTypeSupporter,
		})
	}

	return &SubscriptionBalanceResult{
		BalanceByteCount:       transferBalanceSummary.BalanceByteCount,
		StartBalanceByteCount:  transferBalanceSummary.StartBalanceByteCount,
		OpenTransferByteCount:  transferBalanceSummary.OpenTransferByteCount,
		CurrentSubscription:    currentSubscription,
		Subscriptions:          subscriptions,
		ActiveTransferBalances: transferBalanceSummary.ActiveTransferBalances,
		UpdateTime:             server.NowUtc(),
		Guest:                  isGuestNetwork(session, model.HasAnyAuthMethod),
		PaymentPastDue:         paymentPastDueForNetwork(session.Ctx, session.ByJwt.NetworkId, stripeDunningNow(), stripeDunningSettingsFunc()),
	}, nil
}

// isGuestNetwork reports whether the session's network has no login method.
// It deliberately ignores the jwt's GuestMode claim: RefreshToken signs every
// jwt with GuestMode=false, so a refreshed legacy guest no longer carries it,
// and a guest that just added a login method still carries it until the next
// refresh.
func isGuestNetwork(
	session *session.ClientSession,
	hasAnyAuthMethod func(ctx context.Context, userId server.Id) bool,
) bool {
	return !hasAnyAuthMethod(session.Ctx, session.ByJwt.UserId)
}

// Stable machine codes for a refused checkout or payment intent
// (/stripe/create-checkout-session, /stripe/payment-intent,
// /subscription/stripe/payment-sheet, /solana/payment-intent, /pay/data/checkout,
// /pay/data/solana-intent and the crypto endpoints of crypto_pay_controller.go).
// Clients pick a localized message (or flow) from
// the code and fall back to `Message`, which every refusal still carries, so a
// client older than a code reads the refusal as before.
const (
	// the network has no login method; add one (AddAuth) before buying
	PurchaseErrorCodeGuestSignInRequired = "guest_sign_in_required"
	// the network already has an active Pro subscription; nothing was charged
	PurchaseErrorCodeAlreadySubscribed = "already_subscribed"
	// the plan cannot be sold now: no price or product is configured for it
	PurchaseErrorCodePlanUnavailable = "plan_unavailable"
	// the data pack cannot be sold now: no price or product is configured for it
	PurchaseErrorCodeItemUnavailable = "item_unavailable"
	// the welcome offer is not, or no longer, redeemable by this network
	PurchaseErrorCodeOfferUnavailable = "offer_unavailable"
	// this deployment has no checkout configured
	PurchaseErrorCodeCheckoutUnavailable = "checkout_unavailable"
	// the request names an item, plan, provider, mode or reference the endpoint
	// does not take: a client defect, not something the buyer can change
	PurchaseErrorCodeInvalidRequest = "invalid_request"
	// the email address the code goes to does not look like one
	PurchaseErrorCodeInvalidEmail = "invalid_email"
	// a data purchase for a network by name was sent without the name
	PurchaseErrorCodeNetworkNameRequired = "network_name_required"
	// no network has the given name
	PurchaseErrorCodeNetworkNotFound = "network_not_found"
	// too many attempts from this address; it may try again in a minute
	PurchaseErrorCodeRateLimited = "rate_limited"
	// the payment could not be started (Stripe or the database failed); a
	// retry may work
	PurchaseErrorCodeStartFailed = "start_failed"
	// a crypto asset is switched off, or cannot be quoted right now (rates
	// missing, stale or divergent, the node or the scanner lagging, the
	// address pool empty). UI: "Temporarily unavailable"
	PurchaseErrorCodeAssetUnavailable = "asset_unavailable"
	// a crypto quote's time ran out with nothing received; the UI offers
	// "Refresh quote"
	PurchaseErrorCodeQuoteExpired = "quote_expired"
	// a crypto invoice ended short of its quote; the UI shows the review copy
	PurchaseErrorCodeUnderpaid = "underpaid"
)

const purchaseGuestSignInRequiredMessage = "Add a sign-in to your account before buying a plan."

// The message of a payment that could not be started. The cause is logged, not
// sent: it can carry Stripe's or the database's own words.
const purchaseStartFailedMessage = "Could not start the payment. Please try again."

// The auth-method lookup, model.HasAnyAuthMethod. Replaceable only by hermetic
// tests, which answer the lookup without a database. Production never mutates
// it.
var purchaseHasAnyAuthMethod = model.HasAnyAuthMethod

// Reports whether the session's network must not start a checkout or a
// payment intent: a legacy guest network has no login method, so nothing could
// sign back in to the plan it bought. Current apps convert a guest in place
// before checkout; this covers the builds from before that.
//
// Only the server-created checkouts and intents call this. A store-verified
// purchase (verify-play-purchase, verify-apple-transaction) and every webhook
// still credit a guest network: the store has already taken the money.
func refuseGuestPurchase(session *session.ClientSession) bool {
	if !isGuestNetwork(session, purchaseHasAnyAuthMethod) {
		return false
	}
	if glog.V(1) {
		glog.Infof("[sub]refused a purchase for guest network %s\n", session.ByJwt.NetworkId)
	}
	return true
}

type CoinbaseWebhookArgs struct {
	Event *CoinbaseEvent `json:"event"`
}

type CoinbaseEvent struct {
	Id   string             `json:"id"`
	Type string             `json:"type"`
	Data *CoinbaseEventData `json:"data"`
}

type CoinbaseEventData struct {
	Id          string                      `json:"id"`
	Name        string                      `json:"name"`
	Description string                      `json:"description"`
	Payments    []*CoinbaseEventDataPayment `json:"payments"`
	Checkout    *CoinbaseEventDataCheckout  `json:"checkout"`
	Metadata    *CoinbaseEventDataMetadata  `json:"metadata"`
}

type CoinbaseEventDataCheckout struct {
	Id string `json:"id"`
}

type CoinbaseEventDataMetadata struct {
	Email string `json:"email"`
	// set by /pay/data/checkout when the purchase applies to a named network
	NetworkId      string `json:"network_id"`
	NetworkName    string `json:"network_name"`
	ApplyToNetwork string `json:"apply_to_network"`
}

// coinbaseAppliedNetwork reads the buy-data checkout metadata: the network the
// purchase applies to, when the charge was created for one. A malformed id is
// treated as "no network" so the purchase still delivers as a code by email.
func coinbaseAppliedNetwork(metadata *CoinbaseEventDataMetadata) (redeemNetworkId *server.Id, appliedNetworkName string) {
	if metadata == nil || metadata.ApplyToNetwork != payDataMetadataApplyYes {
		return nil, ""
	}
	networkId, err := server.ParseId(metadata.NetworkId)
	if err != nil {
		return nil, ""
	}
	return &networkId, metadata.NetworkName
}

type CoinbaseEventDataPayment struct {
	Net *CoinbaseEventDataPaymentNet `json:"net"`
}

type CoinbaseEventDataPaymentNet struct {
	Local  *CoinbaseEventDataPaymentAmount `json:"local"`
	Crypto *CoinbaseEventDataPaymentAmount `json:"crypto"`
}

type CoinbaseEventDataPaymentAmount struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type CoinbaseWebhookResult struct {
}

func CoinbaseWebhook(
	coinbaseWebhook *CoinbaseWebhookArgs,
	clientSession *session.ClientSession,
) (*CoinbaseWebhookResult, error) {
	// A signed-but-malformed event must be an ERROR, never a panic: a nil deref
	// here would 500 this delivery and every retry of it, forever, with only a
	// stack trace to find it by. The error is still a non-2xx (Coinbase retries
	// and shows the failure in its dashboard), but it says what is missing.
	if coinbaseWebhook.Event == nil {
		return nil, errors.New("Coinbase event missing.")
	}
	if coinbaseWebhook.Event.Type == "charge:confirmed" {
		if coinbaseWebhook.Event.Data == nil {
			return nil, errors.New("Coinbase event data missing.")
		}
		skuName := coinbaseWebhook.Event.Data.Name
		skus := coinbaseSkusFunc()
		if sku, ok := skus[skuName]; ok {
			purchaseEmail := ""
			var redeemNetworkId *server.Id
			appliedNetworkName := ""
			if metadata := coinbaseWebhook.Event.Data.Metadata; metadata != nil {
				purchaseEmail = metadata.Email
				redeemNetworkId, appliedNetworkName = coinbaseAppliedNetwork(metadata)
			}
			// with a known network the credit lands directly; the email is optional
			if purchaseEmail == "" && redeemNetworkId == nil {
				return nil, errors.New("Missing purchase email to send balance code.")
			}

			coinbaseDataJsonBytes, err := json.Marshal(coinbaseWebhook.Event.Data)
			if err != nil {
				return nil, err
			}

			payments := coinbaseWebhook.Event.Data.Payments
			if len(payments) == 0 || payments[0] == nil || payments[0].Net == nil || payments[0].Net.Local == nil {
				return nil, errors.New("Coinbase event has no payment amount.")
			}

			paymentUsd, err := strconv.ParseFloat(payments[0].Net.Local.Amount, 64)
			if err != nil {
				return nil, err
			}
			netRevenue := model.UsdToNanoCents((1.0 - sku.FeeFraction) * paymentUsd)

			chargeId := coinbaseWebhook.Event.Data.Id
			err = createBalanceCodeForSale(
				clientSession.Ctx,
				sku.BalanceByteCount(),
				model.Pro().DataCodeDuration,
				netRevenue,
				chargeId,
				string(coinbaseDataJsonBytes),
				purchaseEmail,
				// a Coinbase purchase is not tied to a signed-in session: the network is
				// known only when the buy-data checkout named one (charge metadata),
				// otherwise the emailed code IS the delivery mechanism
				redeemNetworkId,
				appliedNetworkName,
				func() *model.SalesPaymentNotice {
					return coinbaseSalesNotice(chargeId, sku.BalanceByteCount(), payments[0], redeemNetworkId)
				},
			)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, fmt.Errorf("Coinbase sku not found: %s", skuName)
		}

	}
	// else ignore

	return &CoinbaseWebhookResult{}, nil
}

// CreateBalanceCode creates the data code for a purchase and emails it.
//
// When redeemNetworkId is set, the code is ALSO redeemed into that network immediately,
// so the data simply lands. That is the case for a purchase made while SIGNED IN, where
// the Stripe checkout session told us exactly whose network it is
// (client_reference_id). The code is still emailed, as a record.
//
// Without this, a signed-in customer who buys data on the site is emailed a code they
// have to go and find and paste back into the app -- and the confirmation page sits
// there polling for a balance that never arrives, eventually telling them their purchase
// is "taking longer than usual" when in fact it worked perfectly. We know who they are.
// Make the data appear.
//
// redeemNetworkId is nil for purchases where we genuinely do not know the network (the
// Coinbase flow), which is what data codes exist for in the first place.
func CreateBalanceCode(
	ctx context.Context,
	balanceByteCount model.ByteCount,
	duration time.Duration,
	netRevenue model.NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
	redeemNetworkId *server.Id,
) error {
	return createBalanceCode(
		ctx,
		balanceByteCount,
		duration,
		netRevenue,
		purchaseEventId,
		purchaseRecord,
		purchaseEmail,
		redeemNetworkId,
		"",
	)
}

// createBalanceCode is CreateBalanceCode plus the "applied" email. When
// appliedNetworkName is set -- the customer bought the data FOR that network from
// the buy-data page -- and the code is on that network, the email says the data
// is already there instead of offering a code to redeem.
func createBalanceCode(
	ctx context.Context,
	balanceByteCount model.ByteCount,
	duration time.Duration,
	netRevenue model.NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
	redeemNetworkId *server.Id,
	appliedNetworkName string,
) error {
	return createBalanceCodeForSale(
		ctx,
		balanceByteCount,
		duration,
		netRevenue,
		purchaseEventId,
		purchaseRecord,
		purchaseEmail,
		redeemNetworkId,
		appliedNetworkName,
		nil,
	)
}

// createBalanceCodeForSale is createBalanceCode for a payment: salesNotice,
// when set, builds the sales notice registered with the code's creation, so
// it is posted once the new code commits and never for a retry that finds the
// code already made.
func createBalanceCodeForSale(
	ctx context.Context,
	balanceByteCount model.ByteCount,
	duration time.Duration,
	netRevenue model.NanoCents,
	purchaseEventId string,
	purchaseRecord string,
	purchaseEmail string,
	redeemNetworkId *server.Id,
	appliedNetworkName string,
	salesNotice func() *model.SalesPaymentNotice,
) error {
	// This is a PAID path -- by the time we are here the customer's money has already
	// moved. So this one does NOT no-op like the grants do.
	//
	// A code with a zero duration expires the instant it is created: the customer pays,
	// receives a code, redeems it, and gets nothing, with no error anywhere. Refuse
	// instead. The caller is a webhook, so an error means the provider RETRIES and the
	// failure is visible in their dashboard -- an unfulfilled payment we can see and fix
	// beats a fulfilled one that is worthless.
	if duration <= 0 {
		glog.Errorf(
			"[sub]refusing to create a balance code with a zero duration "+
				"(purchase_event_id = %s). Is pro.yml present?\n",
			purchaseEventId,
		)
		return fmt.Errorf("balance code duration is not configured (pro.yml)")
	}

	// With no email AND no network there is no delivery mechanism at all -- the
	// code would exist and nobody could ever learn it. Refuse so the webhook
	// retries and the failure is visible.
	if purchaseEmail == "" && redeemNetworkId == nil {
		return fmt.Errorf("balance code needs a purchase email or a network to redeem into")
	}

	var balanceCode *model.BalanceCode

	if balanceCodeId, err := model.GetBalanceCodeIdForPurchaseEventId(ctx, purchaseEventId); err == nil {
		// the code was already created for this purchase event -- a webhook retry.
		// Re-send it, and fall through so an earlier failed redeem is retried too.
		balanceCode, err = model.GetBalanceCode(ctx, balanceCodeId)
		if err != nil {
			return err
		}
	} else {
		var createdInTx func(tx server.PgTx)
		if salesNotice != nil {
			createdInTx = func(tx server.PgTx) {
				addSalesPaymentNoticeInTx(tx, salesNotice)
			}
		}
		balanceCode, err = model.CreateBalanceCodeForSale(
			ctx,
			balanceByteCount,
			duration,
			netRevenue,
			purchaseEventId,
			purchaseRecord,
			purchaseEmail,
			createdInTx,
		)
		if err != nil {
			return err
		}
	}

	// A normal webhook retry reaches this call after the first redemption has
	// committed. Preserve that state instead of asking the redeem endpoint,
	// whose public unavailable-code result deliberately does not distinguish a
	// spent code from an unknown one.
	applied := redeemNetworkId != nil &&
		balanceCode.RedeemNetworkId != nil &&
		*balanceCode.RedeemNetworkId == *redeemNetworkId &&
		!balanceCode.RedeemTime.IsZero()
	if redeemNetworkId != nil && !applied {
		redeemResult, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
			Secret:    balanceCode.Secret,
			NetworkId: *redeemNetworkId,
		}, ctx)
		if err != nil {
			// Do NOT fail the webhook here. Stripe retries a failed webhook, and every
			// retry would re-send the email -- so a transient redeem error would turn
			// into a stream of duplicate emails. The most likely "error" is simply that
			// the code is already redeemed (this IS the retry), in which case the data is
			// already where it belongs.
			//
			// The customer is not stranded either way: they hold the emailed code and can
			// redeem it by hand.
			glog.Infof(
				"[sub]balance code %s redeem into network %s: %s\n",
				balanceCode.BalanceCodeId, *redeemNetworkId, err,
			)
			if balanceCode.PurchaseEmail == "" {
				return fmt.Errorf("automatic balance-code delivery failed without email recovery: %w", err)
			}
		} else if redeemResult != nil && redeemResult.Error != nil {
			// "already redeemed" IS the retry: the data is already where it belongs.
			// Anything else (expired, voided by a refund) means it is not.
			applied = strings.Contains(strings.ToLower(redeemResult.Error.Message), "already redeemed")
			glog.Infof(
				"[sub]balance code %s redeem into network %s: %s\n",
				balanceCode.BalanceCodeId, *redeemNetworkId, redeemResult.Error.Message,
			)
			if !applied && balanceCode.PurchaseEmail == "" {
				return errors.New("automatic balance-code delivery failed without email recovery")
			}
		} else {
			applied = true
			glog.Infof(
				"[sub]balance code %s redeemed into network %s (%s)\n",
				balanceCode.BalanceCodeId, *redeemNetworkId,
				model.ByteCountHumanReadable(balanceCode.BalanceByteCount),
			)
		}
	}

	// No email on the purchase means automatic redemption is the only delivery
	// channel. Every unsuccessful branch above returns an error, so reaching
	// here proves the data landed in the requested network.
	if balanceCode.PurchaseEmail == "" {
		return nil
	}

	awsMessageSender := GetAWSMessageSender()

	if applied && appliedNetworkName != "" {
		// bought FOR this network from the buy-data page, and the data is there:
		// say so rather than offer a code that is already spent
		return awsMessageSender.SendAccountMessageTemplate(
			balanceCode.PurchaseEmail,
			&SubscriptionDataAppliedTemplate{
				Secret:           balanceCode.Secret,
				BalanceByteCount: balanceCode.BalanceByteCount,
				NetworkName:      appliedNetworkName,
			},
		)
	}

	return awsMessageSender.SendAccountMessageTemplate(
		balanceCode.PurchaseEmail,
		&SubscriptionTransferBalanceCodeTemplate{
			Secret:           balanceCode.Secret,
			BalanceByteCount: balanceCode.BalanceByteCount,
		},
	)
}

type RedeemBalanceCodeArgs struct {
	Secret string `json:"secret"`
}

func RedeemBalanceCode(
	redeemBalanceCode RedeemBalanceCodeArgs,
	session *session.ClientSession,
) (*model.RedeemBalanceCodeResult, error) {

	return model.RedeemBalanceCode(
		&model.RedeemBalanceCodeArgs{
			Secret:    redeemBalanceCode.Secret,
			NetworkId: session.ByJwt.NetworkId,
		},
		session.Ctx,
	)
}

// https://developers.google.com/android-publisher/authorization
func playAuth(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Add("grant_type", "refresh_token")
	form.Add("client_id", playClientId())
	form.Add("client_secret", playClientSecret())
	form.Add("refresh_token", playRefreshToken())

	result, err := server.HttpPostForm(
		ctx,
		"https://accounts.google.com/o/oauth2/token",
		form,
		server.NoCustomHeaders,
		server.ResponseJsonObject[map[string]any],
	)
	if err != nil {
		return "", err
	}

	tokenType := result["token_type"]
	accessToken := result["access_token"]

	if tokenType == "Bearer" {
		return fmt.Sprintf("Bearer %s", accessToken), nil
	}
	return "", errors.New("Could not auth.")
}

func playAuthHeader(ctx context.Context, header http.Header) {
	if auth, err := playAuth(ctx); err == nil {
		header.Add("Authorization", auth)
	}
}

type PlayRtdnMessage struct {
	Version                  string                        `json:"version"`
	PackageName              string                        `json:"packageName"`
	SubscriptionNotification *PlaySubscriptionNotification `json:"subscriptionNotification,omitempty"`
}

type PlaySubscriptionNotification struct {
	Version          string `json:"version"`
	NotificationType int    `json:"notificationType"`
	PurchaseToken    string `json:"purchaseToken"`
	SubscriptionId   string `json:"subscriptionId"`
}

// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2#SubscriptionPurchaseV2
type PlaySubscription struct {
	LineItems []*PlaySubscriptionPurchaseLineItem `json:"lineItems"`
	StartTime string                              `json:"startTime"`
	// RegionCode is the ISO 3166-1 alpha-2 billing country of the purchase: the
	// storefront that prices the regional tier
	RegionCode string `json:"regionCode,omitempty"`
	// values:
	// - SUBSCRIPTION_STATE_UNSPECIFIED
	// - SUBSCRIPTION_STATE_PENDING
	// - SUBSCRIPTION_STATE_ACTIVE
	// - SUBSCRIPTION_STATE_PAUSED
	// - SUBSCRIPTION_STATE_IN_GRACE_PERIOD
	// - SUBSCRIPTION_STATE_ON_HOLD
	// - SUBSCRIPTION_STATE_CANCELED
	// - SUBSCRIPTION_STATE_EXPIRED
	// - SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED
	SubscriptionState string `json:"subscriptionState"`
	// values:
	// - ACKNOWLEDGEMENT_STATE_UNSPECIFIED
	// - ACKNOWLEDGEMENT_STATE_PENDING
	// - ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED
	AcknowledgementState       string                          `json:"acknowledgementState"`
	ExternalAccountIdentifiers *PlayExternalAccountIdentifiers `json:"externalAccountIdentifiers"`
	SubscribeWithGoogleInfo    *PlaySubscribeWithGoogleInfo    `json:"subscribeWithGoogleInfo,omitempty"`
	// LinkedPurchaseToken is the token of the subscription this purchase
	// continues (re-signup, upgrade/downgrade, plan conversion); an unlinked
	// token resolves through its binding (play_purchase_binding)
	LinkedPurchaseToken string `json:"linkedPurchaseToken,omitempty"`
	// LatestOrderId is the order id of the latest charge (GPA....), the id on
	// the customer's Google Play receipt
	LatestOrderId string `json:"latestOrderId,omitempty"`
	// OutOfAppPurchaseContext is set on a re-subscription bought in the Play
	// Store's subscriptions center ("Resubscribe") after the previous
	// subscription of the product expired, until the purchase is
	// acknowledged. Such a purchase carries no account identifiers of its own
	// (playOutOfAppNetworkIdInConn).
	OutOfAppPurchaseContext *PlayOutOfAppPurchaseContext `json:"outOfAppPurchaseContext,omitempty"`
	// TestPurchase is present only on a license tester's purchase: no money
	// moves for it, so it posts no sales notice
	TestPurchase json.RawMessage `json:"testPurchase,omitempty"`
}

// PlayOutOfAppPurchaseContext names the expired subscription a
// re-subscription follows: its account identifiers (the obfuscated account id
// the app set on the original purchase) and its token.
type PlayOutOfAppPurchaseContext struct {
	ExpiredExternalAccountIdentifiers *PlayExternalAccountIdentifiers `json:"expiredExternalAccountIdentifiers,omitempty"`
	ExpiredPurchaseToken              string                          `json:"expiredPurchaseToken,omitempty"`
}

// playAcknowledgeSubscription acknowledges the purchase with Google, unless
// Play already reports it acknowledged. Every renewal RTDN is for an
// acknowledged purchase; acknowledging it again would make each renewal depend
// on Play accepting a repeat, and a non-200 there would fail every redelivery
// while the renewal is never credited inline.
func playAcknowledgeSubscription(
	ctx context.Context,
	rtdnMessage *PlayRtdnMessage,
	sub *PlaySubscription,
) error {
	if sub.AcknowledgementState == "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED" {
		return nil
	}
	url := fmt.Sprintf(
		"%s/androidpublisher/v3/applications/%s/purchases/subscriptions/%s/tokens/%s:acknowledge",
		playPublisherApiBaseUrl,
		rtdnMessage.PackageName,
		rtdnMessage.SubscriptionNotification.SubscriptionId,
		rtdnMessage.SubscriptionNotification.PurchaseToken,
	)
	_, err := server.HttpPostRawRequireStatusOk(
		ctx,
		url,
		[]byte{},
		func(header http.Header) {
			playAuthHeaderFunc(ctx, header)
		},
	)
	if err != nil {
		glog.Errorf(
			"[sub]play acknowledge failed for token %s: %s\n",
			rtdnMessage.SubscriptionNotification.PurchaseToken, err,
		)
		return fmt.Errorf("could not acknowledge play purchase: %w", err)
	}
	return nil
}

func (self *PlaySubscription) ParseStartTime() (time.Time, error) {
	return time.Parse(time.RFC3339, self.StartTime)
}

func (self *PlaySubscription) RequireStartTime() time.Time {
	t, err := self.ParseStartTime()
	if err != nil {
		panic(err)
	}
	return t
}

type PlayExternalAccountIdentifiers struct {
	ExternalAccountId           string `json:"externalAccountId,omitempty"`
	ObfuscatedExternalAccountId string `json:"obfuscatedExternalAccountId,omitempty"`
	ObfuscatedExternalProfileId string `json:"obfuscatedExternalProfileId,omitempty"`
}

type PlaySubscribeWithGoogleInfo struct {
	EmailAddress string `json:"emailAddress,omitempty"`
}

type PlaySubscriptionPurchaseLineItem struct {
	ProductId  string `json:"productId"`
	ExpiryTime string `json:"expiryTime"`
	// the order id of the latest successful charge for the item (a renewal's
	// order id ends in "..N"); read for the sales notice
	LatestSuccessfulOrderId string `json:"latestSuccessfulOrderId,omitempty"`
	// present for auto-renewing plans (absent for prepaid): the customer's
	// auto-renew switch, read by the subscription details
	AutoRenewingPlan *PlayAutoRenewingPlan `json:"autoRenewingPlan,omitempty"`
	// OfferDetails names the base plan and offer the purchase was made under;
	// the welcome offer is recognized by its offer tag
	OfferDetails *PlayOfferDetails `json:"offerDetails,omitempty"`
	// SignupPromotion is set when a Play promotion code was applied at signup
	SignupPromotion *PlaySignupPromotion `json:"signupPromotion,omitempty"`
	// OfferPhase is the phase of the offer the item is in now, when Play
	// reports it; read by the sales notice only
	OfferPhase *PlayOfferPhase `json:"offerPhase,omitempty"`
}

// PlayOfferPhase names exactly one phase: the free trial, an introductory
// price, the base price, or a proration period. Only which one is set is read.
type PlayOfferPhase struct {
	FreeTrial         json.RawMessage `json:"freeTrial,omitempty"`
	IntroductoryPrice json.RawMessage `json:"introductoryPrice,omitempty"`
	BasePrice         json.RawMessage `json:"basePrice,omitempty"`
	ProrationPeriod   json.RawMessage `json:"prorationPeriod,omitempty"`
}

// reported is whether Play named a phase at all.
func (self *PlayOfferPhase) reported() bool {
	return self != nil && (playJsonPresent(self.FreeTrial) || playJsonPresent(self.IntroductoryPrice) ||
		playJsonPresent(self.BasePrice) || playJsonPresent(self.ProrationPeriod))
}

// freeTrial is whether Play reports the item in its offer's free trial.
func (self *PlayOfferPhase) freeTrial() bool {
	return self != nil && playJsonPresent(self.FreeTrial)
}

// playJsonPresent is whether a field Play may leave out was there, and not
// null.
func playJsonPresent(raw json.RawMessage) bool {
	return 0 < len(raw) && string(raw) != "null"
}

// testPurchase is whether the purchase is a license tester's.
func (self *PlaySubscription) testPurchase() bool {
	return playJsonPresent(self.TestPurchase)
}

// PlaySignupPromotion is exactly one of oneTimeCode (a single-use code; the
// API reports no identifier for it) or vanityCode (a custom code, named).
type PlaySignupPromotion struct {
	OneTimeCode *struct{}       `json:"oneTimeCode,omitempty"`
	VanityCode  *PlayVanityCode `json:"vanityCode,omitempty"`
}

type PlayVanityCode struct {
	PromotionCode string `json:"promotionCode,omitempty"`
}

type PlayOfferDetails struct {
	OfferTags  []string `json:"offerTags,omitempty"`
	BasePlanId string   `json:"basePlanId,omitempty"`
	OfferId    string   `json:"offerId,omitempty"`
}

// HasOfferTag reports whether any line item was bought under an offer carrying
// the tag.
func (self *PlaySubscription) HasOfferTag(tag string) bool {
	if tag == "" {
		return false
	}
	for _, item := range self.LineItems {
		if item == nil || item.OfferDetails == nil {
			continue
		}
		for _, itemTag := range item.OfferDetails.OfferTags {
			if itemTag == tag {
				return true
			}
		}
	}
	return false
}

type PlayAutoRenewingPlan struct {
	AutoRenewEnabled bool `json:"autoRenewEnabled"`
	// the plan's recurring price, a Money (before tax and discounts); kept
	// raw and read leniently by the sales notice only
	RecurringPrice json.RawMessage `json:"recurringPrice,omitempty"`
}

func (self *PlaySubscriptionPurchaseLineItem) ParseExpiryTime() (time.Time, error) {
	return time.Parse(time.RFC3339, self.ExpiryTime)
}

func (self *PlaySubscriptionPurchaseLineItem) RequireExpiryTime() time.Time {
	t, err := self.ParseExpiryTime()
	if err != nil {
		panic(err)
	}
	return t
}

type PlayWebhookArgs struct {
	Message *PlayWebhookMessage `json:"message"`
}

type PlayWebhookMessage struct {
	Data string `json:"data"`
}

type PlayWebhookResultMessage struct {
	Message string `json:"message"`
}

type PlayWebhookResult struct {
	Message *PlayWebhookResultMessage
}

// SUBSCRIPTION_REVOKED (RTDN notificationType 12): the user was refunded and
// access ends NOW (UPGRADE.md §2 S7). Distinct from SUBSCRIPTION_EXPIRED (13),
// the normal end of a paid period, which never claws anything back.
const playRtdnNotificationTypeRevoked = 12

// Replaceable only by the hermetic Play webhook tests, which stand up a fake
// Android Publisher API (this test env has no google vault/config to read the
// real package name, skus or oauth credentials from). Production never mutates
// these.
var playPublisherApiBaseUrl = "https://androidpublisher.googleapis.com"
var playPackageNameFunc = playPackageName
var playSkusFunc = func() map[string]*Sku { return playSkus() }
var playAuthHeaderFunc = playAuthHeader

// The onboarding trial outcome writers the webhook calls. Replaceable only by
// the hermetic Play webhook tests, which record the outcome instead of reading
// the network's onboarding events; production never mutates them.
var playStoreTrialCancelled = storeTrialCancelled
var playStoreTrialConverted = storeTrialConverted

// Replaceable only by the terminal-renewal error-path test. Production always
// ends through the network-and-purchase-token-scoped model transaction.
var endPlaySubscriptionEntitlement = func(
	connOwner server.PgConn,
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
	now time.Time,
) (bool, error) {
	return model.EndReconciledEntitlementForNetworkPurchaseTokenInConn(
		connOwner,
		ctx,
		networkId,
		model.SubscriptionMarketGoogle,
		purchaseToken,
		now,
	)
}

// https://developer.android.com/google/play/billing/getting-ready#configure-rtdn
// https://developer.android.com/google/play/billing/rtdn-reference
//
// A verified push is accepted when it is processed, else it is an error the
// route answers 500 and Pub/Sub redelivers (store_notification_flow.go).
func PlayWebhook(
	webhookArgs *PlayWebhookArgs,
	clientSession *session.ClientSession,
) (*PlayWebhookResult, error) {
	result, err := playWebhook(webhookArgs, clientSession)
	if err != nil {
		RecordStoreNotificationOutcome(clientSession.Ctx, model.SubscriptionMarketGoogle, StoreNotificationError)
	} else {
		RecordStoreNotificationOutcome(clientSession.Ctx, model.SubscriptionMarketGoogle, StoreNotificationAccepted)
	}
	return result, err
}

// Processes one verified push, as the route always has; PlayWebhook records
// its outcome.
func playWebhook(
	webhookArgs *PlayWebhookArgs,
	clientSession *session.ClientSession,
) (*PlayWebhookResult, error) {

	data, err := base64.StdEncoding.DecodeString(webhookArgs.Message.Data)
	if err != nil {
		return nil, err
	}
	var rtdnMessage *PlayRtdnMessage
	err = json.Unmarshal(data, &rtdnMessage)
	if err != nil {
		return nil, err
	}

	if rtdnMessage.PackageName == playPackageNameFunc() {
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2/get
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptionsv2#SubscriptionPurchaseV2
		// https://developers.google.com/android-publisher/api-ref/rest/v3/purchases.subscriptions/acknowledge
		if rtdnMessage.SubscriptionNotification != nil {
			url := fmt.Sprintf(
				"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
				playPublisherApiBaseUrl,
				rtdnMessage.PackageName,
				rtdnMessage.SubscriptionNotification.PurchaseToken,
			)
			sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
				clientSession.Ctx,
				url,
				func(header http.Header) {
					playAuthHeaderFunc(clientSession.Ctx, header)
				},
				server.ResponseJsonObject[*PlaySubscription],
			)
			if err != nil {
				if v, ok := err.(*server.HttpStatusError); ok {
					switch v.StatusCode {
					// Gone
					case 410:
						if rtdnMessage.SubscriptionNotification.NotificationType == playRtdnNotificationTypeRevoked {
							// a revoked purchase can be gone from the store
							// entirely; the renewal rows still map the token to
							// its network
							return playHandleRevoked(
								clientSession,
								rtdnMessage.SubscriptionNotification.PurchaseToken,
								"GONE",
							)
						}
						return &PlayWebhookResult{}, nil
					default:
						return nil, err
					}
				} else {
					return nil, err
				}
			}

			glog.Infof("[sub]google play sub: %v\n", sub)

			if rtdnMessage.SubscriptionNotification.NotificationType == playRtdnNotificationTypeRevoked {
				// verified against Google before clawing back: the RTDN push is
				// only as trusted as the state fetch above confirms. A revoked
				// subscription reports EXPIRED (or is 410, handled above); if
				// Play still says ACTIVE, do nothing -- later signals will tell.
				if sub.SubscriptionState == "SUBSCRIPTION_STATE_ACTIVE" {
					glog.Warningf(
						"[sub]play REVOKED notification for token %s but Play reports %s; ignoring\n",
						rtdnMessage.SubscriptionNotification.PurchaseToken,
						sub.SubscriptionState,
					)
					return &PlayWebhookResult{}, nil
				}
				return playHandleRevoked(
					clientSession,
					rtdnMessage.SubscriptionNotification.PurchaseToken,
					sub.SubscriptionState,
				)
			}

			if len(sub.LineItems) == 0 {
				glog.Infof("Google play cannot not renew subscription with zero line items (%s)", rtdnMessage.SubscriptionNotification.PurchaseToken)
				return &PlayWebhookResult{
					Message: &PlayWebhookResultMessage{
						Message: fmt.Sprintf(
							"Google play cannot not renew subscription with zero line items (%s), sub state: (%s), sub aknowledgement: (%s)",
							rtdnMessage.SubscriptionNotification.PurchaseToken,
							sub.SubscriptionState,
							sub.AcknowledgementState,
						),
					},
				}, nil
			}

			var networkId server.Id
			identifiers := sub.ExternalAccountIdentifiers
			if identifiers != nil && identifiers.ExternalAccountId != "" {
				networkId, err = server.ParseId(identifiers.ExternalAccountId)
				if err != nil {
					return nil, fmt.Errorf("Google Play subscription malformed external account id: \"%s\" = %s", identifiers.ExternalAccountId, err)
				}
			} else if identifiers != nil && identifiers.ObfuscatedExternalAccountId != "" {
				networkIdOrSubscriptionPaymentId, err := server.ParseId(identifiers.ObfuscatedExternalAccountId)
				if err != nil {
					return nil, fmt.Errorf("Google Play subscription malformed obfuscated external account id: \"%s\" = %s", identifiers.ObfuscatedExternalAccountId, err)
				}
				networkId, err = model.SubscriptionGetNetworkIdForPaymentId(clientSession.Ctx, networkIdOrSubscriptionPaymentId)
				if err != nil {
					// the obfuscated account id is just a plain network id
					networkId = networkIdOrSubscriptionPaymentId
				}
			} else {
				// no account identifiers (a purchase outside the app's billing
				// flow): only a binding made by the verify endpoint names the
				// network (play_purchase_binding_controller.go), or, for a
				// Play Store "Resubscribe", the expired subscription's account
				// identifiers, which bind it
				boundNetworkId, bound := playPurchaseBindingLookupFunc(nil,
					clientSession.Ctx,
					rtdnMessage.SubscriptionNotification.PurchaseToken,
					sub.LinkedPurchaseToken,
					true,
				)
				if !bound {
					resubscribedNetworkId, ok := playOutOfAppNetworkIdFunc(nil, clientSession, sub, rtdnMessage.SubscriptionNotification.PurchaseToken)
					if !ok {
						return nil, fmt.Errorf("Google Play re-subscription malformed expired account identifiers")
					}
					if resubscribedNetworkId != nil {
						boundNetworkId, bound = *resubscribedNetworkId, true
					}
				}
				if !bound {
					// paid but nothing names its network: keep it visible to the
					// owner (a later verify binding credits it and resolves it)
					playRecordUnownedPurchase(
						clientSession.Ctx,
						rtdnMessage.SubscriptionNotification.PurchaseToken,
						sub,
						"destination_unresolved",
						nil,
					)
					return &PlayWebhookResult{
						Message: &PlayWebhookResultMessage{
							Message: fmt.Sprintf(
								"Google Play subscription no external account information and no binding: sub state: (%s), sub aknowledgement: (%s)",
								sub.SubscriptionState,
								sub.AcknowledgementState,
							),
						},
					}, nil
				}
				networkId = boundNetworkId
			}

			minExpiryTime := sub.LineItems[0].RequireExpiryTime()
			for _, item := range sub.LineItems[1:] {
				if item.RequireExpiryTime().Before(minExpiryTime) {
					minExpiryTime = item.RequireExpiryTime()
				}
			}

			// the trial outcomes' plan is the purchase's base plan: the product
			// id ("supporter") sells both plans
			plan := playPlanForSubscription(sub, rtdnMessage.SubscriptionNotification.SubscriptionId)

			acknowledgeAndCheckRenewal := true
			switch sub.SubscriptionState {
			case "SUBSCRIPTION_STATE_CANCELED",
				"SUBSCRIPTION_STATE_EXPIRED",
				"SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED":
				// the onboarding trial outcome: a trial that stops renewing or
				// expires before its first paid period is trial.cancelled
				playStoreTrialCancelled(
					clientSession.Ctx,
					networkId,
					model.OnboardingStorePlay,
					plan,
					server.NowUtc(),
				)
				// cancelled is not expired: paid time left is acknowledged (or Google
				// refunds it after three days) and credited like an active purchase
				acknowledgeAndCheckRenewal = playPurchasePaid(sub, server.NowUtc())
			}

			if acknowledgeAndCheckRenewal {
				// Aknowledge. The result MATTERS: a purchase Google never sees
				// acknowledged is auto-refunded after 3 days while any granted
				// balance would stand. A failure here is a non-2xx so Pub/Sub
				// redelivers and the acknowledge is attempted again.
				if err := playAcknowledgeSubscription(clientSession.Ctx, rtdnMessage, sub); err != nil {
					return nil, err
				}

				// fire this immediately since we pull current plan from subscription_renewal table.
				// A credit failure (sku missing from play.yml, Google API failure) is a
				// non-2xx so Pub/Sub RETRIES the delivery -- otherwise the entitlement
				// would arrive only via the task scheduled at the END of the paid
				// period, or never.
				renewalResult, err := PlaySubscriptionRenewal(
					&PlaySubscriptionRenewalArgs{
						NetworkId:      networkId,
						PackageName:    rtdnMessage.PackageName,
						SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
						PurchaseToken:  rtdnMessage.SubscriptionNotification.PurchaseToken,
					},
					clientSession,
				)
				if err != nil {
					glog.Errorf(
						"[sub]play inline renewal failed for token %s: %s\n",
						rtdnMessage.SubscriptionNotification.PurchaseToken, err,
					)
					if errors.Is(err, model.ErrPaymentNetworkNotFound) {
						// paid for a network that was deleted: never credit
						// another one; keep it visible to the owner
						playRecordUnownedPurchase(
							clientSession.Ctx,
							rtdnMessage.SubscriptionNotification.PurchaseToken,
							sub,
							"destination_deleted",
							&networkId,
						)
					}
					return nil, err
				}
				if renewalResult != nil && renewalResult.Renewed {
					// the onboarding trial outcome: a credited renewal past a
					// recorded trial's length is trial.converted
					playStoreTrialConverted(
						clientSession.Ctx,
						networkId,
						model.OnboardingStorePlay,
						plan,
						server.NowUtc(),
					)
				}

				// continually renew as long as the expiry time keeps getting pushed forward
				// note RTDN messages for renewal may unreliably delivered, so Google
				// recommends polling their system around the expiry time
				server.Tx(clientSession.Ctx, func(tx server.PgTx) {
					SchedulePlaySubscriptionRenewal(
						clientSession,
						tx,
						&PlaySubscriptionRenewalArgs{
							NetworkId:      networkId,
							PackageName:    rtdnMessage.PackageName,
							SubscriptionId: rtdnMessage.SubscriptionNotification.SubscriptionId,
							PurchaseToken:  rtdnMessage.SubscriptionNotification.PurchaseToken,
							CheckTime:      minExpiryTime,
						},
					)
				})
			}
		}
	}
	// else unknown package, ignore the message

	return &PlayWebhookResult{}, nil
}

// playRecordUnownedPurchaseFunc is the durable-record seam for paid Play
// purchases no live network can be credited for. Replaceable only by hermetic
// tests; production never mutates it.
var playRecordUnownedPurchaseFunc = model.AddPaymentReconciliationEventOnce

// playPurchasePaid reports whether the purchase holds paid time the customer
// is entitled to now. cancelled ≠ expired: a cancel with time left is paid.
func playPurchasePaid(sub *PlaySubscription, now time.Time) bool {
	switch sub.SubscriptionState {
	case "SUBSCRIPTION_STATE_ACTIVE", "SUBSCRIPTION_STATE_IN_GRACE_PERIOD":
		return true
	case "SUBSCRIPTION_STATE_CANCELED":
		for _, item := range sub.LineItems {
			if expiryTime, err := item.ParseExpiryTime(); err == nil && expiryTime.After(now) {
				return true
			}
		}
	}
	return false
}

// playRecordUnownedPurchase records, once per purchase token, a paid Play
// purchase that no live network can be credited for: no account identifiers
// and no binding (destination_unresolved), or an account link naming a
// deleted network (destination_deleted). Google keeps the money once the
// purchase is acknowledged, so it must stay visible to the owner; nothing here
// picks an owner. Pub/Sub redeliveries find the one row, and a purchase the
// verify endpoint binds later is credited and bound, which resolves the row
// for the monitor. The order id is the one on the customer's receipt. Never
// fails the caller.
func playRecordUnownedPurchase(
	ctx context.Context,
	purchaseToken string,
	sub *PlaySubscription,
	reason string,
	networkId *server.Id,
) {
	if purchaseToken == "" || len(sub.LineItems) == 0 || !playPurchasePaid(sub, server.NowUtc()) {
		return
	}
	details := map[string]any{
		"reason":             reason,
		"leg":                "webhook",
		"subscription_state": sub.SubscriptionState,
		"product":            sub.LineItems[0].ProductId,
		"start_time":         sub.StartTime,
		"expiry_time":        sub.LineItems[0].ExpiryTime,
	}
	if sub.LatestOrderId != "" {
		details["order"] = sub.LatestOrderId
	}
	if sub.RegionCode != "" {
		details["region"] = sub.RegionCode
	}
	added, err := playRecordUnownedPurchaseFunc(ctx, &model.PaymentReconciliationEvent{
		RunId:     server.NewId(),
		Store:     model.SubscriptionMarketGoogle,
		NetworkId: networkId,
		Action:    model.PaymentReconcileActionCreditUnfulfillable,
		Evidence:  purchaseToken,
		Details:   details,
	})
	if err != nil {
		glog.Errorf("[sub]play purchase %s: could not record the unowned payment: %s\n", sub.LatestOrderId, err)
		return
	}
	if added {
		glog.Errorf(
			"[sub]play purchase %s is paid but no live network can be credited (%s); recorded for support as credit_unfulfillable\n",
			sub.LatestOrderId,
			reason,
		)
	}
}

// playHandleRevoked ends a revoked purchase's entitlement: the renewals the
// purchase token bought and the pro balances they granted, with the network
// derived from the renewal rows themselves (a revoked token can be 410-Gone
// at the store). Idempotent: a Pub/Sub redelivery finds nothing left to end
// and records nothing. Always 200 -- a retry cannot do more.
func playHandleRevoked(
	clientSession *session.ClientSession,
	purchaseToken string,
	subscriptionState string,
) (*PlayWebhookResult, error) {
	endedNetworkIds := model.EndReconciledEntitlementForPurchaseToken(
		clientSession.Ctx,
		model.SubscriptionMarketGoogle,
		purchaseToken,
		server.NowUtc(),
	)
	for _, networkId := range endedNetworkIds {
		if err := model.AddPaymentReconciliationEvent(clientSession.Ctx, &model.PaymentReconciliationEvent{
			RunId:     server.NewId(),
			Store:     model.SubscriptionMarketGoogle,
			NetworkId: &networkId,
			Action:    model.PaymentReconcileActionRevoked,
			Evidence:  purchaseToken,
			Details: map[string]any{
				"subscription_state": subscriptionState,
			},
		}); err != nil {
			// the audit trail must never turn a completed clawback into a
			// failed delivery (Pub/Sub would redeliver into a no-op)
			glog.Errorf("[sub]play revoked token %s: could not record event: %s\n", purchaseToken, err)
		}
		// the onboarding refund outcome
		RecordRefund(clientSession.Ctx, networkId, model.OnboardingStorePlay, 0)
	}
	glog.Infof(
		"[sub]play revoked token %s (%s): ended %d network(s)\n",
		purchaseToken, subscriptionState, len(endedNetworkIds),
	)
	return &PlayWebhookResult{}, nil
}

type PlaySubscriptionRenewalArgs struct {
	NetworkId      server.Id `json:"network_id"`
	PackageName    string    `json:"package_name"`
	SubscriptionId string    `json:"subscription_id"`
	PurchaseToken  string    `json:"purchase_token"`
	CheckTime      time.Time `json:"check_time"`
	// ExpiryTime time.Time `json:"expiry_time"`
}

type PlaySubscriptionRenewalResult struct {
	Canceled         bool      `json:"canceled"`
	EntitlementEnded bool      `json:"entitlement_ended"`
	ExpiryTime       time.Time `json:"expiry_time"`
	Renewed          bool      `json:"renewed"`
	Terminal         bool      `json:"terminal"`
	// Play's grace period: the renewal's charge failed and Play is retrying it,
	// with the customer entitled through ExpiryTime
	InGrace bool `json:"in_grace,omitempty"`
	// this call wrote the grace period's hold (playHoldGraceEntitlementInTx)
	GraceHeld bool `json:"grace_held,omitempty"`
}

func SchedulePlaySubscriptionRenewal(
	clientSession *session.ClientSession,
	tx server.PgTx,
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
) {
	task.ScheduleTaskInTx(
		tx,
		PlaySubscriptionRenewal,
		playSubscriptionRenewal,
		clientSession,
		task.RunOnce("play_subscription_renewal", playSubscriptionRenewal.PurchaseToken),
		task.RunAt(playSubscriptionRenewal.CheckTime),
	)
}

func PlaySubscriptionRenewal(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	clientSession *session.ClientSession,
) (*PlaySubscriptionRenewalResult, error) {
	return PlaySubscriptionRenewalInConn(nil, playSubscriptionRenewal, clientSession)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func PlaySubscriptionRenewalInConn(connOwner server.PgConn,
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	clientSession *session.ClientSession,
) (*PlaySubscriptionRenewalResult, error) {

	url := fmt.Sprintf(
		"%s/androidpublisher/v3/applications/%s/purchases/subscriptionsv2/tokens/%s",
		playPublisherApiBaseUrl,
		playSubscriptionRenewal.PackageName,
		playSubscriptionRenewal.PurchaseToken,
	)
	sub, err := server.HttpGetRequireStatusOk[*PlaySubscription](
		clientSession.Ctx,
		url,
		func(header http.Header) {
			playAuthHeaderFunc(clientSession.Ctx, header)
		},
		server.ResponseJsonObject[*PlaySubscription],
	)
	if err != nil {
		if v, ok := err.(*server.HttpStatusError); ok {
			switch v.StatusCode {
			// Gone
			case 410:
				return endTerminalPlaySubscriptionRenewalInConn(connOwner,
					playSubscriptionRenewal,
					clientSession,
					time.Time{},
				)
			default:
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	if len(sub.LineItems) == 0 {
		return nil, fmt.Errorf("Google play cannot not renew subscription with zero line items (%s)", playSubscriptionRenewal.PurchaseToken)
	}

	maxExpiryTime := sub.LineItems[0].RequireExpiryTime()
	minExpiryTime := maxExpiryTime
	for _, item := range sub.LineItems[1:] {
		if maxExpiryTime.Before(item.RequireExpiryTime()) {
			maxExpiryTime = item.RequireExpiryTime()
		} else if item.RequireExpiryTime().Before(minExpiryTime) {
			minExpiryTime = item.RequireExpiryTime()
		}
	}
	for _, item := range sub.LineItems[1:] {
		if maxExpiryTime.Before(item.RequireExpiryTime()) {
			maxExpiryTime = item.RequireExpiryTime()
		}
	}
	startTime, err := sub.ParseStartTime()
	if err != nil {
		return nil, err
	}

	now := server.NowUtc()
	active := false
	canceled := false
	terminal := false
	grace := false
	switch sub.SubscriptionState {
	case "SUBSCRIPTION_STATE_ACTIVE":
		active = true
	case "SUBSCRIPTION_STATE_IN_GRACE_PERIOD":
		grace = true
	case "SUBSCRIPTION_STATE_CANCELED":
		canceled = true
		terminal = !maxExpiryTime.After(now)
	case "SUBSCRIPTION_STATE_EXPIRED",
		"SUBSCRIPTION_STATE_PENDING_PURCHASE_CANCELED":
		canceled = true
		terminal = true
	}

	if terminal {
		return endTerminalPlaySubscriptionRenewalInConn(connOwner,
			playSubscriptionRenewal,
			clientSession,
			minExpiryTime,
		)
	}

	if grace {
		held := false
		var holdErr error
		if sku, ok := playSkusFunc()[playSubscriptionRenewal.SubscriptionId]; ok && sku.Supporter {
			server.TxInConn(clientSession.Ctx, connOwner, func(tx server.PgTx) {
				held, holdErr = playHoldGraceEntitlementInTx(tx, clientSession, playSubscriptionRenewal, startTime, maxExpiryTime)
			}, server.TxReadCommitted)
		}
		if holdErr != nil {
			return nil, holdErr
		}
		if held {
			model.UpdateProNetworkInConn(connOwner, clientSession.Ctx, playSubscriptionRenewal.NetworkId)
		}
		return &PlaySubscriptionRenewalResult{
			ExpiryTime: minExpiryTime,
			InGrace:    true,
			GraceHeld:  held,
		}, nil
	}

	// cancelled ≠ expired: a CANCELED subscription that is not terminal has
	// auto-renew off and is paid through maxExpiryTime. A period charged
	// before the cancel is entitled like an active one, so it is credited
	// through the same gate when no balance covers its expiry yet (its renewal
	// notification and the scheduled poll both missed it).
	if active || canceled {
		if _, err := model.GetOverlappingTransferBalanceInConn(connOwner, clientSession.Ctx, playSubscriptionRenewal.PurchaseToken, maxExpiryTime); err != nil {
			skus := playSkusFunc()
			skuName := playSubscriptionRenewal.SubscriptionId
			sku, ok := skus[skuName]
			if !ok {
				return nil, fmt.Errorf("Play sku not found: %s", skuName)
			}

			// The overlap check above is only a fast path: the inline webhook call
			// and the scheduled renewal task can both pass it for the same token at
			// once, and then both credit. So the credit happens in ONE tx that
			// serializes on the purchase token (advisory xact lock, released at
			// commit/rollback) and RE-CHECKS the overlap inside -- whichever of the
			// two racers gets the lock second sees the balance the first one wrote
			// and adds nothing.
			renewed := false
			var creditErr error
			// ReadCommitted, NOT the default RepeatableRead: the gate is
			// lock-then-recheck, and under RepeatableRead the second racer's
			// snapshot is pinned by the lock statement itself, taken BEFORE it
			// blocks -- after the winner commits, the loser re-checks against
			// the pre-winner snapshot (sees no credit) and then aborts with a
			// serialization failure (40001) on the rows the winner wrote.
			// Per-statement snapshots make the post-lock re-check see the
			// winner's commit, which is the entire point of the re-check.
			server.TxInConn(clientSession.Ctx, connOwner, func(tx server.PgTx) {
				renewed, creditErr = playCreditSubscriptionInTx(
					tx,
					clientSession,
					playSubscriptionRenewal,
					sub,
					sku,
					startTime,
					maxExpiryTime,
				)
			}, server.TxReadCommitted)
			if creditErr != nil {
				return nil, creditErr
			}

			if renewed {
				if sku.Supporter {
					// the pro balance is committed -- refresh the entitlement so the
					// upgrade is visible immediately rather than after ProCacheTtl
					model.UpdateProNetworkInConn(connOwner, clientSession.Ctx, playSubscriptionRenewal.NetworkId)
				}

				if canceled {
					// still one poll at the paid-through expiry
					return &PlaySubscriptionRenewalResult{
						Canceled:   true,
						Renewed:    true,
						ExpiryTime: maxExpiryTime,
					}, nil
				}
				return &PlaySubscriptionRenewalResult{
					ExpiryTime: minExpiryTime,
					Renewed:    true,
				}, nil
			}
		}
	}

	if canceled {
		return &PlaySubscriptionRenewalResult{
			Canceled:   true,
			ExpiryTime: maxExpiryTime,
		}, nil
	}

	// not active or
	// a transfer balance was already for the current expiry time
	// hence, the subscription has not been extended/renewed
	return &PlaySubscriptionRenewalResult{
		ExpiryTime: minExpiryTime,
		Renewed:    false,
	}, nil
}

// playCreditSubscriptionInTx is the Play credit gate: it serializes on the
// purchase token (advisory xact lock, released at commit/rollback), re-checks
// the overlap inside the tx and only then adds the renewal and the balance.
// renewed is false when a credit for this expiry already landed (or the paid
// window has ended). The tx must be ReadCommitted (see PlaySubscriptionRenewal).
// Shared by PlaySubscriptionRenewal and the unlinked-purchase binding
// (play_purchase_binding_controller.go), which credits inside its own tx.
func playCreditSubscriptionInTx(
	tx server.PgTx,
	clientSession *session.ClientSession,
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	sub *PlaySubscription,
	sku *Sku,
	startTime time.Time,
	maxExpiryTime time.Time,
) (bool, error) {
	if err := model.LockPlaySubscriptionPurchaseInTx(
		tx,
		clientSession.Ctx,
		playSubscriptionRenewal.NetworkId,
		playSubscriptionRenewal.PurchaseToken,
	); err != nil {
		return false, err
	}
	// A provider response can cross a terminal poll while waiting
	// for this lock. Never let a stale ACTIVE response whose paid
	// window has now ended recreate the entitlement the terminal
	// owner just closed. A real renewal has a future max expiry and
	// still proceeds.
	if !server.NowUtc().Before(maxExpiryTime) {
		return false, nil
	}

	if _, err := model.GetOverlappingTransferBalanceInTx(tx, clientSession.Ctx, playSubscriptionRenewal.PurchaseToken, maxExpiryTime); err == nil {
		// a concurrent credit for this expiry already landed
		return false, nil
	}

	// the purchase's earlier credits, read before this one is added, tell the
	// sales notice whether Play charged this window. A license tester's
	// purchase moves no money and reads nothing.
	testPurchase := sub.testPurchase()
	var earlierExpiries []time.Time
	historyRead := false
	if !testPurchase {
		earlierExpiries, historyRead = playEarlierCreditExpiriesInTx(tx, clientSession.Ctx, playSubscriptionRenewal.NetworkId, playSubscriptionRenewal.PurchaseToken)
	}

	if sku.Supporter {

		endTime := maxExpiryTime.Add(SubscriptionGracePeriod)
		netRevenue := model.UsdToNanoCents((1.0 - sku.FeeFraction) * sku.PriceAmountUsd)

		// the row starts at the subscription's start; its paid period ends at
		// the expiry and starts where the purchase's previous paid period
		// ended (model.proPaidPeriods)
		paidEndTime := maxExpiryTime
		renewal := &model.SubscriptionRenewal{
			NetworkId:          playSubscriptionRenewal.NetworkId,
			StartTime:          startTime,
			EndTime:            endTime,
			NetRevenue:         netRevenue,
			PurchaseToken:      playSubscriptionRenewal.PurchaseToken,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			SubscriptionMarket: model.SubscriptionMarketGoogle,
			PaidEndTime:        &paidEndTime,
		}
		if err := model.AddSubscriptionRenewalInTx(tx, clientSession.Ctx, renewal); err != nil {
			return false, err
		}
		// the regional price tier and the welcome offer, from the
		// store's region code and offer tag
		playRecordOnboardingInTx(tx, clientSession, sub, renewal)
		// the re-engagement program's win-back offer, from its offer tag,
		// under its own savepoint
		playRecordWinbackInTx(tx, clientSession.Ctx, sub, renewal)

		// a supporter subscription -> carries the Pro entitlement. The
		// period's marker holds no data (SupporterPeriodTransferBalance); its
		// revenue makes Play's overlap gate count it as this expiry's credit
		transferBalance := &model.TransferBalance{
			NetworkId:             playSubscriptionRenewal.NetworkId,
			StartTime:             startTime,
			EndTime:               endTime,
			StartBalanceByteCount: SupporterPeriodTransferBalance,
			SubsidyNetRevenue:     netRevenue,
			BalanceByteCount:      SupporterPeriodTransferBalance,
			PurchaseToken:         playSubscriptionRenewal.PurchaseToken,
			Pro:                   true,
		}
		model.AddTransferBalanceInTx(
			clientSession.Ctx,
			tx,
			transferBalance,
		)
		// the period's data: the full Pro grant for the cycle it starts
		model.GrantProCycleInTx(tx, clientSession.Ctx, playSubscriptionRenewal.NetworkId)

	} else {
		// a data pack, NOT a subscription -> data only, never Pro
		transferBalance := &model.TransferBalance{
			NetworkId:             playSubscriptionRenewal.NetworkId,
			StartTime:             startTime,
			EndTime:               maxExpiryTime.Add(SubscriptionGracePeriod),
			StartBalanceByteCount: sku.BalanceByteCount(),
			SubsidyNetRevenue:     model.UsdToNanoCents((1.0 - sku.FeeFraction) * sku.PriceAmountUsd),
			BalanceByteCount:      sku.BalanceByteCount(),
			PurchaseToken:         playSubscriptionRenewal.PurchaseToken,
			Pro:                   false,
		}
		model.AddTransferBalanceInTx(
			clientSession.Ctx,
			tx,
			transferBalance,
		)
	}

	// no balance covered this expiry. A window Play charged is a sale, and a
	// free trial's first window a trial, posted once the caller's transaction
	// commits; a window Play only moved out posts nothing
	if historyRead {
		credit := classifyPlayCredit(sub, playSubscriptionRenewal.SubscriptionId, startTime, maxExpiryTime, earlierExpiries)
		addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
			return playSalesNotice(playSubscriptionRenewal, sub, sku, maxExpiryTime, credit)
		})
	}

	return true, nil
}

// Keeps Pro through Play's grace period. The renewal's charge failed and Play is retrying it, and Play holds the customer entitled
// until maxExpiryTime, the grace period's end. Stripe's manual-payment grace
// keeps a failing renewal's Pro the same way. Nothing here records the unpaid
// renewal as paid: a renewal row with no revenue, so the network has an open
// window (the monthly grant, the new-purchase guard, the cancel path), and a
// zero-byte Pro marker with its exact window, so the end paths retire it. No
// bytes, no sales notice and no onboarding or win-back record. When the charge
// lands, the recovered period credits as usual
// (model.GetOverlappingTransferBalance does not count a hold as a credit), and
// if it never does, the hold runs out on its own.
//
// held is false when a credit or an earlier hold covers the expiry, or the
// window has already ended. The tx must be ReadCommitted (see
// PlaySubscriptionRenewal).
func playHoldGraceEntitlementInTx(
	tx server.PgTx,
	clientSession *session.ClientSession,
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	startTime time.Time,
	maxExpiryTime time.Time,
) (held bool, returnErr error) {
	ctx := clientSession.Ctx
	if err := model.LockPlaySubscriptionPurchaseInTx(
		tx,
		ctx,
		playSubscriptionRenewal.NetworkId,
		playSubscriptionRenewal.PurchaseToken,
	); err != nil {
		return false, err
	}
	endTime := maxExpiryTime.Add(SubscriptionGracePeriod)
	if !server.NowUtc().Before(endTime) {
		return false, nil
	}
	if _, err := model.GetOverlappingTransferBalanceInTx(tx, ctx, playSubscriptionRenewal.PurchaseToken, maxExpiryTime); err == nil {
		return false, nil
	}
	renewal := &model.SubscriptionRenewal{
		NetworkId:          playSubscriptionRenewal.NetworkId,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         0,
		PurchaseToken:      playSubscriptionRenewal.PurchaseToken,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		SubscriptionMarket: model.SubscriptionMarketGoogle,
	}
	if err := model.AddSubscriptionRenewalInTx(tx, ctx, renewal); err != nil {
		return false, err
	}
	if model.HasPlayGraceHoldInTx(tx, ctx, playSubscriptionRenewal.PurchaseToken, startTime, endTime) {
		return false, nil
	}
	model.AddTransferBalanceInTx(ctx, tx, &model.TransferBalance{
		NetworkId:             playSubscriptionRenewal.NetworkId,
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: 0,
		NetRevenue:            0,
		SubsidyNetRevenue:     0,
		BalanceByteCount:      0,
		PurchaseToken:         playSubscriptionRenewal.PurchaseToken,
		Pro:                   true,
	})
	return true, nil
}

// endTerminalPlaySubscriptionRenewal applies the same terminal-state contract
// as reconciliation, but inside the ordinary scheduled poll: EXPIRED,
// PENDING_PURCHASE_CANCELED, expired CANCELED, and 410 end only the task's
// network-and-token entitlement. An end failure is returned so the task is
// retried instead of recording a successful terminal stop.
func endTerminalPlaySubscriptionRenewal(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	clientSession *session.ClientSession,
	expiryTime time.Time,
) (*PlaySubscriptionRenewalResult, error) {
	return endTerminalPlaySubscriptionRenewalInConn(nil, playSubscriptionRenewal, clientSession, expiryTime)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func endTerminalPlaySubscriptionRenewalInConn(connOwner server.PgConn,
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	clientSession *session.ClientSession,
	expiryTime time.Time,
) (*PlaySubscriptionRenewalResult, error) {
	ended, err := endPlaySubscriptionEntitlement(
		connOwner,
		clientSession.Ctx,
		playSubscriptionRenewal.NetworkId,
		playSubscriptionRenewal.PurchaseToken,
		server.NowUtc(),
	)
	if err != nil {
		return nil, fmt.Errorf("could not end terminal Play entitlement: %w", err)
	}
	return &PlaySubscriptionRenewalResult{
		Canceled:         true,
		EntitlementEnded: ended,
		ExpiryTime:       expiryTime,
		Terminal:         true,
	}, nil
}

// Runs in the transaction that finishes the renewal task and schedules the
// next poll there. The subscription-ended notice is added to the account
// message outbox in the same transaction: a finish that rolls back leaves no
// notice, a finish that reruns its callback (or a retried post) owes one
// notice, and a crash after the commit no longer loses it. The delivery task
// sends it after the commit.
func PlaySubscriptionRenewalPost(
	playSubscriptionRenewal *PlaySubscriptionRenewalArgs,
	playSubscriptionRenewalResult *PlaySubscriptionRenewalResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if playSubscriptionRenewalResult.Canceled {
		if !playSubscriptionRenewalResult.Terminal &&
			!playSubscriptionRenewalResult.ExpiryTime.IsZero() {
			// Cancellation before the paid-through expiry preserves access and
			// retains exactly one poll at that terminal boundary. If Post was
			// delayed across the boundary, make that poll immediately due.
			playSubscriptionRenewal.CheckTime = server.MaxTime(
				playSubscriptionRenewalResult.ExpiryTime,
				server.NowUtc(),
			)
			SchedulePlaySubscriptionRenewal(
				clientSession,
				tx,
				playSubscriptionRenewal,
			)
		}
		return nil
	}

	if playSubscriptionRenewalResult.InGrace {
		// Play is still retrying the charge, so nothing has ended: no notice,
		// and one more poll at the grace period's end, when Play has decided.
		// The recovery's notification usually gets there first.
		playSubscriptionRenewal.CheckTime = server.MaxTime(
			playSubscriptionRenewalResult.ExpiryTime,
			server.NowUtc().Add(1*time.Hour),
		)
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
		return nil
	}

	if playSubscriptionRenewalResult.Renewed {
		// FIXME is the expiry time messed up sometimes?
		playSubscriptionRenewal.CheckTime = server.MaxTime(
			playSubscriptionRenewalResult.ExpiryTime,
			server.NowUtc().Add(1*time.Hour),
		)
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
	} else if now := server.NowUtc(); playSubscriptionRenewalResult.ExpiryTime.Before(now) && now.Before(playSubscriptionRenewalResult.ExpiryTime.Add(SubscriptionGracePeriod)) {
		// check again in an hour
		playSubscriptionRenewal.CheckTime = now.Add(1 * time.Hour)
		SchedulePlaySubscriptionRenewal(
			clientSession,
			tx,
			playSubscriptionRenewal,
		)
	} else {
		// else not renewed, stop trying
		networkId := playSubscriptionRenewal.NetworkId
		userAuth, err := model.GetUserAuthInTx(clientSession.Ctx, tx, networkId)
		if err != nil {
			if errors.Is(err, model.ErrMissingUserAuth) {
				// The notice is optional; a wallet/guest account must not strand
				// completed renewal post-processing for lack of a recipient.
				return nil
			}
			return err
		}
		if userAuth == "" {
			// the network or its admin is gone
			return nil
		}
		// one notice per lapse of this purchase on this network
		addAccountMessageInTx(clientSession.Ctx, tx, &accountMessage{
			key: fmt.Sprintf(
				"%s/%s/%d",
				networkId,
				playSubscriptionRenewal.PurchaseToken,
				playSubscriptionRenewalResult.ExpiryTime.Unix(),
			),
			networkId: &networkId,
			userAuth:  userAuth,
			template:  &SubscriptionEndedTemplate{},
		})
	}

	return nil
}

// Fails closed. Anyone can compute an HMAC under an absent or blank key, so
// without a configured secret every webhook is refused as unavailable, before
// its body is read.
func VerifyCoinbaseBody(_ *session.ClientSession, req *http.Request) (io.Reader, error) {
	secret, err := coinbaseWebhookSharedSecret()
	if err != nil {
		glog.Warningf("[coinbase]webhook refused, verification secret unavailable: %s\n", err)
		return nil, errWebhookVerificationUnavailable
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	// see https://docs.cloud.coinbase.com/commerce-onchain/docs/webhooks-security
	err = coinbaseSignature(bodyBytes, req.Header.Get("X-CC-Webhook-Signature"), secret)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

func coinbaseSignature(bodyBytes []byte, header string, secret string) error {
	// an HMAC under an empty key authenticates nothing
	if secret == "" {
		return errors.New("Invalid authentication.")
	}
	// see https://docs.cloud.coinbase.com/commerce-onchain/docs/webhooks-security
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(bodyBytes)
	computedSignature := mac.Sum(nil)
	headerSignature, err := hex.DecodeString(header)
	if err != nil {
		return err
	}
	if hmac.Equal(computedSignature, headerSignature) {
		return nil
	}

	return errors.New("Invalid authentication.")
}

// The claims Google's tokeninfo endpoint returns for an id token. Replaceable
// only by hermetic tests; production never mutates it.
var playIdTokenClaims = func(ctx context.Context, idToken string) (map[string]any, error) {
	url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", idToken)
	claimBytes, err := server.HttpGetRawRequireStatusOk(ctx, url, server.NoCustomHeaders)
	if err != nil {
		return nil, err
	}
	// parse the body as a claim map
	var claims map[string]any
	if err := json.Unmarshal(claimBytes, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// Fails closed: without the configured publisher and audience every push is
// refused as unavailable, before Google is asked about its token. Every
// refusal is recorded with its outcome (store_notification_flow.go).
func VerifyPlayBody(_ *session.ClientSession, req *http.Request) (body io.Reader, returnErr error) {
	defer func() {
		if returnErr == nil {
			return
		}
		outcome := StoreNotificationRefusedMalformed
		if returnErr == errWebhookVerificationUnavailable {
			outcome = StoreNotificationRefusedUnavailable
		} else if refused, ok := returnErr.(*playAuthError); ok {
			outcome = refused.outcome
		}
		RecordStoreNotificationOutcome(req.Context(), model.SubscriptionMarketGoogle, outcome)
	}()
	publisherEmail, err := playPublisherEmail()
	if err != nil {
		glog.Warningf("[play]webhook refused, verification publisher unavailable: %s\n", err)
		return nil, errWebhookVerificationUnavailable
	}
	audience, err := playAudience()
	if err != nil {
		glog.Warningf("[play]webhook refused, verification audience unavailable: %s\n", err)
		return nil, errWebhookVerificationUnavailable
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	authHeader := req.Header.Get("Authorization")

	if authHeader == "" {
		return nil, &playAuthError{outcome: StoreNotificationRefusedAuth, message: "missing authorization header"}
	}

	// see https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions?hl=en#protocol
	err = verifyPlayAuth(req.Context(), authHeader, publisherEmail, audience)
	if err != nil {
		glog.Infof("verifyPlayAuth failed: %v", err)
		return nil, err
	}

	return bytes.NewReader(bodyBytes), nil
}

// Binds a push's Google id token to the configured publisher and audience. A
// refusal says why (playAuthError): no token Google accepts, or one of
// another publisher or issuer (refused_auth); the configured publisher's
// token for another audience (refused_audience: our google.yml and the
// Pub/Sub subscription disagree); or no answer from Google (error).
func verifyPlayAuth(ctx context.Context, auth string, publisherEmail string, audience string) error {
	bearerPrefix := "Bearer "

	if strings.HasPrefix(auth, bearerPrefix) {
		claims, err := playIdTokenClaims(ctx, auth[len(bearerPrefix):])
		if err != nil {
			var statusErr *server.HttpStatusError
			if errors.As(err, &statusErr) {
				// Google answered: the token is not one it issued, or expired
				return &playAuthError{outcome: StoreNotificationRefusedAuth, message: err.Error()}
			}
			// Google could not be asked: the push is unverified, not refused
			return &playAuthError{outcome: StoreNotificationError, message: err.Error()}
		}
		// Google's tokeninfo endpoint verifies the signature. Bind that token
		// to this subscription and its verified service account as well.
		issuer, _ := claims["iss"].(string)
		verified := claims["email_verified"] == true || claims["email_verified"] == "true"
		publisher := claims["email"] == publisherEmail && verified &&
			(issuer == "https://accounts.google.com" || issuer == "accounts.google.com")
		if publisher && claims["aud"] == audience {
			return nil
		}
		if publisher {
			return playAuthRefused(StoreNotificationRefusedAudience)
		}
	}
	return playAuthRefused(StoreNotificationRefusedAuth)
}

// The grant windows (model.FreeGrantWindow, model.ReferralGrantWindow) and the Pro
// grant cycles (model.NextProCycle) live in the model, next to the balance summary
// that tells a grant in its grace from the next grant of its kind.

// AddRefreshTransferBalance grants one network the data allowance for its CURRENT
// tier and period: a Pro network gets the Pro grant of its current subscription cycle
// unless that cycle has had it, and everyone else gets the daily free amount. Used
// when a network is created and when a subscription changes (bringyourctl
// upgrade-plan), so the network does not have to wait for the next scheduled grant.
// A Pro network also gets the daily free amount, from the daily run
// (RefreshFreeTransferBalances), like every network; a network that becomes Pro
// keeps the day's free grant it already holds, so a refresh adds no second one.
//
// ReadCommitted, which the cycle grant needs (model.GrantProCycleInTx).
func AddRefreshTransferBalance(ctx context.Context, networkId server.Id) (returnErr error) {
	proGranted := false
	server.Tx(ctx, func(tx server.PgTx) {
		proGranted, returnErr = AddRefreshTransferBalanceInTx(tx, ctx, networkId)
	}, server.TxReadCommitted)
	if returnErr != nil {
		return
	}

	if proGranted {
		// the Pro grant is committed -- refresh the entitlement so the upgrade is
		// visible immediately rather than after ProCacheTtl
		model.UpdateProNetwork(ctx, networkId)
	}
	return
}

// Writes the refresh grant in the caller's tx and
// returns true when it is the Pro grant. The caller must then refresh the Pro cache
// (model.UpdateProNetwork) after the tx commits. A refresh inside the tx reads on its
// own connection, so it would cache the entitlement from before the grant for up to
// ProCacheTtl, and a tx that rolled back would still have written the cache.
//
// The tier comes from the renewals as the tx sees them, read on the tx's own
// connection: a renewal the tx itself wrote counts, and the tx does not hold a second
// pool connection while it holds this one.
func AddRefreshTransferBalanceInTx(tx server.PgTx, ctx context.Context, networkId server.Id) (proGranted bool, returnErr error) {
	pro, _ := model.HasSubscriptionRenewalInTx(tx, ctx, networkId, model.SubscriptionTypeSupporter)

	// Nothing to grant -> grant nothing. With no pro.yml the amount is ZERO, and granting
	// zero is not a no-op: it writes a real transfer_balance row with nothing in it.
	// Keyed off the amount rather than a "was pro.yml loaded" flag, so a pro.yml that is
	// present but says `data: 0` is handled the same way.
	if model.Pro().DataAmount(pro) <= 0 {
		glog.Errorf("[sub]no data amount configured for pro = %t; skipping the grant\n", pro)
		return false, nil
	}

	if pro {
		// the Pro grant of the network's current cycle, once per cycle. It
		// carries pro = true, which is what confers the entitlement. A tx at
		// another isolation grants nothing here; the sweep grants it.
		return model.GrantProCycleInTx(tx, ctx, networkId), nil
	}

	startTime, endTime := model.FreeGrantWindow(server.NowUtc())
	return false, model.AddGrantTransferBalanceInTx(
		tx,
		ctx,
		networkId,
		model.GrantKindFree,
		model.Pro().DataAmount(false),
		startTime,
		endTime,
	)
}

// ----- Free grant: runs every day -----

type RefreshFreeTransferBalancesArgs struct {
}

type RefreshFreeTransferBalancesResult struct {
}

// The worker's durable task id survives body replay after a lost handback.
// Direct administrative refreshes intentionally remain independent grants.
func transferGrantRunIds(ctx context.Context) []server.Id {
	if identity, ok := task.ExecutionIdentityFromContext(ctx); ok {
		return []server.Id{identity.TaskId}
	}
	return nil
}

func ScheduleRefreshFreeTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	// the start of the next day
	year, month, day := server.NowUtc().Date()
	runAt := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	task.ScheduleTaskInTx(
		tx,
		RefreshFreeTransferBalances,
		&RefreshFreeTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_free_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

// RefreshFreeTransferBalances grants the daily free allowance (pro.yml free.data) to
// every network, Pro and payment grace included, on top of the Pro cycle grant
// (model.AddFreeTransferBalanceToAllNetworks). A Pro network spends it after its
// paid Pro data.
func RefreshFreeTransferBalances(
	refreshFreeTransferBalances *RefreshFreeTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshFreeTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the grants resume on their normal cadence by themselves.
	if model.Pro().DataAmount(false) <= 0 {
		glog.Errorf("[sub]RefreshFreeTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshFreeTransferBalancesResult{}, nil
	}

	startTime, endTime := model.FreeGrantWindow(server.NowUtc())
	model.AddFreeTransferBalanceToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		model.Pro().DataAmount(false),
		transferGrantRunIds(clientSession.Ctx)...,
	)
	return &RefreshFreeTransferBalancesResult{}, nil
}

func RefreshFreeTransferBalancesPost(
	refreshFreeTransferBalances *RefreshFreeTransferBalancesArgs,
	refreshFreeTransferBalancesResult *RefreshFreeTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshFreeTransferBalances(clientSession, tx)
	return nil
}

// ----- Pro grant: each network on its own subscription cycle, swept hourly -----

// How often the sweep runs. A network whose cycle starts with no renewal (the
// monthly cycles of a yearly plan) gets its grant within this of the cycle's
// start, inside the previous grant's one-day grace.
const RefreshProCycleTransferBalancesInterval = 1 * time.Hour

type RefreshProCycleTransferBalancesArgs struct {
}

type RefreshProCycleTransferBalancesResult struct {
	GrantedCount int `json:"granted_count"`
	EndedCount   int `json:"ended_count"`
}

func ScheduleRefreshProCycleTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RefreshProCycleTransferBalances,
		&RefreshProCycleTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_pro_cycle_transfer_balances"),
		task.RunAt(server.NowUtc().Add(RefreshProCycleTransferBalancesInterval)),
		task.MaxTime(1*time.Hour),
	)
}

// RefreshProCycleTransferBalances grants the FULL Pro allowance (pro.yml pro.data)
// to every network whose subscription cycle is due and was not granted when its
// renewal was recorded. It then ends the running cycle grants of networks whose
// paid period was ended early (model.GrantDueProCycles). The balance is not
// rationed per day: a Pro network gets the whole 10 TiB at the start of its cycle
// and can spend it however it likes over the cycle.
func RefreshProCycleTransferBalances(
	refreshProCycleTransferBalances *RefreshProCycleTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshProCycleTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the sweep grants the cycles that are due by itself.
	if model.Pro().DataAmount(true) <= 0 {
		glog.Errorf("[sub]RefreshProCycleTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshProCycleTransferBalancesResult{}, nil
	}
	grantedNetworkIds, endedNetworkIds := model.GrantDueProCycles(clientSession.Ctx)
	if 0 < len(grantedNetworkIds) || 0 < len(endedNetworkIds) {
		glog.Infof("[sub]Pro cycle grants: %d granted, %d ended early\n", len(grantedNetworkIds), len(endedNetworkIds))
	}
	return &RefreshProCycleTransferBalancesResult{
		GrantedCount: len(grantedNetworkIds),
		EndedCount:   len(endedNetworkIds),
	}, nil
}

func RefreshProCycleTransferBalancesPost(
	refreshProCycleTransferBalances *RefreshProCycleTransferBalancesArgs,
	refreshProCycleTransferBalancesResult *RefreshProCycleTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshProCycleTransferBalances(clientSession, tx)
	return nil
}

// ----- Retired: the calendar-month Pro grant -----
//
// RefreshProTransferBalances granted every subscriber the Pro allowance on the 1st
// of the month (UTC). Grants now follow each network's own cycle (above). The task
// stays registered so its pending row (due 2026-12-01) is consumed by a binary that
// grants nothing, and its post-hook schedules no successor. Nothing schedules it
// any more.

type RefreshProTransferBalancesArgs struct {
}

type RefreshProTransferBalancesResult struct {
}

func RefreshProTransferBalances(
	refreshProTransferBalances *RefreshProTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshProTransferBalancesResult, error) {
	glog.Infof("[sub]RefreshProTransferBalances is retired; Pro grants follow each network's cycle (RefreshProCycleTransferBalances)\n")
	return &RefreshProTransferBalancesResult{}, nil
}

func RefreshProTransferBalancesPost(
	refreshProTransferBalances *RefreshProTransferBalancesArgs,
	refreshProTransferBalancesResult *RefreshProTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

// ----- Referral grant: runs every referral period -----

type RefreshReferralTransferBalancesArgs struct {
}

type RefreshReferralTransferBalancesResult struct {
}

func ScheduleRefreshReferralTransferBalances(clientSession *session.ClientSession, tx server.PgTx) {
	// ReferralGrantPeriod, never the raw ReferralPeriod: a zero period here schedules the
	// task for NOW, and its Post hook reschedules it for now again -- a hot loop.
	runAt := server.NowUtc().Add(model.Pro().ReferralGrantPeriod())
	task.ScheduleTaskInTx(
		tx,
		RefreshReferralTransferBalances,
		&RefreshReferralTransferBalancesArgs{},
		clientSession,
		task.RunOnce("refresh_referral_transfer_balances"),
		task.RunAt(runAt),
		task.MaxTime(1*time.Hour),
	)
}

// RefreshReferralTransferBalances grants the referral bonus to both sides for one
// period, all from pro.yml: the referrer earns bonus_per_referral x min(referrals,
// max_referrals), and each referred network earns referred_bonus. Referrals pay out
// every period for life. The balances are unpaid and pro = false, so referral data
// never confers Pro.
func RefreshReferralTransferBalances(
	refreshReferralTransferBalances *RefreshReferralTransferBalancesArgs,
	clientSession *session.ClientSession,
) (*RefreshReferralTransferBalancesResult, error) {
	// Nothing to grant -> grant nothing, rather than write zero-byte balance rows. With no
	// pro.yml this amount is zero. The task stays SCHEDULED, so once pro.yml lands (and
	// the process restarts) the grants resume on their normal cadence by themselves.
	if model.Pro().ReferralBonus <= 0 && model.Pro().ReferredBonus <= 0 {
		glog.Errorf("[sub]RefreshReferralTransferBalances: no amount configured (is pro.yml present?); skipping the grant\n")
		return &RefreshReferralTransferBalancesResult{}, nil
	}

	startTime, endTime := model.ReferralGrantWindow(server.NowUtc())
	model.AddReferralBonusesToAllNetworks(
		clientSession.Ctx,
		startTime,
		endTime,
		model.Pro().ReferralBonus,
		model.Pro().ReferredBonus,
		transferGrantRunIds(clientSession.Ctx)...,
	)
	return &RefreshReferralTransferBalancesResult{}, nil
}

func RefreshReferralTransferBalancesPost(
	refreshReferralTransferBalances *RefreshReferralTransferBalancesArgs,
	refreshReferralTransferBalancesResult *RefreshReferralTransferBalancesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRefreshReferralTransferBalances(clientSession, tx)
	return nil
}

/**
 * Apple App Store Webhooks
 */

type AppleNotificationPayload struct {
	SignedPayload string `json:"signedPayload"`
}

type AppleNotificationDecodedPayload struct {
	NotificationType      string                 `json:"notificationType"`
	Subtype               string                 `json:"subtype"`
	NotificationUUID      string                 `json:"notificationUUID"`
	NotificationVersion   string                 `json:"version"`
	SignedDate            int64                  `json:"signedDate"`
	Data                  map[string]interface{} `json:"data"` // need to parse this depending on the notification type
	AppAppleId            int64                  `json:"appAppleId"`
	BundleId              string                 `json:"bundleId"`
	BundleVersion         string                 `json:"bundleVersion"`
	Environment           string                 `json:"environment"`
	Status                int                    `json:"status"`
	SignedRenewalInfo     string                 `json:"signedRenewalInfo"`
	SignedTransactionInfo string                 `json:"signedTransactionInfo"`
	// Populated only by the App Store JWS verifier. Controller code must never
	// decode SignedRenewalInfo or SignedTransactionInfo itself.
	RenewalInfo     map[string]any `json:"-"`
	TransactionInfo map[string]any `json:"-"`
}

/**
 * Helius Webhooks for Solana payments
 */

// The Authorization value Helius sends with every webhook.
func heliusWebhookAuthorization() (string, error) {
	return vaultNonblankString("helius.yml", "helius", "webhook_auth_header")
}

// Why a Helius webhook cannot be verified now, or nil.
func HeliusWebhookVerifiable() error {
	_, err := heliusWebhookAuthorization()
	return err
}

// Fails closed. An absent or blank configured value would otherwise match a
// request that sends no Authorization, so without one every webhook is refused
// as unavailable, before its body is read.
func VerifyHeliusBody(_ *session.ClientSession, req *http.Request) (io.Reader, error) {
	secret, err := heliusWebhookAuthorization()
	if err != nil {
		glog.Warningf("[helius]webhook refused, verification secret unavailable: %s\n", err)
		return nil, errWebhookVerificationUnavailable
	}

	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}

	if !hmac.Equal([]byte(req.Header.Get("Authorization")), []byte(secret)) {
		glog.Infof("[helius]Invalid authentication\n")
		return nil, errors.New("Invalid authentication.")
	}

	return bytes.NewReader(bodyBytes), nil
}

type SolanaTransaction struct {
	AccountData      []AccountData          `json:"accountData"`
	Description      string                 `json:"description"`
	Events           map[string]interface{} `json:"events"`
	Fee              int64                  `json:"fee"`
	FeePayer         string                 `json:"feePayer"`
	Instructions     []Instruction          `json:"instructions"`
	NativeTransfers  []NativeTransfer       `json:"nativeTransfers"`
	Signature        string                 `json:"signature"`
	Slot             int64                  `json:"slot"`
	Source           string                 `json:"source"`
	Timestamp        int64                  `json:"timestamp"`
	TokenTransfers   []TokenTransfer        `json:"tokenTransfers"`
	TransactionError interface{}            `json:"transactionError"`
	Type             string                 `json:"type"`
}

type AccountData struct {
	Account             string               `json:"account"`
	NativeBalanceChange int64                `json:"nativeBalanceChange"`
	TokenBalanceChanges []TokenBalanceChange `json:"tokenBalanceChanges"`
}

type TokenBalanceChange struct {
	Mint           string         `json:"mint"`
	RawTokenAmount RawTokenAmount `json:"rawTokenAmount"`
	TokenAccount   string         `json:"tokenAccount"`
	UserAccount    string         `json:"userAccount"`
}

type RawTokenAmount struct {
	Decimals    int    `json:"decimals"`
	TokenAmount string `json:"tokenAmount"`
}

type Instruction struct {
	Accounts          []string           `json:"accounts"`
	Data              string             `json:"data"`
	InnerInstructions []InnerInstruction `json:"innerInstructions"`
	ProgramId         string             `json:"programId"`
}

type InnerInstruction struct {
	Accounts  []string `json:"accounts"`
	Data      string   `json:"data"`
	ProgramId string   `json:"programId"`
}

type NativeTransfer struct {
	Amount          int64  `json:"amount"`
	FromUserAccount string `json:"fromUserAccount"`
	ToUserAccount   string `json:"toUserAccount"`
}

type TokenTransfer struct {
	FromTokenAccount string  `json:"fromTokenAccount"`
	FromUserAccount  string  `json:"fromUserAccount"`
	Mint             string  `json:"mint"`
	ToTokenAccount   string  `json:"toTokenAccount"`
	ToUserAccount    string  `json:"toUserAccount"`
	TokenAmount      float64 `json:"tokenAmount"`
	TokenStandard    string  `json:"tokenStandard"`
}

type HeliusWebhookArgs struct{}

type HeliusWebhookResult struct {
	Message string `json:"message,omitempty"`
}

const solanaUsdcMint = model.SolanaUsdcMint

// The one source of where a Solana Pay payment goes. The webhook credits a
// USDC transfer to any of these; the first is the one every intent path
// quotes (solanaPaymentRecipient). Clients pay the address the quote names
// and keep none of their own, so rotating the receiver is an edit here: the
// new address first, and the old one listed after it while payments quoted
// to it can still arrive.
var solanaReceiverAddresses = []string{
	"4Fj9RCwJqHLdLNK28DwWHunHqWapxKbbzeYZLmreSYCM", // coinbase account address
	"74UNdYRpvakSABaYHSZMQNaXBVtA6eY9Nt8chcqocKe7", // deprecating this
}

// The receiver a client is told to pay: the one every intent result names
// (SolanaPaymentIntentResult.Recipient, PayDataSolanaIntentResult.Recipient)
// and the one the browser-wallet transfer pays (CreateSolanaPaymentTransaction).
func solanaPaymentRecipient() string {
	return solanaReceiverAddresses[0]
}

// db lookups and writes HeliusWebhook makes, as seams so a test can drive a batch
// (including a failed write) without Postgres
var (
	heliusSearchPaymentIntents           = model.SearchPaymentIntents
	heliusIsSolanaPaymentCompleted       = model.IsSolanaPaymentCompleted
	heliusRecordUnfulfilledSolanaPayment = model.RecordUnfulfilledSolanaPayment
	heliusListIntentsByAmountMicro       = model.ListSolanaPaymentIntentsByAmountMicro
)

func HeliusWebhook(
	transactions []*SolanaTransaction,
	clientSession *session.ClientSession,
) (*HeliusWebhookResult, error) {

	if len(transactions) == 0 {
		return &HeliusWebhookResult{Message: "No transactions"}, nil
	}

	// One Helius delivery carries a BATCH of transactions, and a customer's payment can
	// arrive behind any number of unrelated transfers in the same batch. So a
	// transaction that does not match is SKIPPED (continue), never returned on: the
	// early returns this loop used to take meant a valid payment behind any unrelated
	// transfer was never examined at all -- Helius got its 200 and never retried, and
	// the money bought nothing.
	//
	// A per-transaction DB failure is remembered and returned AFTER the whole batch is
	// examined: the non-2xx makes Helius redeliver everything, and the consumed intents
	// (tx_signature set) make the already-credited ones no-ops on the retry.
	var matched int
	var firstErr error
	// the reason the transaction was skipped -- reported as the result message for a
	// single-transaction delivery, so a caller (and the existing tests) can see WHY
	skipMessage := ""
	// native SOL payments to SOL invoice addresses: the chain read credits
	// them, so a delivery only brings that read forward (heliusHintNativeInvoices)
	nativeHinted := 0
	lookups := &solanaDbPaymentLookups{clientSession: clientSession}
	for _, transaction := range transactions {
		nativeHinted += heliusHintNativeInvoices(clientSession, transaction)
		decision := solanaDecidePayment(transaction, lookups)

		switch decision.action {
		case solanaPaymentActionError:
			if firstErr == nil {
				firstErr = decision.err
			}
			continue
		case solanaPaymentActionSkip:
			skipMessage = decision.skipMessage
			continue
		case solanaPaymentActionRecord:
			// a payment of one of our Shopify store's orders, made with Solana
			// Pay into the same receiver, bought an order, not an app purchase:
			// recorded as the order's, or left to the receiver leg while Shopify
			// cannot verify it yet (solana_shopify_order.go)
			if decision.unfulfilled.Reason == model.SolanaUnfulfilledReasonNoIntent &&
				heliusShopifyOrderPayment(clientSession, transaction, decision.unfulfilled.TokenAmountUsd) {
				skipMessage = "A Shopify order's payment"
				continue
			}
			// Money arrived at our address and bought nothing. Helius is still acked
			// 200 (it never re-examines a delivered tx) once it is recorded where an
			// operator can see and repair it.
			if err := heliusRecordUnfulfilledSolanaPayment(clientSession.Ctx, decision.unfulfilled); err != nil {
				// the row is the only trace of this payment, so a failed write fails
				// the batch and Helius redelivers (the insert is ON CONFLICT DO NOTHING)
				glog.Errorf("HeliusWebhook: could not record unfulfilled payment %s: %v\n", transaction.Signature, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			skipMessage = decision.skipMessage
			continue
		}

		credited, insertErr := solanaCreditPaymentIntent(
			clientSession,
			decision.intent,
			transaction.Signature,
			decision.tokenAmountUsd,
		)

		if insertErr != nil {
			glog.Infof("HeliusWebhook: error inserting payment data: %v", insertErr)
			if firstErr == nil {
				firstErr = insertErr
			}
			continue
		}

		if !credited {
			// lost the race to a concurrent delivery of the same transaction, which
			// already consumed the intent and credited
			skipMessage = "No payment intent found for this network ID"
			continue
		}

		// the regional price tier and the welcome offer on the credited row
		solanaRecordOnboarding(clientSession, decision.intent)

		matched++
	}

	// a per-transaction DB failure surfaces as a non-2xx AFTER the whole batch was
	// examined, so Helius redelivers everything; the consumed intents make the
	// already-credited transactions no-ops on the retry
	if firstErr != nil {
		return nil, firstErr
	}

	if matched == 0 && 0 < nativeHinted {
		return &HeliusWebhookResult{Message: fmt.Sprintf("%d SOL invoice payments to read", nativeHinted)}, nil
	}
	if matched == 0 {
		glog.Infof("HeliusWebhook: no matching payments found for %v", transactions)
		// a single-transaction delivery keeps its specific reason; a batch with
		// nothing matched can only be summarized
		if len(transactions) == 1 && skipMessage != "" {
			return &HeliusWebhookResult{Message: skipMessage}, nil
		}
		return &HeliusWebhookResult{Message: "No matching payments"}, nil
	}
	return &HeliusWebhookResult{Message: fmt.Sprintf("Processed %d matching payments", matched)}, nil
}

// Whether the receiver leg of the Solana reconcile runs (helius.yml names
// the RPC key it reads the chain with), so the webhook can leave a payment to
// it. Replaceable only by tests.
var heliusReceiverLegRuns = func() bool {
	_, ok := solanaChainRpcUrl()
	return ok
}

// The webhook's Shopify branch for a payment it would record as no_intent:
// true when the payment is a Shopify order's, recorded as the order's payment,
// or a Solana Pay app payment Shopify cannot verify yet (its order may be made
// seconds after the block; the lookup may fail) that the receiver leg reads
// after its minimum age and decides. false keeps today's behaviour: the
// payment is recorded as no_intent. A redelivery of a payment already recorded
// as an order's is true at once.
func heliusShopifyOrderPayment(clientSession *session.ClientSession, transaction *SolanaTransaction, receivedUsd float64) bool {
	ctx := clientSession.Ctx
	if !solanaShopifyAppPayment(transaction) {
		return false
	}
	if heliusShopifyOrderRecorded(ctx, transaction.Signature) {
		return true
	}
	leg := model.SolanaShopifyOrderLegWebhook
	outcome, note, err := solanaRecognizeShopifyOrder(nil, ctx, transaction, receivedUsd, leg, DefaultSolanaShopifyOrderSettings(), false)
	switch {
	case err != nil:
		solanaShopifyOrderPayments.WithLabelValues(leg, "error").Inc()
		glog.Warningf("HeliusWebhook: the Shopify order of Solana Pay payment %s could not be looked up: %s\n", transaction.Signature, err)
		note = "the order lookup failed"
	case outcome == solanaShopifyRecorded:
		solanaShopifyOrderPayments.WithLabelValues(leg, "recorded").Inc()
		return true
	case outcome == solanaShopifyNotApplicable:
		return false
	}
	if !heliusReceiverLegRuns() {
		if err == nil {
			solanaShopifyOrderPayments.WithLabelValues(leg, "unverified").Inc()
		}
		return false
	}
	solanaShopifyOrderPayments.WithLabelValues(leg, "deferred").Inc()
	glog.Infof("HeliusWebhook: Solana Pay payment %s is not verified as a Shopify order yet (%s); the receiver leg decides\n", transaction.Signature, note)
	return true
}

// Whether a signature is recorded as a Shopify order's payment. Replaceable
// only by tests.
var heliusShopifyOrderRecorded = func(ctx context.Context, signature string) bool {
	return model.IsSolanaShopifyOrderPaymentRecordedInConn(nil, ctx, signature)
}

// heliusHintNativeInvoices is the webhook's native branch: the native SOL
// transfers of a delivered transaction to SOL invoice addresses (each SOL
// invoice has a Coinbase deposit address of its own) are read on the SOL
// scanner's next scan, whatever the invoice's age, and that read of the chain
// at the finalized commitment credits them (crypto_pay_solana_native.go).
// The delivery itself credits nothing and changes nothing of the USDC
// decision. Helius delivers a per-invoice address's transactions only when the
// address is on its webhook; the scanner's polling is the source of truth
// either way. Returns how many invoice addresses the transaction paid. A
// failed lookup is logged, never the delivery's failure: the scan still reads
// every open invoice.
func heliusHintNativeInvoices(clientSession *session.ClientSession, transaction *SolanaTransaction) (hinted int) {
	if cryptoPayConfigFunc().Asset(model.CryptoAssetSol) == nil {
		// native SOL is not switched on: there is no SOL invoice to hint
		return 0
	}
	destinations := []string{}
	for _, nativeTransfer := range transaction.NativeTransfers {
		if nativeTransfer.Amount <= 0 || nativeTransfer.ToUserAccount == "" {
			continue
		}
		address, err := cryptoSolanaAddressCanonical(nativeTransfer.ToUserAccount)
		if err != nil || slices.Contains(destinations, address) {
			continue
		}
		destinations = append(destinations, address)
	}
	if len(destinations) == 0 {
		return 0
	}
	server.HandleError(func() {
		invoiceAddresses := cryptoInvoiceAddresses(clientSession.Ctx, model.CryptoAssetSol, destinations)
		if len(invoiceAddresses) == 0 {
			return
		}
		cryptoAddScanHints(clientSession.Ctx, model.CryptoAssetSol, invoiceAddresses)
		cryptoPayWebhookDeliveries.WithLabelValues("helius", model.CryptoAssetSol, "hinted").Inc()
		hinted = len(invoiceAddresses)
		glog.Infof("HeliusWebhook: transaction %s pays %d SOL invoice addresses; their scan reads them next\n", transaction.Signature, hinted)
	}, func(err error) {
		glog.Warningf("HeliusWebhook: could not look up the SOL invoice addresses of transaction %s: %s\n", transaction.Signature, err)
	})
	return
}

// What the webhook does with one transaction (solanaDecidePayment).
type solanaPaymentAction int

const (
	// nothing to credit or record; skipMessage says why
	solanaPaymentActionSkip solanaPaymentAction = iota
	// credit intent with tokenAmountUsd (the intent one-shot still guards it)
	solanaPaymentActionCredit
	// record unfulfilled for an operator; skipMessage says why
	solanaPaymentActionRecord
	// a lookup failed; err is returned after the batch so Helius redelivers
	solanaPaymentActionError
)

type solanaPaymentDecision struct {
	action         solanaPaymentAction
	skipMessage    string
	intent         *model.PaymentIntentSearchResult
	tokenAmountUsd float64
	unfulfilled    *model.UnfulfilledSolanaPayment
	err            error
}

// The reads solanaDecidePayment makes. The webhook uses the db
// (solanaDbPaymentLookups); tests use an in-memory fake, so the matching rules
// are exercised without Postgres.
type solanaPaymentLookups interface {
	// the open intent whose reference is one of references, or nil
	searchIntentsByReference(references []string) (*model.PaymentIntentSearchResult, error)
	// whether this transaction signature already consumed an intent
	isPaymentCompleted(signature string) bool
	// every intent, open or consumed, quoted exactly amountMicro that expires
	// after minExpiresAt (model.ListSolanaPaymentIntentsByAmountMicro)
	intentsByAmountMicro(amountMicro int64, minExpiresAt time.Time) ([]*model.SolanaPaymentIntent, error)
}

type solanaDbPaymentLookups struct {
	clientSession *session.ClientSession
}

func (self *solanaDbPaymentLookups) searchIntentsByReference(references []string) (*model.PaymentIntentSearchResult, error) {
	return heliusSearchPaymentIntents(references, self.clientSession)
}

func (self *solanaDbPaymentLookups) isPaymentCompleted(signature string) bool {
	return heliusIsSolanaPaymentCompleted(self.clientSession.Ctx, signature)
}

func (self *solanaDbPaymentLookups) intentsByAmountMicro(amountMicro int64, minExpiresAt time.Time) ([]*model.SolanaPaymentIntent, error) {
	return heliusListIntentsByAmountMicro(self.clientSession.Ctx, amountMicro, minExpiresAt)
}

// solanaDecidePayment decides what one Helius transaction buys, with no writes:
// the webhook carries out the decision (credit through the intent one-shot, or
// record for an operator). The rules, in order:
//
//   - only a TRANSFER with a USDC transfer to one of our receiving addresses is
//     a payment; anything else is skipped
//   - the reference is searched among the account keys and memo texts
//     (solanaReferenceCandidates); an open intent found there is credited when
//     the amount is not under its quote (underpaid is recorded)
//   - no open intent: a redelivery of an already credited signature is
//     skipped; otherwise the payment is matched by its exact unique amount
//     (solanaDecideMemolessPayment) and credited only when unambiguous, else
//     recorded as no_intent
func solanaDecidePayment(
	transaction *SolanaTransaction,
	lookups solanaPaymentLookups,
) *solanaPaymentDecision {
	if transaction.Type != "TRANSFER" {
		glog.Infof("HeliusWebhook: ignoring non-transfer transaction: %s of type %s", transaction.Signature, transaction.Type)
		return &solanaPaymentDecision{
			action:      solanaPaymentActionSkip,
			skipMessage: fmt.Sprintf("Ignoring non-transfer transaction of type %s", transaction.Type),
		}
	}

	if len(transaction.TokenTransfers) == 0 {
		glog.Infof("HeliusWebhook: no token transfers found for transaction: %s", transaction.Signature)
		return &solanaPaymentDecision{
			action:      solanaPaymentActionSkip,
			skipMessage: "Ignoring transaction with no token transfers",
		}
	}

	// Take the largest USDC transfer to one of our receiving addresses, WHATEVER its
	// size. It used to require `>= 40` here -- a hardcoded stand-in for the yearly
	// price -- which meant a customer who chose the $5 monthly plan on the site had
	// their payment ignored entirely as "no matching USDC payment". They paid and got
	// nothing.
	//
	// The amount is checked below, against what they were actually QUOTED.
	paymentReceived := false
	var tokenAmountReceived float64

	for _, tokenTransfer := range transaction.TokenTransfers {

		if tokenTransfer.Mint == solanaUsdcMint &&
			slices.Contains(solanaReceiverAddresses, tokenTransfer.ToUserAccount) &&
			0 < tokenTransfer.TokenAmount {
			paymentReceived = true
			if tokenAmountReceived < tokenTransfer.TokenAmount {
				tokenAmountReceived = tokenTransfer.TokenAmount
			}
		}

	}

	if !paymentReceived {
		glog.Infof("HeliusWebhook: no USDC payment found for transaction: %s", transaction.Signature)
		return &solanaPaymentDecision{
			action:      solanaPaymentActionSkip,
			skipMessage: "Ignoring transaction with no matching USDC payment",
		}
	}

	// every string the reference could be found under: the account keys (a
	// Solana Pay wallet attaches the reference as an account) and the memo
	// texts (a payment sent by hand carries it as the transfer memo)
	accounts := solanaReferenceCandidates(transaction)

	paymentSearchResult, err := lookups.searchIntentsByReference(accounts)

	if err != nil {
		glog.Infof("HeliusWebhook: error searching payment intents: %v", err)
		return &solanaPaymentDecision{
			action: solanaPaymentActionError,
			err:    err,
		}
	}

	// on-chain time, when the webhook carried one, for the unfulfilled record
	var transactionTime *time.Time
	if transaction.Timestamp != 0 {
		t := time.Unix(transaction.Timestamp, 0).UTC()
		transactionTime = &t
	}

	if paymentSearchResult == nil {
		// a REDELIVERY of a payment that already consumed its intent finds no
		// open intent either -- that one is credited and done, not unfulfilled
		if lookups.isPaymentCompleted(transaction.Signature) {
			glog.Infof("HeliusWebhook: transaction %s already credited; ignoring redelivery", transaction.Signature)
			return &solanaPaymentDecision{
				action:      solanaPaymentActionSkip,
				skipMessage: "No payment intent found for this network ID",
			}
		}

		// No open intent matched the reference -- a payment sent without it (a
		// wallet that cannot add a memo), after the intent was swept, or with an
		// unknown reference. A late payment whose intent has merely EXPIRED but
		// not yet been swept never lands here: the search ignores expires_at on
		// purpose, so it still resolves and is credited -- late is not
		// fraudulent.
		//
		// Try the exact unique amount; what does not match unambiguously is
		// recorded with the account keys and memos the reference was searched
		// among, the sender, and why the amount did not match.
		decision := solanaDecideMemolessPayment(transaction, lookups, &model.UnfulfilledSolanaPayment{
			TxSignature:         transaction.Signature,
			Reason:              model.SolanaUnfulfilledReasonNoIntent,
			TokenAmountUsd:      tokenAmountReceived,
			ReferenceCandidates: accounts,
			TransactionTime:     transactionTime,
		})
		switch decision.action {
		case solanaPaymentActionCredit:
			glog.Infof(
				"HeliusWebhook: transaction %s has no reference; matched intent %s (network %s) by its unique amount %.6f USDC\n",
				transaction.Signature,
				decision.intent.PaymentReference,
				*decision.intent.NetworkId,
				decision.tokenAmountUsd,
			)
		case solanaPaymentActionRecord:
			glog.Errorf(
				"HeliusWebhook: no payment intent found for transaction: %s; recording as unfulfilled (%s)\n",
				transaction.Signature,
				*decision.unfulfilled.MatchNote,
			)
		}
		return decision
	}

	// Verify the payment against what the customer was QUOTED. Underpaying must not
	// buy a plan; overpaying is their choice and is honored.
	//
	// The tolerance absorbs float dust in the token amount (it arrives as a float64
	// from the chain), not a real discount.
	if solanaAmountTolerance < paymentSearchResult.ExpectedAmountUsd-tokenAmountReceived {
		glog.Errorf(
			"HeliusWebhook: underpaid %s: received %.2f USDC, quoted %.2f (reference %s)\n",
			transaction.Signature,
			tokenAmountReceived,
			paymentSearchResult.ExpectedAmountUsd,
			paymentSearchResult.PaymentReference,
		)
		// funds were kept and the intent stays open -- record the shortfall where
		// an operator can see it, with the quote it was checked against
		return &solanaPaymentDecision{
			action:      solanaPaymentActionRecord,
			skipMessage: "Payment is less than the quoted price",
			unfulfilled: &model.UnfulfilledSolanaPayment{
				TxSignature:       transaction.Signature,
				Reason:            model.SolanaUnfulfilledReasonUnderpaid,
				TokenAmountUsd:    tokenAmountReceived,
				ExpectedAmountUsd: &paymentSearchResult.ExpectedAmountUsd,
				PaymentReference:  &paymentSearchResult.PaymentReference,
				NetworkId:         paymentSearchResult.NetworkId,
				TransactionTime:   transactionTime,
			},
		}
	}

	return &solanaPaymentDecision{
		action:         solanaPaymentActionCredit,
		intent:         paymentSearchResult,
		tokenAmountUsd: tokenAmountReceived,
	}
}

// solanaCreditPaymentIntent consumes an open intent and grants the plan the
// customer was quoted, in ONE tx. Shared by the Helius webhook and the payment
// reconciler (which sweeps recorded unfulfilled payments whose reference now
// resolves), so a reconcile credit racing a webhook redelivery of the same
// transaction produces exactly one credit.
//
// The intent is consumed FIRST, in the same tx as the credit. The guarded
// UPDATE (tx_signature IS NULL, rows-affected checked) is the concurrency
// gate: two callers with the same transaction set the same signature, so only
// rows-affected can tell them apart -- exactly one proceeds to credit, the
// other sees a consumed intent and adds nothing (credited = false, no error).
func solanaCreditPaymentIntent(
	clientSession *session.ClientSession,
	paymentSearchResult *model.PaymentIntentSearchResult,
	signature string,
	tokenAmountReceivedUsd float64,
) (credited bool, returnErr error) {
	return solanaCreditPaymentIntentInConn(nil, clientSession, paymentSearchResult, signature, tokenAmountReceivedUsd)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func solanaCreditPaymentIntentInConn(connOwner server.PgConn,
	clientSession *session.ClientSession,
	paymentSearchResult *model.PaymentIntentSearchResult,
	signature string,
	tokenAmountReceivedUsd float64,
) (credited bool, returnErr error) {
	// an intent of the generic crypto API (POST /pay/crypto/intent,
	// /pay/data/crypto-intent) is credited through its invoice, which consumes
	// this intent in the same transaction (crypto_pay_credit.go)
	if linked, credited, err := cryptoCreditLinkedSolanaIntentInConn(
		connOwner,
		clientSession,
		paymentSearchResult,
		signature,
		tokenAmountReceivedUsd,
	); linked {
		return credited, err
	}
	// only a linked intent can lack a network (a USDC code bought without one)
	if paymentSearchResult.NetworkId == nil {
		return false, fmt.Errorf("solana intent %s has no network", paymentSearchResult.PaymentReference)
	}

	// a data pack bought for a named network from the buy-data page: data only,
	// no subscription (pay_data_solana_controller.go)
	if solanaIsDataPackPlan(paymentSearchResult.SubscriptionPlan) {
		return solanaCreditDataPackInConn(connOwner, clientSession, paymentSearchResult, signature, tokenAmountReceivedUsd)
	}

	// Grant the plan they actually bought. This used to be a YEAR every time,
	// whatever they had chosen and whatever they had paid.
	startTime := server.NowUtc()
	endTime := startTime.Add(solanaPlanDuration(paymentSearchResult.SubscriptionPlan) + manualPaymentGracePeriod)

	server.TxInConn(clientSession.Ctx, connOwner, func(tx server.PgTx) {
		credited, returnErr = solanaCreditPlanIntentInTx(
			tx,
			clientSession,
			paymentSearchResult,
			signature,
			tokenAmountReceivedUsd,
			startTime,
			endTime,
		)
	}, server.TxReadCommitted)

	if returnErr != nil {
		return false, returnErr
	}

	if credited {
		// the pro balance is committed -- refresh the entitlement so the upgrade
		// is visible immediately rather than after ProCacheTtl
		model.UpdateProNetworkInConn(connOwner, clientSession.Ctx, *paymentSearchResult.NetworkId)
	}

	return credited, nil
}

// solanaCreditPlanIntentInTx is one attempt of a plan intent's credit, in the
// caller's ReadCommitted transaction: the intent one-shot, then the renewal and
// the pro balance, then the sales notice of the new payment.
func solanaCreditPlanIntentInTx(
	tx server.PgTx,
	clientSession *session.ClientSession,
	paymentSearchResult *model.PaymentIntentSearchResult,
	signature string,
	tokenAmountReceivedUsd float64,
	startTime time.Time,
	endTime time.Time,
) (credited bool, returnErr error) {
	netRevenue := model.UsdToNanoCents(tokenAmountReceivedUsd)

	if err := model.LockPaymentNetworkInTx(
		tx,
		clientSession.Ctx,
		*paymentSearchResult.NetworkId,
	); err != nil {
		return false, err
	}

	completed, err := model.MarkPaymentIntentCompletedInTx(
		tx,
		paymentSearchResult.PaymentReference,
		signature,
		clientSession,
	)
	if err != nil {
		glog.Infof("[sub]solana credit: error marking payment intent completed: %v", err)
		return false, err
	}
	if !completed {
		glog.Infof("[sub]solana credit: payment intent %s already completed; not crediting again", paymentSearchResult.PaymentReference)
		return false, nil
	}

	// the paid term, without the manual-payment grace the caller added to
	// endTime (solanaCreditPaymentIntentInConn)
	paidEndTime := endTime.Add(-manualPaymentGracePeriod)
	subscriptionRenewal := model.SubscriptionRenewal{
		NetworkId:          *paymentSearchResult.NetworkId,
		SubscriptionType:   model.SubscriptionTypeSupporter,
		StartTime:          startTime,
		EndTime:            endTime,
		NetRevenue:         netRevenue,
		SubscriptionMarket: model.SubscriptionMarketSolana,
		TransactionId:      paymentSearchResult.PaymentReference,
		PaidEndTime:        &paidEndTime,
	}

	err = model.AddSubscriptionRenewalInTx(tx, clientSession.Ctx, &subscriptionRenewal)

	if err != nil {
		glog.Infof("[sub]solana credit: error adding subscription renewal: %v", err)
		return false, err
	}

	// a supporter subscription -> carries the Pro entitlement; the term's
	// marker holds no data (SupporterPeriodTransferBalance)
	transferBalance := &model.TransferBalance{
		NetworkId:             *paymentSearchResult.NetworkId,
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: SupporterPeriodTransferBalance,
		SubsidyNetRevenue:     netRevenue,
		BalanceByteCount:      SupporterPeriodTransferBalance,
		Pro:                   true,
	}
	model.AddTransferBalanceInTx(
		clientSession.Ctx,
		tx,
		transferBalance,
	)
	// the term's data: the full Pro grant for the cycle it starts
	model.GrantProCycleInTx(tx, clientSession.Ctx, *paymentSearchResult.NetworkId)

	// this call consumed the intent: a new payment, posted once the
	// transaction commits
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		return solanaSalesNotice(paymentSearchResult, tokenAmountReceivedUsd, 0)
	})

	return true, nil
}

/**
 * Solana Payment intents
 * We create a reference for each payment intent and map it to the network ID
 */

// a plan intent is paid from a wallet the buyer is already in; the data pack
// intents of the buy-data page last longer (payDataSolanaIntentDuration)
const solanaPlanIntentDuration = 1 * time.Hour

// solanaPickAmountSuffixMicro proposes a quote suffix in [1,
// model.SolanaUniqueAmountMaxSuffixMicro]; the reservation decides whether it
// is free.
var solanaPickAmountSuffixMicro = func() int64 {
	return 1 + mathrand.Int64N(model.SolanaUniqueAmountMaxSuffixMicro)
}

// solanaAmountTolerance absorbs float dust in the chain-reported token amount. It is not
// a discount: anything more than a cent short of the quoted price is an underpayment.
const solanaAmountTolerance = 0.01

// solanaPlanDuration is how long the plan the customer bought lasts. An empty plan means
// an intent created before the plan was recorded, which was always treated as yearly --
// so that is what those legacy intents still get.
func solanaPlanDuration(subscriptionPlan string) time.Duration {
	switch subscriptionPlan {
	case model.SolanaPlanMonthly:
		return 30 * 24 * time.Hour
	case model.SolanaPlanYearlyOnboarding:
		// the welcome offer stacks on the 14-day trial: a year plus the trial
		return SubscriptionYearDuration + StripeSubscriptionTrialDays*24*time.Hour
	default:
		return SubscriptionYearDuration
	}
}

type SolanaPaymentIntentArgs struct {
	Reference string `json:"reference"`
	// The plan the customer picked: yearly | monthly | yearly_onboarding (the
	// welcome offer, only while the caller's offer is redeemable). The PRICE is
	// never taken from the client -- the server derives it from pro.yml. A
	// client-supplied amount would let anyone quote themselves a year for a cent.
	Plan string `json:"plan"`
	// the store's storefront country, when the app knows it; else the tier is
	// resolved from the Stripe billing country or the client ip (Solana keeps the
	// estimate: accepted risk on a one-time crypto payment)
	StorefrontCountry string `json:"storefront_country,omitempty"`
}

type SolanaPaymentIntentResult struct {
	// the amount the SERVER quoted -- the client must pay exactly this. It is the
	// price plus a unique sub-cent suffix (up to 6 decimals), which identifies a
	// payment sent without the reference; show and pay all its decimals
	AmountUsd float64                   `json:"amount_usd,omitempty"`
	Error     *SolanaPaymentIntentError `json:"error,omitempty"`
	// the tier the quote came from and the plan's regular price (the offer's
	// full-year price for yearly_onboarding)
	Tier             string  `json:"tier,omitempty"`
	Plan             string  `json:"plan,omitempty"`
	RegularAmountUsd float64 `json:"regular_amount_usd,omitempty"`
	OfferApplied     bool    `json:"offer_applied,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	// where to pay: the merchant address (base58) and the SPL token mint (USDC).
	// Clients build the payment url from these instead of hardcoding them, so a
	// rotated receiving address reaches every client with the quote.
	Recipient    string `json:"recipient,omitempty"`
	SplTokenMint string `json:"spl_token_mint,omitempty"`
}

type SolanaPaymentIntentError struct {
	// one of the `PurchaseErrorCode*` values, when the refusal has one. Added
	// after `Message`; older clients ignore it.
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// solanaPlanPriceUsd is the quoted price for a plan, from pro.yml. Server-side, always.
// solanaPlanPriceUsd is the price we QUOTE for a plan, and the price the webhook then
// checks the payment against. ok = false means we will not sell the plan at all.
//
// A price of zero is never sellable. With no pro.yml (or a mis-specified price: 0) this
// would otherwise quote UR Pro at $0.00 -- and the webhook's check is
// `amount >= price - tolerance`, which at price 0 is `amount >= -0.01`: satisfied by
// ANY payment, including none. We would hand out a year of Pro for nothing. Refuse.
func solanaPlanPriceUsd(subscriptionPlan string) (float64, bool) {
	return solanaPlanPriceUsdForTier(subscriptionPlan, model.Pro().DefaultPriceTier())
}

// solanaPlanPriceUsdForTier is solanaPlanPriceUsd at a regional price tier. The
// welcome-offer plan (yearly_onboarding) is the tier's yearly price less the
// configured discount; the caller must have checked the offer is redeemable.
func solanaPlanPriceUsdForTier(subscriptionPlan string, tier *model.ProPriceTier) (float64, bool) {
	var priceUsd float64
	switch subscriptionPlan {
	case model.SolanaPlanMonthly:
		priceUsd = tier.MonthlyUsd
	case model.SolanaPlanYearly:
		priceUsd = tier.YearlyUsd
	case model.SolanaPlanYearlyOnboarding:
		cfg := model.Onboarding()
		if !cfg.OfferEnabled() {
			return 0, false
		}
		priceUsd = cfg.OfferPriceUsd(tier.YearlyUsd)
	default:
		return 0, false
	}
	if priceUsd <= 0 {
		glog.Errorf(
			"[sub]refusing to quote %s: no price is configured (is pro.yml present?)\n",
			subscriptionPlan,
		)
		return 0, false
	}
	return priceUsd, true
}

func CreateSolanaPaymentIntent(
	intent *SolanaPaymentIntentArgs,
	clientSession *session.ClientSession,
) (*SolanaPaymentIntentResult, error) {
	if refuseGuestPurchase(clientSession) {
		return &SolanaPaymentIntentResult{
			Error: &SolanaPaymentIntentError{
				Code:    PurchaseErrorCodeGuestSignInRequired,
				Message: purchaseGuestSignInRequiredMessage,
			},
		}, nil
	}
	// the per-network budget shared with POST /pay/crypto/intent (decision
	// 13: this endpoint had no limit)
	if !allowNetworkIntent(clientSession.ByJwt.NetworkId) {
		return &SolanaPaymentIntentResult{
			Error: &SolanaPaymentIntentError{
				Code:    PurchaseErrorCodeRateLimited,
				Message: "Too many payment attempts. Try again in a minute.",
			},
		}, nil
	}

	// The price comes from pro.yml, keyed by the plan and the caller's regional
	// price tier. It is NEVER taken from the client.
	tier := ResolvePriceTier(clientSession, intent.StorefrontCountry)
	offerApplied := false
	if intent.Plan == model.SolanaPlanYearlyOnboarding {
		if EligibleOnboardingOffer(clientSession, clientSession.ByJwt.NetworkId) == nil {
			return &SolanaPaymentIntentResult{
				Error: &SolanaPaymentIntentError{
					Code:    PurchaseErrorCodeOfferUnavailable,
					Message: "The welcome offer is not available.",
				},
			}, nil
		}
		offerApplied = true
	}
	priceUsd, ok := solanaPlanPriceUsdForTier(intent.Plan, tier.Tier)
	if !ok || priceUsd <= 0 {
		// an unknown plan or one with no configured price: not for sale now
		return &SolanaPaymentIntentResult{
			Error: &SolanaPaymentIntentError{
				Code:    PurchaseErrorCodePlanUnavailable,
				Message: "Unknown plan.",
			},
		}, nil
	}
	regularUsd := priceUsd
	if offerApplied {
		regularUsd = tier.Tier.YearlyUsd
	}

	// The error used to be discarded here, so a duplicate or failed intent looked exactly
	// like a successful one -- and the customer was sent off to pay against an intent
	// that did not exist.
	//
	// The quote is the price plus a reserved sub-cent suffix, so a payment sent
	// without the reference is still identified by its exact amount.
	amountUsd, err := model.CreateSolanaPaymentIntentWithUniqueAmount(
		clientSession.Ctx,
		intent.Reference,
		clientSession.ByJwt.NetworkId,
		priceUsd,
		intent.Plan,
		server.NowUtc().Add(solanaPlanIntentDuration),
		solanaPickAmountSuffixMicro,
	)
	if err != nil {
		glog.Errorf("[sub]could not create solana payment intent: %s\n", err)
		return &SolanaPaymentIntentResult{
			Error: &SolanaPaymentIntentError{
				Code:    PurchaseErrorCodeStartFailed,
				Message: purchaseStartFailedMessage,
			},
		}, nil
	}

	// Hand the quoted amount back so the payment url the client builds and the intent the
	// webhook checks against cannot disagree.
	return solanaPaymentIntentQuote(amountUsd, regularUsd, tier.Tier.Name, intent.Plan, offerApplied), nil
}

// solanaPaymentIntentQuote is the successful intent result: the quoted amount
// (the price plus its unique suffix) and where to pay it, the same receiver and
// mint the webhook credits.
func solanaPaymentIntentQuote(
	amountUsd float64,
	regularUsd float64,
	tierName string,
	plan string,
	offerApplied bool,
) *SolanaPaymentIntentResult {
	return &SolanaPaymentIntentResult{
		AmountUsd:        amountUsd,
		Tier:             tierName,
		Plan:             plan,
		RegularAmountUsd: regularUsd,
		OfferApplied:     offerApplied,
		Currency:         model.PriceTierCurrency,
		Recipient:        solanaPaymentRecipient(),
		SplTokenMint:     solanaUsdcMint,
	}
}
