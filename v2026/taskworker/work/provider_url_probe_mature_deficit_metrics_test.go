// Diagnostic observations have fixed aggregate labels and the census clock.
package work

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/v2026/model"
)

func urlProbeMatureDeficitMetricFixture() model.ProviderUrlProbeMatureDeficitDiagnostics {
	return model.ProviderUrlProbeMatureDeficitDiagnostics{
		ContractVersion: 1, PolicyVersion: 1, SampleLimit: 128, Selected: 128,
		TotalMatureDeficient: 140, RunsNeeded: 222, Capped: 1,
		AcceptedSuccesses: 800, AcceptedFailures: 258,
		MissingCredits: [4]int{100, 11, 10, 7}, Deadline: [5]int{10, 20, 30, 40, 28}, HintFalse: 3,
		Claim: [4]int{1, 2, 25, 100}, PendingExact900: 17, CompletedExact900: 80, CompletedLocalFailure: 60,
		Attempt: [4]int{2, 60, 50, 16}, RetainedFewer10: 11, ExpiredAge: [3]int{20, 30, 70},
		ClaimVsExpiry: [5]int{8, 4, 17, 48, 51}, CompletionVsExpiry: [3]int{10, 90, 28},
		LatestAccepted: [3]int{0, 5, 123}, PriorityReady: 77, SecurityException: 3, CompletedCountGE10: 111,
		ClaimClockFuture: 1, AttemptClockFuture: 2, CompletionReceiveLagMaxSeconds: 2.5, AttemptUpdateLagMaxSeconds: 3.75,
	}
}

func urlProbeFleetMetricKey(metric prometheus.Metric, value *dto.Metric) string {
	key := metric.Desc().String()
	for _, label := range value.Label {
		key += fmt.Sprintf("|%s=%s", label.GetName(), label.GetValue())
	}
	return key
}

func urlProbeFleetMetricValues(t *testing.T, collector *providerUrlProbeFleetCollector) map[string]float64 {
	t.Helper()
	metrics := make(chan prometheus.Metric, 128)
	collector.Collect(metrics)
	close(metrics)
	values := map[string]float64{}
	for metric := range metrics {
		var value dto.Metric
		if err := metric.Write(&value); err != nil {
			t.Fatal(err)
		}
		key := urlProbeFleetMetricKey(metric, &value)
		if _, exists := values[key]; exists {
			t.Fatalf("duplicate aggregate metric %s", key)
		}
		values[key] = value.GetGauge().GetValue()
	}
	return values
}

func urlProbeMatureDeficitMetricFamilies(t *testing.T, collector *providerUrlProbeFleetCollector) map[string]*dto.MetricFamily {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, family := range families {
		out[family.GetName()] = family
	}
	return out
}

func TestUrlProbeMatureDeficitMetricsClosedVocabularyAndUnits(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{
		fleet:      model.ProviderUrlProbeFleet{MatureDeficitDiagnostics: urlProbeMatureDeficitMetricFixture()},
		observedAt: time.Unix(123, 0),
	})
	families := urlProbeMatureDeficitMetricFamilies(t, collector)
	want := map[string]float64{
		"policy_version": 1, "sample_limit": 128, "selected": 128, "total_mature_deficient": 140,
		"runs_needed": 222, "capped": 1, "accepted_successes": 800, "accepted_failures": 258,
		"missing_credit_1": 100, "missing_credit_2": 11, "missing_credit_3_to9": 10, "missing_credit_10": 7,
		"deadline_due": 10, "deadline_0_to90_seconds": 20, "deadline_90_to360_seconds": 30,
		"deadline_360_to900_seconds": 40, "deadline_over900_seconds": 28, "hint_false": 3,
		"claim_never": 1, "claim_missing": 2, "claim_pending": 25, "claim_completed": 100,
		"pending_exact900": 17, "completed_exact900": 80, "completed_local_failure": 60,
		"attempt_missing": 2, "attempt_local_setup_submit": 60, "attempt_no_failure_text": 50, "attempt_other_failure": 16,
		"retained_history_fewer10": 11, "expired_age_0_to90_seconds": 20,
		"expired_age_90_to360_seconds": 30, "expired_age_over360_seconds": 70,
		"claim_vs_expiry_no_expired_receipt": 8, "claim_vs_expiry_missing_or_future": 4,
		"claim_vs_expiry_before_headroom": 17, "claim_vs_expiry_in_headroom": 48, "claim_vs_expiry_at_or_after_expiry": 51,
		"completion_vs_expiry_before": 10, "completion_vs_expiry_at_or_after": 90, "completion_vs_expiry_missing_or_unknown": 28,
		"latest_accepted_none": 0, "latest_accepted_0_to90_seconds": 5, "latest_accepted_over90_seconds": 123,
		"priority_ready": 77, "security_exception": 3, "completed_count_ge10": 111,
		"claim_clock_future": 1, "attempt_clock_future": 2,
		"completion_receive_lag_max_seconds": 2.5, "attempt_update_lag_max_seconds": 3.75,
	}
	family := families["urnetwork_url_probe_mature_deficit_diagnostic"]
	if family == nil || len(family.Metric) != len(want) || len(want) != 51 {
		t.Fatalf("diagnostic shape changed: metrics=%v expected=%d", family, len(want))
	}
	seen := map[string]bool{}
	for _, metric := range family.Metric {
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "state" {
			t.Fatalf("private or open diagnostic label: %s", metric.String())
		}
		state := metric.Label[0].GetValue()
		value, known := want[state]
		if !known || seen[state] || metric.GetGauge().GetValue() != value {
			t.Fatalf("unknown, duplicate or miswired diagnostic cell: %s", metric.String())
		}
		seen[state] = true
	}
	version := families["urnetwork_url_probe_mature_deficit_diagnostic_contract"]
	if version == nil || len(version.Metric) != 1 || len(version.Metric[0].Label) != 0 || version.Metric[0].GetGauge().GetValue() != 1 {
		t.Fatal("diagnostic contract is not a snapshot-owned scalar")
	}
	if families["urnetwork_url_probe_fleet_observed_timestamp_seconds"].Metric[0].GetGauge().GetValue() != 123 {
		t.Fatal("diagnostics lost the census comparison clock")
	}
	// Legacy state vocabulary and admission partitions remain closed as the
	// separate extension grows. The reader budget is exactly two generations.
	if len(families["urnetwork_url_probe_fleet"].Metric) != 12 || len(families["urnetwork_url_probe_admission_cohort"].Metric) != 9 {
		t.Fatal("diagnostics changed the established quota census shape")
	}
	const runtimeRows, rawClockAndCountRows, slots, generations = 8, 3, 8, 2
	if rows := (runtimeRows + len(want) + 1 + 1 + rawClockAndCountRows) * slots * generations; rows != 1024 {
		t.Fatalf("dedicated diagnostic projection budget changed: %d rows", rows)
	}
}

func TestUrlProbeMatureDeficitMetricsEmptyIsDistinctFromUnobserved(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	if families := urlProbeMatureDeficitMetricFamilies(t, collector); len(families) != 0 {
		t.Fatal("uninitialized collector manufactured diagnostic evidence")
	}
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{
		fleet: model.ProviderUrlProbeFleet{MatureEligible: 100, MatureQuotaComplete: 90}, observedAt: time.Unix(1, 0),
	})
	for name := range urlProbeMatureDeficitMetricFamilies(t, collector) {
		if name == "urnetwork_url_probe_mature_deficit_diagnostic" || name == "urnetwork_url_probe_mature_deficit_diagnostic_contract" {
			t.Fatal("default or legacy fleet became an authoritative empty diagnostic")
		}
	}
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{
		fleet: model.ProviderUrlProbeFleet{MatureDeficitDiagnostics: model.ProviderUrlProbeMatureDeficitDiagnostics{
			ContractVersion: 1, PolicyVersion: 1, SampleLimit: 128,
		}}, observedAt: time.Unix(2, 0),
	})
	families := urlProbeMatureDeficitMetricFamilies(t, collector)
	family := families["urnetwork_url_probe_mature_deficit_diagnostic"]
	if family == nil || len(family.Metric) != 51 {
		t.Fatal("complete empty census omitted explicit diagnostic zero cells")
	}
	for _, metric := range family.Metric {
		state := metric.Label[0].GetValue()
		want := float64(0)
		if state == "policy_version" {
			want = 1
		} else if state == "sample_limit" {
			want = 128
		}
		if metric.GetGauge().GetValue() != want {
			t.Fatalf("empty diagnostic invented observations: %s", metric.String())
		}
	}
}

func TestUrlProbeMatureDeficitMetricsRetainPolicyAndFutureClocks(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	diagnostic := urlProbeMatureDeficitMetricFixture()
	diagnostic.PolicyVersion = -1
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{
		fleet: model.ProviderUrlProbeFleet{MatureDeficitDiagnostics: diagnostic}, observedAt: time.Unix(10, 0),
	})
	family := urlProbeMatureDeficitMetricFamilies(t, collector)["urnetwork_url_probe_mature_deficit_diagnostic"]
	seen := map[string]float64{}
	for _, metric := range family.Metric {
		seen[metric.Label[0].GetValue()] = metric.GetGauge().GetValue()
	}
	if seen["policy_version"] != -1 || seen["claim_clock_future"] != 1 || seen["attempt_clock_future"] != 2 {
		t.Fatal("unsupported policy or after-comparison-clock evidence was coerced to a provider zero")
	}
}

func TestUrlProbeMatureDeficitMetricsFailedRefreshRetainsAllCellsAndClock(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	previous := &providerUrlProbeFleetSnapshot{
		fleet: model.ProviderUrlProbeFleet{MatureDeficitDiagnostics: urlProbeMatureDeficitMetricFixture()}, observedAt: time.Unix(10, 0),
	}
	collector.snapshot.Store(previous)
	before := urlProbeFleetMetricValues(t, collector)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
		read func(context.Context, time.Time) model.ProviderUrlProbeFleet
	}{
		{"canceled", canceled, func(context.Context, time.Time) model.ProviderUrlProbeFleet { return model.ProviderUrlProbeFleet{} }},
		{"failed", t.Context(), func(context.Context, time.Time) model.ProviderUrlProbeFleet { panic("synthetic census failure") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := collector.refresh(test.ctx, time.Unix(20, 0), test.read); err == nil {
				t.Fatal("failed refresh was accepted")
			}
			if collector.snapshot.Load() != previous || !reflect.DeepEqual(before, urlProbeFleetMetricValues(t, collector)) {
				t.Fatal("failed refresh changed diagnostic counts, contract, or original clock")
			}
		})
	}
}

func TestUrlProbeMatureDeficitSnapshotContainsOnlyFixedNumericValues(t *testing.T) {
	var check func(reflect.Type)
	check = func(value reflect.Type) {
		switch value.Kind() {
		case reflect.Int, reflect.Float64:
		case reflect.Array:
			check(value.Elem())
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				check(value.Field(i).Type)
			}
		default:
			t.Fatalf("diagnostic snapshot gained a mutable or private payload type: %s", value)
		}
	}
	check(reflect.TypeFor[model.ProviderUrlProbeMatureDeficitDiagnostics]())
}
