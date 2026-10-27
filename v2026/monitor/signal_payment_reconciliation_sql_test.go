// Execute the audit classifiers against connection-local fixtures. The webhook
// and reconciliation writers share an action but own different leg contracts.
package monitor

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Retained from monitor revision 93057ac: it misclassifies the S11 webhook
// writer's complete unresolved-destination record as an invalid disposition.
const paymentReconciliationCreditOnlyControlQuery = `
SELECT count(DISTINCT evidence)::bigint,
       count(*)::bigint,
       count(DISTINCT run_id)::bigint,
       count(*) FILTER (
           WHERE evidence IS NULL
              OR btrim(evidence) = ''
              OR COALESCE(details::jsonb ->> 'reason', '') NOT IN ('destination_deleted', 'destination_unresolved')
              OR (details::jsonb ->> 'leg') IS DISTINCT FROM 'credit'
       )::bigint,
       COALESCE(extract(epoch FROM now() - max(event_time))::bigint, -1)
FROM payment_reconciliation_event
WHERE store = 'stripe'
  AND action = 'credit_unfulfillable'
  AND NOT dry_run
  AND event_time >= now() - interval '24 hours'
`

// Temporary tables cover every relation in the two owning SELECTs, so no
// application history or provider service participates in these controls.
func paymentReconciliationAuditSqlFixture(t testing.TB, conn server.PgConn) {
	t.Helper()
	server.RaisePgResult(conn.Exec(t.Context(), `
CREATE TEMP TABLE payment_reconciliation_event (
    store text, action text, evidence text, run_id uuid, details json,
    dry_run boolean, event_time timestamptz
);
CREATE TEMP TABLE subscription_renewal (
    network_id uuid, market text, subscription_type text,
    start_time timestamptz, end_time timestamptz
);
CREATE TEMP TABLE network (network_id uuid);
CREATE TEMP TABLE transfer_balance (
    network_id uuid, pro boolean, start_time timestamptz, end_time timestamptz
);
CREATE TEMP TABLE solana_unfulfilled_payment (reason text, record_time timestamptz);
CREATE TEMP TABLE apple_subscription_transaction (transaction_id text);
CREATE TEMP TABLE play_purchase_binding (purchase_token text);
`))
}

// Read only the five aggregate cells exported by the reconciliation query.
func paymentReconciliationAuditSqlRead(t testing.TB, conn server.PgConn, query string) pgRow {
	t.Helper()
	var values [5]int64
	server.Raise(conn.QueryRow(t.Context(), query).Scan(
		&values[0], &values[1], &values[2], &values[3], &values[4],
	))
	row := make(pgRow, len(values))
	for index, value := range values {
		row[index] = strconv.FormatInt(value, 10)
	}
	return row
}

// Insert synthetic private evidence with an independent writer provenance ID.
func paymentReconciliationAuditSqlInsert(
	t testing.TB, conn server.PgConn, evidence any, details any,
	store string, action string, dryRun bool, ageSeconds int64,
) {
	t.Helper()
	var detailsJson *string
	if details != nil {
		encoded, err := json.Marshal(details)
		if err != nil {
			t.Fatal(err)
		}
		value := string(encoded)
		detailsJson = &value
	}
	server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO payment_reconciliation_event
    (store, action, evidence, run_id, details, dry_run, event_time)
VALUES ($1, $2, $3, $4, $5::json, $6, now() - $7::bigint * interval '1 second')`,
		store, action, evidence, server.NewId(), detailsJson, dryRun, ageSeconds,
	))
}

// The S11 webhook tuple must reach its existing WARN through the actual payment
// failure SELECT, including when reconciliation repeats the same invoice.
// Unowned App Store and Play purchases form their own store groups; every
// group must still classify and keep private evidence out of its WARN.
func paymentReconciliationAuditSqlRequireWarn(t testing.TB, conn server.PgConn) {
	t.Helper()
	stripeGroups := 0
	result, err := conn.Query(t.Context(), paymentFailuresQuery)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var kind, store string
			var count, oldestAge, newestAge int64
			server.Raise(result.Scan(&kind, &store, &count, &oldestAge, &newestAge))
			issue, err := paymentFailureFinding(kind, store, count, oldestAge, newestAge)
			if err != nil {
				t.Fatal("the payment failure query returned an unclassifiable group")
			}
			if kind == "credit_unfulfillable" && store == "stripe" {
				stripeGroups += 1
				if count != 1 {
					t.Fatal("the payment failure query lost the distinct unresolved invoice")
				}
				if issue.class != "payment-credit-unfulfillable" || issue.tier != tierWarn {
					t.Fatal("the unresolved payment no longer retains its separate WARN")
				}
			}
			rendered := alertFromFinding(syntheticSettings(&syntheticSource{}), "2.22", "payment-failures", "Payment failures", issue).Markdown()
			for _, forbidden := range []string{"in_private_audit", "in_other_private_audit", "synthetic-private-details", "payer@example.invalid"} {
				if strings.Contains(rendered, forbidden) {
					t.Fatal("private payment evidence escaped the aggregate WARN")
				}
			}
		}
	})
	if stripeGroups != 1 {
		t.Fatal("the payment failure query lost the distinct unresolved invoice")
	}
}

// The old owning query must reproduce the false PAGE; the corrected one must
// exclude only the complete unresolved-webhook tuple, preserving invalid rows.
func TestPaymentReconciliationWebhookAuditSqlContract(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			paymentReconciliationAuditSqlFixture(t, conn)
			cases := []struct {
				name     string
				evidence any
				details  any
				class    string
			}{
				{"valid webhook", "in_private_audit", map[string]any{"reason": "destination_unresolved", "leg": "webhook", "customer_email": "payer@example.invalid", "private": "synthetic-private-details"}, ""},
				{"deleted credit", "in_private_audit", map[string]any{"reason": "destination_deleted", "leg": "credit"}, "payment-reconciliation-credit-unfulfillable"},
				{"unresolved credit", "in_private_audit", map[string]any{"reason": "destination_unresolved", "leg": "credit"}, "payment-reconciliation-credit-unfulfillable"},
				{"deleted webhook", "in_private_audit", map[string]any{"reason": "destination_deleted", "leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"missing webhook evidence", nil, map[string]any{"reason": "destination_unresolved", "leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"empty webhook evidence", "", map[string]any{"reason": "destination_unresolved", "leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"blank webhook evidence", "   ", map[string]any{"reason": "destination_unresolved", "leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"missing credit evidence", nil, map[string]any{"reason": "destination_deleted", "leg": "credit"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"blank credit evidence", "   ", map[string]any{"reason": "destination_unresolved", "leg": "credit"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"missing leg", "in_private_audit", map[string]any{"reason": "destination_unresolved"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"null leg", "in_private_audit", map[string]any{"reason": "destination_unresolved", "leg": nil}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"empty leg", "in_private_audit", map[string]any{"reason": "destination_unresolved", "leg": ""}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"unknown leg", "in_private_audit", map[string]any{"reason": "destination_unresolved", "leg": "synthetic-private-details"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"object leg", "in_private_audit", map[string]any{"reason": "destination_unresolved", "leg": map[string]any{"source": "webhook"}}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"missing webhook reason", "in_private_audit", map[string]any{"leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"null webhook reason", "in_private_audit", map[string]any{"reason": nil, "leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"unknown webhook reason", "in_private_audit", map[string]any{"reason": "synthetic-private-details", "leg": "webhook"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"unknown credit reason", "in_private_audit", map[string]any{"reason": "synthetic-private-details", "leg": "credit"}, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"missing details", "in_private_audit", nil, "payment-reconciliation-credit-unfulfillable-invalid"},
				{"json null details", "in_private_audit", json.RawMessage(`null`), "payment-reconciliation-credit-unfulfillable-invalid"},
				{"scalar details", "in_private_audit", "synthetic-private-details", "payment-reconciliation-credit-unfulfillable-invalid"},
			}
			for _, test := range cases {
				server.RaisePgResult(conn.Exec(t.Context(), "TRUNCATE payment_reconciliation_event"))
				paymentReconciliationAuditSqlInsert(t, conn, test.evidence, test.details, "stripe", "credit_unfulfillable", false, 120)
				if test.name == "valid webhook" {
					control := paymentReconciliationAuditSqlRead(t, conn, paymentReconciliationCreditOnlyControlQuery)
					issue, err := paymentReconciliationUnfulfillableFinding([]pgRow{control})
					if err != nil || issue == nil || issue.class != "payment-reconciliation-credit-unfulfillable-invalid" || control[3] != "1" {
						t.Fatal("the old query did not reproduce the unresolved-webhook false PAGE")
					}
					paymentReconciliationAuditSqlRequireWarn(t, conn)
				}
				row := paymentReconciliationAuditSqlRead(t, conn, paymentReconciliationUnfulfillableQuery)
				issue, err := paymentReconciliationUnfulfillableFinding([]pgRow{row})
				if err != nil {
					t.Fatalf("%s: owning query returned an invalid aggregate: %v", test.name, err)
				}
				if test.class == "" {
					if issue != nil || strings.Join(row, ",") != "0,0,0,0,-1" {
						t.Fatal("a complete unresolved-webhook record still produces a reconciliation PAGE")
					}
					continue
				}
				if issue == nil || issue.class != test.class || issue.tier != tierPage || row[1] != "1" {
					t.Fatalf("%s: the credit or malformed audit record lost its PAGE", test.name)
				}
				rendered := alertFromFinding(syntheticSettings(&syntheticSource{}), "2.21", "payment-reconciliation", "Payment reconciliation", *issue).Markdown()
				if strings.Contains(rendered, "in_private_audit") || strings.Contains(rendered, "synthetic-private-details") {
					t.Fatal("private payment evidence escaped the reconciliation PAGE")
				}
			}
		})
	})
}

// Writer provenance is not invoice identity: excluding a valid webhook must
// never hide a reconciliation or malformed record for that same payment.
func TestPaymentReconciliationWebhookAuditSqlMixedWritersAndScope(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			paymentReconciliationAuditSqlFixture(t, conn)
			webhook := map[string]any{"reason": "destination_unresolved", "leg": "webhook"}
			credit := map[string]any{"reason": "destination_unresolved", "leg": "credit"}
			paymentReconciliationAuditSqlInsert(t, conn, "in_private_audit", webhook, "stripe", "credit_unfulfillable", false, 120)
			for range 2 {
				paymentReconciliationAuditSqlInsert(t, conn, "in_private_audit", credit, "stripe", "credit_unfulfillable", false, 60)
			}
			row := paymentReconciliationAuditSqlRead(t, conn, paymentReconciliationUnfulfillableQuery)
			issue, err := paymentReconciliationUnfulfillableFinding([]pgRow{row})
			if err != nil || issue == nil || issue.class != "payment-reconciliation-credit-unfulfillable" || strings.Join(row[:4], ",") != "1,2,2,0" {
				t.Fatal("a valid webhook hid a same-invoice credit disposition or distorted overlap counts")
			}
			paymentReconciliationAuditSqlRequireWarn(t, conn)
			// These invalid rows are outside the existing time/dry-run/store/action
			// window; no newly broadened source or heartbeat join may admit them.
			for _, scope := range []struct {
				store, action string
				dryRun        bool
				ageSeconds    int64
			}{
				{"stripe", "credit_unfulfillable", true, 60},
				{"stripe", "credit_unfulfillable", false, 25 * 60 * 60},
				{"apple", "credit_unfulfillable", false, 60},
				{"stripe", "error", false, 60},
			} {
				paymentReconciliationAuditSqlInsert(t, conn, "in_other_private_audit", nil, scope.store, scope.action, scope.dryRun, scope.ageSeconds)
			}
			row = paymentReconciliationAuditSqlRead(t, conn, paymentReconciliationUnfulfillableQuery)
			if strings.Join(row[:4], ",") != "1,2,2,0" {
				t.Fatal("the existing 24-hour non-dry-run Stripe terminal scope changed")
			}
			paymentReconciliationAuditSqlRequireWarn(t, conn)
			paymentReconciliationAuditSqlInsert(t, conn, "in_private_audit", map[string]any{"leg": "webhook"}, "stripe", "credit_unfulfillable", false, 30)
			row = paymentReconciliationAuditSqlRead(t, conn, paymentReconciliationUnfulfillableQuery)
			issue, err = paymentReconciliationUnfulfillableFinding([]pgRow{row})
			if err != nil || issue == nil || issue.class != "payment-reconciliation-credit-unfulfillable-invalid" || strings.Join(row[:4], ",") != "1,3,3,1" {
				t.Fatal("a valid webhook hid a same-invoice malformed record")
			}
		})
	})
}

// Unowned App Store and Play purchases reach the payment failure WARN as their
// own store groups through the actual SELECT. A purchase the app later bound
// and credited (its transaction in the App Store ledger, its token in the Play
// binding) drops out; a Play purchase bound to another network stays, since its
// binding is the conflict itself. Dry-run and stale rows stay outside.
func TestPaymentFailuresSqlCountsUnownedStorePurchases(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		server.Db(t.Context(), func(conn server.PgConn) {
			paymentReconciliationAuditSqlFixture(t, conn)
			unresolvedWebhook := map[string]any{"reason": "destination_unresolved", "leg": "webhook"}
			paymentReconciliationAuditSqlInsert(t, conn, "apple_unowned", unresolvedWebhook, "apple", "credit_unfulfillable", false, 120)
			paymentReconciliationAuditSqlInsert(t, conn, "apple_unowned", unresolvedWebhook, "apple", "credit_unfulfillable", false, 60)
			paymentReconciliationAuditSqlInsert(t, conn, "apple_credited_later", unresolvedWebhook, "apple", "credit_unfulfillable", false, 60)
			paymentReconciliationAuditSqlInsert(t, conn, "apple_dry_run", unresolvedWebhook, "apple", "credit_unfulfillable", true, 60)
			paymentReconciliationAuditSqlInsert(t, conn, "apple_stale", unresolvedWebhook, "apple", "credit_unfulfillable", false, 25*60*60)
			paymentReconciliationAuditSqlInsert(t, conn, "play_unowned", unresolvedWebhook, "google", "credit_unfulfillable", false, 60)
			paymentReconciliationAuditSqlInsert(t, conn, "play_bound_later", unresolvedWebhook, "google", "credit_unfulfillable", false, 60)
			paymentReconciliationAuditSqlInsert(t, conn, "play_bound_elsewhere", map[string]any{"reason": "bound_to_another_network", "leg": "credit"}, "google", "credit_unfulfillable", false, 60)
			server.RaisePgResult(conn.Exec(t.Context(), `
INSERT INTO apple_subscription_transaction (transaction_id) VALUES ('apple_credited_later');
INSERT INTO play_purchase_binding (purchase_token) VALUES ('play_bound_later'), ('play_bound_elsewhere');
`))

			counts := map[string]int64{}
			result, err := conn.Query(t.Context(), paymentFailuresQuery)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var kind, store string
					var count, oldestAge, newestAge int64
					server.Raise(result.Scan(&kind, &store, &count, &oldestAge, &newestAge))
					issue, err := paymentFailureFinding(kind, store, count, oldestAge, newestAge)
					if err != nil || issue.class != "payment-credit-unfulfillable" || issue.tier != tierWarn {
						t.Fatalf("%s/%s did not classify as the credit-unfulfillable WARN", kind, store)
					}
					rendered := alertFromFinding(syntheticSettings(&syntheticSource{}), "2.22", "payment-failures", "Payment failures", issue).Markdown()
					for _, forbidden := range []string{"apple_unowned", "play_unowned", "play_bound_elsewhere"} {
						if strings.Contains(rendered, forbidden) {
							t.Fatal("private payment evidence escaped the aggregate WARN")
						}
					}
					counts[kind+"/"+store] = count
				}
			})
			if counts["credit_unfulfillable/apple"] != 1 || counts["credit_unfulfillable/google"] != 2 || len(counts) != 2 {
				t.Fatalf("unowned store purchase groups = %v; want apple 1 (unowned) and google 2 (unowned, bound elsewhere)", counts)
			}
		})
	})
}
