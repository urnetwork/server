package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

func syntheticEgressOutcomeRow(snapshot egressOutcomeSnapshot) Row {
	values := []any{
		snapshot.eligible, snapshot.observed, snapshot.successes, snapshot.failures,
		snapshot.tunnelFailed, snapshot.contractFailed, snapshot.noConsensus,
		snapshot.locateFailed, snapshot.notConfident, snapshot.submitFailed,
		snapshot.unknownFailure, snapshot.inconsistent, snapshot.unobserved,
		snapshot.newestOutcomeAgeSeconds, snapshot.oldestOutcomeAgeSeconds,
	}
	row := make(Row, len(values))
	for index, value := range values {
		row[index] = fmt.Sprint(value)
	}
	return row
}

func runSyntheticEgressOutcomes(t *testing.T, snapshot egressOutcomeSnapshot) []Alert {
	t.Helper()
	alerts, err := NewEgressOutcomesSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "monitor-signal-2.23-egress-outcomes") {
				t.Fatal("egress outcome query marker is absent")
			}
			return []Row{syntheticEgressOutcomeRow(snapshot)}, nil
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func TestEgressOutcomesHealthyPopulation(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 100, observed: 100, successes: 95, failures: 5,
		noConsensus: 5, newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 3600,
	})
	if len(alerts) != 0 {
		t.Fatalf("healthy egress outcomes alerts = %+v", alerts)
	}
}

func TestEgressOutcomesStaysHealthyBelowTheExactFailureShare(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 100, observed: 100, successes: 11, failures: 89,
		noConsensus: 89, newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 3600,
	})
	if len(alerts) != 0 {
		t.Fatalf("89 percent failure share produced alerts = %+v", alerts)
	}
}

func TestEgressOutcomesPagesAtTheExactPopulationBoundary(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 20, observed: 20, successes: 2, failures: 18,
		noConsensus: 18, newestOutcomeAgeSeconds: 30, oldestOutcomeAgeSeconds: 600,
	})
	alert := requireAlertClass(t, alerts, "egress-common-mode")
	if alert.Severity != SeverityPage || alert.SignalNumber != "2.23" || alert.SignalKey != "egress-outcomes" {
		t.Fatalf("common-mode alert identity = %+v", alert)
	}
	rendered := alert.Markdown()
	for _, want := range []string{
		"18 of 20 currently eligible providers",
		"dominant_failure=no_consensus",
		"prober_identity",
		"six-hour failure backoff",
		"12-hour health due age",
		"Alert absence caused only by failures aging to unobserved is not recovery",
		"SIGNALS.md §2.23",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("common-mode alert missing %q:\n%s", want, rendered)
		}
	}
	for _, forbidden := range []string{
		"UR_PROBER_BY_JWT",
		"synthetic-private-token-marker",
		"synthetic-provider-id-marker",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("common-mode alert leaked obsolete/private value %q:\n%s", forbidden, rendered)
		}
	}
}

func TestEgressOutcomesKeepsUnobservedProvidersInTheDenominator(t *testing.T) {
	// Twenty observed failures among 22 eligible providers is over 90% and can
	// be attributed. Eighteen failures among 20 eligible providers is also 90%,
	// but 18 observations do not satisfy the independent evidence floor.
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 22, observed: 20, failures: 20, noConsensus: 20, unobserved: 2,
		newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 1800,
	})
	requireAlertClass(t, alerts, "egress-common-mode")

	alerts = runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 20, observed: 18, failures: 18, noConsensus: 18, unobserved: 2,
		newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 1800,
	})
	if len(alerts) != 0 {
		t.Fatalf("under-floor outcomes produced alerts = %+v", alerts)
	}

	alerts = runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 23, observed: 20, failures: 20, noConsensus: 20, unobserved: 3,
		newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 1800,
	})
	if len(alerts) != 0 {
		t.Fatalf("20 of 23 failures used the observed subset as denominator: %+v", alerts)
	}

	alerts = runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 100, observed: 5, failures: 5, noConsensus: 5, unobserved: 95,
		newestOutcomeAgeSeconds: 60, oldestOutcomeAgeSeconds: 300,
	})
	if len(alerts) != 0 {
		t.Fatalf("survivor-biased small failure sample produced alerts = %+v", alerts)
	}
}

func TestEgressOutcomesWarnsOnMixedFleetFailureWithoutCommonCauseClaim(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 20, observed: 20, successes: 2, failures: 18,
		noConsensus: 10, locateFailed: 8,
		newestOutcomeAgeSeconds: 20, oldestOutcomeAgeSeconds: 500,
	})
	alert := requireAlertClass(t, alerts, "egress-mixed-failure")
	if alert.Severity != SeverityWarn {
		t.Fatalf("mixed failure severity = %s, want warn", alert.Severity)
	}
	if strings.Contains(strings.ToLower(alert.Markdown()), "credential") {
		t.Fatalf("mixed failure made an unsupported credential claim:\n%s", alert.Markdown())
	}
	if got := requireAlertClassCount(alerts, "egress-common-mode"); got != 0 {
		t.Fatalf("mixed failures also emitted %d common-mode alerts", got)
	}
}

func TestEgressOutcomesUnknownClassesCannotManufactureDominance(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 20, observed: 20, failures: 20, unknownFailure: 20,
		newestOutcomeAgeSeconds: 10, oldestOutcomeAgeSeconds: 400,
	})
	if got := requireAlertClassCount(alerts, "egress-common-mode"); got != 0 {
		t.Fatalf("collapsed unknown failures emitted %d common-mode alerts", got)
	}
	requireAlertClass(t, alerts, "egress-mixed-failure")
	unknown := requireAlertClass(t, alerts, "egress-outcome-unknown")
	mixed := requireAlertClass(t, alerts, "egress-mixed-failure")
	if !strings.Contains(mixed.Markdown(), "do not establish one dominant known class") ||
		strings.Contains(mixed.Markdown(), "split across multiple") {
		t.Fatalf("unknown-only mixed alert claims unsupported class plurality:\n%s", mixed.Markdown())
	}
	for _, forbidden := range []string{"synthetic-raw-class-marker", "synthetic-provider-id-marker"} {
		requireAlertOmits(t, unknown, forbidden)
	}
}

func TestEgressOutcomesWarnsOnSuccessWithoutTrustedLocation(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 1, observed: 0, inconsistent: 1, unobserved: 0,
		newestOutcomeAgeSeconds: -1, oldestOutcomeAgeSeconds: -1,
	})
	alert := requireAlertClass(t, alerts, "egress-outcome-inconsistent")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "current successful attempt") {
		t.Fatalf("inconsistent outcome alert = %+v", alert)
	}
}

func TestEgressOutcomesNoObservedOutcomeUsesAbsentAges(t *testing.T) {
	alerts := runSyntheticEgressOutcomes(t, egressOutcomeSnapshot{
		eligible: 3, observed: 0, unobserved: 3,
		newestOutcomeAgeSeconds: -1, oldestOutcomeAgeSeconds: -1,
	})
	if len(alerts) != 0 {
		t.Fatalf("all-unobserved population produced alerts = %+v", alerts)
	}

	query := egressOutcomesQuery()
	if got := strings.Count(query, "CASE WHEN count(outcome_at) = 0 THEN -1::bigint"); got != 2 {
		t.Fatalf("egress outcome query has %d explicit absent-age branches, want 2", got)
	}
	if strings.Contains(query, "COALESCE(\n               GREATEST") {
		t.Fatal("egress outcome query revived PostgreSQL's GREATEST(0, NULL) = 0 bug")
	}
}

func TestEgressOutcomesRejectsMalformedOrContradictoryAggregate(t *testing.T) {
	valid := egressOutcomeSnapshot{
		eligible: 20, observed: 20, successes: 20,
		newestOutcomeAgeSeconds: 10, oldestOutcomeAgeSeconds: 20,
	}
	tests := []struct {
		name string
		rows []Row
	}{
		{name: "missing row", rows: nil},
		{name: "extra row", rows: []Row{syntheticEgressOutcomeRow(valid), syntheticEgressOutcomeRow(valid)}},
		{name: "short row", rows: []Row{{"1"}}},
		{name: "negative count", rows: []Row{func() Row {
			row := syntheticEgressOutcomeRow(valid)
			row[0] = "-1"
			return row
		}()}},
		{name: "failure buckets disagree", rows: []Row{func() Row {
			row := syntheticEgressOutcomeRow(valid)
			row[3] = "1"
			return row
		}()}},
		{name: "population disagrees", rows: []Row{func() Row {
			row := syntheticEgressOutcomeRow(valid)
			row[0] = "21"
			return row
		}()}},
		{name: "ages without outcomes", rows: []Row{syntheticEgressOutcomeRow(egressOutcomeSnapshot{
			eligible: 1, inconsistent: 1, newestOutcomeAgeSeconds: 1, oldestOutcomeAgeSeconds: 1,
		})}},
		{name: "inverted ages", rows: []Row{func() Row {
			row := syntheticEgressOutcomeRow(valid)
			row[13], row[14] = "30", "20"
			return row
		}()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewEgressOutcomesSignal().Run(context.Background(), syntheticSettings(&syntheticSource{
				postgresFn: func(string) ([]Row, error) { return test.rows, nil },
			}))
			if err == nil {
				t.Fatal("invalid aggregate error = nil")
			}
		})
	}
}

func TestEgressOutcomesQueryPinsPopulationFreshnessAndPrivacy(t *testing.T) {
	query := egressOutcomesQuery()
	for _, want := range []string{
		"nc.active",
		"nc.source_client_id IS NULL",
		"nclr.connected",
		"nclr.valid",
		"EXISTS (",
		"pk.provide_mode = 3",
		fmt.Sprintf("interval '%d seconds'", int64(model.ProviderEgressProbeAttemptBackoff/time.Second)),
		fmt.Sprintf("interval '%d seconds'", int64(model.ProviderEgressLocationMaxAge/time.Second)),
		"attempt_update > location_update",
		"ELSE 'unknown_failure'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("egress outcome query missing %q", want)
		}
	}
	if strings.Contains(query, "location_update > attempt_update") {
		t.Fatal("overloaded location update time was allowed to prove a successful recovery")
	}
	finalIndex := strings.LastIndex(query, "SELECT eligible::text")
	if finalIndex < 0 {
		t.Fatal("egress outcome query has no fixed final aggregate")
	}
	for _, forbidden := range []string{"client_id", "probe_failure", "observed_at", "update_time"} {
		if strings.Contains(query[finalIndex:], forbidden) {
			t.Fatalf("final egress outcome projection leaks %q", forbidden)
		}
	}
}

func requireAlertClassCount(alerts []Alert, class string) int {
	count := 0
	for _, alert := range alerts {
		if alert.Class == class {
			count++
		}
	}
	return count
}
