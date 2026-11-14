package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The task moves a gift year up once the App Store year it waited behind is
// refunded, reports it, leaves the next run nothing to do, and its post-hook
// arms the next hourly run.
func TestRebaseQueuedGiftYearsTaskMovesUpAWaitingYear(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-requeue-%s", networkId.String()[:8]), userId)
		friendSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "friend", false, false))

		// an App Store year, paid, eleven months to run
		now := server.NowUtc()
		paidEnd := now.Add(330 * 24 * time.Hour)
		transactionId := "synthetic-apple-" + networkId.String()
		if err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          now.Add(-35 * 24 * time.Hour),
			EndTime:            paidEnd.Add(24 * time.Hour),
			NetRevenue:         model.UsdToNanoCents(40),
			SubscriptionMarket: model.SubscriptionMarketApple,
			TransactionId:      transactionId,
			PaidEndTime:        &paidEnd,
		}); err != nil {
			t.Fatal(err)
		}
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:         networkId,
			StartTime:         now.Add(-35 * 24 * time.Hour),
			EndTime:           paidEnd.Add(24 * time.Hour),
			SubsidyNetRevenue: model.UsdToNanoCents(40),
			Pro:               true,
		})
		codes := networkCreateGiftTestCodes(t, ctx, 1, nil)
		redeemed, err := RedeemBalanceCode(RedeemBalanceCodeArgs{Secret: codes[0].Secret}, friendSession)
		if err != nil || redeemed.Error != nil || !redeemed.Pro.Queued {
			t.Fatalf("redeem: %+v %v", redeemed, err)
		}

		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		if result, err := RebaseQueuedGiftYears(&RebaseQueuedGiftYearsArgs{}, clientSession); err != nil || result.NetworkCount != 0 {
			t.Fatalf("while the App Store year runs the task moved %+v, %v", result, err)
		}

		// Apple refunds the year
		model.EndReconciledEntitlementForTransactions(ctx, model.SubscriptionMarketApple, []string{transactionId}, server.NowUtc())
		model.UpdateProNetwork(ctx, networkId)
		if model.IsProNetwork(ctx, networkId) {
			t.Fatal("the network kept Pro after its year was refunded")
		}

		result, err := RebaseQueuedGiftYears(&RebaseQueuedGiftYearsArgs{}, clientSession)
		if err != nil || result.NetworkCount != 1 || result.YearCount != 1 {
			t.Fatalf("the task moved %+v, %v", result, err)
		}
		if !model.IsProNetwork(ctx, networkId) {
			t.Fatal("the gift year did not start once the App Store year was refunded")
		}
		if again, err := RebaseQueuedGiftYears(&RebaseQueuedGiftYearsArgs{}, clientSession); err != nil || again.NetworkCount != 0 {
			t.Fatalf("a second run moved %+v, %v", again, err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			if err := RebaseQueuedGiftYearsPost(&RebaseQueuedGiftYearsArgs{}, result, clientSession, tx); err != nil {
				t.Fatal(err)
			}
		})
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT run_at FROM pending_task WHERE run_once_key = $1`,
				task.RunOnce("rebase_queued_gift_years").String(),
			).Scan(&runAt))
		})
		if delay := runAt.Sub(server.NowUtc()); delay <= 0 || RebaseQueuedGiftYearsInterval < delay {
			t.Fatalf("the next run is at %s, want within the hour", runAt)
		}
	})
}
