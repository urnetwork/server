package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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
