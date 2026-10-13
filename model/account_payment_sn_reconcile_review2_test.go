package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Tests for the §12 amendments of SN-RECONCILE.md. Synthetic legacy rows are
// written directly, as the historical planner and sender left them.

// An RC-1 row without sweeps, created at `created` and canceled at
// `cancelTime`, which may be nil.
func snReconcileReview2Canceled(t testing.TB, ctx context.Context, networkId server.Id, created time.Time, cancelTime *time.Time, contracts ...server.Id) server.Id {
	t.Helper()
	paymentId := snReconcileTestPayment(t, ctx, networkId, nil, created, contracts...)
	snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,circle_idempotency_key=$3 WHERE payment_id=$1`, paymentId, cancelTime, server.NewId())
	return paymentId
}

// A10: A5's evidence counts only a row the network created after the stuck
// row's cancellation, or after its creation when it has no cancel time. A row
// created before the cancellation cannot hold the re-planned sweeps.
func TestSnReconcileReplanEvidenceAfterCancelOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		created := payoutTestCutoff.Add(-60 * day)
		canceled := created.Add(30 * day)
		// A later row of the network, created at `at`, holding an early sweep
		// that closed before the stuck row was created.
		holder := func(cohort *payoutTransitionCohort, at time.Time) {
			closed := created.Add(-day)
			paymentId := snReconcileTestPayment(t, ctx, cohort.network, nil, at, cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)))
			snReconcileTestEpoch(t, ctx, paymentId, at)
		}
		a, b, c, d := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		beforeCancel := snReconcileReview2Canceled(t, ctx, a.network, created, &canceled)
		holder(a, canceled.Add(-time.Microsecond))
		atCancel := snReconcileReview2Canceled(t, ctx, b.network, created, &canceled)
		holder(b, canceled)
		afterCancel := snReconcileReview2Canceled(t, ctx, c.network, created, &canceled)
		holder(c, canceled.Add(time.Microsecond))
		withoutCancelTime := snReconcileReview2Canceled(t, ctx, d.network, created, nil)
		holder(d, created.Add(time.Microsecond))

		rows := snReconcileTestStuckById(t, ctx)
		for paymentId, want := range map[server.Id]bool{beforeCancel: false, atCancel: false, afterCancel: true, withoutCancelTime: true} {
			if rows[paymentId] == nil || rows[paymentId].ReplanEvidence != want {
				t.Fatal("replan evidence", paymentId, rows[paymentId], want)
			}
		}
	})
}

// A8: a stuck row without sweeps whose network created any row after its
// cancellation (its creation without a cancel time), and on or before
// 2026-08-17T23:59:59Z, had its sweeps re-planned, even when that row holds no
// sweeps now.
func TestSnReconcileStuckRowsReplannedBeforeFix(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		created := payoutTestCutoff.Add(-60 * day)
		canceled := created.Add(30 * day)
		later := func(networkId server.Id, at time.Time) {
			snReconcileTestPayment(t, ctx, networkId, nil, at)
		}
		// A stuck row canceled at `cancelTime`, and a later row of its network created at `at`.
		stuck := func(cancelTime *time.Time, at time.Time) server.Id {
			networkId := server.NewId()
			paymentId := snReconcileReview2Canceled(t, ctx, networkId, created, cancelTime)
			later(networkId, at)
			return paymentId
		}
		atFix := stuck(&canceled, SnReconcileReplanFixTime)
		afterFix := stuck(&canceled, SnReconcileReplanFixTime.Add(time.Microsecond))
		atCancel := stuck(&canceled, canceled)
		afterCancel := stuck(&canceled, canceled.Add(time.Microsecond))
		withoutCancelTime := stuck(nil, created.Add(time.Microsecond))
		otherNetwork := snReconcileReview2Canceled(t, ctx, server.NewId(), created, &canceled)
		later(server.NewId(), canceled.Add(day))
		// A row that holds sweeps itself never reads the flag.
		cohort := newPayoutTransitionCohort(t, ctx)
		closed := created.Add(-day)
		withSweeps := snReconcileReview2Canceled(t, ctx, cohort.network, created, &canceled, cohort.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)))
		later(cohort.network, canceled.Add(day))

		rows := snReconcileTestStuckById(t, ctx)
		for paymentId, want := range map[server.Id]bool{atFix: true, afterFix: false, atCancel: false, afterCancel: true, withoutCancelTime: true, otherNetwork: false, withSweeps: false} {
			if rows[paymentId] == nil || rows[paymentId].ReplannedBeforeFix != want || rows[paymentId].ReplanEvidence {
				t.Fatal("replanned before the fix", paymentId, rows[paymentId], want)
			}
		}
	})
}
