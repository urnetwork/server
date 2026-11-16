package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Root-cause tests for the plan a store purchase is posted and recorded on.
// Play sells both plans under one product id, "supporter": the base plan
// (supporter-base monthly, supporter-yearly yearly) is the only thing that
// names the plan. The product id read as the App Store's legacy monthly
// "supporter" labelled every Play sale "Pro monthly", penguin1234's yearly
// trial among them (owner report 2026-11-08), and recorded every Play trial
// outcome with no plan.

// playPenguinSubscription is penguin1234's purchase as subscriptionsv2 serves
// it: the yearly base plan's 14-day trial offer, at the yearly list price of
// 39.99 USD, the purchase's first order.
func playPenguinSubscription(start time.Time, expiry time.Time) *PlaySubscription {
	return &PlaySubscription{
		LineItems: []*PlaySubscriptionPurchaseLineItem{{
			ProductId:               "supporter",
			ExpiryTime:              expiry.UTC().Format(time.RFC3339),
			LatestSuccessfulOrderId: "GPA.3300-0000-0000-00001",
			AutoRenewingPlan: &PlayAutoRenewingPlan{
				AutoRenewEnabled: true,
				RecurringPrice:   json.RawMessage(`{"currencyCode":"USD","units":"39","nanos":990000000}`),
			},
			OfferDetails: &PlayOfferDetails{BasePlanId: "supporter-yearly", OfferId: "trial-14d"},
		}},
		StartTime:            start.UTC().Format(time.RFC3339),
		RegionCode:           "US",
		SubscriptionState:    "SUBSCRIPTION_STATE_ACTIVE",
		AcknowledgementState: "ACKNOWLEDGEMENT_STATE_ACKNOWLEDGED",
		LatestOrderId:        "GPA.3300-0000-0000-00001",
	}
}

func TestPlaySalesNoticeNamesTheBasePlan(t *testing.T) {
	networkId := server.NewId()
	sku := &Sku{Supporter: true, PriceAmountUsd: 5, FeeFraction: 0.3}
	args := &PlaySubscriptionRenewalArgs{NetworkId: networkId, SubscriptionId: "supporter", PurchaseToken: "token-penguin"}
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	expiry := start.Add(14 * 24 * time.Hour)

	sub := playPenguinSubscription(start, expiry)
	connect.AssertEqual(t, playSalesNotice(args, sub, sku, expiry, playCreditNew).Item, "Pro yearly")

	// the monthly base plan
	sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "supporter-base"}
	connect.AssertEqual(t, playSalesNotice(args, sub, sku, expiry, playCreditNew).Item, "Pro monthly")

	// a base plan this server does not know is read by its words
	sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "pro-annual-2027"}
	connect.AssertEqual(t, playSalesNotice(args, sub, sku, expiry, playCreditNew).Item, "Pro yearly")

	// and otherwise names no plan, rather than the wrong one
	sub.LineItems[0].OfferDetails = &PlayOfferDetails{BasePlanId: "pro-2027"}
	connect.AssertEqual(t, playSalesNotice(args, sub, sku, expiry, playCreditNew).Item, "Pro")
	sub.LineItems[0].OfferDetails = nil
	connect.AssertEqual(t, playSalesNotice(args, sub, sku, expiry, playCreditNew).Item, "Pro")
}

// The Play webhook records a trial that stops renewing as trial.cancelled
// with the purchase's plan: penguin1234's trial is the yearly plan's. The
// trial's remaining days are still the customer's: cancelled is not expired,
// so a first notice that finds the trial already cancelled credits it.
func TestPlayTrialOutcomeRecordsTheBasePlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newPlayWebhookTestEnv(t, map[string]*Sku{
			"supporter": {Supporter: true, PriceAmountUsd: 5, FeeFraction: 0.3},
		})
		type outcome struct {
			store string
			plan  string
		}
		cancelled := []outcome{}
		previousCancelled := playStoreTrialCancelled
		playStoreTrialCancelled = func(ctx context.Context, networkId server.Id, store string, plan string, now time.Time) bool {
			cancelled = append(cancelled, outcome{store: store, plan: plan})
			return true
		}
		t.Cleanup(func() { playStoreTrialCancelled = previousCancelled })

		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "penguin1234", server.NewId())
		start := server.NowUtc().Add(-time.Hour).Truncate(time.Second)
		sub := playPenguinSubscription(start, start.Add(14*24*time.Hour))
		// auto-renew turned off during the trial
		sub.SubscriptionState = "SUBSCRIPTION_STATE_CANCELED"
		sub.LineItems[0].AutoRenewingPlan.AutoRenewEnabled = false
		sub.ExternalAccountIdentifiers = &PlayExternalAccountIdentifiers{ExternalAccountId: networkId.String()}
		env.subscriptions["token-penguin"] = sub

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		_, err := PlayWebhook(env.webhookArgs(t, "supporter", "token-penguin"), clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, cancelled, []outcome{{store: model.OnboardingStorePlay, plan: model.PlanYearly}})
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), true)
	})
}
