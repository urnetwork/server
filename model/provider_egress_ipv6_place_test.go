package model

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// One dual-stack provider's URL outcome on site at `at`: its IPv4 HTTPS leg
// passed; its IPv4 HTTP/3 leg is h3 (passed, silent, or none for ""); its IPv6
// HTTPS leg is tcp (passed, failed, not_measured) and that leg's HTTP/3 leg is
// udp6 (passed, answered, silent, or none for ""). The prober's verdict
// follows: the outcome passes only when no leg failed.
func testingIpv6UrlResult(clientId server.Id, site egresshealth.Destination, at time.Time, h3 string, tcp string, udp6 string) *ProviderEgressHealth {
	evidence := fp2TestUrlProbeEvidence(at, true)
	site.Ipv6 = true
	site.H3 = h3 != "" || udp6 != ""
	evidence.Destination = site
	evidence.Security = []egresshealth.UrlProbeSecurityEvent{{Destination: site, MeasuredAt: at, TlsAuthenticated: true}}
	evidence.IpFamily = egresshealth.IpFamilyDualstack
	passedH3 := func() *egresshealth.UrlProbeH3Evidence {
		return &egresshealth.UrlProbeH3Evidence{Udp: egresshealth.UrlProbeH3UdpPassed, ContentClassification: "content", PerformanceClassification: "insufficient_sample",
			TlsAuthenticated: true, StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
			RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000}
	}
	leg := func(udp string) *egresshealth.UrlProbeH3Evidence {
		switch udp {
		case egresshealth.UrlProbeH3UdpPassed:
			return passedH3()
		case egresshealth.UrlProbeH3UdpAnswered:
			return &egresshealth.UrlProbeH3Evidence{Udp: udp, FailureStage: "tls"}
		case egresshealth.UrlProbeH3UdpSilent:
			return &egresshealth.UrlProbeH3Evidence{Udp: udp, FailureStage: egresshealth.FailureStageDialQuic}
		}
		return nil
	}
	evidence.H3 = leg(h3)
	ipv6 := &egresshealth.UrlProbeIpv6Evidence{Tcp: tcp}
	switch tcp {
	case egresshealth.UrlProbeIpv6TcpPassed:
		ipv6 = &egresshealth.UrlProbeIpv6Evidence{Tcp: tcp, ContentClassification: "content", PerformanceClassification: "insufficient_sample",
			TlsAuthenticated: true, StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
			RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000, H3: leg(udp6)}
	case egresshealth.UrlProbeIpv6TcpFailed:
		ipv6.FailureStage = "dial_tcp"
	case egresshealth.UrlProbeIpv6TcpNotMeasured:
		ipv6.FailureStage = "dial_dns"
	}
	evidence.Ipv6 = ipv6
	ok := 1
	switch {
	case h3 == egresshealth.UrlProbeH3UdpSilent:
		ok, evidence.FailureStage = 0, egresshealth.FailureStageH3
	case tcp == egresshealth.UrlProbeIpv6TcpFailed || udp6 == egresshealth.UrlProbeH3UdpSilent:
		ok, evidence.FailureStage = 0, egresshealth.FailureStageIpv6
	}
	failed := ""
	if ok == 0 {
		failed = site.Name
	}
	return &ProviderEgressHealth{
		RunId: server.NewId(), ClientId: clientId, MeasuredAt: at, OKCount: ok, Total: 1, UrlProbeEvidence: evidence,
		ClassResults: map[string]ProviderEgressHealthClassResult{string(site.Class): {OK: ok, Total: 1}}, FailedNames: failed,
	}
}

// Place evidence in memory: the other networks that reached each URL, per
// leg, and every record and read the judgement made.
type testingLegPlaces struct {
	provider     providerEgressH3Provider
	others       map[string]int
	providerRead int
	recorded     []string
	read         []string
}

func (self *testingLegPlaces) places() providerEgressLegPlaces {
	return providerEgressLegPlaces{
		provider: func() providerEgressH3Provider { self.providerRead++; return self.provider },
		addH3: func(name string, countryCode string, _ time.Time, _ server.Id) {
			self.recorded = append(self.recorded, "h3 "+name+" "+countryCode)
		},
		countH3: func(name string, countryCode string, _ time.Time, _ *server.Id) int {
			self.read = append(self.read, "h3 "+name+" "+countryCode)
			return self.others["h3"]
		},
		addIpv6: func(name string, countryCode string, _ time.Time, transport string, _ server.Id) {
			self.recorded = append(self.recorded, "ipv6 "+transport+" "+name+" "+countryCode)
		},
		countIpv6: func(name string, countryCode string, transport string, _ time.Time, _ *server.Id) int {
			self.read = append(self.read, "ipv6 "+transport+" "+name+" "+countryCode)
			return self.others[transport]
		},
	}
}

func testingLegPlacesIn(countryCode string, others map[string]int) *testingLegPlaces {
	networkId := server.NewId()
	return &testingLegPlaces{provider: providerEgressH3Provider{countryCode: countryCode, networkId: &networkId, established: true}, others: others}
}

// An IPv6 failure counts against the provider only where established networks
// in its country reach the URL over IPv6 (and, for an HTTP/3 leg, over QUIC
// over IPv6): three of them make the place, and the failure then fails the
// outcome; fewer leave the outcome to its IPv4 legs. Each transport reads its
// own evidence, and neither verdict changes the caller's record.
func TestJudgedProviderEgressIpv6LegsByPlace(t *testing.T) {
	site := egresshealth.Destination{Name: "dual-site", Class: egresshealth.ClassSite, Url: "https://dual.example/"}
	at := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name      string
		health    *ProviderEgressHealth
		transport string
	}{
		{"IPv6 HTTPS failed", testingIpv6UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpFailed, ""), "tcp"},
		{"IPv6 HTTP/3 silent", testingIpv6UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeH3UdpSilent), "udp"},
	} {
		if err := c.health.UrlProbeEvidence.ValidateOutcome(c.health.OKCount, c.health.Total, false); err != nil {
			t.Fatalf("%s: the prober's outcome fails validation: %v", c.name, err)
		}
		for others := 0; others <= 4; others++ {
			places := testingLegPlacesIn("zz", map[string]int{c.transport: others})
			judged := judgeProviderEgressLegs(c.health, places.places())
			verdict := judged.UrlProbeEvidence.Ipv6.Verdict
			if c.transport == "udp" {
				verdict = judged.UrlProbeEvidence.Ipv6.H3.Verdict
			}
			wantVerdict, wantOk, wantFailed := egresshealth.UrlProbeH3VerdictProvider, 0, site.Name
			if others < ProviderEgressH3PlaceMinNetworks {
				wantVerdict, wantOk, wantFailed = egresshealth.UrlProbeH3VerdictPlace, 1, ""
			}
			if verdict != wantVerdict || judged.OKCount != wantOk || judged.FailedNames != wantFailed ||
				judged.ClassResults["site"] != (ProviderEgressHealthClassResult{OK: wantOk, Total: 1}) {
				t.Fatalf("%s, %d other networks: verdict=%q ok=%d failed=%q classes=%v", c.name, others, verdict, judged.OKCount, judged.FailedNames, judged.ClassResults)
			}
			if err := judged.UrlProbeEvidence.ValidateOutcome(judged.OKCount, judged.Total, false); err != nil {
				t.Fatalf("%s, %d other networks: the judged outcome fails validation: %v", c.name, others, err)
			}
			if want := []string{"ipv6 " + c.transport + " dual-site zz"}; !slices.Equal(places.read, want) {
				t.Fatalf("%s: read %v, want %v", c.name, places.read, want)
			}
		}
		if c.health.OKCount != 0 || c.health.UrlProbeEvidence.HasLegVerdict() || c.health.FailedNames != site.Name {
			t.Fatalf("%s: judging changed the caller's record", c.name)
		}
	}
}

// The outcome passes only when every failed leg was the place's: a silent
// IPv4 HTTP/3 leg and a failed IPv6 HTTPS leg are judged each by its own
// evidence, and either one judged the provider's fails the outcome.
func TestJudgedProviderEgressLegsPassOnlyWhenEveryFailureIsThePlaces(t *testing.T) {
	site := egresshealth.Destination{Name: "dual-site", Class: egresshealth.ClassSite, Url: "https://dual.example/"}
	at := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	health := testingIpv6UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpSilent, egresshealth.UrlProbeIpv6TcpFailed, "")
	for _, c := range []struct {
		h3, tcp int
		ok      int
	}{
		{0, 0, 1}, {3, 0, 0}, {0, 3, 0}, {3, 3, 0},
	} {
		judged := judgeProviderEgressLegs(health, testingLegPlacesIn("zz", map[string]int{"h3": c.h3, "tcp": c.tcp}).places())
		if judged.OKCount != c.ok {
			t.Fatalf("h3 networks %d, IPv6 networks %d: ok=%d, want %d", c.h3, c.tcp, judged.OKCount, c.ok)
		}
		if err := judged.UrlProbeEvidence.ValidateOutcome(judged.OKCount, judged.Total, false); err != nil {
			t.Fatalf("h3 networks %d, IPv6 networks %d: %v", c.h3, c.tcp, err)
		}
	}
}

// A leg that reached its site is place evidence for its own transport and
// family, recorded only for an established network in a known country; a leg
// that was not measured is neither evidence nor judged, and an outcome with
// nothing to record or judge reads nothing.
func TestJudgedProviderEgressLegsRecordPlaceEvidence(t *testing.T) {
	site := egresshealth.Destination{Name: "dual-site", Class: egresshealth.ClassSite, Url: "https://dual.example/"}
	at := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	passed := testingIpv6UrlResult(server.NewId(), site, at, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, egresshealth.UrlProbeH3UdpAnswered)
	places := testingLegPlacesIn("zz", nil)
	if judged := judgeProviderEgressLegs(passed, places.places()); judged != passed {
		t.Fatal("an outcome with nothing to judge was copied")
	}
	if want := []string{"h3 dual-site zz", "ipv6 tcp dual-site zz", "ipv6 udp dual-site zz"}; !slices.Equal(places.recorded, want) || places.providerRead == 0 {
		t.Fatalf("recorded %v, want %v", places.recorded, want)
	}
	young := testingLegPlacesIn("zz", nil)
	young.provider.established = false
	unknown := testingLegPlacesIn("", nil)
	for _, places := range []*testingLegPlaces{young, unknown} {
		judgeProviderEgressLegs(passed, places.places())
		if len(places.recorded) != 0 {
			t.Fatalf("a young network or an unknown country made place evidence: %v", places.recorded)
		}
	}
	// an unknown country has no place to compare with: its failure is the place's
	failedInUnknown := testingIpv6UrlResult(server.NewId(), site, at, "", egresshealth.UrlProbeIpv6TcpFailed, "")
	if judged := judgeProviderEgressLegs(failedInUnknown, testingLegPlacesIn("", map[string]int{"tcp": 5}).places()); judged.OKCount != 1 ||
		judged.UrlProbeEvidence.Ipv6.Verdict != egresshealth.UrlProbeH3VerdictPlace {
		t.Fatalf("unknown country: ok=%d verdict=%q", judged.OKCount, judged.UrlProbeEvidence.Ipv6.Verdict)
	}
	notMeasured := testingIpv6UrlResult(server.NewId(), site, at, "", egresshealth.UrlProbeIpv6TcpNotMeasured, "")
	quiet := testingLegPlacesIn("zz", map[string]int{"tcp": 5})
	if judged := judgeProviderEgressLegs(notMeasured, quiet.places()); judged != notMeasured || judged.OKCount != 1 ||
		quiet.providerRead != 0 || len(quiet.read) != 0 || len(quiet.recorded) != 0 {
		t.Fatalf("a leg that was not measured was judged: ok=%d reads=%v records=%v provider reads=%d", judged.OKCount, quiet.read, quiet.recorded, quiet.providerRead)
	}
}

// The ingest's judgement leaves an outcome without IPv6 legs to the HTTP/3
// judgement as it stands, and never re-judges a verdict.
func TestJudgeProviderEgressLegsLeavesOtherOutcomes(t *testing.T) {
	site := egresshealth.Destination{Name: "dual-site", Class: egresshealth.ClassSite, Url: "https://dual.example/"}
	at := time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)
	judged := testingIpv6UrlResult(server.NewId(), site, at, "", egresshealth.UrlProbeIpv6TcpFailed, "")
	judged.UrlProbeEvidence.Ipv6.Verdict = egresshealth.UrlProbeH3VerdictProvider
	if JudgeProviderEgressLegs(t.Context(), judged) != judged {
		t.Fatal("a judged outcome was judged again")
	}
	if JudgeProviderEgressLegs(t.Context(), nil) != nil {
		t.Fatal("a missing outcome came back")
	}
}
