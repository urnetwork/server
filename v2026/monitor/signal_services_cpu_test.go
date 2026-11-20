package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

var stormServicesEdges = []string{"by-us-fmt-5-edge-0", "by-us-fmt-5-edge-1", "by-us-fmt-5-edge-3", "by-us-fmt-5-edge-4"}

// Incident A replayed from recorded Mimir history. The four edges together
// pass 80% at 19:42Z on 10-25 (79.8% at 19:41Z), so their tenth evaluation
// above it is 19:51Z, and not a minute earlier. edge-3 alone passes 85% at
// 19:45Z and warns on its thirtieth evaluation, 20:14Z. The healthy
// reference of 10-20, with edge-3 at 76-80%, raises nothing.
func TestServicesCpuReplaysIncidentA(t *testing.T) {
	cases := loadStormReplayFixture(t, "services-cpu")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-a-tier", targets: []string{"main/services-tier"}},
		{name: "incident-a-before-tier"},
		{name: "incident-a-edge-3", targets: []string{"by-us-fmt-5-edge-3", "main/services-tier"}},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewServicesCpuSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["services-cpu-busy"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
		for _, alert := range alerts {
			if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.SignalNumber != "26.2" {
				t.Fatalf("%s: %+v", test.name, alert)
			}
		}
	}
	tier := stormAlertsByTarget(runStormReplay(t, NewServicesCpuSignal(), cases["incident-a-tier"], nil), "services-cpu-busy")["main/services-tier"]
	if !strings.Contains(tier.Observed, "busy_share_now=0.832") || !strings.Contains(tier.Observed, "by-us-fmt-5-edge-3:0.854") {
		t.Fatalf("tier observed %q", tier.Observed)
	}
}

// A synthetic answer: the edges together and each edge, one value per step.
func servicesCpuResponse(t *testing.T, tier []float64, hosts map[string][]float64) string {
	series := []stormSyntheticSeries{{part: "tier_busy", values: tier}}
	for _, name := range stormServicesEdges {
		if values, present := hosts[name]; present {
			series = append(series, stormSyntheticSeries{part: "host_busy", labels: map[string]string{"host": name}, values: values})
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(30), time.Minute, series)
}

// The edges come from placement; the edges together count only where every
// edge answered; one hot edge needs all thirty minutes.
func TestServicesCpuPlacementCoverageAndHolds(t *testing.T) {
	probe := servicesCpuProbe{}
	everyEdge := func(value float64) map[string][]float64 {
		hosts := map[string][]float64{}
		for _, name := range stormServicesEdges {
			hosts[name] = stormRepeat(value, 30)
		}
		return hosts
	}
	tierIdentity, edge3 := "services-cpu-busy main/services-tier", "services-cpu-busy by-us-fmt-5-edge-3"
	visibility := "services-cpu-unobservable main/services-cpu"

	hotEdge := everyEdge(0.6)
	hotEdge["by-us-fmt-5-edge-3"] = stormRepeat(0.9, 30)
	shortHot := everyEdge(0.6)
	shortHot["by-us-fmt-5-edge-3"] = append(stormRepeat(0.6, 1), stormRepeat(0.9, 29)...)
	missingEdge := everyEdge(0.9)
	delete(missingEdge, "by-us-fmt-5-edge-1")
	gapEdge := everyEdge(0.5)
	gapEdge["by-us-fmt-5-edge-4"][29] = math.NaN()
	for _, test := range []struct {
		name   string
		tier   []float64
		hosts  map[string][]float64
		mutate func(*SignalSettings)
		want   map[string]string
	}{
		{name: "healthy", tier: stormRepeat(0.5, 30), hosts: everyEdge(0.5), want: map[string]string{tierIdentity: "healthy", edge3: "healthy", visibility: "healthy"}},
		{name: "tier at its level", tier: stormRepeat(0.8, 30), hosts: everyEdge(0.8), want: map[string]string{tierIdentity: "healthy"}},
		{name: "tier above", tier: stormRepeat(0.81, 30), hosts: everyEdge(0.81), want: map[string]string{tierIdentity: "warn", edge3: "healthy"}},
		{name: "one hot edge", tier: stormRepeat(0.7, 30), hosts: hotEdge, want: map[string]string{tierIdentity: "healthy", edge3: "warn"}},
		{name: "one edge hot for 29 minutes", tier: stormRepeat(0.7, 30), hosts: shortHot, want: map[string]string{edge3: "hold"}},
		{name: "a missing edge leaves the tier unknown", tier: stormRepeat(0.9, 30), hosts: missingEdge, want: map[string]string{tierIdentity: "hold", edge3: "warn", visibility: "unobservable"}},
		{name: "an edge gap at the last evaluation", tier: stormRepeat(0.5, 30), hosts: gapEdge, want: map[string]string{tierIdentity: "hold", "services-cpu-busy by-us-fmt-5-edge-4": "hold", visibility: "unobservable"}},
		{name: "an excluded edge leaves the tier unknown", tier: stormRepeat(0.5, 30), hosts: everyEdge(0.5),
			mutate: func(settings *SignalSettings) { settings.ExcludedHosts = []string{"by-us-fmt-5-edge-1"} },
			want:   map[string]string{tierIdentity: "hold", edge3: "healthy", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, servicesCpuResponse(t, test.tier, test.hosts), test.mutate)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestServicesCpuQueryIsTheCatalogExpressionOverPlacedEdges(t *testing.T) {
	settings := DefaultServicesCpuSettings()
	if settings.TierBusy != 0.80 || settings.TierHold != 10*time.Minute || settings.HostBusy != 0.85 || settings.HostHold != 30*time.Minute || settings.Window != 5*time.Minute {
		t.Fatalf("§26.2 policy changed: %+v", settings)
	}
	query := servicesCpuQuery("main", stormServicesEdges, settings)
	hosts := `host=~"by-us-fmt-5-edge-0|by-us-fmt-5-edge-1|by-us-fmt-5-edge-3|by-us-fmt-5-edge-4"`
	for _, want := range []string{
		`1 - sum(rate(node_cpu_seconds_total{env="main",` + hosts + `,mode=~"idle|iowait"}[5m])) / sum(rate(node_cpu_seconds_total{env="main",` + hosts + `}[5m]))`,
		`1 - sum by (host) (rate(node_cpu_seconds_total{env="main",` + hosts + `,mode=~"idle|iowait"}[5m])) / sum by (host) (rate(node_cpu_seconds_total{env="main",` + hosts + `}[5m]))`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost the catalog expression %s\n%s", want, query)
		}
	}
}
