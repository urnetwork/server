package controller

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tests added by the sn-reconcile mutation run: each one fails for a mutant
// of the classification or of `--send` that the other tests let pass.

// RC-6: the listing must hold every record stored from exactly an hour after
// `from` to an hour before the listing started; a record a microsecond inside
// the lower margin need not appear.
func TestSnReconcileCompletenessMarginsAreExact(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		from := snReconcileTestClosed.Add(-snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		keyOnly := f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(1))
		f.cancelWithKey(keyOnly)
		at := func(value time.Time) *time.Time {
			return &value
		}
		f.completedRecord(networkId, &walletId, "rc6-lower-edge", at(from.Add(time.Hour)))
		f.completedRecord(networkId, &walletId, "rc6-lower-margin", at(from.Add(time.Hour-time.Microsecond)))
		// The listing starts after this, so this is more than an hour before it.
		f.completedRecord(networkId, &walletId, "rc6-upper-inside", at(server.NowUtc().Add(-time.Hour-5*time.Minute)))

		report := f.dry()
		f.expect(report, map[server.Id]string{keyOnly: "unresolved: listing_incomplete"})
		if !report.Listing.From.Equal(from) || !slices.Equal(report.Listing.MissingIds, []string{"rc6-lower-edge", "rc6-upper-inside"}) {
			t.Fatalf("listing = %+v", report.Listing)
		}
	})
}

// RC-7, A3: an orphan created exactly a day before the row's earliest close
// blocks the row; one created a second earlier does not.
func TestSnReconcileOrphanWindowEdge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		start := snReconcileTestClosed.Add(-snReconcileTestDay)
		networkA, walletA, addressA := f.network("MATIC")
		atStart := f.legacy(networkA, &walletA, created, model.UsdToNanoCents(1))
		f.cancelWithKey(atStart)
		networkB, walletB, addressB := f.network("MATIC")
		beforeStart := f.legacy(networkB, &walletB, created, model.UsdToNanoCents(1))
		f.cancelWithKey(beforeStart)
		f.list(snReconcileTestMaticWallet, f.transfer("at-start", "COMPLETE", addressA, start), f.transfer("before-start", "COMPLETE", addressB, start.Add(-time.Second)))

		report := f.dry()
		f.expect(report, map[server.Id]string{atStart: "unresolved: orphan_transfer", beforeStart: snReconcilePayable})
		if ids := snReconcileTestRow(t, report, atStart).OrphanIds; !slices.Equal(ids, []string{"at-start"}) {
			t.Fatal("blocking orphans", ids)
		}
	})
}

// A3, RC-5: the listing starts a day before the earliest window of any stuck
// row, here a later row whose contract closed long before the oldest row's.
func TestSnReconcileListingStartsAtEarliestWindowOfAnyRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkA, walletA, _ := f.network("MATIC")
		oldest := f.payment(networkA, &walletA, created, 0, f.contract(networkA, created.Add(-time.Hour), model.UsdToNanoCents(1)))
		f.cancelWithKey(oldest)
		networkB, walletB, _ := f.network("MATIC")
		earliestClose := f.legacy(networkB, &walletB, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(earliestClose)

		report := f.dry()
		f.expect(report, map[server.Id]string{oldest: snReconcilePayable, earliestClose: snReconcilePayable})
		from := snReconcileTestClosed.Add(-snReconcileTestDay)
		if !report.Listing.From.Equal(from) || len(f.ledger.froms) == 0 {
			t.Fatal("listing from", report.Listing.From, "want", from)
		}
		for _, listedFrom := range f.ledger.froms {
			if !listedFrom.Equal(from) {
				t.Fatal("a wallet was listed from", listedFrom, "want", from)
			}
		}
	})
}

// A7: the digest binds each payable row's chain; --send refuses after the
// network moved its payout wallet to another chain.
func TestSnReconcileDigestBindsChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		paymentId := f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(1))
		f.cancelWithKey(paymentId)
		digest := f.dry().Digest
		solWallet := f.wallet(networkId, "SOL", "sol-"+networkId.String(), true)
		f.exec(`UPDATE payout_wallet SET wallet_id=$2 WHERE network_id=$1`, networkId, solWallet)
		before := f.tables()

		report, err := SnReconcile(f.session, true, digest)
		if !errors.Is(err, ErrSnReconcileDigestMismatch) || snReconcileTestRow(t, report, paymentId).Chain != "SOL" || len(report.Revived) != 0 || f.tables() != before {
			t.Fatal("a payable row on another chain was sent under the reviewed digest", err)
		}
	})
}

// A7: the digest binds each payable row's owed and send amounts; --send
// refuses after the row's obligation changed while it was briefly pending.
func TestSnReconcileDigestBindsAmounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		paymentId := f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(1))
		f.cancelWithKey(paymentId)
		digest := f.dry().Digest
		// The row is pending again, gains a correction of its own, and is
		// canceled with a key once more.
		f.exec(`UPDATE account_payment SET canceled=false,cancel_time=NULL,circle_idempotency_key=NULL WHERE payment_id=$1`, paymentId)
		f.exec(`INSERT INTO account_payment_bonus(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,policy_sha256)
			SELECT $1,payment_id,payment_id,payment_plan_id,network_id,$2,'own correction',$3,'' FROM account_payment WHERE payment_id=$4`,
			server.NewId(), model.UsdToNanoCents(1), snReconcileTestClosed, paymentId)
		f.exec(`UPDATE account_payment SET payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2 WHERE payment_id=$1`, paymentId, model.UsdToNanoCents(1))
		f.cancelWithKey(paymentId)
		before := f.tables()

		report, err := SnReconcile(f.session, true, digest)
		if !errors.Is(err, ErrSnReconcileDigestMismatch) || snReconcileTestRow(t, report, paymentId).OwedNanoCents != model.UsdToNanoCents(2) || len(report.Revived) != 0 || f.tables() != before {
			t.Fatal("a changed obligation was sent under the reviewed digest", err)
		}
	})
}
