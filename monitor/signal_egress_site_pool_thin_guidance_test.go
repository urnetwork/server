// Pool deficits retain their warning without inventing candidate shortage,
// per-run sample loss or a one-refresh recovery guarantee.
package monitor

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Synthetic site counts leave the other classes and source controls healthy.
func testSitePoolThinGuidanceFixture(active, candidates int) (*egressSitePoolFixture, *egressSitePoolContext) {
	fixture := healthyEgressSitePoolFixture()
	fixture.destinations = fixture.destinations[:3]
	poolContext := testEgressSitePoolContext()
	poolContext.siteSettings.SitePoolSize["site"] = 100
	poolContext.siteSettings.SiteSampleSize["site"] = 26
	poolContext.siteSettings.SiteMaxRetirePerRun = 1
	delete(poolContext.candidateNameClasses, "site-candidate")
	for i := 0; i < active; i++ {
		fixture.destinations = append(fixture.destinations, testEgressSitePoolDestinationRow(fmt.Sprintf("synthetic-active-%d", i), "site", true, -1, 0.01))
	}
	for i := 0; i < candidates; i++ {
		poolContext.candidateNameClasses[fmt.Sprintf("synthetic-candidate-%d", i)] = "site"
	}
	return fixture, poolContext
}

// A target deficit with ample eligible reserve requires refresh evidence;
// 98 active sites do not establish a short configured sample of 26.
func TestEgressSitePoolThinGuidanceAvailableCandidates(t *testing.T) {
	fixture, poolContext := testSitePoolThinGuidanceFixture(98, 44)
	alerts, _ := runEgressSitePoolFixture(t, fixture, poolContext)
	alert := requireAlertClass(t, alerts, "egress-site-pool-thin")
	if alert.Frame != "site/below-pool-size" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
		t.Fatal("pool deficit identity, severity or sustain changed")
	}
	if alert.Observed != "class=site active=98 pool_size=100 candidates=44 reason=below-pool-size" {
		t.Fatal("pool deficit or eligible reserve counts changed")
	}
	if strings.Contains(alert.Action, "Extend") || strings.Contains(alert.Mechanism, "samples fewer sites per run") || strings.Contains(alert.Verify, "The next refresh") {
		t.Fatal("eligible reserve still receives a shortage, sample-loss or one-refresh diagnosis")
	}
	for _, phrase := range []string{"RefreshEgressDestinations completion", "protective skip reason", "opened vacancies", "host-check outcomes"} {
		if !strings.Contains(alert.Action, phrase) {
			t.Errorf("available reserve action lacks %q evidence", phrase)
		}
	}
	for _, phrase := range []string{"per-run sample=26", "SiteMaxRetirePerRun=1", "do not attest"} {
		if !strings.Contains(alert.Context, phrase) {
			t.Errorf("pool context lacks %q authority limit", phrase)
		}
	}
	if !strings.Contains(alert.Verify, "several completed refresh cadences") || !strings.Contains(alert.Verify, "eligible reserve remains") {
		t.Fatal("recovery omits capped refreshes or the remaining reserve")
	}
}

// True exhaustion keeps candidate-list guidance for either a deficit or a
// full active pool without a replacement reserve.
func TestEgressSitePoolThinGuidanceExhaustedCandidates(t *testing.T) {
	for _, active := range []int{98, 100} {
		fixture, poolContext := testSitePoolThinGuidanceFixture(active, 0)
		alerts, _ := runEgressSitePoolFixture(t, fixture, poolContext)
		alert := requireAlertClass(t, alerts, "egress-site-pool-thin")
		frame := "site/below-pool-size"
		if active == 100 {
			frame = "site/candidates-exhausted"
		}
		if alert.Frame != frame || !strings.Contains(alert.Action, "Extend the site candidates") || !strings.Contains(alert.Action, "successful candidate host checks") {
			t.Errorf("active=%d lost exhaustion identity or checked replenishment guidance", active)
		}
	}
}

// Complete pool and reserve stay healthy, including surplus active sites.
func TestEgressSitePoolThinGuidanceHealthyControl(t *testing.T) {
	for _, active := range []int{100, 101} {
		fixture, poolContext := testSitePoolThinGuidanceFixture(active, 1)
		alerts, _ := runEgressSitePoolFixture(t, fixture, poolContext)
		for _, alert := range alerts {
			if alert.Class == "egress-site-pool-thin" {
				t.Errorf("active=%d with an eligible reserve acquired a pool warning", active)
			}
		}
	}
}

// Other protective and observation findings remain independent of the
// pool deficit; their coexistence does not attest the refresh's skip cause.
func TestEgressSitePoolThinGuidanceUnknownAndIndependentCauses(t *testing.T) {
	fixture, poolContext := testSitePoolThinGuidanceFixture(98, 44)
	fixture.mimirDown = true
	fixture.classLoads = [][]string{{"dns", "60", "100"}, {"connectivity", "60", "100"}, {"cdn", "60", "100"}, {"site", "60", "100"}}
	fixture.countries = [][]string{{"aa", "1", "1", "1", "30", "0"}, {"bb", "2", "2", "2", "30", "0"}}
	alerts, _ := runEgressSitePoolFixture(t, fixture, poolContext)
	if len(alerts) != 5 {
		t.Fatalf("independent retained boundaries=%d, want 5", len(alerts))
	}
	requireAlertClass(t, alerts, "egress-prober-fault")
	requireAlertClass(t, alerts, "egress-site-pool-unobservable")
	alert := requireAlertClass(t, alerts, "egress-site-pool-thin")
	if !strings.Contains(alert.Action, "Check bounded") || !strings.Contains(alert.Context, "do not attest") || strings.Contains(alert.Action, "Extend") {
		t.Fatal("coincident findings became a proven refresh cause or candidate shortage")
	}
}

// Guidance uses the owning refresh cap and sample, not the observed example.
func TestEgressSitePoolThinGuidanceConfiguredControl(t *testing.T) {
	fixture, poolContext := testSitePoolThinGuidanceFixture(98, 44)
	poolContext.siteSettings.SiteSampleSize["site"] = 13
	poolContext.siteSettings.SiteMaxRetirePerRun = 4
	alerts, _ := runEgressSitePoolFixture(t, fixture, poolContext)
	alert := requireAlertClass(t, alerts, "egress-site-pool-thin")
	if !strings.Contains(alert.Context, "per-run sample=13") || !strings.Contains(alert.Context, "SiteMaxRetirePerRun=4") {
		t.Fatal("guidance did not use the configured sample and extra-vacancy cap")
	}
}

// The owning catalog retains the same cause and recovery limits.
func TestEgressSitePoolThinGuidanceCatalog(t *testing.T) {
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
	for _, phrase := range []string{"nonempty reserve is not candidate exhaustion", "successful host checks", "short compatible per-run sample", "SiteMaxRetirePerRun", "several completed refresh cadences", "first inspect refresh completion"} {
		if !strings.Contains(section, phrase) {
			t.Errorf("catalog lacks %q boundary", phrase)
		}
	}
}
