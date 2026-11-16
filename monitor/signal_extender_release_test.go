package monitor

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// §28.1 over synthetic series: an hour of one-minute evaluations, the longest
// hold, of the online pool, the gossip release, the stored activations and the
// gated release answers.

const extenderReleaseTestPoints = 60

// One value per evaluation of each part, NaN for none. A nil slice leaves the
// part out of the answer.
type extenderReleaseTestSeries struct {
	online          []float64
	gossipExtenders []float64
	gossipRecords   []float64
	dnsExtenders    []float64
	activated       []float64
	activationPosts []float64
	releaseOk       []float64
	releaseAnswers  []float64
}

// Main on 2026-11-05: 160 online extenders, 179 of them released in the day,
// about 39 activations stored an hour, and no gated release requests.
func extenderReleaseTestHealthy() extenderReleaseTestSeries {
	return extenderReleaseTestSeries{
		online:          stormRepeat(160, extenderReleaseTestPoints),
		gossipExtenders: stormRepeat(179, extenderReleaseTestPoints),
		gossipRecords:   stormRepeat(1915, extenderReleaseTestPoints),
		dnsExtenders:    stormRepeat(59, extenderReleaseTestPoints),
		activated:       stormRepeat(39, extenderReleaseTestPoints),
		activationPosts: stormRepeat(4300, extenderReleaseTestPoints),
		releaseOk:       stormRepeat(0, extenderReleaseTestPoints),
		releaseAnswers:  stormRepeat(0, extenderReleaseTestPoints),
	}
}

func extenderReleaseTestResponse(t *testing.T, series extenderReleaseTestSeries) string {
	t.Helper()
	synthetic := []stormSyntheticSeries{}
	for part, values := range map[string][]float64{
		"online":           series.online,
		"gossip_extenders": series.gossipExtenders,
		"gossip_records":   series.gossipRecords,
		"dns_extenders":    series.dnsExtenders,
		"activated":        series.activated,
		"activation_posts": series.activationPosts,
		"release_ok":       series.releaseOk,
		"release_answers":  series.releaseAnswers,
	} {
		if values == nil {
			continue
		}
		synthetic = append(synthetic, stormSyntheticSeries{part: part, labels: map[string]string{}, values: values})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(extenderReleaseTestPoints), time.Minute, synthetic)
}

// Values that change at a step: before for the first count, after for the
// rest.
func extenderReleaseTestStep(before float64, after float64, count int) []float64 {
	values := stormRepeat(after, extenderReleaseTestPoints)
	for index := range count {
		values[index] = before
	}
	return values
}

func TestExtenderReleaseJudgesEveryChannel(t *testing.T) {
	probe := extenderReleaseProbe{}
	const (
		coverage    = "extender-release-coverage-low main/extender-release/gossip"
		activations = "extender-activations-stopped main/extender-release/activation"
		gated       = "extender-gated-release-failing main/extender-release/gated"
		visibility  = "extender-release-unobservable main/extender-release"
	)

	// 2026-11-07 10:32Z: the drain died the morning before, and no record
	// has been released for a day while 160 extenders are online
	stalled := extenderReleaseTestHealthy()
	stalled.gossipExtenders = stormRepeat(0, extenderReleaseTestPoints)
	stalled.gossipRecords = stormRepeat(0, extenderReleaseTestPoints)

	// the slide out of the day: 72 of 160 is under half, 88 is not
	sliding := extenderReleaseTestHealthy()
	sliding.gossipExtenders = extenderReleaseTestStep(88, 72, 10)

	// the drain restarted ten minutes ago, as on 11-11 at 22:15Z
	recovered := extenderReleaseTestHealthy()
	recovered.gossipExtenders = extenderReleaseTestStep(0, 266, extenderReleaseTestPoints-10)

	// an environment with a handful of extenders is not judged
	small := extenderReleaseTestHealthy()
	small.online = stormRepeat(4, extenderReleaseTestPoints)
	small.gossipExtenders = stormRepeat(0, extenderReleaseTestPoints)
	small.activated = stormRepeat(0, extenderReleaseTestPoints)

	// posts arrive and none is stored, for the whole hour
	stopped := extenderReleaseTestHealthy()
	stopped.activated = stormRepeat(0, extenderReleaseTestPoints)

	// one quiet hour that ended twenty minutes ago
	quiet := extenderReleaseTestHealthy()
	quiet.activated = extenderReleaseTestStep(0, 2, extenderReleaseTestPoints-20)

	// clients ask for gated records and nothing is eligible
	empty := extenderReleaseTestHealthy()
	empty.releaseAnswers = stormRepeat(40, extenderReleaseTestPoints)
	empty.releaseOk = stormRepeat(0, extenderReleaseTestPoints)

	// clients ask and most answers release
	serving := extenderReleaseTestHealthy()
	serving.releaseAnswers = stormRepeat(40, extenderReleaseTestPoints)
	serving.releaseOk = stormRepeat(31, extenderReleaseTestPoints)

	gappy := extenderReleaseTestHealthy()
	gappy.gossipExtenders[20] = math.NaN()

	for _, test := range []struct {
		name   string
		series extenderReleaseTestSeries
		want   map[string]string
	}{
		{name: "healthy", series: extenderReleaseTestHealthy(), want: map[string]string{
			coverage: "healthy", activations: "healthy", gated: "healthy", visibility: "healthy",
		}},
		{name: "nothing released for a day", series: stalled, want: map[string]string{
			coverage: "warn", activations: "healthy", gated: "healthy", visibility: "healthy",
		}},
		{name: "the share sliding under half", series: sliding, want: map[string]string{
			coverage: "hold",
		}},
		{name: "the drain restarted", series: recovered, want: map[string]string{
			coverage: "hold",
		}},
		{name: "a pool too small to judge", series: small, want: map[string]string{
			coverage: "healthy", activations: "healthy",
		}},
		{name: "activations stopped", series: stopped, want: map[string]string{
			activations: "warn", coverage: "healthy",
		}},
		{name: "activations back", series: quiet, want: map[string]string{
			activations: "hold",
		}},
		{name: "gated requests release nothing", series: empty, want: map[string]string{
			gated: "warn", coverage: "healthy",
		}},
		{name: "gated requests served", series: serving, want: map[string]string{
			gated: "healthy",
		}},
		{name: "an evaluation missing", series: gappy, want: map[string]string{
			coverage: "hold", activations: "healthy", visibility: "unobservable",
		}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, extenderReleaseTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// Without the online pool nothing is judged: the coverage and the activations
// are read against it, so its absence is unknown, never a healthy zero.
func TestExtenderReleaseReportsMissingState(t *testing.T) {
	probe := extenderReleaseProbe{}
	missing := extenderReleaseTestHealthy()
	missing.online = nil
	states := stormFindingStates(t, probe, stormSyntheticNow, extenderReleaseTestResponse(t, missing), nil)
	if len(states) != 1 || states["extender-release-unobservable main/extender-release"] != "unobservable" {
		t.Fatalf("states = %v", states)
	}
	alerts := runStormSynthetic(t, NewExtenderReleaseSignal(), stormSyntheticNow, extenderReleaseTestResponse(t, missing), nil)
	if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "extender_state_missing") {
		t.Fatalf("alerts = %+v", alerts)
	}
}

// The alerts carry the fleet values that explain them, and the query reads
// the series and windows §28.1 names.
func TestExtenderReleaseQueryAndObservedValues(t *testing.T) {
	settings := DefaultExtenderReleaseSettings()
	if settings.CoverageMinShare != 0.5 || settings.CoverageHold != time.Hour || settings.ActivationWindow != time.Hour ||
		settings.ActivationHold != 30*time.Minute || settings.ReleaseMinAnswers != 10 || settings.ReleaseMinOkShare != 0.5 ||
		settings.ReleaseHold != 30*time.Minute || settings.MinOnlineExtenders != 10 {
		t.Fatalf("§28.1 policy changed: %+v", settings)
	}
	query := extenderReleaseQuery("main", settings)
	for _, want := range []string{
		`max(urnetwork_stats_online_extenders{env="main"})`,
		`max(urnetwork_stats_extender_gossip_extenders_24h{env="main",kind="record"})`,
		`sum(increase(urnetwork_extender_activations_total{env="main",stage="store",outcome="activated"}[1h]))`,
		`sum(increase(urnetwork_extender_releases_total{env="main",outcome="ok"}[1h]))`,
		`sum(increase(urnetwork_extender_releases_total{env="main",outcome!~"rate_limited|epoch_spent|probation"}[1h]))`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}

	series := extenderReleaseTestHealthy()
	series.gossipExtenders = stormRepeat(0, extenderReleaseTestPoints)
	series.gossipRecords = stormRepeat(0, extenderReleaseTestPoints)
	series.activated = stormRepeat(0, extenderReleaseTestPoints)
	series.releaseAnswers = stormRepeat(40, extenderReleaseTestPoints)
	alerts := runStormSynthetic(t, NewExtenderReleaseSignal(), stormSyntheticNow, extenderReleaseTestResponse(t, series), nil)
	found := map[string]bool{}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "203.0.113", "2001:db8", "https://")
		if alert.SignalNumber != "28.1" || alert.Severity != SeverityWarn {
			continue
		}
		switch alert.Class {
		case "extender-release-coverage-low":
			found[alert.Class] = strings.Contains(alert.Observed, "online_extenders=160") && strings.Contains(alert.Observed, "coverage=0.00")
		case "extender-activations-stopped":
			found[alert.Class] = strings.Contains(alert.Observed, "stored_activations_1h=0.0") && strings.Contains(alert.Observed, "activation_posts_1h=4300.0")
		case "extender-gated-release-failing":
			found[alert.Class] = strings.Contains(alert.Observed, "release_answers_1h=40.0") && strings.Contains(alert.Observed, "ok_share=0.00")
		}
	}
	for _, class := range []string{"extender-release-coverage-low", "extender-activations-stopped", "extender-gated-release-failing"} {
		if !found[class] {
			t.Fatalf("no %s alert with its values in %+v", class, alerts)
		}
	}
}

func TestExtenderReleaseRejectsInvalidPolicyWithoutReading(t *testing.T) {
	signal := NewExtenderReleaseSignalWithSettings(&ExtenderReleaseSettings{})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatalf("read its source under an invalid policy")
		return "", nil
	}}
	alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "extender-release-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
		t.Fatalf("%v %+v", err, alerts)
	}
}
