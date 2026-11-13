// Database tests of `payouts close-parked --paid`: a keyed payment that another
// Circle transfer paid is closed with the transfer journaled, every refusal
// writes nothing, a payment that changes after the census is skipped, and a
// closed payment never reads as an sn-reconcile stuck row. Circle is a fake
// listing; every network, wallet and transfer is synthetic.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A complete listing of each destination, as Circle would return it.
type closePaidTestLister struct {
	transactions map[string][]*CircleListedTransaction
	err          error
}

func (self *closePaidTestLister) ListDestinationTransactions(ctx context.Context, blockchain string, destinationAddress string) ([]*CircleListedTransaction, error) {
	if self.err != nil {
		return nil, self.err
	}
	listed := []*CircleListedTransaction{}
	for _, transaction := range self.transactions[destinationAddress] {
		copied := *transaction
		listed = append(listed, &copied)
	}
	return listed, nil
}

type closePaidDbTestFixture struct {
	*closeParkedTestFixture
	lister    *closePaidTestLister
	circle    *payoutClosePaidCircle
	paymentId server.Id
	key       server.Id
	holderId  server.Id
	address   string
	paying    *CircleListedTransaction
	entries   []*PayoutClosePaidEntry
}

// A MATIC network with a keyed pending payment (no record, no tx hash), and
// the completed reimbursement-like payment whose record is the transfer that
// paid the same obligation a week later, listed with the destination's other
// transfers.
func newClosePaidDbTestFixture(t testing.TB) *closePaidDbTestFixture {
	t.Helper()
	f := &closePaidDbTestFixture{closeParkedTestFixture: newCloseParkedTestFixture(t)}
	networkId, walletId, address := f.network("MATIC")
	f.address = address
	created := server.NowUtc().Add(-20 * 24 * time.Hour)
	f.paymentId = f.pending(networkId, &walletId, created, closePaidTestPayout)
	f.key = server.NewId()
	f.set(f.paymentId, `circle_idempotency_key=$2`, f.key)

	f.paying = closePaidTestTransfer(created.Add(5*24*time.Hour), closePaidTestSendAmount)
	f.paying.WalletId = snReconcileTestMaticWallet
	f.paying.DestinationAddress = address
	f.holderId = f.pending(networkId, &walletId, created.Add(5*24*time.Hour), closePaidTestPayout)
	f.set(f.holderId, `payment_record=$2,tx_hash=$3,completed=true,complete_time=$4`, f.paying.Id, f.paying.TxHash, created.Add(5*24*time.Hour))

	other := closePaidTestTransfer(created.Add(-30*24*time.Hour), "7.000000")
	other.WalletId = snReconcileTestMaticWallet
	other.DestinationAddress = address
	f.lister = &closePaidTestLister{transactions: map[string][]*CircleListedTransaction{address: {other, f.paying}}}
	f.circle = &payoutClosePaidCircle{
		lister: f.lister,
		payoutWalletId: func(blockchain string) (string, error) {
			if blockchain != "MATIC" {
				return "", errors.New("unconfigured chain")
			}
			return snReconcileTestMaticWallet, nil
		},
	}
	f.entries = []*PayoutClosePaidEntry{{PaymentId: f.paymentId, CircleTransactionId: f.paying.Id}}
	return f
}

func (self *closePaidDbTestFixture) run(apply bool, expect string, entries []*PayoutClosePaidEntry) (*PayoutCloseParkedReport, error) {
	return payoutsCloseParked(self.session, apply, expect, entries, self.circle)
}

// The usdc_payout_closed event details of the payment, raw.
func (self *closePaidDbTestFixture) closure(paymentId server.Id) (details map[string]any, count int) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT event_id, event_details FROM audit_account_payment WHERE payment_id = $1 AND event_type = 'usdc_payout_closed'`, paymentId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var eventId server.Id
				var text string
				server.Raise(result.Scan(&eventId, &text))
				if eventId != model.UsdcPayoutClosedEventId(paymentId) {
					self.t.Fatalf("closure event id %s, want the derived id", eventId)
				}
				details = map[string]any{}
				server.Raise(json.Unmarshal([]byte(text), &details))
				count += 1
			}
		})
	})
	return
}

// The dry run reads only and reports the payment with the transfer; the apply
// closes exactly it: canceled, with one closure event naming the reason, the
// key and the transfer with its hash, and the payment holding the transfer
// is untouched. A second run refuses it as closed before, and the sn-reconcile
// census, which reads canceled keyed rows, never sees it.
func TestPayoutsCloseParkedPaidClosesTheKeyedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClosePaidDbTestFixture(t)
		before := f.tables()
		parkedOnly, err := f.run(false, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		dry, err := f.run(false, "", f.entries)
		if err != nil {
			t.Fatal(err)
		}
		if f.tables() != before {
			t.Fatal("the dry run wrote")
		}
		if len(dry.Paid) != 1 || len(dry.PaidRefused) != 0 || dry.Paid[0].PaymentId != f.paymentId || dry.Paid[0].CircleTransactionId != f.paying.Id ||
			dry.Paid[0].TxHash != f.paying.TxHash || !slices.Equal(dry.Paid[0].RecordOfPaymentIds, []server.Id{f.holderId}) || dry.Paid[0].ListedCount != 2 {
			t.Fatalf("census %+v refused %+v", dry.Paid, dry.PaidRefused)
		}
		if dry.Digest == parkedOnly.Digest || dry.Parked.Count != parkedOnly.Parked.Count {
			t.Fatal("the paid payment is not in the digest, or changed the parked census")
		}
		if !strings.Contains(dry.ApplyCommand(), "--paid="+f.paymentId.String()+":"+f.paying.Id+" --apply --expect="+dry.Digest) {
			t.Fatalf("apply command %q", dry.ApplyCommand())
		}

		applied, err := f.run(true, dry.Digest, f.entries)
		if err != nil {
			t.Fatal(err)
		}
		if len(applied.ClosedPaid) != 1 || len(applied.SkippedPaid) != 0 || applied.ClosedPaid[0].PaymentId != f.paymentId {
			t.Fatalf("closed %+v skipped %+v", applied.ClosedPaid, applied.SkippedPaid)
		}
		payment := closeParkedTestPayment(t, f.closeParkedTestFixture, f.paymentId)
		if !payment.Canceled || payment.Completed || payment.CircleIdempotencyKey == nil || *payment.CircleIdempotencyKey != f.key ||
			payment.PaymentRecord != nil || payment.TxHash != nil {
			t.Fatalf("closed payment %+v", payment)
		}
		details, count := f.closure(f.paymentId)
		if count != 1 || details["reason"] != "paid_elsewhere" || details["switch"] != payoutCloseParkedSwitch ||
			details["circle_transaction_id"] != f.paying.Id || details["tx_hash"] != f.paying.TxHash ||
			details["circle_idempotency_key"] != f.key.String() || details["transfer_amount_usdc"] != closePaidTestSendAmount ||
			details["digest"] != dry.Digest || details["run_id"] != applied.RunId.String() {
			t.Fatalf("%d closure events, details %v", count, details)
		}
		holder := closeParkedTestPayment(t, f.closeParkedTestFixture, f.holderId)
		if !holder.Completed || holder.Canceled || holder.PaymentRecord == nil || *holder.PaymentRecord != f.paying.Id {
			t.Fatalf("the holder changed: %+v", holder)
		}

		again, err := f.run(false, "", f.entries)
		if !errors.Is(err, ErrPayoutClosePaidRefused) || len(again.PaidRefused) != 1 || !strings.Contains(again.PaidRefused[0].Reason, "closed before") {
			t.Fatalf("a closed payment was not refused: %v %+v", err, again.PaidRefused)
		}
		if _, count := f.closure(f.paymentId); count != 1 {
			t.Fatalf("%d closure events after the refusal", count)
		}
		stuck, err := model.GetSnReconcileStuckRows(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range stuck {
			if row.PaymentId == f.paymentId {
				t.Fatal("the closed keyed payment is an sn-reconcile stuck row")
			}
		}
	})
}

// Each failed check refuses the whole command, dry run and apply, and writes
// nothing; so do the switch and the digest.
func TestPayoutsCloseParkedPaidRefusesBeforeAnyWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClosePaidDbTestFixture(t)
		before := f.tables()
		refused := func(name string, entries []*PayoutClosePaidEntry, want string) {
			t.Helper()
			for _, apply := range []bool{false, true} {
				report, err := f.run(apply, strings.Repeat("0", 64), entries)
				if !errors.Is(err, ErrPayoutClosePaidRefused) || len(report.PaidRefused) != 1 || !strings.Contains(report.PaidRefused[0].Reason, want) {
					t.Fatalf("%s (apply %v): %v %+v", name, apply, err, report.PaidRefused)
				}
			}
			if f.tables() != before {
				t.Fatalf("%s wrote", name)
			}
		}
		refused("an unknown payment", []*PayoutClosePaidEntry{{PaymentId: server.NewId(), CircleTransactionId: f.paying.Id}}, "no such payment")
		refused("an unlisted transfer", []*PayoutClosePaidEntry{{PaymentId: f.paymentId, CircleTransactionId: uuid.NewString()}}, "lists no transfer")
		refused("the payment's own holder", []*PayoutClosePaidEntry{{PaymentId: f.holderId, CircleTransactionId: f.paying.Id}}, "completed")

		f.lister.err = errors.New("synthetic listing failure")
		refused("a failed listing", f.entries, "synthetic listing failure")
		f.lister.err = nil

		own := closePaidTestTransfer(f.paying.CreateDate.Add(time.Hour), closePaidTestSendAmount)
		own.WalletId, own.DestinationAddress = snReconcileTestMaticWallet, f.address
		f.lister.transactions[f.address] = append(f.lister.transactions[f.address], own)
		refused("a transfer that can be the own attempt", f.entries, own.Id)
		f.lister.transactions[f.address] = f.lister.transactions[f.address][:2]

		dry, err := f.run(false, "", f.entries)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.run(true, dry.Digest+"0", f.entries); !errors.Is(err, ErrPayoutCloseParkedDigestMismatch) {
			t.Fatalf("a mismatched digest: %v", err)
		}
		if _, err := f.run(true, dry.Digest, nil); !errors.Is(err, ErrPayoutCloseParkedDigestMismatch) {
			t.Fatalf("the digest of a census with the paid payment closed a census without it: %v", err)
		}
		usdcSwitchTestDeclare(t, "payments: on\n")
		if _, err := f.run(true, dry.Digest, f.entries); !errors.Is(err, ErrPayoutCloseParkedPaymentsNotOff) {
			t.Fatalf("payments on: %v", err)
		}
		if f.tables() != before {
			t.Fatal("a refused apply wrote")
		}
	})
}

// Runs the census, changes the payment's facts with `change` just before the
// apply checks it again, and requires the apply to skip it and leave it as it
// is.
func closePaidTestSkipsChanged(t testing.TB, change func(f *closePaidDbTestFixture)) {
	f := newClosePaidDbTestFixture(t)
	dry, err := f.run(false, "", f.entries)
	if err != nil {
		t.Fatal(err)
	}
	payoutClosePaidBeforeClose = func(paymentId server.Id) {
		if paymentId == f.paymentId {
			change(f)
		}
	}
	defer func() {
		payoutClosePaidBeforeClose = nil
	}()
	report, err := f.run(true, dry.Digest, f.entries)
	if err == nil || !strings.Contains(err.Error(), "changed since the census") || len(report.ClosedPaid) != 0 || len(report.SkippedPaid) != 1 {
		t.Fatalf("%v closed %+v skipped %+v", err, report.ClosedPaid, report.SkippedPaid)
	}
	if payment := closeParkedTestPayment(t, f.closeParkedTestFixture, f.paymentId); payment.Canceled {
		t.Fatal("the changed payment was canceled")
	}
	if _, count := f.closure(f.paymentId); count != 0 {
		t.Fatal("the changed payment was journaled")
	}
}

// A payment recorded after the census is skipped.
func TestPayoutsCloseParkedPaidSkipsAPaymentRecordedAfterTheCensus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		closePaidTestSkipsChanged(t, func(f *closePaidDbTestFixture) {
			f.set(f.paymentId, `payment_record=$2`, uuid.NewString())
		})
	})
}

// A payment whose destination lists a transfer that can be its own attempt
// after the census is skipped.
func TestPayoutsCloseParkedPaidSkipsAPaymentWhoseAttemptAppears(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		closePaidTestSkipsChanged(t, func(f *closePaidDbTestFixture) {
			own := closePaidTestTransfer(f.paying.CreateDate.Add(time.Hour), closePaidTestSendAmount)
			own.WalletId, own.DestinationAddress = snReconcileTestMaticWallet, f.address
			f.lister.transactions[f.address] = append(f.lister.transactions[f.address], own)
		})
	})
}

// The model close writes nothing for a payment that is no longer as read,
// under its row lock.
func TestCloseUsdcPaidElsewherePaymentRefusesAChangedPayment(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newClosePaidDbTestFixture(t)
		read := model.GetUsdcPaidElsewherePayment(f.ctx, f.paymentId)
		if read == nil || read.Closed || read.CircleIdempotencyKey == nil || *read.CircleIdempotencyKey != f.key || read.WalletAddress == nil || *read.WalletAddress != f.address {
			t.Fatalf("read %+v", read)
		}
		details := func(*model.UsdcPaidElsewherePayment) string { return `{}` }
		for _, change := range []func(*model.UsdcPaidElsewherePayment){
			func(p *model.UsdcPaidElsewherePayment) { p.Payout += 1 },
			func(p *model.UsdcPaidElsewherePayment) { key := server.NewId(); p.CircleIdempotencyKey = &key },
			func(p *model.UsdcPaidElsewherePayment) { walletId := server.NewId(); p.WalletId = &walletId },
		} {
			changed := *read
			change(&changed)
			if model.CloseUsdcPaidElsewherePayment(f.ctx, &changed, server.NowUtc(), details) {
				t.Fatal("closed a payment that is not as read")
			}
		}
		f.staged(f.paymentId)
		if model.CloseUsdcPaidElsewherePayment(f.ctx, read, server.NowUtc(), details) {
			t.Fatal("closed a payment whose plan has a pending sweep assignment")
		}
		if _, count := f.closure(f.paymentId); count != 0 {
			t.Fatal("a refused close journaled")
		}
	})
}
