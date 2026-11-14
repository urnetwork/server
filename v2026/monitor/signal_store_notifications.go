// Store notifications that stopped arriving (SIGNALS.md §2.22c). From
// 2026-10-19 to 10-27 Main refused every Google Play real-time developer
// notification and nothing noticed for eight days, while purchases went on
// being credited by the app's purchase report and the hourly reconcile; a
// purchase cancelled before its first notification was processed was never
// acknowledged, and Google refunded it. The api records when it last
// accepted a notification of each store (store_notification_flow); this
// signal reads that against the store's purchases. A store that accepted
// nothing for the silent window while purchases of it were credited pages,
// whatever refused the notifications or kept them from arriving. Refusals
// themselves page from the log tail (store-notification-refused,
// store-notification-unverified, payment-webhook-refused).
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The store notifications probe's tunables.
type StoreNotificationsSettings struct {
	// How long a store may go without an accepted notification while its
	// purchases continue before it pages.
	SilentWindow time.Duration
	// A purchase younger than this may still have its notification in
	// flight, and does not count.
	PurchaseGrace time.Duration
}

// A new instance on every call.
func DefaultStoreNotificationsSettings() *StoreNotificationsSettings {
	return &StoreNotificationsSettings{
		// a day: the 2026-10-19 outage would have paged on its first day
		// instead of going unnoticed for eight, and a quiet store with few
		// purchases is not a false page
		SilentWindow: 24 * time.Hour,
		// past the notification's delivery and the store's first retries
		PurchaseGrace: time.Hour,
	}
}

// What the schema preflight reads: the installed migration head, and whether
// store_notification_flow exists. PostgreSQL resolves every relation a
// statement names before it runs, so the observation is issued only once it
// does.
const storeNotificationsSchemaQuery = `
/* monitor-signal-2.22c-store-notifications-schema */
SELECT coalesce(max(end_version_number), 0)::bigint,
       (to_regclass('public.store_notification_flow') IS NOT NULL
        AND to_regclass('public.subscription_renewal') IS NOT NULL)
FROM migration_audit
WHERE status = 'success'
`

// Per store: the purchases credited inside the silent window (any path: the
// notification, the app's purchase report, the reconcile) that are past the
// grace, and the age of the last accepted notification (-1 with no row). A
// purchase that started inside the window ends inside it or later, so the
// end time bounds the scan to the supporter window index. Only stores,
// counts and ages leave PostgreSQL.
func storeNotificationsQueryWith(settings *StoreNotificationsSettings) string {
	storeValues := []string{}
	for _, store := range slices.Sorted(slices.Values(model.StoreNotificationStores)) {
		storeValues = append(storeValues, "("+paymentFailuresSqlList([]string{store})+")")
	}
	windowSql := fmt.Sprintf("%d seconds", int64(settings.SilentWindow/time.Second))
	graceSql := fmt.Sprintf("%d seconds", int64(settings.PurchaseGrace/time.Second))
	return `
/* monitor-signal-2.22c-store-notifications */
WITH store(target) AS (
    VALUES ` + strings.Join(storeValues, ", ") + `
), purchase AS (
    SELECT renewal.market AS target,
           count(*)::bigint AS purchase_count
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.end_time > now() - interval '` + windowSql + `'
      AND renewal.market IN (` + paymentFailuresSqlList(model.StoreNotificationStores) + `)
      AND renewal.start_time > now() - interval '` + windowSql + `'
      AND renewal.start_time <= now() - interval '` + graceSql + `'
    GROUP BY renewal.market
)
SELECT store.target,
       COALESCE(purchase.purchase_count, 0)::bigint,
       COALESCE(greatest(0, extract(epoch FROM now() - flow.last_accepted_at))::bigint, -1)
FROM store
LEFT JOIN purchase ON purchase.target = store.target
LEFT JOIN store_notification_flow flow ON flow.store = store.target
ORDER BY store.target
`
}

// SIGNALS.md §2.22c maps to signal_store_notifications.go and
// signal_store_notifications_test.go.
func NewStoreNotificationsSignal() Signal {
	return &signalAdapter{
		number: "2.22c",
		key:    "store-notifications",
		name:   "Store notifications stopped while purchases continue",
		probe:  storeNotificationsProbe{settings: DefaultStoreNotificationsSettings()},
	}
}

// The store notifications probe, at its settings.
type storeNotificationsProbe struct {
	settings *StoreNotificationsSettings
}

// The probe's alert identity.
func (self storeNotificationsProbe) id() string { return "pg/store-notifications" }

// Its tier: a store whose notifications stopped pages.
func (self storeNotificationsProbe) tier() string { return tierPage }

// Its cadence, as the other payment probes'.
func (self storeNotificationsProbe) cadence() time.Duration { return 5 * time.Minute }

// Reads the preflight, then the observation, and reports each store that
// accepted nothing for the window while its purchases continued.
func (self storeNotificationsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := self.settings
	if settings == nil {
		settings = DefaultStoreNotificationsSettings()
	}
	version := server.StoreNotificationFlowVersion()
	if version <= 0 {
		return nil, fmt.Errorf("store notifications: this build carries no store notification flow migration")
	}
	rows, err := env.runner.pg(ctx, storeNotificationsSchemaQuery)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return nil, fmt.Errorf("store notifications schema observation is incomplete")
	}
	head, err := parseStrictInt64(rows[0].str(0))
	if err != nil || head < 0 || (rows[0].str(1) != "t" && rows[0].str(1) != "f") {
		return nil, fmt.Errorf("store notifications schema observation is invalid")
	}
	if head < int64(version) {
		// before the migration nothing is tracked
		return nil, nil
	}
	if rows[0].str(1) != "t" {
		return nil, fmt.Errorf("store_notification_flow unavailable at the installed migration head")
	}

	rows, err = env.runner.pg(ctx, storeNotificationsQueryWith(settings))
	if err != nil {
		return nil, err
	}
	if len(rows) != len(model.StoreNotificationStores) {
		return nil, fmt.Errorf("store notifications returned %d stores, want %d", len(rows), len(model.StoreNotificationStores))
	}
	windowSeconds := int64(settings.SilentWindow / time.Second)
	silentFinding := func(store string, purchaseCount int64, ageSeconds int64) finding {
		storeName := map[string]string{
			model.SubscriptionMarketGoogle: "Google Play real-time developer notification",
			model.SubscriptionMarketApple:  "App Store Server Notification",
		}[store]
		lastAccepted := "never"
		observedAge := "none"
		if 0 <= ageSeconds {
			lastAccepted = fmt.Sprintf("%d hours ago", ageSeconds/3600)
			observedAge = fmt.Sprintf("%d", ageSeconds)
		}
		return finding{
			probeId: "pg/store-notifications", tier: tierPage, class: "store-notification-silent", target: store, sustain: 1,
			symptom:   fmt.Sprintf("no %s was accepted for %s (last accepted: %s) while %d %s purchase(s) were credited by other paths", storeName, settings.SilentWindow, lastAccepted, purchaseCount, store),
			baseline:  fmt.Sprintf("Every purchase sends its store's notification within minutes, so a store with purchases inside %s has an accepted notification inside it too.", settings.SilentWindow),
			observed:  fmt.Sprintf("store=%s purchases_in_window=%d last_accepted_age_seconds=%s window_seconds=%d", store, purchaseCount, observedAge, windowSeconds),
			mechanism: "The api accepted no notification of the store inside the window (store_notification_flow.last_accepted_at), yet subscription_renewal holds purchases of it inside the window, past the grace: the app's purchase report or the hourly reconcile credited them, not the notification. The notifications are being refused (a vault field the verifier needs, an audience or publisher that no longer matches the subscription, Apple roots or claims that no longer match), failing, or no longer sent (a Pub/Sub push subscription or App Store Server Notification URL changed or removed). A Play purchase cancelled before its first notification is processed is never acknowledged, and Google refunds it after three days.",
			context:   "The refusals themselves page or warn from the log tail as store-notification-refused, store-notification-unverified and payment-webhook-refused, and are counted in urnetwork_store_notifications_total{store,outcome} on the api dashboard; urnetwork_store_notification_last_accepted_timestamp_seconds{store} is the fleet's last accepted time. A store tracked from the migration with an api that does not write the flow (an older build) reads as silent too.",
			evidence:  "Only the store, a purchase count and an age are selected. Tokens, purchase tokens, transaction ids, networks and notification bodies never leave PostgreSQL.",
			action:    "Read the api's store notification outcomes (dashboard urnetwork / api, store notifications) and the [store-notification] and [play]/[apple] webhook lines. Restore the missing or mismatched verifier input through the vault owner (google.yml webhook.publisher_email and webhook.audience, apple.yml app_store_notifications), or the push subscription or notification URL in Play Console or App Store Connect. Then have the stores resend: Pub/Sub redelivers unacknowledged pushes, App Store Connect can request notification history. Check Play Console for purchases of the window cancelled before their first notification, and reconcile them; never grant from this aggregate.",
			verify:    "Accepted notifications resume for the store (the flow's last accepted age falls under the window and the accepted outcome count rises), refusals stop, and purchases of the window are acknowledged and credited once.",
			playbook:  "SIGNALS.md §2.22c",
		}
	}
	findings := []finding{}
	for _, row := range rows {
		if len(row) != 3 {
			return nil, fmt.Errorf("store notifications returned %d columns, want 3", len(row))
		}
		store := strings.TrimSpace(row.str(0))
		if !slices.Contains(model.StoreNotificationStores, store) {
			return nil, fmt.Errorf("store notifications returned an unknown store")
		}
		purchaseCount, err := parseStrictInt64(row.str(1))
		if err != nil || purchaseCount < 0 {
			return nil, fmt.Errorf("store notifications returned an invalid purchase count")
		}
		ageSeconds, err := parseStrictInt64(row.str(2))
		if err != nil || ageSeconds < -1 {
			return nil, fmt.Errorf("store notifications returned an invalid age")
		}
		if purchaseCount == 0 || (0 <= ageSeconds && ageSeconds <= windowSeconds) {
			continue
		}
		findings = append(findings, silentFinding(store, purchaseCount, ageSeconds))
	}
	return findings, nil
}
