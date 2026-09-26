package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/coupon"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/ephemeralkey"
	"github.com/stripe/stripe-go/v82/paymentmethod"
	"github.com/stripe/stripe-go/v82/price"
	"github.com/stripe/stripe-go/v82/subscription"
)

// Stripe for the onboarding program: the inline payment sheet (the non-Play
// Android flavors, Windows and Linux), the per-tier price ids, the welcome-offer
// coupon, and the webhook steps that finalize the price tier from the card's
// billing country before the trial ends. The existing Checkout path keeps
// working and applies the same tier and coupon (see StripeCreateCheckoutSession).

// Subscription metadata keys the onboarding code stamps on Stripe subscriptions.
const (
	stripeMetadataNetworkId       = "network_id"
	stripeMetadataPlan            = "plan"
	stripeMetadataPriceTier       = "price_tier"
	stripeMetadataOnboardingOffer = "onboarding_offer"
	// payment_sheet: "pending" until the sheet's SetupIntent succeeds and the
	// payment method is attached; "attached" after. While pending, the $0 trial
	// invoice is NOT credited -- an abandoned sheet must not hand out a trial.
	stripeMetadataPaymentSheet = "payment_sheet"
	stripePaymentSheetPending  = "pending"
	stripePaymentSheetAttached = "attached"
)

// The default Stripe API version for the sheet's ephemeral key; the mobile SDKs
// pass the version they need, and the sheet endpoint honors it.
const stripeEphemeralKeyDefaultVersion = "2023-08-16"

// stripeSubscriptionPricesByTier reads the per-tier price ids from
// config/<env>/stripe.yml: `subscription_prices.yearly/monthly` are the standard
// tier's, and `subscription_prices.<tier>.yearly/monthly` any other tier's.
// Optional: a tier without configured ids gets prices created (or found by
// lookup key) under the standard product on first use.
var stripeSubscriptionPricesByTier = sync.OnceValue(func() map[string]map[string]string {
	prices := map[string]map[string]string{}
	resource, err := server.Config.SimpleResource("stripe.yml")
	if err != nil {
		return prices
	}
	c, err := resource.ParseE()
	if err != nil {
		return prices
	}
	sub, ok := c["subscription_prices"].(map[string]any)
	if !ok {
		return prices
	}
	standard := map[string]string{}
	for _, plan := range []string{model.PlanYearly, model.PlanMonthly} {
		if id, ok := sub[plan].(string); ok {
			standard[plan] = id
		}
	}
	prices[model.PriceTierStandard] = standard
	for key, value := range sub {
		tierPrices, ok := value.(map[string]any)
		if !ok {
			continue
		}
		m := map[string]string{}
		for _, plan := range []string{model.PlanYearly, model.PlanMonthly} {
			if id, ok := tierPrices[plan].(string); ok {
				m[plan] = id
			}
		}
		prices[key] = m
	}
	return prices
})

// stripeTierPriceCache caches created/found price ids per "<tier>/<plan>".
var stripeTierPriceCache sync.Map

func stripePriceLookupKey(tier string, plan string) string {
	return fmt.Sprintf("ur_pro_%s_%s", plan, tier)
}

// StripePriceIdForTier is the Stripe price id that charges a tier's plan price.
// Configured ids win; otherwise the price is found by lookup key or created under
// the standard price's product with the tier's USD amount. stripe.Key must be set.
func StripePriceIdForTier(tier *model.ProPriceTier, plan string) (string, error) {
	if plan != model.PlanYearly && plan != model.PlanMonthly {
		return "", fmt.Errorf("unknown plan %q", plan)
	}
	configured := stripeSubscriptionPricesByTier()
	if id := configured[tier.Name][plan]; id != "" {
		return id, nil
	}
	cacheKey := tier.Name + "/" + plan
	if id, ok := stripeTierPriceCache.Load(cacheKey); ok {
		return id.(string), nil
	}
	amountUsd := tier.PriceUsd(plan)
	if amountUsd <= 0 {
		return "", fmt.Errorf("no price configured for %s %s", tier.Name, plan)
	}
	standardId := configured[model.PriceTierStandard][plan]
	if standardId == "" {
		return "", fmt.Errorf("no standard %s price configured", plan)
	}

	lookupKey := stripePriceLookupKey(tier.Name, plan)
	listParams := &stripe.PriceListParams{LookupKeys: []*string{stripe.String(lookupKey)}}
	iter := price.List(listParams)
	for iter.Next() {
		p := iter.Price()
		if p.Active {
			stripeTierPriceCache.Store(cacheKey, p.ID)
			return p.ID, nil
		}
	}
	if err := iter.Err(); err != nil {
		return "", err
	}

	standard, err := price.Get(standardId, nil)
	if err != nil {
		return "", err
	}
	if standard.Product == nil {
		return "", fmt.Errorf("standard price %s has no product", standardId)
	}
	interval := stripe.PriceRecurringIntervalYear
	if plan == model.PlanMonthly {
		interval = stripe.PriceRecurringIntervalMonth
	}
	created, err := price.New(&stripe.PriceParams{
		Product:    stripe.String(standard.Product.ID),
		Currency:   stripe.String(string(stripe.CurrencyUSD)),
		UnitAmount: stripe.Int64(int64(math.Round(amountUsd * 100))),
		Recurring:  &stripe.PriceRecurringParams{Interval: stripe.String(string(interval))},
		LookupKey:  stripe.String(lookupKey),
		Nickname:   stripe.String(fmt.Sprintf("Pro %s (%s)", plan, tier.Name)),
		Metadata: map[string]string{
			stripeMetadataPriceTier: tier.Name,
			stripeMetadataPlan:      plan,
		},
	})
	if err != nil {
		return "", err
	}
	glog.Infof("[stripe]created %s price %s for tier %s (%s)\n", plan, created.ID, tier.Name, lookupKey)
	stripeTierPriceCache.Store(cacheKey, created.ID)
	return created.ID, nil
}

// ----- the welcome-offer coupon -----

var stripeCouponCache sync.Map

// StripeOnboardingCouponId creates or fetches the welcome-offer coupon
// (onboarding.yml offer.stripe_coupon_id, percent_off). Duration is `repeating`
// for 2 months: applied at subscription creation it discounts the first paid
// yearly invoice at day 14 and nothing a year later, whether or not the $0 trial
// invoice counts as an application. SEAM: a Stripe TEST-MODE run confirming that
// the $0 trial invoice does not consume a `once` coupon would allow `once`; no
// test key is configured, so `repeating` it is. stripe.Key must be set.
func StripeOnboardingCouponId() (string, error) {
	cfg := model.Onboarding()
	id := strings.TrimSpace(cfg.Offer.StripeCouponId)
	if id == "" || !cfg.OfferEnabled() {
		return "", fmt.Errorf("the welcome offer is not configured")
	}
	if cached, ok := stripeCouponCache.Load(id); ok {
		return cached.(string), nil
	}
	existing, err := coupon.Get(id, nil)
	if err == nil && existing != nil && existing.Valid {
		if math.Abs(existing.PercentOff-float64(cfg.Offer.PercentOff)) > 0.001 {
			return "", fmt.Errorf("coupon %s is %g%% off, config says %d%%", id, existing.PercentOff, cfg.Offer.PercentOff)
		}
		stripeCouponCache.Store(id, existing.ID)
		return existing.ID, nil
	}
	var stripeErr *stripe.Error
	if err != nil && !(errors.As(err, &stripeErr) && stripeErr.HTTPStatusCode == 404) {
		return "", err
	}
	created, err := coupon.New(&stripe.CouponParams{
		ID:               stripe.String(id),
		Name:             stripe.String(fmt.Sprintf("Welcome offer: %d%% off", cfg.Offer.PercentOff)),
		PercentOff:       stripe.Float64(float64(cfg.Offer.PercentOff)),
		Duration:         stripe.String(string(stripe.CouponDurationRepeating)),
		DurationInMonths: stripe.Int64(2),
		Metadata:         map[string]string{"program": "onboarding"},
	})
	if err != nil {
		return "", err
	}
	glog.Infof("[stripe]created welcome-offer coupon %s\n", created.ID)
	stripeCouponCache.Store(id, created.ID)
	return created.ID, nil
}

// stripeOnboardingDiscount is the coupon to apply for a purchase: the network's
// eligible offer on a yearly plan, else "".
func stripeOnboardingDiscount(clientSession *session.ClientSession, plan string) (couponId string, offer *model.OnboardingOffer) {
	if plan != model.PlanYearly {
		return "", nil
	}
	offer = EligibleOnboardingOffer(clientSession, clientSession.ByJwt.NetworkId)
	if offer == nil {
		return "", nil
	}
	id, err := StripeOnboardingCouponId()
	if err != nil {
		glog.Errorf("[stripe]welcome-offer coupon unavailable: %s\n", err)
		return "", nil
	}
	return id, offer
}

// ----- GET /subscription/stripe/prices -----

type StripePricesResult struct {
	Tier           string  `json:"tier"`
	Currency       string  `json:"currency"`
	YearlyPriceId  string  `json:"yearly_price_id"`
	MonthlyPriceId string  `json:"monthly_price_id"`
	YearlyUsd      float64 `json:"yearly_usd"`
	MonthlyUsd     float64 `json:"monthly_usd"`
	PublishableKey string  `json:"publishable_key"`
	// the welcome-offer coupon, when the caller's offer is redeemable
	OnboardingCouponId string           `json:"onboarding_coupon_id,omitempty"`
	OfferEligible      bool             `json:"offer_eligible"`
	Error              *OnboardingError `json:"error,omitempty"`
}

// StripePrices returns the caller's tier's Stripe price ids.
func StripePrices(storefrontCountry string, clientSession *session.ClientSession) (*StripePricesResult, error) {
	tier := ResolvePriceTier(clientSession, storefrontCountry)
	stripe.Key = stripeApiToken()
	yearly, err := StripePriceIdForTier(tier.Tier, model.PlanYearly)
	if err != nil {
		glog.Errorf("[stripe]no yearly price for tier %s: %s\n", tier.Tier.Name, err)
		return &StripePricesResult{Error: &OnboardingError{Message: "Prices are not configured."}}, nil
	}
	monthly, err := StripePriceIdForTier(tier.Tier, model.PlanMonthly)
	if err != nil {
		glog.Errorf("[stripe]no monthly price for tier %s: %s\n", tier.Tier.Name, err)
		return &StripePricesResult{Error: &OnboardingError{Message: "Prices are not configured."}}, nil
	}
	result := &StripePricesResult{
		Tier:           tier.Tier.Name,
		Currency:       model.PriceTierCurrency,
		YearlyPriceId:  yearly,
		MonthlyPriceId: monthly,
		YearlyUsd:      tier.Tier.YearlyUsd,
		MonthlyUsd:     tier.Tier.MonthlyUsd,
		PublishableKey: stripePublishableKey(),
	}
	if couponId, offer := stripeOnboardingDiscount(clientSession, model.PlanYearly); offer != nil {
		result.OnboardingCouponId = couponId
		result.OfferEligible = couponId != ""
	}
	return result, nil
}

// ----- POST /subscription/stripe/payment-sheet -----

type StripePaymentSheetArgs struct {
	// yearly | monthly
	Plan string `json:"plan"`
	// the store's storefront country, when the app knows it
	StorefrontCountry string `json:"storefront_country,omitempty"`
	// the Stripe API version the mobile SDK requires for its ephemeral key
	StripeVersion string `json:"stripe_version,omitempty"`
}

type StripePaymentSheetResult struct {
	CustomerId         string `json:"customer_id,omitempty"`
	EphemeralKeySecret string `json:"ephemeral_key_secret,omitempty"`
	// the yearly plan (with its trial) collects the card through a SetupIntent;
	// the monthly plan (no trial) charges its first invoice through a
	// PaymentIntent. IntentType says which secret is set.
	SetupIntentClientSecret   string `json:"setup_intent_client_secret,omitempty"`
	PaymentIntentClientSecret string `json:"payment_intent_client_secret,omitempty"`
	// setup | payment
	IntentType     string `json:"intent_type,omitempty"`
	SubscriptionId string `json:"subscription_id,omitempty"`
	PublishableKey string `json:"publishable_key,omitempty"`
	Tier           string `json:"tier,omitempty"`
	Currency       string `json:"currency,omitempty"`
	Plan           string `json:"plan,omitempty"`
	// what the first period costs (the offer applied when eligible) and the
	// regular price of a period
	AmountFirstPeriodUsd float64 `json:"amount_first_period_usd"`
	RegularPeriodUsd     float64 `json:"regular_period_usd"`
	TrialDays            int     `json:"trial_days"`
	// TrialEndAt is set when the plan carries a trial
	TrialEndAt   *time.Time       `json:"trial_end_at,omitempty"`
	OfferApplied bool             `json:"offer_applied"`
	Error        *OnboardingError `json:"error,omitempty"`
}

func stripePaymentSheetError(message string) *StripePaymentSheetResult {
	return &StripePaymentSheetResult{Error: &OnboardingError{Message: message}}
}

// StripePaymentSheet prepares an inline Stripe PaymentSheet purchase of Pro: the
// customer (created once, metadata network_id), the subscription at the caller's
// tier price (yearly with the 14-day trial and the welcome coupon when eligible;
// monthly without a trial), and the intent the sheet confirms. The trial is not
// credited until the card is attached (see the payment_sheet metadata).
func StripePaymentSheet(
	args *StripePaymentSheetArgs,
	clientSession *session.ClientSession,
) (*StripePaymentSheetResult, error) {
	plan := strings.ToLower(strings.TrimSpace(args.Plan))
	if plan != model.PlanYearly && plan != model.PlanMonthly {
		return stripePaymentSheetError("Unknown plan."), nil
	}
	if err := model.CheckOnboardingRateLimit(clientSession, model.OnboardingOfferRateLimit); err != nil {
		return nil, err
	}
	networkId := clientSession.ByJwt.NetworkId
	tier := ResolvePriceTier(clientSession, args.StorefrontCountry)

	stripe.Key = stripeApiToken()

	priceId, err := StripePriceIdForTier(tier.Tier, plan)
	if err != nil {
		glog.Errorf("[stripe]payment sheet: no %s price for tier %s: %s\n", plan, tier.Tier.Name, err)
		return stripePaymentSheetError("That plan is not available."), nil
	}

	customerId, err := stripeCustomerIdForSession(clientSession)
	if err != nil {
		glog.Errorf("[stripe]payment sheet: customer: %s\n", err)
		return stripePaymentSheetError("Could not start the payment. Please try again."), nil
	}

	// a sheet the customer abandoned leaves a pending subscription behind; replace
	// it rather than pile up another one
	stripeCancelPendingPaymentSheetSubscriptions(customerId, networkId)

	couponId, offer := stripeOnboardingDiscount(clientSession, plan)

	metadata := map[string]string{
		stripeMetadataNetworkId:    networkId.String(),
		stripeMetadataPlan:         plan,
		stripeMetadataPriceTier:    tier.Tier.Name,
		stripeMetadataPaymentSheet: stripePaymentSheetPending,
	}
	if offer != nil && couponId != "" {
		metadata[stripeMetadataOnboardingOffer] = "1"
	}
	params := &stripe.SubscriptionParams{
		Customer:        stripe.String(customerId),
		Items:           []*stripe.SubscriptionItemsParams{{Price: stripe.String(priceId)}},
		PaymentBehavior: stripe.String("default_incomplete"),
		PaymentSettings: &stripe.SubscriptionPaymentSettingsParams{
			SaveDefaultPaymentMethod: stripe.String(string(stripe.SubscriptionPaymentSettingsSaveDefaultPaymentMethodOnSubscription)),
		},
		Metadata: metadata,
		Expand: []*string{
			stripe.String("pending_setup_intent"),
			stripe.String("latest_invoice.confirmation_secret"),
		},
	}
	trialDays := 0
	if plan == model.PlanYearly {
		trialDays = StripeSubscriptionTrialDays
		params.TrialPeriodDays = stripe.Int64(StripeSubscriptionTrialDays)
		params.TrialSettings = &stripe.SubscriptionTrialSettingsParams{
			EndBehavior: &stripe.SubscriptionTrialSettingsEndBehaviorParams{
				MissingPaymentMethod: stripe.String("cancel"),
			},
		}
	}
	if couponId != "" {
		params.Discounts = []*stripe.SubscriptionDiscountParams{{Coupon: stripe.String(couponId)}}
	}

	sub, err := subscription.New(params)
	if err != nil {
		glog.Errorf("[stripe]payment sheet: could not create subscription for network %s: %s\n", networkId, err)
		return stripePaymentSheetError("Could not start the payment. Please try again."), nil
	}

	stripeVersion := strings.TrimSpace(args.StripeVersion)
	if stripeVersion == "" {
		stripeVersion = stripeEphemeralKeyDefaultVersion
	}
	ek, err := ephemeralkey.New(&stripe.EphemeralKeyParams{
		Customer:      stripe.String(customerId),
		StripeVersion: stripe.String(stripeVersion),
	})
	if err != nil {
		glog.Errorf("[stripe]payment sheet: ephemeral key: %s\n", err)
		return stripePaymentSheetError("Could not start the payment. Please try again."), nil
	}

	regular := tier.Tier.PriceUsd(plan)
	first := regular
	if offer != nil && couponId != "" {
		first = model.Onboarding().OfferPriceUsd(regular)
	}
	result := &StripePaymentSheetResult{
		CustomerId:           customerId,
		EphemeralKeySecret:   ek.Secret,
		SubscriptionId:       sub.ID,
		PublishableKey:       stripePublishableKey(),
		Tier:                 tier.Tier.Name,
		Currency:             model.PriceTierCurrency,
		Plan:                 plan,
		AmountFirstPeriodUsd: first,
		RegularPeriodUsd:     regular,
		TrialDays:            trialDays,
		OfferApplied:         offer != nil && couponId != "",
	}
	if 0 < sub.TrialEnd {
		trialEnd := time.Unix(sub.TrialEnd, 0).UTC()
		result.TrialEndAt = &trialEnd
	}
	switch {
	case sub.PendingSetupIntent != nil && sub.PendingSetupIntent.ClientSecret != "":
		result.IntentType = "setup"
		result.SetupIntentClientSecret = sub.PendingSetupIntent.ClientSecret
	case sub.LatestInvoice != nil && sub.LatestInvoice.ConfirmationSecret != nil && sub.LatestInvoice.ConfirmationSecret.ClientSecret != "":
		result.IntentType = "payment"
		result.PaymentIntentClientSecret = sub.LatestInvoice.ConfirmationSecret.ClientSecret
	default:
		glog.Errorf("[stripe]payment sheet: subscription %s has no intent to confirm\n", sub.ID)
		return stripePaymentSheetError("Could not start the payment. Please try again."), nil
	}
	glog.Infof(
		"[stripe]payment sheet: subscription %s for network %s plan %s tier %s (%s) offer=%t\n",
		sub.ID, networkId, plan, tier.Tier.Name, tier.Source, result.OfferApplied,
	)
	return result, nil
}

// stripeCustomerIdForSession returns the network's Stripe customer, creating it
// (metadata network_id) on first use. stripe.Key must be set.
func stripeCustomerIdForSession(clientSession *session.ClientSession) (string, error) {
	if existing, _ := model.GetStripeCustomer(clientSession); existing != nil && *existing != "" {
		return *existing, nil
	}
	created, err := customer.New(&stripe.CustomerParams{
		Metadata: map[string]string{stripeMetadataNetworkId: clientSession.ByJwt.NetworkId.String()},
	})
	if err != nil {
		return "", err
	}
	if err := model.CreateStripeCustomer(created.ID, clientSession); err != nil {
		// lost a race with a concurrent create: use the stored one
		if existing, _ := model.GetStripeCustomer(clientSession); existing != nil && *existing != "" {
			return *existing, nil
		}
		return "", err
	}
	return created.ID, nil
}

// stripeCancelPendingPaymentSheetSubscriptions cancels the customer's
// subscriptions still waiting for a payment sheet to finish. Best effort.
func stripeCancelPendingPaymentSheetSubscriptions(customerId string, networkId server.Id) {
	iter := subscription.List(&stripe.SubscriptionListParams{
		Customer: stripe.String(customerId),
		Status:   stripe.String("all"),
	})
	for iter.Next() {
		sub := iter.Subscription()
		if sub.Metadata[stripeMetadataPaymentSheet] != stripePaymentSheetPending {
			continue
		}
		switch sub.Status {
		case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusIncompleteExpired:
			continue
		}
		if _, err := subscription.Cancel(sub.ID, nil); err != nil {
			glog.Infof("[stripe]could not cancel pending subscription %s for network %s: %s\n", sub.ID, networkId, err)
		} else {
			glog.Infof("[stripe]canceled abandoned payment sheet subscription %s for network %s\n", sub.ID, networkId)
		}
	}
	if err := iter.Err(); err != nil {
		glog.Infof("[stripe]could not list subscriptions for customer %s: %s\n", customerId, err)
	}
}

// ----- the Checkout path -----

// stripeCheckoutTierAndDiscount is what StripeCreateCheckoutSession needs from the
// onboarding program: the tier's price id for the item and the coupon to apply
// when the caller's welcome offer is redeemable (yearly only). The metadata to
// stamp on the subscription is returned as well.
func stripeCheckoutTierAndDiscount(
	itemId string,
	storefrontCountry string,
	clientSession *session.ClientSession,
) (priceId string, discounts []*stripe.CheckoutSessionDiscountParams, metadata map[string]string, err error) {
	plan := model.PlanMonthly
	if itemId == StripeItemProYearly {
		plan = model.PlanYearly
	}
	tier := ResolvePriceTier(clientSession, storefrontCountry)
	priceId, err = StripePriceIdForTier(tier.Tier, plan)
	if err != nil {
		return "", nil, nil, err
	}
	metadata = map[string]string{
		stripeMetadataPlan:      plan,
		stripeMetadataPriceTier: tier.Tier.Name,
	}
	if couponId, offer := stripeOnboardingDiscount(clientSession, plan); offer != nil && couponId != "" {
		discounts = []*stripe.CheckoutSessionDiscountParams{{Coupon: stripe.String(couponId)}}
		metadata[stripeMetadataOnboardingOffer] = "1"
	}
	return priceId, discounts, metadata, nil
}

// ----- webhooks -----

// stripeHandleInvoicePaidWithOnboarding wraps stripeHandleInvoicePaid: it holds
// back the $0 trial invoice of a payment sheet whose card is not attached yet,
// and after a credit it records the price tier on the subscription row and stamps
// the welcome offer redeemed.
func stripeHandleInvoicePaidWithOnboarding(
	invoice *StripeEventInvoiceObject,
	clientSession *session.ClientSession,
) (*StripeWebhookResult, error) {
	sub := stripeSubscriptionForInvoice(clientSession.Ctx, invoice.Id)
	if sub != nil && invoice.Total == 0 && sub.Metadata[stripeMetadataPaymentSheet] == stripePaymentSheetPending {
		glog.Infof("[stripe]invoice %s: trial invoice of pending payment sheet subscription %s; crediting waits for the card\n", invoice.Id, sub.ID)
		return &StripeWebhookResult{}, nil
	}
	result, err := stripeHandleInvoicePaid(invoice, clientSession)
	if err != nil {
		return result, err
	}
	if sub != nil {
		stripeRecordOnboardingForInvoice(clientSession, sub, invoice.Id)
		stripeRecordTrialOutcomeForInvoice(clientSession.Ctx, sub, invoice)
	}
	return result, nil
}

// stripePlanForSubscription is yearly or monthly from the subscription's first
// recurring price, "" when unknown.
func stripePlanForSubscription(sub *stripe.Subscription) string {
	if sub == nil || sub.Items == nil {
		return ""
	}
	for _, item := range sub.Items.Data {
		if item == nil || item.Price == nil || item.Price.Recurring == nil {
			continue
		}
		switch item.Price.Recurring.Interval {
		case stripe.PriceRecurringIntervalYear:
			return model.PlanYearly
		case stripe.PriceRecurringIntervalMonth:
			return model.PlanMonthly
		}
	}
	return ""
}

// stripeRecordTrialOutcomeForInvoice writes trial.converted when a
// subscription that had a trial is charged a non-zero invoice: the trial's
// first paid period (mmm/onboarding/PLAN.md "MEASUREMENT", S3).
func stripeRecordTrialOutcomeForInvoice(ctx context.Context, sub *stripe.Subscription, invoice *StripeEventInvoiceObject) {
	if sub == nil || invoice == nil || invoice.Total <= 0 || sub.TrialEnd <= 0 {
		return
	}
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		return
	}
	if !server.NowUtc().After(time.Unix(sub.TrialEnd, 0).UTC().Add(-24 * time.Hour)) {
		// a paid invoice while the trial is running is a plan change, not
		// the conversion
		return
	}
	if RecordTrialConverted(ctx, networkId, model.OnboardingStoreStripe, stripePlanForSubscription(sub)) {
		glog.Infof("[onboarding]trial converted on stripe by network %s (invoice %s)\n", networkId, invoice.Id)
	}
}

// stripeEventSubscriptionObject is the customer.subscription.deleted payload's
// fields the trial outcome needs.
type stripeEventSubscriptionObject struct {
	Id         string            `json:"id"`
	Status     string            `json:"status"`
	TrialEnd   int64             `json:"trial_end"`
	EndedAt    int64             `json:"ended_at"`
	CanceledAt int64             `json:"canceled_at"`
	Metadata   map[string]string `json:"metadata"`
	Items      *struct {
		Data []struct {
			Price *struct {
				Recurring *struct {
					Interval string `json:"interval"`
				} `json:"recurring"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
}

func (self *stripeEventSubscriptionObject) plan() string {
	if self.Items == nil {
		return ""
	}
	for _, item := range self.Items.Data {
		if item.Price == nil || item.Price.Recurring == nil {
			continue
		}
		switch item.Price.Recurring.Interval {
		case "year":
			return model.PlanYearly
		case "month":
			return model.PlanMonthly
		}
	}
	return ""
}

// stripeHandleSubscriptionDeleted writes trial.cancelled when a subscription
// with a trial ends before (or at) the trial's end: the trial never became a
// paid period. Any other deletion is the end of a paid subscription, which the
// entitlement code already handles by the paid-through date. Always 200.
func stripeHandleSubscriptionDeleted(object json.RawMessage, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var sub stripeEventSubscriptionObject
	if err := json.Unmarshal(object, &sub); err != nil {
		glog.Warningf("[onboarding]could not parse deleted subscription: %s\n", err)
		return &StripeWebhookResult{}, nil
	}
	if sub.TrialEnd <= 0 {
		return &StripeWebhookResult{}, nil
	}
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		return &StripeWebhookResult{}, nil
	}
	endedAt := server.NowUtc()
	if 0 < sub.EndedAt {
		endedAt = time.Unix(sub.EndedAt, 0).UTC()
	}
	trialEnd := time.Unix(sub.TrialEnd, 0).UTC()
	if sub.Status != string(stripe.SubscriptionStatusTrialing) && trialEnd.Add(24*time.Hour).Before(endedAt) {
		// ended after a paid period began
		return &StripeWebhookResult{}, nil
	}
	if storeTrialCancelled(clientSession.Ctx, networkId, model.OnboardingStoreStripe, sub.plan(), endedAt) {
		glog.Infof("[onboarding]trial cancelled on stripe by network %s (subscription %s)\n", networkId, sub.Id)
	}
	return &StripeWebhookResult{}, nil
}

// stripeSubscriptionForInvoice fetches the subscription an invoice bills, nil
// when the invoice is not a subscription's or cannot be read.
func stripeSubscriptionForInvoice(ctx context.Context, invoiceId string) *stripe.Subscription {
	if invoiceId == "" {
		return nil
	}
	stripe.Key = stripeApiToken()
	fullInvoice, err := server.HttpGetRequireStatusOk[*stripeInvoiceSubscriptionRef](
		ctx,
		fmt.Sprintf("%s/v1/invoices/%s", stripeApiBaseUrl, invoiceId),
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[*stripeInvoiceSubscriptionRef],
	)
	if err != nil || fullInvoice == nil {
		return nil
	}
	subscriptionId := fullInvoice.subscriptionId()
	if subscriptionId == "" {
		return nil
	}
	sub, err := subscription.Get(subscriptionId, nil)
	if err != nil {
		glog.Infof("[stripe]could not read subscription %s: %s\n", subscriptionId, err)
		return nil
	}
	return sub
}

// stripeInvoiceSubscriptionRef reads the subscription id off an invoice in both
// API shapes: the top-level `subscription` (older versions) and
// `parent.subscription_details.subscription` (2025+), plus the line items.
type stripeInvoiceSubscriptionRef struct {
	Subscription json.RawMessage `json:"subscription"`
	Parent       *struct {
		SubscriptionDetails *struct {
			Subscription json.RawMessage `json:"subscription"`
		} `json:"subscription_details"`
	} `json:"parent"`
	Lines *struct {
		Data []struct {
			Subscription json.RawMessage `json:"subscription"`
		} `json:"data"`
	} `json:"lines"`
}

func stripeIdFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.ID
	}
	return ""
}

func (self *stripeInvoiceSubscriptionRef) subscriptionId() string {
	if id := stripeIdFromRaw(self.Subscription); id != "" {
		return id
	}
	if self.Parent != nil && self.Parent.SubscriptionDetails != nil {
		if id := stripeIdFromRaw(self.Parent.SubscriptionDetails.Subscription); id != "" {
			return id
		}
	}
	if self.Lines != nil {
		for _, line := range self.Lines.Data {
			if id := stripeIdFromRaw(line.Subscription); id != "" {
				return id
			}
		}
	}
	return ""
}

// stripeRecordOnboardingForInvoice stamps the price tier on the credited renewal
// row and redeems the welcome offer when the subscription carries it.
func stripeRecordOnboardingForInvoice(clientSession *session.ClientSession, sub *stripe.Subscription, invoiceId string) {
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		return
	}
	if tierName := sub.Metadata[stripeMetadataPriceTier]; tierName != "" {
		model.SetSubscriptionRenewalPriceTierByTransaction(clientSession.Ctx, networkId, model.SubscriptionMarketStripe, invoiceId, tierName)
	}
	if sub.Metadata[stripeMetadataOnboardingOffer] == "1" {
		if model.RedeemOnboardingOffer(clientSession.Ctx, networkId, model.OnboardingStoreStripe, server.NowUtc()) {
			glog.Infof("[onboarding]offer redeemed on stripe by network %s (invoice %s)\n", networkId, invoiceId)
		}
	}
}

type stripeEventSetupIntentObject struct {
	Id            string          `json:"id"`
	Customer      json.RawMessage `json:"customer"`
	PaymentMethod json.RawMessage `json:"payment_method"`
	Status        string          `json:"status"`
}

type stripeEventPaymentMethodObject struct {
	Id       string          `json:"id"`
	Customer json.RawMessage `json:"customer"`
	Card     *struct {
		Country string `json:"country"`
	} `json:"card"`
	BillingDetails *struct {
		Address *struct {
			Country string `json:"country"`
		} `json:"address"`
	} `json:"billing_details"`
}

type stripeEventCustomerObject struct {
	Id      string `json:"id"`
	Address *struct {
		Country string `json:"country"`
	} `json:"address"`
	InvoiceSettings *struct {
		DefaultPaymentMethod json.RawMessage `json:"default_payment_method"`
	} `json:"invoice_settings"`
}

// stripeHandleSetupIntentSucceeded: the payment sheet's card is attached. Resolve
// the tier from the card and finalize the subscription.
func stripeHandleSetupIntentSucceeded(object json.RawMessage, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var setupIntent stripeEventSetupIntentObject
	if err := json.Unmarshal(object, &setupIntent); err != nil {
		return nil, fmt.Errorf("failed to parse setup intent: %v", err)
	}
	customerId := stripeIdFromRaw(setupIntent.Customer)
	paymentMethodId := stripeIdFromRaw(setupIntent.PaymentMethod)
	if customerId == "" {
		return &StripeWebhookResult{}, nil
	}
	stripe.Key = stripeApiToken()
	country := ""
	if paymentMethodId != "" {
		country = stripePaymentMethodCountry(paymentMethodId)
	}
	stripeFinalizeTierForCustomer(clientSession, customerId, country)
	return &StripeWebhookResult{}, nil
}

// stripeHandlePaymentMethodAttached: a card was attached to a customer (Checkout
// or the sheet). Resolve the tier from the card's billing country.
func stripeHandlePaymentMethodAttached(object json.RawMessage, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var paymentMethod stripeEventPaymentMethodObject
	if err := json.Unmarshal(object, &paymentMethod); err != nil {
		return nil, fmt.Errorf("failed to parse payment method: %v", err)
	}
	customerId := stripeIdFromRaw(paymentMethod.Customer)
	if customerId == "" {
		return &StripeWebhookResult{}, nil
	}
	country := ""
	if paymentMethod.BillingDetails != nil && paymentMethod.BillingDetails.Address != nil {
		country = paymentMethod.BillingDetails.Address.Country
	}
	if country == "" && paymentMethod.Card != nil {
		country = paymentMethod.Card.Country
	}
	stripe.Key = stripeApiToken()
	stripeFinalizeTierForCustomer(clientSession, customerId, country)
	return &StripeWebhookResult{}, nil
}

// stripeHandleCustomerUpdated: the customer's address or default payment method
// changed. Re-resolve the tier from the default card, else the address.
func stripeHandleCustomerUpdated(object json.RawMessage, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var c stripeEventCustomerObject
	if err := json.Unmarshal(object, &c); err != nil {
		return nil, fmt.Errorf("failed to parse customer: %v", err)
	}
	if c.Id == "" {
		return &StripeWebhookResult{}, nil
	}
	stripe.Key = stripeApiToken()
	country := ""
	if c.InvoiceSettings != nil {
		if paymentMethodId := stripeIdFromRaw(c.InvoiceSettings.DefaultPaymentMethod); paymentMethodId != "" {
			country = stripePaymentMethodCountry(paymentMethodId)
		}
	}
	if country == "" && c.Address != nil {
		country = c.Address.Country
	}
	if country == "" {
		return &StripeWebhookResult{}, nil
	}
	stripeFinalizeTierForCustomer(clientSession, c.Id, country)
	return &StripeWebhookResult{}, nil
}

// stripePaymentMethodCountry is the billing country of a payment method: the
// billing address country, else the card's issuing country.
func stripePaymentMethodCountry(paymentMethodId string) string {
	pm, err := paymentmethod.Get(paymentMethodId, nil)
	if err != nil || pm == nil {
		return ""
	}
	if pm.BillingDetails != nil && pm.BillingDetails.Address != nil && pm.BillingDetails.Address.Country != "" {
		return pm.BillingDetails.Address.Country
	}
	if pm.Card != nil {
		return pm.Card.Country
	}
	return ""
}

// stripeFinalizeTierForCustomer records the customer's billing country and
// brings every subscription still in its trial (or waiting on the payment sheet)
// onto the tier that country resolves to: the item price is switched BEFORE the
// trial ends (nothing has been charged yet, no proration), the metadata is
// updated, a pending payment sheet is marked attached and its trial invoice is
// credited now. Idempotent, so any of the three webhooks may run it. stripe.Key
// must be set.
func stripeFinalizeTierForCustomer(clientSession *session.ClientSession, customerId string, countryCode string) {
	networkId := model.GetStripeCustomerNetworkId(clientSession.Ctx, customerId)
	if networkId == nil {
		// not a customer of ours (a buy-data checkout customer, or a legacy one)
		return
	}
	code := model.NormalizeCountryCode(countryCode)
	if code != "" {
		model.SetStripeCustomerBillingCountry(clientSession.Ctx, customerId, code)
	}
	now := server.NowUtc()

	iter := subscription.List(&stripe.SubscriptionListParams{
		Customer: stripe.String(customerId),
		Status:   stripe.String("all"),
	})
	for iter.Next() {
		sub := iter.Subscription()
		if sub.Metadata[stripeMetadataNetworkId] != networkId.String() {
			continue
		}
		switch sub.Status {
		case stripe.SubscriptionStatusCanceled, stripe.SubscriptionStatusIncompleteExpired, stripe.SubscriptionStatusUnpaid:
			continue
		}
		plan := sub.Metadata[stripeMetadataPlan]
		currentTier := sub.Metadata[stripeMetadataPriceTier]
		pending := sub.Metadata[stripeMetadataPaymentSheet] == stripePaymentSheetPending
		inTrial := sub.Status == stripe.SubscriptionStatusTrialing && 0 < sub.TrialEnd && now.Before(time.Unix(sub.TrialEnd, 0))

		metadata := map[string]string{}
		var items []*stripe.SubscriptionItemsParams
		if code != "" && plan != "" && (inTrial || pending) {
			tier := model.Pro().PriceTierForCountry(code)
			if tier.Name != currentTier {
				priceId, err := StripePriceIdForTier(tier, plan)
				if err != nil {
					glog.Errorf("[stripe]could not resolve %s price for tier %s: %s\n", plan, tier.Name, err)
				} else if sub.Items != nil && len(sub.Items.Data) == 1 {
					items = []*stripe.SubscriptionItemsParams{{
						ID:    stripe.String(sub.Items.Data[0].ID),
						Price: stripe.String(priceId),
					}}
					metadata[stripeMetadataPriceTier] = tier.Name
					glog.Infof(
						"[stripe]subscription %s for network %s moves from tier %s to %s (billing country %s) before the trial ends\n",
						sub.ID, networkId, currentTier, tier.Name, code,
					)
				}
			}
		}
		if pending && (sub.DefaultPaymentMethod != nil || code != "") {
			metadata[stripeMetadataPaymentSheet] = stripePaymentSheetAttached
		}
		if len(metadata) == 0 {
			continue
		}
		params := &stripe.SubscriptionParams{Metadata: metadata}
		if items != nil {
			params.Items = items
			params.ProrationBehavior = stripe.String("none")
		}
		updated, err := subscription.Update(sub.ID, params)
		if err != nil {
			glog.Errorf("[stripe]could not update subscription %s: %s\n", sub.ID, err)
			continue
		}
		if pending && metadata[stripeMetadataPaymentSheet] == stripePaymentSheetAttached && updated.LatestInvoice != nil {
			// the trial invoice held back while the sheet was pending credits now
			stripeCreditHeldInvoice(clientSession, updated.LatestInvoice.ID, customerId)
		}
	}
	if err := iter.Err(); err != nil {
		glog.Errorf("[stripe]could not list subscriptions for customer %s: %s\n", customerId, err)
	}
}

// stripeCreditHeldInvoice runs the invoice.paid crediting for an invoice whose
// webhook was answered without a credit (the pending payment sheet).
func stripeCreditHeldInvoice(clientSession *session.ClientSession, invoiceId string, customerId string) {
	if invoiceId == "" {
		return
	}
	total := 0
	fullInvoice, err := server.HttpGetRequireStatusOk[*struct {
		Total  int    `json:"total"`
		Status string `json:"status"`
	}](
		clientSession.Ctx,
		fmt.Sprintf("%s/v1/invoices/%s", stripeApiBaseUrl, invoiceId),
		func(header http.Header) {
			header.Add("Authorization", fmt.Sprintf("Bearer %s", stripeApiTokenFunc()))
		},
		server.ResponseJsonObject[*struct {
			Total  int    `json:"total"`
			Status string `json:"status"`
		}],
	)
	if err == nil && fullInvoice != nil {
		if fullInvoice.Status != "paid" {
			// not paid yet: the regular invoice.paid webhook will credit it
			return
		}
		total = fullInvoice.Total
	}
	if _, err := stripeHandleInvoicePaidWithOnboarding(&StripeEventInvoiceObject{
		Id:       invoiceId,
		Total:    total,
		Customer: customerId,
	}, clientSession); err != nil {
		glog.Errorf("[stripe]could not credit held invoice %s: %s\n", invoiceId, err)
	}
}
