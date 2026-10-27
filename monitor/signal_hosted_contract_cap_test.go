package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Recorded Mimir history of 10-26: g4 on both proxy hosts, running since
// 18:35Z, holds above 50 refusals/s from 20:12Z and warns at 20:21Z, not a
// minute earlier. The rollout burst of g7-g10 at 18:52-19:19Z (103-137/s at
// peak) came within 30 minutes of each block's start and is left out. Late
// that evening, with every block old and quiet, nothing is raised.
func TestHostedContractCapReplays(t *testing.T) {
	cases := loadStormReplayFixture(t, "hosted-contract-cap")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "cap-saturated", targets: []string{"crisp/g4", "fireside/g4"}},
		{name: "cap-before"},
		{name: "rollout-burst-left-out"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewHostedContractCapSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["hosted-contract-cap-saturated"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
		for _, alert := range alerts {
			if alert.Severity != SeverityWarn || alert.SignalNumber != "26.11" {
				t.Fatalf("%s: %+v", test.name, alert)
			}
		}
	}
	g4 := stormAlertsByTarget(runStormReplay(t, NewHostedContractCapSignal(), cases["cap-saturated"], nil), "hosted-contract-cap-saturated")["crisp/g4"]
	if !strings.Contains(g4.Observed, "refusals_per_second_now=57.8") || !strings.Contains(g4.Observed, "newest_process_uptime_seconds=6351") {
		t.Fatalf("observed %q", g4.Observed)
	}
	states := stormFindingStates(t, hostedContractCapProbe{}, cases["rollout-burst-left-out"].Now, string(cases["rollout-burst-left-out"].Response), nil)
	requireStormStates(t, "rollout-burst-left-out", states, map[string]string{
		"hosted-contract-cap-saturated crisp/g10":                   "hold",
		"hosted-contract-cap-unobservable main/hosted-contract-cap": "healthy",
	})
}

// A synthetic answer for one block: refusals as the catalog expression
// returns them (NaN where the uptime condition drops the block), whether its
// processes export the counter, and its newest process's uptime.
func hostedContractCapResponse(t *testing.T, refusals, capped, uptime []float64) string {
	series := []stormSyntheticSeries{}
	for _, host := range []string{"crisp", "fireside"} {
		for _, block := range stormMainServiceBlocks["proxy"] {
			labels := map[string]string{"host": host, "block": block}
			if host == "crisp" && block == "g4" {
				series = append(series,
					stormSyntheticSeries{part: "refusals", labels: labels, values: refusals},
					stormSyntheticSeries{part: "capped", labels: labels, values: capped},
					stormSyntheticSeries{part: "uptime", labels: labels, values: uptime},
				)
				continue
			}
			series = append(series,
				stormSyntheticSeries{part: "refusals", labels: labels, values: stormRepeat(0, 10)},
				stormSyntheticSeries{part: "capped", labels: labels, values: stormRepeat(1, 10)},
				stormSyntheticSeries{part: "uptime", labels: labels, values: stormRepeat(7200, 10)},
			)
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, series)
}

// The band is strict and needs ten judged minutes; an evaluation the uptime
// condition drops neither fires nor clears; a block without the counter runs
// no cap; a placed block without a process is unknown.
func TestHostedContractCapUptimeRuleAndBlocks(t *testing.T) {
	probe := hostedContractCapProbe{}
	identity := "hosted-contract-cap-saturated crisp/g4"
	visibility := "hosted-contract-cap-unobservable main/hosted-contract-cap"
	old := stormRepeat(7200, 10)
	young := []float64{1500, 1560, 1620, 1680, 1740, 1800, 1860, 1920, 1980, 2040}
	droppedYoung := append(stormRepeat(math.NaN(), 6), stormRepeat(80, 4)...)
	nan := stormRepeat(math.NaN(), 10)
	for _, test := range []struct {
		name                     string
		refusals, capped, uptime []float64
		want                     map[string]string
	}{
		{name: "healthy", refusals: stormRepeat(2, 10), capped: stormRepeat(1, 10), uptime: old, want: map[string]string{identity: "healthy", visibility: "healthy"}},
		{name: "at the level", refusals: stormRepeat(50, 10), capped: stormRepeat(1, 10), uptime: old, want: map[string]string{identity: "healthy"}},
		{name: "above the level", refusals: stormRepeat(51, 10), capped: stormRepeat(1, 10), uptime: old, want: map[string]string{identity: "warn", "hosted-contract-cap-saturated fireside/g4": "healthy"}},
		{name: "a young process is left out", refusals: droppedYoung, capped: stormRepeat(1, 10), uptime: young, want: map[string]string{identity: "hold", visibility: "healthy"}},
		{name: "no cap refuses nothing", refusals: nan, capped: nan, uptime: young, want: map[string]string{identity: "healthy", visibility: "healthy"}},
		{name: "a block without a process", refusals: nan, capped: nan, uptime: nan, want: map[string]string{identity: "hold", visibility: "unobservable"}},
		{name: "an old process without a rate", refusals: nan, capped: stormRepeat(1, 10), uptime: old, want: map[string]string{identity: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, hostedContractCapResponse(t, test.refusals, test.capped, test.uptime), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestHostedContractCapQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultHostedContractCapSettings()
	if settings.WarnRefusals != 50 || settings.Hold != 10*time.Minute || settings.MinUptime != 30*time.Minute || settings.Window != 5*time.Minute {
		t.Fatalf("§26.11 policy changed: %+v", settings)
	}
	want := `sum by (host, block) (rate(urnetwork_hosted_contract_admission_refusals_total{env="main"}[5m])) and on (host, block) (time() - max by (host, block) (process_start_time_seconds{env="main",service="proxy"}) > 1800)`
	if query := hostedContractCapQuery("main", settings); !strings.Contains(query, want) {
		t.Fatalf("query lost the catalog expression:\n%s", query)
	}
}
