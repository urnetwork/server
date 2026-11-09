package sidecarhost

import (
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Runs declare and returns what it panicked with, "" when it did not.
func metricsPanic(declare func()) (message string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			message = fmt.Sprint(recovered)
			if message == "" {
				message = "panic"
			}
		}
	}()
	declare()
	return ""
}

// A name outside urnetwork_<svc>_* panics at declaration (§19.8).
func TestMetricsRefuseANameOutsideUrnetwork(t *testing.T) {
	metrics := newMetrics(prometheus.NewRegistry())
	for _, name := range []string{"evs_requests_total", "urnetwork_requests", "urnetwork_Evs_requests", "urnetwork_evs-requests_total", "other_evs_requests_total"} {
		if message := metricsPanic(func() { metrics.Counter(name, "help") }); message == "" {
			t.Errorf("%q was declared", name)
		}
	}
	if message := metricsPanic(func() { metrics.Counter("urnetwork_evs_requests_total", "help") }); message != "" {
		t.Errorf("a good name panicked: %s", message)
	}
}

// A label named for a network, client, repo, host, alias, address, user or domain panics at declaration,
// so no series can carry one (D-OPS-8, ops-1, I-13).
func TestMetricsRefuseIdentityLabels(t *testing.T) {
	metrics := newMetrics(prometheus.NewRegistry())
	for _, label := range []string{"network", "network_id", "client_id", "repo", "repo_id", "host", "hostname", "alias", "address", "client_ip", "user", "user_id", "domain", "source_addr", "account_id", "email"} {
		if message := metricsPanic(func() {
			metrics.Counter("urnetwork_evs_refused_"+strings.ReplaceAll(label, "-", "_")+"_total", "help", label)
		}); message == "" {
			t.Errorf("counter label %q was declared", label)
		}
		if message := metricsPanic(func() { metrics.Gauge("urnetwork_evs_level_"+label, "help", "route", label) }); message == "" {
			t.Errorf("gauge label %q was declared", label)
		}
		if message := metricsPanic(func() { metrics.Histogram("urnetwork_evs_seconds_"+label, "help", nil, label) }); message == "" {
			t.Errorf("histogram label %q was declared", label)
		}
	}
	// §19.8's vocabularies are fine
	for _, label := range []string{"route", "code", "result", "cluster", "tier", "class", "kind", "outcome", "platform", "topic", "by", "state", "pass", "window", "resource"} {
		if message := metricsPanic(func() { metrics.Counter("urnetwork_evs_ok_"+label+"_total", "help", label) }); message != "" {
			t.Errorf("label %q panicked: %s", label, message)
		}
	}
}

// A second declaration alike returns the same metric, so counts from two engines add up in one series;
// another kind or label set panics, as MustRegister does.
func TestMetricsRepeatDeclarationReturnsTheSameMetric(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newMetrics(registry)
	first := metrics.Counter("urnetwork_evs_pushes_total", "pushes", "result")
	second := metrics.Counter("urnetwork_evs_pushes_total", "pushes, again", "result")
	first.Add(2, "ok")
	second.Add(3, "ok")
	if count, err := testutil.GatherAndCount(registry, "urnetwork_evs_pushes_total"); err != nil || count != 1 {
		t.Fatalf("series %d, %v", count, err)
	}
	if value := testutil.ToFloat64(metrics.declared["urnetwork_evs_pushes_total"].counter.WithLabelValues("ok")); value != 5 {
		t.Fatalf("value %v", value)
	}

	if message := metricsPanic(func() { metrics.Gauge("urnetwork_evs_pushes_total", "pushes", "result") }); message == "" {
		t.Error("a second kind was declared")
	}
	if message := metricsPanic(func() { metrics.Counter("urnetwork_evs_pushes_total", "pushes", "result", "cluster") }); message == "" {
		t.Error("a second label set was declared")
	}
	if message := metricsPanic(func() { metrics.Counter("urnetwork_evs_pushes_total", "pushes") }); message == "" {
		t.Error("no labels for a labelled name was declared")
	}

	gauge := metrics.Gauge("urnetwork_evs_ready", "ready")
	gauge.Set(1)
	metrics.Gauge("urnetwork_evs_ready", "ready").Set(0)
	if value := testutil.ToFloat64(metrics.declared["urnetwork_evs_ready"].gauge.WithLabelValues()); value != 0 {
		t.Fatalf("gauge %v", value)
	}

	histogram := metrics.Histogram("urnetwork_evs_push_seconds", "seconds", []float64{0.1, 1}, "result")
	histogram.Observe(0.05, "ok")
	metrics.Histogram("urnetwork_evs_push_seconds", "seconds", nil, "result").Observe(0.5, "ok")
	if count := testutil.CollectAndCount(metrics.declared["urnetwork_evs_push_seconds"].histogram); count != 1 {
		t.Fatalf("histogram series %d", count)
	}
}

// What prometheus refuses at a write still panics: a wrong number of label values, a negative count.
func TestMetricsRefuseBadWrites(t *testing.T) {
	metrics := newMetrics(prometheus.NewRegistry())
	counter := metrics.Counter("urnetwork_evs_refused_total", "help", "result")
	if message := metricsPanic(func() { counter.Add(1) }); message == "" {
		t.Error("too few label values were written")
	}
	if message := metricsPanic(func() { counter.Add(1, "a", "b") }); message == "" {
		t.Error("too many label values were written")
	}
	if message := metricsPanic(func() { counter.Add(-1, "a") }); message == "" {
		t.Error("a counter fell")
	}
}

// The process's metrics are one value on the default registry, so StartStatsPusher carries them.
func TestNewMetricsIsTheProcesss(t *testing.T) {
	if NewMetrics() != NewMetrics() {
		t.Fatal("two metrics for one process")
	}
	if NewMetrics().registerer != prometheus.DefaultRegisterer {
		t.Fatal("the process's metrics are not on the default registry")
	}
}
