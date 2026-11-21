//go:build sidecar_pending

package controller

// The reconciler's Team leg (QUOTA-M6-07; owner decision 199), on Postgres with MG6 against the
// fake Stripe of team_stripe_fake_test.go: a Team subscription Stripe shows deleted whose
// customer.subscription.deleted never arrived is ended within one run, as the event would have
// ended it, and a live one is left alone.

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestReconcileEndsATeamSubscriptionWhoseDeletionWasLost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		periods := newTestTeamPeriods()
		networkId, _, subscriptionId := newTestPaidTeam(t, ctx, env, periods, "lostdeletion")
		liveNetworkId, _, liveSubscriptionId := newTestPaidTeam(t, ctx, env, periods, "stillalive")
		const duplicate = "sub_synthetic_legacy_other_customer"
		const personal = "sub_synthetic_legacy_personal"
		env.setSubscription(duplicate, liveNetworkId, true, "active")
		env.setSubscription(personal, liveNetworkId, false, "active")
		// An exact duplicate was classified, but its cancellation failed and the
		// legitimate contract ended before another invoice or webhook arrived.
		orphanNet, orphanSession, orphanRetained := newTestPaidTeam(t, ctx, env, periods, "endedowner")
		const orphan = "sub_synthetic_pending_cancellation"
		const orphanInvoice = "in_synthetic_pending_cancellation"
		env.setSubscription(orphan, orphanNet, true, "active")
		setTestTeamCycleInvoice(env, periods, orphan, orphanInvoice, "paid")
		env.rejectNextCancellation = true
		_, deliveryErr := StripeWebhook(stripeWebhookEvent(t, "invoice.paid", map[string]any{
			"id": orphanInvoice, "object": "invoice", "status": "paid", "total": 14293,
			"currency": "usd", "billing_reason": "subscription_cycle", "customer": "cus_synthetic_" + orphan,
		}), orphanSession)
		if deliveryErr == nil {
			t.Fatal("the unconfirmed cancellation was acknowledged")
		}
		// Stripe ended it, and the event was lost
		env.setSubscriptionStatus(subscriptionId, "canceled")

		run := func(dryRun bool) *PaymentReconcileRunResult {
			t.Helper()
			defer disableAllReconcileStores(t)()
			stripeReconcileHasCredentials = func() bool { return true }
			result, err := RunPaymentReconciliationWithOptions(reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{DryRun: dryRun})
			if err != nil || result == nil {
				t.Fatalf("reconcile: %+v %v", result, err)
			}
			return result
		}

		// a dry run ends nothing
		env.stateLock.Lock()
		env.listedPaidInvoiceIds = []string{orphanInvoice}
		env.stateLock.Unlock()
		run(true)
		env.stateLock.Lock()
		env.listedPaidInvoiceIds = nil
		env.stateLock.Unlock()
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldCredit, orphanInvoice), 0)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionCreditUnfulfillable, orphanInvoice), 2)
		connect.AssertEqual(t, model.GetLiveTeamSubscription(ctx, networkId) != nil, true)
		connect.AssertEqual(t, len(env.confirmedCancellations()), 0)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldEnd, orphan), 1)
		connect.AssertEqual(t, countPaymentReconciliationEventRows(t, ctx, model.SubscriptionMarketStripe, model.PaymentReconcileActionWouldEnd, duplicate), 1)

		env.setSubscriptionStatus(orphanRetained, "canceled")
		env.deliverSubscriptionDeleted(t, orphanSession, orphanRetained)
		connect.AssertEqual(t, model.GetLiveTeamSubscription(ctx, orphanNet) == nil, true)
		run(false)
		connect.AssertEqual(t, env.confirmedCancellations()[duplicate], 1)
		connect.AssertEqual(t, env.confirmedCancellations()[orphan], 1)
		connect.AssertEqual(t, env.confirmedCancellations()[personal], 0)
		connect.AssertEqual(t, env.confirmedCancellations()[liveSubscriptionId], 0)
		subscription := model.GetTeamSubscription(ctx, networkId, subscriptionId)
		connect.AssertEqual(t, subscription != nil && subscription.EndTime != nil, true)
		live := model.GetLiveTeamSubscription(ctx, liveNetworkId)
		connect.AssertEqual(t, live != nil && live.SubscriptionId == liveSubscriptionId, true)
		// its line ended, as the event would have ended it
		now := server.NowUtc()
		line := testTeamBalanceAt(testTeamBalances(t, ctx, networkId), string(model.GrantKindTeamCredit), periods.firstStart)
		connect.AssertEqual(t, line != nil && !line.endTime.After(now), true)

		// a second run ends nothing more; the job grants the live Team alone
		run(false)
		connect.AssertEqual(t, env.confirmedCancellations()[duplicate], 1)
		connect.AssertEqual(t, env.confirmedCancellations()[orphan], 1)
		connect.AssertEqual(t, len(model.GetTeamSubscriptions(ctx, networkId)), 1)
		grants := model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, grants[0].NetworkId, liveNetworkId)
	})
}
