package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func payoutTransitionSubsidyPlan(t testing.TB, ctx context.Context) (*payoutTransitionCohort, *AccountPayment, *SubsidyConfig) {
	t.Helper()
	f := newPayoutTransitionCohort(t, ctx)
	closed := payoutTestCutoff.Add(-time.Microsecond)
	f.insert(t, ctx, closed.Add(-90*time.Second), &closed, closed.Add(time.Hour), 1024, 0)
	config := payoutTransitionRevenueConfig()
	config.MinPayoutUsd = 100
	config.MinWalletPayoutUsd = 1000
	plan, err := CreatePaymentPlan(ctx, config, false, 0)
	if err != nil || plan == nil || plan.NetworkPayments[f.network] == nil || plan.NetworkPayments[f.network].SubsidyPayout <= 0 {
		t.Fatalf("actual subsidy-only plan missing: %+v %v", plan, err)
	}
	return f, plan.NetworkPayments[f.network], config
}

func TestProviderTransitionLegacyComponentsRetainOriginalAllocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f, payment, config := payoutTransitionSubsidyPlan(t, ctx)
		if err := CancelPayment(ctx, payment.PaymentId); err == nil {
			t.Fatal("allocated subsidy was discarded for raw replan")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET create_time=$2 WHERE payment_id=$1`, payment.PaymentId, server.NowUtc().Add(-2*HungPaymentExpiration)))
		})
		if count := CancelHungAccountPayments(ctx, server.NowUtc()); count != 0 {
			t.Fatal("age canceled allocated component", count)
		}
		// Reproduce an old safely canceled row without rewriting its financial
		// values. The new planner must reopen exactly this allocation once.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET canceled=true,cancel_time=now() WHERE payment_id=$1`, payment.PaymentId))
		})
		var priorPoints, priorWindows int
		var priorPointValue int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM account_point),(SELECT COUNT(*) FROM subsidy_payment),(SELECT COALESCE(SUM(point_value),0)::bigint FROM account_point)`).Scan(&priorPoints, &priorWindows, &priorPointValue))
		})
		for attempt := 0; attempt < 2; attempt++ {
			plan, err := CreatePaymentPlan(ctx, config, false, 0)
			if err != nil || len(plan.NetworkPayments) != 0 || plan.SubsidyPayment != nil {
				t.Fatalf("restoration created a new financial allocation: %+v %v", plan, err)
			}
			want := 1
			if attempt == 1 {
				want = 0
			}
			if plan.RestoredLegacyPaymentCount != want || plan.UnresolvedCensusComplete {
				t.Fatalf("wrong bounded recovery report: %+v", plan)
			}
		}
		got, err := GetPayment(ctx, payment.PaymentId)
		if err != nil || got.Canceled || got.Completed || got.PaymentPlanId != payment.PaymentPlanId || got.Payout != payment.Payout || got.SubsidyPayout != payment.SubsidyPayout || got.PayoutByteCount != 1024 {
			t.Fatalf("original allocation changed: %+v %v", got, err)
		}
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal("restored legacy authority refused", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var points, windows, owners int
			var pointValue int64
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM account_point),(SELECT COUNT(*) FROM subsidy_payment),(SELECT COUNT(*) FROM account_payment WHERE network_id=$1),(SELECT COALESCE(SUM(point_value),0)::bigint FROM account_point)`, f.network).Scan(&points, &windows, &owners, &pointValue))
			if points != priorPoints || windows != priorWindows || owners != 1 || pointValue != priorPointValue {
				t.Fatal("recovery repeated points/window/payment", points, windows, owners)
			}
		})
	})
}

func TestProviderTransitionLegacyRecoveryBoundedCensusDoesNotStarve(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f, payment, config := payoutTransitionSubsidyPlan(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET canceled=true WHERE payment_id=$1`, payment.PaymentId))
			// These earlier synthetic rows lack original sweep authority. A
			// bounded report must not pretend they were paid or repeatedly block
			// the next valid original beyond its first batch.
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,subsidy_payout_nano_cents,min_sweep_time,canceled)
			SELECT ('00000000-0000-0000-0000-'||lpad(i::text,12,'0'))::uuid,$1,$2,0,1,1,$3,true FROM generate_series(1,$4) i`, payment.PaymentPlanId, f.network, payoutTestCutoff.Add(-time.Microsecond), legacyPaymentRecoveryLimit))
		})
		first, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || first.UnresolvedLegacyPaymentCount != legacyPaymentRecoveryLimit || first.RestoredLegacyPaymentCount != 0 || len(first.NetworkPayments) != 0 || first.UnresolvedCensusComplete {
			t.Fatalf("first bounded recovery invented completeness: %+v %v", first, err)
		}
		second, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || second.RestoredLegacyPaymentCount != 1 || second.UnresolvedLegacyPaymentCount != 0 || len(second.NetworkPayments) != 0 {
			t.Fatalf("reviewed rows starved later exact debt: %+v %v", second, err)
		}
		rows, err := GetNetworkPayments(f.session)
		if err != nil || len(rows) != legacyPaymentRecoveryLimit+1 {
			t.Fatalf("public history hid retained ambiguous components: %d %v", len(rows), err)
		}
	})
}

func TestProviderTransitionLegacyRecoveryRefusesPartialOriginalCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		_, payment, config := payoutTransitionSubsidyPlan(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET canceled=true WHERE payment_id=$1`, payment.PaymentId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payout_byte_count=payout_byte_count-1 WHERE payment_id=$1`, payment.PaymentId))
		})
		plan, err := CreatePaymentPlan(ctx, config, false, 0)
		if err != nil || plan.UnresolvedLegacyPaymentCount != 1 || plan.RestoredLegacyPaymentCount != 0 || len(plan.NetworkPayments) != 0 {
			t.Fatalf("partial original was reconstructed: %+v %v", plan, err)
		}
		got, err := GetPayment(ctx, payment.PaymentId)
		if err != nil || !got.Canceled || !got.AttributionReviewRequired || got.Payout != payment.Payout || got.PayoutByteCount != 1024 {
			t.Fatalf("unresolved original changed: %+v %v", got, err)
		}
	})
}
