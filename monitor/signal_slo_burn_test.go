package monitor

// SIGNALS.md §30.1 on fixture Mimir answers: a burn at each line pages or warns, a slow burn only warns, a
// service under 30 days old, a missing bucket, an unpublished SLI or a blind Mimir reads unobservable and
// never healthy; check-burn reads clear, burning or unknown, the dns roles with the canary beside; and no
// query holds a label that could carry an id.

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// evs's availability within budget: a million requests in 30 days at 99.9%, so a budget of 1,000, with
// a day's series at the period's start and current samples.
func sloBurnTestSamples(service string, objective string) []*sidecarStorageSample {
	return []*sidecarStorageSample{
		sidecarStorageTestSample(sloBurnCheck(service, objective, sloBurnPartTotal), 1000000),
		sidecarStorageTestSample(sloBurnCheck(service, objective, sloBurnPartHistory), 30000),
		sidecarStorageTestSample(sloBurnCheck(service, objective, sloBurnPartFresh), 4),
		sidecarStorageTestSample(sloBurnCheck(service, objective, sloBurnPartPage), 5),
		sidecarStorageTestSample(sloBurnCheck(service, objective, sloBurnPartWarn), 20),
	}
}

// Sets check's sample to value, or drops it with drop.
func setSloBurnTestValue(samples []*sidecarStorageSample, check string, value float64, drop bool) []*sidecarStorageSample {
	kept := []*sidecarStorageSample{}
	for _, sample := range samples {
		if sample.check == check {
			if drop {
				continue
			}
			sample.value = value
		}
		kept = append(kept, sample)
	}
	return kept
}

// Settings listing services.
func sloBurnTestSettings(services ...string) *SidecarSloSettings {
	settings := DefaultSidecarSloSettings()
	settings.Services = services
	return settings
}

// An env over the settings whose Mimir answers samples, or err.
func sloBurnTestEnv(settings *SidecarSloSettings) *probeEnv {
	return &probeEnv{cfg: &monitorConfig{env: "synthetic", sidecarSlo: settings}, now: func() time.Time {
		return time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)
	}}
}

// The probe's findings for the services on samples.
func sloBurnTestCheck(t *testing.T, settings *SidecarSloSettings, samples []*sidecarStorageSample, err error) []finding {
	t.Helper()
	findings, checkErr := sloBurnProbe{query: fixedSidecarStorageQuery(samples, err)}.check(t.Context(), sloBurnTestEnv(settings))
	if checkErr != nil {
		t.Fatal(checkErr)
	}
	return findings
}

// The findings of one target, raised or healthy, as "class healthy|raised".
func sloBurnTargetStates(findings []finding, target string) []string {
	states := []string{}
	for _, finding := range findings {
		if finding.target != target {
			continue
		}
		state := "raised"
		if finding.healthy {
			state = "healthy"
		}
		states = append(states, finding.class+" "+state)
	}
	slices.Sort(states)
	return states
}

// A burn under both lines raises nothing, and clears both classes.
func TestSloBurnWithinBudgetRaisesNothing(t *testing.T) {
	findings := sloBurnTestCheck(t, sloBurnTestSettings("evs"), sloBurnTestSamples("evs", "availability"), nil)
	if got := sloBurnTargetStates(findings, "evs availability"); !slices.Equal(got, []string{"slo-burn-page healthy", "slo-burn-warn healthy"}) {
		t.Errorf("evs availability = %q", got)
	}
}

// 2% of the budget in an hour pages, at the line and not under it; the page's alert reaches §19.8's rota.
func TestSloBurnPagesAtTwoPercentOfTheBudgetInAnHour(t *testing.T) {
	check := sloBurnCheck("evs", "availability", sloBurnPartPage)
	samples := setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), check, 19, false)
	if got := sloBurnTargetStates(sloBurnTestCheck(t, sloBurnTestSettings("evs"), samples, nil), "evs availability"); !slices.Contains(got, "slo-burn-page healthy") {
		t.Errorf("19 of 1,000 paged: %q", got)
	}
	samples = setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), check, 20, false)
	samples = setSloBurnTestValue(samples, sloBurnCheck("evs", "availability", sloBurnPartWarn), 20, false)
	findings := sloBurnTestCheck(t, sloBurnTestSettings("evs"), samples, nil)
	if got := sloBurnTargetStates(findings, "evs availability"); !slices.Equal(got, []string{"slo-burn-page raised", "slo-burn-warn healthy"}) {
		t.Fatalf("20 of 1,000 = %q", got)
	}
	for _, finding := range findings {
		if finding.class == sloBurnClassPage && !finding.healthy {
			alert := alertFromFinding(syntheticSettings(nil), "30.1", "slo-burn", "Sidecar SLO budget burn", finding)
			if _, paged := pageClassOf(alert.Class); !paged || alert.Severity != SeverityPage || finding.sustain != 2 {
				t.Errorf("the burn page does not reach the rota: %+v", alert)
			}
			if !strings.Contains(alert.Markdown(), "roll") && !strings.Contains(alert.Markdown(), "Roll") {
				t.Errorf("the page does not say to roll back: %s", alert.Markdown())
			}
		}
	}
}

// 10% in 6 h warns; a slow burn under the hour's line only warns.
func TestSloBurnSlowBurnOnlyWarns(t *testing.T) {
	samples := setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), sloBurnCheck("evs", "availability", sloBurnPartWarn), 99, false)
	if got := sloBurnTargetStates(sloBurnTestCheck(t, sloBurnTestSettings("evs"), samples, nil), "evs availability"); !slices.Contains(got, "slo-burn-warn healthy") {
		t.Errorf("99 of 1,000 warned: %q", got)
	}
	samples = setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), sloBurnCheck("evs", "availability", sloBurnPartWarn), 100, false)
	if got := sloBurnTargetStates(sloBurnTestCheck(t, sloBurnTestSettings("evs"), samples, nil), "evs availability"); !slices.Equal(got, []string{"slo-burn-page healthy", "slo-burn-warn raised"}) {
		t.Errorf("a slow burn = %q", got)
	}
}

// No budget, no verdict: under 30 days of history, no current series, no event in 30 days, or Mimir not
// reading each read unobservable, with no healthy finding to clear a burn.
func TestSloBurnWithoutABudgetIsUnobservableNeverHealthy(t *testing.T) {
	for _, c := range []struct {
		label   string
		samples []*sidecarStorageSample
		err     error
		want    string
	}{
		{label: "under 30 days", samples: setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), sloBurnCheck("evs", "availability", sloBurnPartHistory), 0, true), want: "under 30 days old"},
		{label: "no current series", samples: setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), sloBurnCheck("evs", "availability", sloBurnPartFresh), 0, true), want: "no current series"},
		{label: "no event", samples: setSloBurnTestValue(sloBurnTestSamples("evs", "availability"), sloBurnCheck("evs", "availability", sloBurnPartTotal), 0, false), want: "no event in 30 days"},
		{label: "mimir", err: errors.New("mimir did not answer"), want: "did not read from Mimir"},
	} {
		findings := sloBurnTestCheck(t, sloBurnTestSettings("evs"), c.samples, c.err)
		if got := sloBurnTargetStates(findings, "evs availability"); !slices.Equal(got, []string{"slo-unobservable raised"}) {
			t.Errorf("%s: %q", c.label, got)
		}
		for _, finding := range findings {
			if finding.target == "evs availability" && (!finding.isUnobservable() || !strings.Contains(finding.symptom, c.want)) {
				t.Errorf("%s: %+v", c.label, finding)
			}
		}
	}
}

// A latency objective counts events past its bound from that bucket, and reads unobservable when the bound
// is no bucket of its histogram; an objective whose SLI is not published reads unobservable.
func TestSloBurnLatencyAndUnmeasuredObjectives(t *testing.T) {
	samples := append(sloBurnTestSamples("sockets", "availability"), sloBurnTestSamples("sockets", "find")...)
	// p99: a budget of 1% of a million, 10,000; 200 slow in an hour is exactly 2%
	samples = setSloBurnTestValue(samples, sloBurnCheck("sockets", "find", sloBurnPartPage), 200, false)
	findings := sloBurnTestCheck(t, sloBurnTestSettings("sockets"), samples, nil)
	if got := sloBurnTargetStates(findings, "sockets find"); !slices.Equal(got, []string{"slo-burn-page raised", "slo-burn-warn healthy"}) {
		t.Errorf("sockets find = %q", got)
	}
	samples = setSloBurnTestValue(samples, sloBurnCheck("sockets", "find", sloBurnPartPage), 0, true)
	findings = sloBurnTestCheck(t, sloBurnTestSettings("sockets"), samples, nil)
	if got := sloBurnTargetStates(findings, "sockets find"); !slices.Equal(got, []string{"slo-unobservable raised"}) {
		t.Errorf("sockets find with no bucket = %q", got)
	}
	findings = sloBurnTestCheck(t, sloBurnTestSettings("evs"), sloBurnTestSamples("evs", "availability"), nil)
	if got := sloBurnTargetStates(findings, "evs head-to-hint"); !slices.Equal(got, []string{"slo-unobservable raised"}) {
		t.Errorf("evs head-to-hint = %q", got)
	}
}

// Without the block the signal is configured off and raises no incident; a block that does not read is
// blind.
func TestSloBurnConfiguredOffAndBlind(t *testing.T) {
	findings := sloBurnTestCheck(t, nil, nil, nil)
	if len(findings) != 1 || !findings[0].configuredOff {
		t.Errorf("no block = %+v", findings)
	}
	settings := sloBurnTestSettings()
	settings.ConfigError = "sidecar_services names \"nope\""
	findings = sloBurnTestCheck(t, settings, nil, nil)
	if len(findings) != 1 || !findings[0].isUnobservable() || findings[0].configuredOff {
		t.Errorf("a broken block = %+v", findings)
	}
	if settings := loadSidecarSloSettings(nil); settings != nil {
		t.Errorf("no block loads %+v", settings)
	}
	for _, block := range []*sidecarServicesMonitorYaml{{Services: []string{"evs", "nope"}}, {Services: []string{"evs", "evs"}}} {
		if settings := loadSidecarSloSettings(block); settings.ConfigError == "" {
			t.Errorf("%v loaded without an error", block.Services)
		}
	}
	if settings := loadSidecarSloSettings(&sidecarServicesMonitorYaml{Services: []string{"evs", "gkm"}}); settings.ConfigError != "" || !slices.Equal(settings.Services, []string{"evs", "gkm"}) {
		t.Errorf("evs and gkm load as %+v", settings)
	}
}

// D-OPS-8: every query sums its labels away and matches only the environment, the code and a bucket's le,
// so no SLO series or finding carries a network, repo, client or route.
func TestSloBurnQueriesHoldNoIdLabel(t *testing.T) {
	services := []string{}
	for _, row := range sloTable {
		services = append(services, row.service)
	}
	settings := sloBurnTestSettings(services...)
	for _, query := range []string{sloBurnQuery("synthetic", settings, services), sidecarServiceFailuresQuery("synthetic", settings, services)} {
		if query == "" {
			t.Fatal("no query")
		}
		if strings.Contains(query, " by (") || strings.Contains(query, " by(") || strings.Contains(query, "without") {
			t.Errorf("a query keeps labels: %s", query)
		}
		for _, matcher := range regexp.MustCompile(`([a-z_]+)(=~|=)"`).FindAllStringSubmatch(query, -1) {
			if !slices.Contains([]string{"env", "code", "le"}, matcher[1]) {
				t.Errorf("a query matches on %s", matcher[1])
			}
		}
	}
}

// check-burn: clear, burning at either line, unknown for any objective without a budget or a service not
// joined; the dns roles read the canary, its signature or serial divergence burning.
func TestCheckSloBurnReadsClearBurningOrUnknown(t *testing.T) {
	settings := sloBurnTestSettings("sockets", "evs", "dns")
	samples := append(sloBurnTestSamples("sockets", "availability"), sloBurnTestSamples("sockets", "find")...)
	check := func(service string, samples []*sidecarStorageSample) *SloBurnCheck {
		return checkSloBurn(t.Context(), sloBurnTestEnv(settings), sloBurnProbe{query: fixedSidecarStorageQuery(samples, nil)}, service)
	}
	if result := check("sockets", samples); result.State != SloBurnClear || len(result.Lines) != 2 {
		t.Errorf("sockets = %+v", result)
	}
	burning := setSloBurnTestValue(slices.Clone(samples), sloBurnCheck("sockets", "availability", sloBurnPartWarn), 1000, false)
	if result := check("sockets", burning); result.State != SloBurnBurning {
		t.Errorf("sockets burning = %+v", result)
	}
	// evs's head to hint has no SLI yet, so evs stays unknown however its availability reads
	if result := check("evs", sloBurnTestSamples("evs", "availability")); result.State != SloBurnUnknown {
		t.Errorf("evs = %+v", result)
	}
	if result := check("gkm", samples); result.State != SloBurnUnknown || !strings.Contains(strings.Join(result.Lines, "\n"), "not joined") {
		t.Errorf("gkm, not joined = %+v", result)
	}

	canary := []finding{}
	var canaryErr error
	original := sloBurnDnsCanaryCheck
	sloBurnDnsCanaryCheck = func(ctx context.Context, env *probeEnv) ([]finding, error) { return canary, canaryErr }
	t.Cleanup(func() { sloBurnDnsCanaryCheck = original })
	for _, c := range []struct {
		label   string
		canary  []finding
		err     error
		service string
		want    string
	}{
		{label: "clean", canary: []finding{healthyFinding(dnsCanaryProbeId, tierPage, dnsCanaryClassSignature, "example.com")}, service: "dnsedge-2", want: SloBurnClear},
		{label: "signature", canary: []finding{{probeId: dnsCanaryProbeId, class: dnsCanaryClassSignature, target: "example.com"}}, service: "dnsedge-2", want: SloBurnBurning},
		{label: "divergence", canary: []finding{{probeId: dnsCanaryProbeId, class: dnsCanaryClassDivergence, target: "example.com"}}, service: "dnsedge-1", want: SloBurnBurning},
		{label: "propagation alone", canary: []finding{{probeId: dnsCanaryProbeId, class: dnsCanaryClassPropagation, target: "example.com"}}, service: "dnsedge-2", want: SloBurnClear},
		{label: "blind", canary: []finding{{probeId: dnsCanaryProbeId, class: dnsCanaryClassUnobservable, target: "dns-canary", unobservable: true}}, service: "dnsedge-2", want: SloBurnUnknown},
		{label: "off", canary: []finding{{probeId: dnsCanaryProbeId, class: dnsCanaryClassUnobservable, target: "dns-canary", configuredOff: true}}, service: "dnsedge-2", want: SloBurnUnknown},
		{label: "failed", err: errors.New("no unbound"), service: "dnsedge-2", want: SloBurnUnknown},
		{label: "dns, whose own SLO has no SLI yet", canary: []finding{healthyFinding(dnsCanaryProbeId, tierPage, dnsCanaryClassSignature, "example.com")}, service: "dns", want: SloBurnUnknown},
	} {
		canary, canaryErr = c.canary, c.err
		if result := check(c.service, nil); result.State != c.want {
			t.Errorf("%s: %+v, want %s", c.label, result, c.want)
		}
	}
}
