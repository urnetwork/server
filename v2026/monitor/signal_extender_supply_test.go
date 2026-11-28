package monitor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// §28.5 over synthetic series: two hours of one-minute evaluations, the
// longest hold, of the online extenders against their day's peak and the
// directory's created and revoked counts of the day.

const extenderSupplyTestPoints = 120

type extenderSupplyTestSeries struct {
	online     []float64
	onlinePeak []float64
	created    []float64
	revoked    []float64
	activated  []float64
}

// Main from 2026-11-02 to 11-11: about 170 online, 11 created and 20
// revoked a day.
func extenderSupplyTestHealthy() extenderSupplyTestSeries {
	return extenderSupplyTestSeries{
		online:     stormRepeat(168, extenderSupplyTestPoints),
		onlinePeak: stormRepeat(172, extenderSupplyTestPoints),
		created:    stormRepeat(11, extenderSupplyTestPoints),
		revoked:    stormRepeat(20, extenderSupplyTestPoints),
		activated:  stormRepeat(920, extenderSupplyTestPoints),
	}
}

func extenderSupplyTestResponse(t *testing.T, series extenderSupplyTestSeries) string {
	t.Helper()
	synthetic := []stormSyntheticSeries{}
	for part, values := range map[string][]float64{
		"online":      series.online,
		"online_peak": series.onlinePeak,
		"created":     series.created,
		"revoked":     series.revoked,
		"activated":   series.activated,
	} {
		if values == nil {
			continue
		}
		synthetic = append(synthetic, stormSyntheticSeries{part: part, labels: map[string]string{}, values: values})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(extenderSupplyTestPoints), time.Minute, synthetic)
}

// Values that change at a step: before for the first count, after for the
// rest.
func extenderSupplyTestStep(before float64, after float64, count int) []float64 {
	values := stormRepeat(after, extenderSupplyTestPoints)
	for index := range count {
		values[index] = before
	}
	return values
}

func TestExtenderSupplyJudgesFallsAndChurn(t *testing.T) {
	probe := extenderSupplyProbe{}
	const (
		falling    = "extender-supply-falling main/extender-supply"
		churn      = "extender-churn-high main/extender-churn"
		visibility = "extender-supply-unobservable main/extender-supply"
	)

	// 2026-11-02 04:32Z: 173 online against the day's peak of 307, after the
	// prober's false failures; about 136 revoked in the half day
	revoked := extenderSupplyTestHealthy()
	revoked.online = stormRepeat(173, extenderSupplyTestPoints)
	revoked.onlinePeak = stormRepeat(307, extenderSupplyTestPoints)
	revoked.created = stormRepeat(14, extenderSupplyTestPoints)
	revoked.revoked = stormRepeat(150, extenderSupplyTestPoints)

	// a fall that began forty minutes ago
	beginning := extenderSupplyTestHealthy()
	beginning.online = extenderSupplyTestStep(168, 120, extenderSupplyTestPoints-40)

	// a small pool losing a third is under the minimum loss
	small := extenderSupplyTestHealthy()
	small.online = stormRepeat(8, extenderSupplyTestPoints)
	small.onlinePeak = stormRepeat(12, extenderSupplyTestPoints)

	// churn high for the last hour only
	churning := extenderSupplyTestHealthy()
	churning.revoked = extenderSupplyTestStep(20, 60, 60)

	for _, test := range []struct {
		name   string
		series extenderSupplyTestSeries
		want   map[string]string
	}{
		{name: "healthy", series: extenderSupplyTestHealthy(), want: map[string]string{falling: "healthy", churn: "healthy", visibility: "healthy"}},
		{name: "the prober revoked the fleet", series: revoked, want: map[string]string{falling: "warn", churn: "warn", visibility: "healthy"}},
		{name: "a fall beginning", series: beginning, want: map[string]string{falling: "hold", churn: "healthy"}},
		{name: "a small pool", series: small, want: map[string]string{falling: "healthy"}},
		{name: "churn for an hour", series: churning, want: map[string]string{churn: "hold", falling: "healthy"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, extenderSupplyTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// A taskworker that predates the supply gauges still has the fall judged,
// and says the churn is unknown; without the online pool nothing is judged.
func TestExtenderSupplyReportsMissingState(t *testing.T) {
	probe := extenderSupplyProbe{}
	old := extenderSupplyTestHealthy()
	old.online = stormRepeat(173, extenderSupplyTestPoints)
	old.onlinePeak = stormRepeat(307, extenderSupplyTestPoints)
	old.created = nil
	old.revoked = nil
	states := stormFindingStates(t, probe, stormSyntheticNow, extenderSupplyTestResponse(t, old), nil)
	requireStormStates(t, "a build without the supply gauges", states, map[string]string{
		"extender-supply-falling main/extender-supply":      "warn",
		"extender-supply-unobservable main/extender-supply": "unobservable",
	})
	if _, judged := states["extender-churn-high main/extender-churn"]; judged {
		t.Fatalf("churn judged without its counts: %v", states)
	}
	alerts := runStormSynthetic(t, NewExtenderSupplySignal(), stormSyntheticNow, extenderSupplyTestResponse(t, old), nil)
	sawMissing := false
	for _, alert := range alerts {
		if alert.Class == "extender-supply-unobservable" {
			sawMissing = strings.Contains(alert.Observed, "churn_state_missing")
		}
		if alert.Class == "extender-supply-falling" && !strings.Contains(alert.Observed, "online_extenders=173 peak_24h=307 created_24h=unknown revoked_24h=unknown") {
			t.Fatalf("falling alert = %s", alert.Observed)
		}
	}
	if !sawMissing {
		t.Fatalf("no churn_state_missing in %+v", alerts)
	}

	nothing := extenderSupplyTestSeries{}
	states = stormFindingStates(t, probe, stormSyntheticNow, extenderSupplyTestResponse(t, nothing), nil)
	if len(states) != 1 || states["extender-supply-unobservable main/extender-supply"] != "unobservable" {
		t.Fatalf("nothing exported: %v", states)
	}
}

func TestExtenderSupplyQueryAndPolicy(t *testing.T) {
	settings := DefaultExtenderSupplySettings()
	if settings.PeakWindow != 24*time.Hour || settings.FallShare != 0.25 || settings.FallMinLoss != 20 ||
		settings.FallHold != time.Hour || settings.ChurnExcess != 30 || settings.ChurnHold != 2*time.Hour {
		t.Fatalf("§28.5 policy changed: %+v", settings)
	}
	query := extenderSupplyQuery("main", settings)
	for _, want := range []string{
		`max(max_over_time(urnetwork_stats_online_extenders{env="main"}[24h]))`,
		`max(urnetwork_stats_extenders_created_24h{env="main"})`,
		`max(urnetwork_stats_extenders_revoked_24h{env="main"})`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}

	signal := NewExtenderSupplySignalWithSettings(&ExtenderSupplySettings{})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatalf("read its source under an invalid policy")
		return "", nil
	}}
	alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "extender-supply-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
		t.Fatalf("%v %+v", err, alerts)
	}
}
