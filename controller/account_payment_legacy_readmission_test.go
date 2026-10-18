// Correction lineage is immutable even when an older writer keys its released
// replacement without retaining a request. Only local fixtures and fake Circle
// listings are used; reconciliation remains separate from amount correction.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Build the real journaled pair, then model an older sender's key-only reserve.
// The remaining revenue forces the existing resume path to refuse that key.
func legacyCorrectionReadmissionFixture(t testing.TB, released bool) (*session.ClientSession, *model.AccountPayment, *model.LegacyProviderPaymentReplacement) {
	t.Helper()
	owner, original, _ := legacyRevenueTestFixture(t, 2)
	pair := legacyRevenueTestPartialAnchor(t, owner, original, released)
	key := server.NewId()
	server.Tx(owner.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, pair.ReplacementPaymentId, key))
	})
	replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
	var retained bool
	server.Db(owner.Ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(owner.Ctx, `SELECT EXISTS (SELECT 1 FROM audit_account_payment
			WHERE event_id=$1 AND payment_id=$2 AND event_type='circle_attempt_request')`, key, replacement.PaymentId).Scan(&retained))
	})
	if retained || replacement.CircleIdempotencyKey == nil || *replacement.CircleIdempotencyKey != key || replacement.AttributionReviewRequired == released {
		t.Fatal("fixture did not retain a journaled replacement with only a legacy key", replacement, retained)
	}
	return owner, replacement, pair
}

// Compare exact local rows so refusal cannot hide a second reduction, changed
// sweep ownership, new payment, overwritten link or extra attempt outcome.
func legacyCorrectionReadmissionState(t testing.TB, ctx context.Context) (state string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_object(
			'payments', (SELECT jsonb_agg(to_jsonb(p) ORDER BY p.payment_id) FROM account_payment p),
			'sweeps', (SELECT jsonb_agg(to_jsonb(s) ORDER BY s.contract_id,s.balance_id,s.network_id) FROM transfer_escrow_sweep s),
			'audits', (SELECT jsonb_agg(to_jsonb(a) ORDER BY a.event_id) FROM audit_account_payment a)
		)::text`).Scan(&state))
	})
	return
}

// A journaled pair remains a reconciliation candidate, never a second amount
// correction. Exercise both preview and the real refusal/resume boundary.
func testLegacyCorrectionReadmission(t *testing.T, released bool) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, replacement, _ := legacyCorrectionReadmissionFixture(t, released)
		attempts, err := model.GetLegacyProviderPaymentAttempts(owner.Ctx)
		if err != nil || len(attempts) != 1 || attempts[0].Payment.PaymentId != replacement.PaymentId {
			t.Fatal("journaled replacement disappeared from generic legacy reconciliation", attempts, err)
		}
		before := legacyCorrectionReadmissionState(t, owner.Ctx)
		preview := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, true, false, 2, 0)
		if preview.AttemptCount != 1 || preview.AbsentCount != 1 || preview.CorrectionCount != 0 || preview.OriginalPayout != 0 || preview.CorrectedPayout != 0 || preview.SweepCount != 0 {
			t.Fatalf("journaled replacement was admitted for a second correction: %+v", preview)
		}
		if legacyCorrectionReadmissionState(t, owner.Ctx) != before {
			t.Fatal("correction preview changed the existing pair")
		}
		result := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, false, false, 2, 0)
		if result.RefusedCount != 1 || result.CorrectionCount != 0 || result.ReplacedCount != 0 || result.MovedSweepCount != 0 || result.ReleasedCount != 0 || result.ScheduledCount != 0 {
			t.Fatalf("keyed pair did not reach its sole existing resume refusal: %+v", result)
		}
		if legacyCorrectionReadmissionState(t, owner.Ctx) != before {
			t.Fatal("refused correction changed a payment, sweep or immutable audit event")
		}
	})
}

// The observed failure: a released key-only replacement must not be reduced
// again or collide with its first immutable payment-id-keyed pair event.
func TestLegacyProviderPaymentCorrectionRevenueReleasedReplacementIsNotRecorrected(t *testing.T) {
	testLegacyCorrectionReadmission(t, true)
}

// A held pair also cannot appear as a fresh reduction in a correction preview.
func TestLegacyProviderPaymentCorrectionRevenueHeldReplacementIsNotRecorrected(t *testing.T) {
	testLegacyCorrectionReadmission(t, false)
}

// Discovery is not authority: a direct caller with a valid reducible amount
// must be refused under the existing transaction before any mutation.
func TestLegacyProviderPaymentCorrectionRevenueReplacementMutationRefusesSecondCorrection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, replacement, _ := legacyCorrectionReadmissionFixture(t, true)
		windows, err := model.GetLegacyProviderPaymentSubsidyWindows(owner.Ctx, []server.Id{replacement.PaymentId})
		if err != nil || windows[replacement.PaymentId] == nil {
			t.Fatal("replacement lost its original subsidy window", err)
		}
		policy := legacyCorrectionTestPolicy()
		correction := model.CorrectLegacyProviderPayment(replacement, windows[replacement.PaymentId], policy)
		if !correction.Changed() || correction.Payout >= replacement.Payout {
			t.Fatal("fixture does not expose repeated application of the reduction", correction)
		}
		before := legacyCorrectionReadmissionState(t, owner.Ctx)
		var secondPaymentId server.Id
		var refusal error
		failure := server.HandleError(func() {
			secondPaymentId, refusal = model.ReplaceLegacyProviderPayment(owner.Ctx, correction, policy, `{"fixture":"second correction must refuse"}`)
		})
		if failure != nil || !errors.Is(refusal, model.ErrLegacyProviderPaymentReplacementChanged) || secondPaymentId != (server.Id{}) {
			t.Fatal("journaled replacement did not refuse cleanly before writes", secondPaymentId, refusal, failure)
		}
		if legacyCorrectionReadmissionState(t, owner.Ctx) != before {
			t.Fatal("direct refusal changed a payment, sweep or immutable audit event")
		}
	})
}

// Excluding correction does not discard a historical USDC attempt: a verified
// listed transfer still binds through the actual legacy reconciliation writer.
func TestLegacyProviderPaymentCorrectionRevenueReplacementStillReconciles(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, replacement, pair := legacyCorrectionReadmissionFixture(t, true)
		before := legacyRevenueTestJournal(t, owner.Ctx, pair)
		amount, formatted := legacyProviderPaymentAmount(replacement.Payout)
		lister := &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
			legacyReconcileTestTransfer("synthetic-corrected-legacy-transfer", "SENT", formatted, replacement.CreateTime.Add(time.Minute)),
		}}
		result := legacyReconcileTestRun(t, owner, lister, false)
		if result.AttemptCount != 1 || result.FoundCount != 1 || result.AbsentCount != 0 || result.RefusedCount != 0 {
			t.Fatalf("correction lineage removed a valid historical reconciliation: %+v", result)
		}
		bound, err := model.GetPayment(owner.Ctx, replacement.PaymentId)
		if err != nil || bound == nil || bound.PaymentRecord == nil || *bound.PaymentRecord != "synthetic-corrected-legacy-transfer" ||
			bound.CircleIdempotencyKey == nil || *bound.CircleIdempotencyKey != *replacement.CircleIdempotencyKey ||
			bound.TokenAmount == nil || *bound.TokenAmount != amount || bound.Payout != replacement.Payout || bound.Canceled ||
			legacyRevenueTestJournal(t, owner.Ctx, pair) != before {
			t.Fatal("reconciliation changed correction lineage or the original submitted amount", bound, err)
		}
	})
}

// A key-only replacement still competes for its destination's transfer. Do
// not make another original look uniquely attributable by hiding the pair.
func TestLegacyProviderPaymentCorrectionRevenueReplacementPreservesGroupAmbiguity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, replacement, _ := legacyCorrectionReadmissionFixture(t, true)
		_, other, _ := legacyCorrectionTestFixture(t, 2, replacement.Payout, 0)
		_, amount := legacyProviderPaymentAmount(other.Payout)
		created := other.CreateTime
		if created.Before(replacement.CreateTime) {
			created = replacement.CreateTime
		}
		lister := &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
			legacyReconcileTestTransfer("synthetic-ambiguous-corrected-transfer", "SENT", amount, created.Add(time.Minute)),
		}}
		before := legacyCorrectionReadmissionState(t, owner.Ctx)
		preview := legacyCorrectionTestRun(t, owner, lister, true, false, 2, 0)
		if preview.AttemptCount != 2 || preview.GroupCount != 1 || preview.AmbiguousCount != 2 || preview.FoundCount != 0 || preview.AbsentCount != 0 || preview.CorrectionCount != 0 {
			t.Fatalf("replacement was hidden from destination attribution: %+v", preview)
		}
		result := legacyCorrectionTestRun(t, owner, lister, false, false, 2, 0)
		if result.AmbiguousCount != 2 || result.RefusedCount != 1 || result.ReplacedCount != 0 || result.ScheduledCount != 0 || legacyCorrectionReadmissionState(t, owner.Ctx) != before {
			t.Fatalf("ambiguous group changed its original or its keyed replacement: %+v", result)
		}
	})
}
