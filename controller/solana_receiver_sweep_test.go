package controller

// The Solana reconcile's receiver leg (solana_receiver_sweep.go), hermetic: the
// RPC is the fake of solana_receiver_fake_test.go, and the cursor, the
// on-record check, the decision's lookups, the credit and the record are
// seams. The database-backed root-cause test is in
// solana_receiver_sweep_db_test.go.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The receiver, its token account and the payers of these tests.
const (
	solanaReceiverTestReceiver      = "ReceiverWa11et1111111111111111111111111111"
	solanaReceiverTestTokenAccount  = "ReceiverTokenAccount11111111111111111111111"
	solanaReceiverTestPayer         = "PayerWa11et22222222222222222222222222222222"
	solanaReceiverTestPayerToken    = "PayerToken222222222222222222222222222222222"
	solanaReceiverTestReference     = "Reference3333333333333333333333333333333333"
	solanaReceiverTestTreasury      = "TreasuryWa11et4444444444444444444444444444"
	solanaReceiverTestTreasuryToken = "TreasuryToken444444444444444444444444444444"
)

// The run's clock.
var solanaReceiverTestNow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// The receiver leg's seams on top of the chain leg's (credit, record,
// lookups, events), with the leg's settings for the runs it makes.
type solanaReceiverLegTest struct {
	*solanaChainLegTest
	rpc                 *fakeSolanaReceiverRpc
	settings            *SolanaReceiverSweepSettings
	tokenAccountCursors map[string]*model.SolanaReceiverSweepCursor
	saveCount           int
	onRecordSignatures  map[string]bool
}

// Installs the seams for one test; its cleanup restores them.
func newSolanaReceiverLegTest(t *testing.T) *solanaReceiverLegTest {
	test := &solanaReceiverLegTest{
		solanaChainLegTest:  newSolanaChainLegTest(t),
		rpc:                 newFakeSolanaReceiverRpc(t),
		settings:            DefaultSolanaReceiverSweepSettings(),
		tokenAccountCursors: map[string]*model.SolanaReceiverSweepCursor{},
		onRecordSignatures:  map[string]bool{},
	}
	test.rpc.ownerTokenAccounts[solanaReceiverTestReceiver] = []string{solanaReceiverTestTokenAccount}
	previousRpcFor := solanaChainRpcFor
	previousCursor := solanaReceiverSweepCursor
	previousSave := solanaReceiverSweepSaveCursor
	previousOnRecord := solanaReceiverSweepOnRecord
	solanaReceiverAddresses = []string{solanaReceiverTestReceiver}
	solanaChainRpcFor = func(rpcUrl string) solanaChainRpc { return test.rpc }
	solanaReceiverSweepCursor = func(connOwner server.PgConn, ctx context.Context, tokenAccount string) *model.SolanaReceiverSweepCursor {
		if cursor, ok := test.tokenAccountCursors[tokenAccount]; ok {
			copied := *cursor
			return &copied
		}
		return nil
	}
	solanaReceiverSweepSaveCursor = func(connOwner server.PgConn, ctx context.Context, cursor *model.SolanaReceiverSweepCursor) {
		test.saveCount += 1
		copied := *cursor
		test.tokenAccountCursors[cursor.TokenAccount] = &copied
	}
	solanaReceiverSweepOnRecord = func(connOwner server.PgConn, ctx context.Context, signatures []string) map[string]bool {
		onRecordSignatures := map[string]bool{}
		for _, signature := range signatures {
			if test.onRecordSignatures[signature] {
				onRecordSignatures[signature] = true
			}
		}
		return onRecordSignatures
	}
	// a record written is on the record from then on, as in the database
	solanaChainRecord = func(connOwner server.PgConn, ctx context.Context, payment *model.UnfulfilledSolanaPayment) error {
		test.records = append(test.records, payment)
		test.onRecordSignatures[payment.TxSignature] = true
		return nil
	}
	t.Cleanup(func() {
		solanaChainRpcFor = previousRpcFor
		solanaReceiverSweepCursor = previousCursor
		solanaReceiverSweepSaveCursor = previousSave
		solanaReceiverSweepOnRecord = previousOnRecord
	})
	return test
}

// A reconcile run at the test clock with the test's settings.
func (self *solanaReceiverLegTest) run(dryRun bool) *paymentReconcileRun {
	run := solanaChainLegTestRun(dryRun)
	run.now = solanaReceiverTestNow
	run.solanaReceiverSweepSettings = self.settings
	return run
}

// A USDC transfer from the test payer into the receiver, age before the test
// clock.
func (self *solanaReceiverLegTest) payment(signature string, slot uint64, age time.Duration, amountMicro int64, references ...string) *solanaReceiverFakeTransaction {
	return solanaReceiverFakeTransfer(
		signature, slot, solanaReceiverTestNow.Add(-age),
		solanaReceiverTestPayer, solanaReceiverTestPayerToken,
		solanaReceiverTestReceiver, solanaReceiverTestTokenAccount,
		amountMicro, references...,
	)
}

// Sets the token account's transactions, newest first.
func (self *solanaReceiverLegTest) history(transactions ...*solanaReceiverFakeTransaction) {
	self.rpc.tokenAccountHistories[solanaReceiverTestTokenAccount] = transactions
}

// The signature the token account's cursor stands at, "" with none.
func (self *solanaReceiverLegTest) cursorSignature() string {
	if cursor, ok := self.tokenAccountCursors[solanaReceiverTestTokenAccount]; ok {
		return cursor.Signature
	}
	return ""
}

// The root cause: a USDC payment into our receiver that no intent, renewal,
// balance or record names (its intent was swept before the lost delivery
// could be credited) is recorded once, as no_intent with its sender, amount
// and on-chain time, and never credited. A payment a record already names is
// not even read. A second run reads nothing again and records nothing more.
func TestSolanaReceiverLegRecordsAnUncreditedPaymentOnceAndNeverCreditsIt(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	test.history(
		test.payment("lost", 3, 2*time.Hour, 40_000_000, "SweptReference"),
		test.payment("credited", 2, 3*time.Hour, 40_000_000, "PaidReference"),
	)
	test.onRecordSignatures["credited"] = true

	run := test.run(false)
	reconcileSolanaReceivers(run)
	connect.AssertEqual(t, len(test.credits), 0)
	connect.AssertEqual(t, len(test.records), 1)
	record := test.records[0]
	connect.AssertEqual(t, record.TxSignature, "lost")
	connect.AssertEqual(t, record.Reason, model.SolanaUnfulfilledReasonNoIntent)
	connect.AssertEqual(t, record.TokenAmountUsd, 40.0)
	connect.AssertEqual(t, *record.SenderAccount, solanaReceiverTestPayer)
	connect.AssertEqual(t, record.TransactionTime.Equal(solanaReceiverTestNow.Add(-2*time.Hour)), true)
	connect.AssertEqual(t, record.NetworkId, (*server.Id)(nil))
	// the reference candidates are kept, so the first leg credits the
	// record through its intent's one-shot if one ever resolves
	found := false
	for _, candidate := range record.ReferenceCandidates {
		found = found || candidate == "SweptReference"
	}
	connect.AssertEqual(t, found, true)
	// only the uncredited payment was read
	connect.AssertEqual(t, test.rpc.readSignatures, []string{"lost"})
	connect.AssertEqual(t, test.cursorSignature(), "lost")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Examined, 1)
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 0)

	again := test.run(false)
	reconcileSolanaReceivers(again)
	connect.AssertEqual(t, len(test.records), 1)
	connect.AssertEqual(t, len(test.credits), 0)
	connect.AssertEqual(t, test.rpc.readSignatures, []string{"lost"})
	connect.AssertEqual(t, again.storeResult(model.SubscriptionMarketSolana).Examined, 0)

	// with the cursor lost, the record itself keeps it from a second record
	delete(test.tokenAccountCursors, solanaReceiverTestTokenAccount)
	test.settings.SweepFrom = solanaReceiverTestNow.Add(-24 * time.Hour)
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.records), 1)
	connect.AssertEqual(t, test.rpc.readSignatures, []string{"lost"})
}

// A payment that an open intent's reference names is credited, through the
// intent's one-shot (the webhook's own decision), as the delivery would have
// been; a dry run says it would and writes nothing, its cursor included.
func TestSolanaReceiverLegCreditsOnlyThroughAnIntent(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	networkId := server.NewId()
	test.lookups.intents[solanaReceiverTestReference] = &model.PaymentIntentSearchResult{
		NetworkId:         &networkId,
		PaymentReference:  solanaReceiverTestReference,
		ExpectedAmountUsd: 40,
		SubscriptionPlan:  model.SolanaPlanYearly,
	}
	test.history(test.payment("paid", 2, time.Hour, 40_000_000, solanaReceiverTestReference))

	dry := test.run(true)
	reconcileSolanaReceivers(dry)
	connect.AssertEqual(t, len(test.credits), 0)
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, test.saveCount, 0)
	connect.AssertEqual(t, len(test.events), 1)
	connect.AssertEqual(t, test.events[0].Action, model.PaymentReconcileActionWouldCredit)
	connect.AssertEqual(t, test.events[0].Details["leg"], solanaReceiverLegName)

	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, test.credits, []string{fmt.Sprintf("%s paid %.3f", solanaReceiverTestReference, 40.0)})
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, test.onboarded, 1)
	connect.AssertEqual(t, test.events[len(test.events)-1].Action, model.PaymentReconcileActionCredited)
	connect.AssertEqual(t, test.events[len(test.events)-1].Details["leg"], solanaReceiverLegName)
	connect.AssertEqual(t, test.cursorSignature(), "paid")
}

// Not examined, but passed: a failed transaction (never read), an outgoing
// transfer and dust under DustUsd (read once, nothing done). A transaction
// younger than MinAge stops the read for this run; the next run past its age
// examines it.
func TestSolanaReceiverLegPassesDustOutgoingAndFailedAndWaitsForYoung(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	failed := test.payment("failed", 4, 4*time.Hour, 40_000_000)
	failed.failed = true
	young := test.payment("young", 6, 10*time.Minute, 40_000_000)
	test.history(
		young,
		// a transfer out of the receiver
		solanaReceiverFakeTransfer("outgoing", 5, solanaReceiverTestNow.Add(-time.Hour),
			solanaReceiverTestReceiver, solanaReceiverTestTokenAccount,
			solanaReceiverTestTreasury, solanaReceiverTestTreasuryToken, 100_000_000),
		failed,
		test.payment("dust", 3, 5*time.Hour, 10_000),
	)

	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, len(test.credits), 0)
	connect.AssertEqual(t, test.rpc.readSignatures, []string{"dust", "outgoing"})
	connect.AssertEqual(t, test.cursorSignature(), "outgoing")

	later := test.run(false)
	later.now = solanaReceiverTestNow.Add(time.Hour)
	reconcileSolanaReceivers(later)
	connect.AssertEqual(t, len(test.records), 1)
	connect.AssertEqual(t, test.records[0].TxSignature, "young")
	connect.AssertEqual(t, test.cursorSignature(), "young")
}

// A token account with no cursor is read back to SweepFrom only: its earlier
// history was swept by hand. The cursor starts at the newest signature before
// SweepFrom, so the next run lists from there.
func TestSolanaReceiverLegFirstRunStartsAtSweepFrom(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	test.settings.SweepFrom = solanaReceiverTestNow.Add(-6 * time.Hour)
	test.history(
		test.payment("after", 2, 3*time.Hour, 40_000_000),
		test.payment("before", 1, 30*24*time.Hour, 40_000_000),
	)
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.records), 1)
	connect.AssertEqual(t, test.records[0].TxSignature, "after")
	connect.AssertEqual(t, test.rpc.readSignatures, []string{"after"})

	// only the old history: the cursor starts before sweep_from
	fresh := newSolanaReceiverLegTest(t)
	fresh.settings.SweepFrom = solanaReceiverTestNow.Add(-6 * time.Hour)
	fresh.history(fresh.payment("before", 1, 30*24*time.Hour, 40_000_000))
	reconcileSolanaReceivers(fresh.run(false))
	connect.AssertEqual(t, len(fresh.records), 0)
	connect.AssertEqual(t, len(fresh.rpc.readSignatures), 0)
	connect.AssertEqual(t, fresh.cursorSignature(), "before")
}

// A failure keeps the cursor before the transaction it failed on, and is an
// error event (which holds the store's watermark): a getTransaction that
// fails, a record that cannot be written. The next run reads the same
// transaction again and records it once.
func TestSolanaReceiverLegKeepsItsCursorOnFailure(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	test.history(
		test.payment("second", 3, 2*time.Hour, 40_000_000),
		test.payment("first", 2, 3*time.Hour, 40_000_000),
	)

	// the RPC fails on the first read
	test.rpc.methodErrs["getTransaction"] = errors.New("getTransaction: http status 503")
	run := test.run(false)
	reconcileSolanaReceivers(run)
	connect.AssertEqual(t, len(test.records), 0)
	connect.AssertEqual(t, test.cursorSignature(), "")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 1)
	connect.AssertEqual(t, test.events[len(test.events)-1].Details["leg"], solanaReceiverLegName)

	// the record fails on the second payment: the cursor stays at the first
	delete(test.rpc.methodErrs, "getTransaction")
	recordFailures := map[string]bool{"second": true}
	solanaChainRecord = func(connOwner server.PgConn, ctx context.Context, payment *model.UnfulfilledSolanaPayment) error {
		if recordFailures[payment.TxSignature] {
			return errors.New("synthetic record failure")
		}
		test.records = append(test.records, payment)
		test.onRecordSignatures[payment.TxSignature] = true
		return nil
	}
	run = test.run(false)
	reconcileSolanaReceivers(run)
	connect.AssertEqual(t, len(test.records), 1)
	connect.AssertEqual(t, test.records[0].TxSignature, "first")
	connect.AssertEqual(t, test.cursorSignature(), "first")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 1)

	// the write works again: the second payment is recorded, once
	delete(recordFailures, "second")
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.records), 2)
	connect.AssertEqual(t, test.records[1].TxSignature, "second")
	connect.AssertEqual(t, test.cursorSignature(), "second")
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.records), 2)
}

// The leg's call cap and the store's budget stop it where it is, with its
// cursor at the last transaction examined, and no error: the next run goes
// on. More history since the cursor than the page limit reads is an error.
func TestSolanaReceiverLegStopsAtItsCallCapAndPageLimit(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	test.history(
		test.payment("third", 4, 2*time.Hour, 40_000_000),
		test.payment("second", 3, 3*time.Hour, 40_000_000),
		test.payment("first", 2, 4*time.Hour, 40_000_000),
	)
	// token accounts, one page, one transaction
	test.settings.MaxCallsPerRun = 3
	run := test.run(false)
	reconcileSolanaReceivers(run)
	connect.AssertEqual(t, len(test.records), 1)
	connect.AssertEqual(t, test.cursorSignature(), "first")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 0)
	connect.AssertEqual(t, run.storeDetail(model.SubscriptionMarketSolana)["receiver_calls_exhausted"], true)

	test.settings.MaxCallsPerRun = 100
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.records), 3)
	connect.AssertEqual(t, test.cursorSignature(), "third")

	// two pages of history past the cursor with a limit of one page
	paged := newSolanaReceiverLegTest(t)
	paged.settings.PageSize = 1
	paged.settings.MaxPages = 1
	paged.history(
		paged.payment("b", 3, 2*time.Hour, 40_000_000),
		paged.payment("a", 2, 3*time.Hour, 40_000_000),
	)
	run = paged.run(false)
	reconcileSolanaReceivers(run)
	connect.AssertEqual(t, len(paged.records), 0)
	connect.AssertEqual(t, paged.cursorSignature(), "")
	connect.AssertEqual(t, run.storeResult(model.SubscriptionMarketSolana).Errors, 1)
}

// With no Helius key nothing is read.
func TestSolanaReceiverLegWithoutAKeyReadsNothing(t *testing.T) {
	test := newSolanaReceiverLegTest(t)
	test.history(test.payment("lost", 2, 2*time.Hour, 40_000_000))
	solanaChainRpcUrl = func() (string, bool) { return "", false }
	reconcileSolanaReceivers(test.run(false))
	connect.AssertEqual(t, len(test.rpc.methodCalls), 0)
	connect.AssertEqual(t, len(test.records), 0)
}
