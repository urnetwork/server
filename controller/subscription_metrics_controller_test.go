package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

func TestSubscriptionMetricsCollectorPublishesCompleteBoundedSnapshot(t *testing.T) {
	refreshedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	heartbeat := refreshedAt.Add(-30 * time.Minute)
	snapshot := &model.SubscriptionMetricsSnapshot{
		ActiveAccounts: map[string]int64{
			model.SubscriptionMarketApple:              4,
			model.SubscriptionMetricsStoreDeduplicated: 9,
		},
		NewPaidAccounts: map[model.SubscriptionMetricsStoreWindow]int64{
			{Store: model.SubscriptionMarketApple, Window: model.SubscriptionMetricsWindow7Days}:              2,
			{Store: model.SubscriptionMetricsStoreDeduplicated, Window: model.SubscriptionMetricsWindow7Days}: 3,
		},
		EngagedAccounts: map[model.SubscriptionMetricsStoreWindow]int64{
			{Store: model.SubscriptionMarketApple, Window: model.SubscriptionMetricsWindow7Days}: 2,
		},
		ChurnedAccounts: map[model.SubscriptionMetricsStoreWindow]int64{
			{Store: model.SubscriptionMarketGoogle, Window: model.SubscriptionMetricsWindow24Hours}: 1,
		},
		ReconciliationEvents: map[model.SubscriptionMetricsStoreActionWindow]int64{
			{Store: model.SubscriptionMarketStripe, Action: model.PaymentReconcileActionCredited, Window: model.SubscriptionMetricsWindow7Days}: 2,
		},
		ReconciliationHeartbeat: &heartbeat,
		ReconciliationWatermarks: map[string]time.Time{
			model.SubscriptionMarketApple:  heartbeat.Add(-time.Minute),
			model.SubscriptionMarketGoogle: heartbeat.Add(-2 * time.Minute),
			model.SubscriptionMarketStripe: heartbeat.Add(-3 * time.Minute),
			model.SubscriptionMarketSolana: heartbeat.Add(-4 * time.Minute),
		},
		DataPackFulfillments: map[model.SubscriptionMetricsDataPackWindow]int64{
			{Source: model.SubscriptionMetricsDataPackDeduplicated, Window: model.SubscriptionMetricsWindow7Days}: 5,
		},
		DataPackBytes: map[model.SubscriptionMetricsDataPackWindow]int64{
			{Source: model.SubscriptionMetricsDataPackDeduplicated, Window: model.SubscriptionMetricsWindow7Days}: 11 * model.Tib,
		},
	}

	collector := newSubscriptionMetricsCollector()
	collector.set(snapshot, refreshedAt)
	metricFamilies := gatherSubscriptionMetricFamilies(t, collector)

	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_active_accounts", map[string]string{"store": model.SubscriptionMarketApple}); got != 4 {
		t.Fatalf("active apple = %v, want 4", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_active_accounts", map[string]string{"store": model.SubscriptionMarketSolana}); got != 0 {
		t.Fatalf("missing active store = %v, want a known zero", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_new_paid_accounts", map[string]string{"store": model.SubscriptionMetricsStoreDeduplicated, "window": model.SubscriptionMetricsWindow7Days}); got != 3 {
		t.Fatalf("new paid 7d = %v, want 3", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_new_paid_accounts", map[string]string{"store": model.SubscriptionMarketApple, "window": model.SubscriptionMetricsWindow7Days}); got != 2 {
		t.Fatalf("new paid apple 7d = %v, want 2", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_engaged_accounts", map[string]string{"store": model.SubscriptionMarketApple, "window": model.SubscriptionMetricsWindow7Days}); got != 2 {
		t.Fatalf("engaged apple 7d = %v, want 2", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_churned_accounts", map[string]string{"store": model.SubscriptionMarketGoogle, "window": model.SubscriptionMetricsWindow24Hours}); got != 1 {
		t.Fatalf("churned google 24h = %v, want 1", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_reconciliation_events", map[string]string{"store": model.SubscriptionMarketStripe, "action": model.PaymentReconcileActionCredited, "window": model.SubscriptionMetricsWindow7Days}); got != 2 {
		t.Fatalf("stripe credited 7d = %v, want 2", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_data_pack_fulfillments", map[string]string{"source": model.SubscriptionMetricsDataPackDeduplicated, "window": model.SubscriptionMetricsWindow7Days}); got != 5 {
		t.Fatalf("data-pack fulfillments 7d = %v, want 5", got)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_data_pack_bytes", map[string]string{"source": model.SubscriptionMetricsDataPackDeduplicated, "window": model.SubscriptionMetricsWindow7Days}); got != float64(11*model.Tib) {
		t.Fatalf("data-pack bytes 7d = %v, want %d", got, 11*model.Tib)
	}
	if got := subscriptionMetricValue(t, metricFamilies, "urnetwork_subscription_snapshot_timestamp_seconds", nil); got != float64(refreshedAt.Unix()) {
		t.Fatalf("snapshot timestamp = %v, want %d", got, refreshedAt.Unix())
	}

	if got := len(metricFamilies["urnetwork_subscription_active_accounts"].Metric); got != 5 {
		t.Errorf("active series = %d, want 5 bounded stores", got)
	}
	if got := len(metricFamilies["urnetwork_subscription_engaged_accounts"].Metric); got != 15 {
		t.Errorf("engagement series = %d, want 5 stores x 3 windows", got)
	}
	if got := len(metricFamilies["urnetwork_subscription_new_paid_accounts"].Metric); got != 15 {
		t.Errorf("new-paid series = %d, want 5 stores x 3 windows", got)
	}
	if got := len(metricFamilies["urnetwork_subscription_reconciliation_events"].Metric); got != 72 {
		t.Errorf("reconciliation series = %d, want 4 stores x 6 actions x 3 windows", got)
	}
	if got := len(metricFamilies["urnetwork_subscription_data_pack_fulfillments"].Metric); got != 12 {
		t.Errorf("data-pack series = %d, want 3 sources x 4 windows", got)
	}

	// Replacement deletes a missing source timestamp. A stale previous
	// watermark must not keep a later dashboard snapshot looking observable.
	delete(snapshot.ReconciliationWatermarks, model.SubscriptionMarketGoogle)
	collector.set(snapshot, refreshedAt.Add(time.Minute))
	metricFamilies = gatherSubscriptionMetricFamilies(t, collector)
	if got := len(metricFamilies["urnetwork_subscription_reconciliation_source_timestamp_seconds"].Metric); got != 4 {
		t.Errorf("reconciliation source timestamp series = %d, want heartbeat plus 3 present stores", got)
	}
}

// A scrape that has started emitting one generation must finish that exact
// generation even when Taskworker publishes a replacement in the middle.
func TestSubscriptionMetricsCollectorSwapCannotMixScrapeGenerations(t *testing.T) {
	collector := newSubscriptionMetricsCollector()
	oldRefreshedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	newRefreshedAt := oldRefreshedAt.Add(time.Minute)
	oldSourceTime := oldRefreshedAt.Add(-time.Hour)
	newSourceTime := newRefreshedAt.Add(-time.Hour)
	collector.set(uniformSubscriptionMetricsSnapshot(1, oldSourceTime), oldRefreshedAt)

	metricCh := make(chan prometheus.Metric)
	go func() {
		collector.Collect(metricCh)
		close(metricCh)
	}()
	firstMetric, ok := <-metricCh
	if !ok {
		t.Fatal("collector returned no metrics")
	}

	// The unbuffered collector is now blocked on its next send. This forces the
	// replacement after Collect captured its generation but before it finished.
	collector.set(uniformSubscriptionMetricsSnapshot(2, newSourceTime), newRefreshedAt)
	oldMetrics := []prometheus.Metric{firstMetric}
	for metric := range metricCh {
		oldMetrics = append(oldMetrics, metric)
	}
	requireSubscriptionMetricsGeneration(t, oldMetrics, 1, oldSourceTime, oldRefreshedAt)
	requireSubscriptionMetricsGeneration(
		t,
		collectSubscriptionMetrics(collector),
		2,
		newSourceTime,
		newRefreshedAt,
	)
}

// Collects one generation through the same channel boundary Prometheus uses.
func collectSubscriptionMetrics(collector *subscriptionMetricsCollector) []prometheus.Metric {
	metricCh := make(chan prometheus.Metric)
	go func() {
		collector.Collect(metricCh)
		close(metricCh)
	}()
	metrics := []prometheus.Metric{}
	for metric := range metricCh {
		metrics = append(metrics, metric)
	}
	return metrics
}

// Creates an unmistakable complete generation for the forced interleaving
// test; timestamp families use their own time-valued marker.
func uniformSubscriptionMetricsSnapshot(value int64, sourceTime time.Time) *model.SubscriptionMetricsSnapshot {
	snapshot := &model.SubscriptionMetricsSnapshot{
		ActiveAccounts:           map[string]int64{},
		NewPaidAccounts:          map[model.SubscriptionMetricsStoreWindow]int64{},
		EngagedAccounts:          map[model.SubscriptionMetricsStoreWindow]int64{},
		ChurnedAccounts:          map[model.SubscriptionMetricsStoreWindow]int64{},
		ReconciliationEvents:     map[model.SubscriptionMetricsStoreActionWindow]int64{},
		ReconciliationHeartbeat:  &sourceTime,
		ReconciliationWatermarks: map[string]time.Time{},
		DataPackFulfillments:     map[model.SubscriptionMetricsDataPackWindow]int64{},
		DataPackBytes:            map[model.SubscriptionMetricsDataPackWindow]int64{},
	}
	stores := append(model.SubscriptionMetricsStores(), model.SubscriptionMetricsStoreDeduplicated)
	for _, store := range stores {
		snapshot.ActiveAccounts[store] = value
		for _, window := range model.SubscriptionMetricsWindows() {
			key := model.SubscriptionMetricsStoreWindow{Store: store, Window: window}
			snapshot.NewPaidAccounts[key] = value
			snapshot.EngagedAccounts[key] = value
			snapshot.ChurnedAccounts[key] = value
		}
	}
	for _, store := range model.SubscriptionMetricsStores() {
		snapshot.ReconciliationWatermarks[store] = sourceTime
		for _, action := range model.SubscriptionMetricsReconciliationActions() {
			for _, window := range model.SubscriptionMetricsWindows() {
				key := model.SubscriptionMetricsStoreActionWindow{Store: store, Action: action, Window: window}
				snapshot.ReconciliationEvents[key] = value
			}
		}
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
			snapshot.DataPackFulfillments[key] = value
			snapshot.DataPackBytes[key] = value
		}
	}
	return snapshot
}

// Requires every sample to come from one expected immutable generation.
func requireSubscriptionMetricsGeneration(
	t *testing.T,
	metrics []prometheus.Metric,
	businessValue int64,
	sourceTime time.Time,
	refreshedAt time.Time,
) {
	t.Helper()
	if len(metrics) != 152 {
		t.Fatalf("collected metrics = %d, want 152 bounded samples", len(metrics))
	}
	for _, metric := range metrics {
		var dtoMetric dto.Metric
		if err := metric.Write(&dtoMetric); err != nil {
			t.Fatal(err)
		}
		got := dtoMetric.GetGauge().GetValue()
		desc := metric.Desc().String()
		want := float64(businessValue)
		switch {
		case strings.Contains(desc, `fqName: "urnetwork_subscription_snapshot_timestamp_seconds"`):
			want = float64(refreshedAt.Unix())
		case strings.Contains(desc, `fqName: "urnetwork_subscription_reconciliation_source_timestamp_seconds"`):
			want = float64(sourceTime.Unix())
		case !strings.Contains(desc, `fqName: "urnetwork_subscription_`):
			t.Fatalf("unexpected metric descriptor %s", desc)
		}
		if got != want {
			t.Fatalf("mixed subscription snapshot metric %s = %v, want %v", desc, got, want)
		}
	}
}

// Gathers through a private pedantic registry so package-global collectors do
// not affect exact family and label assertions.
func gatherSubscriptionMetricFamilies(t *testing.T, collector prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(collector); err != nil {
		t.Fatal(err)
	}
	metricFamilyList, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	metricFamilies := map[string]*dto.MetricFamily{}
	for _, metricFamily := range metricFamilyList {
		metricFamilies[metricFamily.GetName()] = metricFamily
	}
	return metricFamilies
}

// Finds one exact bounded-label series and returns its gauge value.
func subscriptionMetricValue(
	t *testing.T,
	metricFamilies map[string]*dto.MetricFamily,
	name string,
	labels map[string]string,
) float64 {
	t.Helper()
	metricFamily := metricFamilies[name]
	if metricFamily == nil {
		t.Fatalf("metric family %s is absent", name)
	}
	var value *float64
	for _, metric := range metricFamily.Metric {
		matched := len(metric.Label) == len(labels)
		for labelName, labelValue := range labels {
			found := false
			for _, pair := range metric.Label {
				if pair.GetName() == labelName && pair.GetValue() == labelValue {
					found = true
					break
				}
			}
			matched = matched && found
		}
		if !matched {
			continue
		}
		if value != nil {
			t.Fatalf("metric family %s repeated labels %#v", name, labels)
		}
		metricValue := metric.GetGauge().GetValue()
		value = &metricValue
	}
	if value == nil {
		t.Fatalf("metric family %s omitted labels %#v", name, labels)
	}
	return *value
}

func TestScheduleSubscriptionMetricsSyncIsFleetWideRunOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Cleanup(server.Vault.PushSimpleResource(
			"client.yml",
			[]byte("client_ip_hash_pepper: synthetic-subscription-metrics-test-only\n"),
		))
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.10:1", nil)
		defer clientSession.Cancel()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleSubscriptionMetricsSync(clientSession, tx, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
			ScheduleSubscriptionMetricsSync(clientSession, tx, time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC))
		})

		functionName := task.NewTaskTarget(SubscriptionMetricsSync).TargetFunctionName()
		var count int
		var maxTimeSeconds int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT count(*), max(run_max_time_seconds) FROM pending_task WHERE function_name = $1`,
				functionName,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&count, &maxTimeSeconds))
				}
			})
		})
		if count != 1 {
			t.Fatalf("subscription metrics tasks = %d, want one RunOnce owner", count)
		}
		if got := time.Duration(maxTimeSeconds) * time.Second; got != subscriptionMetricsSyncMaxTime {
			t.Fatalf("subscription metrics max time = %v, want %v", got, subscriptionMetricsSyncMaxTime)
		}
		if subscriptionMetricsSyncMaxTime >= subscriptionMetricsSyncInterval {
			t.Fatalf("subscription metrics max time %v must remain below cadence %v", subscriptionMetricsSyncMaxTime, subscriptionMetricsSyncInterval)
		}
	})
}
