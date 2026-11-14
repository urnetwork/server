package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The current subscription names the first active market that is not a
// redeemed gift year, so a subscriber's manage or cancel link survives a gift;
// a gift alone is "gift"; among the other markets it is the first, as
// MIN(market) named it.
func TestCurrentSubscriptionStorePrefersAPayingStore(t *testing.T) {
	for _, test := range []struct {
		markets []model.SubscriptionMarket
		store   model.SubscriptionMarket
		ok      bool
	}{
		{nil, "", false},
		{[]model.SubscriptionMarket{model.SubscriptionMarketGift}, model.SubscriptionMarketGift, true},
		{[]model.SubscriptionMarket{model.SubscriptionMarketGift, model.SubscriptionMarketStripe}, model.SubscriptionMarketStripe, true},
		{[]model.SubscriptionMarket{model.SubscriptionMarketGift, model.SubscriptionMarketGoogle}, model.SubscriptionMarketGoogle, true},
		{[]model.SubscriptionMarket{model.SubscriptionMarketApple, model.SubscriptionMarketGift}, model.SubscriptionMarketApple, true},
		{[]model.SubscriptionMarket{model.SubscriptionMarketGift, model.SubscriptionMarketMonero, model.SubscriptionMarketSolana, model.SubscriptionMarketZcash}, model.SubscriptionMarketMonero, true},
		{[]model.SubscriptionMarket{model.SubscriptionMarketGamma, model.SubscriptionMarketGift, model.SubscriptionMarketStripe}, model.SubscriptionMarketGamma, true},
		// a legacy row without a market sorts first, and names no store, as before
		{[]model.SubscriptionMarket{"", model.SubscriptionMarketGift}, "", true},
	} {
		store, ok := currentSubscriptionStore(test.markets)
		if store != test.store || ok != test.ok {
			t.Errorf("markets %q name %q (%t), want %q (%t)", test.markets, store, ok, test.store, test.ok)
		}
		// without a gift, the store is MIN(market), as before
		withoutGift := slices.DeleteFunc(slices.Clone(test.markets), func(market model.SubscriptionMarket) bool {
			return market == model.SubscriptionMarketGift
		})
		if 0 < len(withoutGift) && store != slices.Min(withoutGift) {
			t.Errorf("markets %q name %q, want the least other market %q", test.markets, store, slices.Min(withoutGift))
		}
	}
}

// Through the balance endpoint: a Stripe, Play or App Store subscriber with a
// gift year running beside its window keeps its own store as the current
// subscription; a gift alone is the current subscription; a gift queued
// behind a running Stripe period leaves Stripe current. Every active store is
// still listed.
func TestSubscriptionBalanceCurrentStoreIsNeverAGiftOverAPayingStore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		subscriber := func(name string) (server.Id, *session.ClientSession) {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-%s-%s", name, networkId.String()[:8]), userId)
			clientId := server.NewId()
			return networkId, session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, ClientId: &clientId})
		}
		renew := func(networkId server.Id, market model.SubscriptionMarket, start time.Time, paidEnd time.Time, grace time.Duration) {
			if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          start,
				EndTime:            paidEnd.Add(grace),
				NetRevenue:         model.UsdToNanoCents(5),
				SubscriptionMarket: market,
				TransactionId:      fmt.Sprintf("synthetic-%s-%s", market, networkId),
				PurchaseToken:      fmt.Sprintf("synthetic-token-%s", networkId),
				PaidEndTime:        &paidEnd,
			}); err != nil {
				t.Fatal(err)
			}
			model.AddTransferBalance(ctx, &model.TransferBalance{
				NetworkId:         networkId,
				StartTime:         start,
				EndTime:           paidEnd.Add(grace),
				SubsidyNetRevenue: model.UsdToNanoCents(5),
				Pro:               true,
			})
		}
		redeem := func(clientSession *session.ClientSession, wantQueued bool) {
			codes := networkCreateGiftTestCodes(t, ctx, 1, nil)
			result, err := RedeemBalanceCode(RedeemBalanceCodeArgs{Secret: codes[0].Secret}, clientSession)
			if err != nil || result.Error != nil || result.Pro == nil || result.Pro.Queued != wantQueued {
				t.Fatalf("redeem: %+v %v", result, err)
			}
		}
		current := func(clientSession *session.ClientSession) (string, []string) {
			balance, err := SubscriptionBalance(clientSession)
			if err != nil {
				t.Fatal(err)
			}
			stores := []string{}
			for _, subscription := range balance.Subscriptions {
				stores = append(stores, subscription.Store)
			}
			if balance.CurrentSubscription == nil {
				return "<none>", stores
			}
			return balance.CurrentSubscription.Store, stores
		}

		// a gift year running, then the subscription renews beside it
		for _, market := range []model.SubscriptionMarket{model.SubscriptionMarketStripe, model.SubscriptionMarketGoogle, model.SubscriptionMarketApple} {
			networkId, clientSession := subscriber("gift-and-" + market)
			redeem(clientSession, false)
			now := server.NowUtc()
			renew(networkId, market, now.Add(-time.Minute), now.Add(30*24*time.Hour), 24*time.Hour)
			store, stores := current(clientSession)
			want := []string{model.SubscriptionMarketGift, market}
			slices.Sort(want)
			if store != market || strings.Join(stores, ",") != strings.Join(want, ",") {
				t.Fatalf("%s and a gift: the current store is %q of %v, want %q of %v", market, store, stores, market, want)
			}
		}

		// a gift alone
		_, giftSession := subscriber("gift-alone")
		redeem(giftSession, false)
		if store, stores := current(giftSession); store != model.SubscriptionMarketGift || strings.Join(stores, ",") != model.SubscriptionMarketGift {
			t.Fatalf("a gift alone: the current store is %q of %v", store, stores)
		}

		// a gift queued behind a running Stripe period
		stripeNetworkId, stripeSession := subscriber("stripe-then-gift")
		now := server.NowUtc()
		renew(stripeNetworkId, model.SubscriptionMarketStripe, now.Add(-24*time.Hour), now.Add(29*24*time.Hour), 30*24*time.Hour)
		redeem(stripeSession, true)
		if store, stores := current(stripeSession); store != model.SubscriptionMarketStripe || strings.Join(stores, ",") != model.SubscriptionMarketStripe {
			t.Fatalf("a gift queued behind Stripe: the current store is %q of %v", store, stores)
		}
	})
}
