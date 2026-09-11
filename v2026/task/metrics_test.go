package task

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server/v2026"
)

func TestTaskMetricLabelsAndOutcomesAreBounded(t *testing.T) {
	if got := taskMetricName("github.com/urnetwork/server/v2026/controller.SubscriptionMetricsSync"); got != "controller.SubscriptionMetricsSync" {
		t.Fatalf("task metric name = %q, want controller.SubscriptionMetricsSync", got)
	}
	worker := &TaskWorker{targetMetricNames: map[string]string{"registered-function": "controller.Registered"}}
	if got := worker.metricName("caller-controlled-function"); got != "unregistered" {
		t.Fatalf("unregistered task metric name = %q, want unregistered", got)
	}
	for err, want := range map[error]string{
		nil:                         "succeeded",
		ErrDrained:                  "drained",
		ErrTargetNotFound:           "target_not_found",
		errors.New("synthetic err"): "failed",
	} {
		if got := taskMetricOutcome(err); got != want {
			t.Errorf("task metric outcome for %v = %q, want %q", err, got, want)
		}
	}
	if got := taskMetricAttribution(&Task{ClientByJwtJson: "synthetic-jwt-json"}); got != "client" {
		t.Fatalf("client attribution = %q, want client", got)
	}
	if got := taskMetricAttribution(&Task{}); got != "system" {
		t.Fatalf("system attribution = %q, want system", got)
	}
}

func TestTaskExecutionMaximumResetsAtIntervalBoundary(t *testing.T) {
	collector := newTaskExecutionMaxCollector()
	now := time.Unix(1_800_000_000, 0)
	collector.now = func() time.Time { return now }
	collector.observe("controller.Registered", "system", 3*time.Second)
	collector.observe("controller.Registered", "system", time.Second)
	key := "controller.Registered\x00system"
	if got := collector.samples[key].seconds; got != 3 {
		t.Fatalf("same-interval maximum = %v, want 3", got)
	}
	now = now.Add(taskMetricsMaxInterval)
	collector.observe("controller.Registered", "system", 250*time.Millisecond)
	if got := collector.samples[key].seconds; got != 0.25 {
		t.Fatalf("next-interval maximum = %v, want 0.25", got)
	}
}

func TestTaskQueueMetricsCollectorSwapCannotMixScrapeGenerations(t *testing.T) {
	collector := newTaskQueueMetricsCollector()
	oldTime := time.Unix(1_800_000_000, 0)
	newTime := oldTime.Add(time.Minute)
	collector.publish(uniformTaskQueueSnapshot(1), oldTime)

	metricChannel := make(chan prometheus.Metric)
	go func() {
		collector.Collect(metricChannel)
		close(metricChannel)
	}()
	firstMetric, ok := <-metricChannel
	if !ok {
		t.Fatal("task queue collector returned no metrics")
	}
	collector.publish(uniformTaskQueueSnapshot(2), newTime)
	oldMetrics := []prometheus.Metric{firstMetric}
	for metric := range metricChannel {
		oldMetrics = append(oldMetrics, metric)
	}
	requireTaskQueueGeneration(t, oldMetrics, 1, oldTime)

	metricChannel = make(chan prometheus.Metric)
	go func() {
		collector.Collect(metricChannel)
		close(metricChannel)
	}()
	newMetrics := []prometheus.Metric{}
	for metric := range metricChannel {
		newMetrics = append(newMetrics, metric)
	}
	requireTaskQueueGeneration(t, newMetrics, 2, newTime)
}

// uniformTaskQueueSnapshot makes mixed generations immediately visible.
func uniformTaskQueueSnapshot(value int64) taskQueueMetricsSnapshot {
	return taskQueueMetricsSnapshot{
		Total:               value,
		Available:           value,
		Claimed:             value,
		RescheduleError:     value,
		OldestOverdueSecond: float64(value),
	}
}

// requireTaskQueueGeneration requires all six values to share one generation.
func requireTaskQueueGeneration(t *testing.T, metrics []prometheus.Metric, value int64, observedAt time.Time) {
	t.Helper()
	if len(metrics) != 6 {
		t.Fatalf("task queue metrics = %d, want 6", len(metrics))
	}
	for _, metric := range metrics {
		var dtoMetric dto.Metric
		if err := metric.Write(&dtoMetric); err != nil {
			t.Fatal(err)
		}
		want := float64(value)
		if strings.Contains(metric.Desc().String(), `fqName: "urnetwork_taskworker_queue_snapshot_timestamp_seconds"`) {
			want = float64(observedAt.Unix())
		}
		if got := dtoMetric.GetGauge().GetValue(); got != want {
			t.Fatalf("mixed task queue metric %s = %v, want %v", metric.Desc(), got, want)
		}
	}
}

func TestTaskQueueMetricsQueryExecutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		snapshot, err := loadTaskQueueMetricsSnapshot(context.Background(), time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Total < 0 || snapshot.Available < 0 || snapshot.Claimed < 0 || snapshot.RescheduleError < 0 || snapshot.OldestOverdueSecond < 0 {
			t.Fatalf("task queue aggregate contains a negative value: %+v", snapshot)
		}
	})
}
