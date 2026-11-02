// SPDX-License-Identifier: MPL-2.0

package controller

// The win-back offer (mmm/reengagement/PLAN.md "THE STORES", decision 5,
// accepted 2026-11-01): the monthly plan's first month at half price on Google
// Play and Stripe for the W experiment's offer arm, Apple's own win-back offer
// on the monthly product for the networks the App Store lists, nothing on
// yearly, nothing for crypto. The W1 step issues it right before its email
// (issueWinbackOfferForSend), the plan response shows it while it is active
// (winbackOfferResult), and each store's purchase path applies or recognizes
// it and stamps it redeemed (recordWinbackRedemptionInTx):
//
//   - Stripe: a per-customer promotion code on the comeback coupon (one
//     redemption, expiring with the offer), applied server-side when the
//     network buys the monthly plan (stripeWinbackDiscount); the paid invoice
//     redeems it.
//   - Google Play: the app selects the monthly base plan's offer by its tag
//     while the plan response carries the offer; a credit bought under the tag
//     redeems it. Play does not enforce a developer-determined offer, so the
//     deadline is the plan response's.
//   - App Store: only when the App Store Server API lists the configured offer
//     in the subscription's eligibleWinBackOfferIds. The email states no App
//     Store price (guideline 2.3.1(a)); the landing opens Apple's direct link;
//     a transaction of offer type 4 with the offer's identifier redeems it.
//
// Every rule the decision applies is pure (reengagement.WinbackOfferRefusal,
// reengagement.WinbackShapeRefusal); this file loads the facts and talks to
// the stores.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/reengagement"
	"github.com/urnetwork/server/session"

	"github.com/stripe/stripe-go/v82"
	"github.com/stripe/stripe-go/v82/coupon"
	"github.com/stripe/stripe-go/v82/customer"
	"github.com/stripe/stripe-go/v82/promotioncode"
)

var reengagementWinbackOffersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "winback_offers_total",
	Help:      "W1 comeback offer outcomes at send time by store: issued, existing (a replayed W1 found its own offer), or why none was issued (disabled, no_store, holdout, no_offer_arm, below_minimum_charge, country, never_paid, recent_offer, apple_ineligible, apple_error, store_error).",
}, []string{"store", "result"})

var reengagementWinbackRedemptionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "reengagement",
	Name:      "winback_redemptions_total",
	Help:      "Purchases made under a win-back offer's handle by store: redeemed (stamped the network's offer), unissued (the network was never issued an offer on that store), unmatched (the purchase fell outside the network's unredeemed offer), failed (the stamp could not be written; the credit went through).",
}, []string{"store", "result"})

func init() {
	prometheus.MustRegister(
		reengagementWinbackOffersTotal,
		reengagementWinbackRedemptionsTotal,
	)
}

// Offer outcomes that are not reengagement.WinbackRefusal* values.
const (
	winbackOfferIssued   = "issued"
	winbackOfferExisting = "existing"
	// the App Store Server API answered with a final error, or this deployment
	// has no credentials for it: the email goes without the App Store line
	winbackOfferAppleError = "apple_error"
	// Stripe could not make the promotion code; the step is retried
	winbackOfferStoreError = "store_error"
)

// Redemption outcomes.
const (
	winbackRedemptionRedeemed  = "redeemed"
	winbackRedemptionUnissued  = "unissued"
	winbackRedemptionUnmatched = "unmatched"
	winbackRedemptionFailed    = "failed"
)

func winbackStoreLabel(store string) string {
	if store == "" {
		return "none"
	}
	return store
}

// ----- the decision -----

// winbackOfferFacts loads what the offer decision reads for a W1 row at `now`.
// An offer issued in this episode does not count as recent: a replayed W1
// finds it and mails it (issueWinbackOfferForSend).
func winbackOfferFacts(ctx context.Context, row *model.NetworkReengagement, now time.Time) reengagement.WinbackOfferFacts {
	cfg := model.Reengagement()
	f := reengagement.WinbackOfferFacts{
		Enabled: cfg.WinbackOfferEnabled(),
		Arm:     row.Variant,
	}
	if !f.Enabled {
		return f
	}
	if _, market, ok := model.NetworkLastPaidWindow(ctx, row.NetworkId); ok {
		if store := reengagement.WinbackStoreForMarket(market); cfg.WinbackStoreHandle(store) != "" {
			f.Store = store
		}
	}
	if f.Store == reengagement.WinbackStoreStripe {
		f.StripeBelowMinimumCharge = !winbackStripeChargeableForNetwork(ctx, row.NetworkId, row.Country, cfg.WinbackOffer.PercentOff)
	}
	f.CountryAllowed = model.Onboarding().MailPolicy().OfferAllowed(row.Country)
	f.Paid = model.NetworkHasPaidSupporterPeriod(ctx, row.NetworkId)
	if latest := model.GetLatestWinbackOffer(ctx, row.NetworkId); latest != nil && latest.IssuedAt.Before(row.StartedAt) {
		f.OfferedWithinInterval = now.Before(latest.IssuedAt.Add(reengagement.WinbackOfferInterval))
	}
	return f
}

// winbackStripeChargeableForNetwork reports whether Stripe charges the
// offer's first month at every tier the network may be priced on at checkout:
// its Stripe billing country's and its own country's (none known: the default
// tier's). Checkout prices the purchase again and applies the code only when
// Stripe charges it there too (stripeWinbackOfferForPurchase).
func winbackStripeChargeableForNetwork(ctx context.Context, networkId server.Id, country string, percentOff int) bool {
	pro := model.Pro()
	countries := []string{}
	for _, code := range []string{model.GetStripeCustomerBillingCountry(ctx, networkId), country} {
		if code = model.NormalizeCountryCode(code); code != "" {
			countries = append(countries, code)
		}
	}
	if len(countries) == 0 {
		countries = append(countries, "")
	}
	for _, code := range countries {
		if !reengagement.WinbackStripeChargeable(pro.PriceTierForCountry(code).MonthlyUsd, percentOff) {
			return false
		}
	}
	return true
}

// winbackComebackOfferAllowed is the decision's Facts.OfferAllowed: the
// network may be mailed W1's offer variant (Google Play and Stripe; the App
// Store's offer is a line in the default variant).
func winbackComebackOfferAllowed(f reengagement.WinbackOfferFacts) bool {
	return reengagement.WinbackOfferRefusal(f) == "" && f.Store != reengagement.WinbackStoreApple
}

// ----- the issue, right before the W1 email -----

// issueWinbackOfferForSend issues (or, for a replayed W1, finds) the offer the
// W1 email is about to carry, and returns the decision the email goes out
// with: the offer variant only with an offer of Google Play or Stripe, the
// default variant otherwise, with the App Store line when the offer is the
// App Store's. Runs only when the program is enabled, never for the holdout.
// A Stripe failure, or an App Store Server API failure that may pass, is
// returned so the step retries before anything is mailed.
func issueWinbackOfferForSend(ctx context.Context, row *model.NetworkReengagement, decision reengagement.Decision, now time.Time) (*model.WinbackOffer, reengagement.Decision, error) {
	cfg := model.Reengagement()
	withOffer := func(offer *model.WinbackOffer, result string) (*model.WinbackOffer, reengagement.Decision, error) {
		reengagementWinbackOffersTotal.WithLabelValues(winbackStoreLabel(offer.Store), result).Inc()
		if offer.Store == reengagement.WinbackStoreApple {
			decision.Variant = reengagement.VariantDefault
		} else {
			decision.Variant = reengagement.VariantOffer
		}
		return offer, decision, nil
	}
	without := func(store string, result string) (*model.WinbackOffer, reengagement.Decision, error) {
		reengagementWinbackOffersTotal.WithLabelValues(winbackStoreLabel(store), result).Inc()
		decision.Variant = reengagement.VariantDefault
		return nil, decision, nil
	}

	// a W1 replayed after its send failed mails the offer it already issued
	if existing := model.GetLatestWinbackOffer(ctx, row.NetworkId); existing != nil && !existing.IssuedAt.Before(row.StartedAt) {
		if existing.Active(now) {
			return withOffer(existing, winbackOfferExisting)
		}
		return without(existing.Store, reengagement.WinbackRefusalRecent)
	}

	f := winbackOfferFacts(ctx, row, now)
	if refusal := reengagement.WinbackOfferRefusal(f); refusal != "" {
		return without(f.Store, refusal)
	}
	issuedAt := now.UTC().Truncate(time.Second)
	args := &model.IssueWinbackOfferArgs{
		NetworkId:  row.NetworkId,
		Store:      f.Store,
		Plan:       strings.TrimSpace(cfg.WinbackOffer.Plan),
		PercentOff: cfg.WinbackOffer.PercentOff,
		IssuedAt:   issuedAt,
		ExpiresAt:  issuedAt.Add(cfg.WinbackOfferValidity()),
	}
	handle := cfg.WinbackStoreHandle(f.Store)
	switch f.Store {
	case reengagement.WinbackStoreApple:
		eligible, err := appleWinbackOfferEligibleFunc(ctx, row.NetworkId, handle)
		if err != nil {
			if appleWinbackErrorPasses(err) {
				glog.Infof("[reengagement]winback: App Store eligibility for network %s will be asked again: %s\n", row.NetworkId, err)
				return nil, decision, err
			}
			glog.Errorf("[reengagement]winback: App Store eligibility for network %s: %s\n", row.NetworkId, err)
			return without(f.Store, winbackOfferAppleError)
		}
		if !eligible {
			return without(f.Store, reengagement.WinbackRefusalAppleIneligible)
		}
		args.AppleOfferId = handle
	case reengagement.WinbackStorePlay:
		args.PlayOfferTag = handle
	case reengagement.WinbackStoreStripe:
		promotionCodeId, err := stripeWinbackPromotionCodeForNetworkFunc(ctx, row.NetworkId, args.ExpiresAt)
		if err != nil {
			reengagementWinbackOffersTotal.WithLabelValues(f.Store, winbackOfferStoreError).Inc()
			return nil, decision, fmt.Errorf("win-back promotion code: %w", err)
		}
		args.StripePromotionCodeId = promotionCodeId
	}

	offer, created, err := model.IssueWinbackOffer(ctx, args)
	if err != nil {
		// a network deleted under the step: the retry exits the episode
		return nil, decision, err
	}
	if !created {
		// a concurrent issue won; the promotion code made here is never used
		// and expires with the offer it was made for
		if offer.Active(now) && !offer.IssuedAt.Before(row.StartedAt) {
			return withOffer(offer, winbackOfferExisting)
		}
		return without(offer.Store, reengagement.WinbackRefusalRecent)
	}
	glog.Infof("[reengagement]winback: issued %s offer %s to network %s until %s\n", offer.Store, offer.OfferId, offer.NetworkId, offer.ExpiresAt.Format(time.RFC3339))
	return withOffer(offer, winbackOfferIssued)
}

// ----- the plan response -----

// WinbackOfferResult is the plan response's winback_offer: the network's
// active win-back offer, omitted when there is none or it expired or was
// redeemed. An app shows it on its own store's plan screen only: Stripe
// applies it at checkout, the Play app selects the monthly base plan's offer
// tagged play_offer_tag, the App Store app presents Apple's win-back offer
// apple_offer_id.
type WinbackOfferResult struct {
	ExpiresAt  time.Time `json:"expires_at"`
	PercentOff int       `json:"percent_off"`
	Plan       string    `json:"plan"`
	// stripe | play | apple
	Store        string `json:"store"`
	PlayOfferTag string `json:"play_offer_tag,omitempty"`
	AppleOfferId string `json:"apple_offer_id,omitempty"`
}

func winbackOfferResult(offer *model.WinbackOffer, now time.Time) *WinbackOfferResult {
	if !offer.Active(now) {
		return nil
	}
	return winbackOfferResultOf(offer)
}

// winbackPlanOfferResult is winbackOfferResult for a caller priced at `tier`:
// a Stripe offer whose first month Stripe would not charge at that tier is not
// shown, because checkout would not apply it (stripeWinbackOfferForPurchase).
func winbackPlanOfferResult(offer *model.WinbackOffer, tier *model.ProPriceTier, now time.Time) *WinbackOfferResult {
	if !offer.Active(now) {
		return nil
	}
	if offer.Store == reengagement.WinbackStoreStripe && !reengagement.WinbackStripeChargeable(tier.PriceUsd(offer.Plan), offer.PercentOff) {
		return nil
	}
	return winbackOfferResultOf(offer)
}

func winbackOfferResultOf(offer *model.WinbackOffer) *WinbackOfferResult {
	result := &WinbackOfferResult{
		ExpiresAt:  offer.ExpiresAt.UTC(),
		PercentOff: offer.PercentOff,
		Plan:       offer.Plan,
		Store:      offer.Store,
	}
	switch offer.Store {
	case reengagement.WinbackStorePlay:
		if offer.PlayOfferTag != nil {
			result.PlayOfferTag = *offer.PlayOfferTag
		}
	case reengagement.WinbackStoreApple:
		if offer.AppleOfferId != nil {
			result.AppleOfferId = *offer.AppleOfferId
		}
	}
	return result
}

// winbackLandingOpenUrl is where the W1 landing sends a network whose offer is
// the App Store's: Apple's direct link to the win-back offer, while the offer
// is active and the link is configured. "" sends it to the plan screen.
func winbackLandingOpenUrl(ctx context.Context, networkId server.Id, now time.Time) string {
	link := model.Reengagement().WinbackAppleOfferUrl()
	if link == "" {
		return ""
	}
	if offer := model.GetActiveWinbackOffer(ctx, networkId, now); offer != nil && offer.Store == reengagement.WinbackStoreApple {
		return link
	}
	return ""
}

// ----- redemption, in the purchase paths -----

// recordWinbackRedemptionInTx stamps the network's offer on `store` redeemed
// by a purchase made at `at` under the offer's handle (with offerId, only that
// offer), and counts the outcome. A renewal of the purchase that redeemed the
// offer counts nothing. Runs under a savepoint inside the store's credit
// transaction, so a failure here rolls back only the stamp and never the
// credit; a failure that ends the whole transaction (a serialization failure,
// a deadlock, the caller's stop) is raised to the credit's own handling.
func recordWinbackRedemptionInTx(tx server.PgTx, ctx context.Context, networkId server.Id, store string, offerId *server.Id, at time.Time) {
	server.RaisePgResult(tx.Exec(ctx, `SAVEPOINT winback_redemption`))
	result, failure := func() (result string, failure any) {
		defer func() {
			if recovered := recover(); recovered != nil {
				failure = recovered
			}
		}()
		if redeemed := model.RedeemWinbackOfferInTx(tx, ctx, networkId, store, offerId, at); redeemed != nil {
			glog.Infof("[reengagement]winback: offer %s redeemed on %s by network %s\n", *redeemed, store, networkId)
			result = winbackRedemptionRedeemed
			return
		}
		latest := model.LatestWinbackOfferForStoreInTx(tx, ctx, networkId, store)
		switch {
		case latest == nil:
			result = winbackRedemptionUnissued
		case latest.RedeemedAt != nil:
			// a renewal, or a redelivery, of the purchase that redeemed it
		default:
			result = winbackRedemptionUnmatched
		}
		return
	}()
	if failure == nil {
		server.RaisePgResult(tx.Exec(ctx, `RELEASE SAVEPOINT winback_redemption`))
		if result != "" {
			reengagementWinbackRedemptionsTotal.WithLabelValues(store, result).Inc()
		}
		return
	}
	if winbackRedemptionFailureReruns(ctx, failure) {
		panic(failure)
	}
	server.RaisePgResult(tx.Exec(ctx, `ROLLBACK TO SAVEPOINT winback_redemption`))
	reengagementWinbackRedemptionsTotal.WithLabelValues(store, winbackRedemptionFailed).Inc()
	glog.Errorf("[reengagement]winback: the %s redemption for network %s was not stamped; the credit went through: %v\n", store, networkId, failure)
}

// Whether a failure while stamping a redemption ends the whole transaction
// anyway: the caller stopped, or postgres rolled it back (class 40), which
// server.Tx reruns.
func winbackRedemptionFailureReruns(ctx context.Context, failure any) bool {
	if ctx.Err() != nil {
		return true
	}
	err, ok := failure.(error)
	if !ok {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "40")
}

// playRecordWinbackInTx: a Play credit bought under the comeback offer's tag
// redeems the network's Play offer, at Google's startTime.
func playRecordWinbackInTx(tx server.PgTx, ctx context.Context, sub *PlaySubscription, renewal *model.SubscriptionRenewal) {
	tag := model.Reengagement().WinbackStoreHandle(reengagement.WinbackStorePlay)
	if tag == "" || !sub.HasOfferTag(tag) {
		return
	}
	recordWinbackRedemptionInTx(tx, ctx, renewal.NetworkId, reengagement.WinbackStorePlay, nil, renewal.StartTime)
}

// appleRecordWinbackInTx: an App Store transaction of offer type 4 (win-back)
// with the configured offer's identifier redeems the network's App Store
// offer, at the transaction's purchase time.
func appleRecordWinbackInTx(tx server.PgTx, ctx context.Context, transaction *validatedAppleTransaction) {
	offerId := model.Reengagement().WinbackStoreHandle(reengagement.WinbackStoreApple)
	if transaction.offerType != appleOfferTypeWinBack || offerId == "" || transaction.offerIdentifier != offerId {
		return
	}
	recordWinbackRedemptionInTx(tx, ctx, transaction.networkId, reengagement.WinbackStoreApple, nil, transaction.purchaseTime)
}

// ----- Stripe -----

// The Stripe subscription metadata key that names the win-back offer a
// subscription was started with (its offer id).
const stripeMetadataWinbackOffer = "winback_offer"

var stripeWinbackCouponCache sync.Map

// StripeWinbackCouponId creates or fetches the comeback coupon
// (reengagement.yml winback_offer.stripe_coupon_id): percent_off off one
// invoice (duration once), so a monthly subscription's first month is
// discounted and every later one is the regular price. A coupon of any other
// shape under the id is refused, never applied. Monthly and yearly prices
// share one product, so the coupon cannot name the monthly price: the server
// applies it to monthly purchases only. stripe.Key must be set.
func StripeWinbackCouponId() (string, error) {
	cfg := model.Reengagement()
	id := strings.TrimSpace(cfg.WinbackOffer.StripeCouponId)
	if id == "" {
		return "", fmt.Errorf("the win-back coupon is not configured")
	}
	if cached, ok := stripeWinbackCouponCache.Load(id); ok {
		return cached.(string), nil
	}
	existing, err := coupon.Get(id, nil)
	if err == nil && existing != nil && existing.Valid {
		if refusal := stripeWinbackCouponRefusal(existing, cfg.WinbackOffer.PercentOff); refusal != "" {
			return "", fmt.Errorf("coupon %s %s", id, refusal)
		}
		stripeWinbackCouponCache.Store(id, existing.ID)
		return existing.ID, nil
	}
	var stripeErr *stripe.Error
	if err != nil && !(errors.As(err, &stripeErr) && stripeErr.HTTPStatusCode == http.StatusNotFound) {
		return "", err
	}
	created, err := coupon.New(&stripe.CouponParams{
		ID:         stripe.String(id),
		Name:       stripe.String(fmt.Sprintf("Comeback offer: %d%% off the first month", cfg.WinbackOffer.PercentOff)),
		PercentOff: stripe.Float64(float64(cfg.WinbackOffer.PercentOff)),
		Duration:   stripe.String(string(stripe.CouponDurationOnce)),
		Metadata:   map[string]string{"program": "reengagement"},
	})
	if err != nil {
		return "", err
	}
	glog.Infof("[stripe]created win-back coupon %s\n", created.ID)
	stripeWinbackCouponCache.Store(id, created.ID)
	return created.ID, nil
}

// stripeWinbackCouponRefusal is why an existing coupon may not stand for the
// offer, or "": it must be percent_off off one invoice and nothing more.
func stripeWinbackCouponRefusal(c *stripe.Coupon, percentOff int) string {
	switch {
	case math.Abs(c.PercentOff-float64(percentOff)) > 0.001:
		return fmt.Sprintf("is %g%% off, config says %d%%", c.PercentOff, percentOff)
	case c.Duration != stripe.CouponDurationOnce:
		return fmt.Sprintf("has duration %s, not once", c.Duration)
	case 0 < c.AmountOff:
		return "takes an amount off"
	}
	return ""
}

// stripeWinbackCustomerIdForNetwork is the customer a win-back promotion code
// is restricted to: the network's Stripe customer as the billing portal finds
// it (its failing renewal's, the one the server made, its last credited
// renewal's invoice's), else one made (metadata network_id) like the payment
// sheet's. Either way it is recorded for the network (stripe_customer), so the
// network's next checkout is made for that customer and the code applies.
// stripe.Key must be set.
func stripeWinbackCustomerIdForNetwork(ctx context.Context, networkId server.Id, now time.Time) (string, error) {
	existing, err := stripeCustomerIdForNetwork(ctx, networkId, now, stripeDunningSettingsFunc())
	if err != nil {
		return "", err
	}
	if existing != "" {
		if model.GetStripeCustomerIdForNetwork(ctx, networkId) == "" {
			// a Checkout customer the server never recorded; another
			// network's customer is never taken (stripe_customer_id is unique)
			if err := model.CreateStripeCustomerForNetwork(ctx, networkId, existing); err != nil {
				glog.Infof("[stripe]win-back customer %s for network %s was not recorded: %s\n", existing, networkId, err)
			}
		}
		return existing, nil
	}
	created, err := customer.New(&stripe.CustomerParams{
		Metadata: map[string]string{stripeMetadataNetworkId: networkId.String()},
	})
	if err != nil {
		return "", err
	}
	if err := model.CreateStripeCustomerForNetwork(ctx, networkId, created.ID); err != nil {
		// lost a race with a concurrent create: use the stored one
		if existing := model.GetStripeCustomerIdForNetwork(ctx, networkId); existing != "" {
			return existing, nil
		}
		return "", err
	}
	return created.ID, nil
}

// stripeCreateWinbackPromotionCode makes an offer's promotion code: the
// comeback coupon, for one customer, redeemable once, expiring with the offer.
// The code is applied by the server and never shown, so Stripe names it.
// stripe.Key must be set.
func stripeCreateWinbackPromotionCode(customerId string, networkId server.Id, expiresAt time.Time) (string, error) {
	couponId, err := StripeWinbackCouponId()
	if err != nil {
		return "", err
	}
	created, err := promotioncode.New(&stripe.PromotionCodeParams{
		Coupon:         stripe.String(couponId),
		Customer:       stripe.String(customerId),
		MaxRedemptions: stripe.Int64(1),
		ExpiresAt:      stripe.Int64(expiresAt.Unix()),
		Metadata: map[string]string{
			stripeMetadataNetworkId: networkId.String(),
			"program":               "reengagement",
		},
	})
	if err != nil {
		return "", err
	}
	return created.ID, nil
}

// stripeWinbackPromotionCodeForNetworkFunc makes the promotion code of an
// offer about to be issued: the network's customer (made if it has none) and
// a code on the comeback coupon expiring at expiresAt. Tests replace it.
var stripeWinbackPromotionCodeForNetworkFunc = func(ctx context.Context, networkId server.Id, expiresAt time.Time) (string, error) {
	stripe.Key = stripeApiToken()
	customerId, err := stripeWinbackCustomerIdForNetwork(ctx, networkId, server.NowUtc())
	if err != nil {
		return "", fmt.Errorf("customer: %w", err)
	}
	return stripeCreateWinbackPromotionCode(customerId, networkId, expiresAt)
}

// stripeWinbackPromotionCodeUsable reports whether Stripe will still apply the
// promotion code for the customer at `now`: active, redemptions left, before
// its expiry, and restricted to that customer.
func stripeWinbackPromotionCodeUsable(promo *stripe.PromotionCode, customerId string, now time.Time) bool {
	return promo != nil &&
		promo.Active &&
		(promo.MaxRedemptions == 0 || promo.TimesRedeemed < promo.MaxRedemptions) &&
		(promo.ExpiresAt == 0 || now.Unix() < promo.ExpiresAt) &&
		promo.Customer != nil && promo.Customer.ID == customerId
}

// stripeEnsureWinbackPromotionCode is the offer's promotion code for a
// checkout by customerId. A code a checkout used up without paying (a payment
// sheet abandoned after its subscription was made), or one made for another
// customer, is replaced by a fresh one with the offer's own expiry, so the
// offer stays redeemable until its deadline and never past it. stripe.Key must
// be set.
func stripeEnsureWinbackPromotionCode(ctx context.Context, offer *model.WinbackOffer, customerId string, now time.Time) (string, error) {
	if offer.StripePromotionCodeId != nil && *offer.StripePromotionCodeId != "" {
		promo, err := promotioncode.Get(*offer.StripePromotionCodeId, nil)
		if err == nil && stripeWinbackPromotionCodeUsable(promo, customerId, now) {
			return promo.ID, nil
		}
		var stripeErr *stripe.Error
		if err != nil && !(errors.As(err, &stripeErr) && stripeErr.HTTPStatusCode == http.StatusNotFound) {
			return "", err
		}
	}
	promotionCodeId, err := stripeCreateWinbackPromotionCode(customerId, offer.NetworkId, offer.ExpiresAt)
	if err != nil {
		return "", err
	}
	if !model.SetWinbackOfferStripePromotionCode(ctx, offer.OfferId, promotionCodeId, now) {
		return "", fmt.Errorf("win-back offer %s ended", offer.OfferId)
	}
	glog.Infof("[stripe]win-back offer %s for network %s: promotion code replaced by %s\n", offer.OfferId, offer.NetworkId, promotionCodeId)
	return promotionCodeId, nil
}

// stripeWinbackOfferForPurchase is the network's active Stripe win-back offer
// when the purchase is of its plan and Stripe charges the discounted first
// month at the purchase's price (priceUsd, the caller's tier price of the
// plan), else nil. Below Stripe's minimum charge Stripe would mark the first
// invoice paid without charging it (reengagement.StripeMinimumChargeUsdCents),
// so the purchase goes at the regular price; W1 issues no Stripe offer to a
// network priced there, so this only meets a network priced differently at
// checkout than when the offer was issued.
func stripeWinbackOfferForPurchase(clientSession *session.ClientSession, plan string, priceUsd float64, now time.Time) *model.WinbackOffer {
	offer := model.GetActiveWinbackOffer(clientSession.Ctx, clientSession.ByJwt.NetworkId, now)
	if offer == nil || offer.Store != reengagement.WinbackStoreStripe || offer.Plan != plan {
		return nil
	}
	if !reengagement.WinbackStripeChargeable(priceUsd, offer.PercentOff) {
		glog.Infof(
			"[stripe]win-back offer %s for network %s is not applied: %d%% off %.2f USD is below Stripe's minimum charge\n",
			offer.OfferId, offer.NetworkId, offer.PercentOff, priceUsd,
		)
		return nil
	}
	return offer
}

// stripeWinbackDiscount is the promotion code to apply to a purchase of `plan`
// at priceUsd by customerId: the network's active Stripe win-back offer's
// (stripeWinbackOfferForPurchase), else "". A code that cannot be made
// applies no discount and is logged; the offer stays active for the next
// attempt. stripe.Key must be set.
func stripeWinbackDiscount(clientSession *session.ClientSession, plan string, priceUsd float64, customerId string) (promotionCodeId string, offer *model.WinbackOffer) {
	now := server.NowUtc()
	offer = stripeWinbackOfferForPurchase(clientSession, plan, priceUsd, now)
	if offer == nil || customerId == "" {
		return "", nil
	}
	promotionCodeId, err := stripeEnsureWinbackPromotionCode(clientSession.Ctx, offer, customerId, now)
	if err != nil {
		glog.Errorf("[stripe]win-back promotion code unavailable for network %s: %s\n", offer.NetworkId, err)
		return "", nil
	}
	return promotionCodeId, offer
}

// stripeRecordWinbackForInvoice stamps the win-back offer a subscription was
// started with redeemed when its invoice is paid, at the subscription's
// creation (when Stripe applied the promotion code, before its expiry).
// Renewal invoices find the offer redeemed and count nothing.
func stripeRecordWinbackForInvoice(ctx context.Context, sub *stripe.Subscription) {
	raw := sub.Metadata[stripeMetadataWinbackOffer]
	if raw == "" {
		return
	}
	offerId, err := server.ParseId(raw)
	if err != nil {
		return
	}
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		return
	}
	at := server.NowUtc()
	if 0 < sub.Created {
		at = time.Unix(sub.Created, 0).UTC()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		recordWinbackRedemptionInTx(tx, ctx, networkId, reengagement.WinbackStoreStripe, &offerId, at)
	}, server.TxReadCommitted)
}

// ----- App Store -----

// appleWinbackOfferEligibleFunc asks the App Store Server API whether Apple
// lists the win-back offer for the network's subscription. Tests replace it.
var appleWinbackOfferEligibleFunc = appleWinbackOfferEligible

// appleWinbackOfferEligible asks Get All Subscription Statuses (production;
// a sandbox purchase is never mailed an offer) for the subscription of the
// network's latest App Store transaction, and reports whether the configured
// offer is in the renewal info's eligibleWinBackOfferIds. Apple decides the
// eligibility (paid at least a month, lapsed 1 to 24 months, 12 months between
// redemptions, never in billing retry or grace); StoreKit's documentation asks
// for this check before a direct link is sent.
func appleWinbackOfferEligible(ctx context.Context, networkId server.Id, offerId string) (bool, error) {
	if offerId == "" {
		return false, nil
	}
	transactionId := model.NetworkLastAppleTransactionId(ctx, networkId)
	if transactionId == "" {
		return false, nil
	}
	if appleReconcileCredentialsFunc() == nil {
		return false, errAppleWinbackNoCredentials
	}
	statuses, err := appleGetSubscriptionStatuses(ctx, appleAppStoreServerApiBaseUrl, transactionId)
	if err != nil {
		if appleTransactionIdNotFound(err) {
			// a sandbox or unknown transaction: no offer
			return false, nil
		}
		return false, err
	}
	for _, id := range appleEligibleWinbackOfferIds(statuses) {
		if id == offerId {
			return true, nil
		}
	}
	return false, nil
}

// appleEligibleWinBackOfferIds is every win-back offer identifier the App
// Store lists for the customer's subscriptions, best offer first per
// subscription; none when the response is not one Apple documents. StoreKit's
// documentation and Apple's server libraries put eligibleWinBackOfferIds in
// the renewal info; the API reference names the transaction info, so both are
// read.
func appleEligibleWinbackOfferIds(statuses *appleSubscriptionStatusesResponse) []string {
	if validateAppleSubscriptionStatuses(statuses) != nil {
		return nil
	}
	ids := []string{}
	for _, group := range statuses.Data {
		for _, transaction := range group.LastTransactions {
			for _, jws := range []string{transaction.SignedRenewalInfo, transaction.SignedTransactionInfo} {
				if jws == "" {
					continue
				}
				claims, err := appleDecodeJwsPayload(jws)
				if err != nil {
					continue
				}
				values, _ := claims["eligibleWinBackOfferIds"].([]any)
				for _, value := range values {
					if id, ok := value.(string); ok && id != "" && !slices.Contains(ids, id) {
						ids = append(ids, id)
					}
				}
			}
		}
	}
	return ids
}

// errAppleWinbackNoCredentials: this deployment cannot ask the App Store
// Server API (vault apple.yml has no API key), so no network is listed.
var errAppleWinbackNoCredentials = errors.New("no App Store Server API credentials")

// appleWinbackErrorPasses reports whether an App Store Server API failure may
// pass (a transport error, a rate limit, Apple's 5xx): the step is retried
// rather than mailed without the App Store line. Missing credentials and a 4xx
// are final.
func appleWinbackErrorPasses(err error) bool {
	if errors.Is(err, errAppleWinbackNoCredentials) {
		return false
	}
	var statusErr *server.HttpStatusError
	if !errors.As(err, &statusErr) {
		return true
	}
	return statusErr.StatusCode == http.StatusTooManyRequests || 500 <= statusErr.StatusCode
}
