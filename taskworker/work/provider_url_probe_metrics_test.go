// Fleet scrapes never mix generations or manufacture freshness after failure.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server/model"
)

func TestUrlProbeFleetScrapeUsesOneAtomicGeneration(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	metrics := make(chan prometheus.Metric)
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{
		fleet: model.ProviderUrlProbeFleet{Eligible: 1, Due: 1, Complete: 1, QuotaComplete: 1,
			Overdue: 1, RunsNeeded: 1, SecurityExceptions: 1, SecurityUnknownTargets: 1, OldestDueSeconds: 1,
			Warming: 1, MissingCycles: 1, CohortStartedAtSeconds: 1,
			MatureEligible: 1, MatureQuotaComplete: 1, MatureRunsNeeded: 1,
			WarmingEligible: 1, WarmingQuotaComplete: 1, WarmingRunsNeeded: 1,
			EligibilityAgeUnknown: 1, AgeUnknownQuotaComplete: 1, AgeUnknownRunsNeeded: 1},
		observedAt: time.Unix(1, 0),
	})
	go func() {
		collector.Collect(metrics)
		close(metrics)
	}()
	first := <-metrics
	// Publication happens while Collect is blocked partway through this scrape.
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{fleet: model.ProviderUrlProbeFleet{Eligible: 99}, observedAt: time.Unix(99, 0)})
	assertOld := func(metric prometheus.Metric) {
		var value dto.Metric
		if err := metric.Write(&value); err != nil {
			t.Fatal(err)
		}
		if value.GetGauge().GetValue() != 1 {
			t.Fatalf("scrape mixed fleet generations: %s", value.String())
		}
	}
	assertOld(first)
	count := 1
	for metric := range metrics {
		assertOld(metric)
		count++
	}
	if count != 25 {
		t.Fatalf("incomplete fleet scrape: got %d series, want 25", count)
	}
}

func TestUrlProbeFleetRefreshHasDeadlineAndRetainsFailedSnapshot(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	if metrics, err := registry.Gather(); err != nil || len(metrics) != 0 {
		t.Fatalf("missing census became zero-valued evidence: metrics=%v err=%v", metrics, err)
	}
	observedAt := time.Unix(1, 0)
	if err := collector.refresh(t.Context(), observedAt, func(ctx context.Context, comparisonAt time.Time) model.ProviderUrlProbeFleet {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second || !comparisonAt.Equal(observedAt) {
			t.Fatal("census lost its bounded deadline or comparison clock")
		}
		return model.ProviderUrlProbeFleet{Eligible: 1}
	}); err != nil {
		t.Fatal(err)
	}
	previous := collector.snapshot.Load()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := collector.refresh(canceled, time.Unix(2, 0), func(context.Context, time.Time) model.ProviderUrlProbeFleet {
		return model.ProviderUrlProbeFleet{Eligible: 99}
	}); err == nil {
		t.Fatal("canceled census was published as a fresh complete snapshot")
	}
	if collector.snapshot.Load() != previous {
		t.Fatal("failed census advanced evidence freshness")
	}
}

func TestUrlProbeAdmissionCohortMetricsPreserveOldContractAndUnknownAge(t *testing.T) {
	collector := newProviderUrlProbeFleetCollector()
	collector.snapshot.Store(&providerUrlProbeFleetSnapshot{
		observedAt: time.Unix(100, 0),
		fleet: model.ProviderUrlProbeFleet{
			Eligible: 15, QuotaComplete: 9, RunsNeeded: 32,
			MatureEligible: 10, MatureQuotaComplete: 8, MatureRunsNeeded: 12,
			WarmingEligible: 2, WarmingQuotaComplete: 1, WarmingRunsNeeded: 3,
			EligibilityAgeUnknown: 3, AgeUnknownQuotaComplete: 0, AgeUnknownRunsNeeded: 17,
		},
	})
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{
		"mature:eligible": 10, "mature:quota_complete": 8, "mature:runs_needed": 12,
		"warming:eligible": 2, "warming:quota_complete": 1, "warming:runs_needed": 3,
		"age_unknown:eligible": 3, "age_unknown:quota_complete": 0, "age_unknown:runs_needed": 17,
	}
	oldStates, contract := 0, false
	for _, family := range families {
		switch family.GetName() {
		case "urnetwork_url_probe_fleet":
			oldStates = len(family.Metric)
		case "urnetwork_url_probe_admission_cohort_contract":
			contract = len(family.Metric) == 1 && family.Metric[0].GetGauge().GetValue() == 1
		case "urnetwork_url_probe_admission_cohort":
			for _, metric := range family.Metric {
				labels := map[string]string{}
				for _, label := range metric.Label {
					labels[label.GetName()] = label.GetValue()
				}
				key := labels["cohort"] + ":" + labels["state"]
				value, present := want[key]
				if !present || len(labels) != 2 || metric.GetGauge().GetValue() != value {
					t.Fatalf("incorrect cohort projection: %s", metric.String())
				}
				delete(want, key)
			}
		}
	}
	if oldStates != 12 || !contract || len(want) != 0 {
		t.Fatalf("incomplete or incompatible contracts: old states=%d contract=%t missing=%v", oldStates, contract, want)
	}
}

// A capable idle process exports real zero-valued outcome series. Absence must
// remain distinguishable from a measured zero success rate in hourly windows.
func TestUrlProbeMetricsExposeFiniteZeroCounterChildren(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(urlProbeTurns, urlProbeOutcomes, urlProbeSources, urlProbeCapability, urlProbeConfiguredShards)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]bool{
		"urnetwork_url_probe_turns_total":    {"attempted": false, "accepted": false, "local_failure": false},
		"urnetwork_url_probe_outcomes_total": {"success": false, "error": false},
		"urnetwork_url_probe_sources_total":  {"general": false, "country": false, "general_country_unavailable": false, "security_recheck": false},
	}
	capable, configured := false, false
	for _, family := range families {
		if family.GetName() == "urnetwork_url_probe_capability" {
			capable = len(family.Metric) == 1 && family.Metric[0].GetGauge().GetValue() == 2
		}
		if family.GetName() == "urnetwork_url_probe_configured_shards" {
			configured = len(family.Metric) == 1
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if expected := want[family.GetName()]; expected != nil {
					if _, exists := expected[label.GetValue()]; exists {
						expected[label.GetValue()] = true
					}
				}
			}
		}
	}
	if !capable || !configured {
		t.Fatal("idle-capable process is indistinguishable from unsupported instrumentation")
	}
	for name, children := range want {
		for child, found := range children {
			if !found {
				t.Errorf("missing explicit zero counter child %s/%s", name, child)
			}
		}
	}
}
