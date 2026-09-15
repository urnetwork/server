package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func metricCardinalityFixture(values map[string]int, privateMarker string) string {
	results := []map[string]any{}
	for _, name := range []string{"total", "egress_buckets", "egress_sums", "egress_counts", "redis_latency"} {
		metric := map[string]string{"monitor_check": name}
		if privateMarker != "" {
			metric["untrusted_label"] = privateMarker
		}
		results = append(results, map[string]any{
			"metric": metric,
			"value":  []any{1_788_800_000, fmt.Sprintf("%d", values[name])},
		})
	}
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result":     results,
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

func runMetricCardinalitySynthetic(t *testing.T, response string) ([]Alert, error) {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics.invalid" || !strings.Contains(command, metricCardinalityMarker) {
			return "", fmt.Errorf("unexpected cardinality target")
		}
		for _, metric := range []string{
			"urnetwork_egress_probe_health_check_seconds_bucket",
			"urnetwork_egress_probe_health_check_seconds_sum",
			"urnetwork_egress_probe_health_check_seconds_count",
			"redis_latency_percentiles_usec",
		} {
			if !strings.Contains(command, metric) {
				return "", fmt.Errorf("cardinality query omitted %s", metric)
			}
		}
		return response, nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.invalid", Roles: []string{"services"}})
	return NewCardinalitySignal().Run(context.Background(), settings)
}

func TestCardinalitySignalFindsBothAvoidableMultipliers(t *testing.T) {
	signal := NewCardinalitySignal()
	if signal.Number() != "11.20d" || signal.Key() != "cardinality" ||
		signal.ID() != "observability/metric-cardinality" || signal.Cadence() != 5*time.Minute {
		t.Fatalf("wrong signal metadata: %s %s %s %s", signal.Number(), signal.Key(), signal.ID(), signal.Cadence())
	}

	const privateMarker = "synthetic-private-label-value"
	alerts, err := runMetricCardinalitySynthetic(t, metricCardinalityFixture(map[string]int{
		"total": 85_000, "egress_buckets": 8_100, "egress_sums": 625, "egress_counts": 625, "redis_latency": 9_000,
	}, privateMarker))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("cardinality alerts = %d, want 2: %+v", len(alerts), alerts)
	}
	taskworker := requireAlertClass(t, alerts, "taskworker-histogram-cardinality")
	for _, want := range []string{"egress_health_bucket_series=8100", "summary/max implementation", "Do not remove process identity"} {
		if !strings.Contains(taskworker.Markdown(), want) {
			t.Fatalf("Taskworker alert missing %q:\n%s", want, taskworker.Markdown())
		}
	}
	redis := requireAlertClass(t, alerts, "redis-latencystats-cardinality")
	for _, want := range []string{"redis_latency_series=9000", "latency-tracking", "Retain commandstats"} {
		if !strings.Contains(redis.Markdown(), want) {
			t.Fatalf("Redis alert missing %q:\n%s", want, redis.Markdown())
		}
	}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, privateMarker)
	}
}

func TestCardinalitySignalHealthyOnlyAfterBothFamiliesDisappear(t *testing.T) {
	alerts, err := runMetricCardinalitySynthetic(t, metricCardinalityFixture(map[string]int{
		"total": 70_000, "egress_buckets": 0, "egress_sums": 625, "egress_counts": 625, "redis_latency": 0,
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("healthy cardinality alerts = %+v", alerts)
	}
}

func TestCardinalitySignalRejectsUnpairedReplacementMetrics(t *testing.T) {
	alerts, err := runMetricCardinalitySynthetic(t, metricCardinalityFixture(map[string]int{
		"total": 70_000, "egress_buckets": 0, "egress_sums": 625, "egress_counts": 624, "redis_latency": 0,
	}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("unpaired latency alerts = %d, want 1: %+v", len(alerts), alerts)
	}
	alert := requireAlertClass(t, alerts, "taskworker-latency-pair-mismatch")
	for _, want := range []string{"egress_health_sum_series=625", "egress_health_count_series=624", "necessary but not sufficient"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("pair mismatch alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestParseMetricCardinalityFailsClosed(t *testing.T) {
	for _, fixture := range []string{
		`{"status":"success","data":{"resultType":"vector","result":[]}}`,
		`{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_check":"unknown"},"value":[1,"1"]}]}}`,
		metricCardinalityFixture(map[string]int{
			"total": -1, "egress_buckets": 0, "egress_sums": 0, "egress_counts": 0, "redis_latency": 0,
		}, ""),
	} {
		if _, err := parseMetricCardinalitySample(fixture); err == nil {
			t.Errorf("invalid cardinality response parsed successfully: %s", fixture)
		}
	}
}

func TestCardinalitySignalRegistered(t *testing.T) {
	selected, err := IncludeSignals(NewSignals(), "cardinality")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 1 || selected[0].Number() != "11.20d" {
		t.Fatalf("registered cardinality signal = %+v", selected)
	}
}
