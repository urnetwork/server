// Legacy correction must conserve revenue provenance as well as gross and
// sweep identities. Fixtures use only the isolated test database and fakes.
package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A positive revenue history exposes the anchor-only provenance loss that
// zero-revenue subsidy fixtures cannot distinguish.
func legacyRevenueTestFixture(t testing.TB, count int) (*session.ClientSession, *model.AccountPayment, server.Id) {
	t.Helper()
	owner, original, key, _ := legacyCorrectionTestFixtureWithBytes(t, count, 100,
		model.UsdToNanoCents(1), model.UsdToNanoCents(0.3), model.UsdToNanoCents(0.1))
	if err := model.RequireProviderUsdcPayment(owner.Ctx, original.PaymentId); err != nil {
		t.Fatal("positive-revenue original lacks actual earning authority", err)
	}
	return owner, original, key
}

// Every original sweep identity and its revenue must move, without rewriting
// the canceled original, the replacement's gross, or the pair's journal.
func legacyRevenueTestRequireMoved(t testing.TB, owner *session.ClientSession, original, replacement *model.AccountPayment, originalKeys []string) {
	t.Helper()
	if keys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId); len(keys) != 0 {
		t.Fatal("canceled original still owns revenue-bearing history", keys)
	}
	keys, bytes := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId)
	if !slices.Equal(keys, originalKeys) || bytes != original.PayoutByteCount {
		t.Fatal("replacement lost original sweep identities or bytes", keys, bytes)
	}
	var revenue model.NanoCents
	server.Db(owner.Ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(owner.Ctx, `SELECT COALESCE(SUM(payout_net_revenue_nano_cents),0)::bigint
			FROM transfer_escrow_sweep WHERE payment_id=$1`, replacement.PaymentId).Scan(&revenue))
	})
	if revenue != original.Payout-original.SubsidyPayout-original.ReliabilitySubsidy ||
		replacement.Payout != revenue+model.UsdToNanoCents(0.44) || replacement.AttributionReviewRequired {
		t.Fatal("replacement gross no longer equals conserved revenue and corrected components", replacement, revenue)
	}
	if err := model.RequireProviderUsdcPayment(owner.Ctx, replacement.PaymentId); err != nil {
		t.Fatal("released replacement fails the actual sender's earning census", err)
	}
	canceled, err := model.GetPayment(owner.Ctx, original.PaymentId)
	if err != nil || !canceled.Canceled || canceled.Payout != original.Payout || canceled.CircleIdempotencyKey == nil ||
		*canceled.CircleIdempotencyKey != *original.CircleIdempotencyKey {
		t.Fatal("correction rewrote the historical obligation", canceled, err)
	}
}

// This is the M21 root-cause regression: the default correction must leave an
// actually admissible replacement, not merely one nonempty anchor and no hold.
func TestLegacyProviderPaymentCorrectionRevenueDefaultPaysOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, key := legacyRevenueTestFixture(t, 2)
		originalKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, key, &sends)
		result := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, false, true, 2, 0)
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		// Check the actual downstream admission before reporting route counters.
		if err := model.RequireProviderUsdcPayment(owner.Ctx, replacement.PaymentId); err != nil {
			t.Fatal("default correction released an unpayable revenue obligation", err)
		}
		if result.AnchorCount != 0 || result.MovedSweepCount != 2 || result.ReleasedCount != 1 || result.RefusedCount != 0 {
			t.Fatalf("positive revenue did not take the bounded full-move route: %+v", result)
		}
		legacyRevenueTestRequireMoved(t, owner, original, replacement, originalKeys)
		for range 2 {
			if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: replacement.PaymentId}, owner); err != nil {
				t.Fatal("corrected revenue payment did not advance", err)
			}
		}
		if len(sends) != 1 || sends[0].key == key || fmt.Sprintf("%f", sends[0].amount) != "0.630000" {
			t.Fatal("corrected revenue was not sent once with a fresh key", sends)
		}
	})
}

// A call can stop after a committed partial batch and resume without changing
// amounts or journals; no key or external send is admitted while still moving.
func TestLegacyProviderPaymentCorrectionRevenueBoundedResume(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, key := legacyRevenueTestFixture(t, 3)
		originalKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, key, &sends)
		lister := &legacyReconcileTestLister{}
		first := legacyCorrectionTestRun(t, owner, lister, false, true, 1, 1)
		held := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if first.MovedSweepCount != 1 || first.MovingCount != 1 || first.ReleasedCount != 0 || !held.AttributionReviewRequired {
			t.Fatalf("bounded correction did not stop held after one committed batch: %+v", first)
		}
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: held.PaymentId}, owner); err == nil || len(sends) != 0 {
			t.Fatal("partially moved revenue escaped its review hold", err, sends)
		}
		for range 3 {
			legacyCorrectionTestRun(t, owner, lister, false, true, 1, 1)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		legacyRevenueTestRequireMoved(t, owner, original, replacement, originalKeys)
		if replacement.Payout != held.Payout || replacement.CircleIdempotencyKey != nil ||
			legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_replaced") != 1 ||
			legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_replacement") != 1 {
			t.Fatal("resume changed the exact replacement or duplicated its pair")
		}
		again := legacyCorrectionTestRun(t, owner, lister, false, false, 1, 1)
		if again.ReplacedCount != 0 || again.MovedSweepCount != 0 || again.ReleasedCount != 0 || again.RefusedCount != 0 {
			t.Fatalf("finished revenue movement was not idempotent: %+v", again)
		}
	})
}

// Construct the historical anchor-only shape without using the corrected
// release. Clearing review below is an isolated fixture write, not repair code.
func legacyRevenueTestPartialAnchor(t testing.TB, owner *session.ClientSession, original *model.AccountPayment, released bool) *model.LegacyProviderPaymentReplacement {
	t.Helper()
	windows, err := model.GetLegacyProviderPaymentSubsidyWindows(owner.Ctx, []server.Id{original.PaymentId})
	if err != nil || windows[original.PaymentId] == nil {
		t.Fatal("missing correction window", err)
	}
	policy := legacyCorrectionTestPolicy()
	correction := model.CorrectLegacyProviderPayment(original, windows[original.PaymentId], policy)
	if _, err := model.ReplaceLegacyProviderPayment(owner.Ctx, correction, policy, `{"fixture":"affirmatively absent"}`); err != nil {
		t.Fatal(err)
	}
	pairs, err := model.GetLegacyProviderPaymentReplacements(owner.Ctx)
	if err != nil || len(pairs) != 1 {
		t.Fatal("missing replacement pair", err)
	}
	cutoff, err := legacyProviderPaymentEarningCutoff(owner.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := model.MoveLegacyProviderPaymentReplacementAnchor(owner.Ctx, pairs[0], cutoff); err != nil || !moved {
		t.Fatal("could not prepare old anchor shape", moved, err)
	}
	if released {
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET attribution_review_required=false WHERE payment_id=$1`, pairs[0].ReplacementPaymentId))
		})
	}
	return pairs[0]
}

// Release independently checks actual earning authority, even if a caller
// bypasses the controller's choice of full movement for nonzero revenue.
func TestLegacyProviderPaymentCorrectionRevenueRejectsPartialRelease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, _ := legacyRevenueTestFixture(t, 2)
		pair := legacyRevenueTestPartialAnchor(t, owner, original, false)
		released, err := model.ReleaseLegacyProviderPaymentReplacement(owner.Ctx, pair, legacyCorrectionTestPolicy(), true)
		if released || !errors.Is(err, model.ErrProviderUsdcAttributionUnresolved) {
			t.Fatal("one positive-revenue anchor released the entire debt", released, err)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if !replacement.AttributionReviewRequired || replacement.CircleIdempotencyKey != nil {
			t.Fatal("refused release changed submission authority", replacement)
		}
	})
}

// Read the immutable original pair evidence for comparison across recovery.
func legacyRevenueTestJournal(t testing.TB, ctx context.Context, pair *model.LegacyProviderPaymentReplacement) (details string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT event_details FROM audit_account_payment WHERE payment_id=$1
			AND event_type='legacy_payment_replacement'`, pair.ReplacementPaymentId).Scan(&details))
	})
	return
}

// A historical bad release is repairable only while still unsubmitted. The
// paired guard restores review before the bounded move and preserves history.
func TestLegacyProviderPaymentCorrectionRevenueRepairsReleasedUnsubmitted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, _ := legacyRevenueTestFixture(t, 2)
		originalKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		pair := legacyRevenueTestPartialAnchor(t, owner, original, true)
		before := legacyRevenueTestJournal(t, owner.Ctx, pair)
		broken := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if broken.AttributionReviewRequired || model.RequireProviderUsdcPayment(owner.Ctx, broken.PaymentId) == nil {
			t.Fatal("fixture does not reproduce the released but unpayable historical shape")
		}
		result := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, false, false, 2, 0)
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if result.MovedSweepCount != 1 || result.ReleasedCount != 1 || result.RefusedCount != 0 || replacement.Payout != broken.Payout {
			t.Fatalf("unsubmitted old release did not resume with the same amount: %+v", result)
		}
		legacyRevenueTestRequireMoved(t, owner, original, replacement, originalKeys)
		if after := legacyRevenueTestJournal(t, owner.Ctx, pair); after != before {
			t.Fatal("recovery rewrote historical correction evidence")
		}
	})
}

// A key, processor record or chain marker forbids re-holding/moving a released
// replacement. Each caller owns one complete isolated fixture lifecycle.
func legacyRevenueTestRefusesSubmittedRepair(t *testing.T, state string) {
	t.Helper()
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, _ := legacyRevenueTestFixture(t, 2)
		pair := legacyRevenueTestPartialAnchor(t, owner, original, true)
		before := legacyRevenueTestJournal(t, owner.Ctx, pair)
		key := server.NewId()
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			switch state {
			case "key":
				server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, pair.ReplacementPaymentId, key))
			case "record":
				server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET payment_record='synthetic-live-replacement' WHERE payment_id=$1`, pair.ReplacementPaymentId))
			case "chain":
				server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET tx_hash='synthetic-live-chain-hash' WHERE payment_id=$1`, pair.ReplacementPaymentId))
			}
		})
		held, err := model.ReholdLegacyProviderPaymentReplacement(owner.Ctx, pair)
		if held || !errors.Is(err, model.ErrLegacyProviderPaymentReplacementChanged) {
			t.Fatal("recovery interfered with a possibly submitted replacement", held, err)
		}
		result := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, false, false, 2, 0)
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		keys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		if result.RefusedCount != 1 || result.MovedSweepCount != 0 || result.ScheduledCount != 0 || len(keys) != 1 ||
			replacement.AttributionReviewRequired || legacyRevenueTestJournal(t, owner.Ctx, pair) != before {
			t.Fatalf("refused submitted recovery changed the pair: %+v", result)
		}
		switch state {
		case "key":
			if replacement.CircleIdempotencyKey == nil || *replacement.CircleIdempotencyKey != key {
				t.Fatal("key changed")
			}
		case "record":
			if replacement.PaymentRecord == nil || *replacement.PaymentRecord != "synthetic-live-replacement" {
				t.Fatal("record changed")
			}
		case "chain":
			if replacement.TxHash == nil || *replacement.TxHash != "synthetic-live-chain-hash" {
				t.Fatal("chain marker changed")
			}
		}
	})
}

// A reserved key prevents changing provenance beneath a possible submit.
func TestLegacyProviderPaymentCorrectionRevenueRefusesKeyedRepair(t *testing.T) {
	legacyRevenueTestRefusesSubmittedRepair(t, "key")
}

// A processor record prevents changing provenance beneath an accepted submit.
func TestLegacyProviderPaymentCorrectionRevenueRefusesRecordedRepair(t *testing.T) {
	legacyRevenueTestRefusesSubmittedRepair(t, "record")
}

// Chain evidence prevents changing provenance beneath an observed transfer.
func TestLegacyProviderPaymentCorrectionRevenueRefusesChainRepair(t *testing.T) {
	legacyRevenueTestRefusesSubmittedRepair(t, "chain")
}

// Re-hold is an exact released-state transition, not permission to mutate an
// already held/moving replacement. Its ordinary bounded resume owns that path.
func TestLegacyProviderPaymentCorrectionRevenueReholdRefusesMovingPair(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, _ := legacyRevenueTestFixture(t, 2)
		pair := legacyRevenueTestPartialAnchor(t, owner, original, false)
		before := legacyRevenueTestJournal(t, owner.Ctx, pair)
		held, err := model.ReholdLegacyProviderPaymentReplacement(owner.Ctx, pair)
		if held || !errors.Is(err, model.ErrLegacyProviderPaymentReplacementChanged) {
			t.Fatal("re-hold did not require the exact released state", held, err)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		keys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		if !replacement.AttributionReviewRequired || len(keys) != 1 || legacyRevenueTestJournal(t, owner.Ctx, pair) != before {
			t.Fatal("refused re-hold mutated moving provenance")
		}
	})
}
