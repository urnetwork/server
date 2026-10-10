// Staged sweep assignment: committed batches survive a drain, a pending plan's
// payments and reserved sweeps stay untouchable, and sweeps that retention
// could delete are assigned inside the plan transaction.
package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// One bounded slice of count unpaid sweeps after an anchoring subsidy epoch.
func paymentPlanAssignmentFixture(t testing.TB, ctx context.Context, count int) (cohort *paymentPlanInterruptionCohort, anchorEnd time.Time, maxDuration time.Duration) {
	t.Helper()
	cohort = newPaymentPlanInterruptionCohort(t, ctx)
	now := server.NowUtc()
	maxDuration = 20 * 24 * time.Hour
	anchorEnd = now.Add(-60 * 24 * time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO subsidy_payment (
				payment_plan_id, start_time, end_time,
				active_user_count, paid_user_count,
				net_payout_byte_count_paid, net_payout_byte_count_unpaid,
				net_revenue_nano_cents, net_payout_nano_cents
			) VALUES ($1, $2, $3, 0, 0, 0, 0, 0, 0)`,
			server.NewId(), anchorEnd.Add(-5*24*time.Hour), anchorEnd,
		))
	})
	cohort.insert(t, ctx, count, now.Add(-58*24*time.Hour), now.Add(-50*24*time.Hour))
	return
}

// Runs batches of batchSize and drains the run right after drainAfter
// committed batches. Restores the production batch size and observer.
func runPaymentPlanUntilDrain(t testing.TB, ctx context.Context, maxDuration time.Duration, batchSize int, drainAfter int) error {
	t.Helper()
	previousBatchSize := paymentPlanAssignmentBatchSize
	paymentPlanAssignmentBatchSize = batchSize
	defer func() {
		paymentPlanAssignmentBatchSize = previousBatchSize
		paymentPlanAssignmentBatchCommitted = nil
	}()
	drainCtx, drain := context.WithCancel(ctx)
	defer drain()
	committedBatchCount := 0
	paymentPlanAssignmentBatchCommitted = func(server.Id, int64) {
		committedBatchCount += 1
		if committedBatchCount == drainAfter {
			drain()
		}
	}
	_, err := PlanPaymentsWithMaxDurationLoopInTx(drainCtx, maxDuration, nil, nil)
	if committedBatchCount != drainAfter {
		t.Fatalf("run committed %d batches before its drain, want %d", committedBatchCount, drainAfter)
	}
	return err
}

type paymentPlanAssignmentRow struct {
	paymentPlanId      server.Id
	sweepCount         int64
	assignedSweepCount int64
	stagedCount        int64
	cursorSet          bool
	complete           bool
}

func getPaymentPlanAssignmentRow(t testing.TB, ctx context.Context, paymentId server.Id) (row paymentPlanAssignmentRow) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT a.payment_plan_id, a.sweep_count, a.assigned_sweep_count,
			(SELECT COUNT(*) FROM payment_plan_sweep s WHERE s.payment_plan_id=a.payment_plan_id),
			a.cursor_contract_id IS NOT NULL, a.complete_time IS NOT NULL
			FROM payment_plan_assignment a JOIN account_payment p ON p.payment_plan_id=a.payment_plan_id
			WHERE p.payment_id=$1`, paymentId).Scan(&row.paymentPlanId, &row.sweepCount, &row.assignedSweepCount,
			&row.stagedCount, &row.cursorSet, &row.complete))
	})
	return
}

func TestPaymentPlanAssignmentResumesFromCommittedBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		const sweepCount = 7
		cohort, anchorEnd, maxDuration := paymentPlanAssignmentFixture(t, ctx, sweepCount)

		// drain right after the second of four batches commits
		err := runPaymentPlanUntilDrain(t, ctx, maxDuration, 2, 2)
		var progressErr *PaymentPlanProgressError
		if !errors.As(err, &progressErr) || !errors.Is(err, context.Canceled) {
			t.Fatal("drained run did not report its committed progress", err)
		}
		state := cohort.state(t, ctx, anchorEnd)
		if len(state.paymentIds) != 1 || state.epochCount != 1 || state.assignedCount != 4 || state.unassignedCount != 3 {
			t.Fatalf("drain lost committed batches: payments=%d epochs=%d assigned=%d unassigned=%d",
				len(state.paymentIds), state.epochCount, state.assignedCount, state.unassignedCount)
		}
		row := getPaymentPlanAssignmentRow(t, ctx, state.paymentIds[0])
		if row.sweepCount != sweepCount || row.assignedSweepCount != 4 || row.stagedCount != 3 || !row.cursorSet || row.complete {
			t.Fatalf("drain lost the assignment cursor: %+v", row)
		}
		pending, err := PaymentPlanAssignmentPending(ctx, row.paymentPlanId)
		if err != nil || !pending {
			t.Fatal("partially assigned plan is payable", pending, err)
		}
		// the weekly points rollup waits for the payment's remaining sweeps
		if _, err := AdvancePointsBlockRollup(ctx); err != nil {
			t.Fatal(err)
		}
		if paymentPlanBlockRollupComplete(t, ctx, state.paymentIds[0]) {
			t.Fatal("points rollup completed a payment that owns only part of its sweeps")
		}

		// the next run resumes at the cursor and plans nothing new for the payee
		plans, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil)
		if err != nil {
			t.Fatal("resumed run failed", err)
		}
		for _, plan := range plans {
			if _, ok := plan.NetworkPayments[cohort.destinationNetworkId]; ok {
				t.Fatal("resumed run planned committed sweeps again", plan.PaymentPlanId)
			}
		}
		resumed := cohort.state(t, ctx, anchorEnd)
		if len(resumed.paymentIds) != 1 || resumed.paymentIds[0] != state.paymentIds[0] || resumed.assignedCount != sweepCount ||
			resumed.unassignedCount != 0 || resumed.payouts[0] != state.payouts[0] || resumed.pointCount != state.pointCount {
			t.Fatalf("resumed run did not finish the committed plan exactly once: %+v", resumed)
		}
		row = getPaymentPlanAssignmentRow(t, ctx, state.paymentIds[0])
		if row.assignedSweepCount != sweepCount || row.stagedCount != 0 || !row.complete {
			t.Fatalf("resumed assignment did not complete: %+v", row)
		}
		pending, err = PaymentPlanAssignmentPending(ctx, row.paymentPlanId)
		if err != nil || pending {
			t.Fatal("completed plan is not payable", pending, err)
		}
		if _, err := AdvancePointsBlockRollup(ctx); err != nil {
			t.Fatal(err)
		}
		if !paymentPlanBlockRollupComplete(t, ctx, state.paymentIds[0]) {
			t.Fatal("points rollup skipped a payment that owns all of its sweeps")
		}
	})
}

func paymentPlanBlockRollupComplete(t testing.TB, ctx context.Context, paymentId server.Id) (complete bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT block_rollup_complete FROM account_payment WHERE payment_id=$1`, paymentId).Scan(&complete))
	})
	return
}

func TestPaymentPlanPendingAssignmentHoldsPaymentsAndSweeps(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		const sweepCount = 5
		cohort, anchorEnd, maxDuration := paymentPlanAssignmentFixture(t, ctx, sweepCount)
		destinationSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: cohort.destinationNetworkId,
			ClientId:  &cohort.destinationId,
		})
		walletId := CreateAccountWalletExternal(destinationSession, &CreateAccountWalletExternalArgs{
			NetworkId:        cohort.destinationNetworkId,
			Blockchain:       "MATIC",
			WalletAddress:    "0x00000000000000000000000000000000000000d1",
			DefaultTokenType: "USDC",
		})
		connect.AssertNotEqual(t, walletId, nil)
		connect.AssertEqual(t, SetPayoutWallet(ctx, cohort.destinationNetworkId, *walletId), nil)
		unpaidBefore := GetTransferStats(ctx, cohort.destinationNetworkId).UnpaidBytesProvided

		err := runPaymentPlanUntilDrain(t, ctx, maxDuration, 2, 1)
		if !errors.Is(err, context.Canceled) {
			t.Fatal("run was not drained after its first batch", err)
		}
		state := cohort.state(t, ctx, anchorEnd)
		if len(state.paymentIds) != 1 || state.assignedCount != 2 || state.unassignedCount != 3 {
			t.Fatalf("fixture did not stop between batches: %+v", state)
		}
		paymentId := state.paymentIds[0]

		// unpaid bytes are counted once while the plan owns only part of its sweeps
		if unpaidDuring := GetTransferStats(ctx, cohort.destinationNetworkId).UnpaidBytesProvided; unpaidDuring != unpaidBefore {
			t.Fatalf("pending assignment changed unpaid bytes: before=%d during=%d", unpaidBefore, unpaidDuring)
		}

		// no selection runs over reserved sweeps, including a preview
		if _, err := PlanPaymentsDryRunWithMaxDuration(ctx, maxDuration); !errors.Is(err, ErrPaymentPlanAssignmentPending) {
			t.Fatal("preview selected sweeps of a pending plan", err)
		}

		// another writer cannot move a reserved sweep, as an older planner would
		var stagedContractId, stagedBalanceId, stagedNetworkId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT contract_id, balance_id, network_id FROM payment_plan_sweep WHERE payment_id=$1 LIMIT 1`,
				paymentId).Scan(&stagedContractId, &stagedBalanceId, &stagedNetworkId))
		})
		stolen := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id=$4
					WHERE contract_id=$1 AND balance_id=$2 AND network_id=$3`, stagedContractId, stagedBalanceId, stagedNetworkId, server.NewId()))
			}, server.OptNoRetry())
		})
		if stolen == nil || !strings.Contains(server.ErrorJsonNoStack(stolen), "reserved by a pending payment plan") {
			t.Fatal("a reserved sweep moved to another payment", stolen)
		}

		// the pending payment can be neither canceled nor submitted
		if err := CancelPayment(ctx, paymentId); err == nil {
			t.Fatal("pending plan payment was canceled")
		}
		CancelHungAccountPayments(ctx, server.NowUtc().Add(10*HungPaymentExpiration))
		payment, err := GetPayment(ctx, paymentId)
		if err != nil || payment == nil || payment.Canceled || payment.WalletId == nil {
			t.Fatal("pending plan payment changed", payment, err)
		}
		wallet := GetAccountWallet(ctx, *payment.WalletId)
		if _, err := ReserveProviderPaymentRequest(ctx, payment, wallet, 1.0, "MATIC"); !errors.Is(err, ErrProviderPaymentBasisChanged) {
			t.Fatal("pending plan payment reserved a processor attempt", err)
		}
		payment, err = GetPayment(ctx, paymentId)
		if err != nil || payment.CircleIdempotencyKey != nil {
			t.Fatal("refused reservation allocated a processor key", err)
		}

		// resume: the payment becomes payable with its last batch
		if _, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil); err != nil {
			t.Fatal("resumed run failed", err)
		}
		if unpaidAfter := GetTransferStats(ctx, cohort.destinationNetworkId).UnpaidBytesProvided; unpaidAfter != unpaidBefore {
			t.Fatalf("completed assignment changed unpaid bytes: before=%d after=%d", unpaidBefore, unpaidAfter)
		}
		payment, err = GetPayment(ctx, paymentId)
		if err != nil {
			t.Fatal(err)
		}
		request, err := ReserveProviderPaymentRequest(ctx, payment, wallet, 1.0, "MATIC")
		if err != nil || request == nil {
			t.Fatal("completed plan payment cannot reserve its processor attempt", err)
		}
	})
}

func TestPaymentPlanStagingAssignsRetentionRiskSweepsInPlanTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		const sweepCount = 3
		cohort, anchorEnd, maxDuration := paymentPlanAssignmentFixture(t, ctx, sweepCount)

		// retention already made one selected contract due for deletion
		var dueContractId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT contract_id FROM transfer_escrow_sweep WHERE network_id=$1 ORDER BY contract_id LIMIT 1`,
				cohort.destinationNetworkId).Scan(&dueContractId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time=$2 WHERE contract_id=$1`,
				dueContractId, server.NowUtc().Add(-time.Hour)))
		})

		// the plan transaction's one in-transaction assignment is allowed; the
		// first batch is interrupted
		installPaymentPlanAssignmentInterruption(t, ctx, 1)
		interrupted := server.HandleError(func() {
			_, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil)
			server.Raise(err)
		})
		if interrupted == nil || !strings.Contains(server.ErrorJsonNoStack(interrupted), "synthetic drain during sweep assignment") {
			t.Fatal("first batch was not interrupted", interrupted)
		}
		removePaymentPlanAssignmentInterruption(t, ctx)

		state := cohort.state(t, ctx, anchorEnd)
		if len(state.paymentIds) != 1 || state.assignedCount != 1 || state.unassignedCount != sweepCount-1 {
			t.Fatalf("plan transaction did not assign exactly the retention-risk sweep: %+v", state)
		}
		paymentId := state.paymentIds[0]
		var dueOwner *server.Id
		var dueStaged bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT payment_id FROM transfer_escrow_sweep WHERE contract_id=$1),
				EXISTS (SELECT 1 FROM payment_plan_sweep WHERE contract_id=$1)`, dueContractId).Scan(&dueOwner, &dueStaged))
		})
		if dueOwner == nil || *dueOwner != paymentId || dueStaged {
			t.Fatal("retention-risk sweep was left for a batch", dueOwner, dueStaged)
		}

		// retention now finds the due contract held by an active payment
		removeDueContractBatches(ctx, server.NowUtc(), server.NowUtc().Add(-StragglerContractExpiration), 100)
		var contractCount, sweepRowCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT COUNT(*) FROM transfer_contract WHERE contract_id=$1),
				(SELECT COUNT(*) FROM transfer_escrow_sweep WHERE network_id=$2)`, dueContractId, cohort.destinationNetworkId).
				Scan(&contractCount, &sweepRowCount))
		})
		if contractCount != 1 || sweepRowCount != sweepCount {
			t.Fatal("retention deleted a sweep of the pending plan", contractCount, sweepRowCount)
		}

		if _, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil); err != nil {
			t.Fatal("resumed run failed", err)
		}
		resumed := cohort.state(t, ctx, anchorEnd)
		if len(resumed.paymentIds) != 1 || resumed.assignedCount != sweepCount || resumed.unassignedCount != 0 {
			t.Fatalf("resumed run did not assign the staged sweeps: %+v", resumed)
		}
	})
}
