// A payout slice interrupted while assigning its selected sweeps keeps the
// planning it already committed, and the next payout run resumes that slice
// instead of planning it again.
//
// The interruption is a database error raised by a test trigger after a fixed
// number of sweep ownership updates, so it lands inside the assignment on any
// planner, whatever its statement or batch layout, with no clock or scheduler
// dependence. This file uses only planner entry points that predate durable
// planning, so it also runs against a planner that assigns in one transaction.
package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Unpaid sweeps of one payer and payee, one settled contract each, with
// original times inside one slice.
type paymentPlanInterruptionCohort struct {
	sourceNetworkId      server.Id
	sourceId             server.Id
	destinationNetworkId server.Id
	destinationId        server.Id
	balanceId            server.Id
}

func newPaymentPlanInterruptionCohort(t testing.TB, ctx context.Context) *paymentPlanInterruptionCohort {
	t.Helper()
	cohort := &paymentPlanInterruptionCohort{
		sourceNetworkId:      server.NewId(),
		sourceId:             server.NewId(),
		destinationNetworkId: server.NewId(),
		destinationId:        server.NewId(),
	}
	testingCreatePaymentClient(ctx, cohort.sourceNetworkId, cohort.sourceId)
	testingCreatePaymentClient(ctx, cohort.destinationNetworkId, cohort.destinationId)
	sourceSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: cohort.sourceNetworkId,
		ClientId:  &cohort.sourceId,
	})
	balanceCode, err := CreateBalanceCode(ctx, ByteCount(1024*1024*1024*1024), 365*24*time.Hour, UsdToNanoCents(100.00), "", "", "")
	connect.AssertEqual(t, err, nil)
	testingRedeemPaymentBalanceCode(t, sourceSession.Ctx, cohort.sourceNetworkId, balanceCode.Secret)
	balances := GetActiveTransferBalances(ctx, cohort.sourceNetworkId)
	if len(balances) != 1 || !balances[0].Paid {
		t.Fatal("interruption cohort lacks one paid balance")
	}
	cohort.balanceId = balances[0].BalanceId
	return cohort
}

// Pins a nonzero account points policy for a test that asserts a plan's
// points; the release gate's fixture subsidy.yml configures none. The
// returned function restores the process's configuration.
func pinPaymentPlanAccountPoints() (restore func()) {
	subsidyConfig := EnvSubsidyConfig()
	previous := *subsidyConfig
	subsidyConfig.AccountPointsPerPayout = 1000000
	return func() { *subsidyConfig = previous }
}

// Inserts count contracts and their unpaid sweeps. Each sweep pays enough to
// clear the wallet minimum on its own, so the payee is never withheld.
func (self *paymentPlanInterruptionCohort) insert(t testing.TB, ctx context.Context, count int, createTime time.Time, closeTime time.Time) {
	t.Helper()
	usedByteCount := ByteCount(50 * 1024 * 1024 * 1024)
	payoutRevenue := UsdToNanoCents(2 * max(EnvSubsidyConfig().MinWalletPayoutUsd, 0.01))
	usage := &contractUsageSnapshot{Version: 1, ByteCount: usedByteCount,
		Providers: []contractProviderUsage{{ClientId: self.destinationId, NetworkId: self.destinationNetworkId, ByteCount: usedByteCount}}}
	for i := 0; i < count; i += 1 {
		contractId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`,
				contractId, self.sourceNetworkId, self.sourceId, self.destinationNetworkId, self.destinationId,
				usedByteCount, createTime, closeTime, usage))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
				VALUES($1,$2,$3,$4,$5,$6,$7)`, contractId, self.balanceId,
				self.destinationNetworkId, self.destinationId, usedByteCount, payoutRevenue, closeTime))
		})
	}
}

// The payee's live payments and sweep ownership.
type paymentPlanInterruptionState struct {
	paymentIds      []server.Id
	payouts         []NanoCents
	byteCounts      []ByteCount
	subsidies       []NanoCents
	epochCount      int
	pointCount      int
	assignedCount   int
	unassignedCount int
}

func (self *paymentPlanInterruptionCohort) state(t testing.TB, ctx context.Context, anchorEnd time.Time) *paymentPlanInterruptionState {
	t.Helper()
	state := &paymentPlanInterruptionState{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT payment_id, payout_nano_cents, payout_byte_count, subsidy_payout_nano_cents
			FROM account_payment WHERE network_id=$1 AND NOT canceled ORDER BY payment_id`, self.destinationNetworkId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var payout NanoCents
				var byteCount ByteCount
				var subsidy NanoCents
				server.Raise(result.Scan(&paymentId, &payout, &byteCount, &subsidy))
				state.paymentIds = append(state.paymentIds, paymentId)
				state.payouts = append(state.payouts, payout)
				state.byteCounts = append(state.byteCounts, byteCount)
				state.subsidies = append(state.subsidies, subsidy)
			}
		})
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT COUNT(*) FROM subsidy_payment WHERE $2 < end_time),
			(SELECT COUNT(*) FROM account_point WHERE network_id=$1),
			(SELECT COUNT(*) FROM transfer_escrow_sweep s JOIN account_payment p ON p.payment_id=s.payment_id
				WHERE s.network_id=$1 AND NOT p.canceled),
			(SELECT COUNT(*) FROM transfer_escrow_sweep WHERE network_id=$1 AND payment_id IS NULL)`,
			self.destinationNetworkId, anchorEnd).Scan(&state.epochCount, &state.pointCount, &state.assignedCount, &state.unassignedCount))
	})
	return state
}

// Raises inside sweep ownership updates once `budget` rows have changed owner,
// standing in for a drain that cancels the assignment statement.
func installPaymentPlanAssignmentInterruption(t testing.TB, ctx context.Context, budget int) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_sweep_assignment_budget (remaining bigint NOT NULL)`))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO test_sweep_assignment_budget (remaining) VALUES ($1)`, budget))
		server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_sweep_assignment_interrupt() RETURNS trigger LANGUAGE plpgsql AS $interrupt$
			DECLARE remaining_count bigint;
			BEGIN
				IF NEW.payment_id IS DISTINCT FROM OLD.payment_id THEN
					UPDATE test_sweep_assignment_budget SET remaining = remaining - 1 RETURNING remaining INTO remaining_count;
					IF remaining_count < 0 THEN
						RAISE EXCEPTION 'synthetic drain during sweep assignment';
					END IF;
				END IF;
				RETURN NEW;
			END $interrupt$`))
		server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER test_sweep_assignment_interrupt BEFORE UPDATE OF payment_id ON transfer_escrow_sweep
			FOR EACH ROW EXECUTE FUNCTION test_sweep_assignment_interrupt()`))
	})
}

func removePaymentPlanAssignmentInterruption(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER test_sweep_assignment_interrupt ON transfer_escrow_sweep`))
	})
}

func TestPaymentPlanInterruptedSweepAssignmentKeepsCommittedSlice(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		// the committed slice's points are part of what must survive
		defer pinPaymentPlanAccountPoints()()
		ctx := context.Background()
		const sweepCount = 6
		cohort := newPaymentPlanInterruptionCohort(t, ctx)

		now := server.NowUtc()
		maxDuration := 20 * 24 * time.Hour
		anchorEnd := now.Add(-60 * 24 * time.Hour)
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
		// one slice: [anchorEnd, anchorEnd + maxDuration)
		cohort.insert(t, ctx, sweepCount, now.Add(-58*24*time.Hour), now.Add(-50*24*time.Hour))

		// the uninterrupted slice, previewed without persisting anything
		preview, err := PlanPaymentsDryRunWithMaxDuration(ctx, maxDuration)
		if err != nil || preview == nil || preview.SubsidyPayment == nil {
			t.Fatal("preview did not form the slice", preview, err)
		}
		expected, ok := preview.NetworkPayments[cohort.destinationNetworkId]
		if !ok || expected.PayoutByteCount != ByteCount(sweepCount*50*1024*1024*1024) {
			t.Fatal("preview lost the payee's sweeps", expected)
		}

		// interrupt after half of the slice's sweeps changed owner
		installPaymentPlanAssignmentInterruption(t, ctx, sweepCount/2)
		interrupted := server.HandleError(func() {
			_, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil)
			server.Raise(err)
		})
		if interrupted == nil || !strings.Contains(fmt.Sprint(interrupted), "synthetic drain during sweep assignment") {
			t.Fatal("the payout run was not interrupted during its sweep assignment", interrupted)
		}

		// the interruption must not lose the slice's committed planning
		state := cohort.state(t, ctx, anchorEnd)
		if len(state.paymentIds) != 1 || state.epochCount != 1 || state.pointCount == 0 {
			t.Fatalf("interrupted slice lost its committed plan: payments=%d epochs=%d points=%d",
				len(state.paymentIds), state.epochCount, state.pointCount)
		}
		if state.payouts[0] != expected.Payout || state.byteCounts[0] != expected.PayoutByteCount || state.subsidies[0] != expected.SubsidyPayout {
			t.Fatalf("committed slice differs from its preview: payout=%d/%d bytes=%d/%d subsidy=%d/%d",
				state.payouts[0], expected.Payout, state.byteCounts[0], expected.PayoutByteCount, state.subsidies[0], expected.SubsidyPayout)
		}
		paymentId := state.paymentIds[0]
		var stagedCount int
		var pending bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT COUNT(*) FROM payment_plan_sweep WHERE payment_id=$1),
				EXISTS (SELECT 1 FROM payment_plan_assignment a JOIN account_payment p ON p.payment_plan_id=a.payment_plan_id
					WHERE p.payment_id=$1 AND a.complete_time IS NULL)`, paymentId).Scan(&stagedCount, &pending))
		})
		if state.assignedCount+stagedCount != sweepCount || !pending {
			t.Fatalf("interrupted slice lost its sweep reservation: assigned=%d staged=%d pending=%t", state.assignedCount, stagedCount, pending)
		}

		removePaymentPlanAssignmentInterruption(t, ctx)

		// the next payout run resumes the committed slice instead of planning it again
		plans, err := PlanPaymentsWithMaxDurationLoopInTx(ctx, maxDuration, nil, nil)
		if err != nil {
			t.Fatal("resumed payout run failed", err)
		}
		for _, plan := range plans {
			if _, ok := plan.NetworkPayments[cohort.destinationNetworkId]; ok {
				t.Fatal("the resumed run planned the committed slice's sweeps again", plan.PaymentPlanId)
			}
		}
		resumed := cohort.state(t, ctx, anchorEnd)
		if len(resumed.paymentIds) != 1 || resumed.paymentIds[0] != paymentId || resumed.epochCount != 1 {
			t.Fatalf("resumed run changed the committed slice: payments=%v epochs=%d", resumed.paymentIds, resumed.epochCount)
		}
		if resumed.assignedCount != sweepCount || resumed.unassignedCount != 0 || resumed.payouts[0] != expected.Payout || resumed.pointCount != state.pointCount {
			t.Fatalf("resumed slice did not assign each sweep exactly once: assigned=%d unassigned=%d payout=%d/%d points=%d/%d",
				resumed.assignedCount, resumed.unassignedCount, resumed.payouts[0], expected.Payout, resumed.pointCount, state.pointCount)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_plan_assignment a JOIN account_payment p
				ON p.payment_plan_id=a.payment_plan_id WHERE p.payment_id=$1 AND a.complete_time IS NULL)`, paymentId).Scan(&pending))
		})
		if pending {
			t.Fatal("resumed slice left its payment unpayable")
		}
	})
}
