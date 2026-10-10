// Guard and broad failure observations retain urgency without inventing a cause.
package monitor

import (
	"os"
	"strings"
	"testing"
)

// Selects one owning alert after running the real probe on synthetic sources.
func testSitePoolGuardCauseAlert(t *testing.T, fixture *egressSitePoolFixture, frame string) Alert {
	t.Helper()
	alerts, _ := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	for _, alert := range alerts {
		if alert.Class == "egress-prober-fault" && alert.Frame == frame {
			if alert.Severity != SeverityWarn {
				t.Fatalf("changed guard severity: %s", alert.Severity)
			}
			return alert
		}
	}
	t.Fatalf("missing protective finding %q", frame)
	return Alert{}
}

// Rendered action requires the missing same-batch join, not just a matching rate.
func TestEgressSitePoolGuardCauseFullTripIsCandidate(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.guardTrips["full"] = 2
	alert := testSitePoolGuardCauseAlert(t, fixture, "guard-full")
	if alert.Sustain != 1 || alert.Observed != "schedule=full trips=2" {
		t.Fatalf("changed guard counting or sustain: %+v", alert)
	}
	for _, want := range []string{"protective", "not proof", "same-batch", "provider place", "scoring snapshot", "exact running artifact"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("full guard lacks %q qualification", want)
		}
	}
}

// A blackhole guard must retain passing/TLS evidence, not condemn every check.
func TestEgressSitePoolGuardCauseBlackholeKeepsEvidence(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.guardTrips["blackhole"] = 3
	alert := testSitePoolGuardCauseAlert(t, fixture, "guard-blackhole")
	if alert.Sustain != 1 || alert.Observed != "schedule=blackhole trips=3" {
		t.Fatalf("changed blackhole guard counting or sustain: %+v", alert)
	}
	for _, want := range []string{"not proof", "not_measured", "passing", "TLS"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("blackhole guard lacks %q qualification", want)
		}
	}
}

// Recorded class tallies support a protective heuristic, not a unique cause.
func TestEgressSitePoolGuardCauseClassPatternIsCandidate(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.classLoads = [][]string{{"cdn", "600", "1000"}, {"connectivity", "600", "1000"}, {"dns", "600", "1000"}, {"site", "600", "1000"}}
	alert := testSitePoolGuardCauseAlert(t, fixture, "failure-share")
	if alert.Sustain != 2 || alert.Observed != "failed/total dns=400/1000 connectivity=400/1000 cdn=400/1000 site=400/1000" {
		t.Fatalf("changed class counts or sustain: %+v", alert)
	}
	for _, want := range []string{"recorded", "protective", "not proof", "refresh execution"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Errorf("class pattern lacks %q qualification", want)
		}
	}
	if strings.Contains(alert.Markdown(), "fleet-wide") || strings.Contains(alert.Markdown(), "not the pool") {
		t.Error("recorded class pattern overclaims fleet coverage or excludes pool causes")
	}
}

// Positive partial evidence remains visible, but does not become causal proof.
func TestEgressSitePoolGuardCausePartialTripRetained(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.guardTrips = map[string]float64{"full": 2}
	findings := sitePoolCoverageTestFindings(t, fixture, nil, nil)
	requireSitePoolCoverageUnknown(t, findings, "egress-prober-fault")
	for _, f := range findings {
		if f.class == "egress-prober-fault" && f.frame == "guard-full" && !f.healthy {
			if f.observed != "schedule=full trips=2" || !strings.Contains(f.mechanism, "not proof") {
				t.Fatalf("partial positive evidence lost its counts or causal limit: %+v", f)
			}
			return
		}
	}
	t.Fatal("partial guard evidence disappeared")
}

// The complete passing baseline does not acquire a warning from prose changes.
func TestEgressSitePoolGuardCauseHealthyControl(t *testing.T) {
	alerts, _ := runEgressSitePoolFixture(t, healthyEgressSitePoolFixture(), testEgressSitePoolContext())
	for _, alert := range alerts {
		if alert.Class == "egress-prober-fault" {
			t.Fatal("healthy control acquired a protective warning")
		}
	}
}

// Unknown observation cannot resolve a previously raised protective finding.
func TestEgressSitePoolGuardCauseUnknownControl(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.mimirDown = true
	requireSitePoolCoverageUnknown(t, sitePoolCoverageTestFindings(t, fixture, nil, nil), "egress-prober-fault")
}

// Insufficient samples in one class cannot establish the all-class condition.
func TestEgressSitePoolGuardCauseMinimumSampleControl(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.classLoads = [][]string{{"cdn", "0", "1000"}, {"connectivity", "0", "1000"}, {"dns", "0", "50"}, {"site", "0", "1000"}}
	alerts, _ := runEgressSitePoolFixture(t, fixture, testEgressSitePoolContext())
	for _, alert := range alerts {
		if alert.Class == "egress-prober-fault" {
			t.Fatal("sample threshold changed")
		}
	}
}

// Only the fixed schedule classes reach rendered protective findings.
func TestEgressSitePoolGuardCausePrivacyControl(t *testing.T) {
	fixture := healthyEgressSitePoolFixture()
	fixture.guardTrips = map[string]float64{"full": 2, "blackhole": 0, "synthetic-private-token.invalid": 99}
	alert := testSitePoolGuardCauseAlert(t, fixture, "guard-full")
	if strings.Contains(alert.Markdown(), "synthetic-private-token") || strings.Contains(alert.Markdown(), "provider-identity") {
		t.Fatal("unexpected schedule or provider detail leaked")
	}
}

// The owning catalog must preserve both false-trip and false-healthy limits.
func TestEgressSitePoolGuardCauseCatalogContract(t *testing.T) {
	raw, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(raw), "### 2.19b ")
	if !ok {
		t.Fatal("missing owning section")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	section = strings.Join(strings.Fields(section), " ")
	for _, want := range []string{"protective candidate", "not proof", "same-batch", "provider place", "scoring snapshot", "exact running artifact", "incompatible passing loads", "unknown"} {
		if !strings.Contains(section, want) {
			t.Errorf("catalog lacks %q qualifier", want)
		}
	}
}
