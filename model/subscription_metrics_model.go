package model

// Privacy-safe aggregate inputs for the internal subscriptions dashboard.
//
// The dashboard deliberately starts from durable payment/entitlement ledgers
// instead of request counters. Store and window are closed label sets; no
// network, user, client, purchase, invoice, or transaction identifier leaves
// PostgreSQL.

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

const (
	SubscriptionMetricsStoreDeduplicated = "deduplicated"

	SubscriptionMetricsWindow24Hours = "24h"
	SubscriptionMetricsWindow7Days   = "7d"
	SubscriptionMetricsWindow30Days  = "30d"
	SubscriptionMetricsWindowAll     = "all"

	SubscriptionMetricsDataPackBalanceCode  = "balance_code"
	SubscriptionMetricsDataPackDirect       = "direct_balance"
	SubscriptionMetricsDataPackDeduplicated = "deduplicated"

	// a trial whose product names no plan
	SubscriptionMetricsPlanUnknown = "unknown"
)

var subscriptionMetricsPlans = []string{
	PlanYearly,
	PlanMonthly,
	SubscriptionMetricsPlanUnknown,
}

func SubscriptionMetricsPlans() []string {
	return append([]string(nil), subscriptionMetricsPlans...)
}

type SubscriptionMetricsStorePlan struct {
	Store string
	Plan  string
}

var subscriptionMetricsStores = []string{
	SubscriptionMarketApple,
	SubscriptionMarketGoogle,
	SubscriptionMarketStripe,
	SubscriptionMarketSolana,
	SubscriptionMarketMonero,
	SubscriptionMarketZcash,
}

var subscriptionMetricsWindows = []string{
	SubscriptionMetricsWindow24Hours,
	SubscriptionMetricsWindow7Days,
	SubscriptionMetricsWindow30Days,
}

var subscriptionMetricsReconciliationActions = []string{
	PaymentReconcileActionCredited,
	PaymentReconcileActionEnded,
	PaymentReconcileActionEntitlementRepaired,
	PaymentReconcileActionRefunded,
	PaymentReconcileActionDisputed,
	PaymentReconcileActionRevoked,
}

func SubscriptionMetricsStores() []string {
	return append([]string(nil), subscriptionMetricsStores...)
}

func SubscriptionMetricsWindows() []string {
	return append([]string(nil), subscriptionMetricsWindows...)
}

func SubscriptionMetricsReconciliationActions() []string {
	return append([]string(nil), subscriptionMetricsReconciliationActions...)
}

type SubscriptionMetricsStoreWindow struct {
	Store  string
	Window string
}

type SubscriptionMetricsStoreActionWindow struct {
	Store  string
	Action string
	Window string
}

type SubscriptionMetricsDataPackWindow struct {
	Source string
	Window string
}

type SubscriptionMetricsSnapshot struct {
	ActiveAccounts map[string]int64
	// networks inside a free trial now, by store and plan; they count in
	// ActiveAccounts too
	TrialsActive             map[SubscriptionMetricsStorePlan]int64
	NewPaidAccounts          map[SubscriptionMetricsStoreWindow]int64
	EngagedAccounts          map[SubscriptionMetricsStoreWindow]int64
	ChurnedAccounts          map[SubscriptionMetricsStoreWindow]int64
	ReconciliationEvents     map[SubscriptionMetricsStoreActionWindow]int64
	ReconciliationHeartbeat  *time.Time
	ReconciliationWatermarks map[string]time.Time
	DataPackFulfillments     map[SubscriptionMetricsDataPackWindow]int64
	DataPackBytes            map[SubscriptionMetricsDataPackWindow]int64
}

// subscriptionMetricsAccountQuery defines the account semantics in one
// snapshot:
//   - active: distinct networks with an in-window supporter renewal;
//   - new paid: the first supporter window per network/store, plus the first
//     such window across all tracked stores for the deduplicated total;
//   - engagement: an active paid network with an active top-level client whose
//     auth_time is inside the rolling window; and
//   - observed churn: the latest tracked supporter window ended inside the
//     rolling window and no later window remains in that store/any store.
//
// The per-store active/engagement/churn series count a network once per store.
// The deduplicated series count a network once across the tracked stores.
// `network_client_network_id_top_level` makes each active-subscriber
// engagement EXISTS probe index-backed; starting from the much smaller active
// paid cohort avoids scanning the fleet-wide 30-day auth band.
//
// An App Store sandbox purchase (TestFlight, App Review) is credited so the
// tester keeps Pro, with no revenue (controller
// appleCreditSubscriptionTransactionInTx): it is not a subscription and is
// left out. An App Store grace period's hold records no revenue either, but
// beside the paid transaction it holds, whose id it carries, so it stays.
const subscriptionMetricsAccountQuery = `
/* subscription-metrics-accounts */
WITH tracked_renewal AS MATERIALIZED (
    SELECT renewal.network_id, renewal.market, renewal.start_time, renewal.end_time
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.market IN ('apple', 'google', 'stripe', 'solana', 'monero', 'zcash')
      AND NOT (
          renewal.market = 'apple'
          AND renewal.net_revenue_nano_cents = 0
          AND NOT EXISTS (
              SELECT 1
              FROM subscription_renewal paid
              WHERE paid.network_id = renewal.network_id
                AND paid.subscription_type = 'supporter'
                AND paid.market = 'apple'
                AND paid.transaction_id = renewal.transaction_id
                AND 0 < paid.net_revenue_nano_cents
          )
      )
), metric_window(window_name, since_time) AS (
    VALUES ('24h', $2::timestamp), ('7d', $3::timestamp), ('30d', $4::timestamp)
), active_by_store AS (
    SELECT DISTINCT network_id, market
    FROM tracked_renewal
    WHERE start_time <= $1 AND $1 < end_time
), active_deduplicated AS (
    SELECT DISTINCT network_id
    FROM active_by_store
), first_paid_by_store AS (
    SELECT network_id, market, min(start_time) AS first_start_time
    FROM tracked_renewal
    WHERE start_time <= $1
    GROUP BY network_id, market
), first_paid_deduplicated AS (
    SELECT network_id, min(first_start_time) AS first_start_time
    FROM first_paid_by_store
    GROUP BY network_id
), latest_by_store AS (
    SELECT network_id, market, max(end_time) AS latest_end_time
    FROM tracked_renewal
    GROUP BY network_id, market
), latest_deduplicated AS (
    SELECT network_id, max(end_time) AS latest_end_time
    FROM tracked_renewal
    GROUP BY network_id
), metric AS (
    SELECT 'active'::text AS kind, market AS store, 'current'::text AS window_name,
           count(*)::bigint AS value
    FROM active_by_store
    GROUP BY market
    UNION ALL
    SELECT 'active', 'deduplicated', 'current', count(*)::bigint
    FROM active_deduplicated
    UNION ALL
    SELECT 'new_paid', first_paid_by_store.market, metric_window.window_name, count(*)::bigint
    FROM metric_window
    INNER JOIN first_paid_by_store
      ON metric_window.since_time <= first_paid_by_store.first_start_time
     AND first_paid_by_store.first_start_time <= $1
    GROUP BY first_paid_by_store.market, metric_window.window_name
    UNION ALL
    SELECT 'new_paid', 'deduplicated', metric_window.window_name, count(*)::bigint
    FROM metric_window
    INNER JOIN first_paid_deduplicated
      ON metric_window.since_time <= first_paid_deduplicated.first_start_time
     AND first_paid_deduplicated.first_start_time <= $1
    GROUP BY metric_window.window_name
    UNION ALL
    SELECT 'engaged', active_by_store.market, metric_window.window_name,
           count(*)::bigint
    FROM active_by_store
    CROSS JOIN metric_window
    WHERE EXISTS (
        SELECT 1
        FROM network_client
        WHERE network_client.network_id = active_by_store.network_id
          AND network_client.active = true
          AND network_client.source_client_id IS NULL
          AND metric_window.since_time <= network_client.auth_time
          AND network_client.auth_time <= $1
    )
    GROUP BY active_by_store.market, metric_window.window_name
    UNION ALL
    SELECT 'engaged', 'deduplicated', metric_window.window_name, count(*)::bigint
    FROM active_deduplicated
    CROSS JOIN metric_window
    WHERE EXISTS (
        SELECT 1
        FROM network_client
        WHERE network_client.network_id = active_deduplicated.network_id
          AND network_client.active = true
          AND network_client.source_client_id IS NULL
          AND metric_window.since_time <= network_client.auth_time
          AND network_client.auth_time <= $1
    )
    GROUP BY metric_window.window_name
    UNION ALL
    SELECT 'churned', latest_by_store.market, metric_window.window_name,
           count(*)::bigint
    FROM latest_by_store
    CROSS JOIN metric_window
    WHERE metric_window.since_time <= latest_by_store.latest_end_time
      AND latest_by_store.latest_end_time <= $1
    GROUP BY latest_by_store.market, metric_window.window_name
    UNION ALL
    SELECT 'churned', 'deduplicated', metric_window.window_name, count(*)::bigint
    FROM latest_deduplicated
    CROSS JOIN metric_window
    WHERE metric_window.since_time <= latest_deduplicated.latest_end_time
      AND latest_deduplicated.latest_end_time <= $1
    GROUP BY metric_window.window_name
)
SELECT kind, store, window_name, value
FROM metric
ORDER BY kind, store, window_name
`

// subscriptionMetricsTrialQuery counts the networks inside a free trial now:
// a credited window, shorter than any paid period (27 days), whose store
// period has not ended. Trials are subscriptions (owner decision 2026-11-08)
// and count in the account query too; this splits them out.
//
//   - Google Play: a window with its credit's revenue (a grace period's hold
//     has none) from the purchase's start to an expiry 2 to 27 days later:
//     the 14-day trial, or a day Play added before the trial's first charge.
//     Every Play window starts at the purchase's start, so a paid window is
//     never that short. The trial's first charge usually lands inside the day
//     Play added, whose window stays current for up to a day after it: once
//     the purchase has a charged window (an expiry at least 27 days after its
//     start), none of its windows is a trial. Play sells the trial only on the
//     yearly base plan.
//   - Stripe: the trial's $0 invoice, inside its period (its window runs the
//     30-day manual-payment grace past it). Stripe starts a trial only on the
//     yearly plan.
//   - App Store: a free transaction (the ledger's price is 0) that is not a
//     sandbox purchase (one records no revenue), inside its period; its
//     product names the plan.
//
// Apple and Play windows end SubscriptionGracePeriod (24 hours) past the
// store's expiry.
const subscriptionMetricsTrialQuery = `
/* subscription-metrics-trials */
WITH trial AS (
    SELECT renewal.network_id, 'google'::text AS store, 'yearly'::text AS plan
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.market = 'google'
      AND 0 < renewal.net_revenue_nano_cents
      AND renewal.start_time <= $1
      AND $1 < renewal.end_time - interval '24 hours'
      AND renewal.start_time + interval '2 days' <= renewal.end_time - interval '24 hours'
      AND renewal.end_time - interval '24 hours' < renewal.start_time + interval '27 days'
      AND NOT EXISTS (
          SELECT 1
          FROM subscription_renewal paid
          WHERE paid.network_id = renewal.network_id
            AND paid.subscription_type = 'supporter'
            AND paid.market = 'google'
            AND paid.purchase_token = renewal.purchase_token
            AND 0 < paid.net_revenue_nano_cents
            AND paid.start_time + interval '27 days' <= paid.end_time - interval '24 hours'
      )
    UNION ALL
    SELECT renewal.network_id, 'stripe'::text, 'yearly'::text
    FROM subscription_renewal renewal
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.market = 'stripe'
      AND renewal.net_revenue_nano_cents = 0
      AND renewal.start_time <= $1
      AND $1 < renewal.end_time - interval '30 days'
      AND renewal.end_time - interval '30 days' < renewal.start_time + interval '27 days'
    UNION ALL
    SELECT renewal.network_id, 'apple'::text,
           CASE
               WHEN ledger.product_id ILIKE '%year%' THEN 'yearly'
               WHEN ledger.product_id ILIKE '%month%' OR ledger.product_id = 'supporter' THEN 'monthly'
               ELSE 'unknown'
           END
    FROM subscription_renewal renewal
    INNER JOIN apple_subscription_transaction ledger
      ON ledger.transaction_id = renewal.transaction_id
    WHERE renewal.subscription_type = 'supporter'
      AND renewal.market = 'apple'
      AND 0 < renewal.net_revenue_nano_cents
      AND ledger.price_milliunits = 0
      AND renewal.start_time <= $1
      AND $1 < renewal.end_time - interval '24 hours'
      AND renewal.end_time - interval '24 hours' < renewal.start_time + interval '27 days'
), metric AS (
    SELECT store, plan, count(DISTINCT network_id)::bigint AS value
    FROM trial
    GROUP BY store, plan
    UNION ALL
    SELECT 'deduplicated', plan, count(DISTINCT network_id)::bigint
    FROM trial
    GROUP BY plan
)
SELECT store, plan, value
FROM metric
ORDER BY store, plan
`

const subscriptionMetricsReconciliationQuery = `
/* subscription-metrics-reconciliation */
WITH metric_window(window_name, since_time) AS (
    VALUES ('24h', $2::timestamp), ('7d', $3::timestamp), ('30d', $4::timestamp)
)
SELECT event.store, event.action, metric_window.window_name, count(*)::bigint
FROM payment_reconciliation_event event
CROSS JOIN metric_window
WHERE event.store IN ('apple', 'google', 'stripe', 'solana', 'monero', 'zcash')
  AND event.action IN ('credited', 'ended', 'entitlement_repaired', 'refunded', 'disputed', 'revoked')
  AND NOT event.dry_run
  AND metric_window.since_time <= event.event_time
  AND event.event_time <= $1
GROUP BY event.store, event.action, metric_window.window_name
ORDER BY event.store, event.action, metric_window.window_name
`

// A balance code is the fulfillment ledger for the Stripe/Coinbase/manual
// code-backed paths. A paid non-Pro transfer_balance is a direct data grant;
// exclude every balance named by a redeemed code so redemption cannot be
// counted once as a code and again as a direct grant. The remaining direct
// set includes Solana and any other architecture-consistent direct paid data
// path. Existing schema does not retain a reliable processor/item dimension,
// so only these bounded ledger-source labels are exported.
const subscriptionMetricsDataPackQuery = `
/* subscription-metrics-data-packs */
WITH fulfillment AS MATERIALIZED (
    SELECT 'balance_code'::text AS source,
           create_time AS fulfilled_at,
           balance_byte_count::bigint AS byte_count
    FROM transfer_balance_code
    WHERE 0 < net_revenue_nano_cents
      AND 0 < balance_byte_count
      AND purchase_event_id <> ''
    UNION ALL
    SELECT 'direct_balance',
           transfer_balance.start_time,
           transfer_balance.start_balance_byte_count::bigint
    FROM transfer_balance
    WHERE transfer_balance.paid
      AND NOT transfer_balance.pro
      AND 0 < transfer_balance.start_balance_byte_count
      AND NOT EXISTS (
          SELECT 1
          FROM transfer_balance_code
          WHERE transfer_balance_code.redeem_balance_id = transfer_balance.balance_id
      )
), metric_window(window_name, since_time) AS (
    VALUES
        ('24h', $2::timestamp),
        ('7d', $3::timestamp),
        ('30d', $4::timestamp),
        ('all', NULL::timestamp)
)
SELECT
    CASE WHEN GROUPING(fulfillment.source) = 1
         THEN 'deduplicated'
         ELSE fulfillment.source
    END AS source,
    metric_window.window_name,
    count(*)::bigint AS fulfillment_count,
    COALESCE(sum(fulfillment.byte_count), 0)::bigint AS byte_count
FROM fulfillment
CROSS JOIN metric_window
WHERE (metric_window.since_time IS NULL OR metric_window.since_time <= fulfillment.fulfilled_at)
  AND fulfillment.fulfilled_at <= $1
GROUP BY metric_window.window_name,
         GROUPING SETS ((fulfillment.source), ())
ORDER BY source, metric_window.window_name
`

func newSubscriptionMetricsSnapshot() *SubscriptionMetricsSnapshot {
	return &SubscriptionMetricsSnapshot{
		ActiveAccounts:           map[string]int64{},
		TrialsActive:             map[SubscriptionMetricsStorePlan]int64{},
		NewPaidAccounts:          map[SubscriptionMetricsStoreWindow]int64{},
		EngagedAccounts:          map[SubscriptionMetricsStoreWindow]int64{},
		ChurnedAccounts:          map[SubscriptionMetricsStoreWindow]int64{},
		ReconciliationEvents:     map[SubscriptionMetricsStoreActionWindow]int64{},
		ReconciliationWatermarks: map[string]time.Time{},
		DataPackFulfillments:     map[SubscriptionMetricsDataPackWindow]int64{},
		DataPackBytes:            map[SubscriptionMetricsDataPackWindow]int64{},
	}
}

// LoadSubscriptionMetricsSnapshot computes one aggregate dashboard snapshot.
// Any database error aborts the caller before it can advance the Prometheus
// snapshot timestamp, so old exporter state becomes no-data rather than a
// current-looking zero.
func LoadSubscriptionMetricsSnapshot(ctx context.Context, now time.Time) *SubscriptionMetricsSnapshot {
	now = now.UTC()
	cutoff24Hours := now.Add(-24 * time.Hour)
	cutoff7Days := now.Add(-7 * 24 * time.Hour)
	cutoff30Days := now.Add(-30 * 24 * time.Hour)
	snapshot := newSubscriptionMetricsSnapshot()

	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			subscriptionMetricsAccountQuery,
			now,
			cutoff24Hours,
			cutoff7Days,
			cutoff30Days,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var kind string
				var store string
				var window string
				var value int64
				server.Raise(result.Scan(&kind, &store, &window, &value))
				switch kind {
				case "active":
					snapshot.ActiveAccounts[store] = value
				case "new_paid":
					snapshot.NewPaidAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: window}] = value
				case "engaged":
					snapshot.EngagedAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: window}] = value
				case "churned":
					snapshot.ChurnedAccounts[SubscriptionMetricsStoreWindow{Store: store, Window: window}] = value
				}
			}
		})

		result, err = conn.Query(
			ctx,
			subscriptionMetricsTrialQuery,
			now,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var store string
				var plan string
				var value int64
				server.Raise(result.Scan(&store, &plan, &value))
				snapshot.TrialsActive[SubscriptionMetricsStorePlan{Store: store, Plan: plan}] = value
			}
		})

		result, err = conn.Query(
			ctx,
			subscriptionMetricsReconciliationQuery,
			now,
			cutoff24Hours,
			cutoff7Days,
			cutoff30Days,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var store string
				var action string
				var window string
				var value int64
				server.Raise(result.Scan(&store, &action, &window, &value))
				snapshot.ReconciliationEvents[SubscriptionMetricsStoreActionWindow{
					Store: store, Action: action, Window: window,
				}] = value
			}
		})

		result, err = conn.Query(
			ctx,
			`SELECT max(event_time) FROM payment_reconciliation_event WHERE store = 'all' AND action = 'heartbeat' AND NOT dry_run`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&snapshot.ReconciliationHeartbeat))
			}
		})

		result, err = conn.Query(
			ctx,
			`SELECT store, update_time FROM payment_reconciliation_watermark WHERE store IN ('apple', 'google', 'stripe', 'solana', 'monero', 'zcash') ORDER BY store`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var store string
				var updateTime time.Time
				server.Raise(result.Scan(&store, &updateTime))
				snapshot.ReconciliationWatermarks[store] = updateTime.UTC()
			}
		})

		result, err = conn.Query(
			ctx,
			subscriptionMetricsDataPackQuery,
			now,
			cutoff24Hours,
			cutoff7Days,
			cutoff30Days,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var source string
				var window string
				var fulfillmentCount int64
				var byteCount int64
				server.Raise(result.Scan(&source, &window, &fulfillmentCount, &byteCount))
				key := SubscriptionMetricsDataPackWindow{Source: source, Window: window}
				snapshot.DataPackFulfillments[key] = fulfillmentCount
				snapshot.DataPackBytes[key] = byteCount
			}
		})
	})

	return snapshot
}
