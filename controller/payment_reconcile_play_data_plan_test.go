// The hourly reconciler's Google Play data plan leg (reconcilePlayDataPlans).
// A data plan writes no renewal row, so the reconciler never looked at one: a
// data plan whose renewal poll was gone was left to its store notifications
// alone, and a renewal whose notification was refused went uncredited. The
// leg gives an active data plan with no pending poll its poll back. The tests
// run the reconciler against the fake Play API (play_webhook_test.go) with
// the poll tests' products (play_subscription_renewal_poll_test.go).
package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The leg's audit actions (model.PaymentReconcileActionPollRearmed and
// model.PaymentReconcileActionWouldRearmPoll), as the event rows carry them;
// spelled out so the tests build against the code before the leg and fail
// there.
const (
	playDataPlanPollRearmed    = "poll_rearmed"
	playDataPlanWouldRearmPoll = "would_rearm_poll"
)

// A data plan's credited window as the credit gate writes it: a balance with
// the purchase token, no Pro, the plan's data, and subsidy revenue only.
func seedPlayDataPlanWindow(
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
	startTime time.Time,
	endTime time.Time,
) {
	model.AddTransferBalance(ctx, &model.TransferBalance{
		NetworkId:             networkId,
		StartTime:             startTime,
		EndTime:               endTime,
		StartBalanceByteCount: 10 * model.Gib,
		BalanceByteCount:      10 * model.Gib,
		SubsidyNetRevenue:     model.UsdToNanoCents(3.5),
		PurchaseToken:         purchaseToken,
		Pro:                   false,
	})
}

// A data plan with its window open, which Play reports paid through next
// month, and no renewal poll pending gets its poll back, due at the expiry
// Play reports, recorded as poll_rearmed. The leg credits nothing, and a
// second run, with the poll pending, re-arms nothing. A data plan whose window
// ended, and one Play reports expired, get no poll. Before the fix the
// reconciler never looked at a data plan: no poll came back.
func TestPaymentReconcilePlayDataPlanWithNoPollGetsOne(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		playEnv := newPlayPollTestEnv(t)
		previousHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = previousHasCredentials })

		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour
		start := now.Add(-40 * day)

		// active: its window is open and Play reports it paid through next month
		activeNetworkId := newPlayPollTestNetwork(ctx, "reconcile-data-active")
		activeToken := "synthetic-reconcile-data-active"
		activeExpiry := now.Add(20 * day)
		seedPlayDataPlanWindow(ctx, activeNetworkId, activeToken, start, activeExpiry.Add(SubscriptionGracePeriod))
		playEnv.subscriptions[activeToken] = playTestSubscription(activeNetworkId, playPollTestDataProductId, start, activeExpiry)

		// ended: its window ended yesterday
		endedNetworkId := newPlayPollTestNetwork(ctx, "reconcile-data-ended")
		endedToken := "synthetic-reconcile-data-ended"
		seedPlayDataPlanWindow(ctx, endedNetworkId, endedToken, start, now.Add(-day))
		ended := playTestSubscription(endedNetworkId, playPollTestDataProductId, start, now.Add(-2*day))
		ended.SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"
		playEnv.subscriptions[endedToken] = ended

		// expired at Play while its window still runs out its grace
		expiredNetworkId := newPlayPollTestNetwork(ctx, "reconcile-data-expired")
		expiredToken := "synthetic-reconcile-data-expired"
		seedPlayDataPlanWindow(ctx, expiredNetworkId, expiredToken, start, now.Add(12*time.Hour))
		expired := playTestSubscription(expiredNetworkId, playPollTestDataProductId, start, now.Add(-12*time.Hour))
		expired.SubscriptionState = "SUBSCRIPTION_STATE_EXPIRED"
		playEnv.subscriptions[expiredToken] = expired

		result, err := RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		connect.AssertEqual(t, result.Credited, 0)

		count, runAt := countScheduledPlayRenewals(t, ctx, activeToken)
		if count != 1 || !runAt.Equal(activeExpiry) {
			t.Fatalf("the active data plan has %d polls, the first at %s, want one at its expiry %s", count, runAt, activeExpiry)
		}
		for _, token := range []string{endedToken, expiredToken} {
			if count, _ := countScheduledPlayRenewals(t, ctx, token); count != 0 {
				t.Fatalf("%s: %d polls scheduled, want none for a plan that ended", token, count)
			}
		}
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketGoogle, playDataPlanPollRearmed), 1)
		storeResultJson, err := json.Marshal(result.StoreResults[model.SubscriptionMarketGoogle])
		connect.AssertEqual(t, err, nil)
		storeResult := map[string]any{}
		connect.AssertEqual(t, json.Unmarshal(storeResultJson, &storeResult), nil)
		connect.AssertEqual(t, storeResult["polls_rearmed"], float64(1))
		// the leg credits nothing: the poll does, when it runs
		connect.AssertEqual(t, countPlayDataBalances(t, ctx, activeNetworkId, activeToken), 1)

		// the poll is pending: a second run re-arms nothing
		result, err = RunPaymentReconciliation(reconcileTestSession(t, ctx))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		events = model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketGoogle, playDataPlanPollRearmed), 0)
		count, runAt = countScheduledPlayRenewals(t, ctx, activeToken)
		if count != 1 || !runAt.Equal(activeExpiry) {
			t.Fatalf("after a second run the active data plan has %d polls, the first at %s, want the one at %s", count, runAt, activeExpiry)
		}
	})
}

// A dry run reads Play and records would_rearm_poll, and schedules nothing.
func TestPaymentReconcilePlayDataPlanDryRunSchedulesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer disableAllReconcileStores(t)()
		playEnv := newPlayPollTestEnv(t)
		previousHasCredentials := playReconcileHasCredentials
		playReconcileHasCredentials = func() bool { return true }
		t.Cleanup(func() { playReconcileHasCredentials = previousHasCredentials })

		now := server.NowUtc().Truncate(time.Second)
		start := now.Add(-40 * 24 * time.Hour)
		networkId := newPlayPollTestNetwork(ctx, "reconcile-data-dry")
		token := "synthetic-reconcile-data-dry"
		expiry := now.Add(20 * 24 * time.Hour)
		seedPlayDataPlanWindow(ctx, networkId, token, start, expiry.Add(SubscriptionGracePeriod))
		playEnv.subscriptions[token] = playTestSubscription(networkId, playPollTestDataProductId, start, expiry)

		result, err := RunPaymentReconciliationWithOptions(reconcileTestSession(t, ctx), &PaymentReconcileRunOptions{
			DryRun: true,
			Stores: []string{model.SubscriptionMarketGoogle},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Errors, 0)
		events := model.GetPaymentReconciliationEvents(ctx, result.RunId)
		connect.AssertEqual(t, countReconcileEvents(events, model.SubscriptionMarketGoogle, playDataPlanWouldRearmPoll), 1)
		if count, _ := countScheduledPlayRenewals(t, ctx, token); count != 0 {
			t.Fatalf("a dry run scheduled %d polls, want none", count)
		}
	})
}
