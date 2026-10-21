// Synthetic public-adapter frames exercise exact aggregate bounds independently
// from child parsing, process identity, and the admission incident flags.
package monitor

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Keep every child value individually valid while varying the fleet sum.
func mimirAdmissionAggregateResponses(counters [][4]int64) map[string]mimirAdmissionSyntheticResponse {
	instances := make([]string, 0, len(counters))
	for i, values := range counters {
		instance := mimirAdmissionCounterInstanceFixture(1900000000, values[0], values[1], values[2], values[3])
		instance = strings.Replace(instance, fmt.Sprintf("instance_begin %d\n", mimirAdmissionSyntheticPort),
			fmt.Sprintf("instance_begin %d\n", mimirAdmissionSyntheticPort+i), 1)
		instances = append(instances, instance)
	}
	return map[string]mimirAdmissionSyntheticResponse{
		"metrics-a.example": {output: mimirAdmissionHostFixture(instances, true, 0, 0, 0)},
	}
}

// Both exact reasons remain affirmative even when neither total fits int64.
func requireMimirAdmissionAggregatePages(t *testing.T, alerts []Alert, complete bool) {
	t.Helper()
	for _, class := range []string{"mimir-series-limit", "mimir-ingestion-rate-limit"} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Severity != SeverityPage {
			t.Fatalf("direct positive lost PAGE: %+v", alert)
		}
		text := alert.Markdown()
		if !strings.Contains(text, fmt.Sprintf("direct_complete=%t", complete)) {
			t.Fatalf("wrong aggregate completeness: %s", text)
		}
		if strings.Contains(text, "-8446744073709551616") || strings.Contains(text, "%!") {
			t.Fatalf("wrapped or malformed aggregate: %s", text)
		}
	}
}

// A first observation must retain positives and explicitly refuse a wrapped sum.
func TestMimirAdmissionAggregateInitialOverflow(t *testing.T) {
	now := time.Date(2035, 1, 2, 3, 4, 0, 0, time.UTC)
	responses := mimirAdmissionAggregateResponses([][4]int64{{5e18, 5e18, 0, 0}, {5e18, 5e18, 0, 0}})
	alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now, responses)
	series := requireAlertClass(t, alerts, "mimir-series-limit")
	if !strings.Contains(series.Markdown(), "discard_counter_increase=unavailable") {
		t.Fatalf("unrepresentable initial series total rendered as exact: %s", series.Markdown())
	}
	rate := requireAlertClass(t, alerts, "mimir-ingestion-rate-limit")
	if !strings.Contains(rate.Markdown(), "rate_discard_counter_increase=unavailable") {
		t.Fatalf("unrepresentable initial rate total rendered as exact: %s", rate.Markdown())
	}
	requireMimirAdmissionAggregatePages(t, alerts, false)
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Target != "mimir-admission/aggregate" {
		t.Fatalf("overflow lost fixed visibility identity: %+v", visibility)
	}
}

// Same-generation positive deltas and created/removed churn share the bound.
func TestMimirAdmissionAggregateComparableOverflow(t *testing.T) {
	now := time.Date(2035, 1, 2, 4, 4, 0, 0, time.UTC)
	signal := NewMimirAdmissionSignal()
	zero := mimirAdmissionAggregateResponses([][4]int64{{}, {}})
	if alerts := runMimirAdmissionSynthetic(t, signal, now, zero); len(alerts) != 0 {
		t.Fatalf("healthy zero setup alerted: %+v", alerts)
	}
	large := mimirAdmissionAggregateResponses([][4]int64{{5e18, 5e18, 5e18, 5e18}, {5e18, 5e18, 5e18, 5e18}})
	alerts := runMimirAdmissionSynthetic(t, signal, now.Add(time.Minute), large)
	text := requireAlertClass(t, alerts, "mimir-series-limit").Markdown()
	for _, field := range []string{"discard_counter_increase", "created_increase", "removed_increase"} {
		if !strings.Contains(text, field+"=unavailable") {
			t.Fatalf("unrepresentable comparable %s rendered as exact: %s", field, text)
		}
	}
	if !strings.Contains(requireAlertClass(t, alerts, "mimir-ingestion-rate-limit").Markdown(), "rate_discard_counter_increase=unavailable") {
		t.Fatal("unrepresentable comparable rate total rendered as exact")
	}
	requireMimirAdmissionAggregatePages(t, alerts, false)
}

// Equality with MaxInt64 stays exact; the following flat tick has zero deltas.
func TestMimirAdmissionAggregateExactBoundary(t *testing.T) {
	now := time.Date(2035, 1, 2, 5, 4, 0, 0, time.UTC)
	signal := NewMimirAdmissionSignal()
	runMimirAdmissionSynthetic(t, signal, now, mimirAdmissionAggregateResponses([][4]int64{{}, {}}))
	maximum := mimirAdmissionAggregateResponses([][4]int64{{math.MaxInt64 - 7, math.MaxInt64 - 7, math.MaxInt64 - 7, math.MaxInt64 - 7}, {7, 7, 7, 7}})
	alerts := runMimirAdmissionSynthetic(t, signal, now.Add(time.Minute), maximum)
	requireMimirAdmissionAggregatePages(t, alerts, true)
	text := requireAlertClass(t, alerts, "mimir-series-limit").Markdown()
	for _, field := range []string{"discard_counter_increase", "created_increase", "removed_increase"} {
		if !strings.Contains(text, field+"=9223372036854775807") {
			t.Fatalf("representable boundary lost exact %s: %s", field, text)
		}
	}
	if !strings.Contains(requireAlertClass(t, alerts, "mimir-ingestion-rate-limit").Markdown(), "rate_discard_counter_increase=9223372036854775807") {
		t.Fatal("representable rate boundary lost exact total")
	}
	flat := runMimirAdmissionSynthetic(t, signal, now.Add(2*time.Minute), maximum)
	if !strings.Contains(requireAlertClass(t, flat, "mimir-series-limit").Markdown(), "discard_counter_increase=0") {
		t.Fatal("exact child baselines did not retain a zero delta")
	}
	if alerts := runMimirAdmissionSynthetic(t, signal, now.Add(2*time.Minute+mimirAdmissionQuietWindow), maximum); len(alerts) != 0 {
		t.Fatalf("complete quiet healthy control did not recover: %+v", alerts)
	}
}

// An additional child cannot turn an already unavailable field into a number.
func TestMimirAdmissionAggregateOverflowRemainsUnavailable(t *testing.T) {
	now := time.Date(2035, 1, 2, 6, 4, 0, 0, time.UTC)
	responses := mimirAdmissionAggregateResponses([][4]int64{{math.MaxInt64, 1, 0, 0}, {1, 2, 0, 0}, {1, 3, 0, 0}})
	alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now, responses)
	text := requireAlertClass(t, alerts, "mimir-series-limit").Markdown()
	if !strings.Contains(text, "discard_counter_increase=unavailable") {
		t.Fatalf("later child replaced unknown total: %s", text)
	}
	if !strings.Contains(requireAlertClass(t, alerts, "mimir-ingestion-rate-limit").Markdown(), "rate_discard_counter_increase=6") {
		t.Fatal("one overflow discarded an independent exact total")
	}
}

// Churn-only overflow interrupts an otherwise mature discard quiet window.
func TestMimirAdmissionAggregateOverflowRestartsQuietRecovery(t *testing.T) {
	now := time.Date(2035, 1, 2, 7, 4, 0, 0, time.UTC)
	signal := NewMimirAdmissionSignal()
	positive := mimirAdmissionAggregateResponses([][4]int64{{1, 1, 0, 0}, {1, 1, 0, 0}})
	runMimirAdmissionSynthetic(t, signal, now, positive)
	runMimirAdmissionSynthetic(t, signal, now.Add(time.Minute), positive)
	large := mimirAdmissionAggregateResponses([][4]int64{{1, 1, 5e18, 5e18}, {1, 1, 5e18, 5e18}})
	overflowAt := now.Add(time.Minute + mimirAdmissionQuietWindow)
	alerts := runMimirAdmissionSynthetic(t, signal, overflowAt, large)
	requireMimirAdmissionAggregatePages(t, alerts, false)
	requireAlertClass(t, alerts, "cannot-observe")
	quietAt := overflowAt.Add(time.Minute)
	flat := runMimirAdmissionSynthetic(t, signal, quietAt, large)
	requireMimirAdmissionAggregatePages(t, flat, true)
	if !strings.Contains(requireAlertClass(t, flat, "mimir-series-limit").Markdown(), "created_increase=0 removed_increase=0") {
		t.Fatal("overflow damaged exact child churn baselines")
	}
	requireMimirAdmissionAggregatePages(t, runMimirAdmissionSynthetic(t, signal, quietAt.Add(mimirAdmissionQuietWindow-time.Second), large), true)
	if alerts := runMimirAdmissionSynthetic(t, signal, quietAt.Add(mimirAdmissionQuietWindow), large); len(alerts) != 0 {
		t.Fatalf("new complete quiet window did not recover: %+v", alerts)
	}
}

// A source failure remains private beside valid overflowed sibling positives.
func TestMimirAdmissionAggregatePartialSourcePrivacy(t *testing.T) {
	now := time.Date(2035, 1, 2, 8, 4, 0, 0, time.UTC)
	responses := mimirAdmissionAggregateResponses([][4]int64{{5e18, 5e18, 0, 0}, {5e18, 5e18, 0, 0}})
	responses["metrics-b.example"] = mimirAdmissionSyntheticResponse{err: fmt.Errorf("PRIVATE_SYNTHETIC_SOURCE_SENTINEL")}
	alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now, responses)
	requireMimirAdmissionAggregatePages(t, alerts, false)
	for _, alert := range alerts {
		if strings.Contains(alert.Markdown(), "PRIVATE_SYNTHETIC_SOURCE_SENTINEL") {
			t.Fatal("source text entered a public alert")
		}
	}
}

// Optional journal totals can be unknown without taking away direct authority.
func TestMimirAdmissionAggregateJournalOverflowIsContextOnly(t *testing.T) {
	now := time.Date(2035, 1, 2, 9, 4, 0, 0, time.UTC)
	signal := NewMimirAdmissionSignal()
	response := mimirAdmissionSyntheticResponse{output: mimirAdmissionHostFixture([]string{mimirAdmissionCounterInstanceFixture(1900000000, 1, 1, 0, 0)}, true, 5e18, 5e18, 5e18)}
	responses := map[string]mimirAdmissionSyntheticResponse{"metrics-a.example": response, "metrics-b.example": response}
	alerts := runMimirAdmissionSynthetic(t, signal, now, responses)
	requireMimirAdmissionAggregatePages(t, alerts, true)
	text := requireAlertClass(t, alerts, "mimir-series-limit").Markdown()
	for _, field := range []string{"publisher_starts", "readiness_rejects", "admission_rejects"} {
		if !strings.Contains(text, field+"=unavailable") {
			t.Fatalf("journal overflow rendered as exact %s: %s", field, text)
		}
	}
	for _, alert := range alerts {
		if alert.Class == "cannot-observe" {
			t.Fatalf("optional journal overflow revoked direct authority: %+v", alert)
		}
	}
	runMimirAdmissionSynthetic(t, signal, now.Add(time.Minute), responses)
	if alerts := runMimirAdmissionSynthetic(t, signal, now.Add(time.Minute+mimirAdmissionQuietWindow), responses); len(alerts) != 0 {
		t.Fatalf("optional context blocked complete direct quiet recovery: %+v", alerts)
	}
}

// An ended public call cannot consume otherwise valid large synthetic frames.
func TestMimirAdmissionAggregateScopedEndedParent(t *testing.T) {
	var calls atomic.Int64
	settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls.Add(1)
		return "PRIVATE_UNEXPECTED_SOURCE_ENTRY", nil
	}})
	settings.Hosts = []HostSettings{{Name: "metrics-a.example", Roles: []string{"services"}}, {Name: "excluded.example", Roles: []string{"services"}}}
	settings.ExcludedHosts = []string{"excluded.example"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewMimirAdmissionSignal().Run(ctx, settings)
	if err != context.Canceled || len(alerts) != 0 || calls.Load() != 0 {
		t.Fatalf("ended public call entered source or lost cancellation: calls=%d alerts=%d err=%v", calls.Load(), len(alerts), err)
	}
}
