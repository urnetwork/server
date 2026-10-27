package monitor

import (
	"strings"
	"testing"
	"time"
)

// Incident B replayed from recorded Mimir history: the largest proxy
// process passes 1,000 creations in stage other at 14:47Z on 10-26, so the
// page lands at 14:51Z, eight minutes after onset, and not a minute earlier.
// The healthy reference of 10-26 08:00-12:00Z raises nothing, with all 94
// placed slots answering.
func TestContractCreationsStuckReplaysIncidentB(t *testing.T) {
	cases := loadStormReplayFixture(t, "contract-creations-stuck")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-b", targets: []string{"main/proxy"}},
		{name: "incident-b-before"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewContractCreationsStuckSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["contract-creations-stuck"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
	}
	alert := requireAlertClass(t, runStormReplay(t, NewContractCreationsStuckSignal(), cases["incident-b"], nil), "contract-creations-stuck")
	if alert.Severity != SeverityPage || alert.SignalNumber != "26.4" || !strings.Contains(alert.Observed, "largest_process_now=3521") ||
		!strings.Contains(alert.Observed, "service_total_now=22675") || !strings.Contains(alert.Observed, "slots_answering=20/20") {
		t.Fatalf("%+v", alert)
	}
}

// A synthetic answer: per service, its largest process and whether its
// placed slots answered.
func contractCreationsStuckResponse(t *testing.T, stuck map[string][]float64, silentSlots map[string]bool) string {
	series := []stormSyntheticSeries{}
	for service, values := range stuck {
		series = append(series,
			stormSyntheticSeries{part: "stuck", labels: map[string]string{"service": service}, values: values},
			stormSyntheticSeries{part: "total", labels: map[string]string{"service": service}, values: values},
		)
	}
	for service, hosts := range stormMainServiceHosts {
		for _, host := range hosts {
			for _, block := range stormMainServiceBlocks[service] {
				if silentSlots[service+"/"+host+"/"+block] {
					continue
				}
				series = append(series, stormSyntheticSeries{part: "slots", labels: map[string]string{"service": service, "host": host, "block": block}, values: stormRepeat(2, 5)})
			}
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(5), time.Minute, series)
}

// The page is strict at 1,000 and needs all five minutes; a maximum over
// fewer processes still proves it, but a silent slot never clears.
func TestContractCreationsStuckBandsAndSlotCoverage(t *testing.T) {
	probe := contractCreationsStuckProbe{}
	quiet := func() map[string][]float64 {
		stuck := map[string][]float64{}
		for service := range stormMainServiceHosts {
			stuck[service] = stormRepeat(3, 5)
		}
		return stuck
	}
	proxy, api := "contract-creations-stuck main/proxy", "contract-creations-stuck main/api"
	visibility := "contract-creations-stuck-unobservable main/contract-creations-stuck"

	atLevel, above, brief := quiet(), quiet(), quiet()
	atLevel["proxy"] = stormRepeat(1000, 5)
	above["proxy"] = stormRepeat(1001, 5)
	brief["proxy"] = []float64{900, 1500, 1500, 1500, 1500}
	silent := map[string]bool{"proxy/crisp/g7": true}
	for _, test := range []struct {
		name   string
		stuck  map[string][]float64
		silent map[string]bool
		want   map[string]string
	}{
		{name: "healthy", stuck: quiet(), want: map[string]string{proxy: "healthy", api: "healthy", visibility: "healthy"}},
		{name: "at the level", stuck: atLevel, want: map[string]string{proxy: "healthy"}},
		{name: "above the level", stuck: above, want: map[string]string{proxy: "page", api: "healthy"}},
		{name: "four minutes", stuck: brief, want: map[string]string{proxy: "hold"}},
		{name: "a silent slot still proves a page", stuck: above, silent: silent, want: map[string]string{proxy: "page", visibility: "unobservable"}},
		{name: "a silent slot never clears", stuck: quiet(), silent: silent, want: map[string]string{proxy: "hold", api: "healthy", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, contractCreationsStuckResponse(t, test.stuck, test.silent), nil)
		requireStormStates(t, test.name, states, test.want)
	}
	gaps := runStormSynthetic(t, NewContractCreationsStuckSignal(), stormSyntheticNow, contractCreationsStuckResponse(t, quiet(), silent), nil)
	if alert := requireAlertClass(t, gaps, "contract-creations-stuck-unobservable"); !strings.Contains(alert.Observed, "proxy/crisp/g7[missing]") {
		t.Fatalf("the silent slot was not named: %q", alert.Observed)
	}
}

func TestContractCreationsStuckQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultContractCreationsStuckSettings()
	if settings.PageCalls != 1000 || settings.Hold != 5*time.Minute || settings.Stage != "other" || settings.Window != 2*time.Minute {
		t.Fatalf("§26.4 policy changed: %+v", settings)
	}
	want := `max by (service) (sum by (service, host, block, instance) (max_over_time(urnetwork_contract_creation_stage_inflight{env="main",stage="other"}[2m])))`
	if !strings.Contains(contractCreationsStuckQuery("main", settings), want) {
		t.Fatalf("query lost the catalog expression:\n%s", contractCreationsStuckQuery("main", settings))
	}
}
