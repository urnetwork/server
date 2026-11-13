package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Circle as the pass reads it: each destination's listing and each record.
type attemptedCloseTestCircle struct {
	listings map[string][]*circleAttemptedTransfer
	records  map[string]*circleAttemptedTransfer
}

func (self *attemptedCloseTestCircle) payoutWalletId(blockchain string) (string, error) {
	return attemptedCloseTestPayoutWallet, nil
}

func (self *attemptedCloseTestCircle) listDestination(ctx context.Context, blockchain string, address string, relevant func(*circleAttemptedTransfer) bool) ([]*circleAttemptedTransfer, error) {
	return self.listings[address], nil
}

func (self *attemptedCloseTestCircle) getTransfer(ctx context.Context, blockchain string, id string) (*circleAttemptedTransfer, error) {
	transfer, ok := self.records[id]
	if !ok {
		return nil, errors.New("synthetic unknown transfer")
	}
	return transfer, nil
}

// A canceled payment of the payout to a fresh network's Solana wallet, keyed,
// with the payment record when one is given. Returns it and its wallet's
// address.
func attemptedCloseTestPayment(t testing.TB, ctx context.Context, seed string, payout model.NanoCents, createTime time.Time, record *string) (server.Id, server.Id, string) {
	t.Helper()
	networkId := server.NewId()
	testingCreatePaymentNetworkRow(ctx, networkId)
	owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
	defer owner.Cancel()
	address := walletAddressTestSolana(seed)
	walletId := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{NetworkId: networkId, Blockchain: "SOL", WalletAddress: address, DefaultTokenType: "USDC"})
	if walletId == nil {
		t.Fatal("no synthetic wallet")
	}
	paymentId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment
			(payment_id, payment_plan_id, network_id, wallet_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time,
			 canceled, cancel_time, circle_idempotency_key, payment_record)
			VALUES ($1, $2, $3, $4, 1024, $5, $6, $6, true, $7, $8, $9)`,
			paymentId, server.NewId(), networkId, *walletId, payout, createTime, createTime.Add(time.Hour), server.NewId(), record))
	})
	return paymentId, networkId, address
}

func attemptedCloseTestState(t testing.TB, ctx context.Context, paymentId server.Id) (completed bool, canceled bool, txHash *string, closed bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT completed, canceled, tx_hash,
			EXISTS (SELECT 1 FROM audit_account_payment WHERE event_id = $2 AND payment_id = $1 AND event_type = $3)
			FROM account_payment WHERE payment_id = $1`,
			paymentId, model.UsdcPayoutClosedEventId(paymentId), server.UsdcPayoutClosedEventType).Scan(&completed, &canceled, &txHash, &closed))
	})
	return
}

// The owner's decision of 2026-11-13: each canceled payment an attempt of
// which reached Circle is read from Circle once. A dry run decides and writes
// nothing; a commit with another digest writes nothing; a commit with the dry
// run's digest closes the forfeited ones, with no transfer or a failed record,
// records the one whose transfer completed paid, with its chain hash and the
// network's paid total, and leaves the one in flight. A later census lists
// only that one.
func TestPayoutsCloseAttemptedClosesForfeitedAndRecordsPaid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		created := server.NowUtc().Add(-30 * 24 * time.Hour)
		failedRecord := server.NewId().String()
		absentId, _, _ := attemptedCloseTestPayment(t, ctx, "absent", model.UsdToNanoCents(1.01), created, nil)
		paidId, paidNetworkId, paidAddress := attemptedCloseTestPayment(t, ctx, "paid", model.UsdToNanoCents(2.01), created, nil)
		inFlightId, _, inFlightAddress := attemptedCloseTestPayment(t, ctx, "in flight", model.UsdToNanoCents(3.01), created, nil)
		failedId, _, failedAddress := attemptedCloseTestPayment(t, ctx, "failed", model.UsdToNanoCents(4.01), created, &failedRecord)
		paidTransfer := attemptedCloseTestTransfer("2", "COMPLETE", "synthetic-paid-hash", created.Add(2*time.Hour), paidAddress)
		failedTransfer := attemptedCloseTestTransfer("4", "FAILED", "", created.Add(2*time.Hour), failedAddress)
		failedTransfer.Id = failedRecord
		circle := &attemptedCloseTestCircle{
			listings: map[string][]*circleAttemptedTransfer{
				paidAddress:     {paidTransfer},
				inFlightAddress: {attemptedCloseTestTransfer("3", "SENT", "", created.Add(2*time.Hour), inFlightAddress)},
			},
			records: map[string]*circleAttemptedTransfer{failedRecord: failedTransfer},
		}
		settings := &UsdcAttemptedCloseSettings{}

		dryRun, err := usdcAttemptedClose(ctx, circle, settings, false, "", server.NowUtc)
		if err != nil {
			t.Fatal(err)
		}
		decisions := map[server.Id]string{}
		for _, row := range dryRun.Rows {
			decisions[row.PaymentId] = row.Class + "/" + row.Reason
		}
		want := map[server.Id]string{
			absentId:   "closed/circle_absent",
			paidId:     "paid/circle_complete",
			inFlightId: "in_flight/circle_sent",
			failedId:   "closed/circle_failed",
		}
		if len(decisions) != len(want) {
			t.Fatalf("decisions %v, want %v", decisions, want)
		}
		for paymentId, decision := range want {
			if decisions[paymentId] != decision {
				t.Errorf("payment %s: %s, want %s", paymentId, decisions[paymentId], decision)
			}
		}
		unwritten := func() {
			t.Helper()
			for paymentId := range want {
				if completed, canceled, _, closed := attemptedCloseTestState(t, ctx, paymentId); completed || !canceled || closed {
					t.Fatalf("payment %s was written: completed %t, canceled %t, closed %t", paymentId, completed, canceled, closed)
				}
			}
		}
		unwritten()

		if _, err := usdcAttemptedClose(ctx, circle, settings, true, "synthetic-other-digest", server.NowUtc); !errors.Is(err, ErrUsdcAttemptedDigestMismatch) {
			t.Fatal("a commit with another digest was not refused", err)
		}
		unwritten()

		commit, err := usdcAttemptedClose(ctx, circle, settings, true, dryRun.Digest, server.NowUtc)
		if err != nil || commit.Closed != 2 || commit.Completed != 1 || commit.Skipped != 0 {
			t.Fatalf("commit %+v %v", commit, err)
		}
		for _, paymentId := range []server.Id{absentId, failedId} {
			if completed, canceled, _, closed := attemptedCloseTestState(t, ctx, paymentId); completed || !canceled || !closed {
				t.Errorf("forfeited payment %s: completed %t, canceled %t, closed %t", paymentId, completed, canceled, closed)
			}
		}
		if completed, canceled, txHash, closed := attemptedCloseTestState(t, ctx, paidId); !completed || canceled || txHash == nil || *txHash != "synthetic-paid-hash" || !closed {
			t.Errorf("paid payment: completed %t, canceled %t, tx %v, closed %t", completed, canceled, txHash, closed)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var record string
			var paidNanoCents model.NanoCents
			server.Raise(conn.QueryRow(ctx, `SELECT payment_record FROM account_payment WHERE payment_id = $1`, paidId).Scan(&record))
			server.Raise(conn.QueryRow(ctx, `SELECT paid_net_revenue_nano_cents FROM account_balance WHERE network_id = $1`, paidNetworkId).Scan(&paidNanoCents))
			if record != paidTransfer.Id || paidNanoCents != model.UsdToNanoCents(2.01) {
				t.Errorf("paid payment record %s, paid total %d", record, paidNanoCents)
			}
		})
		if completed, canceled, _, closed := attemptedCloseTestState(t, ctx, inFlightId); completed || !canceled || closed {
			t.Errorf("in-flight payment written: completed %t, canceled %t, closed %t", completed, canceled, closed)
		}

		again, err := usdcAttemptedClose(ctx, circle, settings, false, "", server.NowUtc)
		if err != nil || len(again.Rows) != 1 || again.Rows[0].PaymentId != inFlightId {
			t.Fatalf("a later census: %+v %v", again, err)
		}
	})
}

// A payment that changed since the census is not written: one closed
// meanwhile is neither closed nor completed again, and a transfer another
// payment owns is never recorded as this payment's.
func TestUsdcAttemptedWritesRefuseAChangedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		created := server.NowUtc().Add(-30 * 24 * time.Hour)
		paymentId, _, _ := attemptedCloseTestPayment(t, ctx, "changed", model.UsdToNanoCents(1.01), created, nil)
		otherId, _, _ := attemptedCloseTestPayment(t, ctx, "owner", model.UsdToNanoCents(1.01), created, nil)
		payments := model.GetUsdcAttemptedPayments(ctx)
		var payment, other *model.UsdcAttemptedPayment
		for _, candidate := range payments {
			switch candidate.PaymentId {
			case paymentId:
				payment = candidate
			case otherId:
				other = candidate
			}
		}
		if payment == nil || other == nil {
			t.Fatal("the census did not read the payments")
		}

		// another payment owns the transfer
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET payment_record = 'synthetic-owned-record' WHERE payment_id = $1`, otherId))
		})
		if model.CompleteUsdcAttemptedPayment(ctx, payment, "synthetic-owned-record", "synthetic-hash", "{}", server.NowUtc(), "{}") {
			t.Fatal("another payment's transfer was recorded as this payment's")
		}

		// closed meanwhile
		if !model.CloseUsdcAttemptedPayment(ctx, payment, server.NowUtc(), "{}") {
			t.Fatal("the unchanged payment did not close")
		}
		if model.CloseUsdcAttemptedPayment(ctx, payment, server.NowUtc(), "{}") ||
			model.CompleteUsdcAttemptedPayment(ctx, payment, "synthetic-record", "synthetic-hash", "{}", server.NowUtc(), "{}") {
			t.Fatal("a closed payment was written again")
		}
		if completed, canceled, _, closed := attemptedCloseTestState(t, ctx, paymentId); completed || !canceled || !closed {
			t.Fatalf("closed payment: completed %t, canceled %t, closed %t", completed, canceled, closed)
		}
	})
}

// A payment of the payout to a fresh network's Solana wallet, created 30 days
// ago, in the state named: "attempted" (canceled and keyed, as the pass held
// one), "recorded" (canceled with a key and a payment record), "unattempted"
// (canceled with no key, record or hash), "pending" (keyed, neither canceled
// nor completed), "completed" (keyed, recorded, with a chain hash) or "closed"
// (attempted, then closed with its usdc_payout_closed event).
func forfeitTestPayment(t testing.TB, ctx context.Context, seed string, state string) server.Id {
	t.Helper()
	created := server.NowUtc().Add(-30 * 24 * time.Hour)
	networkId := server.NewId()
	testingCreatePaymentNetworkRow(ctx, networkId)
	owner := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId})
	defer owner.Cancel()
	walletId := model.CreateAccountWalletExternal(owner, &model.CreateAccountWalletExternalArgs{
		NetworkId: networkId, Blockchain: "SOL", WalletAddress: walletAddressTestSolana("forfeit " + seed), DefaultTokenType: "USDC"})
	if walletId == nil {
		t.Fatal("no synthetic wallet")
	}
	key := attemptedCloseTestPointer(server.NewId())
	var record, txHash *string
	canceled, completed := true, false
	switch state {
	case "attempted", "closed":
	case "recorded":
		record = attemptedCloseTestPointer(server.NewId().String())
	case "unattempted":
		key = nil
	case "pending":
		canceled = false
	case "completed":
		canceled, completed = false, true
		record = attemptedCloseTestPointer(server.NewId().String())
		txHash = attemptedCloseTestPointer("synthetic-hash-" + seed)
	default:
		t.Fatal("unknown synthetic payment state", state)
	}
	paymentId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment
			(payment_id, payment_plan_id, network_id, wallet_id, payout_byte_count, payout_nano_cents, min_sweep_time, create_time,
			 canceled, cancel_time, completed, complete_time, circle_idempotency_key, payment_record, tx_hash)
			VALUES ($1, $2, $3, $4, 1024, $5, $6, $6,
			 $7, CASE WHEN $7 THEN $6::timestamp + interval '1 hour' END,
			 $8, CASE WHEN $8 THEN $6::timestamp + interval '1 hour' END, $9, $10, $11)`,
			paymentId, server.NewId(), networkId, *walletId, model.UsdToNanoCents(1.01), created,
			canceled, completed, key, record, txHash))
	})
	if state == "closed" {
		payments := model.GetUsdcForfeitPayments(ctx, []server.Id{paymentId})
		if payments[paymentId] == nil || !model.CloseUsdcAttemptedPayment(ctx, &payments[paymentId].UsdcAttemptedPayment, server.NowUtc(), "{}") {
			t.Fatal("the synthetic payment did not close")
		}
	}
	return paymentId
}

// The payment's closure event details, or nil, and whether retention counts
// it settled.
func forfeitTestClosure(t testing.TB, ctx context.Context, paymentId server.Id) (details map[string]any, settled bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var detailsJson *string
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT event_details FROM audit_account_payment WHERE event_id = $2 AND payment_id = $1 AND event_type = $3),
			`+server.AccountPaymentRetentionSettledSql+`
			FROM account_payment WHERE payment_id = $1`,
			paymentId, model.UsdcPayoutClosedEventId(paymentId), server.UsdcPayoutClosedEventType).Scan(&detailsJson, &settled))
		if detailsJson != nil {
			server.Raise(json.Unmarshal([]byte(*detailsJson), &details))
		}
	})
	return
}

// The owner's decision of 2026-11-13 on the payments the pass held: each named
// payment is closed as forfeited, with no Circle read. A dry run decides and
// writes nothing; a commit with another digest writes nothing; a commit with
// the dry run's digest closes exactly the named payments with the pass's
// closure event, reason owner_forfeit, so retention counts them settled and
// the pass reads them no more. A payment not named stays as it was, and a
// named one closed already refuses a later forfeit.
func TestPayoutsCloseAttemptedForfeitClosesExactlyTheNamedPayments(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		attemptedId := forfeitTestPayment(t, ctx, "attempted", "attempted")
		recordedId := forfeitTestPayment(t, ctx, "recorded", "recorded")
		unnamedId := forfeitTestPayment(t, ctx, "unnamed", "attempted")
		named := []server.Id{recordedId, attemptedId}
		if recordedId.Less(attemptedId) {
			named = []server.Id{attemptedId, recordedId}
		}

		dryRun, err := usdcAttemptedForfeit(ctx, named, false, "", server.NowUtc)
		if err != nil {
			t.Fatal(err)
		}
		if len(dryRun.Rows) != 2 || dryRun.Digest == "" || !slices.IsSortedFunc(dryRun.Forfeit, func(a server.Id, b server.Id) int { return a.Cmp(b) }) {
			t.Fatalf("dry run %+v", dryRun)
		}
		for _, row := range dryRun.Rows {
			if row.Class != UsdcAttemptedClosed || row.Reason != UsdcAttemptedOwnerForfeit || row.PayoutNanoCents != model.UsdToNanoCents(1.01) {
				t.Fatalf("payment %s decided %s/%s, payout %d", row.PaymentId, row.Class, row.Reason, row.PayoutNanoCents)
			}
		}
		command := "commit with: bringyourctl payouts close-attempted --forfeit=" + dryRun.Forfeit[0].String() + "," + dryRun.Forfeit[1].String() +
			" --commit --expect=" + dryRun.Digest
		if !strings.Contains(dryRun.Text(), command) {
			t.Fatalf("the dry run does not print its commit command %q:\n%s", command, dryRun.Text())
		}
		unwritten := func() {
			t.Helper()
			for _, paymentId := range []server.Id{attemptedId, recordedId, unnamedId} {
				if details, settled := forfeitTestClosure(t, ctx, paymentId); details != nil || settled {
					t.Fatalf("payment %s was written: %v, settled %t", paymentId, details, settled)
				}
			}
		}
		unwritten()

		if _, err := usdcAttemptedForfeit(ctx, named, true, "synthetic-other-digest", server.NowUtc); !errors.Is(err, ErrUsdcAttemptedDigestMismatch) {
			t.Fatal("a commit with another digest was not refused", err)
		}
		unwritten()

		commit, err := usdcAttemptedForfeit(ctx, named, true, dryRun.Digest, server.NowUtc)
		if err != nil || commit.Closed != 2 || commit.Skipped != 0 || commit.Digest != dryRun.Digest {
			t.Fatalf("commit %+v %v", commit, err)
		}
		for _, paymentId := range []server.Id{attemptedId, recordedId} {
			details, settled := forfeitTestClosure(t, ctx, paymentId)
			if details == nil || details["class"] != UsdcAttemptedClosed || details["reason"] != UsdcAttemptedOwnerForfeit ||
				details["lookup"] != "owner_decision" || details["digest"] != dryRun.Digest || !settled {
				t.Fatalf("forfeited payment %s: closure %v, settled %t", paymentId, details, settled)
			}
			if completed, canceled, _, closed := attemptedCloseTestState(t, ctx, paymentId); completed || !canceled || !closed {
				t.Fatalf("forfeited payment %s: completed %t, canceled %t, closed %t", paymentId, completed, canceled, closed)
			}
		}
		if details, settled := forfeitTestClosure(t, ctx, unnamedId); details != nil || settled {
			t.Fatalf("a payment not named was written: %v, settled %t", details, settled)
		}
		read := []server.Id{}
		for _, payment := range model.GetUsdcAttemptedPayments(ctx) {
			read = append(read, payment.PaymentId)
		}
		if !slices.Contains(read, unnamedId) || slices.Contains(read, attemptedId) || slices.Contains(read, recordedId) {
			t.Fatalf("the pass reads %v after the forfeit", read)
		}

		again, err := usdcAttemptedForfeit(ctx, []server.Id{attemptedId}, false, "", server.NowUtc)
		if !errors.Is(err, ErrUsdcAttemptedForfeitRefused) || len(again.Rows) != 1 || again.Rows[0].Reason != "closed" || again.Digest != "" {
			t.Fatalf("a forfeit of a closed payment: %+v %v", again, err)
		}
	})
}

// Each named payment must be an unclosed attempted payment. A completed, a
// pending, a safely canceled, a closed or an unknown one refuses the whole run,
// dry run and commit alike, listed with why and with no digest, and nothing is
// written, not even for the eligible payment named with it. Naming no payment
// or one twice is refused before any read.
func TestPayoutsCloseAttemptedForfeitRefusesAnyPaymentNotUnclosedAndAttempted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		eligibleId := forfeitTestPayment(t, ctx, "eligible", "attempted")
		eligibleDryRun, err := usdcAttemptedForfeit(ctx, []server.Id{eligibleId}, false, "", server.NowUtc)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			reason    string
			paymentId server.Id
		}{
			{reason: "completed", paymentId: forfeitTestPayment(t, ctx, "completed", "completed")},
			{reason: "not_canceled", paymentId: forfeitTestPayment(t, ctx, "pending", "pending")},
			{reason: "not_attempted", paymentId: forfeitTestPayment(t, ctx, "unattempted", "unattempted")},
			{reason: "closed", paymentId: forfeitTestPayment(t, ctx, "closed", "closed")},
			{reason: "unknown", paymentId: server.NewId()},
		} {
			named := []server.Id{eligibleId, c.paymentId}
			for _, commit := range []bool{false, true} {
				report, err := usdcAttemptedForfeit(ctx, named, commit, eligibleDryRun.Digest, server.NowUtc)
				if !errors.Is(err, ErrUsdcAttemptedForfeitRefused) || report == nil || report.Digest != "" || report.Closed != 0 {
					t.Fatalf("%s, commit %t: %+v %v", c.reason, commit, report, err)
				}
				decisions := map[server.Id]string{}
				for _, row := range report.Rows {
					decisions[row.PaymentId] = row.Class + "/" + row.Reason
				}
				if decisions[c.paymentId] != UsdcAttemptedRefused+"/"+c.reason || decisions[eligibleId] != UsdcAttemptedClosed+"/"+UsdcAttemptedOwnerForfeit {
					t.Fatalf("%s, commit %t: decisions %v", c.reason, commit, decisions)
				}
				if text := report.Text(); strings.Contains(text, "digest") || strings.Contains(text, "commit with") {
					t.Fatalf("%s: a refused forfeit printed a digest or a command:\n%s", c.reason, text)
				}
				if details, settled := forfeitTestClosure(t, ctx, eligibleId); details != nil || settled {
					t.Fatalf("%s, commit %t: the eligible payment was written: %v, settled %t", c.reason, commit, details, settled)
				}
			}
		}
		for _, named := range [][]server.Id{nil, {}, {eligibleId, eligibleId}} {
			if report, err := usdcAttemptedForfeit(ctx, named, true, eligibleDryRun.Digest, server.NowUtc); !errors.Is(err, ErrUsdcAttemptedForfeitIds) || report != nil {
				t.Fatalf("named %v: %+v %v", named, report, err)
			}
		}
		if details, _ := forfeitTestClosure(t, ctx, eligibleId); details != nil {
			t.Fatal("the eligible payment was written", details)
		}
	})
}

// A forfeit closes all of its payments or none. One closed between a census
// and the write leaves the others unwritten, and a commit whose census finds
// one closed since the dry run refuses before any write.
func TestUsdcAttemptedForfeitClosesNoneWhenOneChanged(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		firstId := forfeitTestPayment(t, ctx, "first", "attempted")
		secondId := forfeitTestPayment(t, ctx, "second", "attempted")
		census := model.GetUsdcForfeitPayments(ctx, []server.Id{firstId, secondId})
		first, second := &census[firstId].UsdcAttemptedPayment, &census[secondId].UsdcAttemptedPayment
		details := map[server.Id]string{firstId: "{}", secondId: "{}"}

		if model.CloseUsdcAttemptedPayments(ctx, []*model.UsdcAttemptedPayment{first, first}, server.NowUtc(), details) ||
			model.CloseUsdcAttemptedPayments(ctx, []*model.UsdcAttemptedPayment{first, second}, server.NowUtc(), map[server.Id]string{firstId: "{}"}) ||
			model.CloseUsdcAttemptedPayments(ctx, nil, server.NowUtc(), details) {
			t.Fatal("a repeated payment, missing details or no payment was written")
		}
		dryRun, err := usdcAttemptedForfeit(ctx, []server.Id{firstId, secondId}, false, "", server.NowUtc)
		if err != nil {
			t.Fatal(err)
		}

		// the first closes after the census
		if !model.CloseUsdcAttemptedPayment(ctx, first, server.NowUtc(), "{}") {
			t.Fatal("the first payment did not close")
		}
		if model.CloseUsdcAttemptedPayments(ctx, []*model.UsdcAttemptedPayment{first, second}, server.NowUtc(), details) {
			t.Fatal("a forfeit with a payment closed since its census was written")
		}
		if details, _ := forfeitTestClosure(t, ctx, secondId); details != nil {
			t.Fatal("the unchanged payment was closed without the changed one", details)
		}

		report, err := usdcAttemptedForfeit(ctx, []server.Id{firstId, secondId}, true, dryRun.Digest, server.NowUtc)
		if !errors.Is(err, ErrUsdcAttemptedForfeitRefused) || report.Closed != 0 {
			t.Fatalf("a commit after a payment closed: %+v %v", report, err)
		}
		if details, _ := forfeitTestClosure(t, ctx, secondId); details != nil {
			t.Fatal("the unchanged payment was closed by a refused commit", details)
		}

		// the unchanged one alone still forfeits
		alone, err := usdcAttemptedForfeit(ctx, []server.Id{secondId}, false, "", server.NowUtc)
		if err != nil {
			t.Fatal(err)
		}
		if committed, err := usdcAttemptedForfeit(ctx, []server.Id{secondId}, true, alone.Digest, server.NowUtc); err != nil || committed.Closed != 1 {
			t.Fatalf("the unchanged payment alone: %+v %v", committed, err)
		}
	})
}
