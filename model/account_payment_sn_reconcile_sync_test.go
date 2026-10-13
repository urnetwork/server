package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Tests for what origin/main's legacy payment tools change about the census.

// RC-22: the legacy correction cancels an inflated legacy attempt with its
// key kept and moves its earnings to a replacement, which the sender pays. The
// canceled original would read as RC-1; neither it nor its replacement is a
// stuck row, before or after the replacement takes its anchor sweep.
func TestSnReconcileStuckRowsExcludeReplacedLegacyOriginal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cohort := newPayoutTransitionCohort(t, ctx)
		wallet := GetPayoutWalletId(ctx, cohort.network)
		created := payoutTestCutoff.Add(-60 * 24 * time.Hour)
		closed := created.Add(-24 * time.Hour)
		reliability := UsdToNanoCents(2)
		// A reliability subsidy for a plan window of a microsecond: the
		// correction pays it pro rata over its period, so it shrinks.
		original := snReconcileTestPaymentPlus(t, ctx, reliability, cohort.network, wallet, created,
			cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)), cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)))
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
		assertNotStuck := func(stage string) {
			t.Helper()
			rows := snReconcileTestStuckById(t, ctx)
			if rows[original] != nil || rows[replacement] != nil {
				t.Fatal("a replaced original or its replacement is stuck", stage)
			}
		}
		assertNotStuck("replaced")

		replacements, err := GetLegacyProviderPaymentReplacements(ctx)
		if err != nil || len(replacements) != 1 {
			t.Fatal("replacements", replacements, err)
		}
		anchored, err := MoveLegacyProviderPaymentReplacementAnchor(ctx, replacements[0], nil)
		if err != nil || !anchored {
			t.Fatal("the replacement did not take its anchor sweep", anchored, err)
		}
		if released, err := ReleaseLegacyProviderPaymentReplacement(ctx, replacements[0], policy, anchored); err != nil || !released {
			t.Fatal("the replacement hold was not released", released, err)
		}
		assertNotStuck("anchored")
	})
}
