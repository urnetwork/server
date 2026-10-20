// SIGNALS.md §2.21: execute the five-store observation contract on local rows,
// then pass the actual aggregates through the parser and public Signal adapter.
package monitor

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// A transaction freezes now() and shadows all relations used by this signal.
func paymentReconciliationX402SqlFixture(t testing.TB, tx server.PgTx) {
	t.Helper()
	server.RaisePgResult(tx.Exec(t.Context(), `
CREATE TEMP TABLE payment_reconciliation_event (
    store text, action text, evidence text, run_id uuid, details json,
    dry_run boolean, event_time timestamptz
) ON COMMIT DROP;
CREATE TEMP TABLE payment_reconciliation_watermark (store text, update_time timestamptz) ON COMMIT DROP;
CREATE TEMP TABLE pending_task (function_name text, reschedule_error text, claim_time timestamptz) ON COMMIT DROP;
INSERT INTO pending_task VALUES ('github.com/urnetwork/server/taskworker/work.PaymentReconcile',NULL,NULL);
INSERT INTO payment_reconciliation_watermark
SELECT store,now()-interval '10 minutes' FROM (VALUES ('apple'),('google'),('solana'),('stripe'),('x402')) s(store);
`))
	server.RaisePgResult(tx.Exec(t.Context(), `INSERT INTO payment_reconciliation_event
VALUES ('all','heartbeat',NULL,$1,NULL,false,now()-interval '10 minutes')`, server.NewId()))
}

// The owning queries return only bounded strings/counts/ages, never private cells.
func paymentReconciliationX402SqlRead(t testing.TB, tx server.PgTx, query string) []Row {
	t.Helper()
	rows, err := tx.Query(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []Row{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		row := make(Row, len(values))
		for index, value := range values {
			row[index] = fmt.Sprint(value)
		}
		result = append(result, row)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	return result
}

// Read all three production SELECTs and invoke their actual parsers/adapter.
func paymentReconciliationX402SqlAlerts(t testing.TB, tx server.PgTx) []Alert {
	t.Helper()
	health := paymentReconciliationX402SqlRead(t, tx, paymentReconciliationHealthQuery)
	repairs := paymentReconciliationX402SqlRead(t, tx, paymentReconciliationRepairQuery)
	terminal := paymentReconciliationX402SqlRead(t, tx, paymentReconciliationUnfulfillableQuery)
	alerts, err := NewPaymentReconciliationSignal().Run(t.Context(), syntheticSettings(
		paymentReconciliationSourceWithUnfulfillable(health, repairs, terminal),
	))
	if err != nil {
		t.Fatal("actual reconciliation SQL aggregates were rejected")
	}
	for _, alert := range alerts {
		for _, secret := range []string{"synthetic-private-x402-evidence", "synthetic-private-x402-details"} {
			if strings.Contains(alert.Markdown(), secret) {
				t.Fatal("private audit data escaped the public aggregate")
			}
		}
	}
	return alerts
}

// A fresh heartbeat and four healthy siblings must not hide the fifth local leg.
func TestPaymentReconciliationX402SqlHealthBoundaries(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		server.Tx(tb.Context(), func(tx server.PgTx) {
			paymentReconciliationX402SqlFixture(tb, tx)
			for _, c := range []struct {
				name         string
				age          int
				errors       int
				skipped      int
				wantClass    string
				wantSeverity Severity
			}{
				{name: "empty local pass", age: 600},
				{name: "exact three hours", age: 10800},
				{name: "over three hours", age: 10801, wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityWarn},
				{name: "exact six hours", age: 21600, wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityWarn},
				{name: "over six hours", age: 21601, wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityPage},
				{name: "missing watermark", age: -1, wantClass: "payment-reconciliation-watermark-stale", wantSeverity: SeverityPage},
				{name: "one local error", age: 600, errors: 1, wantClass: "payment-reconciliation-store-error", wantSeverity: SeverityWarn},
				{name: "two local errors", age: 600, errors: 2, wantClass: "payment-reconciliation-store-error", wantSeverity: SeverityPage},
				{name: "unexpected local skip", age: 600, skipped: 1, wantClass: "payment-reconciliation-store-skipped", wantSeverity: SeverityPage},
			} {
				server.RaisePgResult(tx.Exec(tb.Context(), `DELETE FROM payment_reconciliation_event WHERE store <> 'all';
DELETE FROM payment_reconciliation_watermark WHERE store='x402'`))
				if c.age >= 0 {
					server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_watermark VALUES ('x402',now()-$1::int*interval '1 second')`, c.age))
				}
				for range c.errors {
					server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event VALUES
('x402','error','synthetic-private-x402-evidence',$1,'{"private":"synthetic-private-x402-details"}',false,now())`, server.NewId()))
				}
				if c.skipped > 0 {
					server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event VALUES
('x402','skipped_store',NULL,$1,NULL,false,now())`, server.NewId()))
				}
				// Dry-run error/skip/repair events cannot pollute live health or repair counts.
				server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event
SELECT 'x402',action,'synthetic-private-x402-evidence',$1,NULL,true,now()
FROM (VALUES ('error'),('skipped_store'),('credited')) a(action)`, server.NewId()))
				alerts := paymentReconciliationX402SqlAlerts(tb, tx)
				if c.wantClass == "" {
					if len(alerts) != 0 {
						tb.Fatalf("%s: empty/healthy local leg was not healthy", c.name)
					}
					continue
				}
				if len(alerts) != 1 || alerts[0].Class != c.wantClass || alerts[0].Target != "x402" || alerts[0].Severity != c.wantSeverity {
					tb.Fatalf("%s: fifth-store boundary lost or siblings polluted: %+v", c.name, alerts)
				}
				if strings.Contains(alerts[0].Action, "Run the credentials signal") || strings.Contains(alerts[0].Action, "Restore credentials") {
					tb.Fatal("local leg received external credential instructions")
				}
				if !strings.Contains(alerts[0].Markdown(), "local") {
					tb.Fatal("x402 diagnosis lost the local authority boundary")
				}
			}
		})
	})
}

// The real repair SELECT admits only same-run recent live repairs; x402 must
// parse as a legitimate bounded WARN rather than fail as an unknown store.
func TestPaymentReconciliationX402SqlRepairScopeAndUnknownStore(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		server.Tx(tb.Context(), func(tx server.PgTx) {
			paymentReconciliationX402SqlFixture(tb, tx)
			runId := server.NewId()
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event VALUES
('all','heartbeat',NULL,$1,NULL,false,now()),
('x402','credited','synthetic-private-x402-evidence',$1,'{"private":"synthetic-private-x402-details"}',false,now()),
('x402','credited','synthetic-private-x402-evidence',$1,NULL,true,now()),
('x402','credited','synthetic-private-x402-evidence',$1,NULL,false,now()-interval '25 hours'),
('x402','credit_unfulfillable','synthetic-private-x402-evidence',$1,NULL,false,now())`, runId))
			// A recent event with no matching heartbeat run must stay excluded.
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event VALUES
('x402','credited','synthetic-private-x402-evidence',$1,NULL,false,now())`, server.NewId()))
			// A dry-run or expired heartbeat cannot qualify a recent live repair.
			for _, dryRun := range []bool{true, false} {
				otherRun := server.NewId()
				age := 0
				if !dryRun {
					age = 25 * 60 * 60
				}
				server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event VALUES
('all','heartbeat',NULL,$1,NULL,$2,now()-$3::int*interval '1 second'),
('x402','credited',NULL,$1,NULL,false,now())`, otherRun, dryRun, age))
			}
			rows := paymentReconciliationX402SqlRead(tb, tx, paymentReconciliationRepairQuery)
			if len(rows) != 1 || rows[0][0] != "x402" || rows[0][1] != "credited" || rows[0][2] != "1" {
				tb.Fatal("owning repair SQL lost same-run/time/dry-run scope")
			}
			alerts := paymentReconciliationX402SqlAlerts(tb, tx)
			if len(alerts) != 1 || alerts[0].Class != "payment-reconciliation-repair" || alerts[0].Target != "x402" || alerts[0].Frame != "action=credited" || alerts[0].Severity != SeverityWarn {
				tb.Fatal("legitimate x402 repair did not reach its bounded public WARN")
			}
			if !strings.Contains(alerts[0].Mechanism, "local post-settlement recovery") {
				tb.Fatal("local repair received external notification semantics")
			}
			// Do not relax the fail-closed contract for unsupported stores.
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event VALUES
('synthetic-unsupported','credited',NULL,$1,NULL,false,now())`, runId))
			rows = paymentReconciliationX402SqlRead(tb, tx, paymentReconciliationRepairQuery)
			repairRows, err := (&sourceRunner{source: paymentReconciliationSource(nil, rows)}).pg(tb.Context(), paymentReconciliationRepairQuery)
			if err != nil {
				tb.Fatal("actual repair SQL rows could not reach the existing row adapter")
			}
			if _, err := paymentReconciliationRepairFindings(repairRows); err == nil {
				tb.Fatal("unknown repair store was silently dropped")
			}
			health := paymentReconciliationX402SqlRead(tb, tx, paymentReconciliationHealthQuery)
			unknownAlerts, err := NewPaymentReconciliationSignal().Run(tb.Context(), syntheticSettings(paymentReconciliationSource(health, rows)))
			if err != nil || len(unknownAlerts) != 1 || unknownAlerts[0].Class != "cannot-observe" ||
				unknownAlerts[0].Target != "payment-reconciliation/repairs" || !isVisibilityAlert(unknownAlerts[0]) {
				tb.Fatal("unknown repair store became public healthy observation or lost component visibility")
			}
		})
	})
}

// A failed local leg must neither erase nor misattribute an independent store.
func TestPaymentReconciliationX402SqlPreservesIndependentSiblingFailure(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		server.Tx(tb.Context(), func(tx server.PgTx) {
			paymentReconciliationX402SqlFixture(tb, tx)
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO payment_reconciliation_event
SELECT store,'error',NULL,$1,NULL,false,now() FROM (VALUES ('apple'),('x402'),('x402')) s(store)`, server.NewId()))
			alerts := paymentReconciliationX402SqlAlerts(tb, tx)
			if len(alerts) != 2 {
				tb.Fatal("independent store failures were lost or duplicated")
			}
			seen := map[string]Severity{}
			for _, alert := range alerts {
				if alert.Class != "payment-reconciliation-store-error" {
					tb.Fatal("unexpected sibling failure class")
				}
				seen[alert.Target] = alert.Severity
			}
			if seen["apple"] != SeverityWarn || seen["x402"] != SeverityPage {
				tb.Fatal("per-store error severity was not preserved")
			}
		})
	})
}
