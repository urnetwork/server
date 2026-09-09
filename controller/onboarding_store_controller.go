package controller

import (
	"context"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The store purchase paths' onboarding bookkeeping: record the regional price
// tier on the subscription row and stamp the welcome offer redeemed. Each helper
// runs after the store's own crediting, inside its transaction where there is
// one, and never fails the credit.

// Apple offerType claim values (App Store Server API JWSTransactionDecodedPayload).
const (
	appleOfferTypeIntroductory = 1
	appleOfferTypePromotional  = 2
	appleOfferTypeOfferCode    = 3
	appleOfferTypeWinBack      = 4
)

// appleRecordOnboardingInTx: the tier from the transaction's storefront, and the
// offer when the purchase used an offer code (the one-time or custom code the
// offer carries) or a promotional offer.
func appleRecordOnboardingInTx(tx server.PgTx, ctx context.Context, transaction *validatedAppleTransaction) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]apple record failed for transaction %s: %v\n", transaction.transactionId, r)
		}
	}()
	if code := model.CountryCodeFromStorefront(transaction.storefront); code != "" {
		tier := model.Pro().PriceTierForCountry(code)
		model.SetSubscriptionRenewalPriceTierByTransactionInTx(
			tx, ctx, transaction.networkId, model.SubscriptionMarketApple, transaction.transactionId, tier.Name,
		)
	}
	switch transaction.offerType {
	case appleOfferTypeOfferCode, appleOfferTypePromotional:
		if model.RedeemOnboardingOfferInTx(tx, ctx, transaction.networkId, model.OnboardingStoreApple, server.NowUtc()) {
			glog.Infof(
				"[onboarding]offer redeemed on apple by network %s (transaction %s, offer %q)\n",
				transaction.networkId, transaction.transactionId, transaction.offerIdentifier,
			)
		}
	}
}

// playRecordOnboardingInTx: the tier from the purchase's region code, and the
// offer when a line item was bought under the welcome offer's tag.
func playRecordOnboardingInTx(tx server.PgTx, clientSession *session.ClientSession, sub *PlaySubscription, renewal *model.SubscriptionRenewal) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]play record failed for network %s: %v\n", renewal.NetworkId, r)
		}
	}()
	ctx := clientSession.Ctx
	if code := model.NormalizeCountryCode(sub.RegionCode); code != "" {
		tier := model.Pro().PriceTierForCountry(code)
		model.SetSubscriptionRenewalPriceTierByPurchaseTokenInTx(
			tx, ctx, renewal.NetworkId, model.SubscriptionMarketGoogle, renewal.PurchaseToken, renewal.EndTime, tier.Name,
		)
	}
	if tag := model.Onboarding().Offer.PlayOfferTag; tag != "" && sub.HasOfferTag(tag) {
		if model.RedeemOnboardingOfferInTx(tx, ctx, renewal.NetworkId, model.OnboardingStorePlay, server.NowUtc()) {
			glog.Infof("[onboarding]offer redeemed on play by network %s (tag %s)\n", renewal.NetworkId, tag)
		}
	}
}

// solanaRecordOnboarding: the tier recovered from the quoted price (an intent
// carries a price, not a tier), and the offer when the plan was the welcome
// offer.
func solanaRecordOnboarding(clientSession *session.ClientSession, paymentSearchResult *model.PaymentIntentSearchResult) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[onboarding]solana record failed for reference %s: %v\n", paymentSearchResult.PaymentReference, r)
		}
	}()
	if paymentSearchResult == nil || paymentSearchResult.NetworkId == nil {
		return
	}
	networkId := *paymentSearchResult.NetworkId
	plan := paymentSearchResult.SubscriptionPlan
	offerApplied := plan == model.SolanaPlanYearlyOnboarding
	basePlan := plan
	if offerApplied {
		basePlan = model.PlanYearly
	}
	if basePlan == "" {
		basePlan = model.PlanYearly
	}
	if tier := model.Pro().PriceTierForPrice(basePlan, paymentSearchResult.ExpectedAmountUsd, model.Onboarding().Offer.PercentOff); tier != "" {
		model.SetSubscriptionRenewalPriceTierByTransaction(
			clientSession.Ctx, networkId, model.SubscriptionMarketSolana, paymentSearchResult.PaymentReference, tier,
		)
	}
	if offerApplied {
		if model.RedeemOnboardingOffer(clientSession.Ctx, networkId, model.OnboardingStoreSolana, server.NowUtc()) {
			glog.Infof("[onboarding]offer redeemed on solana by network %s (reference %s)\n", networkId, paymentSearchResult.PaymentReference)
		}
	}
}
