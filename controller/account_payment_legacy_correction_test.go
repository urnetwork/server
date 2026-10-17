// Inflated legacy keyed payments are corrected by replacement, never by an
// in-place update: the basis guard refuses that, and the replacement must be
// paid exactly once while the original's key is never used again. The Circle
// lister and the Circle client are fakes owned by each test; every identifier
// and address is synthetic.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func legacyCorrectionTestPolicy() *model.LegacyProviderPaymentCorrectionPolicy {
	return &model.LegacyProviderPaymentCorrectionPolicy{
		MinPayoutUsd:                   4000,
		UsdPerActiveUser:               0.1,
		SubscriptionNetRevenueFraction: 0.2,
		ReliabilityPeriod:              30 * 24 * time.Hour,
		TransferFee:                    model.UsdToNanoCents(0.01),
	}
}

// The synthetic identities of a legacy correction fixture.
type legacyCorrectionTestIdentity struct {
	network       server.Id
	client        server.Id
	sourceNetwork server.Id
	source        server.Id
	balance       server.Id
	closed        time.Time
}

// A legacy attempt of a plan whose 4-day subsidy epoch took the revenue term
// (0.2 x $50,000 above the $4,000 floor), owning sweepCount settled contracts
// closed before the cutoff, and holding a legacy key with no retained request.
func legacyCorrectionTestFixture(t testing.TB, sweepCount int, subsidy model.NanoCents, reliability model.NanoCents) (*session.ClientSession, *model.AccountPayment, server.Id) {
	t.Helper()
	owner, payment, legacyKey, _ := legacyCorrectionTestFixtureWithIdentity(t, sweepCount, subsidy, reliability)
	return owner, payment, legacyKey
}

func legacyCorrectionTestFixtureWithIdentity(t testing.TB, sweepCount int, subsidy model.NanoCents, reliability model.NanoCents) (*session.ClientSession, *model.AccountPayment, server.Id, *legacyCorrectionTestIdentity) {
	t.Helper()
	return legacyCorrectionTestFixtureWithBytes(t, sweepCount, 100, subsidy, reliability)
}

func legacyCorrectionTestFixtureWithBytes(t testing.TB, sweepCount int, sweepByteCount int64, subsidy model.NanoCents, reliability model.NanoCents, sweepRevenue ...model.NanoCents) (*session.ClientSession, *model.AccountPayment, server.Id, *legacyCorrectionTestIdentity) {
	t.Helper()
	ctx := context.Background()
	cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
	controllerPayoutSchedule(t, cutoff)
	network, client, sourceNetwork, source := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	testingCreatePaymentClient(ctx, network, client)
	testingCreatePaymentClient(ctx, sourceNetwork, source)
	owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: network, ClientId: &client})
	t.Cleanup(owner.Cancel)
	wallet := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: network, Blockchain: "MATIC", WalletAddress: "0x0000000000000000000000000000000000000012", DefaultTokenType: "USDC"})
	if wallet == nil {
		t.Fatal("missing fixture wallet")
	}
	if err := model.SetPayoutWallet(ctx, network, *wallet); err != nil {
		t.Fatal(err)
	}
	code, err := model.CreateBalanceCode(ctx, 1024*1024, 365*24*time.Hour, model.UsdToNanoCents(100), "correction-"+server.NewId().String(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	testingRedeemPaymentBalanceCode(t, ctx, sourceNetwork, code.Secret)
	balances := model.GetActiveTransferBalances(ctx, sourceNetwork)
	if len(balances) != 1 {
		t.Fatal("missing fixture balance")
	}
	paymentId, planId := server.NewId(), server.NewId()
	closed := cutoff.Add(-time.Hour)
	epochEnd := cutoff.Add(-2 * time.Hour)
	usage := fmt.Sprintf(`{"version":1,"byte_count":%d,"providers":[{"client_id":%q,"network_id":%q,"byte_count":%d}]}`, sweepByteCount, client.String(), network.String(), sweepByteCount)
	var revenue model.NanoCents
	if len(sweepRevenue) == 1 {
		revenue = sweepRevenue[0]
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for range sweepCount {
			contract := server.NewId()
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,'settled',$9,true)`, contract, sourceNetwork, source, network, client, sweepByteCount, closed.Add(-time.Hour), closed, usage))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, contract, balances[0].BalanceId, network, client, sweepByteCount, revenue, closed, paymentId))
		}
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,payout_nano_cents,subsidy_payout_nano_cents,reliability_subsidy_nano_cents,min_sweep_time)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, paymentId, planId, network, *wallet, sweepByteCount*int64(sweepCount), subsidy+reliability+int64(sweepCount)*revenue, subsidy, reliability, closed))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO subsidy_payment(payment_plan_id,start_time,end_time,active_user_count,paid_user_count,net_payout_byte_count_paid,net_payout_byte_count_unpaid,net_revenue_nano_cents,net_payout_nano_cents)
			VALUES($1,$2,$3,100,10,0,0,$4,$5)`, planId, epochEnd.Add(-4*24*time.Hour), epochEnd, model.UsdToNanoCents(50000), subsidy))
	})
	legacyKey := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, legacyKey))
	})
	payment, err := model.GetPayment(ctx, paymentId)
	if err != nil || payment == nil || payment.CircleIdempotencyKey == nil || *payment.CircleIdempotencyKey != legacyKey {
		t.Fatal("legacy correction fixture lost its key", payment, err)
	}
	identity := &legacyCorrectionTestIdentity{network: network, client: client, sourceNetwork: sourceNetwork, source: source, balance: balances[0].BalanceId, closed: closed}
	return owner, payment, legacyKey, identity
}

func legacyCorrectionTestRun(t testing.TB, owner *session.ClientSession, lister *legacyReconcileTestLister, dryRun bool, anchor bool, batchSize int, batchLimit int) *LegacyProviderPaymentCorrectionResult {
	t.Helper()
	settings := &LegacyProviderPaymentCorrectionSettings{
		DryRun:          dryRun,
		AnchorSweeps:    anchor,
		SweepBatchSize:  batchSize,
		SweepBatchLimit: batchLimit,
		CountSweeps:     true,
		Policy:          legacyCorrectionTestPolicy(),
	}
	// the fixture payment was planned now; observe it past the minimum age
	later := func() time.Time { return server.NowUtc().Add(2 * legacyProviderPaymentMinAge) }
	result, err := correctLegacyProviderPaymentAttempts(owner, settings, lister, legacyReconcileTestPayoutWalletId, later)
	if err != nil {
		t.Fatal("correction failed", err)
	}
	return result
}

// The sweep identities a payment owns, sorted, and their bytes.
func legacyCorrectionTestSweeps(t testing.TB, ctx context.Context, paymentId server.Id) (sweepKeys []string, byteCount int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT contract_id, balance_id, network_id, payout_byte_count FROM transfer_escrow_sweep WHERE payment_id=$1`, paymentId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId, balanceId, networkId server.Id
				var bytes int64
				server.Raise(result.Scan(&contractId, &balanceId, &networkId, &bytes))
				sweepKeys = append(sweepKeys, contractId.String()+"/"+balanceId.String()+"/"+networkId.String())
				byteCount += bytes
			}
		})
	})
	slices.Sort(sweepKeys)
	return
}

func legacyCorrectionTestEventCount(t testing.TB, ctx context.Context, eventType string) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM audit_account_payment WHERE event_type=$1`, eventType).Scan(&count))
	})
	return
}

// The single journaled replacement and its payment.
func legacyCorrectionTestReplacement(t testing.TB, ctx context.Context, original server.Id) *model.AccountPayment {
	t.Helper()
	replacements, err := model.GetLegacyProviderPaymentReplacements(ctx)
	if err != nil || len(replacements) != 1 || replacements[0].OriginalPaymentId != original {
		t.Fatal("missing the journaled replacement", replacements, err)
	}
	replacement, err := model.GetPayment(ctx, replacements[0].ReplacementPaymentId)
	if err != nil || replacement == nil {
		t.Fatal("missing the replacement payment", err)
	}
	return replacement
}

// Captures the error a guarded statement raises, rolled back with its transaction.
func legacyCorrectionTestExec(ctx context.Context, sql string, args ...any) (returnErr error) {
	defer func() {
		if value := recover(); value != nil {
			if err, ok := value.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", value)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, sql, args...))
	})
	return nil
}

type legacyCorrectionTestSend struct {
	key    server.Id
	amount float64
}

func legacyCorrectionTestMock(t testing.TB, legacyKey server.Id, sends *[]legacyCorrectionTestSend) {
	controllerPayoutMock(t, func(ctx context.Context, key server.Id, amount float64, address, network string) (*CreateTransferTransactionResult, error) {
		if err := requireCircleProviderPayment(ctx); err != nil {
			return nil, err
		}
		if key == legacyKey {
			t.Fatal("a legacy key was resubmitted")
		}
		*sends = append(*sends, legacyCorrectionTestSend{key: key, amount: amount})
		return &CreateTransferTransactionResult{Id: "synthetic-replacement-transfer", State: "INITIATED"}, nil
	})
}

// The root cause of the refused correction: the basis guard freezes a keyed
// payment's amounts, and an unkeyed payout changes only by an exact bonus.
func TestLegacyProviderPaymentCorrectionInPlaceUpdateIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, _ := legacyCorrectionTestFixture(t, 1, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		err := legacyCorrectionTestExec(owner.Ctx, `UPDATE account_payment SET payout_nano_cents=$2, subsidy_payout_nano_cents=$3, reliability_subsidy_nano_cents=$4 WHERE payment_id=$1`,
			original.PaymentId, model.UsdToNanoCents(0.44), model.UsdToNanoCents(0.4), model.UsdToNanoCents(0.04))
		if err == nil || !strings.Contains(err.Error(), "provider payment submission basis is immutable") {
			t.Fatal("an in-place correction of a keyed payment was not refused", err)
		}
		// without its key, a reduction still has no bonus provenance
		if err := legacyCorrectionTestExec(owner.Ctx, `UPDATE account_payment SET circle_idempotency_key=NULL WHERE payment_id=$1`, original.PaymentId); err != nil {
			t.Fatal(err)
		}
		err = legacyCorrectionTestExec(owner.Ctx, `UPDATE account_payment SET payout_nano_cents=$2, subsidy_payout_nano_cents=$3, reliability_subsidy_nano_cents=$4 WHERE payment_id=$1`,
			original.PaymentId, model.UsdToNanoCents(0.44), model.UsdToNanoCents(0.4), model.UsdToNanoCents(0.04))
		if err == nil || !strings.Contains(err.Error(), "lacks exact bonus provenance") {
			t.Fatal("an in-place reduction was not refused", err)
		}
		unchanged, err := model.GetPayment(owner.Ctx, original.PaymentId)
		if err != nil || unchanged.Payout != original.Payout || unchanged.SubsidyPayout != original.SubsidyPayout || unchanged.ReliabilitySubsidy != original.ReliabilitySubsidy {
			t.Fatal("a refused correction changed the payment", unchanged, err)
		}
	})
}

func TestLegacyProviderPaymentCorrectionFullMoveReplacesAndPaysOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey := legacyCorrectionTestFixture(t, 5, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		sweepKeys, byteCount := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		if len(sweepKeys) != 5 || byteCount != 500 {
			t.Fatal("fixture sweeps", len(sweepKeys), byteCount)
		}
		lister := &legacyReconcileTestLister{}

		preview := legacyCorrectionTestRun(t, owner, lister, true, false, 2, 0)
		if preview.AttemptCount != 1 || preview.AbsentCount != 1 || preview.CorrectionCount != 1 || preview.ReplacedCount != 0 ||
			preview.OriginalPayout != model.UsdToNanoCents(1.3) || preview.CorrectedPayout != model.UsdToNanoCents(0.44) ||
			preview.SubsidyReduction != model.UsdToNanoCents(0.6) || preview.ReliabilityReduction != model.UsdToNanoCents(0.26) ||
			preview.SweepCount != 5 || preview.DustCount != 0 || lister.calls != 1 {
			t.Fatalf("dry run did not report the correction: %+v", preview)
		}
		requireLegacyAttemptHeld(t, owner, original, legacyKey)
		if n := legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_replacement"); n != 0 {
			t.Fatal("dry run journaled a replacement", n)
		}

		result := legacyCorrectionTestRun(t, owner, lister, false, false, 2, 0)
		if result.ReplacedCount != 1 || result.RefusedCount != 0 || result.MovedSweepCount != 5 || result.MovingCount != 0 ||
			result.ReleasedCount != 1 || result.DustHeldCount != 0 || result.ScheduledCount != 1 {
			t.Fatalf("correction was not applied: %+v", result)
		}

		canceled, err := model.GetPayment(owner.Ctx, original.PaymentId)
		if err != nil || !canceled.Canceled || canceled.Completed || canceled.CircleIdempotencyKey == nil || *canceled.CircleIdempotencyKey != legacyKey ||
			canceled.PaymentRecord != nil || canceled.Payout != original.Payout || canceled.SubsidyPayout != original.SubsidyPayout {
			t.Fatal("original was not canceled with its key and amounts retained", canceled, err)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if replacement.Payout != model.UsdToNanoCents(0.44) || replacement.SubsidyPayout != model.UsdToNanoCents(0.4) ||
			replacement.ReliabilitySubsidy != model.UsdToNanoCents(0.04) || replacement.BonusPayout != 0 ||
			replacement.PaymentPlanId != original.PaymentPlanId || replacement.NetworkId != original.NetworkId ||
			replacement.WalletId == nil || *replacement.WalletId != *original.WalletId || replacement.PayoutByteCount != original.PayoutByteCount ||
			replacement.CircleIdempotencyKey != nil || replacement.AttributionReviewRequired || replacement.Canceled || replacement.Completed {
			t.Fatal("replacement does not carry the corrected basis", replacement)
		}

		// sweeps are conserved: the same identities, all moved
		if remaining, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId); len(remaining) != 0 {
			t.Fatal("original kept sweeps", len(remaining))
		}
		movedKeys, movedBytes := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId)
		if !slices.Equal(movedKeys, sweepKeys) || movedBytes != byteCount {
			t.Fatal("sweeps were not conserved", len(movedKeys), movedBytes)
		}

		// the original is resolved: the reconciliation no longer lists it
		if again := legacyReconcileTestRun(t, owner, lister, false); again.AttemptCount != 0 || again.AbsentCount != 0 {
			t.Fatalf("a replaced original is still a legacy attempt: %+v", again)
		}
		// and it is never sent
		advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: original.PaymentId}, owner)
		if err != nil || !advanced.Canceled || len(sends) != 0 {
			t.Fatal("canceled original advanced", advanced, err, len(sends))
		}

		// the replacement is sent once, with a fresh key and the corrected amount
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: replacement.PaymentId}, owner); err != nil {
			t.Fatal("replacement did not submit", err)
		}
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: replacement.PaymentId}, owner); err != nil {
			t.Fatal("replacement status read failed", err)
		}
		if len(sends) != 1 || sends[0].key == legacyKey || fmt.Sprintf("%f", sends[0].amount) != "0.430000" {
			t.Fatal("replacement was not sent exactly once at the corrected amount", sends)
		}
		submitted, err := model.GetPayment(owner.Ctx, replacement.PaymentId)
		if err != nil || submitted.CircleIdempotencyKey == nil || *submitted.CircleIdempotencyKey != sends[0].key ||
			submitted.PaymentRecord == nil || *submitted.PaymentRecord != "synthetic-replacement-transfer" {
			t.Fatal("replacement submission was not recorded", submitted, err)
		}
	})
}

// A bounded full move leaves the replacement held until its move completes,
// and a completed correction is a no-op when run again.
func TestLegacyProviderPaymentCorrectionFullMoveResumesAndIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey := legacyCorrectionTestFixture(t, 5, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		sweepKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		lister := &legacyReconcileTestLister{}

		first := legacyCorrectionTestRun(t, owner, lister, false, false, 2, 1)
		if first.ReplacedCount != 1 || first.MovedSweepCount != 2 || first.MovingCount != 1 || first.ReleasedCount != 0 || first.ScheduledCount != 0 {
			t.Fatalf("first bounded call: %+v", first)
		}
		held := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if !held.AttributionReviewRequired {
			t.Fatal("a moving replacement is payable")
		}
		// the earning admission refuses a moving replacement: no key, no send
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: held.PaymentId}, owner); err == nil || len(sends) != 0 {
			t.Fatal("a moving replacement was submitted", err, len(sends))
		}
		if moving, err := model.GetPayment(owner.Ctx, held.PaymentId); err != nil || moving.CircleIdempotencyKey != nil {
			t.Fatal("a moving replacement reserved a key", moving, err)
		}

		second := legacyCorrectionTestRun(t, owner, lister, false, false, 2, 1)
		if second.AttemptCount != 0 || second.ReplacedCount != 0 || second.MovedSweepCount != 2 || second.MovingCount != 1 || second.ReleasedCount != 0 {
			t.Fatalf("second bounded call: %+v", second)
		}
		third := legacyCorrectionTestRun(t, owner, lister, false, false, 2, 1)
		if third.MovedSweepCount != 1 || third.MovingCount != 0 || third.ReleasedCount != 1 || third.ScheduledCount != 1 {
			t.Fatalf("third bounded call: %+v", third)
		}

		journaled := map[string]int{}
		for _, eventType := range []string{"legacy_payment_replaced", "legacy_payment_replacement", "circle_attempt_outcome"} {
			journaled[eventType] = legacyCorrectionTestEventCount(t, owner.Ctx, eventType)
		}
		if journaled["legacy_payment_replaced"] != 1 || journaled["legacy_payment_replacement"] != 1 || journaled["circle_attempt_outcome"] != 1 {
			t.Fatal("replacement journal", journaled)
		}
		fourth := legacyCorrectionTestRun(t, owner, lister, false, false, 2, 1)
		if fourth.ReplacedCount != 0 || fourth.MovedSweepCount != 0 || fourth.ReleasedCount != 0 || fourth.RefusedCount != 0 || fourth.MovingCount != 0 {
			t.Fatalf("a completed correction changed again: %+v", fourth)
		}
		for eventType, count := range journaled {
			if again := legacyCorrectionTestEventCount(t, owner.Ctx, eventType); again != count {
				t.Fatal("a repeated correction journaled again", eventType, again, count)
			}
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		movedKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId)
		if !slices.Equal(movedKeys, sweepKeys) || replacement.AttributionReviewRequired {
			t.Fatal("resumed move did not conserve every sweep", len(movedKeys))
		}
	})
}

// A correction that cannot cover the transfer fee is anchored and then held.
func TestLegacyProviderPaymentCorrectionHoldsDust(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey := legacyCorrectionTestFixture(t, 2, model.UsdToNanoCents(0.02), 0)
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		lister := &legacyReconcileTestLister{}

		result := legacyCorrectionTestRun(t, owner, lister, false, true, 10, 0)
		if result.ReplacedCount != 1 || result.DustCount != 1 || result.DustPayout != model.UsdToNanoCents(0.008) ||
			result.AnchorCount != 1 || result.ReleasedCount != 0 || result.DustHeldCount != 1 || result.ScheduledCount != 0 {
			t.Fatalf("dust correction: %+v", result)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		if !replacement.AttributionReviewRequired || replacement.Payout != model.UsdToNanoCents(0.008) {
			t.Fatal("dust replacement is payable", replacement)
		}
		if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: replacement.PaymentId}, owner); err == nil || len(sends) != 0 {
			t.Fatal("dust replacement was submitted", err, len(sends))
		}
		again := legacyCorrectionTestRun(t, owner, lister, false, true, 10, 0)
		if again.DustHeldCount != 1 || again.ReleasedCount != 0 || again.ScheduledCount != 0 || again.AnchorCount != 0 ||
			legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_replacement_held") != 1 ||
			legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_sweeps_pending_reattribution") != 2 {
			t.Fatalf("dust hold was not stable: %+v", again)
		}
	})
}

// Only Circle's affirmative absence authorizes a replacement.
func TestLegacyProviderPaymentCorrectionRequiresCircleAbsence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey := legacyCorrectionTestFixture(t, 1, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		cases := []struct {
			lister    *legacyReconcileTestLister
			found     int
			ambiguous int
			lookup    int
		}{
			// the exact legacy amount, payout less the fixed fee
			{lister: &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
				legacyReconcileTestTransfer("synthetic-sent", "COMPLETE", "1.290000", original.CreateTime.Add(time.Minute)),
			}}, found: 1},
			{lister: &legacyReconcileTestLister{transactions: []*CircleListedTransaction{
				legacyReconcileTestTransfer("synthetic-unexplained", "COMPLETE", "1.280000", original.CreateTime.Add(time.Minute)),
			}}, ambiguous: 1},
			{lister: &legacyReconcileTestLister{err: errors.New("synthetic Circle listing outage")}, lookup: 1},
		}
		for _, c := range cases {
			result := legacyCorrectionTestRun(t, owner, c.lister, false, true, 10, 0)
			if result.FoundCount != c.found || result.AmbiguousCount != c.ambiguous || result.LookupErrorCount != c.lookup ||
				result.AbsentCount != 0 || result.ReplacedCount != 0 || result.CorrectionCount != 0 {
				t.Errorf("a correction was decided without absence: %+v", result)
			}
		}
		requireLegacyAttemptHeld(t, owner, original, legacyKey)
		if n := legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_replacement"); n != 0 || len(sends) != 0 {
			t.Fatal("a held attempt was replaced or sent", n, len(sends))
		}
	})
}

// A canceled payment whose own Circle record completed is recorded completed,
// as status only; any other Circle state or amount leaves it unchanged.
func TestCompletePaidCanceledProviderPaymentFromCircle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		cutoff := time.Date(2020, 1, 7, 0, 0, 0, 0, time.UTC)
		controllerPayoutSchedule(t, cutoff)
		type paidCanceled struct {
			owner   *session.ClientSession
			payment *model.AccountPayment
			record  string
		}
		paidCanceledFixture := func(record string) *paidCanceled {
			owner, payment := controllerPayoutFixture(t, cutoff.Add(-time.Microsecond))
			if err := legacyCorrectionTestExec(owner.Ctx, `UPDATE account_payment SET circle_idempotency_key=$2, payment_record=$3, token_type='USDC', token_amount=9.99, payment_time=now() WHERE payment_id=$1`,
				payment.PaymentId, server.NewId(), record); err != nil {
				t.Fatal(err)
			}
			if err := legacyCorrectionTestExec(owner.Ctx, `UPDATE account_payment SET canceled=true, cancel_time=now() WHERE payment_id=$1`, payment.PaymentId); err != nil {
				t.Fatal(err)
			}
			return &paidCanceled{owner: owner, payment: payment, record: record}
		}
		paid := paidCanceledFixture("synthetic-paid-record")
		mismatched := paidCanceledFixture("synthetic-mismatched-record")
		failed := paidCanceledFixture("synthetic-failed-record")
		client := controllerPayoutMock(t, func(context.Context, server.Id, float64, string, string) (*CreateTransferTransactionResult, error) {
			t.Fatal("recording a completion sent a transfer")
			return nil, nil
		})
		client.GetTransactionFunc = func(_ context.Context, id string) (*GetTransactionResult, error) {
			transfer := CircleTransaction{Id: id, State: "COMPLETE", TxHash: "0xsynthetic-" + id, Amounts: []string{"9.990000"}, Blockchain: "MATIC"}
			switch id {
			case mismatched.record:
				transfer.Amounts = []string{"9.980000"}
			case failed.record:
				transfer.State, transfer.TxHash = "FAILED", ""
			}
			return &GetTransactionResult{Transaction: transfer, ResponseBodyBytes: []byte(`{"state":"` + transfer.State + `"}`)}, nil
		}
		run := func(dryRun bool) *PaidCanceledProviderPaymentResult {
			result, err := completePaidCanceledProviderPayments(paid.owner, &PaidCanceledProviderPaymentSettings{DryRun: dryRun}, client)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		paidBalance := func() (paidNetRevenue model.NanoCents) {
			server.Db(paid.owner.Ctx, func(conn server.PgConn) {
				result, err := conn.Query(paid.owner.Ctx, `SELECT paid_net_revenue_nano_cents FROM account_balance WHERE network_id=$1`, paid.payment.NetworkId)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&paidNetRevenue))
					}
				})
			})
			return
		}

		preview := run(true)
		if preview.CandidateCount != 3 || preview.CompleteCount != 1 || preview.CompletePayout != paid.payment.Payout ||
			preview.MismatchCount != 1 || preview.NotCompleteCount != 1 || preview.RecordedCount != 0 {
			t.Fatalf("dry run: %+v", preview)
		}
		if unchanged, err := model.GetPayment(paid.owner.Ctx, paid.payment.PaymentId); err != nil || !unchanged.Canceled || unchanged.Completed {
			t.Fatal("dry run changed the payment", unchanged, err)
		}

		result := run(false)
		if result.RecordedCount != 1 || result.RefusedCount != 0 {
			t.Fatalf("completion was not recorded: %+v", result)
		}
		completed, err := model.GetPayment(paid.owner.Ctx, paid.payment.PaymentId)
		if err != nil || !completed.Completed || completed.Canceled || completed.TxHash == nil || *completed.TxHash != "0xsynthetic-"+paid.record ||
			completed.Payout != paid.payment.Payout || paidBalance() != paid.payment.Payout {
			t.Fatal("paid canceled payment was not recorded completed", completed, err)
		}
		for _, other := range []*paidCanceled{mismatched, failed} {
			if unchanged, err := model.GetPayment(other.owner.Ctx, other.payment.PaymentId); err != nil || !unchanged.Canceled || unchanged.Completed || unchanged.TxHash != nil {
				t.Fatal("an unconfirmed canceled payment changed", unchanged, err)
			}
		}
		if again := run(false); again.CandidateCount != 2 || again.RecordedCount != 0 || paidBalance() != paid.payment.Payout {
			t.Fatalf("a recorded completion was considered again: %+v", again)
		}
	})
}

type legacyCorrectionTestReattributionDetails struct {
	OriginalPaymentId        server.Id `json:"original_payment_id"`
	ReplacementPaymentId     server.Id `json:"replacement_payment_id"`
	RemainingSweepCount      int64     `json:"remaining_sweep_count"`
	RemainingPayoutByteCount int64     `json:"remaining_payout_byte_count"`
	ReplacementSweepCount    int64     `json:"replacement_sweep_count"`
}

// The pending reattribution journaled on one payment.
func legacyCorrectionTestReattribution(t testing.TB, ctx context.Context, paymentId server.Id) (detailsList []*legacyCorrectionTestReattributionDetails) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT event_details FROM audit_account_payment WHERE payment_id=$1 AND event_type='legacy_payment_sweeps_pending_reattribution'`, paymentId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var text string
				server.Raise(result.Scan(&text))
				details := &legacyCorrectionTestReattributionDetails{}
				server.Raise(json.Unmarshal([]byte(text), details))
				detailsList = append(detailsList, details)
			}
		})
	})
	return
}

// The owner's mode: the replacement takes one deterministic anchor sweep, the
// canceled original keeps the rest, journaled on both payments, and only the
// replacement is ever sent, once.
func TestLegacyProviderPaymentCorrectionAnchorsOneSweepAndPaysOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey := legacyCorrectionTestFixture(t, 5, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		sweepKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		lister := &legacyReconcileTestLister{}

		preview := legacyCorrectionTestRun(t, owner, lister, true, true, 10, 0)
		if preview.CorrectionCount != 1 || preview.AnchorCount != 1 || preview.AnchorMissingCount != 0 ||
			preview.CorrectedPayout != model.UsdToNanoCents(0.44) || preview.ReplacedCount != 0 {
			t.Fatalf("anchored dry run: %+v", preview)
		}
		if remaining, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId); !slices.Equal(remaining, sweepKeys) {
			t.Fatal("dry run moved a sweep")
		}

		result := legacyCorrectionTestRun(t, owner, lister, false, true, 10, 0)
		if result.ReplacedCount != 1 || result.AnchorCount != 1 || result.AnchorMissingCount != 0 || result.ReleasedCount != 1 ||
			result.ScheduledCount != 1 || result.RefusedCount != 0 || result.MovedSweepCount != 0 {
			t.Fatalf("anchored correction: %+v", result)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		anchorKeys, anchorBytes := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId)
		remainingKeys, remainingBytes := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		// the first sweep in contract order moves, and every other sweep stays
		if len(anchorKeys) != 1 || anchorKeys[0] != sweepKeys[0] || anchorBytes != 100 || !slices.Equal(remainingKeys, sweepKeys[1:]) || remainingBytes != 400 {
			t.Fatal("anchor is not the first sweep, or the original lost sweeps", anchorKeys, len(remainingKeys))
		}
		if replacement.AttributionReviewRequired || replacement.PayoutByteCount != original.PayoutByteCount || replacement.Payout != model.UsdToNanoCents(0.44) {
			t.Fatal("anchored replacement", replacement)
		}
		for _, paymentId := range []server.Id{original.PaymentId, replacement.PaymentId} {
			detailsList := legacyCorrectionTestReattribution(t, owner.Ctx, paymentId)
			if len(detailsList) != 1 || detailsList[0].RemainingSweepCount != 4 || detailsList[0].RemainingPayoutByteCount != 400 ||
				detailsList[0].ReplacementSweepCount != 1 || detailsList[0].OriginalPaymentId != original.PaymentId ||
				detailsList[0].ReplacementPaymentId != replacement.PaymentId {
				t.Fatal("pending reattribution was not journaled", paymentId, detailsList)
			}
		}

		// only the replacement is sent, once, with a fresh key
		if advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: original.PaymentId}, owner); err != nil || !advanced.Canceled {
			t.Fatal("canceled original advanced", advanced, err)
		}
		for range 2 {
			if _, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: replacement.PaymentId}, owner); err != nil {
				t.Fatal("anchored replacement did not submit", err)
			}
		}
		if len(sends) != 1 || sends[0].key == legacyKey || fmt.Sprintf("%f", sends[0].amount) != "0.430000" {
			t.Fatal("anchored replacement was not sent exactly once at the corrected amount", sends)
		}

		again := legacyCorrectionTestRun(t, owner, lister, false, true, 10, 0)
		if again.ReplacedCount != 0 || again.AnchorCount != 0 || again.ReleasedCount != 0 || again.RefusedCount != 0 ||
			legacyCorrectionTestEventCount(t, owner.Ctx, "legacy_payment_sweeps_pending_reattribution") != 2 {
			t.Fatalf("anchored correction changed again: %+v", again)
		}
		if anchorAgain, _ := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId); !slices.Equal(anchorAgain, anchorKeys) {
			t.Fatal("a repeated correction moved another sweep")
		}
	})
}

// The canceled original keeps its remaining sweeps and its key. A real plan
// selects only unpaid sweeps (a null owner, or a canceled owner without a key)
// and restores only keyless canceled payments, so the remainder is never paid
// again; the reconciliation and the advance ignore the original, and transfer
// statistics count its bytes once, through the replacement.
func TestLegacyProviderPaymentCorrectionAnchorRemainderIsNeverPickedUp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey, identity := legacyCorrectionTestFixtureWithIdentity(t, 3, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		lister := &legacyReconcileTestLister{}
		result := legacyCorrectionTestRun(t, owner, lister, false, true, 10, 0)
		if result.ReplacedCount != 1 || result.AnchorCount != 1 || result.ReleasedCount != 1 {
			t.Fatalf("anchored correction: %+v", result)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		remainingKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId)
		anchorKeys, _ := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId)
		if len(remainingKeys) != 2 || len(anchorKeys) != 1 {
			t.Fatal("anchored fixture", len(remainingKeys), len(anchorKeys))
		}

		// a positive control: an unpaid sweep of the same network a plan must select
		control := server.NewId()
		usage := fmt.Sprintf(`{"version":1,"byte_count":7,"providers":[{"client_id":%q,"network_id":%q,"byte_count":7}]}`, identity.client.String(), identity.network.String())
		server.Tx(owner.Ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.Ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				VALUES($1,$2,$3,$4,$5,7,$6,$7,'settled',$8,true)`, control, identity.sourceNetwork, identity.source, identity.network, identity.client, identity.closed.Add(-time.Hour), identity.closed, usage))
			server.RaisePgResult(tx.Exec(owner.Ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
				VALUES($1,$2,$3,$4,7,$5,$6)`, control, identity.balance, identity.network, identity.client, model.UsdToNanoCents(1), identity.closed))
		})
		config := *model.EnvSubsidyConfig()
		config.MinPayoutUsd = 0
		config.UsdPerActiveUser = 0
		config.SubscriptionNetRevenueFraction = 0
		config.MinWalletPayoutUsd = 0
		plan, err := model.CreatePaymentPlan(owner.Ctx, &config, false, 0)
		if err != nil || plan.NetworkPayments[identity.network] == nil || plan.NetworkPayments[identity.network].PayoutByteCount != 7 || plan.RestoredLegacyPaymentCount != 0 {
			t.Fatal("a plan selected the canceled original's sweeps or restored it", plan, err)
		}
		if after, _ := legacyCorrectionTestSweeps(t, owner.Ctx, original.PaymentId); !slices.Equal(after, remainingKeys) {
			t.Fatal("the remainder left the canceled original", len(after))
		}
		if anchorAfter, _ := legacyCorrectionTestSweeps(t, owner.Ctx, replacement.PaymentId); !slices.Equal(anchorAfter, anchorKeys) {
			t.Fatal("the anchor left the replacement")
		}
		canceled, err := model.GetPayment(owner.Ctx, original.PaymentId)
		if err != nil || !canceled.Canceled || canceled.Completed || canceled.CircleIdempotencyKey == nil || *canceled.CircleIdempotencyKey != legacyKey {
			t.Fatal("the original was restored or lost its key", canceled, err)
		}

		if again := legacyReconcileTestRun(t, owner, lister, false); again.AttemptCount != 0 || again.AbsentCount != 0 {
			t.Fatalf("the reconciliation listed the canceled original: %+v", again)
		}
		if advanced, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: original.PaymentId}, owner); err != nil || !advanced.Canceled || len(sends) != 0 {
			t.Fatal("the canceled original advanced", advanced, err, len(sends))
		}

		// unpaid: the replacement's full bytes and the control, never the remainder again
		stats := model.GetTransferStats(owner.Ctx, identity.network)
		if stats == nil || stats.UnpaidBytesProvided != replacement.PayoutByteCount+7 || stats.PaidBytesProvided != 0 {
			t.Fatal("transfer statistics counted the remainder again", stats)
		}
		// the payment history shows only the replacement of the pair
		payments, err := model.GetNetworkPayments(owner)
		if err != nil {
			t.Fatal(err)
		}
		listed := map[server.Id]*model.AccountPayment{}
		for _, payment := range payments {
			listed[payment.PaymentId] = payment
		}
		if listed[original.PaymentId] != nil || listed[replacement.PaymentId] == nil || listed[replacement.PaymentId].Canceled {
			t.Fatal("payment history", len(payments))
		}
	})
}

// Providers never see the canceled, inflated original: the payment history
// lists only the replacement of a corrected pair, at the corrected amount with
// the full bytes, naming the payment it replaces. Other payments are unchanged.
func TestLegacyProviderPaymentCorrectionHistoryShowsOnlyReplacement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey := legacyCorrectionTestFixture(t, 3, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		before, err := model.GetNetworkPayments(owner)
		if err != nil || len(before) != 1 || before[0].PaymentId != original.PaymentId || before[0].ReplacesPaymentId != nil {
			t.Fatal("history before the correction", before, err)
		}

		result := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, false, true, 10, 0)
		if result.ReplacedCount != 1 || result.ReleasedCount != 1 {
			t.Fatalf("anchored correction: %+v", result)
		}
		replacement := legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId)
		after, err := model.GetNetworkPayments(owner)
		if err != nil || len(after) != 1 {
			t.Fatal("the history shows more than the replacement of the pair", len(after), err)
		}
		shown := after[0]
		if shown.PaymentId != replacement.PaymentId || shown.Payout != model.UsdToNanoCents(0.44) || shown.PayoutByteCount != original.PayoutByteCount ||
			shown.Canceled || shown.ReplacesPaymentId == nil || *shown.ReplacesPaymentId != original.PaymentId {
			t.Fatal("the history does not show the corrected replacement", shown)
		}
		// the added field is omitted for every other payment, so old clients see the same shape
		encodedOriginal, err := json.Marshal(before[0])
		if err != nil || strings.Contains(string(encodedOriginal), "replaces_payment_id") {
			t.Fatal("an unreplaced payment gained a field", string(encodedOriginal), err)
		}
		encodedReplacement, err := json.Marshal(shown)
		if err != nil || !strings.Contains(string(encodedReplacement), `"replaces_payment_id":"`+original.PaymentId.String()+`"`) {
			t.Fatal("the replacement does not name the payment it replaces", string(encodedReplacement), err)
		}
	})
}

// The public leaderboard counts a corrected pair's bytes once, as the replacement.
func TestLegacyProviderPaymentCorrectionLeaderboardCountsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner, original, legacyKey, identity := legacyCorrectionTestFixtureWithBytes(t, 3, 1024*1024, model.UsdToNanoCents(1), model.UsdToNanoCents(0.3))
		sends := []legacyCorrectionTestSend{}
		legacyCorrectionTestMock(t, legacyKey, &sends)
		// a public network keeps its id in the public leaderboard
		if err := legacyCorrectionTestExec(owner.Ctx, `UPDATE network SET leaderboard_public = true WHERE network_id = $1`, identity.network); err != nil {
			t.Fatal(err)
		}
		mib := func() (rankingMib float32, earnerMib float32) {
			rankings, err := model.GetNetworkLeaderboardRankings(owner.Ctx)
			if err != nil {
				t.Fatal(err)
			}
			earners, err := model.GetLeaderboard(owner.Ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, earner := range earners {
				if earner.NetworkId == identity.network.String() {
					earnerMib = earner.NetMiBCount
				}
			}
			return rankings[identity.network].NetMiBCount, earnerMib
		}
		if rankingMib, earnerMib := mib(); rankingMib != 3 || earnerMib != 3 {
			t.Fatal("leaderboard before the correction", rankingMib, earnerMib)
		}
		result := legacyCorrectionTestRun(t, owner, &legacyReconcileTestLister{}, false, true, 10, 0)
		if result.ReplacedCount != 1 || result.ReleasedCount != 1 || legacyCorrectionTestReplacement(t, owner.Ctx, original.PaymentId).PayoutByteCount != 3*1024*1024 {
			t.Fatalf("anchored correction: %+v", result)
		}
		if rankingMib, earnerMib := mib(); rankingMib != 3 || earnerMib != 3 {
			t.Fatal("the leaderboard counted a corrected pair twice", rankingMib, earnerMib)
		}
	})
}
