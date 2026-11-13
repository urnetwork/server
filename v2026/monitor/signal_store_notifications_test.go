package monitor

// SIGNALS.md §2.22c: store notifications that stopped while purchases
// continue, and the log tail's refused and unverified store notifications.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Answers the preflight with the migration head and table presence, and the
// observation with rows; counts the observations.
func storeNotificationsTestSource(t testing.TB, head int, present bool, rows []Row) (*syntheticSource, *int) {
	observationCount := 0
	presence := "f"
	if present {
		presence = "t"
	}
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		switch {
		case strings.Contains(query, "monitor-signal-2.22c-store-notifications-schema"):
			return []Row{{fmt.Sprintf("%d", head), presence}}, nil
		case strings.Contains(query, "monitor-signal-2.22c-store-notifications"):
			observationCount++
			return rows, nil
		}
		t.Fatalf("unexpected query: %s", query)
		return nil, nil
	}}, &observationCount
}

// Below the migration nothing is tracked and the observation is never
// issued; at or past it the table must exist.
func TestStoreNotificationsMigrationGate(t *testing.T) {
	version := server.StoreNotificationFlowVersion()
	if version <= 0 {
		t.Fatal("no store notification flow migration")
	}
	source, observationCount := storeNotificationsTestSource(t, version-1, false, nil)
	alerts, err := NewStoreNotificationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 || *observationCount != 0 {
		t.Fatalf("below the migration: alerts=%+v err=%v observations=%d", alerts, err, *observationCount)
	}
	source, _ = storeNotificationsTestSource(t, version, false, nil)
	if _, err := NewStoreNotificationsSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
		t.Fatal("a missing table at the migration head was not an observation failure")
	}
}

// A store pages when it accepted nothing for the window while its purchases
// continue, and only then: no purchases, or a recent accepted notification,
// is healthy.
func TestStoreNotificationsSilentWhilePurchasesContinue(t *testing.T) {
	window := int64(DefaultStoreNotificationsSettings().SilentWindow / time.Second)
	source, _ := storeNotificationsTestSource(t, server.StoreNotificationFlowVersion(), true, []Row{
		{"apple", "4", fmt.Sprintf("%d", window-60)},
		{"google", "3", fmt.Sprintf("%d", 8*24*3600)},
	})
	alerts, err := NewStoreNotificationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "store-notification-silent")
	if alert.Severity != SeverityPage || alert.Target != "google" {
		t.Fatalf("silent alert = %s %s, want page google", alert.Severity, alert.Target)
	}
	if count := requireAlertClassCount(alerts, "store-notification-silent"); count != 1 {
		t.Fatalf("silent alerts = %d, want google only", count)
	}
	markdown := alert.Markdown()
	for _, want := range []string{"Google Play real-time developer notification", "purchases_in_window=3", "SIGNALS.md §2.22c"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("silent alert lacks %q:\n%s", want, markdown)
		}
	}

	// no purchases: nothing to notify about, whatever the age; no row and
	// purchases: never accepted since tracking began
	source, _ = storeNotificationsTestSource(t, server.StoreNotificationFlowVersion(), true, []Row{
		{"apple", "2", "-1"},
		{"google", "0", fmt.Sprintf("%d", 30*24*3600)},
	})
	alerts, err = NewStoreNotificationsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert = requireAlertClass(t, alerts, "store-notification-silent")
	if alert.Target != "apple" || !strings.Contains(alert.Markdown(), "last_accepted_age_seconds=none") {
		t.Fatalf("an untracked store with purchases was not reported: %+v", alert)
	}
	if count := requireAlertClassCount(alerts, "store-notification-silent"); count != 1 {
		t.Fatalf("silent alerts = %d, want apple only", count)
	}
}

func TestStoreNotificationsRejectsMalformedAggregates(t *testing.T) {
	version := server.StoreNotificationFlowVersion()
	for name, rows := range map[string][]Row{
		"unknown store":   {{"apple", "0", "1"}, {"stripe", "1", "1"}},
		"missing store":   {{"apple", "0", "1"}},
		"negative count":  {{"apple", "-1", "1"}, {"google", "0", "1"}},
		"invalid age":     {{"apple", "1", "-2"}, {"google", "0", "1"}},
		"wrong columns":   {{"apple", "1"}, {"google", "0"}},
		"non-numeric age": {{"apple", "1", "soon"}, {"google", "0", "1"}},
	} {
		source, _ := storeNotificationsTestSource(t, version, true, rows)
		alerts, err := NewStoreNotificationsSignal().Run(context.Background(), syntheticSettings(source))
		if requireAlertClassCount(alerts, "store-notification-silent") != 0 {
			t.Errorf("%s: a malformed aggregate alerted as silent", name)
		}
		if err == nil {
			t.Errorf("%s: a malformed aggregate was not an observation failure", name)
		}
	}
}

// The observation reads the flow and the purchases inside the window past
// the grace, at the probe's settings, and exports only stores, counts and
// ages.
func TestStoreNotificationsQueryContract(t *testing.T) {
	settings := &StoreNotificationsSettings{SilentWindow: 6 * time.Hour, PurchaseGrace: 10 * time.Minute}
	query := storeNotificationsQueryWith(settings)
	for _, required := range []string{
		"VALUES ('apple'), ('google')",
		"FROM subscription_renewal renewal",
		"renewal.market IN ('google', 'apple')",
		"renewal.subscription_type = 'supporter'",
		"renewal.end_time > now() - interval '21600 seconds'",
		"renewal.start_time > now() - interval '21600 seconds'",
		"renewal.start_time <= now() - interval '600 seconds'",
		"LEFT JOIN store_notification_flow flow ON flow.store = store.target",
	} {
		if !strings.Contains(query, required) {
			t.Errorf("store notifications query lacks %q", required)
		}
	}
	selectTail := query[strings.LastIndex(query, "SELECT "):]
	for _, identifier := range []string{"network_id", "purchase_token", "transaction_id"} {
		if strings.Contains(selectTail, identifier) {
			t.Errorf("store notifications exports %s", identifier)
		}
	}
}

// The real query on the migrated schema: Play purchases credited for a day
// with no accepted Play notification page; the App Store, whose
// notifications are accepted, does not.
func TestStoreNotificationsSilentOnTheMigratedSchema(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		exec := func(sql string, args ...any) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql, args...))
			})
		}
		// the migration's rows; Play's last accepted two days ago
		exec(`UPDATE store_notification_flow SET last_accepted_at = $1 WHERE store = 'google'`, now.Add(-48*time.Hour))
		exec(`UPDATE store_notification_flow SET last_accepted_at = $1 WHERE store = 'apple'`, now.Add(-10*time.Minute))
		for _, market := range []string{"google", "apple"} {
			exec(
				`
					INSERT INTO subscription_renewal
					(network_id, subscription_type, start_time, end_time, net_revenue_nano_cents, purchase_token, market, transaction_id)
					VALUES ($1, 'supporter', $2, $3, 1, 'synthetic-token', $4, $5)
				`,
				server.NewId(),
				now.Add(-3*time.Hour),
				now.Add(27*24*time.Hour),
				market,
				"synthetic-"+market,
			)
		}

		source := paymentNotificationTestDbSource(t, ctx)
		alerts, err := NewStoreNotificationsSignal().Run(ctx, syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "store-notification-silent")
		if alert.Target != "google" || requireAlertClassCount(alerts, "store-notification-silent") != 1 {
			t.Fatalf("silent alerts = %+v, want google only", alerts)
		}
		requireAlertOmits(t, alert, "synthetic-token", "synthetic-google")

		// a Play notification accepted now: healthy again
		exec(`UPDATE store_notification_flow SET last_accepted_at = $1 WHERE store = 'google'`, now)
		alerts, err = NewStoreNotificationsSignal().Run(ctx, syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if requireAlertClassCount(alerts, "store-notification-silent") != 0 {
			t.Fatalf("an accepted notification left the silent alert: %+v", alerts)
		}
	})
}

// Answers every PostgreSQL query from the test database, each value as text
// (a bool as t or f).
func paymentNotificationTestDbSource(t testing.TB, ctx context.Context) *syntheticSource {
	return &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		rows := []Row{}
		var queryErr error
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, query)
			if err != nil {
				queryErr = err
				return
			}
			defer result.Close()
			for result.Next() {
				values, err := result.Values()
				if err != nil {
					queryErr = err
					return
				}
				row := Row{}
				for _, value := range values {
					switch v := value.(type) {
					case bool:
						if v {
							row = append(row, "t")
						} else {
							row = append(row, "f")
						}
					default:
						row = append(row, fmt.Sprint(value))
					}
				}
				rows = append(rows, row)
			}
			queryErr = result.Err()
		})
		return rows, queryErr
	}}
}

// The log tail pages on a verified notification refused or failed, warns on
// a sustained rate of unverified ones, and frames each by store and outcome
// without the request's details. A missing apple.yml pages as a fail-closed
// webhook refusal like the other webhooks.
func TestLogErrorsSignalClassifiesStoreNotificationLines(t *testing.T) {
	run := func(lines []string) []Alert {
		source := &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
			if len(args) > 1 && args[0] == "ls" {
				return "repo names synthetic-api", nil
			}
			return strings.Join(lines, "\n") + "\n", nil
		}}
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		return alerts
	}
	prefix := "W1112 19:29:42.879518       1 store_notification_flow.go:160] "

	for _, test := range []struct {
		line  string
		frame string
	}{
		{line: prefix + "[store-notification]google refused_audience", frame: "store=google outcome=refused_audience"},
		{line: prefix + "[store-notification]apple refused_claims", frame: "store=apple outcome=refused_claims"},
		{line: prefix + "[store-notification]google error", frame: "store=google outcome=error"},
	} {
		alert := requireAlertClass(t, run([]string{test.line}), "store-notification-refused")
		if alert.Severity != SeverityPage {
			t.Fatalf("%s: severity = %s, want page", test.line, alert.Severity)
		}
		if !strings.Contains(alert.Observed, test.frame) {
			t.Fatalf("%s: not framed by %q: %s", test.line, test.frame, alert.Observed)
		}
	}

	// one unverified push is anyone's; a sustained rate warns
	unverified := prefix + "[store-notification]google refused_auth"
	if alerts := run([]string{unverified}); requireAlertClassCount(alerts, "store-notification-unverified") != 0 {
		t.Fatal("one unverified push alerted")
	}
	lines := []string{}
	for range 40 {
		lines = append(lines, unverified)
	}
	alert := requireAlertClass(t, run(lines), "store-notification-unverified")
	if alert.Severity != SeverityWarn {
		t.Fatalf("unverified severity = %s, want warn", alert.Severity)
	}

	// the accepted mark's failure is no refusal
	for _, alert := range run([]string{prefix + "[store-notification]apple accepted, but the time it was accepted was not kept: synthetic"}) {
		if strings.HasPrefix(alert.Class, "store-notification-") {
			t.Fatalf("an accepted notification alerted as %s", alert.Class)
		}
	}

	refused := requireAlertClass(t, run([]string{
		"W1112 19:29:42.879518       1 auth_handlers.go:120] [apple]webhook refused, verification config unavailable: apple.yml app_store_notifications is required",
	}), "payment-webhook-refused")
	if refused.Severity != SeverityPage || !strings.Contains(refused.Observed, "frame=apple") {
		t.Fatalf("apple fail-closed refusal = %s %s", refused.Severity, refused.Observed)
	}
	requireAlertOmits(t, refused, "app_store_notifications is required")
}
