package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestProviderBoundaryDriftCannotReassignPlannedOrUnplannedUsage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		usePayoutTransition(t)
		original := server.Config.RequireSimpleResource("sn.yml").Bytes()
		f := newPayoutTransitionCohort(t, ctx)
		before, after := payoutTestCutoff.Add(-time.Microsecond), payoutTestCutoff.Add(time.Microsecond)
		f.insert(t, ctx, before.Add(-time.Hour), &before, after.Add(time.Hour), 11, UsdToNanoCents(1))
		future := f.insert(t, ctx, before.Add(-time.Hour), &after, after.Add(time.Hour), 22, UsdToNanoCents(2))
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || plan == nil {
			t.Fatal(err)
		}
		payment := plan.NetworkPayments[f.network]
		if payment == nil || payment.PayoutByteCount != 11 {
			t.Fatal("missing original legacy allocation")
		}
		for _, changedCutoff := range []time.Time{payoutTestCutoff.Add(time.Hour), payoutTestCutoff.Add(-time.Hour)} {
			changed := bytes.Replace(original, []byte(payoutTestCutoff.Format(time.RFC3339)), []byte(changedCutoff.Format(time.RFC3339)), 1)
			release := server.Config.PushSimpleResource("sn.yml", changed)
			_, planningErr := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
			submitErr := RequireProviderUsdcPayment(ctx, payment.PaymentId)
			_, usageErr := GetStEpochProviderUsageAtEpoch(ctx, 7, before.Add(-time.Hour), after.Add(time.Hour))
			release()
			for _, err := range []error{planningErr, submitErr, usageErr} {
				if !errors.Is(err, server.ErrProviderEarningBoundaryMismatch) {
					t.Fatal("hot edit reinterpreted retained economic usage", changedCutoff, err)
				}
			}
		}
		got, err := GetPayment(ctx, payment.PaymentId)
		if err != nil || got == nil || got.Canceled || got.Completed || got.CircleIdempotencyKey != nil || got.Payout != payment.Payout {
			t.Fatal("drift refusal discarded or changed original debt", got, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$1`, future).Scan(&untouched))
			if !untouched {
				t.Fatal("post-cutoff usage acquired legacy payment")
			}
		})
		if err := RequireProviderUsdcPayment(ctx, payment.PaymentId); err != nil {
			t.Fatal("restoring original policy did not resume exact debt", err)
		}
		second, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || second == nil || len(second.NetworkPayments) != 0 {
			t.Fatal("restored policy duplicated original allocation", second, err)
		}
		usage, err := GetStEpochProviderUsageAtEpoch(ctx, 7, before.Add(-time.Hour), after.Add(time.Hour))
		if err != nil || len(usage) != 1 || usage[0] == nil || usage[0].PayoutByteCount != 22 {
			t.Fatal("restored policy lost retained subnet usage", usage, err)
		}
	})
}

func TestProviderBoundaryUnpreparedPlannerRetainsSource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		data := fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %q\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", payoutTestCutoff.Format(time.RFC3339), "0x"+strings.Repeat("11", 32))
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", []byte(data)))
		f := newPayoutTransitionCohort(t, ctx)
		closed := payoutTestCutoff.Add(-time.Microsecond)
		contract := f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed.Add(time.Hour), 11, UsdToNanoCents(1))
		if _, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0); !errors.Is(err, server.ErrProviderEarningBoundaryUnprepared) {
			t.Fatal("public planner enrolled missing authority", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var untouched bool
			server.Raise(conn.QueryRow(ctx, `SELECT payment_id IS NULL FROM transfer_escrow_sweep WHERE contract_id=$1`, contract).Scan(&untouched))
			if !untouched {
				t.Fatal("unprepared planner consumed liability")
			}
		})
		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256); err != nil {
			t.Fatal(err)
		}
		plan, err := CreatePaymentPlan(ctx, payoutTransitionRevenueConfig(), false, 0)
		if err != nil || plan == nil || plan.NetworkPayments[f.network] == nil || plan.NetworkPayments[f.network].PayoutByteCount != 11 {
			t.Fatal("explicit preparation did not admit retained original earning", plan, err)
		}
	})
}
