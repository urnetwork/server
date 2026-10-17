// Destination/record ownership is exercised through real reservation, legacy
// reconciliation and status paths. Barriers order accepted responses and SQL
// admission; no sleeps or short negative timeouts establish exclusion.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// This per-test listing starts only after the controller's stale snapshots.
type paymentAttributionTestLister struct {
	list func(context.Context) ([]*CircleListedTransaction, error)
}

// The fixed destination arguments are still passed through the real caller.
func (self *paymentAttributionTestLister) ListDestinationTransactions(ctx context.Context, _, _ string) ([]*CircleListedTransaction, error) {
	return self.list(ctx)
}

// Every started worker publishes its actual return, including panic failures.
type paymentAttributionTestResult struct {
	reconcile *LegacyProviderPaymentReconcileResult
	advance   *AdvancePaymentResult
	err       error
}

// A bounded receive is only a failsafe; the returned event is the positive edge.
func paymentAttributionTestWait(t testing.TB, ctx context.Context, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal("synthetic payment ownership barrier did not arrive", ctx.Err())
	}
}

// Both actual callbacks remain concurrent until the test selects their order.
func paymentAttributionTestRace(t *testing.T, submissionFirst, loseAcknowledgement, emptyListing bool) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		legacyOwner, legacy, legacyKey := legacyReconcileTestFixture(t)
		freshOwner, fresh := controllerPayoutFixture(t, time.Date(2020, 1, 6, 23, 59, 59, 0, time.UTC))
		ctx, cancel := context.WithTimeout(legacyOwner.Ctx, 30*time.Second)
		defer cancel()
		legacySession, freshSession := *legacyOwner, *freshOwner
		legacySession.Ctx, freshSession.Ctx = ctx, ctx
		listed, accepted := make(chan struct{}), make(chan struct{})
		finishListing, finishAcceptance := make(chan struct{}), make(chan struct{})
		var listingOnce, acceptanceOnce sync.Once
		var workers sync.WaitGroup
		t.Cleanup(func() {
			cancel()
			listingOnce.Do(func() { close(finishListing) })
			acceptanceOnce.Do(func() { close(finishAcceptance) })
			joined := make(chan struct{})
			go func() { workers.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(30 * time.Second):
				t.Error("payment attribution workers did not join")
			}
		})
		const record = "synthetic-concurrent-processor-record"
		client := controllerPayoutMock(t, func(callCtx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
			if err := requireCircleProviderTransfer(callCtx, circleTransferArguments{IdempotencyKey: key, Amount: amount, Destination: address, Network: network}); err != nil {
				return nil, err
			}
			close(accepted)
			select {
			case <-finishAcceptance:
			case <-callCtx.Done():
				return nil, callCtx.Err()
			}
			if loseAcknowledgement {
				return nil, errors.New("synthetic processor accepted but acknowledgement was lost")
			}
			return &CreateTransferTransactionResult{Id: record, State: "INITIATED"}, nil
		})
		lister := &paymentAttributionTestLister{list: func(callCtx context.Context) ([]*CircleListedTransaction, error) {
			close(listed)
			select {
			case <-finishListing:
			case <-callCtx.Done():
				return nil, callCtx.Err()
			}
			if emptyListing {
				return nil, nil
			}
			return []*CircleListedTransaction{legacyReconcileTestTransfer(record, "COMPLETE", "9.990000", legacy.CreateTime.Add(time.Minute))}, nil
		}}
		legacyDone, freshDone := make(chan paymentAttributionTestResult, 1), make(chan paymentAttributionTestResult, 1)
		workers.Go(func() {
			outcome := paymentAttributionTestResult{}
			if panicValue := server.HandleError(func() {
				outcome.reconcile, outcome.err = reconcileLegacyProviderPaymentAttempts(&legacySession, &LegacyProviderPaymentReconcileSettings{}, lister,
					legacyReconcileTestPayoutWalletId, func() time.Time { return server.NowUtc().Add(2 * legacyProviderPaymentMinAge) })
			}); panicValue != nil {
				outcome.err = fmt.Errorf("legacy worker panic: %v", panicValue)
			}
			legacyDone <- outcome
		})
		paymentAttributionTestWait(t, ctx, listed)
		workers.Go(func() {
			outcome := paymentAttributionTestResult{}
			if panicValue := server.HandleError(func() {
				outcome.advance, outcome.err = AdvancePayment(&AdvancePaymentArgs{PaymentId: fresh.PaymentId}, &freshSession)
			}); panicValue != nil {
				outcome.err = fmt.Errorf("submission worker panic: %v", panicValue)
			}
			freshDone <- outcome
		})
		paymentAttributionTestWait(t, ctx, accepted)
		await := func(done <-chan paymentAttributionTestResult) paymentAttributionTestResult {
			select {
			case outcome := <-done:
				return outcome
			case <-ctx.Done():
				t.Fatal("payment worker did not return", ctx.Err())
				return paymentAttributionTestResult{}
			}
		}
		var submitted paymentAttributionTestResult
		if submissionFirst {
			acceptanceOnce.Do(func() { close(finishAcceptance) })
			submitted = await(freshDone)
		}
		listingOnce.Do(func() { close(finishListing) })
		reconciled := await(legacyDone)
		if !submissionFirst {
			acceptanceOnce.Do(func() { close(finishAcceptance) })
			submitted = await(freshDone)
		}
		workers.Wait()
		if reconciled.err != nil || reconciled.reconcile == nil || reconciled.reconcile.RefusedCount != 1 || reconciled.reconcile.FoundCount != 0 || reconciled.reconcile.AbsentCount != 0 {
			t.Fatalf("stale destination snapshot decided legacy debt: %+v %v", reconciled.reconcile, reconciled.err)
		}
		if (submitted.err != nil) != loseAcknowledgement {
			t.Fatal("fresh attempt lost its original acceptance outcome", submitted.err)
		}
		requireLegacyAttemptHeld(t, legacyOwner, legacy, legacyKey)
		stored, err := model.GetPayment(ctx, fresh.PaymentId)
		if err != nil || stored.CircleIdempotencyKey == nil || stored.Completed || stored.Canceled {
			t.Fatal("newer request lost its retained attempt", stored, err)
		}
		if loseAcknowledgement {
			if stored.PaymentRecord != nil {
				t.Fatal("lost acknowledgement invented a record", stored)
			}
			if _, err := model.GetRetainedProviderPaymentRequest(ctx, stored); err != nil {
				t.Fatal("lost acknowledgement lost exact retry authority", err)
			}
		} else {
			if stored.PaymentRecord == nil || *stored.PaymentRecord != record {
				t.Fatal("accepted processor record did not retain its actual owner", stored)
			}
			client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
				return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "COMPLETE", TxHash: "synthetic-owned-record-hash"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
			}
			for range 2 {
				advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: fresh.PaymentId}, freshOwner)
				if err != nil || !advanced.Complete {
					t.Fatal("real record owner did not complete once", advanced, err)
				}
			}
		}
		if balance := model.GetAccountBalance(legacyOwner); balance.Balance.PaidByteCount != 0 {
			t.Fatal("one processor transfer completed the other debt", balance)
		}
	})
}

// The legacy write reaches its decision while the actual acceptance is held.
func TestProviderPaymentAttributionLegacyFirst(t *testing.T) {
	paymentAttributionTestRace(t, false, false, false)
}

// The normal acceptance commits before the already-started listing returns.
func TestProviderPaymentAttributionSubmissionFirst(t *testing.T) {
	paymentAttributionTestRace(t, true, false, false)
}

// No acknowledgement is required for a retained request to block attribution.
func TestProviderPaymentAttributionLostAcknowledgement(t *testing.T) {
	paymentAttributionTestRace(t, true, true, false)
}

// A newly retained request also invalidates an older affirmative-absence write.
func TestProviderPaymentAttributionNewRequestPreventsAbsence(t *testing.T) {
	paymentAttributionTestRace(t, false, true, true)
}

// Resetting the current marker does not make its accepted record unowned.
func TestProviderPaymentAttributionClearedRecordRemainsOwned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		legacyOwner, legacy, key := legacyReconcileTestFixture(t)
		owner, payment := controllerPayoutFixture(t, time.Date(2020, 1, 6, 23, 59, 59, 0, time.UTC))
		const record = "synthetic-historical-accepted-record"
		_, _ = controllerRetainAttempt(t, owner, payment, record)
		if err := model.RemovePaymentRecord(owner.Ctx, payment.PaymentId); err != nil {
			t.Fatal(err)
		}
		if err := model.BindLegacyProviderPaymentRecord(legacyOwner.Ctx, legacy, record, 9.99, `{"record":"synthetic-historical-accepted-record"}`); !errors.Is(err, model.ErrProviderPaymentAttemptChanged) {
			t.Fatal("cleared accepted record was attributed to a different payment", err)
		}
		requireLegacyAttemptHeld(t, legacyOwner, legacy, key)
		fresh, err := model.GetPayment(owner.Ctx, payment.PaymentId)
		if err != nil {
			t.Fatal(err)
		}
		wallet := model.GetAccountWallet(owner.Ctx, *fresh.WalletId)
		request, err := model.ReserveProviderPaymentRequest(owner.Ctx, fresh, wallet, 9.99, "MATIC")
		if err != nil {
			t.Fatal(err)
		}
		if err := model.SetProviderPaymentRecord(owner.Ctx, &request.Basis, request.Amount, record); !errors.Is(err, model.ErrProviderPaymentAttemptChanged) {
			t.Fatal("new attempt reused its predecessor's processor record", err)
		}
	})
}

// Historical marker resets did not always journal the populated current row.
// The original ACCEPTED response itself must retain the processor id's owner.
func TestProviderPaymentAttributionHistoricalAcceptanceSurvivesReset(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		legacyOwner, legacy, key := legacyReconcileTestFixture(t)
		owner, payment := controllerPayoutFixture(t, time.Date(2020, 1, 6, 23, 59, 59, 0, time.UTC))
		const record = "synthetic-original-acceptance-only"
		_, _ = controllerRetainAttempt(t, owner, payment, record)
		// Exact old stored shape, without adding a new REMOVED outcome.
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET payment_record=NULL,circle_idempotency_key=NULL WHERE payment_id=$1`, payment.PaymentId))
		})
		records, err := model.GetProviderPaymentProcessorRecords(owner.Ctx)
		if err != nil || !records[record] {
			t.Fatal("original acceptance disappeared with the current marker", records, err)
		}
		if err := model.BindLegacyProviderPaymentRecord(legacyOwner.Ctx, legacy, record, 9.99, `{"record":"synthetic-original-acceptance-only"}`); !errors.Is(err, model.ErrProviderPaymentAttemptChanged) {
			t.Fatal("historically accepted transfer acquired another owner", err)
		}
		requireLegacyAttemptHeld(t, legacyOwner, legacy, key)
	})
}

// Historical duplicate markers cannot earn two completions through the normal
// status path; no automatic rewrite guesses which old obligation owned them.
func TestProviderPaymentAttributionStatusRefusesHistoricalDuplicate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		legacyOwner, legacy, _ := legacyReconcileTestFixture(t)
		owner, payment := controllerPayoutFixture(t, time.Date(2020, 1, 6, 23, 59, 59, 0, time.UTC))
		const record = "synthetic-historical-duplicate"
		_, _ = controllerRetainAttempt(t, owner, payment, record)
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `UPDATE account_payment SET payment_record=$2,token_amount=9.99,token_type='USDC' WHERE payment_id=$1`, legacy.PaymentId, record))
		})
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			return nil, errors.New("historical duplicate must never submit")
		})
		client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
			return &GetTransactionResult{Transaction: CircleTransaction{Id: id, State: "COMPLETE", TxHash: "synthetic-duplicate-chain-hash"}, ResponseBodyBytes: []byte(`{"state":"COMPLETE"}`)}, nil
		}
		for _, testCase := range []struct {
			owner   *session.ClientSession
			payment *model.AccountPayment
		}{{legacyOwner, legacy}, {owner, payment}} {
			if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: testCase.payment.PaymentId}, testCase.owner); !errors.Is(err, model.ErrProviderPaymentAttemptChanged) {
				t.Fatal("historical duplicate was completed", err)
			}
			stored, err := model.GetPayment(testCase.owner.Ctx, testCase.payment.PaymentId)
			if err != nil || stored.Completed || !stored.AttributionReviewRequired ||
				model.GetAccountBalance(testCase.owner).Balance.PaidByteCount != 0 {
				t.Fatal("duplicate status changed paid ownership", stored, err)
			}
		}
	})
}

// A positive real-ownership wait proves reservation cannot cross a legacy
// decision's fresh read/write boundary; a second record bind then still refuses.
func TestProviderPaymentAttributionRetentionWaitsForLegacyOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		_, legacy, _ := legacyReconcileTestFixture(t)
		owner, payment := controllerPayoutFixture(t, time.Date(2020, 1, 6, 23, 59, 59, 0, time.UTC))
		wallet := model.GetAccountWallet(owner.Ctx, *payment.WalletId)
		ctx, cancel := context.WithTimeout(owner.Ctx, 30*time.Second)
		defer cancel()
		admitted, release, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
		var admitOnce, releaseOnce sync.Once
		var workers sync.WaitGroup
		t.Cleanup(func() {
			cancel()
			releaseOnce.Do(func() { close(release) })
			joined := make(chan struct{})
			go func() { workers.Wait(); close(joined) }()
			select {
			case <-joined:
			case <-time.After(30 * time.Second):
				t.Error("retention ownership workers did not join")
			}
		})
		legacyCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipAdmitted {
				admitOnce.Do(func() { close(admitted) })
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		})
		reserveCtx := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind == server.PgOwnershipWaiting {
				select {
				case waiting <- struct{}{}:
				default:
				}
			}
		})
		legacyDone, requestDone := make(chan error, 1), make(chan error, 1)
		var request *model.ProviderPaymentRequest
		workers.Go(func() {
			var err error
			if panicValue := server.HandleError(func() {
				err = model.BindLegacyProviderPaymentRecord(legacyCtx, legacy, "synthetic-owner-fenced-record", 9.99, `{"record":"synthetic-owner-fenced-record"}`)
			}); panicValue != nil {
				err = fmt.Errorf("legacy owner panic: %v", panicValue)
			}
			legacyDone <- err
		})
		paymentAttributionTestWait(t, ctx, admitted)
		workers.Go(func() {
			var err error
			if panicValue := server.HandleError(func() {
				request, err = model.ReserveProviderPaymentRequest(reserveCtx, payment, wallet, 9.99, "MATIC")
			}); panicValue != nil {
				err = fmt.Errorf("request owner panic: %v", panicValue)
			}
			requestDone <- err
		})
		select {
		case <-waiting:
		case err := <-requestDone:
			t.Fatal("retained request crossed the held destination owner", err)
		case <-ctx.Done():
			t.Fatal("retainer did not reach the shared ownership barrier", ctx.Err())
		}
		releaseOnce.Do(func() { close(release) })
		for _, done := range []<-chan error{legacyDone, requestDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("released attribution owner did not finish", ctx.Err())
			}
		}
		workers.Wait()
		if request == nil {
			t.Fatal("newer reservation did not retain its request")
		}
		if err := model.SetProviderPaymentRecord(owner.Ctx, &request.Basis, request.Amount, "synthetic-owner-fenced-record"); !errors.Is(err, model.ErrProviderPaymentAttemptChanged) {
			t.Fatal("normal acceptance double-bound the legacy-owned record", err)
		}
	})
}
