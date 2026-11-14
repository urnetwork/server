package monitor

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// §27.1 over synthetic seed-chain series: one hour of one-minute evaluations,
// the origin's stored bundle and one mirror route, m1/ipv4.

const extenderSeedTestPoints = 60

// A source's series over the read: sequence, peak, issue time and expiry, one
// value per evaluation (NaN for none), and the route's up state.
type extenderSeedTestSource struct {
	sequence, peak, issued, expires, up []float64
}

// A healthy source: the bundle issued at issued, valid for a day, serving
// sequence, at every evaluation.
func extenderSeedTestHealthy(sequence float64, issued float64) extenderSeedTestSource {
	return extenderSeedTestSource{
		sequence: stormRepeat(sequence, extenderSeedTestPoints),
		peak:     stormRepeat(sequence, extenderSeedTestPoints),
		issued:   stormRepeat(issued, extenderSeedTestPoints),
		expires:  stormRepeat(issued+24*3600, extenderSeedTestPoints),
		up:       stormRepeat(1, extenderSeedTestPoints),
	}
}

func extenderSeedTestResponse(t *testing.T, enabled []float64, origin extenderSeedTestSource, routes map[string]extenderSeedTestSource) string {
	t.Helper()
	series := []stormSyntheticSeries{
		{part: "enabled", values: enabled},
		{part: "published", values: origin.sequence},
		{part: "published_peak", values: origin.peak},
		{part: "published_issued", values: origin.issued},
		{part: "published_expires", values: origin.expires},
	}
	for key, route := range routes {
		mirror, routeName, _ := strings.Cut(key, "/")
		labels := map[string]string{"mirror": mirror, "route": routeName}
		for part, values := range map[string][]float64{
			"up": route.up, "sequence": route.sequence, "peak": route.peak, "issued": route.issued, "expires": route.expires,
		} {
			if values == nil {
				continue
			}
			series = append(series, stormSyntheticSeries{part: part, labels: labels, values: values})
		}
	}
	return stormSyntheticResponse(t, stormSyntheticStart(extenderSeedTestPoints), time.Minute, series)
}

func TestExtenderSeedMirrorsJudgesEverySource(t *testing.T) {
	probe := extenderSeedMirrorsProbe{}
	at := float64(fleetEvaluationTime(stormSyntheticNow, DefaultStormReadSettings().EvaluationLag).Unix())
	issued := at - 3600
	sequence := issued * 1000
	on := stormRepeat(1, extenderSeedTestPoints)
	nan := stormRepeat(math.NaN(), extenderSeedTestPoints)
	healthy := extenderSeedTestHealthy(sequence, issued)

	const (
		originStale    = "extender-seed-mirror-stale main/extender-seed/origin"
		originExpired  = "extender-seed-mirror-expired main/extender-seed/origin"
		originRollback = "extender-seed-mirror-rollback main/extender-seed/origin"
		routeStale     = "extender-seed-mirror-stale main/extender-seed/m1/ipv4"
		routeExpired   = "extender-seed-mirror-expired main/extender-seed/m1/ipv4"
		routeRollback  = "extender-seed-mirror-rollback main/extender-seed/m1/ipv4"
		visibility     = "extender-seed-mirrors-unobservable main/extender-seed"
	)

	behind := extenderSeedTestHealthy(sequence-6*3600*1000, issued-6*3600)
	down := extenderSeedTestSource{up: stormRepeat(0, extenderSeedTestPoints)}
	expiring := extenderSeedTestHealthy(sequence, issued)
	expiring.expires = stormRepeat(at+5*3600, extenderSeedTestPoints)
	expiredOrigin := extenderSeedTestHealthy(sequence, at-30*3600)
	expiredOrigin.expires = stormRepeat(at-6*3600, extenderSeedTestPoints)
	rolledBack := extenderSeedTestHealthy(sequence-1000, issued)
	rolledBack.peak = stormRepeat(sequence, extenderSeedTestPoints)
	// behind for most of the hour, caught up at the end
	recovering := extenderSeedTestHealthy(sequence, issued)
	for i := range extenderSeedTestPoints - 5 {
		recovering.issued[i] = issued - 6*3600
		recovering.sequence[i] = sequence - 6*3600*1000
		recovering.peak[i] = sequence - 6*3600*1000
	}
	gappy := extenderSeedTestHealthy(sequence, issued)
	gappy.up[10] = math.NaN()

	for _, test := range []struct {
		name   string
		origin extenderSeedTestSource
		route  extenderSeedTestSource
		want   map[string]string
	}{
		{name: "healthy", origin: healthy, route: healthy, want: map[string]string{
			originStale: "healthy", originExpired: "healthy", originRollback: "healthy",
			routeStale: "healthy", routeExpired: "healthy", routeRollback: "healthy", visibility: "healthy",
		}},
		{name: "a route an hour behind", origin: healthy, route: behind, want: map[string]string{
			routeStale: "warn", routeExpired: "healthy", routeRollback: "healthy", originStale: "healthy", visibility: "healthy",
		}},
		{name: "a route down", origin: healthy, route: down, want: map[string]string{
			routeStale: "warn", routeExpired: "hold", routeRollback: "hold", visibility: "healthy",
		}},
		{name: "a route that caught up", origin: healthy, route: recovering, want: map[string]string{
			routeStale: "hold", routeRollback: "healthy",
		}},
		{name: "a route expiring", origin: healthy, route: expiring, want: map[string]string{
			routeExpired: "warn", routeStale: "healthy", originExpired: "healthy",
		}},
		{name: "the origin expired", origin: expiredOrigin, route: expiredOrigin, want: map[string]string{
			originExpired: "warn", routeExpired: "warn", originStale: "healthy",
		}},
		{name: "a route rolled back", origin: healthy, route: rolledBack, want: map[string]string{
			routeRollback: "warn", originRollback: "healthy",
		}},
		{name: "nothing stored", origin: extenderSeedTestSource{sequence: nan, peak: nan, issued: nan, expires: nan}, route: down, want: map[string]string{
			originStale: "warn", originExpired: "hold", routeStale: "warn",
		}},
		{name: "a route missing an evaluation", origin: healthy, route: gappy, want: map[string]string{
			routeStale: "hold", visibility: "unobservable",
		}},
	} {
		response := extenderSeedTestResponse(t, on, test.origin, map[string]extenderSeedTestSource{"m1/ipv4": test.route})
		states := stormFindingStates(t, probe, stormSyntheticNow, response, nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestExtenderSeedMirrorsReportsOffAndMissingState(t *testing.T) {
	probe := extenderSeedMirrorsProbe{}
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return extenderSeedTestResponse(t, stormRepeat(0, extenderSeedTestPoints), extenderSeedTestSource{}, nil), nil
	}}
	env, err := newProbeEnv(stormMainSettings(stormSyntheticNow, source).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].class != "extender-seed-off" || !findings[0].configuredOff || !findings[0].isUnobservable() {
		t.Fatalf("off: %+v", findings)
	}

	missing := extenderSeedTestResponse(t, stormRepeat(math.NaN(), extenderSeedTestPoints), extenderSeedTestSource{}, nil)
	states := stormFindingStates(t, probe, stormSyntheticNow, missing, nil)
	requireStormStates(t, "missing state", states, map[string]string{"extender-seed-mirrors-unobservable main/extender-seed": "unobservable"})
	if len(states) != 1 {
		t.Fatalf("missing state judged sources: %v", states)
	}
	alerts := runStormSynthetic(t, NewExtenderSeedMirrorsSignal(), stormSyntheticNow, missing, nil)
	if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "seed_state_missing") {
		t.Fatalf("missing state: %+v", alerts)
	}
}

// Alerts name a mirror and a route, never an address, and the query reads the
// series §27.1 names.
func TestExtenderSeedMirrorsQueryAndLabels(t *testing.T) {
	settings := DefaultExtenderSeedMirrorsSettings()
	if settings.StaleHold != time.Hour || settings.ExpiredHold != 10*time.Minute || settings.ExpiryMargin != 6*time.Hour ||
		settings.RollbackHold != 2*time.Minute || settings.RollbackWindow != 6*time.Hour {
		t.Fatalf("§27.1 policy changed: %+v", settings)
	}
	query := extenderSeedMirrorsQuery("main", settings)
	for _, want := range []string{
		`max by (mirror, route) (urnetwork_taskworker_extender_seed_mirror_up{env="main"})`,
		`max by (mirror, route) (max_over_time(urnetwork_taskworker_extender_seed_mirror_sequence{env="main"}[6h]))`,
		`max(urnetwork_taskworker_extender_seed_published_expires_timestamp_seconds{env="main"})`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}

	at := float64(fleetEvaluationTime(stormSyntheticNow, DefaultStormReadSettings().EvaluationLag).Unix())
	behind := extenderSeedTestHealthy((at-7*3600)*1000, at-7*3600)
	response := extenderSeedTestResponse(t, stormRepeat(1, extenderSeedTestPoints), extenderSeedTestHealthy((at-3600)*1000, at-3600), map[string]extenderSeedTestSource{"origin/dns": behind})
	alerts := runStormSynthetic(t, NewExtenderSeedMirrorsSignal(), stormSyntheticNow, response, nil)
	found := false
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "203.0.113", "2001:db8", "https://")
		if alert.Class == "extender-seed-mirror-stale" {
			found = alert.SignalNumber == "27.1" && alert.Severity == SeverityWarn && strings.Contains(alert.Observed, "source=origin/dns")
		}
	}
	if !found {
		t.Fatalf("no stale alert for origin/dns in %+v", alerts)
	}
}

func TestExtenderSeedMirrorsRejectsInvalidPolicyWithoutReading(t *testing.T) {
	signal := NewExtenderSeedMirrorsSignalWithSettings(&ExtenderSeedMirrorsSettings{})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatalf("read its source under an invalid policy")
		return "", nil
	}}
	alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "extender-seed-mirrors-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
		t.Fatalf("%v %+v", err, alerts)
	}
}
