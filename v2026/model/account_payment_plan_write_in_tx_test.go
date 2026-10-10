// The payout loop runs its caller's write in each committed slice's own
// transaction, after the slice's payments are written, and never for a dry run.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
)

// The account_payment rows of the plan, counted with `query`.
func planPaymentCount(t testing.TB, ctx context.Context, query server.PgCanQuery, paymentPlanId server.Id) (count int) {
	result, err := query.Query(
		ctx,
		`SELECT COUNT(*) FROM account_payment WHERE payment_plan_id = $1`,
		paymentPlanId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return
}

// Two cohorts one slice apart: the loop commits two slices, and the write runs
// once in each, seeing that slice's payments inside the transaction while no
// other connection does yet. A dry run of the same backlog never calls it.
func TestPlanPaymentsMaxDurationLoopWritesInEachSliceTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		netTransferByteCount := ByteCount(1024 * 1024 * 1024 * 1024)
		netRevenue := UsdToNanoCents(100.00)
		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		destinationNetworkId := server.NewId()
		destinationId := server.NewId()
		testingCreatePaymentClient(ctx, sourceNetworkId, sourceId)
		testingCreatePaymentClient(ctx, destinationNetworkId, destinationId)
		sourceSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: sourceNetworkId,
			ClientId:  &sourceId,
		})
		balanceCode, err := CreateBalanceCode(ctx, netTransferByteCount, 365*24*time.Hour, netRevenue, "", "", "")
		connect.AssertEqual(t, err, nil)
		testingRedeemPaymentBalanceCode(t, sourceSession.Ctx, sourceNetworkId, balanceCode.Secret)

		historical := historicalPaymentCohort{
			sourceNetworkId:      sourceNetworkId,
			sourceId:             sourceId,
			destinationNetworkId: destinationNetworkId,
			destinationId:        destinationId,
			usedByteCount:        50 * 1024 * 1024 * 1024,
			payoutRevenue:        NanoCents(float64(netRevenue) * 50 / 1024),
		}
		now := server.NowUtc()
		maxDuration := 20 * 24 * time.Hour
		subsidyEnd := now.Add(-60 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx,
				`INSERT INTO subsidy_payment (
					payment_plan_id, start_time, end_time,
					active_user_count, paid_user_count,
					net_payout_byte_count_paid, net_payout_byte_count_unpaid,
					net_revenue_nano_cents, net_payout_nano_cents
				) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)`,
				server.NewId(), subsidyEnd.Add(-5*24*time.Hour), subsidyEnd,
			))
		})
		historical.insert(t, ctx, now.Add(-58*24*time.Hour), now.Add(-50*24*time.Hour))
		historical.insert(t, ctx, now.Add(-45*24*time.Hour), now.Add(-38*24*time.Hour))
		testingHistoricalPaymentAttributionImmutable(t, ctx)

		dryRunWrites := 0
		_, err = createPaymentPlan(ctx, EnvSubsidyConfig(), true, maxDuration, false, func(server.PgTx, *PaymentPlan) {
			dryRunWrites += 1
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dryRunWrites, 0)

		writtenPlanIds := []server.Id{}
		plans, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, func(tx server.PgTx, plan *PaymentPlan) {
			writtenPlanIds = append(writtenPlanIds, plan.PaymentPlanId)
			if inTx := planPaymentCount(t, ctx, tx, plan.PaymentPlanId); inTx != len(plan.NetworkPayments) {
				t.Errorf("the write saw %d payments of its slice, want %d", inTx, len(plan.NetworkPayments))
			}
			server.Db(ctx, func(conn server.PgConn) {
				if outside := planPaymentCount(t, ctx, conn, plan.PaymentPlanId); outside != 0 {
					t.Errorf("another connection saw %d payments of the open slice", outside)
				}
			})
		}, nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, 2 <= len(plans), true)
		planIds := []server.Id{}
		paidPlanCount := 0
		for _, plan := range plans {
			planIds = append(planIds, plan.PaymentPlanId)
			if 0 < len(plan.NetworkPayments) {
				paidPlanCount += 1
			}
		}
		connect.AssertEqual(t, writtenPlanIds, planIds)
		connect.AssertEqual(t, 2 <= paidPlanCount, true)
	})
}
