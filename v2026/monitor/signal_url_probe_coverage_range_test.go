package monitor

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Range aggregations have the current query timestamp even after every instant
// series for a retired instance disappears. They are not live-owner evidence.
func TestPrivateUrlRangeOnlyPredecessorDoesNotEraseCurrentOwner(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	retired := &urlProbeCoverageProcess{
		host: first.host, block: first.block, instance: "synthetic-retired-instance",
		values: map[string]float64{"success": 1000000, "success_samples": 80, "success_early": 10},
	}
	payload := urlProbeCoverageFixtureJson(t, now, first, second, retired)
	processes, err := parseUrlProbeCoverage(payload, "synthetic", now)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{first.host + "\x00" + first.block: true, second.host + "\x00" + second.block: true}
	current := currentUrlProbeProcesses(processes, expected, now)
	if len(current) != 2 || current[first.host+"\x00"+first.block].instance != first.instance {
		t.Fatalf("range-only retired history erased the current owner: current=%d want=2", len(current))
	}
	if alerts := runUrlProbeCoverageFixture(t, now, payload); len(alerts) != 0 {
		t.Fatalf("range-only predecessor changed a fully qualified current verdict: %+v", alerts)
	}
	first.values["success"] = 1
	second.values["success"] = 1
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second, retired))
	requireAlertClass(t, alerts, "url-probe-throughput-deficit")
	if len(alerts) != 1 {
		t.Fatalf("retired success range filled or invalidated the current-process rate: %+v", alerts)
	}
}

// A single instant family without a trustworthy start still makes the slot
// ambiguous. Historical filtering must never hide a genuinely unbound owner.
func TestPrivateUrlRangeOnlyDoesNotForgiveUnknownLiveInstance(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, field := range []string{
		"start", "start_time", "configured", "configured_time", "capability", "capability_time",
		"heartbeat:0", "heartbeat_time:0", "observed", "observed_time", "fleet:eligible", "fleet_time:eligible",
		"oldest", "oldest_time", "cohort_started", "cohort_started_time", "success_time",
	} {
		first := urlProbeCoverageFixture(now, "worker-a.example", 0)
		second := urlProbeCoverageFixture(now, "worker-b.example", 1)
		unknown := &urlProbeCoverageProcess{
			host: first.host, block: first.block, instance: "synthetic-unknown-instance",
			values: map[string]float64{"success": 1000000, "success_samples": 80, "success_early": 10, field: float64(now.Unix())},
		}
		alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second, unknown))
		requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
		if len(alerts) != 1 {
			t.Fatalf("unknown instant family %s did not remain unobservable: %+v", field, alerts)
		}
	}
}

func TestPrivateUrlRangeOnlyMalformedHistoryRemainsUnknown(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	first := urlProbeCoverageFixture(now, "worker-a.example", 0)
	second := urlProbeCoverageFixture(now, "worker-b.example", 1)
	invalid := &urlProbeCoverageProcess{
		host: first.host, block: first.block, instance: "synthetic-invalid-instance",
		values: map[string]float64{"success": -1, "success_samples": 80, "success_early": 10},
	}
	alerts := runUrlProbeCoverageFixture(t, now, urlProbeCoverageFixtureJson(t, now, first, second, invalid))
	requireAlertClass(t, alerts, "url-probe-coverage-unobservable")
	if len(alerts) != 1 {
		t.Fatalf("malformed historical observation became healthy: %+v", alerts)
	}
}

// The enabled runtime and desired inventory are separate. Ignoring old ranges
// restores the eight current owners, not the two excluded desired placements.
func TestPrivateUrlRangeOnlyPreservesDesiredSlotGaps(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	expected := map[string]bool{}
	processes := []*urlProbeCoverageProcess{}
	shard := 0
	for hostIndex := 0; hostIndex < 5; hostIndex++ {
		host := fmt.Sprintf("worker-%d.example", hostIndex)
		for _, block := range []string{"worker-group", "worker-other"} {
			expected[host+"\x00"+block] = true
			if hostIndex == 4 {
				continue
			}
			current := urlProbeCoverageFixture(now, host, shard)
			current.block = block
			current.values["configured"] = 8
			processes = append(processes, current, &urlProbeCoverageProcess{
				host: host, block: block, instance: "synthetic-retired-instance",
				values: map[string]float64{"success": 1000000, "success_samples": 80, "success_early": 10},
			})
			shard++
		}
	}
	parsed, err := parseUrlProbeCoverage(urlProbeCoverageFixtureJson(t, now, processes...), "synthetic", now)
	if err != nil {
		t.Fatal(err)
	}
	if current := currentUrlProbeProcesses(parsed, expected, now); len(current) != 8 {
		t.Fatalf("current owner count=%d want=8, desired=%d", len(current), len(expected))
	}
	findings := evaluateUrlProbeCoverage(parsed, expected, 8, now)
	if len(findings) != 1 || findings[0].class != "url-probe-coverage-unobservable" ||
		!strings.Contains(findings[0].observed, "hourly_measured_run_ranges_or_expected_process_coverage_incomplete") ||
		strings.Contains(findings[0].observed, "owners=") || strings.Contains(findings[0].observed, "census_unavailable") {
		t.Fatalf("range filtering erased desired gaps or valid owners/census: %+v", findings)
	}
}
