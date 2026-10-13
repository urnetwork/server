package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tests for what origin/main's legacy payment tools change about the census,
// on the fixture of account_payment_sn_reconcile_test.go.

// RC-23: a row the legacy correction (model/account_payment_legacy_correction.go)
// would lower is `inflated` and never paid as recorded; the report shows its
// corrected payout. A subsidy or reliability the correction keeps, and a row
// without either, stay payable.
func TestSnReconcileInflatedLegacyAmountIsNotPaid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		// A canceled row with a subsidy and a reliability subsidy in its payout.
		// Its plan epoch lasts a microsecond unless `window` widens it.
		stuck := func(subsidy model.NanoCents, reliability model.NanoCents, window time.Duration) (server.Id, server.Id) {
			networkId, walletId, _ := f.network("MATIC")
			paymentId := f.payment(networkId, &walletId, created, subsidy+reliability, f.contract(networkId, snReconcileTestClosed, model.UsdToNanoCents(4)))
			f.exec(`UPDATE account_payment SET subsidy_payout_nano_cents=$2,reliability_subsidy_nano_cents=$3 WHERE payment_id=$1`, paymentId, subsidy, reliability)
			if 0 < window {
				f.exec(`UPDATE subsidy_payment SET end_time=start_time+$2*INTERVAL '1 microsecond' WHERE payment_plan_id=(SELECT payment_plan_id FROM account_payment WHERE payment_id=$1)`,
					paymentId, window.Microseconds())
			}
			f.cancelWithKey(paymentId)
			return paymentId, networkId
		}
		// The test subsidy period is a day: a reliability subsidy over a shorter
		// window is paid pro rata.
		inflated, _ := stuck(0, model.UsdToNanoCents(2), 0)
		fullWindow, _ := stuck(0, model.UsdToNanoCents(2), 24*time.Hour)
		// The test subsidy is the floor alone, which the correction keeps.
		subsidy, _ := stuck(model.UsdToNanoCents(1), 0, 0)
		plainNetwork, plainWallet, _ := f.network("MATIC")
		plain := f.legacy(plainNetwork, &plainWallet, created, model.UsdToNanoCents(4))
		f.cancelWithKey(plain)

		before := f.payments()
		report := f.send()
		f.expect(report, map[server.Id]string{inflated: "unresolved: inflated", fullWindow: snReconcilePayable, subsidy: snReconcilePayable, plain: snReconcilePayable})
		// 2 USD over a microsecond of the day rounds to nothing.
		if corrected := snReconcileTestRow(t, report, inflated).CorrectedPayoutNanoCents; corrected == nil || *corrected != model.UsdToNanoCents(4) {
			t.Fatal("corrected payout", corrected)
		}
		for _, paymentId := range []server.Id{fullWindow, subsidy, plain} {
			if snReconcileTestRow(t, report, paymentId).CorrectedPayoutNanoCents != nil {
				t.Fatal("a corrected payout for", paymentId)
			}
		}
		if !slices.Equal(report.Revived, snReconcileTestRevivedOrder(report, fullWindow, subsidy, plain)) {
			t.Fatal("revived", report.Revived)
		}
		after := f.payments()
		if after[inflated] != before[inflated] {
			t.Fatal("an inflated row was written")
		}
		text := report.Text()
		if !strings.Contains(text, "  "+inflated.String()+" network ") || !strings.Contains(text, " canceled_with_evidence unresolved: inflated, owed 0.000000 USD, corrected payout 4.000000 USD\n") ||
			strings.Count(text, ", corrected payout ") != 1 {
			t.Fatalf("report text:\n%s", text)
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
			corrected, ok := row["corrected_payout_nano_cents"]
			if (row["payment_id"] == inflated.String()) != ok || (ok && corrected != float64(model.UsdToNanoCents(4))) {
				t.Fatal("JSON row", row)
			}
		}
	})
}

// RC-24: upstream's reconcile-legacy-attempts decides RC-2 rows. One that
// passes every check is `unresolved: upstream_scope`: it is outside the
// payable set, the digest and funding, and never revived. An RC-2 row with
// other evidence keeps its reason. RC-1 rows are revived as before.
func TestSnReconcileUnretainedKeyIsReportOnly(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newSnReconcileTestFixture(t)
		created := snReconcileTestCutoff.Add(-30 * snReconcileTestDay)
		networkId, walletId, _ := f.network("MATIC")
		unretained := f.legacy(networkId, &walletId, created, model.UsdToNanoCents(5))
		f.reserveUnjournaled(unretained)
		canceled := f.legacy(networkId, &walletId, created.Add(time.Hour), model.UsdToNanoCents(1))
		f.cancelWithKey(canceled)
		orphanNetwork, orphanWallet, orphanAddress := f.network("MATIC")
		blocked := f.legacy(orphanNetwork, &orphanWallet, created, model.UsdToNanoCents(1))
		f.reserveUnjournaled(blocked)
		f.list(snReconcileTestMaticWallet, f.transfer("hidden-transfer", "COMPLETE", orphanAddress, created))

		report := f.dry()
		f.expect(report, map[server.Id]string{unretained: "unresolved: upstream_scope", canceled: snReconcilePayable, blocked: "unresolved: orphan_transfer"})
		item, funding := snReconcileTestRow(t, report, unretained), snReconcileTestFunding(t, report, "MATIC")
		if item.OwedNanoCents != 0 || item.Chain != "" || funding.PayableCount != 1 || funding.SendNanoCents != model.UsdToNanoCents(0.99) ||
			!slices.Equal(snReconcileTestRow(t, report, blocked).OrphanIds, []string{"hidden-transfer"}) ||
			!strings.Contains(report.Text(), "  "+unretained.String()+" network "+networkId.String()+" unretained_key unresolved: upstream_scope, owed 0.000000 USD\n") {
			t.Fatalf("report:\n%s", report.Text())
		}
		// The digest's payable list holds the RC-1 row alone.
		digest := sha256.Sum256([]byte(fmt.Sprintf("payable %s %d %d MATIC", canceled, model.UsdToNanoCents(1), model.UsdToNanoCents(0.99))))
		if report.Digest != hex.EncodeToString(digest[:]) {
			t.Fatal("the digest binds more than the RC-1 row", report.Digest)
		}

		before := f.payments()
		sent := f.sendWith(report.Digest)
		if !slices.Equal(sent.Revived, []server.Id{canceled}) || !slices.Equal(f.scheduled(), []server.Id{canceled}) || len(sent.Skipped) != 0 {
			t.Fatal("not only the RC-1 row was revived", sent.Revived, snReconcileTestSkips(sent))
		}
		after := f.payments()
		for _, paymentId := range []server.Id{unretained, blocked} {
			if after[paymentId] != before[paymentId] || f.auditCount(paymentId, "sn_reconcile_revived") != 0 {
				t.Fatal("an RC-2 row was written", paymentId)
			}
		}
	})
}

// The given payable rows in the order the report lists them.
func snReconcileTestRevivedOrder(report *SnReconcileReport, paymentIds ...server.Id) []server.Id {
	ordered := []server.Id{}
	for _, item := range report.Rows {
		if slices.Contains(paymentIds, item.PaymentId) {
			ordered = append(ordered, item.PaymentId)
		}
	}
	return ordered
}
