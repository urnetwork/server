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
		run(true)
		connect.AssertEqual(t, model.GetLiveTeamSubscription(ctx, networkId) != nil, true)

		run(false)
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
		connect.AssertEqual(t, len(model.GetTeamSubscriptions(ctx, networkId)), 1)
		grants := model.RefreshTeamTransferBalances(ctx, server.NowUtc(), server.NewId())
		connect.AssertEqual(t, len(grants), 1)
		connect.AssertEqual(t, grants[0].NetworkId, liveNetworkId)
	})
}
