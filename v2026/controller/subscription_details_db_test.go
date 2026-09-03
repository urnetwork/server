package controller

// Database-backed walk-through of the "Manage subscription" screen: a network
// billed by Stripe and by a one-time USDC payment reads its details, the
// details are cached for a minute, cancelling the Stripe subscription flips
// cancel_at_period_end at Stripe and invalidates the cache, resuming flips
// it back. Needs the test database and redis (server.DefaultTestEnv); the
// Stripe API is the fake from subscription_details_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func TestSubscriptionDetailsDbListsCancelsAndResumes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "managesub", userId)
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		// nothing billing yet: an empty list, no customer, nothing cached that
		// could hide a later purchase for long
		details, err := SubscriptionDetails(clientSession)
		if err != nil || len(details.Subscriptions) != 0 || details.HasStripeCustomer {
			t.Fatalf("fresh network: %+v %v", details, err)
		}
		clearSubscriptionDetailsCached(ctx, networkId)

		env := newStripeSubscriptionTestEnv(t)
		stripePeriodEnd := now.Add(25 * 24 * time.Hour).Truncate(time.Second)
		env.addSubscription("cus_managesub", "sub_managesub", "active", false, stripePeriodEnd, "month")
		env.invoiceSubscription["in_managesub"] = "sub_managesub"
		if err := model.CreateStripeCustomer("cus_managesub", clientSession); err != nil {
			t.Fatal(err)
		}

		// the stripe window our webhook wrote (its end differs from stripe's
		// period end by the grace period: the store's date wins on the screen)
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-5 * 24 * time.Hour),
			EndTime:            now.Add(25*24*time.Hour + SubscriptionGracePeriod),
			NetRevenue:         model.UsdToNanoCents(5),
			SubscriptionMarket: model.SubscriptionMarketStripe,
			TransactionId:      "in_managesub",
		}); err != nil {
			t.Fatal(err)
		}
		// a yearly usdc payment, credited through its intent
		reference := "ManageSubTestRef111111111111111111111111111"
		if err := model.CreateSolanaPaymentIntent(reference, 40, model.SolanaPlanYearly, clientSession); err != nil {
			t.Fatal(err)
		}
		solanaEnd := now.Add(360 * 24 * time.Hour)
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-5 * 24 * time.Hour),
			EndTime:            solanaEnd,
			NetRevenue:         model.UsdToNanoCents(40),
			SubscriptionMarket: model.SubscriptionMarketSolana,
			TransactionId:      reference,
		}); err != nil {
			t.Fatal(err)
		}

		details, err = SubscriptionDetails(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if !details.HasStripeCustomer || len(details.Subscriptions) != 2 {
			t.Fatalf("expected a stripe customer and 2 entries: %+v", details)
		}
		solana, stripe := details.Subscriptions[0], details.Subscriptions[1]
		if solana.Store != model.SubscriptionMarketSolana || stripe.Store != model.SubscriptionMarketStripe {
			t.Fatalf("stores: %s %s", solana.Store, stripe.Store)
		}
		if stripe.AutoRenew == nil || !*stripe.AutoRenew || !stripe.CanCancel || stripe.CancelAtPeriodEnd {
			t.Fatalf("stripe renews and can be cancelled here: %+v", stripe)
		}
		if !stripe.EndTime.Equal(stripePeriodEnd) {
			t.Fatalf("stripe: the store's period end %v, got %v", stripePeriodEnd, stripe.EndTime)
		}
		if stripe.Cadence != SubscriptionCadenceMonthly {
			t.Fatalf("stripe cadence %q", stripe.Cadence)
		}
		if solana.AutoRenew == nil || *solana.AutoRenew || solana.CanCancel || solana.Cadence != SubscriptionCadenceYearly {
			t.Fatalf("solana: one yearly window, no renewal: %+v", solana)
		}
		if !solana.EndTime.Equal(solanaEnd) {
			t.Fatalf("solana end %v, got %v", solanaEnd, solana.EndTime)
		}

		// cached: a change at stripe is not seen within the ttl
		env.subscriptions["sub_managesub"]["cancel_at_period_end"] = true
		cached, err := SubscriptionDetails(clientSession)
		if err != nil || cached.Subscriptions[1].CancelAtPeriodEnd {
			t.Fatalf("expected the cached answer: %+v %v", cached, err)
		}
		if !cached.UpdateTime.Equal(details.UpdateTime) {
			t.Fatal("expected the same cached result")
		}
		env.subscriptions["sub_managesub"]["cancel_at_period_end"] = false

		// cancel: stripe flips cancel_at_period_end, the customer keeps the
		// period, the cache is dropped so the screen shows it at once
		cancelResult, err := SubscriptionCancel(&SubscriptionCancelArgs{Store: "stripe"}, clientSession)
		if err != nil || cancelResult.Error != nil {
			t.Fatalf("cancel: %+v %v", cancelResult, err)
		}
		if cancelResult.AutoRenew == nil || *cancelResult.AutoRenew || cancelResult.EndTime == nil || !cancelResult.EndTime.Equal(stripePeriodEnd) {
			t.Fatalf("cancel result: %+v", cancelResult)
		}
		if len(env.updates) != 1 || env.updates[0] != "true" {
			t.Fatalf("expected one cancel_at_period_end=true update, got %v", env.updates)
		}
		details, err = SubscriptionDetails(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		stripe = details.Subscriptions[1]
		if stripe.AutoRenew == nil || *stripe.AutoRenew || !stripe.CancelAtPeriodEnd || !stripe.EndTime.Equal(stripePeriodEnd) {
			t.Fatalf("after cancel: %+v", stripe)
		}
		// cancelling again is a no-op at stripe
		if _, err := SubscriptionCancel(&SubscriptionCancelArgs{Store: "stripe"}, clientSession); err != nil {
			t.Fatal(err)
		}
		if len(env.updates) != 1 {
			t.Fatalf("a second cancel must not write again: %v", env.updates)
		}

		// resume
		resumeResult, err := SubscriptionResume(&SubscriptionCancelArgs{Store: "stripe"}, clientSession)
		if err != nil || resumeResult.Error != nil || resumeResult.AutoRenew == nil || !*resumeResult.AutoRenew {
			t.Fatalf("resume: %+v %v", resumeResult, err)
		}
		details, err = SubscriptionDetails(clientSession)
		if err != nil || details.Subscriptions[1].CancelAtPeriodEnd || !*details.Subscriptions[1].AutoRenew {
			t.Fatalf("after resume: %+v %v", details, err)
		}

		// the other store on the same network: refused with where to go
		refused, err := SubscriptionCancel(&SubscriptionCancelArgs{Store: "solana"}, clientSession)
		if err != nil || refused.Error == nil {
			t.Fatalf("solana cancel: %+v %v", refused, err)
		}
		apple, _ := SubscriptionCancel(&SubscriptionCancelArgs{Store: "apple"}, clientSession)
		if apple.Error == nil || apple.ManageUrl != appleManageSubscriptionsUrl {
			t.Fatalf("apple cancel: %+v", apple)
		}

		// the balance endpoint's wire shape is untouched
		balance, err := SubscriptionBalance(clientSession)
		if err != nil || len(balance.Subscriptions) != 2 {
			t.Fatalf("balance still lists the stores: %+v %v", balance, err)
		}
	})
}

// TestSubscriptionDetailsDbNoCustomerNoPortal: a network billed only by the App
// Store never had a Stripe customer; the details say so (the web hides the
// portal link instead of showing "No stripe customer found") and cancelling
// on stripe is refused without touching Stripe.
func TestSubscriptionDetailsDbNoCustomerNoPortal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := server.NewId()
		userId := server.NewId()
		clientId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "managesubapple", userId)
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})
		env := newStripeSubscriptionTestEnv(t)

		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-1 * 24 * time.Hour),
			EndTime:            now.Add(364 * 24 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(40),
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      "2000000999",
		}); err != nil {
			t.Fatal(err)
		}

		details, err := SubscriptionDetails(clientSession)
		if err != nil || details.HasStripeCustomer || len(details.Subscriptions) != 1 {
			t.Fatalf("apple only: %+v %v", details, err)
		}
		apple := details.Subscriptions[0]
		if apple.Store != model.SubscriptionMarketApple || apple.ManageUrl != appleManageSubscriptionsUrl || apple.CanCancel {
			t.Fatalf("apple entry: %+v", apple)
		}
		// no App Store credentials in the test env: unknown, with the row's window
		if apple.AutoRenew != nil || !apple.EndTime.Equal(now.Add(364*24*time.Hour)) || apple.Cadence != SubscriptionCadenceYearly {
			t.Fatalf("apple entry without a lookup: %+v", apple)
		}

		result, err := SubscriptionCancel(&SubscriptionCancelArgs{Store: "stripe"}, clientSession)
		if err != nil || result.Error == nil {
			t.Fatalf("no stripe subscription: %+v %v", result, err)
		}
		if len(env.updates) != 0 {
			t.Fatal("nothing must be written to stripe")
		}
	})
}
