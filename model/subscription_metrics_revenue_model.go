package model

// Expected dollars in for the internal subscriptions dashboard (owner request
// 2026-11-16): net new forecasted revenue and net recurring revenue, with the
// trial conversion rate the forecast values trials at.
//
// Both numbers are net of store fees and read the same tracked supporter
// windows as the account query (subscriptionMetricsStores, App Store sandbox
// purchases left out). Only stores, windows, outcomes and USD sums leave
// PostgreSQL.
//
// The ledger's net revenue is the amount the store charged, less its fee, on
// Stripe (the invoice total), the App Store (a USD price), the crypto markets
// (the payment's USD value) and gamma (the intent's price). Two credits record
// a placeholder instead, and are valued at the plan's price tier (pro.yml
// price_tiers) less the store's fee:
//   - every Google Play credit records the play.yml supporter SKU's price
//     ($5 less 30%), whatever the base plan, tier or trial;
//   - an App Store sale in a currency other than USD records a $0.01 marker.
//
// A Stripe trial that only the payment reconcile credited before the card
// check shipped (SubscriptionMetricsStripeTrialCardGate) is left out of every
// revenue number and of the conversion rate: until then the reconcile's
// store-side leg credited a payment sheet's $0 trial invoice without the hold
// that waits for the card (stripeHandleInvoicePaidWithOnboarding), so those
// are abandoned payment sheets with no card, which can never convert.
//
// The revenue is its own step of the snapshot (loadSubscriptionMetricsRevenue):
// its queries run on their own connection under their own time bound, and a
// failure leaves the snapshot's other numbers whole.

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

const (
	// the label of a total over every store's trials or payments, where a
	// network is not deduplicated: the pooled conversion rate and recurring
	// revenue, which adds every store's payments
	SubscriptionMetricsStoreAll = "all"

	// the outcomes of a trial whose free period ended
	SubscriptionMetricsTrialOutcomeConverted = "converted"
	SubscriptionMetricsTrialOutcomeLapsed    = "lapsed"
)

// SubscriptionMetricsStripeTrialCardGate is when the payment-sheet card check
// (feature/trial-card-check) deployed, RFC 3339 in UTC. From then on the
// reconcile's store-side leg credits a payment sheet's $0 trial invoice only
// once its card is attached, as invoice.paid does, so a reconcile credit is a
// real trial whose webhook was missed and counts like any other. A reconcile
// credit of a $0 Stripe invoice made before it is an abandoned payment sheet
// and is left out.
//
// PLACEHOLDER: "" while the card check has not deployed, which leaves every
// reconcile credit of a $0 Stripe invoice out. Set it to the deploy time (e.g.
// "2026-11-18T19:00:00Z") in the release that ships the card check.
const SubscriptionMetricsStripeTrialCardGate = ""

// subscriptionMetricsParseStripeTrialCardGate is the gate as a time, nil while
// it is "" (not deployed). A value that is not RFC 3339 panics, so a malformed
// gate stops the release's tests rather than move the cutoff.
func subscriptionMetricsParseStripeTrialCardGate(gate string) *time.Time {
	if gate == "" {
		return nil
	}
	gateTime, err := time.Parse(time.RFC3339, gate)
	if err != nil {
		panic(fmt.Sprintf("subscription metrics: Stripe trial card gate %q is not RFC 3339: %s", gate, err))
	}
	gateTime = gateTime.UTC()
	return &gateTime
}

// The stores that sell a free trial. The crypto markets and gamma sell none.
var subscriptionMetricsTrialStores = []string{
	SubscriptionMarketApple,
	SubscriptionMarketGoogle,
	SubscriptionMarketStripe,
}

var subscriptionMetricsTrialOutcomes = []string{
	SubscriptionMetricsTrialOutcomeConverted,
	SubscriptionMetricsTrialOutcomeLapsed,
}

func SubscriptionMetricsTrialStores() []string {
	return append([]string(nil), subscriptionMetricsTrialStores...)
}

func SubscriptionMetricsTrialOutcomes() []string {
	return append([]string(nil), subscriptionMetricsTrialOutcomes...)
}

type SubscriptionMetricsStoreOutcome struct {
	Store   string
	Outcome string
}

// A Pro price tier the forecast values a plan at.
type SubscriptionMetricsPriceTier struct {
	Name       string
	YearlyUsd  float64
	MonthlyUsd float64
}

// The fee each store keeps of a price, as its credit path takes it off when it
// records net revenue: Stripe's invoice credit (stripeInvoiceFeeFraction), the
// App Store's validation (appleTransactionFeeFraction) and Google Play's
// supporter SKU (play.yml fee_fraction). The crypto markets and gamma record
// what was paid. The taskworker passes the credit paths' own (controller
// subscriptionMetricsStoreFeeFractions); these are the defaults for every other
// caller. A new map on every call.
func DefaultSubscriptionMetricsStoreFeeFractions() map[string]float64 {
	return map[string]float64{
		SubscriptionMarketStripe: 0.3,
		SubscriptionMarketApple:  0.2,
		SubscriptionMarketGoogle: 0.3,
	}
}

// The tiers of pro.yml, the first the default.
func subscriptionMetricsProPriceTiers() []SubscriptionMetricsPriceTier {
	tiers := []SubscriptionMetricsPriceTier{}
	for _, tier := range Pro().PriceTiers() {
		tiers = append(tiers, SubscriptionMetricsPriceTier{
			Name:       tier.Name,
			YearlyUsd:  tier.YearlyUsd,
			MonthlyUsd: tier.MonthlyUsd,
		})
	}
	return tiers
}

// subscriptionMetricsTrialConversionRates is the rate each tracked store's
// trials are valued at, and the pooled rate (SubscriptionMetricsStoreAll): a
// store's own when at least minTrials of its trials ended in the period, else
// the pooled rate of every store's trials. With no ended trial at all, every
// rate is zero: there is no evidence of a conversion.
func subscriptionMetricsTrialConversionRates(sample map[SubscriptionMetricsStoreOutcome]int64, minTrials int64) map[string]float64 {
	minTrials = max(1, minTrials)
	rate := func(converted int64, ended int64) float64 {
		if ended <= 0 {
			return 0
		}
		return float64(converted) / float64(ended)
	}
	stores := SubscriptionMetricsStores()
	pooledConverted := int64(0)
	pooledEnded := int64(0)
	for _, store := range stores {
		converted := sample[SubscriptionMetricsStoreOutcome{Store: store, Outcome: SubscriptionMetricsTrialOutcomeConverted}]
		lapsed := sample[SubscriptionMetricsStoreOutcome{Store: store, Outcome: SubscriptionMetricsTrialOutcomeLapsed}]
		pooledConverted += converted
		pooledEnded += converted + lapsed
	}
	pooled := rate(pooledConverted, pooledEnded)
	rates := map[string]float64{SubscriptionMetricsStoreAll: pooled}
	for _, store := range stores {
		converted := sample[SubscriptionMetricsStoreOutcome{Store: store, Outcome: SubscriptionMetricsTrialOutcomeConverted}]
		ended := converted + sample[SubscriptionMetricsStoreOutcome{Store: store, Outcome: SubscriptionMetricsTrialOutcomeLapsed}]
		if minTrials <= ended {
			rates[store] = rate(converted, ended)
		} else {
			rates[store] = pooled
		}
	}
	return rates
}

// subscriptionMetricsPeriodPlanSql is the plan a paid period of the given
// length was sold on, for a window whose ledger names no plan: a year (a year
// and a trial for the yearly welcome offer), a month, or unknown. Only the
// period the store billed is read, never a window's length with its grace or
// an early end.
func subscriptionMetricsPeriodPlanSql(period string) string {
	return fmt.Sprintf(
		`CASE
               WHEN interval '300 days' <= (%[1]s) THEN 'yearly'
               WHEN interval '20 days' <= (%[1]s) AND (%[1]s) <= interval '45 days' THEN 'monthly'
               ELSE 'unknown'
           END`,
		period,
	)
}

// subscriptionMetricsRevenueRowsSql is the start of the WITH list of both
// revenue queries ($1 is now, $<gateParam> the card check's deploy time or
// NULL): every tracked supporter window, classified.
//
// supporter_window is the account query's tracked set, App Store sandbox
// purchases left out, less the $0 Stripe invoices the reconcile credited
// before the card check deployed (a payment sheet abandoned before its card
// was attached; SubscriptionMetricsStripeTrialCardGate). paid_end_time is the
// period the window paid for as the trial query reads it (cut short by an
// early end); billed_end_time is the period the store billed, uncut.
//
// revenue_row gives each window a kind:
//   - trial: the trial query's free trials, whether or not they are running:
//     a Stripe $0 invoice shorter than any paid period; an App Store free
//     transaction that short; a Play window from the purchase's start to an
//     expiry 2 to 27 days later (the trial and the days Play adds before its
//     first charge). trial_running is the trial query's "inside the trial
//     now", and trial_end_time when its free period ends.
//   - paid: a window a payment paid for: a Stripe invoice that took money, an
//     App Store transaction with a price (or, on a row older than the ledger's
//     price, more revenue than the $0.01 marker), a Play window that charged
//     (its expiry at least 27 days past the purchase's start and 20 days past
//     the purchase's previous credit: the days Play adds move it a day or
//     two), a crypto or gamma term.
//   - ended_early: a paid window an end path (a refund, a dispute, a
//     revocation) cut before its paid period was over. It counts nothing.
//   - none: a grace hold (left out on Play), a free App Store promotion, a
//     Stripe invoice discounted to nothing, the days Play adds.
//
// plan is yearly, monthly or unknown: the App Store product's, else the
// period the store billed (subscriptionMetricsPeriodPlanSql); Stripe and Play
// sell the trial only with the yearly plan. payment_time is when a payment
// was taken: the Stripe invoice's credit (else its period's start), Play's
// period start (the purchase's previous expiry), the App Store purchase, the
// crypto credit, gamma's credit (a gamma renewal's term can start later).
// placeholder marks a payment whose ledger revenue is a placeholder: every
// Play credit, and an App Store sale in another currency.
func subscriptionMetricsRevenueRowsSql(gateParam int) string {
	paidEnd := subscriptionMetricsPaidEndSql("tracked")
	cryptoAndGamma := subscriptionMetricsSqlList(slices.Concat(SubscriptionOneTimeCryptoMarkets, []string{SubscriptionMarketGamma}))
	gate := fmt.Sprintf("$%d::timestamp", gateParam)
	return `reconcile_credited_invoice AS MATERIALIZED (
    SELECT DISTINCT event.evidence AS invoice_id
    FROM payment_reconciliation_event event
    WHERE event.store = 'stripe'
      AND event.action = 'credited'
      AND NOT event.dry_run
      AND event.evidence IS NOT NULL
      AND (` + gate + ` IS NULL OR event.event_time < ` + gate + `)
), supporter_window AS MATERIALIZED (
    SELECT tracked.network_id,
           tracked.market,
           tracked.purchase_token,
           tracked.price_tier,
           tracked.start_time,
           tracked.end_time,
           tracked.net_revenue_nano_cents,
           ` + paidEnd + ` AS paid_end_time,
           CASE
               WHEN tracked.paid_end_time IS NOT NULL THEN tracked.paid_end_time
               ELSE ` + paidEnd + `
           END AS billed_end_time,
           ledger.price_milliunits,
           ledger.currency,
           ledger.product_id,
           invoice.process_time AS invoice_process_time,
           intent.credited_at AS intent_credited_at
    FROM (
        SELECT renewal.*
        FROM subscription_renewal renewal
        WHERE renewal.subscription_type = 'supporter'
          AND renewal.market IN (` + subscriptionMetricsStoresSql + `)
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
          AND NOT (
              renewal.market = 'stripe'
              AND renewal.net_revenue_nano_cents = 0
              AND EXISTS (
                  SELECT 1
                  FROM reconcile_credited_invoice credited
                  WHERE credited.invoice_id = renewal.transaction_id
              )
          )
    ) tracked
    LEFT JOIN apple_subscription_transaction ledger
      ON tracked.market = 'apple'
     AND ledger.transaction_id = tracked.transaction_id
    LEFT JOIN stripe_invoice invoice
      ON tracked.market = 'stripe'
     AND invoice.invoice_id = tracked.transaction_id
    LEFT JOIN gamma_payment_intent intent
      ON tracked.market = 'gamma'
     AND intent.intent_id::text = tracked.transaction_id
), revenue_row AS MATERIALIZED (
    SELECT sw.network_id,
           sw.market,
           sw.purchase_token,
           sw.price_tier,
           sw.start_time,
           CASE
               WHEN sw.net_revenue_nano_cents = 0
                AND sw.paid_end_time < sw.start_time + interval '27 days' THEN 'trial'
               WHEN sw.net_revenue_nano_cents <= 0 THEN 'none'
               WHEN sw.end_time < sw.billed_end_time THEN 'ended_early'
               ELSE 'paid'
           END AS kind,
           CASE
               WHEN sw.net_revenue_nano_cents = 0 THEN 'yearly'
               ELSE ` + subscriptionMetricsPeriodPlanSql("sw.billed_end_time - sw.start_time") + `
           END AS plan,
           COALESCE(sw.invoice_process_time, sw.start_time) AS payment_time,
           sw.net_revenue_nano_cents::float8 / 1e9::float8 AS recorded_usd,
           false AS placeholder,
           sw.paid_end_time AS trial_end_time,
           (sw.start_time <= $1 AND $1 < sw.end_time AND $1 < sw.paid_end_time) AS trial_running
    FROM supporter_window sw
    WHERE sw.market = 'stripe'
    UNION ALL
    SELECT sw.network_id,
           sw.market,
           sw.purchase_token,
           sw.price_tier,
           sw.start_time,
           CASE
               WHEN sw.price_milliunits = 0
                AND 0 < sw.net_revenue_nano_cents
                AND sw.end_time - interval '24 hours' < sw.start_time + interval '27 days' THEN 'trial'
               WHEN NOT (
                   0 < sw.net_revenue_nano_cents
                   AND COALESCE(0 < sw.price_milliunits, 10000000 < sw.net_revenue_nano_cents)
               ) THEN 'none'
               WHEN sw.end_time < sw.billed_end_time THEN 'ended_early'
               ELSE 'paid'
           END,
           CASE
               WHEN sw.product_id ILIKE '%year%' THEN 'yearly'
               WHEN sw.product_id ILIKE '%month%' OR sw.product_id = 'supporter' THEN 'monthly'
               ELSE ` + subscriptionMetricsPeriodPlanSql("sw.billed_end_time - sw.start_time") + `
           END,
           sw.start_time,
           sw.net_revenue_nano_cents::float8 / 1e9::float8,
           COALESCE(0 < sw.price_milliunits AND sw.currency IS DISTINCT FROM 'USD', false),
           sw.end_time - interval '24 hours',
           (sw.start_time <= $1 AND $1 < sw.end_time - interval '24 hours')
    FROM supporter_window sw
    WHERE sw.market = 'apple'
    UNION ALL
    SELECT credit.network_id,
           credit.market,
           credit.purchase_token,
           credit.price_tier,
           credit.start_time,
           CASE
               WHEN credit.start_time + interval '2 days' <= credit.expiry_time
                AND credit.expiry_time < credit.start_time + interval '27 days' THEN 'trial'
               WHEN NOT (
                   credit.start_time + interval '27 days' <= credit.expiry_time
                   AND (
                       credit.previous_expiry_time IS NULL
                       OR credit.previous_expiry_time + interval '20 days' <= credit.expiry_time
                   )
               ) THEN 'none'
               WHEN credit.end_time < credit.billed_end_time THEN 'ended_early'
               ELSE 'paid'
           END,
           CASE
               WHEN credit.expiry_time < credit.start_time + interval '27 days' THEN 'yearly'
               ELSE ` + subscriptionMetricsPeriodPlanSql("credit.expiry_time - COALESCE(credit.previous_expiry_time, credit.start_time)") + `
           END,
           COALESCE(credit.previous_expiry_time, credit.start_time),
           credit.net_revenue_nano_cents::float8 / 1e9::float8,
           true,
           credit.expiry_time,
           (credit.start_time <= $1 AND $1 < credit.expiry_time AND NOT credit.purchase_charged)
    FROM (
        SELECT sw.*,
               sw.end_time - interval '24 hours' AS expiry_time,
               lag(sw.end_time - interval '24 hours') OVER (
                   PARTITION BY sw.network_id, sw.purchase_token
                   ORDER BY sw.end_time
               ) AS previous_expiry_time,
               bool_or(sw.start_time + interval '27 days' <= sw.end_time - interval '24 hours') OVER (
                   PARTITION BY sw.network_id, sw.purchase_token
               ) AS purchase_charged
        FROM supporter_window sw
        WHERE sw.market = 'google'
          AND 0 < sw.net_revenue_nano_cents
    ) credit
    UNION ALL
    SELECT sw.network_id,
           sw.market,
           sw.purchase_token,
           sw.price_tier,
           sw.start_time,
           CASE
               WHEN sw.net_revenue_nano_cents <= 0 THEN 'none'
               WHEN sw.end_time < sw.billed_end_time THEN 'ended_early'
               ELSE 'paid'
           END,
           ` + subscriptionMetricsPeriodPlanSql("sw.billed_end_time - sw.start_time") + `,
           COALESCE(sw.intent_credited_at, sw.start_time),
           sw.net_revenue_nano_cents::float8 / 1e9::float8,
           false,
           NULL::timestamp,
           false
    FROM supporter_window sw
    WHERE sw.market IN (` + cryptoAndGamma + `)
)`
}

// subscriptionMetricsTrialConversionQuery reads, per store that sells a
// trial, the trials whose free period ended between $2 and $3 (the trailing
// period, less the days a first charge may take), and how many converted:
// followed by a paid window in the same store that started from the trial's
// start to $4 seconds after its end. $5 is the card check's deploy time
// (subscriptionMetricsRevenueRowsSql). A Play purchase's trial windows (the
// trial and the days Play adds) are one trial; its conversion is the
// purchase's charged window, which starts at the purchase's start too.
var subscriptionMetricsTrialConversionQuery = `
/* subscription-metrics-trial-conversion */
WITH ` + subscriptionMetricsRevenueRowsSql(5) + `, trial AS (
    SELECT r.network_id, r.market, r.start_time AS trial_start_time, r.trial_end_time
    FROM revenue_row r
    WHERE r.kind = 'trial'
      AND r.market IN ('apple', 'stripe')
    UNION ALL
    SELECT r.network_id, r.market, min(r.start_time), max(r.trial_end_time)
    FROM revenue_row r
    WHERE r.kind = 'trial'
      AND r.market = 'google'
    GROUP BY r.network_id, r.market, r.purchase_token
), ended_trial AS (
    SELECT row_number() OVER () AS trial_number,
           trial.network_id,
           trial.market,
           trial.trial_start_time,
           trial.trial_end_time
    FROM trial
    WHERE $2 <= trial.trial_end_time
      AND trial.trial_end_time < $3
), outcome AS (
    SELECT ended.market,
           bool_or(conversion.network_id IS NOT NULL) AS converted
    FROM ended_trial ended
    LEFT JOIN revenue_row conversion
      ON conversion.network_id = ended.network_id
     AND conversion.market = ended.market
     AND conversion.kind = 'paid'
     AND ended.trial_start_time <= conversion.start_time
     AND conversion.start_time <= ended.trial_end_time + $4::bigint * interval '1 second'
    GROUP BY ended.market, ended.trial_number
)
SELECT market, count(*) FILTER (WHERE converted)::bigint, count(*)::bigint
FROM outcome
GROUP BY market
ORDER BY market
`

// subscriptionMetricsRevenueQuery values the tracked windows ($1 now; $2-$4
// the rolling windows' starts; $5-$7 the price tiers, the first the default;
// $8-$9 each store's fee fraction; $10-$11 each store's trial conversion rate;
// $12 the card check's deploy time, subscriptionMetricsRevenueRowsSql):
//
//   - trial_expected: each trial running now at its plan's yearly net price
//     (the tier's yearly price, or 12 monthly prices, less the store's fee,
//     less the welcome offer's percent off the first year when the network
//     redeemed it in that store) times the store's conversion rate, per store
//     and deduplicated (a network once, at its largest);
//   - new_forecast: each network-store pair (deduplicated: each network) once:
//     a trial running now that started inside the window, at its expected
//     value; else, when its first window (deduplicated: its first window in
//     any tracked store) started inside the window, the yearly net of its
//     first paid window: a monthly net x 12, a yearly net x 1, by plan. A
//     trial that converted counts at the paid window that converted it; a
//     trial that ended without a charge counts nothing;
//   - recurring: the net of every paid window after the network's first paid
//     window in that store, by when its payment was taken, per store and all
//     stores;
//   - trials_convertible: the trials running now, per store and deduplicated.
//
// A payment's net is the ledger's, or, where the ledger holds a placeholder,
// the plan's tier price less the store's fee; a placeholder first paid yearly
// window of a network that redeemed the welcome offer in that store is less
// the offer's percent too.
var subscriptionMetricsRevenueQuery = `
/* subscription-metrics-revenue */
WITH ` + subscriptionMetricsRevenueRowsSql(12) + `, metric_window(window_name, since_time) AS (
    VALUES ('24h', $2::timestamp), ('7d', $3::timestamp), ('30d', $4::timestamp)
), pro_tier(tier_name, yearly_usd, monthly_usd, tier_order) AS (
    SELECT DISTINCT ON (tier.tier_name) tier.tier_name, tier.yearly_usd, tier.monthly_usd, tier.tier_order
    FROM unnest($5::text[], $6::float8[], $7::float8[]) WITH ORDINALITY
        AS tier(tier_name, yearly_usd, monthly_usd, tier_order)
    ORDER BY tier.tier_name, tier.tier_order
), store_fee(store, fee_fraction) AS (
    SELECT fee.store, fee.fee_fraction
    FROM unnest($8::text[], $9::float8[]) AS fee(store, fee_fraction)
), store_conversion(store, conversion_rate) AS (
    SELECT conversion.store, conversion.conversion_rate
    FROM unnest($10::text[], $11::float8[]) AS conversion(store, conversion_rate)
), priced_row AS (
    SELECT r.network_id,
           r.market,
           r.start_time,
           r.kind,
           r.plan,
           r.payment_time,
           r.recorded_usd,
           r.placeholder,
           r.trial_running,
           COALESCE(named_tier.yearly_usd, default_tier.yearly_usd) AS tier_yearly_usd,
           COALESCE(named_tier.monthly_usd, default_tier.monthly_usd) AS tier_monthly_usd,
           COALESCE(fee.fee_fraction, 0) AS fee_fraction,
           COALESCE(conversion.conversion_rate, 0) AS conversion_rate,
           CASE
               WHEN offer.percent_off IS NULL THEN 1
               ELSE 1 - offer.percent_off::float8 / 100
           END AS offer_factor
    FROM revenue_row r
    LEFT JOIN pro_tier named_tier
      ON named_tier.tier_name = r.price_tier
    LEFT JOIN pro_tier default_tier
      ON default_tier.tier_order = 1
    LEFT JOIN store_fee fee
      ON fee.store = r.market
    LEFT JOIN store_conversion conversion
      ON conversion.store = r.market
    LEFT JOIN network_onboarding_offer offer
      ON offer.network_id = r.network_id
     AND offer.redeemed_at IS NOT NULL
     AND offer.store = CASE r.market WHEN 'google' THEN 'play' ELSE r.market END
    WHERE r.kind = 'paid'
       OR (r.kind = 'trial' AND r.trial_running)
), valued_row AS MATERIALIZED (
    SELECT priced.network_id,
           priced.market,
           priced.start_time,
           priced.kind,
           priced.payment_time,
           collected.usd AS collected_usd,
           CASE priced.plan WHEN 'monthly' THEN 12 ELSE 1 END
               * collected.usd
               * CASE WHEN priced.placeholder AND priced.plan = 'yearly' THEN priced.offer_factor ELSE 1 END
               AS first_year_usd,
           CASE priced.plan
               WHEN 'monthly' THEN 12 * priced.tier_monthly_usd
               ELSE priced.tier_yearly_usd
           END
               * (1 - priced.fee_fraction)
               * CASE WHEN priced.plan = 'monthly' THEN 1 ELSE priced.offer_factor END
               * priced.conversion_rate
               AS expected_usd
    FROM priced_row priced
    CROSS JOIN LATERAL (
        SELECT CASE
                   WHEN priced.placeholder AND priced.plan = 'yearly' AND priced.tier_yearly_usd IS NOT NULL
                       THEN priced.tier_yearly_usd * (1 - priced.fee_fraction)
                   WHEN priced.placeholder AND priced.plan = 'monthly' AND priced.tier_monthly_usd IS NOT NULL
                       THEN priced.tier_monthly_usd * (1 - priced.fee_fraction)
                   ELSE priced.recorded_usd
               END AS usd
    ) collected
), store_first AS (
    SELECT r.network_id, r.market, min(r.start_time) AS first_start_time
    FROM revenue_row r
    WHERE r.start_time <= $1
    GROUP BY r.network_id, r.market
), network_first AS (
    SELECT store_first.network_id, min(store_first.first_start_time) AS first_start_time
    FROM store_first
    GROUP BY store_first.network_id
), paid_window AS MATERIALIZED (
    SELECT valued.network_id,
           valued.market,
           valued.payment_time,
           valued.collected_usd,
           valued.first_year_usd,
           row_number() OVER (
               PARTITION BY valued.network_id, valued.market
               ORDER BY valued.payment_time, valued.start_time
           ) AS store_paid_number,
           row_number() OVER (
               PARTITION BY valued.network_id
               ORDER BY valued.payment_time, valued.start_time
           ) AS network_paid_number
    FROM valued_row valued
    WHERE valued.kind = 'paid'
      AND valued.payment_time <= $1
), running_trial AS MATERIALIZED (
    SELECT valued.network_id,
           valued.market,
           min(valued.start_time) AS trial_start_time,
           max(valued.expected_usd) AS expected_usd
    FROM valued_row valued
    WHERE valued.kind = 'trial'
    GROUP BY valued.network_id, valued.market
), window_trial AS MATERIALIZED (
    SELECT metric_window.window_name, running_trial.network_id, running_trial.market, running_trial.expected_usd
    FROM running_trial
    INNER JOIN metric_window
      ON metric_window.since_time <= running_trial.trial_start_time
), store_forecast AS (
    SELECT window_trial.window_name, window_trial.market, window_trial.expected_usd AS value_usd
    FROM window_trial
    UNION ALL
    SELECT metric_window.window_name, store_first.market, paid_window.first_year_usd
    FROM store_first
    INNER JOIN metric_window
      ON metric_window.since_time <= store_first.first_start_time
    INNER JOIN paid_window
      ON paid_window.network_id = store_first.network_id
     AND paid_window.market = store_first.market
     AND paid_window.store_paid_number = 1
    WHERE NOT EXISTS (
        SELECT 1
        FROM window_trial
        WHERE window_trial.window_name = metric_window.window_name
          AND window_trial.network_id = store_first.network_id
          AND window_trial.market = store_first.market
    )
), network_forecast AS (
    SELECT window_trial.window_name, max(window_trial.expected_usd) AS value_usd
    FROM window_trial
    GROUP BY window_trial.window_name, window_trial.network_id
    UNION ALL
    SELECT metric_window.window_name, paid_window.first_year_usd
    FROM network_first
    INNER JOIN metric_window
      ON metric_window.since_time <= network_first.first_start_time
    INNER JOIN paid_window
      ON paid_window.network_id = network_first.network_id
     AND paid_window.network_paid_number = 1
    WHERE NOT EXISTS (
        SELECT 1
        FROM window_trial
        WHERE window_trial.window_name = metric_window.window_name
          AND window_trial.network_id = network_first.network_id
    )
), metric AS (
    SELECT 'new_forecast'::text AS kind, store_forecast.market AS store, store_forecast.window_name,
           sum(store_forecast.value_usd) AS value
    FROM store_forecast
    GROUP BY store_forecast.market, store_forecast.window_name
    UNION ALL
    SELECT 'new_forecast', 'deduplicated', network_forecast.window_name, sum(network_forecast.value_usd)
    FROM network_forecast
    GROUP BY network_forecast.window_name
    UNION ALL
    SELECT 'recurring', paid_window.market, metric_window.window_name, sum(paid_window.collected_usd)
    FROM paid_window
    INNER JOIN metric_window
      ON metric_window.since_time <= paid_window.payment_time
    WHERE 1 < paid_window.store_paid_number
    GROUP BY paid_window.market, metric_window.window_name
    UNION ALL
    SELECT 'recurring', 'all', metric_window.window_name, sum(paid_window.collected_usd)
    FROM paid_window
    INNER JOIN metric_window
      ON metric_window.since_time <= paid_window.payment_time
    WHERE 1 < paid_window.store_paid_number
    GROUP BY metric_window.window_name
    UNION ALL
    SELECT 'trial_expected', running_trial.market, 'current', sum(running_trial.expected_usd)
    FROM running_trial
    GROUP BY running_trial.market
    UNION ALL
    SELECT 'trial_expected', 'deduplicated', 'current', sum(network_trial.expected_usd)
    FROM (
        SELECT max(running_trial.expected_usd) AS expected_usd
        FROM running_trial
        GROUP BY running_trial.network_id
    ) network_trial
    UNION ALL
    SELECT 'trials_convertible', running_trial.market, 'current', count(*)::float8
    FROM running_trial
    GROUP BY running_trial.market
    UNION ALL
    SELECT 'trials_convertible', 'deduplicated', 'current', count(DISTINCT running_trial.network_id)::float8
    FROM running_trial
)
SELECT kind, store, window_name, COALESCE(value, 0)::float8
FROM metric
ORDER BY kind, store, window_name
`

// The parameters of subscriptionMetricsRevenueQuery after the four times:
// the tiers, the fees and the conversion rates as parallel arrays, then the
// card check's deploy time (nil: not deployed).
func subscriptionMetricsRevenueArgs(
	tiers []SubscriptionMetricsPriceTier,
	feeFractions map[string]float64,
	rates map[string]float64,
	stripeTrialCardGate *time.Time,
) []any {
	tierNames := []string{}
	tierYearly := []float64{}
	tierMonthly := []float64{}
	for _, tier := range tiers {
		tierNames = append(tierNames, tier.Name)
		tierYearly = append(tierYearly, tier.YearlyUsd)
		tierMonthly = append(tierMonthly, tier.MonthlyUsd)
	}
	feeStores := []string{}
	fees := []float64{}
	rateStores := []string{}
	storeRates := []float64{}
	for _, store := range SubscriptionMetricsStores() {
		if fee, ok := feeFractions[store]; ok {
			feeStores = append(feeStores, store)
			fees = append(fees, fee)
		}
		rateStores = append(rateStores, store)
		storeRates = append(storeRates, rates[store])
	}
	return []any{tierNames, tierYearly, tierMonthly, feeStores, fees, rateStores, storeRates, stripeTrialCardGate}
}

// subscriptionMetricsRevenueSettings resolves the settings' revenue inputs:
// the tiers of pro.yml and the default fees where the settings name none.
func subscriptionMetricsRevenueSettings(settings *SubscriptionMetricsSettings) (
	tiers []SubscriptionMetricsPriceTier,
	feeFractions map[string]float64,
) {
	tiers = settings.PriceTiers
	if tiers == nil {
		tiers = subscriptionMetricsProPriceTiers()
	}
	feeFractions = settings.StoreFeeFractions
	if feeFractions == nil {
		feeFractions = DefaultSubscriptionMetricsStoreFeeFractions()
	}
	return
}

// The conversion query's period: trials whose free period ended from
// period before now to settle before now, the settle as the most a
// conversion may start after its trial ended, and the card check's deploy
// time (nil: not deployed).
func subscriptionMetricsTrialConversionArgs(now time.Time, settings *SubscriptionMetricsSettings) []any {
	return []any{
		now.Add(-settings.TrialConversionPeriod),
		now.Add(-settings.TrialConversionSettle),
		int64(settings.TrialConversionSettle / time.Second),
		settings.StripeTrialCardGateTime,
	}
}

// loadSubscriptionMetricsRevenue is the snapshot's revenue step: the trial
// conversion rates, then the revenue that values trials at them, into the
// snapshot's revenue fields. It runs on its own connection under its own time
// bound (settings.RevenueTimeout), after the snapshot's other queries, so a
// slow or failing revenue query leaves every other number of the snapshot
// current. On a failure it logs, leaves the revenue fields empty and returns
// false; the collector then publishes no revenue series for the snapshot (no
// data, never a false zero) and counts the failure.
func loadSubscriptionMetricsRevenue(
	ctx context.Context,
	now time.Time,
	settings *SubscriptionMetricsSettings,
	snapshot *SubscriptionMetricsSnapshot,
) (loaded bool) {
	revenueCtx := ctx
	if 0 < settings.RevenueTimeout {
		var cancel context.CancelFunc
		revenueCtx, cancel = context.WithTimeout(ctx, settings.RevenueTimeout)
		defer cancel()
	}
	cutoff24Hours := now.Add(-24 * time.Hour)
	cutoff7Days := now.Add(-7 * 24 * time.Hour)
	cutoff30Days := now.Add(-30 * 24 * time.Hour)
	// filled here and handed to the snapshot only once every query read
	revenue := newSubscriptionMetricsSnapshot()
	err := server.HandleError(func() {
		server.ReplicaDb(revenueCtx, func(conn server.PgConn) {
			result, err := conn.Query(
				revenueCtx,
				subscriptionMetricsTrialConversionQuery,
				append([]any{now}, subscriptionMetricsTrialConversionArgs(now, settings)...)...,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var store string
					var converted int64
					var ended int64
					server.Raise(result.Scan(&store, &converted, &ended))
					revenue.TrialConversionTrials[SubscriptionMetricsStoreOutcome{Store: store, Outcome: SubscriptionMetricsTrialOutcomeConverted}] = converted
					revenue.TrialConversionTrials[SubscriptionMetricsStoreOutcome{Store: store, Outcome: SubscriptionMetricsTrialOutcomeLapsed}] = ended - converted
				}
			})
			for _, outcome := range SubscriptionMetricsTrialOutcomes() {
				pooled := int64(0)
				for _, store := range SubscriptionMetricsStores() {
					pooled += revenue.TrialConversionTrials[SubscriptionMetricsStoreOutcome{Store: store, Outcome: outcome}]
				}
				revenue.TrialConversionTrials[SubscriptionMetricsStoreOutcome{Store: SubscriptionMetricsStoreAll, Outcome: outcome}] = pooled
			}
			revenue.TrialConversionRates = subscriptionMetricsTrialConversionRates(
				revenue.TrialConversionTrials,
				settings.TrialConversionMinTrials,
			)

			tiers, feeFractions := subscriptionMetricsRevenueSettings(settings)
			result, err = conn.Query(
				revenueCtx,
				subscriptionMetricsRevenueQuery,
				append(
					[]any{now, cutoff24Hours, cutoff7Days, cutoff30Days},
					subscriptionMetricsRevenueArgs(tiers, feeFractions, revenue.TrialConversionRates, settings.StripeTrialCardGateTime)...,
				)...,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var kind string
					var store string
					var window string
					var value float64
					server.Raise(result.Scan(&kind, &store, &window, &value))
					switch kind {
					case "new_forecast":
						revenue.NewForecastRevenueUsd[SubscriptionMetricsStoreWindow{Store: store, Window: window}] = value
					case "recurring":
						revenue.RecurringRevenueUsd[SubscriptionMetricsStoreWindow{Store: store, Window: window}] = value
					case "trial_expected":
						revenue.TrialExpectedRevenueUsd[store] = value
					case "trials_convertible":
						revenue.TrialsConvertible[store] = int64(math.Round(value))
					}
				}
			})
		})
	})
	if err != nil {
		glog.Errorf("[subscription metrics]revenue not refreshed; this snapshot publishes no revenue series: %v\n", err)
		return false
	}
	snapshot.NewForecastRevenueUsd = revenue.NewForecastRevenueUsd
	snapshot.RecurringRevenueUsd = revenue.RecurringRevenueUsd
	snapshot.TrialExpectedRevenueUsd = revenue.TrialExpectedRevenueUsd
	snapshot.TrialsConvertible = revenue.TrialsConvertible
	snapshot.TrialConversionRates = revenue.TrialConversionRates
	snapshot.TrialConversionTrials = revenue.TrialConversionTrials
	return true
}
