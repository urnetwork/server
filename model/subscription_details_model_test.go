package model

// Database-backed: a store's last renewal row, whatever its window, beside the
// rows that cover now. Needs the test database (server.DefaultTestEnv).

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestGetLastSubscriptionRenewal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "lastrenewal", userId)

		now := server.NowUtc()
		day := 24 * time.Hour
		renewal := func(market SubscriptionMarket, startOffsetDays int, endOffsetDays int, transactionId string) {
			err := AddSubscriptionRenewal(ctx, &SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   SubscriptionTypeSupporter,
				StartTime:          now.Add(time.Duration(startOffsetDays) * day),
				EndTime:            now.Add(time.Duration(endOffsetDays) * day),
				NetRevenue:         NanoCents(0),
				SubscriptionMarket: market,
				TransactionId:      transactionId,
			})
			if err != nil {
				t.Fatal(err)
			}
		}

		if last := GetLastSubscriptionRenewal(ctx, networkId, SubscriptionTypeSupporter, SubscriptionMarketStripe); last != nil {
			t.Fatalf("no stripe window yet: %+v", last)
		}

		// two paid stripe years, both over: the later one names the subscription
		renewal(SubscriptionMarketStripe, -740, -370, "in_first_year")
		renewal(SubscriptionMarketStripe, -370, -5, "in_second_year")
		// another store's window that ends later is not stripe's
		renewal(SubscriptionMarketApple, -10, 20, "2000000555")

		last := GetLastSubscriptionRenewal(ctx, networkId, SubscriptionTypeSupporter, SubscriptionMarketStripe)
		if last == nil || last.Market != SubscriptionMarketStripe || last.TransactionId != "in_second_year" {
			t.Fatalf("expected the stripe window that ended last, got %+v", last)
		}
		if !last.EndTime.Before(now) {
			t.Fatalf("the last stripe window is over: %+v", last)
		}
		// while no stripe row covers now
		for _, active := range GetActiveSubscriptionRenewals(ctx, networkId, SubscriptionTypeSupporter) {
			if active.Market == SubscriptionMarketStripe {
				t.Fatalf("no stripe window covers now: %+v", active)
			}
		}

		// a store that never billed the network
		if last := GetLastSubscriptionRenewal(ctx, networkId, SubscriptionTypeSupporter, SubscriptionMarketGoogle); last != nil {
			t.Fatalf("google never billed the network: %+v", last)
		}
	})
}
