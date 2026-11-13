// A country whose national firewall makes UDP results say nothing about the
// provider turns UDP probes off (Pool.CountrySettings, udp_probes: false):
// its exits make no HTTP/3 leg, their h3 URLs are judged on the HTTPS leg
// alone, and the evidence records the UDP leg as not measured.
package egresshealth

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// One URL turn of the h3 catalog through transport, as the URL lane runs it
// for a provider whose country does or does not disable UDP probes.
func udpCountryTestCheck(t *testing.T, transport http.RoundTripper, udpProbesDisabled bool) *Result {
	t.Helper()
	result, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, h3TestCatalog), Rand: rand.New(rand.NewSource(3)),
		UdpProbesDisabled: udpProbesDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.UrlProbeEvidence == nil {
		t.Fatal("URL outcome lost its versioned evidence")
	}
	if err := result.UrlProbeEvidence.ValidateOutcome(result.OkCount, result.Total, result.TlsAuthenticationFailure); err != nil {
		t.Fatalf("measured evidence fails ingest validation: %v", err)
	}
	return result
}

// The owner's switch (2026-11-11): in a country that disables UDP probes, a
// provider that carries no UDP passes an h3 URL on its HTTPS leg, the HTTP/3
// path is never asked for, and the outcome says UDP was not measured: no
// HTTP/3 leg to fail or pass, while the destination keeps its h3.
func TestUdpCountrySwitchMakesNoHttp3LegAndPassesOnHttps(t *testing.T) {
	var udpRequests atomic.Int32
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestDropsUdp(&udpRequests)}
	result := udpCountryTestCheck(t, transport, true)
	if transport.httpsCalls.Load() != 1 || transport.h3Asked.Load() != 0 || udpRequests.Load() != 0 {
		t.Fatalf("legs: https=%d h3 asked=%d udp requests=%d; want the HTTPS fetch alone",
			transport.httpsCalls.Load(), transport.h3Asked.Load(), udpRequests.Load())
	}
	if result.Total != 1 || result.OkCount != 1 || len(result.FailedNames()) != 0 || result.FailureStageSummary() != "" {
		t.Fatalf("ok=%d/%d failed=%v stages=%q; the HTTPS leg passed and decides the URL",
			result.OkCount, result.Total, result.FailedNames(), result.FailureStageSummary())
	}
	// the leg not made is logged as the switch's, never as a failure or a
	// pass: h3=country_disabled:1
	check := result.Checks[0]
	if !check.UdpProbesDisabled || check.H3Required || check.H3 != nil || result.H3Summary() != "country_disabled:1" {
		t.Fatalf("check %+v h3 summary %q; no HTTP/3 leg is planned, and the one not made is counted as country_disabled", check, result.H3Summary())
	}
	evidence := result.UrlProbeEvidence
	if !evidence.UdpProbesDisabled || evidence.H3 != nil || !evidence.Destination.H3 || evidence.FailureStage != "" {
		t.Fatalf("evidence udp_probes_disabled=%t h3 leg=%v destination h3=%t stage=%q",
			evidence.UdpProbesDisabled, evidence.H3, evidence.Destination.H3, evidence.FailureStage)
	}
	if !evidence.HttpsOk(result.OkCount) {
		t.Fatal("the site tallies must read the passed HTTPS leg")
	}
	raw, err := json.Marshal(evidence)
	if err != nil || !strings.Contains(string(raw), `"udp_probes_disabled":true`) || strings.Contains(string(raw), `"h3":{`) {
		t.Fatalf("evidence on the wire: %s %v", raw, err)
	}

	// a failed HTTPS leg is the load's own failure, as for a URL without h3
	failing := &h3TestTransport{https: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, &echoStageSyntheticError{stage: "dial_tcp"}
	}), h3: h3TestContent(&udpRequests)}
	failed := udpCountryTestCheck(t, failing, true)
	if failed.OkCount != 0 || failed.Checks[0].FailureStage == FailureStageH3 || failing.h3Asked.Load() != 0 || !failed.UrlProbeEvidence.UdpProbesDisabled {
		t.Fatalf("ok=%d stage=%q h3 asked=%d; a failed HTTPS leg is judged as it is without h3",
			failed.OkCount, failed.Checks[0].FailureStage, failing.h3Asked.Load())
	}
}

// Elsewhere the switch changes nothing: the same provider and URL make the
// HTTP/3 leg, and a provider that carries no UDP fails it.
func TestUdpCountrySwitchLeavesOtherCountriesHttp3Legs(t *testing.T) {
	var udpRequests atomic.Int32
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestDropsUdp(&udpRequests)}
	result := udpCountryTestCheck(t, transport, false)
	if transport.h3Asked.Load() != 1 || udpRequests.Load() != 1 || result.OkCount != 0 ||
		UrlProbeH3Udp(result.Checks[0].H3) != UrlProbeH3UdpSilent || result.Checks[0].UdpProbesDisabled {
		t.Fatalf("h3 asked=%d udp requests=%d ok=%d udp=%q; the HTTP/3 leg must still be made and judged",
			transport.h3Asked.Load(), udpRequests.Load(), result.OkCount, UrlProbeH3Udp(result.Checks[0].H3))
	}
	if result.UrlProbeEvidence.UdpProbesDisabled || result.UrlProbeEvidence.H3 == nil {
		t.Fatalf("evidence %+v", result.UrlProbeEvidence)
	}
	// and a URL without h3 has no UDP leg to disable
	plain, err := Check(context.Background(), &http.Client{Transport: transport}, Options{
		UrlProbe: true, Destinations: h3TestDestinations(t, h3TestCatalogWithoutH3), Rand: rand.New(rand.NewSource(3)),
		UdpProbesDisabled: true,
	})
	if err != nil || plain.OkCount != 1 || plain.Checks[0].UdpProbesDisabled || plain.UrlProbeEvidence.UdpProbesDisabled {
		t.Fatalf("a URL without h3: err=%v ok=%d evidence %+v", err, plain.OkCount, plain.UrlProbeEvidence)
	}
}

// The ingest holds the flag to what the prober can have done: only an h3
// destination, and never beside an HTTP/3 leg. Without the flag, an h3 URL
// still passes only with an HTTP/3 leg that reached its site.
func TestUdpCountrySwitchEvidenceValidation(t *testing.T) {
	transport := &h3TestTransport{https: h3TestContent(nil), h3: h3TestContent(nil)}
	disabled := udpCountryTestCheck(t, transport, true).UrlProbeEvidence
	measured := udpCountryTestCheck(t, transport, false).UrlProbeEvidence
	if measured.H3 == nil || measured.H3.Udp != UrlProbeH3UdpPassed {
		t.Fatalf("the measured leg: %+v", measured.H3)
	}

	withoutH3 := *disabled
	withoutH3.Destination.H3 = false
	besideALeg := *disabled
	besideALeg.H3 = measured.H3
	notMeasured := *disabled
	notMeasured.UdpProbesDisabled = false
	for name, c := range map[string]struct {
		evidence UrlProbeEvidence
		reason   string
	}{
		"the flag on a URL without h3":            {withoutH3, "disables UDP probes"},
		"the flag beside an HTTP/3 leg":           {besideALeg, "disables UDP probes"},
		"an h3 success with neither leg nor flag": {notMeasured, "missing an HTTP/3 leg"},
	} {
		if err := c.evidence.ValidateOutcome(1, 1, false); err == nil || !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: err=%v, want it refused for %q", name, err, c.reason)
		}
	}
	for name, evidence := range map[string]UrlProbeEvidence{"the switched-off outcome": *disabled, "the measured outcome": *measured} {
		if err := evidence.ValidateOutcome(1, 1, false); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

// The pool's country settings as the prober reads them: a country turns UDP
// off only with an explicit udp_probes: false, matched in either case, and a
// pool whose keys the prober could not match is refused like any other.
func TestUdpCountrySwitchPoolLookupAndValidation(t *testing.T) {
	var pool Pool
	if err := json.Unmarshal([]byte(`{"destinations":`+h3TestCatalog+`,"country_settings":{"ir":{"udp_probes":false},"de":{"udp_probes":true},"fr":{}}}`), &pool); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePool(&pool); err != nil {
		t.Fatalf("a pool with country settings was refused: %v", err)
	}
	for country, want := range map[string]bool{"ir": true, "IR": true, " ir ": true, "de": false, "fr": false, "ru": false, "": false} {
		if got := pool.UdpProbesDisabled(country); got != want {
			t.Errorf("UdpProbesDisabled(%q) = %t, want %t", country, got, want)
		}
	}
	if (*Pool)(nil).UdpProbesDisabled("ir") {
		t.Error("no pool disables nothing")
	}
	for _, key := range []string{"IR", "irn", "i"} {
		bad := pool
		bad.CountrySettings = map[string]CountrySettings{key: pool.CountrySettings["ir"]}
		if err := ValidatePool(&bad); err == nil {
			t.Errorf("a country settings key %q was accepted", key)
		}
	}
}
