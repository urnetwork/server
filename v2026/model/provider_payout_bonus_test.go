package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func payoutTransitionBonusPlan(t testing.TB, ctx context.Context) (*payoutTransitionCohort, *AccountPayment) {
	t.Helper()
	f := newPayoutTransitionCohort(t, ctx)
	closed := payoutTestCutoff.Add(-time.Microsecond)
	f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed.Add(time.Hour), 100, UsdToNanoCents(1))
	plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
	if err != nil || plan == nil || plan.NetworkPayments[f.network] == nil {
		t.Fatalf("bonus base plan missing: %v", err)
	}
	return f, plan.NetworkPayments[f.network]
}

func TestProviderTransitionBonusAuthorityAndSafeReplan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		f, payment := payoutTransitionBonusPlan(t, ctx)
		operation := server.NewId()
		for attempt := 0; attempt < 2; attempt++ {
			if err := PayoutPlanApplyBonus(ctx, payment.PaymentPlanId, UsdToNanoCents(.25), operation, "reviewed legacy correction"); err != nil {
				t.Fatal(err)
			}
		}
		if err := PayoutPlanApplyBonus(ctx, payment.PaymentPlanId, UsdToNanoCents(.25), operation, "different authority"); err == nil {
			t.Fatal("operation id accepted different authority")
		}
		updated, err := GetPayment(ctx, payment.PaymentId)
		if err != nil || updated.Payout != UsdToNanoCents(1.25) || updated.BonusPayout != UsdToNanoCents(.25) {
			t.Fatalf("bonus retry changed liability: %+v %v", updated, err)
		}
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal("explicit legacy bonus refused", err)
		}
		if err := CancelPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal(err)
		}
		replacement, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		next := replacement.NetworkPayments[f.network]
		if next == nil || next.PaymentId == payment.PaymentId || next.Payout != UsdToNanoCents(1.25) || next.BonusPayout != UsdToNanoCents(.25) {
			t.Fatalf("safe replan lost/duplicated correction: %+v", next)
		}
		if err := RequireProviderUsdcPayment(ctx, next.PaymentId); err != nil {
			t.Fatal("replanned exact legacy correction refused", err)
		}
		if err := PayoutPlanApplyBonus(ctx, payment.PaymentPlanId, UsdToNanoCents(.25), operation, "reviewed legacy correction"); err != nil {
			t.Fatal("lost-ack retry changed after safe replan", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			var owner server.Id
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*),MIN(payment_id::text)::uuid FROM account_payment_bonus WHERE operation_id=$1`, operation).Scan(&count, &owner))
			if count != 1 || owner != next.PaymentId {
				t.Fatal("correction has multiple or stale current owners", count, owner)
			}
		})
		empty, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || len(empty.NetworkPayments) != 0 {
			t.Fatal("settled planning repeated corrected obligation", err)
		}
	})
}

func TestProviderTransitionBonusRefusesRetainedSubmissionAndOldWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		for _, state := range []string{"key", "record", "hash", "completed", "canceled"} {
			_, payment := payoutTransitionBonusPlan(t, ctx)
			server.Tx(ctx, func(tx server.PgTx) {
				assignments := map[string]string{"key": "circle_idempotency_key=$2", "record": "payment_record='accepted'", "hash": "tx_hash='0xknown'", "completed": "completed=true", "canceled": "canceled=true"}
				args := []any{payment.PaymentId}
				if state == "key" {
					args = append(args, server.NewId())
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET `+assignments[state]+` WHERE payment_id=$1`, args...))
			})
			if err := PayoutPlanApplyBonus(ctx, payment.PaymentPlanId, UsdToNanoCents(1), server.NewId(), "must refuse"); err == nil {
				t.Fatal("bonus changed retained state", state)
			}
			server.Db(ctx, func(conn server.PgConn) {
				_, err := conn.Exec(ctx, `UPDATE account_payment SET payout_nano_cents=payout_nano_cents+1 WHERE payment_id=$1`, payment.PaymentId)
				if err == nil {
					t.Fatal("old mixed writer changed submitted basis", state)
				}
			})
			stored, err := GetPayment(ctx, payment.PaymentId)
			if err != nil || stored.Payout != payment.Payout || stored.BonusPayout != 0 {
				t.Fatal("refused bonus mutated amount", state, err)
			}
		}
		_, payment := payoutTransitionBonusPlan(t, ctx)
		server.Db(ctx, func(conn server.PgConn) {
			_, err := conn.Exec(ctx, `UPDATE account_payment SET payout_nano_cents=payout_nano_cents+1 WHERE payment_id=$1`, payment.PaymentId)
			if err == nil {
				t.Fatal("unsubmitted old writer minted unbound correction")
			}
		})
	})
}

func TestProviderTransitionBonusPlanIsAtomicAndWindowBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		first, second := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		for _, f := range []*payoutTransitionCohort{first, second} {
			f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed.Add(time.Hour), 100, UsdToNanoCents(1))
		}
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := GetOrCreatePaymentIdempotencyKey(ctx, plan.NetworkPayments[second.network].PaymentId); err != nil {
			t.Fatal(err)
		}
		if err := PayoutPlanApplyBonus(ctx, plan.PaymentPlanId, UsdToNanoCents(.5), server.NewId(), "whole reviewed plan"); err == nil {
			t.Fatal("partly frozen plan accepted correction")
		}
		unchanged, err := GetPayment(ctx, plan.NetworkPayments[first.network].PaymentId)
		if err != nil || unchanged.Payout != UsdToNanoCents(1) || unchanged.BonusPayout != 0 {
			t.Fatal("whole-plan refusal partially changed healthy peer", err)
		}
		// A new payout row cannot turn an at-cutoff contract into bonus authority.
		f := newPayoutTransitionCohort(t, ctx)
		at := payoutTestCutoff
		contract := f.insert(t, ctx, at.Add(-time.Hour), &at, at.Add(time.Hour), 100, UsdToNanoCents(1))
		paymentId, planId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,payout_byte_count,payout_nano_cents,min_sweep_time) VALUES($1,$2,$3,100,$4,$5)`, paymentId, planId, f.network, UsdToNanoCents(1), at))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id=$2 WHERE contract_id=$1`, contract, paymentId))
		})
		if err := PayoutPlanApplyBonus(ctx, planId, UsdToNanoCents(.5), server.NewId(), "invalid new-era correction"); err == nil {
			t.Fatal("post-cutoff usage became legacy bonus")
		}
	})
}
