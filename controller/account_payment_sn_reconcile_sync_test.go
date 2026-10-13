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

// Tests for what origin/main's legacy payment tools change about the census,
// on the fixture of account_payment_sn_reconcile_test.go.

// RC-12: a row the legacy correction (model/account_payment_legacy_correction.go)
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
