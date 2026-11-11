// A country whose national firewall makes UDP results say nothing about the
// provider turns UDP probes off (qualityprobe.yml country_settings
// udp_probes: false). The HTTP/3 legs its exits do not make are not measured:
// no failure and no pass, no HTTP/3 place evidence, and nothing in the HTTP/3
// common-mode window, which goes on judging the countries that measure UDP.
package work

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/fleetprobe"
)

// The pool as the server serves it: one h3 URL, and Iran's UDP probes off.
const udpCountryTestPoolWire = `{"version": 7, "destinations": [
	{"name": "synthetic-h3-site", "class": "site", "url": "https://h3.example/", "expect": "body", "h3": true}
], "country_settings": {"ir": {"udp_probes": false}}}`

var udpCountryTestCycleStartedAt = time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)

// One URL turn of the served pool by a provider published in country, through
// the production prober's health hook, over a provider path whose HTTPS leg
// passes and whose HTTP/3 leg is h3.
func udpCountryTestRun(t *testing.T, country string, h3 http.RoundTripper) *egresshealth.Result {
	t.Helper()
	var pool egresshealth.Pool
	if err := json.Unmarshal([]byte(udpCountryTestPoolWire), &pool); err != nil {
		t.Fatal(err)
	}
	full := fleetprobe.NewFullProber(fleetprobe.FullOptions{UrlProbe: true, ProbeTimeout: time.Minute, LoadAttempts: 1,
		Pool: func() *egresshealth.Pool { return &pool }})
	transport := &testH3Transport{https: testH3Leg(http.StatusOK, nil), h3: h3}
	run, err := full.Health(context.Background(), &http.Client{Transport: transport}, egresshealth.Place{Country: country})
	if err != nil {
		t.Fatal(err)
	}
	if run.UrlProbeEvidence == nil || len(run.Checks) != 1 || !run.UrlProbeEvidence.Destination.H3 {
		t.Fatalf("%s: not one h3 URL load: %+v", country, run)
	}
	if err := run.UrlProbeEvidence.ValidateOutcome(run.OkCount, run.Total, run.TlsAuthenticationFailure); err != nil {
		t.Fatalf("%s: the outcome fails ingest validation: %v", country, err)
	}
	run.CycleStartedAt = udpCountryTestCycleStartedAt
	return run
}

// The HTTP/3 leg of a provider that carries no UDP: nothing answers.
func udpCountryTestSilentLeg() http.RoundTripper {
	return testH3Leg(0, &testH3StageError{stage: egresshealth.FailureStageDialQuic})
}

// In ir, a provider that carries no UDP passes its h3 URL on the HTTPS leg:
// the leg not made is no failure. Its outcome carries no HTTP/3 leg, so the
// ingest's place judgement (model.JudgeProviderEgressH3) returns it unchanged
// at its first check: it records no place evidence for ir, reads none, and
// gets no verdict, so ir exits never count toward the three-network rule. The
// site tallies read its passed HTTPS leg. In de the same provider still makes
// the leg, and its silence fails the load until the ingest judges it.
func TestUdpCountrySwitchLoadIsNoFailureAndNoPlaceEvidence(t *testing.T) {
	ir := udpCountryTestRun(t, "ir", udpCountryTestSilentLeg())
	if ir.Total != 1 || ir.OkCount != 1 || len(ir.FailedNames()) != 0 || ir.FailureStageSummary() != "" || ir.H3Summary() != "" {
		t.Fatalf("ir: ok=%d/%d failed=%v stages=%q h3=%q; the leg not made must not fail the load",
			ir.OkCount, ir.Total, ir.FailedNames(), ir.FailureStageSummary(), ir.H3Summary())
	}
	if ir.UrlProbeEvidence.H3 != nil || ir.Checks[0].H3 != nil || ir.Checks[0].H3Required {
		t.Fatalf("ir: an HTTP/3 leg %+v was made; it would be place evidence or judged by place", ir.UrlProbeEvidence.H3)
	}
	if providerUrlProbeCountedH3Leg(ir) != nil || providerUrlProbeH3OnlyFailure(ir) {
		t.Fatal("ir: the outcome reached the HTTP/3 common-mode window")
	}
	scoring := model.NewProviderEgressHealthScoring([]*model.ProviderEgressDestination{
		{Name: "synthetic-h3-site", Class: "site", Active: true},
	})
	loads, _ := egressHealthSiteLoads(ir, model.ProviderEgressPlace{CountryCode: "ir"}, scoring, 0.9)
	if len(loads) != 1 || !loads[0].Ok {
		t.Fatalf("ir: site loads %+v; the site worked over HTTPS", loads)
	}

	de := udpCountryTestRun(t, "de", udpCountryTestSilentLeg())
	if de.OkCount != 0 || de.UrlProbeEvidence.H3 == nil || de.UrlProbeEvidence.H3.Udp != egresshealth.UrlProbeH3UdpSilent || !providerUrlProbeH3OnlyFailure(de) {
		t.Fatalf("de: ok=%d leg=%+v; the UDP leg must still be made and fail on silence", de.OkCount, de.UrlProbeEvidence.H3)
	}
}

// The HTTP/3 common-mode guard judges the countries that measure UDP. Exits
// in ir neither fill its window with silence nor end an episode with a pass:
//
//   - 15 de networks go silent: the window holds 15, below the 16 it needs;
//   - 10 ir networks whose UDP would be silent publish as HTTPS passes and add
//     nothing, so the window does not trip on them;
//   - a 16th de network goes silent: the window trips and holds it;
//   - 5 ir networks whose UDP would pass publish and add nothing, so the
//     episode goes on;
//   - 3 more de networks go silent and are held.
func TestUdpCountrySwitchLegsStayOutOfTheH3CommonModeWindow(t *testing.T) {
	deSilent := udpCountryTestRun(t, "de", udpCountryTestSilentLeg())
	irSilent := udpCountryTestRun(t, "ir", udpCountryTestSilentLeg())
	irPassing := udpCountryTestRun(t, "ir", testH3Leg(http.StatusOK, nil))
	turns := testUrlCommonModeTurns("synthetic-de", 0, 15, deSilent)
	turns = append(turns, testUrlCommonModeTurns("synthetic-ir-silent", 0, 10, irSilent)...)
	turns = append(turns, testUrlCommonModeTurns("synthetic-de", 15, 1, deSilent)...)
	turns = append(turns, testUrlCommonModeTurns("synthetic-ir-passing", 0, 5, irPassing)...)
	turns = append(turns, testUrlCommonModeTurns("synthetic-de", 16, 3, deSilent)...)
	_, releases := testRunUrlCommonModeTurns(t, 1, turns)
	publishedCount, heldCount, unexpected := testUrlCommonModeCount(releases, func(index int) bool { return index == 25 || 31 <= index })
	if publishedCount != 30 || heldCount != 4 || len(unexpected) != 0 {
		t.Fatalf("published %d held %d (unexpected turns %v), want 30 and 4: ir's legs not made must not count in the HTTP/3 window",
			publishedCount, heldCount, unexpected)
	}
}
