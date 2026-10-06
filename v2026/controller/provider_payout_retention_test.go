package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func controllerRetainAttempt(t testing.TB, owner *session.ClientSession, payment *model.AccountPayment, record string) (*model.AccountPayment, *model.ProviderPaymentBasis) {
	t.Helper()
	wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
	basis, err := model.ReserveProviderPaymentBasis(owner.Ctx, payment, wallet)
	if err != nil {
		t.Fatal(err)
	}
	amount := model.NanoCentsToUsd(payment.Payout) - .01
	if err := model.RetainProviderPaymentRequest(owner.Ctx, basis, amount, "MATIC"); err != nil {
		t.Fatal(err)
	}
	if err := model.SetProviderPaymentRecord(owner.Ctx, basis, amount, record); err != nil {
		t.Fatal(err)
	}
	retained, err := model.GetPayment(owner.Ctx, payment.PaymentId)
	if err != nil {
		t.Fatal(err)
	}
	return retained, basis
}

func controllerRetainedComponents(t testing.TB, owner *session.ClientSession, payment *model.AccountPayment, subsidy, reliability model.NanoCents) *model.AccountPayment {
	t.Helper()
	cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
	server.Tx(owner.Ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(owner.Ctx, `INSERT INTO subsidy_payment(payment_plan_id,start_time,end_time,active_user_count,paid_user_count,net_payout_byte_count_paid,net_payout_byte_count_unpaid,net_revenue_nano_cents,net_payout_nano_cents)
		VALUES($1,$2,$3,1,0,0,100,0,$4)`, payment.PaymentPlanId, cutoff.Add(-time.Hour), cutoff, subsidy+reliability))
		server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET subsidy_payout_nano_cents=$2,reliability_subsidy_nano_cents=$3 WHERE payment_id=$1`, payment.PaymentId, subsidy, reliability))
		server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE transfer_escrow_sweep SET payout_net_revenue_nano_cents=$2 WHERE payment_id=$1`, payment.PaymentId, payment.Payout-subsidy-reliability))
	})
	updated, err := model.GetPayment(owner.Ctx, payment.PaymentId)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.RequireProviderUsdcPayment(owner.Ctx, payment.PaymentId); err != nil {
		t.Fatal("original allocation invalid", err)
	}
	return updated
}

func TestProviderTransitionStaleProcessorResponsesCannotMutateNewAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		for _, status := range []string{"DENIED", "FAILED", "CANCELLED", "SENT", "STUCK", "CONFIRMED", "COMPLETE"} {
			owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
			old, _ := controllerRetainAttempt(t, owner, payment, "old-"+status)
			var next *model.AccountPayment
			client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
				t.Fatal("stale GET unexpectedly submitted")
				return nil, nil
			})
			client.GetTransactionFunc = func(context.Context, string) (*GetTransactionResult, error) {
				// Actual GET has retained its old request. Another worker now
				// observes a definitive failure and admits the next attempt.
				if _, _, err := model.ApplyProviderPaymentOutcome(owner.Ctx, old, "FAILED", `{"state":"FAILED","worker":"first"}`, "", false); err != nil {
					t.Fatal(err)
				}
				fresh, err := model.GetPayment(owner.Ctx, payment.PaymentId)
				if err != nil {
					t.Fatal(err)
				}
				next, _ = controllerRetainAttempt(t, owner, fresh, "next-"+status)
				hash := ""
				if status == "SENT" || status == "STUCK" || status == "CONFIRMED" || status == "COMPLETE" {
					hash = "0xold-attempt"
				}
				return &GetTransactionResult{Transaction: CircleTransaction{Id: *old.PaymentRecord, State: status, TxHash: hash}, ResponseBodyBytes: []byte(fmt.Sprintf(`{"state":%q,"worker":"delayed"}`, status))}, nil
			}
			result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
			if !errors.Is(err, model.ErrProviderPaymentAttemptChanged) || result.Complete || result.Canceled {
				t.Fatalf("stale %s response changed lifecycle: %+v %v", status, result, err)
			}
			got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
			if err != nil || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *next.CircleIdempotencyKey || got.PaymentRecord == nil || *got.PaymentRecord != *next.PaymentRecord || got.Completed || got.Canceled || got.TxHash != nil {
				t.Fatalf("stale %s overwrote current attempt: %+v %v", status, got, err)
			}
			server.Db(owner.Ctx, func(conn server.PgConn) {
				var retained int
				server.Raise(conn.QueryRow(owner.Ctx, `SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_details::jsonb->>'status'=$2`, payment.PaymentId, "STALE_"+status).Scan(&retained))
				if retained != 1 {
					t.Fatal("stale original response not retained", status)
				}
			})
		}
	})
}

func TestProviderTransitionRepeatedCancellationRetainsSameComponentsAndHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		payment = controllerRetainedComponents(t, owner, payment, model.UsdToNanoCents(1), model.UsdToNanoCents(2))
		var keys []server.Id
		client := controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderPayment(ctx); err != nil {
				t.Fatal(err)
			}
			if amount != 9.99 {
				t.Fatal("component retry changed amount", amount)
			}
			keys = append(keys, key)
			return &CreateTransferTransactionResult{Id: fmt.Sprintf("attempt-%d", len(keys))}, nil
		})
		client.GetTransactionFunc = func(ctx context.Context, id string) (*GetTransactionResult, error) {
			return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "CANCELLED"}, ResponseBodyBytes: []byte(fmt.Sprintf(`{"id":%q,"state":"CANCELLED"}`, id))}, nil
		}
		for attempt := 0; attempt < 2; attempt++ {
			for call := 0; call < 2; call++ {
				result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
				if err != nil || result.Complete || result.Canceled {
					t.Fatalf("component obligation reported finished: %+v %v", result, err)
				}
			}
			got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
			if err != nil || got.Canceled || got.Completed || got.CircleIdempotencyKey != nil || got.PaymentRecord != nil || got.Payout != payment.Payout || got.SubsidyPayout != payment.SubsidyPayout || got.ReliabilitySubsidy != payment.ReliabilitySubsidy || got.PaymentPlanId != payment.PaymentPlanId {
				t.Fatalf("same original components lost: %+v %v", got, err)
			}
		}
		if len(keys) != 2 || keys[0] == keys[1] {
			t.Fatal("known failed attempts did not retain distinct processor keys", keys)
		}
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var requests, outcomes, distinctKeys, payments, windows int
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT
			(SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_type='circle_attempt_request'),
			(SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_details::jsonb->>'status'='CANCELLED'),
			(SELECT COUNT(DISTINCT event_details::jsonb->>'idempotency_key') FROM audit_account_payment WHERE payment_id=$1 AND event_details::jsonb->>'status'='CANCELLED'),
			(SELECT COUNT(*) FROM account_payment WHERE network_id=$2),(SELECT COUNT(*) FROM subsidy_payment WHERE payment_plan_id=$3)`, payment.PaymentId, payment.NetworkId, payment.PaymentPlanId).Scan(&requests, &outcomes, &distinctKeys, &payments, &windows))
			if requests != 2 || outcomes != 2 || distinctKeys != 2 || payments != 1 || windows != 1 {
				t.Fatal("attempt history or original allocation lost", requests, outcomes, distinctKeys, payments, windows)
			}
		})
	})
}

func TestProviderTransitionAttemptJournalFailureRollsBackReset(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		old, _ := controllerRetainAttempt(t, owner, payment, "retained-attempt")
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `CREATE FUNCTION cutover_reject_attempt_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic attempt journal unavailable'; END $$;
		CREATE TRIGGER cutover_reject_attempt_audit BEFORE INSERT ON audit_account_payment FOR EACH ROW WHEN(NEW.event_type='circle_attempt_outcome') EXECUTE FUNCTION cutover_reject_attempt_audit()`))
		})
		var failure any
		func() {
			defer func() {
				if value := recover(); value != nil {
					failure = value
				}
			}()
			_, _, err := model.ApplyProviderPaymentOutcome(owner.Ctx, old, "FAILED", `{"state":"FAILED"}`, "", false)
			if err != nil {
				failure = err
			}
		}()
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `DROP TRIGGER cutover_reject_attempt_audit ON audit_account_payment; DROP FUNCTION cutover_reject_attempt_audit()`))
		})
		if failure == nil {
			t.Fatal("failed attempt journal acknowledged reset")
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *old.CircleIdempotencyKey || got.PaymentRecord == nil || *got.PaymentRecord != *old.PaymentRecord || got.Payout != old.Payout || got.Canceled || got.Completed {
			t.Fatalf("journal failure changed obligation or attempt: %+v %v", got, err)
		}
		if _, _, err := model.ApplyProviderPaymentOutcome(owner.Ctx, got, "FAILED", `{"state":"FAILED"}`, "", false); err != nil {
			t.Fatal("same retained attempt could not resume", err)
		}
	})
}

func TestProviderTransitionInvalidDestinationCannotClearAcceptedAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			basis := ctx.Value(providerUsdcPaymentContextKey{}).(providerPaymentSubmission).Basis
			if err := model.SetProviderPaymentRecord(ctx, &basis, amount, "accepted-by-peer"); err != nil {
				t.Fatal(err)
			}
			return nil, &server.HttpStatusError{StatusCode: http.StatusBadRequest, Status: "400", ResponseBody: `{"code":155219,"message":"Invalid destination address"}`}
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err == nil || result.Complete || result.Canceled {
			t.Fatal("stale submit error acknowledged reset", result, err)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got.PaymentRecord == nil || *got.PaymentRecord != "accepted-by-peer" || got.CircleIdempotencyKey == nil || got.Canceled {
			t.Fatal("late invalid-destination erased accepted peer", got, err)
		}
	})
}

func TestProviderTransitionSubFeeOriginalIsHeldNotCanceled(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond), model.UsdToNanoCents(.005))
		payment = controllerRetainedComponents(t, owner, payment, payment.Payout, 0)
		controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			t.Fatal("sub-fee obligation sent")
			return nil, nil
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err == nil || result.Complete || result.Canceled {
			t.Fatalf("sub-fee held state hidden: %+v %v", result, err)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got.Canceled || got.Completed || got.Payout != payment.Payout || got.SubsidyPayout != payment.SubsidyPayout || got.CircleIdempotencyKey != nil {
			t.Fatal("sub-fee original obligation discarded", got, err)
		}
	})
}

func TestProviderTransitionMissingAndInactiveWalletRemainPending(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		for _, missing := range []bool{true, false} {
			owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
			server.Tx(owner.Ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(owner.Ctx, `DELETE FROM payout_wallet WHERE network_id=$1`, payment.NetworkId))
				if missing {
					server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET wallet_id=NULL WHERE payment_id=$1`, payment.PaymentId))
				} else {
					server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_wallet SET active=false WHERE wallet_id=$1`, *payment.WalletId))
				}
			})
			result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
			if err == nil || result.Complete || result.Canceled {
				t.Fatalf("held wallet state reported canceled: %+v %v", result, err)
			}
			got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
			if err != nil || got.Canceled || got.Completed || got.Payout != payment.Payout || got.CircleIdempotencyKey != nil {
				t.Fatal("held wallet debt changed", got, err)
			}
		}
	})
}

func TestProviderTransitionResetFailureRollsBackAttemptJournal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		old, _ := controllerRetainAttempt(t, owner, payment, "retained-attempt")
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `CREATE FUNCTION cutover_reject_attempt_reset() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic attempt reset unavailable'; END $$;
		CREATE TRIGGER cutover_reject_attempt_reset BEFORE UPDATE ON account_payment FOR EACH ROW WHEN(OLD.payment_record IS NOT NULL AND NEW.payment_record IS NULL) EXECUTE FUNCTION cutover_reject_attempt_reset()`))
		})
		var failure any
		func() {
			defer func() {
				if value := recover(); value != nil {
					failure = value
				}
			}()
			_, _, err := model.ApplyProviderPaymentOutcome(owner.Ctx, old, "FAILED", `{"state":"FAILED"}`, "", false)
			if err != nil {
				failure = err
			}
		}()
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `DROP TRIGGER cutover_reject_attempt_reset ON account_payment; DROP FUNCTION cutover_reject_attempt_reset()`))
		})
		if failure == nil {
			t.Fatal("failed reset was acknowledged")
		}
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var resets int
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_details::jsonb->>'status'='FAILED'`, payment.PaymentId).Scan(&resets))
			if resets != 0 {
				t.Fatal("failed reset committed its journal in a separate transaction")
			}
		})
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *old.CircleIdempotencyKey || got.PaymentRecord == nil || *got.PaymentRecord != *old.PaymentRecord || got.Canceled || got.Completed {
			t.Fatal("failed reset changed original attempt", got, err)
		}
	})
}

func TestProviderTransitionLateAcceptancePreservesNewAttemptAndFlagsConflict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
		var next *model.AccountPayment
		controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			basis := ctx.Value(providerUsdcPaymentContextKey{}).(providerPaymentSubmission).Basis
			// A simultaneous worker has already observed a definitive rejection.
			// If this delayed response contradicts it, both attempts must remain
			// attributable; do not overwrite the newer result or authorize more.
			if err := model.ResetProviderPaymentSubmission(ctx, &basis, `{"code":155219}`); err != nil {
				t.Fatal(err)
			}
			current, err := model.GetPayment(ctx, payment.PaymentId)
			if err != nil {
				t.Fatal(err)
			}
			next, _ = controllerRetainAttempt(t, owner, current, "next-accepted")
			return &CreateTransferTransactionResult{Id: "old-late-accepted"}, nil
		})
		result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: payment.PaymentId}, owner)
		if err == nil || result.Complete || result.Canceled {
			t.Fatal("late contradictory acceptance silently replaced attempt", result, err)
		}
		got, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil || !got.AttributionReviewRequired || got.PaymentRecord == nil || *got.PaymentRecord != *next.PaymentRecord || got.CircleIdempotencyKey == nil || *got.CircleIdempotencyKey != *next.CircleIdempotencyKey {
			t.Fatal("late acceptance lost current authority", got, err)
		}
		if err := model.RequireProviderUsdcPayment(owner.Ctx, payment.PaymentId); err == nil {
			t.Fatal("contradictory accepted attempts authorized another submission")
		}
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var records int
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT COUNT(*) FROM audit_account_payment WHERE payment_id=$1 AND event_type='circle_late_acceptance' AND event_details::jsonb->>'record'='old-late-accepted'`, payment.PaymentId).Scan(&records))
			if records != 1 {
				t.Fatal("late accepted processor record was discarded")
			}
		})
	})
}

func TestProviderTransitionHeldPaymentPostSchedulesBoundedContinuation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond), model.UsdToNanoCents(.005))
		controllerRetainedComponents(t, owner, payment, payment.Payout, 0)
		args := &AdvancePaymentArgs{PaymentId: payment.PaymentId}
		result, err := AdvancePayment(args, owner)
		if err == nil || result.Complete || result.Canceled {
			t.Fatal("missing actual pending/held result", result, err)
		}
		before := server.NowUtc()
		// The post consumer uses only the returned lifecycle flags. Its run-once
		// admission owns one delayed continuation, even if invoked twice.
		for i := 0; i < 2; i++ {
			server.Tx(owner.Ctx, func(tx server.PgTx) {
				if err := AdvancePaymentPost(args, result, owner, tx); err != nil {
					t.Fatal(err)
				}
			})
		}
		after := server.NowUtc()
		server.Db(owner.Ctx, func(conn server.PgConn) {
			var count int
			var earliest, latest time.Time
			server.Raise(conn.QueryRow(owner.Ctx, `SELECT COUNT(*),MIN(run_at),MAX(run_at) FROM pending_task WHERE args_json::jsonb->>'payment_id'=$1`, payment.PaymentId.String()).Scan(&count, &earliest, &latest))
			if count != 1 || earliest.Before(before.Add(5*time.Minute)) || latest.After(after.Add(30*time.Minute)) {
				t.Fatal("held result abandoned or busy-spun continuation", count, earliest, latest)
			}
		})
	})
}
