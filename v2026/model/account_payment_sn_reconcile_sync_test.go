// The stuck-row census excludes correction pairs while revenue moves and after
// the replacement earns release through the ordinary payment admission.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// RC-22: the legacy correction cancels an inflated legacy attempt with its
// key kept and moves its earnings to a replacement, which the sender pays. The
// canceled original would read as RC-1; neither it nor its replacement is a
// stuck row, even while a partial positive-revenue move must remain held.
func TestSnReconcileStuckRowsExcludeReplacedLegacyOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		usePayoutTransition(t)
		ctx := context.Background()
		cohort := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, cohort.network)
		created := payoutTestCutoff.Add(-60 * 24 * time.Hour)
		closed := created.Add(-24 * time.Hour)
		reliability := UsdToNanoCents(2)
		// A reliability subsidy for a plan window of a microsecond: the
		// correction pays it pro rata over its period, so it shrinks.
		contractIds := []server.Id{
			cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)),
			cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)),
		}
		original := snReconcileTestPaymentPlus(t, ctx, reliability, cohort.network, wallet, created, contractIds...)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET reliability_subsidy_nano_cents=$2 WHERE payment_id=$1`, original, reliability)
		snReconcileTestEpoch(t, ctx, original, closed.Add(-time.Hour))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, original, server.NewId())
		if rows := snReconcileTestStuckById(t, ctx); rows[original] == nil || rows[original].Kind != SnReconcileUnretainedKey {
			t.Fatal("the legacy attempt is not an RC-2 row", rows[original])
		}

		attempts, err := GetLegacyProviderPaymentAttempts(ctx)
		if err != nil || len(attempts) != 1 || attempts[0].Payment.PaymentId != original {
			t.Fatal("legacy attempts", attempts, err)
		}
		if err := RequireProviderUsdcPayment(ctx, original); err != nil {
			t.Fatal("historical original lacks actual earning authority", err)
		}
		windows, err := GetLegacyProviderPaymentSubsidyWindows(ctx, []server.Id{original})
		if err != nil {
			t.Fatal(err)
		}
		policy := DefaultLegacyProviderPaymentCorrectionPolicy()
		correction := CorrectLegacyProviderPayment(attempts[0].Payment, windows[original], policy)
		replacement, err := ReplaceLegacyProviderPayment(ctx, correction, policy, `{}`)
		if err != nil || !correction.Changed() {
			t.Fatal("the legacy correction did not replace the attempt", correction, err)
		}
		var canceled, keyed bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT canceled, circle_idempotency_key IS NOT NULL FROM account_payment WHERE payment_id=$1`, original).Scan(&canceled, &keyed))
		})
		if !canceled || !keyed {
			t.Fatal("the replaced original is not canceled with its key", canceled, keyed)
		}
		assertState := func(stage string, remainingSweepCount int64, reviewHeld bool) {
			t.Helper()
			rows := snReconcileTestStuckById(t, ctx)
			if rows[original] != nil || rows[replacement] != nil {
				t.Fatal("a replaced original or its replacement is stuck", stage)
			}
			payment, err := GetPayment(ctx, replacement)
			if err != nil || payment.Payout != correction.Payout || payment.PayoutByteCount != 20 ||
				payment.SubsidyPayout != correction.SubsidyPayout || payment.ReliabilitySubsidy != correction.ReliabilitySubsidy ||
				payment.AttributionReviewRequired != reviewHeld || payment.CircleIdempotencyKey != nil || payment.PaymentRecord != nil || payment.TxHash != nil {
				t.Fatal("replacement amount or submission authority changed", stage, payment, err)
			}
			canceled, err := GetPayment(ctx, original)
			if err != nil || !canceled.Canceled || canceled.Payout != attempts[0].Payment.Payout ||
				canceled.CircleIdempotencyKey == nil || *canceled.CircleIdempotencyKey != *attempts[0].Payment.CircleIdempotencyKey {
				t.Fatal("correction changed the historical obligation", stage, canceled, err)
			}
			var originalSweepCount, replacementSweepCount, preservedSweepCount int64
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE payment_id=$1), COUNT(*) FILTER (WHERE payment_id=$2),
					COUNT(*) FILTER (WHERE contract_id=ANY($3) AND balance_id=$4 AND network_id=$5 AND destination_id=$6
						AND payout_byte_count=10 AND payout_net_revenue_nano_cents=$7)
					FROM transfer_escrow_sweep WHERE payment_id IN ($1,$2)`,
					original, replacement, contractIds, cohort.balance, cohort.network, cohort.client, UsdToNanoCents(1),
				).Scan(&originalSweepCount, &replacementSweepCount, &preservedSweepCount))
			})
			if originalSweepCount != remainingSweepCount || replacementSweepCount != 2-remainingSweepCount || preservedSweepCount != 2 {
				t.Fatal("correction lost exact sweep identity, bytes or revenue", stage, originalSweepCount, replacementSweepCount, preservedSweepCount)
			}
		}
		assertState("replaced", 2, true)

		replacements, err := GetLegacyProviderPaymentReplacements(ctx)
		if err != nil || len(replacements) != 1 || replacements[0].OriginalPaymentId != original || replacements[0].ReplacementPaymentId != replacement {
			t.Fatal("replacements", replacements, err)
		}
		anchored, err := MoveLegacyProviderPaymentReplacementAnchor(ctx, replacements[0], nil)
		if err != nil || !anchored {
			t.Fatal("the replacement did not take its anchor sweep", anchored, err)
		}
		assertState("partially anchored", 1, true)
		originalBefore := snReconcileTestFingerprint(t, ctx, original)
		replacementBefore := snReconcileTestFingerprint(t, ctx, replacement)
		if released, err := ReleaseLegacyProviderPaymentReplacement(ctx, replacements[0], policy, anchored); released || !errors.Is(err, ErrProviderUsdcAttributionUnresolved) {
			t.Fatal("one positive-revenue anchor released the entire debt", released, err)
		}
		if snReconcileTestFingerprint(t, ctx, original) != originalBefore || snReconcileTestFingerprint(t, ctx, replacement) != replacementBefore {
			t.Fatal("refused partial release changed payment rows or audit counts")
		}
		assertState("partial release refused", 1, true)

		// Complete the actual bounded move; an expected refusal alone must not
		// replace the census's successful-release coverage.
		if moved, err := MoveLegacyProviderPaymentReplacementSweeps(ctx, replacements[0], 1); err != nil || moved != 1 {
			t.Fatal("remaining revenue did not move in one bounded batch", moved, err)
		}
		assertState("fully moved", 0, true)
		if released, err := ReleaseLegacyProviderPaymentReplacement(ctx, replacements[0], policy, false); err != nil || !released {
			t.Fatal("the replacement hold was not released", released, err)
		}
		assertState("released", 0, false)
		if err := RequireProviderUsdcPayment(ctx, replacement); err != nil {
			t.Fatal("released replacement fails actual earning admission", err)
		}
	})
}
