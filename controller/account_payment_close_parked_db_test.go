// Database tests of `payouts close-parked`. Every test uses synthetic networks,
// wallets and payments on its own test database; the sn-reconcile fixture
// writes legacy payments the way the historical planner left them.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type closeParkedTestFixture struct {
	*snReconcileTestFixture
}

// Also declares USDC payments off, as the close-out requires.
func newCloseParkedTestFixture(t testing.TB) *closeParkedTestFixture {
	t.Helper()
	usdcSwitchTestDeclare(t, "payments: off\n")
	return &closeParkedTestFixture{newSnReconcileTestFixture(t)}
}

// A pending payment of `payout` without sweeps, created at `created`.
func (self *closeParkedTestFixture) pending(networkId server.Id, walletId *server.Id, created time.Time, payout model.NanoCents) server.Id {
	return self.payment(networkId, walletId, created, payout)
}

// A provider network with an active payout wallet, and that wallet.
func (self *closeParkedTestFixture) paid() (server.Id, *server.Id) {
	networkId, walletId, _ := self.network("MATIC")
	return networkId, &walletId
}

func (self *closeParkedTestFixture) set(paymentId server.Id, assignments string, args ...any) {
	self.exec(`UPDATE account_payment SET `+assignments+` WHERE payment_id=$1`, append([]any{paymentId}, args...)...)
}

// The payment's plan has a staged sweep assignment that has not completed.
func (self *closeParkedTestFixture) staged(paymentId server.Id) {
	self.exec(`INSERT INTO payment_plan_assignment(payment_plan_id,create_time,sweep_count)
		SELECT payment_plan_id,now(),0 FROM account_payment WHERE payment_id=$1`, paymentId)
}

// The payment is a manual reimbursement: its marker takes the payment's id.
func (self *closeParkedTestFixture) reimbursement(paymentId server.Id) {
	self.exec(`INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$1,'manual_reimbursement','{}')`, paymentId)
}

// A closure event for the payment, as an earlier close-out wrote it.
func (self *closeParkedTestFixture) closedBefore(paymentId server.Id) {
	self.exec(`INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'usdc_payout_closed','{}')`,
		model.UsdcPayoutClosedEventId(paymentId), paymentId)
}

// A correction of `amount` on the pending payment, with exact provenance.
func (self *closeParkedTestFixture) bonus(paymentId server.Id, amount model.NanoCents) {
	self.exec(`INSERT INTO account_payment_bonus(operation_id,origin_payment_id,payment_id,payment_plan_id,network_id,amount_nano_cents,reason,latest_contract_close_time,policy_sha256)
		SELECT $1,payment_id,payment_id,payment_plan_id,network_id,$2,'synthetic correction',$3,'' FROM account_payment WHERE payment_id=$4`,
		server.NewId(), amount, snReconcileTestClosed, paymentId)
	self.set(paymentId, `payout_nano_cents=payout_nano_cents+$2,bonus_payout_nano_cents=bonus_payout_nano_cents+$2`, amount)
}

// A contract opened at `opened` that has no outcome, and its sweep, not yet
// planned: an unresolved liability to the planner.
func (self *closeParkedTestFixture) openContract(networkId server.Id, opened time.Time) server.Id {
	contractId, idTime := server.NewId(), model.ContractIdTimeBound(opened)
	copy(contractId[0:6], idTime[0:6])
	self.exec(`INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,usage_origin_is_source)
		VALUES($1,$2,$3,$4,$5,100,$6,true)`, contractId, self.sourceNetwork, self.source, networkId, self.clients[networkId], opened)
	self.exec(`INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,100,0,$5)`, contractId, self.balance, networkId, self.clients[networkId], opened)
	return contractId
}

func (self *closeParkedTestFixture) dryRun() *PayoutCloseParkedReport {
	self.t.Helper()
	report, err := PayoutsCloseParked(self.session, false, "")
	if err != nil {
		self.t.Fatal(err)
	}
	return report
}

func (self *closeParkedTestFixture) apply(digest string) *PayoutCloseParkedReport {
	self.t.Helper()
	report, err := PayoutsCloseParked(self.session, true, digest)
	if err != nil {
		self.t.Fatal(err)
	}
	return report
}

// An md5 of every row of each table the close-out could write.
func (self *closeParkedTestFixture) tables() (digest string) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT concat_ws(' ',
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.payment_id),'')) FROM account_payment t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.event_id),'')) FROM audit_account_payment t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.task_id),'')) FROM pending_task t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.contract_id,t.balance_id,t.network_id),'')) FROM transfer_escrow_sweep t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.operation_id,t.origin_payment_id),'')) FROM account_payment_bonus t),
			(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.payment_plan_id),'')) FROM payment_plan_assignment t))`).Scan(&digest))
	})
	return
}

// The closure events: event id, time and details, by payment id.
type closeParkedTestEvent struct {
	eventId   server.Id
	eventTime time.Time
	details   payoutCloseParkedEvent
}

func (self *closeParkedTestFixture) closures() map[server.Id][]closeParkedTestEvent {
	events := map[server.Id][]closeParkedTestEvent{}
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT payment_id,event_id,event_time,event_details FROM audit_account_payment WHERE event_type='usdc_payout_closed'`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var event closeParkedTestEvent
				var details string
				server.Raise(result.Scan(&paymentId, &event.eventId, &event.eventTime, &details))
				server.Raise(json.Unmarshal([]byte(details), &event.details))
				events[paymentId] = append(events[paymentId], event)
			}
		})
	})
	return events
}

// The ids of report rows, in their order.
func closeParkedTestIds(rows []*PayoutCloseParkedRow) []server.Id {
	ids := []server.Id{}
	for _, row := range rows {
		ids = append(ids, row.PaymentId)
	}
	return ids
}

// The row as JSON without its cancellation columns.
func closeParkedTestWithoutCancel(t testing.TB, row string) string {
	t.Helper()
	var columns map[string]any
	if err := json.Unmarshal([]byte(row), &columns); err != nil {
		t.Fatal(err)
	}
	delete(columns, "canceled")
	delete(columns, "cancel_time")
	encoded, err := json.Marshal(columns)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func closeParkedTestPayment(t testing.TB, f *closeParkedTestFixture, paymentId server.Id) *model.AccountPayment {
	t.Helper()
	payment, err := model.GetPayment(f.ctx, paymentId)
	if err != nil || payment == nil {
		t.Fatalf("payment %s: %v", paymentId, err)
	}
	return payment
}

// The digest of exactly these ids and payouts, written out by hand.
func closeParkedTestDigest(payouts map[server.Id]model.NanoCents) string {
	ids := snReconcileTestSorted(slices.Collect(maps.Keys(payouts))...)
	lines := []string{"urnetwork payouts close-parked v1"}
	for _, paymentId := range ids {
		lines = append(lines, fmt.Sprintf("payment %s %d", paymentId, payouts[paymentId]))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// 1. The census holds exactly the pending payments Circle cannot hold: never a
// keyed, recorded, hashed, completed, canceled or staged payment, nor one closed
// before.
func TestPayoutsCloseParkedSelection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		created := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
		networkId, walletId := f.paid()
		bare := f.bareNetwork()

		withWallet := f.pending(networkId, walletId, created, model.UsdToNanoCents(2))
		walletLess := f.pending(bare, nil, created, model.UsdToNanoCents(3))
		dust := f.pending(networkId, walletId, created, 10_000_000)
		reviewHeld := f.pending(networkId, walletId, created, model.UsdToNanoCents(4))
		f.review(reviewHeld)
		reimbursement := f.pending(networkId, walletId, created, model.UsdToNanoCents(5))
		f.reimbursement(reimbursement)
		// the processor cancellation of an attempt holding a legacy component
		// clears its markers and leaves it pending with Circle's receipt
		receipt := f.pending(networkId, walletId, created, model.UsdToNanoCents(6))
		f.set(receipt, `payment_receipt='{"state":"CANCELLED"}'`)
		inScope := []server.Id{withWallet, walletLess, dust, reviewHeld, reimbursement, receipt}

		keyed := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.set(keyed, `circle_idempotency_key=$2`, server.NewId())
		recorded := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.set(recorded, `payment_record='synthetic-record'`)
		hashed := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.set(hashed, `tx_hash='0xsynthetic'`)
		completed := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.set(completed, `completed=true,complete_time=now()`)
		canceled := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.set(canceled, `canceled=true,cancel_time=now()`)
		staged := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.staged(staged)
		closedBefore := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.closedBefore(closedBefore)

		report := f.dryRun()
		if got, want := closeParkedTestIds(report.Parked.Rows), snReconcileTestSorted(inScope...); !slices.Equal(got, want) {
			t.Fatalf("census %v, want %v", got, want)
		}
		if report.ClosedPendingCount != 1 {
			t.Fatal("the payment closed before and pending again was not reported", report.ClosedPendingCount)
		}
		reasons := map[server.Id]string{}
		for _, row := range report.Parked.Rows {
			reasons[row.PaymentId] = row.Reason
		}
		if want := map[server.Id]string{withWallet: model.UsdcParkedOther, walletLess: model.UsdcParkedWalletLess, dust: model.UsdcParkedDust,
			reviewHeld: model.UsdcParkedReviewHeld, reimbursement: model.UsdcParkedOther, receipt: model.UsdcParkedOther}; !maps.Equal(reasons, want) {
			t.Fatalf("reasons %v, want %v", reasons, want)
		}
		if report.Parked.ManualReimbursements.Count != 1 || report.Parked.ManualReimbursements.PayoutNanoCents != model.UsdToNanoCents(5) {
			t.Fatal("the manual reimbursement was not reported", report.Parked.ManualReimbursements)
		}

		// the staged payment is parked once its plan's assignment completes
		f.exec(`UPDATE payment_plan_assignment SET complete_time=now()`)
		if got, want := closeParkedTestIds(f.dryRun().Parked.Rows), snReconcileTestSorted(append(slices.Clone(inScope), staged)...); !slices.Equal(got, want) {
			t.Fatalf("census after the assignment %v, want %v", got, want)
		}
	})
}

// 2. The dry run writes nothing, in a read-only process, and its digest is
// stable: sha256 over each parked payment's id and payout, in id order. A new
// parked payment or a changed payout changes it.
func TestPayoutsCloseParkedDryRunWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		created := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
		networkId, walletId := f.paid()
		first := f.pending(networkId, walletId, created, model.UsdToNanoCents(2))
		second := f.pending(f.bareNetwork(), nil, created.Add(-7*snReconcileTestDay), 1_234_567_891)
		keyed := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.set(keyed, `circle_idempotency_key=$2`, server.NewId())

		before := f.tables()
		server.RequirePgReadOnly()
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		report, err := PayoutsCloseParked(f.session, false, "")
		// the process really was read only: a write on its path fails
		writeRefused := false
		func() {
			defer func() {
				writeRefused = recover() != nil
			}()
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type) VALUES ($1, $1, 'synthetic_write_probe')`, server.NewId()))
			}, server.OptNoRetry())
		}()
		server.Testing_ReleasePgReadOnly()
		if err != nil {
			t.Fatal(err)
		}
		if !writeRefused {
			t.Fatal("the dry run's process accepted a write")
		}
		if after := f.tables(); after != before {
			t.Fatal("the dry run wrote", before, after)
		}
		if report.Mode != "dry_run" || report.Closed != nil || len(report.Skipped) != 0 || report.UsdcPayments != server.UsdcPayoutOff {
			t.Fatal("the dry run reported a close-out", report.Mode, report.Closed, report.Skipped, report.UsdcPayments)
		}

		if want := closeParkedTestDigest(map[server.Id]model.NanoCents{first: model.UsdToNanoCents(2), second: 1_234_567_891}); report.Digest != want {
			t.Fatal("the digest is not sha256 over the parked ids and payouts", report.Digest, want)
		}
		if again := f.dryRun(); again.Digest != report.Digest || again.RunId == report.RunId {
			t.Fatal("the digest of the same census changed", again.Digest, report.Digest)
		}

		third := f.pending(networkId, walletId, created, model.UsdToNanoCents(7))
		withThird := f.dryRun()
		if withThird.Digest == report.Digest || withThird.Parked.Count != 3 {
			t.Fatal("a new parked payment kept the digest")
		}
		f.bonus(third, 1)
		if withBonus := f.dryRun(); withBonus.Digest == withThird.Digest || withBonus.Parked.PayoutNanoCents != withThird.Parked.PayoutNanoCents+1 {
			t.Fatal("a changed payout kept the digest")
		}
		if after := f.tables(); after == before {
			t.Fatal("the fixture changes were not observed")
		}
	})
}

// 3. --apply refuses before any write unless its census digest matches and
// usdc_payout.yml declares payments off: a stale digest, payments on, an
// unreadable file and an absent one (payments on by default) all refuse.
func TestPayoutsCloseParkedApplyRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		created := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
		networkId, walletId := f.paid()
		f.pending(networkId, walletId, created, model.UsdToNanoCents(2))
		f.pending(f.bareNetwork(), nil, created, model.UsdToNanoCents(3))
		digest := f.dryRun().Digest
		before := f.tables()

		refuse := func(name string, expect string, want error) {
			t.Helper()
			report, err := PayoutsCloseParked(f.session, true, expect)
			if !errors.Is(err, want) || !strings.HasSuffix(err.Error(), "; nothing was closed") {
				t.Fatalf("%s: %v, want %v", name, err, want)
			}
			if report == nil || report.Digest != digest || report.Parked.Count != 2 || report.Closed != nil {
				t.Fatalf("%s: the refusal did not report the census: %+v", name, report)
			}
			if after := f.tables(); after != before {
				t.Fatalf("%s: a refused apply wrote", name)
			}
		}
		refuse("stale digest", strings.Repeat("0", 64), ErrPayoutCloseParkedDigestMismatch)
		refuse("no digest", "", ErrPayoutCloseParkedDigestMismatch)

		usdcSwitchTestDeclare(t, "payments: on\n")
		refuse("payments on", digest, ErrPayoutCloseParkedPaymentsNotOff)
		usdcSwitchTestDeclare(t, "wallet_requests: off\n")
		refuse("payments on by default", digest, ErrPayoutCloseParkedPaymentsNotOff)
		usdcSwitchTestDeclare(t, "payments: maybe\n")
		refuse("unreadable switches", digest, ErrPayoutCloseParkedPaymentsNotOff)
		if report, _ := PayoutsCloseParked(f.session, false, ""); !strings.HasPrefix(report.UsdcPayments, "unreadable: ") {
			t.Fatal("the dry run did not report the unreadable switches", report.UsdcPayments)
		}

		usdcSwitchTestDeclare(t, "payments: off\n")
		if report := f.apply(digest); report.Closed.Count != 2 {
			t.Fatal("the same digest did not close once payments were off", report.Closed.Count)
		}
	})
}

// 3. Without usdc_payout.yml, payments are on, so --apply refuses.
func TestPayoutsCloseParkedApplyRefusesWithoutSwitches(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if _, err := server.Config.SimpleResource(server.UsdcPayoutPolicyResourceName); err == nil {
			t.Skip("the environment declares usdc_payout.yml; its absence is not observable")
		}
		f := &closeParkedTestFixture{newSnReconcileTestFixture(t)}
		networkId, walletId := f.paid()
		f.pending(networkId, walletId, time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC), model.UsdToNanoCents(2))
		report := f.dryRun()
		before := f.tables()
		if _, err := PayoutsCloseParked(f.session, true, report.Digest); !errors.Is(err, ErrPayoutCloseParkedPaymentsNotOff) || report.UsdcPayments != server.UsdcPayoutOn {
			t.Fatal("an apply without usdc_payout.yml was not refused", err, report.UsdcPayments)
		}
		if after := f.tables(); after != before {
			t.Fatal("a refused apply wrote")
		}
	})
}

// 4. --apply cancels exactly the census, in batches, with one closure event
// each, keyed by the derived event id and naming the switch, and changes
// nothing else. A rerun closes nothing.
func TestPayoutsCloseParkedApplyClosesSelection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		batchSize := payoutCloseParkedBatchSize
		payoutCloseParkedBatchSize = 2
		t.Cleanup(func() { payoutCloseParkedBatchSize = batchSize })
		created := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
		networkId, walletId := f.paid()
		bare := f.bareNetwork()

		parked := map[server.Id]model.NanoCents{}
		reasons := map[server.Id]string{}
		park := func(networkId server.Id, walletId *server.Id, payout model.NanoCents, reason string) server.Id {
			paymentId := f.pending(networkId, walletId, created, payout)
			parked[paymentId], reasons[paymentId] = payout, reason
			return paymentId
		}
		park(networkId, walletId, model.UsdToNanoCents(2), model.UsdcParkedOther)
		park(bare, nil, model.UsdToNanoCents(3), model.UsdcParkedWalletLess)
		park(networkId, walletId, 10_000_000, model.UsdcParkedDust)
		f.review(park(networkId, walletId, model.UsdToNanoCents(4), model.UsdcParkedReviewHeld))
		reimbursement := park(networkId, walletId, model.UsdToNanoCents(5), model.UsdcParkedOther)
		f.reimbursement(reimbursement)

		others := []server.Id{}
		for _, assignments := range []string{`circle_idempotency_key=gen_random_uuid()`, `payment_record='synthetic-record'`, `tx_hash='0xsynthetic'`,
			`completed=true,complete_time=now()`, `canceled=true,cancel_time=now()`} {
			paymentId := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
			f.set(paymentId, assignments)
			others = append(others, paymentId)
		}
		stagedPayment := f.pending(networkId, walletId, created, model.UsdToNanoCents(1))
		f.staged(stagedPayment)
		others = append(others, stagedPayment)

		dry := f.dryRun()
		if dry.Digest != closeParkedTestDigest(parked) {
			t.Fatal("the census is not the parked payments")
		}
		rowsBefore := f.payments()
		var tasksBefore, sweepsBefore string
		snapshot := func() (string, string) {
			var tasks, sweeps string
			server.Db(f.ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(f.ctx, `SELECT
					(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.task_id),'')) FROM pending_task t),
					(SELECT md5(COALESCE(string_agg(row_to_json(t)::text,'' ORDER BY t.contract_id,t.balance_id,t.network_id),'')) FROM transfer_escrow_sweep t)`).Scan(&tasks, &sweeps))
			})
			return tasks, sweeps
		}
		tasksBefore, sweepsBefore = snapshot()
		var auditBefore int
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT COUNT(*) FROM audit_account_payment`).Scan(&auditBefore))
		})

		start := server.NowUtc()
		report := f.apply(dry.Digest)
		end := server.NowUtc()
		if report.Mode != "apply" || report.Closed.Count != len(parked) || len(report.Skipped) != 0 ||
			!slices.Equal(closeParkedTestIds(report.Closed.Rows), snReconcileTestSorted(slices.Collect(maps.Keys(parked))...)) {
			t.Fatal("the apply did not close exactly the census", report.Closed, report.Skipped)
		}

		rowsAfter := f.payments()
		closures := f.closures()
		for paymentId, payout := range parked {
			payment := closeParkedTestPayment(t, f, paymentId)
			if !payment.Canceled || payment.Completed || payment.CancelTime == nil || payment.CancelTime.Before(start.Add(-time.Second)) || end.Add(time.Second).Before(*payment.CancelTime) {
				t.Fatalf("payment %s was not canceled by the apply: %+v", paymentId, payment)
			}
			if closeParkedTestWithoutCancel(t, rowsAfter[paymentId]) != closeParkedTestWithoutCancel(t, rowsBefore[paymentId]) {
				t.Fatalf("the apply changed more than the cancellation of %s:\n%s\n%s", paymentId, rowsBefore[paymentId], rowsAfter[paymentId])
			}
			events := closures[paymentId]
			if len(events) != 1 {
				t.Fatalf("payment %s has %d closure events", paymentId, len(events))
			}
			event := events[0]
			if event.eventId != model.UsdcPayoutClosedEventId(paymentId) || !event.eventTime.Equal(*payment.CancelTime) {
				t.Fatalf("payment %s closure event %s at %s", paymentId, event.eventId, event.eventTime)
			}
			want := payoutCloseParkedEvent{Switch: "usdc_payout.yml payments: off", RunId: report.RunId, Digest: dry.Digest, Reason: reasons[paymentId],
				Week: "2026-10-12", PaymentPlanId: payment.PaymentPlanId, NetworkId: payment.NetworkId, WalletId: payment.WalletId,
				PayoutNanoCents: payout, ManualReimbursement: paymentId == reimbursement}
			gotJson, _ := json.Marshal(&event.details)
			wantJson, _ := json.Marshal(&want)
			if string(gotJson) != string(wantJson) {
				t.Fatalf("payment %s closure details %s, want %s", paymentId, gotJson, wantJson)
			}
		}
		if len(closures) != len(parked) {
			t.Fatal("a payment outside the census has a closure event", len(closures))
		}
		for _, paymentId := range others {
			if rowsAfter[paymentId] != rowsBefore[paymentId] {
				t.Fatalf("the apply changed payment %s outside the census", paymentId)
			}
		}
		var auditAfter int
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT COUNT(*) FROM audit_account_payment`).Scan(&auditAfter))
		})
		if auditAfter != auditBefore+len(parked) {
			t.Fatal("the apply journaled more than one event per closure", auditBefore, auditAfter)
		}
		if tasks, sweeps := snapshot(); tasks != tasksBefore || sweeps != sweepsBefore {
			t.Fatal("the apply scheduled a task or moved a sweep")
		}

		// a rerun closes nothing twice
		closedState := f.tables()
		rerun := f.dryRun()
		if rerun.Parked.Count != 0 || rerun.Digest != closeParkedTestDigest(map[server.Id]model.NanoCents{}) || rerun.ClosedPendingCount != 0 {
			t.Fatal("closed payments are still parked", rerun.Parked.Rows)
		}
		if _, err := PayoutsCloseParked(f.session, true, dry.Digest); !errors.Is(err, ErrPayoutCloseParkedDigestMismatch) {
			t.Fatal("a rerun with the old digest was not refused", err)
		}
		if again := f.apply(rerun.Digest); again.Closed.Count != 0 || len(again.Skipped) != 0 {
			t.Fatal("a rerun closed something", again.Closed, again.Skipped)
		}
		if after := f.tables(); after != closedState {
			t.Fatal("a rerun wrote")
		}
	})
}

// 4. A census payment that changed before its batch is skipped, not closed:
// keyed by the sender, canceled by the hung-payment sweep, or given another
// payout. The apply closes the rest and reports the skips as an error.
func TestPayoutsCloseParkedApplySkipsChangedPayments(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		batchSize := payoutCloseParkedBatchSize
		payoutCloseParkedBatchSize = 2
		t.Cleanup(func() { payoutCloseParkedBatchSize = batchSize })
		created := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
		networkId, walletId := f.paid()
		ids := []server.Id{}
		payouts := map[server.Id]model.NanoCents{}
		for i := range 4 {
			paymentId := f.pending(networkId, walletId, created, model.UsdToNanoCents(float64(i+1)))
			ids = append(ids, paymentId)
			payouts[paymentId] = model.UsdToNanoCents(float64(i + 1))
		}
		// the batches take the census in id order
		ids = snReconcileTestSorted(ids...)
		keyed, kept, hung, bonused := ids[0], ids[1], ids[2], ids[3]
		key := server.NewId()

		batches := 0
		previous := payoutCloseParkedBeforeBatch
		payoutCloseParkedBeforeBatch = func(paymentIds []server.Id) {
			batches += 1
			switch batches {
			case 1:
				f.set(keyed, `circle_idempotency_key=$2`, key)
			case 2:
				f.set(hung, `canceled=true,cancel_time=now()`)
				f.bonus(bonused, 1)
			}
		}
		t.Cleanup(func() { payoutCloseParkedBeforeBatch = previous })

		dry := f.dryRun()
		report, err := PayoutsCloseParked(f.session, true, dry.Digest)
		if err == nil || !strings.Contains(err.Error(), "3 payment(s) changed since the census") {
			t.Fatal("the skips were not reported as an error", err)
		}
		if batches != 2 || !slices.Equal(closeParkedTestIds(report.Closed.Rows), []server.Id{kept}) ||
			!slices.Equal(closeParkedTestIds(report.Skipped), []server.Id{keyed, hung, bonused}) {
			t.Fatal("the apply did not skip exactly the changed payments", batches, report.Closed.Rows, report.Skipped)
		}
		closures := f.closures()
		if len(closures) != 1 || len(closures[kept]) != 1 {
			t.Fatal("a skipped payment was journaled", closures)
		}
		if payment := closeParkedTestPayment(t, f, keyed); payment.Canceled || payment.CircleIdempotencyKey == nil || *payment.CircleIdempotencyKey != key {
			t.Fatal("the keyed payment was closed", payment)
		}
		if payment := closeParkedTestPayment(t, f, bonused); payment.Canceled || payment.Payout != payouts[bonused]+1 {
			t.Fatal("the payment with another payout was closed", payment)
		}
		if payment := closeParkedTestPayment(t, f, hung); !payment.Canceled {
			t.Fatal("the hung cancellation was undone", payment)
		}
		if !closeParkedTestPayment(t, f, kept).Canceled {
			t.Fatal("the unchanged payment was not closed")
		}
	})
}

// 4. The apply stops before its next batch when payments turn on mid-run; the
// batches before stay closed.
func TestPayoutsCloseParkedApplyStopsWhenPaymentsTurnOn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		batchSize := payoutCloseParkedBatchSize
		payoutCloseParkedBatchSize = 1
		t.Cleanup(func() { payoutCloseParkedBatchSize = batchSize })
		networkId, walletId := f.paid()
		for range 3 {
			f.pending(networkId, walletId, time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC), model.UsdToNanoCents(2))
		}
		batches := 0
		previous := payoutCloseParkedBeforeBatch
		payoutCloseParkedBeforeBatch = func(paymentIds []server.Id) {
			batches += 1
			if batches == 1 {
				usdcSwitchTestDeclare(t, "payments: on\n")
			}
		}
		t.Cleanup(func() { payoutCloseParkedBeforeBatch = previous })
		report, err := PayoutsCloseParked(f.session, true, f.dryRun().Digest)
		if !errors.Is(err, ErrPayoutCloseParkedPaymentsNotOff) || batches != 1 || report.Closed.Count != 1 || len(report.Skipped) != 0 ||
			!strings.Contains(err.Error(), "stopped with 1 of the census's 3 payment(s) closed and 0 skipped") {
			t.Fatal("the apply went on with payments on", err, batches, report.Closed)
		}
		if closures := f.closures(); len(closures) != 1 {
			t.Fatal("closures after payments turned on", closures)
		}
	})
}

// 5. The production scenario, with the earning policy loaded: after the apply,
// neither the daily hung-payment cancellation nor the planner re-plans a closed
// payment's sweeps or moves its bonus, legacy recovery never restores a closed
// payment with a legacy component, and the planner's unresolved-liability
// sample never counts a closed payment's sweep. A payment the hung-payment
// sweep cancels is still re-planned.
func TestPayoutsCloseParkedNeverReplanned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		controllerPayoutSchedule(t, snReconcileTestCutoff)
		closeParkedTestNeverReplanned(t, f, true)
	})
}

// 5. Without the earning policy, the planner's canceled arm selects every
// safely canceled payment, legacy components included: it still never selects a
// closed payment's sweeps.
func TestPayoutsCloseParkedNeverReplannedWithoutEarningPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		if policy, err := server.LoadProviderPayoutEarningPolicy(f.ctx); err != nil || policy != nil {
			t.Fatal("the environment loads an earning policy", policy, err)
		}
		closeParkedTestNeverReplanned(t, f, false)
	})
}

func closeParkedTestNeverReplanned(t testing.TB, f *closeParkedTestFixture, earningPolicy bool) {
	t.Helper()
	created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)

	// A: revenue and a correction
	networkA, walletA := f.paid()
	revenueA := f.payment(networkA, walletA, created, 0, f.contract(networkA, snReconcileTestClosed, model.UsdToNanoCents(2)))
	f.bonus(revenueA, model.UsdToNanoCents(0.5))
	// B: a legacy subsidy component with an exact census, which legacy
	// recovery restores when it is safely canceled
	networkB, walletB := f.paid()
	subsidyB := f.payment(networkB, walletB, created, model.UsdToNanoCents(3), f.contract(networkB, snReconcileTestClosed, model.UsdToNanoCents(1)))
	f.set(subsidyB, `subsidy_payout_nano_cents=$2`, model.UsdToNanoCents(3))
	closedIds := []server.Id{revenueA, subsidyB}
	// E: a sweep of a contract with no outcome, which the planner samples as an
	// unresolved liability while its owner is safely canceled
	var unresolvedE server.Id
	if earningPolicy {
		networkE, walletE := f.paid()
		unresolvedE = f.payment(networkE, walletE, created, 0, f.openContract(networkE, snReconcileTestClosed))
		closedIds = append(closedIds, unresolvedE)
	}

	report := f.apply(f.dryRun().Digest)
	if !slices.Equal(closeParkedTestIds(report.Closed.Rows), snReconcileTestSorted(closedIds...)) {
		t.Fatal("the apply did not close the fixture", report.Closed.Rows)
	}
	payments := f.payments()

	// C: pending, created after the close-out's census, the same age
	networkC, walletC := f.paid()
	controlC := f.payment(networkC, walletC, created, 0, f.contract(networkC, snReconcileTestClosed, model.UsdToNanoCents(5)))

	if canceled := model.CancelHungAccountPayments(f.ctx, server.NowUtc()); canceled != 1 {
		t.Fatal("the hung-payment sweep did not cancel exactly the unclosed payment", canceled)
	}
	if !closeParkedTestPayment(t, f, controlC).Canceled {
		t.Fatal("the hung-payment sweep did not cancel the unclosed payment")
	}
	for attempt := range 2 {
		plan, err := model.CreatePaymentPlan(f.ctx, snReconcileTestRevenueConfig(), false, 0)
		if err != nil {
			t.Fatal(err)
		}
		networkIds := slices.Collect(maps.Keys(plan.NetworkPayments))
		if attempt == 0 && (len(networkIds) != 1 || plan.NetworkPayments[networkC] == nil || plan.NetworkPayments[networkC].Payout != model.UsdToNanoCents(5)) {
			t.Fatal("the planner did not re-plan exactly the hung-canceled payment", networkIds)
		}
		if attempt == 1 && len(networkIds) != 0 {
			t.Fatal("a second plan planned again", networkIds)
		}
		if plan.RestoredLegacyPaymentCount != 0 || plan.UnresolvedLegacyPaymentCount != 0 || plan.QuarantinedSweepCount != 0 {
			t.Fatal("the planner restored, reviewed or sampled a closed payment", plan.RestoredLegacyPaymentCount, plan.UnresolvedLegacyPaymentCount, plan.QuarantinedSweepCount)
		}
	}

	after := f.payments()
	for _, paymentId := range closedIds {
		if after[paymentId] != payments[paymentId] {
			t.Fatalf("closed payment %s changed:\n%s\n%s", paymentId, payments[paymentId], after[paymentId])
		}
	}
	var closedSweeps, restored int
	var bonusOwner server.Id
	server.Db(f.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(f.ctx, `SELECT
			(SELECT COUNT(*) FROM transfer_escrow_sweep WHERE payment_id=ANY($1)),
			(SELECT COUNT(*) FROM audit_account_payment WHERE event_type='legacy_components_restored'),
			(SELECT payment_id FROM account_payment_bonus WHERE origin_payment_id=$2)`, closedIds, revenueA).Scan(&closedSweeps, &restored, &bonusOwner))
	})
	if closedSweeps != len(closedIds) || restored != 0 || bonusOwner != revenueA {
		t.Fatal("a closed payment's sweeps or bonus moved, or it was restored", closedSweeps, restored, bonusOwner)
	}
	if canceled := model.CancelHungAccountPayments(f.ctx, server.NowUtc()); canceled != 0 {
		t.Fatal("the hung-payment sweep canceled again", canceled)
	}
}

// 6. The census totals, by reason and by plan week and reason, match the
// payments exactly, with the sender's fee boundary, the reason order and the
// week boundary at Monday 00:00 UTC.
func TestPayoutsCloseParkedTotalsByWeekAndReason(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newCloseParkedTestFixture(t)
		networkId, walletId := f.paid()
		bare := f.bareNetwork()
		// a payout wallet that is not active, and a wallet of another network
		inactiveNetwork := f.bareNetwork()
		inactiveWallet := f.wallet(inactiveNetwork, "MATIC", "0x"+strings.ReplaceAll(inactiveNetwork.String(), "-", ""), false)
		f.exec(`INSERT INTO payout_wallet(network_id,wallet_id) VALUES($1,$2)`, inactiveNetwork, inactiveWallet)
		foreignNetwork := f.bareNetwork()
		// a network that connected a wallet after its payment was planned
		laterNetwork, _ := f.paid()

		sunday := time.Date(2026, 10, 25, 23, 59, 59, 999999000, time.UTC)
		monday := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
		earlier := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

		// week 2026-10-19
		f.pending(bare, nil, sunday, model.UsdToNanoCents(3))
		f.pending(inactiveNetwork, &inactiveWallet, sunday, 1_000_000_001)
		f.pending(foreignNetwork, walletId, sunday, 7)
		f.pending(networkId, walletId, sunday, 10_000_000)
		f.pending(networkId, walletId, sunday, 0)
		f.pending(networkId, walletId, sunday, 10_000_001)
		f.pending(laterNetwork, nil, sunday, model.UsdToNanoCents(2))
		// week 2026-10-26
		f.review(f.pending(networkId, walletId, monday, model.UsdToNanoCents(4)))
		f.review(f.pending(bare, nil, monday, 9_999_999))
		f.review(f.pending(networkId, walletId, monday, 5))
		f.pending(networkId, walletId, monday, 123_456_789_012)
		// week 2026-10-12: a manual reimbursement keeps its pinned wallet, even
		// when its network has an active payout wallet
		pinnedInactive := f.wallet(networkId, "MATIC", "0x00000000000000000000000000000000000000c7", false)
		f.reimbursement(f.pending(networkId, &pinnedInactive, earlier, model.UsdToNanoCents(6)))
		f.reimbursement(f.pending(networkId, walletId, earlier, model.UsdToNanoCents(7)))

		report := f.dryRun()
		total := func(week string, reason string, count int, payout model.NanoCents) PayoutCloseParkedTotal {
			return PayoutCloseParkedTotal{Week: week, Reason: reason, Count: count, PayoutNanoCents: payout}
		}
		wantWeeks := []PayoutCloseParkedTotal{
			total("2026-10-12", model.UsdcParkedWalletLess, 1, model.UsdToNanoCents(6)),
			total("2026-10-12", model.UsdcParkedOther, 1, model.UsdToNanoCents(7)),
			total("2026-10-19", model.UsdcParkedWalletLess, 3, model.UsdToNanoCents(3)+1_000_000_001+7),
			total("2026-10-19", model.UsdcParkedDust, 2, 10_000_000),
			total("2026-10-19", model.UsdcParkedOther, 2, 10_000_001+model.UsdToNanoCents(2)),
			total("2026-10-26", model.UsdcParkedWalletLess, 1, 9_999_999),
			total("2026-10-26", model.UsdcParkedDust, 1, 5),
			total("2026-10-26", model.UsdcParkedReviewHeld, 1, model.UsdToNanoCents(4)),
			total("2026-10-26", model.UsdcParkedOther, 1, 123_456_789_012),
		}
		wantReasons := []PayoutCloseParkedTotal{
			total("", model.UsdcParkedWalletLess, 5, model.UsdToNanoCents(6)+model.UsdToNanoCents(3)+1_000_000_001+7+9_999_999),
			total("", model.UsdcParkedDust, 3, 10_000_000+5),
			total("", model.UsdcParkedReviewHeld, 1, model.UsdToNanoCents(4)),
			total("", model.UsdcParkedOther, 4, model.UsdToNanoCents(7)+10_000_001+model.UsdToNanoCents(2)+123_456_789_012),
		}
		deref := func(totals []*PayoutCloseParkedTotal) []PayoutCloseParkedTotal {
			values := []PayoutCloseParkedTotal{}
			for _, total := range totals {
				values = append(values, *total)
			}
			return values
		}
		if got := deref(report.Parked.Weeks); !slices.Equal(got, wantWeeks) {
			t.Fatalf("weeks %+v, want %+v", got, wantWeeks)
		}
		if got := deref(report.Parked.Reasons); !slices.Equal(got, wantReasons) {
			t.Fatalf("reasons %+v, want %+v", got, wantReasons)
		}
		wantTotal := model.NanoCents(0)
		for _, total := range wantReasons {
			wantTotal += total.PayoutNanoCents
		}
		if report.Parked.Count != 13 || report.Parked.PayoutNanoCents != wantTotal ||
			*report.Parked.ManualReimbursements != total("", "", 2, model.UsdToNanoCents(13)) {
			t.Fatal("totals", report.Parked.Count, report.Parked.PayoutNanoCents, wantTotal, report.Parked.ManualReimbursements)
		}

		// the operator's text carries the same totals, exactly, and no id
		text := report.Text(false)
		for _, line := range []string{
			"parked: 13 payment(s), 146.486789025 USD (146486789025 nano cents)\n",
			"    wallet_less        5          10.010000007 USD\n",
			"    dust               3           0.010000005 USD\n",
			"    2026-10-19 wallet_less        3           4.000000008 USD\n",
			"    2026-10-26 other              1         123.456789012 USD\n",
			"  of which manual reimbursements: 2, 13.000000000 USD\n",
		} {
			if !strings.Contains(text, line) {
				t.Fatalf("the report lacks %q:\n%s", line, text)
			}
		}
		for _, row := range report.Parked.Rows {
			if strings.Contains(text, row.PaymentId.String()) {
				t.Fatal("the default report prints a payment id")
			}
			if !strings.Contains(report.Text(true), row.PaymentId.String()) {
				t.Fatal("the report with ids omits a payment")
			}
		}
	})
}
