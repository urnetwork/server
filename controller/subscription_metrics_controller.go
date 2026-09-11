package controller

// The internal subscription dashboard is fed by one recurring Taskworker
// snapshot. Durable database ledgers are reduced to bounded store/window/action
// labels here; customer and payment identifiers never enter Prometheus.

import (
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
	"Distinct networks with a current paid supporter window, per tracked store and deduplicated across stores.",
	[]string{"store"},
	nil,
)

var subscriptionMetricsNewPaidAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_new_paid_accounts",
	"Distinct networks whose first paid supporter window for a store began inside the rolling window, plus a first-ever deduplicated total across tracked stores.",
	[]string{"store", "window"},
	nil,
)

var subscriptionMetricsEngagedAccountsDesc = prometheus.NewDesc(
	"urnetwork_subscription_engaged_accounts",
	"Current paid networks with an active top-level client authenticated inside the rolling window, per store and deduplicated.",
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

var subscriptionMetricsSnapshotTimestampDesc = prometheus.NewDesc(
	"urnetwork_subscription_snapshot_timestamp_seconds",
	"Unix time when this Taskworker completed the complete privacy-safe subscription metrics snapshot.",
	nil,
	nil,
)

var subscriptionMetricsDescs = []*prometheus.Desc{
	subscriptionMetricsActiveAccountsDesc,
	subscriptionMetricsNewPaidAccountsDesc,
	subscriptionMetricsEngagedAccountsDesc,
	subscriptionMetricsChurnedAccountsDesc,
	subscriptionMetricsReconciliationEventsDesc,
	subscriptionMetricsReconciliationSourceTimestampDesc,
	subscriptionMetricsDataPackFulfillmentsDesc,
	subscriptionMetricsDataPackBytesDesc,
	subscriptionMetricsSnapshotTimestampDesc,
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
		metrics = append(metrics, prometheus.MustNewConstMetric(
			subscriptionMetricsActiveAccountsDesc,
			prometheus.GaugeValue,
			float64(snapshot.ActiveAccounts[store]),
			store,
		))
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

// Loads and atomically publishes one complete privacy-safe aggregate snapshot.
func SubscriptionMetricsSync(
	_ *SubscriptionMetricsSyncArgs,
	clientSession *session.ClientSession,
) (*SubscriptionMetricsSyncResult, error) {
	startedAt := server.NowUtc()
	snapshot := model.LoadSubscriptionMetricsSnapshot(clientSession.Ctx, startedAt)
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
