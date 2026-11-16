package controller

// The internal subscription dashboard is fed by one recurring Taskworker
// snapshot. Durable database ledgers are reduced to bounded store/window/action
// labels here; customer and payment identifiers never enter Prometheus.

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	subscriptionMetricsSyncInterval = 15 * time.Minute
	subscriptionMetricsSyncMaxTime  = 5 * time.Minute
	subscriptionMetricsTaskKey      = "subscription_metrics_sync"
)

var subscriptionMetricsActiveAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_active_accounts",
	"Distinct networks with a current supporter window (paid, free trial, or a grace period after a failed charge), per tracked store and deduplicated across stores. App Store sandbox purchases are left out.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsInGraceAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_in_grace_accounts",
	"Distinct networks past due: a current supporter window but none that still covers now with the period it paid for (a grace after a failed or cancelled renewal, a store's billing grace hold, or the day after a free trial), per tracked store and deduplicated across stores. They count in urnetwork_subscription_active_accounts too.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsPaymentFailuresOpenDesc = prometheus.NewDesc(
	"urnetwork_subscription_payment_failures_open",
	"Failed subscription renewals still failing now (no payment, void, uncollectible mark or subscription end since), per store that records them.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsPaymentFailuresResolvedDesc = prometheus.NewDesc(
	"urnetwork_subscription_payment_failures_resolved",
	"Failed subscription renewals that stopped failing inside the rolling window, per store and reason: paid, voided, uncollectible, or subscription_ended.",
	[]string{"store", "reason", "window"},
	nil,
)

var subscriptionMetricsTrialsActiveDesc = prometheus.NewDesc(
	"urnetwork_subscription_trials_active",
	"Distinct networks inside a free trial now (nothing charged yet), per tracked store and plan, plus deduplicated across stores. They count in urnetwork_subscription_active_accounts too.",
	[]string{"store", "plan"},
	nil,
)

var subscriptionMetricsNewPaidAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_new_paid_accounts",
	"Distinct networks whose first supporter window for a store (a free trial's start included) began inside the rolling window, plus a first-ever deduplicated total across tracked stores.",
	[]string{"store", "window"},
	nil,
)

var subscriptionMetricsEngagedAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_engaged_accounts",
	"Current subscribers (trials included) with an active top-level client authenticated inside the rolling window, per store and deduplicated.",
	[]string{"store", "window"},
	nil,
)

var subscriptionMetricsChurnedAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_churned_accounts",
	"Network-store pairs whose latest supporter window ended inside the rolling window and remain inactive; deduplicated counts have no active tracked store.",
	[]string{"store", "window"},
	nil,
)

var subscriptionMetricsReconciliationEventsDesc = prometheus.NewDesc(
	"urnetwork_subscription_reconciliation_events",
	"Durable non-dry-run payment lifecycle audit events inside the rolling window, by bounded store and action.",
	[]string{"store", "action", "window"},
	nil,
)

var subscriptionMetricsReconciliationSourceTimestampDesc = prometheus.NewDesc(
	"urnetwork_subscription_reconciliation_source_timestamp_seconds",
	"Latest reconciliation heartbeat (store=all) and successful per-store watermark update used to fail closed on stale audit coverage.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsDataPackFulfillmentsDesc = prometheus.NewDesc(
	"urnetwork_subscription_data_pack_fulfillments",
	"Paid data-only fulfillment ledger rows, split into balance-code and direct-balance sources plus their deduplicated total.",
	[]string{"source", "window"},
	nil,
)

var subscriptionMetricsDataPackBytesDesc = prometheus.NewDesc(
	"urnetwork_subscription_data_pack_bytes",
	"Bytes granted by paid data-only fulfillment ledger rows, split into balance-code and direct-balance sources plus their deduplicated total.",
	[]string{"source", "window"},
	nil,
)

var subscriptionMetricsUncreditedPaymentsDesc = prometheus.NewDesc(
	"urnetwork_subscription_payments_uncredited",
	"Payments received at least the snapshot's minimum age ago (30 minutes by default) that no credit consumed, per rail: a USDC payment into a Solana receiver that bought nothing (usdc_solana), a crypto rail receipt not credited (seen, confirmed or held for review), a Pay with Gamma charge spent but not credited (gamma).",
	[]string{"rail"},
	nil,
)

var subscriptionMetricsUncreditedOldestAgeDesc = prometheus.NewDesc(
	"urnetwork_subscription_payments_uncredited_oldest_age_seconds",
	"Age of the oldest payment counted in urnetwork_subscription_payments_uncredited, per rail that has one.",
	[]string{"rail"},
	nil,
)

var subscriptionMetricsSnapshotTimestampDesc = prometheus.NewDesc(
	"urnetwork_subscription_snapshot_timestamp_seconds",
	"Unix time when this Taskworker completed the complete privacy-safe subscription metrics snapshot.",
	nil,
	nil,
)

var subscriptionMetricsNewForecastRevenueDesc = prometheus.NewDesc(
	"urnetwork_subscription_new_forecast_revenue_usd",
	"Net new forecasted revenue, in USD after store fees: the yearly net value of the subscriptions that started inside the rolling window, per tracked store and deduplicated (each network once, by its first window in any tracked store). "+
		"A subscription whose first supporter window in the store began inside the window counts at its first paid window's net for a year: a monthly net x 12, a yearly net x 1, by the plan, never by the window's length. A trial that converted counts at the paid window that converted it. "+
		"A trial running now that started inside the window counts at its expected value instead (urnetwork_subscription_trial_expected_revenue_usd, counted once), and a trial that ended without a charge counts nothing. "+
		"Net is the ledger's net revenue, except where it holds a placeholder: a Google Play credit (the play.yml supporter SKU's $5 less 30% whatever the plan) and an App Store sale in another currency (a $0.01 marker) are valued at the plan's pro.yml price tier less the store's fee, and less the welcome offer's percent on a first year the network redeemed it for. "+
		"App Store sandbox purchases, Stripe trials only the payment reconcile credited (a payment sheet abandoned before its card was attached), refunded or revoked payments, gift redemptions and the manual and x402 markets count nothing.",
	[]string{"store", "window"},
	nil,
)

var subscriptionMetricsRecurringRevenueDesc = prometheus.NewDesc(
	"urnetwork_subscription_recurring_revenue_usd",
	"Net recurring revenue, in USD after store fees: the net collected from renewals inside the rolling window, per tracked store and all stores together (all adds every store's renewals). "+
		"A renewal is a paid supporter window after the network's first paid window in that store; a store's first paid window (a new subscription, or a trial's first charge) and trials never count. "+
		"Each renewal is dated by its payment: the Stripe invoice's credit, the start of the Google Play period it paid for, the App Store purchase, the crypto credit, Pay with Gamma's credit. A crypto year paid again in the same market is a renewal. "+
		"Net is the ledger's net revenue, except where it holds a placeholder: a Google Play credit and an App Store sale in another currency are valued at the plan's pro.yml price tier less the store's fee. "+
		"Refunded or revoked payments, App Store sandbox purchases, gift redemptions (no sale) and the manual and x402 markets count nothing.",
	[]string{"store", "window"},
	nil,
)

var subscriptionMetricsTrialExpectedRevenueDesc = prometheus.NewDesc(
	"urnetwork_subscription_trial_expected_revenue_usd",
	"Expected trial conversion revenue, in USD after store fees: each free trial running now at its plan's yearly net price (the pro.yml price tier's yearly price, or 12 monthly prices, less the store's fee, and less the welcome offer's percent when the network redeemed it in that store) times its store's trial conversion rate (urnetwork_subscription_trial_conversion_rate), per tracked store and deduplicated (a network once, at its largest). "+
		"It is the trial part of urnetwork_subscription_new_forecast_revenue_usd{window=\"30d\"}, counted there once. The trials are urnetwork_subscription_trials_convertible.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsTrialsConvertibleDesc = prometheus.NewDesc(
	"urnetwork_subscription_trials_convertible",
	"Distinct networks inside a free trial now that can convert: the trials urnetwork_subscription_trial_expected_revenue_usd values, per tracked store and deduplicated. "+
		"They are urnetwork_subscription_trials_active less the Stripe trials only the payment reconcile credited: a payment sheet abandoned before its card was attached, which can never convert.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsTrialConversionRateDesc = prometheus.NewDesc(
	"urnetwork_subscription_trial_conversion_rate",
	"The trial conversion rate each tracked store's trials are valued at: of the trials whose free period ended between 90 and 7 days ago, the share followed by a paid window in the same store that started no later than 7 days after the trial ended. "+
		"A store with fewer than 30 such trials (every store that sells no trial among them) uses the pooled rate of every store's trials, store=all. Stripe trials only the payment reconcile credited are left out.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsTrialConversionTrialsDesc = prometheus.NewDesc(
	"urnetwork_subscription_trial_conversion_trials",
	"The trials behind urnetwork_subscription_trial_conversion_rate: those whose free period ended between 90 and 7 days ago, per tracked store and all stores, by outcome: converted (a paid window in the same store started no later than 7 days after the trial ended) or lapsed.",
	[]string{"store", "outcome"},
	nil,
)

var subscriptionMetricsDescs = []*prometheus.Desc{
	subscriptionMetricsActiveAccountsDesc,
	subscriptionMetricsInGraceAccountsDesc,
	subscriptionMetricsPaymentFailuresOpenDesc,
	subscriptionMetricsPaymentFailuresResolvedDesc,
	subscriptionMetricsTrialsActiveDesc,
	subscriptionMetricsNewPaidAccountsDesc,
	subscriptionMetricsEngagedAccountsDesc,
	subscriptionMetricsChurnedAccountsDesc,
	subscriptionMetricsReconciliationEventsDesc,
	subscriptionMetricsReconciliationSourceTimestampDesc,
	subscriptionMetricsDataPackFulfillmentsDesc,
	subscriptionMetricsDataPackBytesDesc,
	subscriptionMetricsUncreditedPaymentsDesc,
	subscriptionMetricsUncreditedOldestAgeDesc,
	subscriptionMetricsSnapshotTimestampDesc,
	subscriptionMetricsNewForecastRevenueDesc,
	subscriptionMetricsRecurringRevenueDesc,
	subscriptionMetricsTrialExpectedRevenueDesc,
	subscriptionMetricsTrialsConvertibleDesc,
	subscriptionMetricsTrialConversionRateDesc,
	subscriptionMetricsTrialConversionTrialsDesc,
}

// One immutable sample slice makes every Prometheus collection a coherent
// subscription snapshot. A writer materializes the replacement completely
// before swapping it under stateLock; concurrent collectors retain the prior
// slice until they finish.
type subscriptionMetricsCollector struct {
	stateLock sync.Mutex
	snapshot  *subscriptionMetricsCollectedSnapshot
}

// Contains const metrics that are never mutated after publication.
type subscriptionMetricsCollectedSnapshot struct {
	metrics []prometheus.Metric
}

// Starts with only an explicitly unready timestamp; business series remain
// absent until the first complete refresh.
func newSubscriptionMetricsCollector() *subscriptionMetricsCollector {
	return &subscriptionMetricsCollector{
		snapshot: &subscriptionMetricsCollectedSnapshot{
			metrics: []prometheus.Metric{
				prometheus.MustNewConstMetric(
					subscriptionMetricsSnapshotTimestampDesc,
					prometheus.GaugeValue,
					0,
				),
			},
		},
	}
}

// Lists the fixed descriptors even before the first completed snapshot.
func (self *subscriptionMetricsCollector) Describe(descs chan<- *prometheus.Desc) {
	for _, desc := range subscriptionMetricsDescs {
		descs <- desc
	}
}

// Captures one immutable generation before sending any sample. Sending does
// not hold stateLock, so a slow scrape cannot delay a Taskworker refresh.
func (self *subscriptionMetricsCollector) Collect(metrics chan<- prometheus.Metric) {
	var collectedSnapshot *subscriptionMetricsCollectedSnapshot
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		collectedSnapshot = self.snapshot
	}()
	for _, metric := range collectedSnapshot.metrics {
		metrics <- metric
	}
}

var subscriptionMetrics = newSubscriptionMetricsCollector()

func init() {
	prometheus.MustRegister(subscriptionMetrics)
}

// The singleton exporter has no customer-scoped arguments.
type SubscriptionMetricsSyncArgs struct{}

// Carries only aggregate task timing, never dashboard values or identities.
type SubscriptionMetricsSyncResult struct {
	DurationSeconds  float64 `json:"duration_seconds"`
	SnapshotUnixTime int64   `json:"snapshot_unix_time"`
}

// Schedules the single fleet-wide exporter owner at the requested time.
func ScheduleSubscriptionMetricsSync(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		SubscriptionMetricsSync,
		&SubscriptionMetricsSyncArgs{},
		clientSession,
		task.RunOnce(subscriptionMetricsTaskKey),
		task.RunAt(at),
		task.MaxTime(subscriptionMetricsSyncMaxTime),
	)
}

// Materializes every bounded series before atomically publishing the new
// collection generation. The caller retains no mutable map ownership.
func (self *subscriptionMetricsCollector) set(snapshot *model.SubscriptionMetricsSnapshot, refreshedAt time.Time) {
	metrics := []prometheus.Metric{}
	stores := append(model.SubscriptionMetricsStores(), model.SubscriptionMetricsStoreDeduplicated)
	for _, store := range stores {
		metrics = append(metrics,
			prometheus.MustNewConstMetric(
				subscriptionMetricsActiveAccountsDesc,
				prometheus.GaugeValue,
				float64(snapshot.ActiveAccounts[store]),
				store,
			),
			prometheus.MustNewConstMetric(
				subscriptionMetricsInGraceAccountsDesc,
				prometheus.GaugeValue,
				float64(snapshot.InGraceAccounts[store]),
				store,
			),
		)
		for _, plan := range model.SubscriptionMetricsPlans() {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				subscriptionMetricsTrialsActiveDesc,
				prometheus.GaugeValue,
				float64(snapshot.TrialsActive[model.SubscriptionMetricsStorePlan{Store: store, Plan: plan}]),
				store,
				plan,
			))
		}
		for _, window := range model.SubscriptionMetricsWindows() {
			key := model.SubscriptionMetricsStoreWindow{Store: store, Window: window}
			metrics = append(metrics,
				prometheus.MustNewConstMetric(
					subscriptionMetricsEngagedAccountsDesc,
					prometheus.GaugeValue,
					float64(snapshot.EngagedAccounts[key]),
					store,
					window,
				),
				prometheus.MustNewConstMetric(
					subscriptionMetricsChurnedAccountsDesc,
					prometheus.GaugeValue,
					float64(snapshot.ChurnedAccounts[key]),
					store,
					window,
				),
			)
		}
	}
	for _, store := range stores {
		for _, window := range model.SubscriptionMetricsWindows() {
			key := model.SubscriptionMetricsStoreWindow{Store: store, Window: window}
			metrics = append(metrics, prometheus.MustNewConstMetric(
				subscriptionMetricsNewPaidAccountsDesc,
				prometheus.GaugeValue,
				float64(snapshot.NewPaidAccounts[key]),
				store,
				window,
			))
		}
	}

	// every store that records failed renewals, reason and window, zero
	// included
	for _, store := range model.SubscriptionMetricsPaymentFailureStores() {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			subscriptionMetricsPaymentFailuresOpenDesc,
			prometheus.GaugeValue,
			float64(snapshot.PaymentFailuresOpen[store]),
			store,
		))
		for _, reason := range model.SubscriptionMetricsPaymentFailureResolveReasons() {
			for _, window := range model.SubscriptionMetricsWindows() {
				key := model.SubscriptionMetricsStoreReasonWindow{Store: store, Reason: reason, Window: window}
				metrics = append(metrics, prometheus.MustNewConstMetric(
					subscriptionMetricsPaymentFailuresResolvedDesc,
					prometheus.GaugeValue,
					float64(snapshot.PaymentFailuresResolved[key]),
					store,
					reason,
					window,
				))
			}
		}
	}

	for _, store := range model.SubscriptionMetricsStores() {
		for _, action := range model.SubscriptionMetricsReconciliationActions() {
			for _, window := range model.SubscriptionMetricsWindows() {
				key := model.SubscriptionMetricsStoreActionWindow{Store: store, Action: action, Window: window}
				metrics = append(metrics, prometheus.MustNewConstMetric(
					subscriptionMetricsReconciliationEventsDesc,
					prometheus.GaugeValue,
					float64(snapshot.ReconciliationEvents[key]),
					store,
					action,
					window,
				))
			}
		}
		if watermark, ok := snapshot.ReconciliationWatermarks[store]; ok {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				subscriptionMetricsReconciliationSourceTimestampDesc,
				prometheus.GaugeValue,
				float64(watermark.Unix()),
				store,
			))
		}
	}
	if snapshot.ReconciliationHeartbeat != nil {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			subscriptionMetricsReconciliationSourceTimestampDesc,
			prometheus.GaugeValue,
			float64(snapshot.ReconciliationHeartbeat.Unix()),
			"all",
		))
	}

	dataPackSources := []string{
		model.SubscriptionMetricsDataPackBalanceCode,
		model.SubscriptionMetricsDataPackDirect,
		model.SubscriptionMetricsDataPackDeduplicated,
	}
	dataPackWindows := append(model.SubscriptionMetricsWindows(), model.SubscriptionMetricsWindowAll)
	for _, source := range dataPackSources {
		for _, window := range dataPackWindows {
			key := model.SubscriptionMetricsDataPackWindow{Source: source, Window: window}
			metrics = append(metrics,
				prometheus.MustNewConstMetric(
					subscriptionMetricsDataPackFulfillmentsDesc,
					prometheus.GaugeValue,
					float64(snapshot.DataPackFulfillments[key]),
					source,
					window,
				),
				prometheus.MustNewConstMetric(
					subscriptionMetricsDataPackBytesDesc,
					prometheus.GaugeValue,
					float64(snapshot.DataPackBytes[key]),
					source,
					window,
				),
			)
		}
	}

	// every rail, zero included; an age only where a payment waits
	for _, rail := range model.SubscriptionMetricsUncreditedRails() {
		metrics = append(metrics, prometheus.MustNewConstMetric(
			subscriptionMetricsUncreditedPaymentsDesc,
			prometheus.GaugeValue,
			float64(snapshot.UncreditedPayments[rail]),
			rail,
		))
		if 0 < snapshot.UncreditedPayments[rail] {
			metrics = append(metrics, prometheus.MustNewConstMetric(
				subscriptionMetricsUncreditedOldestAgeDesc,
				prometheus.GaugeValue,
				float64(snapshot.UncreditedOldestAgeSeconds[rail]),
				rail,
			))
		}
	}

	metrics = append(metrics, subscriptionMetricsRevenueSeries(snapshot)...)

	metrics = append(metrics, prometheus.MustNewConstMetric(
		subscriptionMetricsSnapshotTimestampDesc,
		prometheus.GaugeValue,
		float64(refreshedAt.UTC().Unix()),
	))
	collectedSnapshot := &subscriptionMetricsCollectedSnapshot{metrics: metrics}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.snapshot = collectedSnapshot
	}()
}

// The revenue series, every label zero-filled: the forecast by tracked store
// and deduplicated, recurring revenue by tracked store and all, per window;
// the trials running now and their expected revenue by tracked store and
// deduplicated; the conversion rate and its trials by tracked store and all.
func subscriptionMetricsRevenueSeries(snapshot *model.SubscriptionMetricsSnapshot) []prometheus.Metric {
	metrics := []prometheus.Metric{}
	deduplicatedStores := append(model.SubscriptionMetricsStores(), model.SubscriptionMetricsStoreDeduplicated)
	allStores := append(model.SubscriptionMetricsStores(), model.SubscriptionMetricsStoreAll)
	gauge := func(desc *prometheus.Desc, value float64, labels ...string) {
		metrics = append(metrics, prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...))
	}
	for _, window := range model.SubscriptionMetricsWindows() {
		for _, store := range deduplicatedStores {
			key := model.SubscriptionMetricsStoreWindow{Store: store, Window: window}
			gauge(subscriptionMetricsNewForecastRevenueDesc, snapshot.NewForecastRevenueUsd[key], store, window)
		}
		for _, store := range allStores {
			key := model.SubscriptionMetricsStoreWindow{Store: store, Window: window}
			gauge(subscriptionMetricsRecurringRevenueDesc, snapshot.RecurringRevenueUsd[key], store, window)
		}
	}
	for _, store := range deduplicatedStores {
		gauge(subscriptionMetricsTrialExpectedRevenueDesc, snapshot.TrialExpectedRevenueUsd[store], store)
		gauge(subscriptionMetricsTrialsConvertibleDesc, float64(snapshot.TrialsConvertible[store]), store)
	}
	for _, store := range allStores {
		gauge(subscriptionMetricsTrialConversionRateDesc, snapshot.TrialConversionRates[store], store)
		for _, outcome := range model.SubscriptionMetricsTrialOutcomes() {
			key := model.SubscriptionMetricsStoreOutcome{Store: store, Outcome: outcome}
			gauge(subscriptionMetricsTrialConversionTrialsDesc, float64(snapshot.TrialConversionTrials[key]), store, outcome)
		}
	}
	return metrics
}

// The fee each store keeps of a price, as its credit path takes it off when
// it records net revenue: Stripe's invoice credit (stripeInvoiceFeeFraction),
// the App Store's validation (appleTransactionFeeFraction), and Google Play's
// supporter SKU (play.yml fee_fraction; the model's default when play.yml
// names none). The subscriptions dashboard values a placeholder payment and a
// trial's plan at a tier price less these.
func subscriptionMetricsStoreFeeFractions() map[string]float64 {
	feeFractions := model.DefaultSubscriptionMetricsStoreFeeFractions()
	feeFractions[model.SubscriptionMarketStripe] = stripeInvoiceFeeFraction
	feeFractions[model.SubscriptionMarketApple] = appleTransactionFeeFraction
	if feeFraction, ok := subscriptionMetricsPlayFeeFraction(); ok {
		feeFractions[model.SubscriptionMarketGoogle] = feeFraction
	}
	return feeFractions
}

// The fee play.yml's supporter SKU takes off each Play credit, the first by
// product id when there are several. ok is false when play.yml is absent or
// names no supporter SKU.
func subscriptionMetricsPlayFeeFraction() (feeFraction float64, ok bool) {
	err := server.HandleError(func() {
		skus := playSkusFunc()
		productIds := slices.Sorted(maps.Keys(skus))
		for _, productId := range productIds {
			if sku := skus[productId]; sku != nil && sku.Supporter {
				feeFraction = sku.FeeFraction
				ok = true
				return
			}
		}
	})
	if err != nil {
		return 0, false
	}
	return feeFraction, ok
}

// The snapshot's settings: the defaults, with the fees the credit paths take.
func subscriptionMetricsSettings() *model.SubscriptionMetricsSettings {
	settings := model.DefaultSubscriptionMetricsSettings()
	settings.StoreFeeFractions = subscriptionMetricsStoreFeeFractions()
	return settings
}

// Loads and atomically publishes one complete privacy-safe aggregate snapshot.
func SubscriptionMetricsSync(
	_ *SubscriptionMetricsSyncArgs,
	clientSession *session.ClientSession,
) (*SubscriptionMetricsSyncResult, error) {
	startedAt := server.NowUtc()
	snapshot := model.LoadSubscriptionMetricsSnapshotWithSettings(clientSession.Ctx, startedAt, subscriptionMetricsSettings())
	completedAt := server.NowUtc()
	subscriptionMetrics.set(snapshot, completedAt)
	return &SubscriptionMetricsSyncResult{
		DurationSeconds:  completedAt.Sub(startedAt).Seconds(),
		SnapshotUnixTime: completedAt.Unix(),
	}, nil
}

// Schedules one successor only after the complete snapshot task finishes.
func SubscriptionMetricsSyncPost(
	_ *SubscriptionMetricsSyncArgs,
	_ *SubscriptionMetricsSyncResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSubscriptionMetricsSync(clientSession, tx, server.NowUtc().Add(subscriptionMetricsSyncInterval))
	return nil
}
