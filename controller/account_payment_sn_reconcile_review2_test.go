package controller

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tests for the §12 amendments of SN-RECONCILE.md, on the fixture of
// account_payment_sn_reconcile_test.go. No Circle endpoint is ever called.

// A8: before the fix the planner moved a canceled row's sweeps into its
// network's next plan, whose completed payout's sweeps were reaped later. Such
// a row is `replanned` and never paid. Its subsidy and its reliability are
// listed separately, each with its guidance: the re-plan's epoch already
// weighted the bytes in its subsidy pool, and reliability is the owner's
// decision. Without a row created after the cancellation and before the fix,
// or as RC-2, the row stays `sweeps_missing`.
func TestSnReconcileReplannedThenReapedIsNotOwedInFull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		subsidy, reliability := model.UsdToNanoCents(2), model.UsdToNanoCents(1)
		networkId, walletId, _ := f.network("MATIC")
		reaped := f.payment(networkId, &walletId, created, subsidy+reliability, f.contract(networkId, snReconcileTestClosed, model.UsdToNanoCents(4)))
		f.exec(`UPDATE account_payment SET subsidy_payout_nano_cents=$2,reliability_subsidy_nano_cents=$3 WHERE payment_id=$1`, reaped, subsidy, reliability)
		record := f.cancelWithRecord(reaped, "FAILED", "")
		// The next plan after the cancellation took its sweeps, was paid, and the
		// reaper deleted them.
		next := f.payment(networkId, &walletId, created.Add(31*snReconcileTestDay), 0)
		f.moveSweeps(reaped, next)
		f.exec(`UPDATE account_payment SET completed=true,complete_time=$2 WHERE payment_id=$1`, next, created.Add(32*snReconcileTestDay))
		f.exec(`DELETE FROM transfer_escrow_sweep WHERE payment_id=$1`, next)
		// The network's only later row was created after the fix.
		afterFixNetwork, afterFixWallet, _ := f.network("MATIC")
		afterFix := f.payment(afterFixNetwork, &afterFixWallet, created, model.UsdToNanoCents(2))
		f.cancelWithKey(afterFix)
		f.payment(afterFixNetwork, &afterFixWallet, model.SnReconcileReplanFixTime.Add(time.Second), 0)
		// RC-2 rows are never `replanned`.
		unretainedNetwork, unretainedWallet, _ := f.network("MATIC")
		unretained := f.payment(unretainedNetwork, &unretainedWallet, created, model.UsdToNanoCents(2))
		f.reserveUnjournaled(unretained)
		f.payment(unretainedNetwork, &unretainedWallet, created.Add(31*snReconcileTestDay), 0)

		before := f.tables()
		report := f.send()
		f.expect(report, map[server.Id]string{reaped: snReconcileReplanned, afterFix: "unresolved: sweeps_missing", unretained: "unresolved: sweeps_missing"})
		subsidyGuidance := "the re-plan's epoch already weighted these bytes in its subsidy pool; paying this by hand would reward them twice"
		reliabilityGuidance := "per-window, not paid by the re-plan; owner decides"
		item := snReconcileTestRow(t, report, reaped)
		if item.Subsidy == nil || *item.Subsidy != (SnReconcileReplannedAmount{NanoCents: subsidy, Guidance: subsidyGuidance}) ||
			item.Reliability == nil || *item.Reliability != (SnReconcileReplannedAmount{NanoCents: reliability, Guidance: reliabilityGuidance}) || item.OwedNanoCents != 0 {
			t.Fatal("replanned subsidy and reliability", item.Subsidy, item.Reliability)
		}
		for _, paymentId := range []server.Id{afterFix, unretained} {
			if other := snReconcileTestRow(t, report, paymentId); other.Subsidy != nil || other.Reliability != nil {
				t.Fatal("a sweeps_missing row listed subsidy or reliability", paymentId)
			}
		}
		if len(report.Revived) != 0 || len(report.Skipped) != 0 || f.tables() != before {
			t.Fatal("a replanned or sweeps_missing row was revived", report.Revived)
		}
		lines := "  " + reaped.String() + " network " + networkId.String() + " canceled_with_evidence replanned, owed 0.000000 USD, circle " + record + " FAILED -\n" +
			"    subsidy 2.000000 USD: " + subsidyGuidance + "\n" +
			"    reliability 1.000000 USD: " + reliabilityGuidance + "\n"
		if !strings.Contains(report.Text(), lines) || strings.Count(report.Text(), "    subsidy ") != 1 || strings.Count(report.Text(), "    reliability ") != 1 {
			t.Fatalf("report text:\n%s", report.Text())
		}
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		var document struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		for _, row := range document.Rows {
			subsidyField, hasSubsidy := row["subsidy"].(map[string]any)
			reliabilityField, hasReliability := row["reliability"].(map[string]any)
			replanned := row["payment_id"] == reaped.String()
			if hasSubsidy != replanned || hasReliability != replanned || (replanned &&
				(len(subsidyField) != 2 || subsidyField["nano_cents"] != float64(subsidy) || subsidyField["guidance"] != subsidyGuidance ||
					len(reliabilityField) != 2 || reliabilityField["nano_cents"] != float64(reliability) || reliabilityField["guidance"] != reliabilityGuidance)) {
				t.Fatal("JSON row", row)
			}
		}
	})
}

// A9: an unattributed transfer that first appears in the --send re-listing may
// have paid any row, so every payable row is skipped and stays payable. An
// unattributed transfer the census had, or a new transfer to our own or to a
// registered address, skips nothing more.
func TestSnReconcileSendRelistNewUnattributedSkipsAll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		f.ledger.developerWallets = []CircleWallet{{ID: "sn-reconcile-developer-wallet", Address: "0xdeveloper"}}
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkA, walletA, _ := f.network("MATIC")
		first := f.legacy(networkA, &walletA, created, model.UsdToNanoCents(1))
		f.cancelWithKey(first)
		networkB, walletB, _ := f.network("SOL")
		second := f.legacy(networkB, &walletB, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(second)
		_, _, registered := f.network("MATIC")
		f.list(snReconcileTestMaticWallet, f.transfer("known-unattributed", "COMPLETE", "0xnobody", created))

		digest := f.dry().Digest
		f.ledger.beforeBalance = func() {
			f.list(snReconcileTestMaticWallet, f.transfer("new-unattributed", "INITIATED", "0xsomebody", server.NowUtc()))
		}
		before := f.tables()
		report := f.sendWith(digest)
		skips := []string{first.String() + " unattributed_orphan new-unattributed", second.String() + " unattributed_orphan new-unattributed"}
		if len(report.Revived) != 0 || !slices.Equal(snReconcileTestSkips(report), skips) || f.tables() != before || len(f.scheduled()) != 0 {
			t.Fatal("a new unattributed transfer did not skip every payable row", report.Revived, snReconcileTestSkips(report))
		}
		if !slices.ContainsFunc(report.Orphans, func(orphan *SnReconcileOrphan) bool { return orphan.Id == "new-unattributed" && orphan.Unattributed }) ||
			!strings.Contains(report.Text(), "  "+first.String()+" unattributed_orphan, orphans new-unattributed\n") {
			t.Fatalf("report:\n%s", report.Text())
		}

		// The rows stay payable, and the next census binds the transfer.
		next := f.dry()
		f.expect(next, map[server.Id]string{first: snReconcilePayable, second: snReconcilePayable})
		f.ledger.beforeBalance = func() {
			f.list(snReconcileTestMaticWallet, f.transfer("new-to-developer", "COMPLETE", " 0xDeveloper", server.NowUtc()),
				f.transfer("new-to-registered", "COMPLETE", registered, server.NowUtc()))
		}
		if report := f.sendWith(next.Digest); !slices.Equal(report.Revived, []server.Id{first, second}) || len(report.Skipped) != 0 {
			t.Fatal("a known unattributed or a new attributed transfer skipped a row", report.Revived, snReconcileTestSkips(report))
		}
	})
}

// A11: funding counts a row an earlier run revived only until the sender
// submits it, that is while it has neither a payment record nor a tx hash.
func TestSnReconcileFundingIgnoresSubmittedRevivals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		submitted := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(10))
		f.cancelWithKey(submitted)
		hashed := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(4))
		f.cancelWithKey(hashed)
		unsubmitted := f.legacy(networkId, &walletId, created.Add(2*time.Hour), model.UsdToNanoCents(2))
		f.cancelWithKey(unsubmitted)
		if report := f.send(); !slices.Equal(report.Revived, []server.Id{submitted, hashed, unsubmitted}) {
			t.Fatal("not revived", report.Revived)
		}
		if funding := snReconcileTestFunding(t, f.dry(), "MATIC"); funding.RevivedCount != 3 ||
			funding.RevivedNanoCents != model.UsdToNanoCents(9.99)+model.UsdToNanoCents(3.99)+model.UsdToNanoCents(1.99) {
			t.Fatalf("funding before submission = %+v", funding)
		}
		// The sender submits one row; another carries a hash without a record.
		if result, err := AdvancePayment(&AdvancePaymentArgs{PaymentId: submitted}, f.session); err != nil || result.Complete || len(f.posts) != 1 {
			t.Fatal("the sender did not submit the revived row", result, err)
		}
		payment, err := model.GetPayment(f.ctx, submitted)
		if err != nil || payment.PaymentRecord == nil || payment.TxHash != nil {
			t.Fatal("submitted row", payment, err)
		}
		f.exec(`UPDATE account_payment SET tx_hash='0xhashed' WHERE payment_id=$1`, hashed)
		// The balance covers a new payable row only without the submitted revivals.
		payable := f.legacy(networkId, &walletId, created.Add(3*time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(payable)
		f.ledger.balances[snReconcileTestMaticWallet+"/"+snReconcileTestMaticUsdc] = model.UsdToNanoCents(3)
		report := f.dry()
		if funding := snReconcileTestFunding(t, report, "MATIC"); funding.PayableCount != 1 || funding.RevivedCount != 1 ||
			funding.RevivedNanoCents != model.UsdToNanoCents(1.99) || funding.ShortfallNanoCents != 0 {
			t.Fatalf("funding after submission = %+v", funding)
		}
		if report := f.sendWith(report.Digest); !slices.Equal(report.Revived, []server.Id{payable}) {
			t.Fatal("a submitted revival was still funded", report.Revived)
		}
	})
}

// A12: a circle.yml key the payout getters read that is missing, null or not a
// string refuses the command before any getter runs; the real getters panic on
// it. A key that is present reaches the getters, and an empty value is refused
// there (RC-5).
func TestSnReconcileMissingCircleKeyRefuses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		networkId, walletId, _ := f.network("MATIC")
		f.cancelWithKey(f.legacy(networkId, &walletId, snReconcileTestCutoff.Add(-30*snReconcileTestDay), model.UsdToNanoCents(1)))
		// The keys the getters of circle_client_controller.go read, with their getters.
		keys := []string{"polygon_wallet_id", "polygon_usdc_address", "solana_wallet_id", "solana_usdc_address"}
		getters := []*func() string{&polygonWalletId, &polygonUSDCAddress, &solanaWalletId, &solanaUSDCAddress}
		values := []string{snReconcileTestMaticWallet, snReconcileTestMaticUsdc, snReconcileTestSolWallet, snReconcileTestSolUsdc}
		for _, getter := range getters {
			*getter = func() string { panic("a circle.yml getter ran before its key was checked") }
		}
		// circle.yml with every key present and empty, except `key` set to `value`
		// or left out when `value` is empty.
		config := func(key string, value string) []byte {
			yml := "wallet_set_id: ''\ncircle:\n  api_token: ''\n"
			for _, name := range keys {
				switch {
				case name != key:
					yml += "  " + name + ": ''\n"
				case value != "":
					yml += "  " + name + ": " + value + "\n"
				}
			}
			return []byte(yml)
		}
		run := func(content []byte) (*SnReconcileReport, error) {
			pop := server.Vault.PushSimpleResource("circle.yml", content)
			defer pop()
			return SnReconcile(f.session, false, "")
		}
		type refusal struct {
			want    string
			content []byte
		}
		// An unreadable file, no `circle` map, and a `circle` that is no map.
		refusals := []refusal{{"reading circle.yml", []byte("circle: [\n")}, {"circle.yml has no string value for circle.", []byte("wallet_set_id: ''\n")},
			{"circle.yml has no string value for circle.", []byte("circle: ''\n")}}
		for _, key := range keys {
			for _, value := range []string{"", "null", "5", "[]"} {
				refusals = append(refusals, refusal{"circle.yml has no string value for circle." + key, config(key, value)})
			}
		}
		for _, refusal := range refusals {
			report, err := run(refusal.content)
			if report != nil || err == nil || !strings.Contains(err.Error(), refusal.want) {
				t.Fatal("not refused", refusal.want, string(refusal.content), report, err)
			}
		}
		if len(f.reads) != 0 || len(f.ledger.listed) != 0 || f.ledger.balanceReads != 0 || f.ledger.developerReads != 0 {
			t.Fatal("a refused command read Circle")
		}
		// Present keys reach the getters.
		for i, getter := range getters {
			value := values[i]
			*getter = func() string { return value }
		}
		if report, err := run(config("", "")); err != nil || len(report.Rows) != 1 {
			t.Fatal("present keys were refused", err)
		}
	})
}
