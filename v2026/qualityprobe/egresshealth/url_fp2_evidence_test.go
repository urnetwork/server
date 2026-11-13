package egresshealth

import (
	"crypto/tls"
	"encoding/json"
	"strings"
	"testing"
)

// The failed later legs mask (FP2-PROTOCOL.md §3.2, §4.1) over every
// combination of the IPv4 HTTP/3 leg, the IPv6 HTTPS leg and the IPv6 HTTP/3
// leg, for a failed measured outcome: a bit for each leg the server judged the
// provider's, none for a leg judged the place's, unjudged, passed, answered
// or not measured. The legs are set on their own fields, which is all the
// mask reads; TestUrlProbeFailedLaterLegsOfJudgedFixtureEvidence covers
// evidence a check produced.
func TestUrlProbeFailedLaterLegsCoversEveryLegCombination(t *testing.T) {
	type h3Leg struct {
		name    string
		leg     *UrlProbeH3Evidence
		counted bool
	}
	h3Legs := []h3Leg{
		{name: "none", leg: nil},
		{name: "passed", leg: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpPassed}},
		{name: "answered", leg: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpAnswered}},
		{name: "silent provider", leg: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpSilent, Verdict: UrlProbeH3VerdictProvider}, counted: true},
		{name: "silent place", leg: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpSilent, Verdict: UrlProbeH3VerdictPlace}},
		{name: "silent unjudged", leg: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpSilent}},
		{name: "not measured", leg: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpNotMeasured, NotMeasured: true}},
	}
	type ipv6Leg struct {
		name    string
		tcp     string
		verdict string
		noAaaa  bool
		counted bool
		h3      bool
	}
	ipv6Legs := []ipv6Leg{
		{name: "none"},
		{name: "passed", tcp: UrlProbeIpv6TcpPassed, h3: true},
		{name: "failed provider", tcp: UrlProbeIpv6TcpFailed, verdict: UrlProbeH3VerdictProvider, counted: true},
		{name: "failed place", tcp: UrlProbeIpv6TcpFailed, verdict: UrlProbeH3VerdictPlace},
		{name: "failed unjudged", tcp: UrlProbeIpv6TcpFailed},
		{name: "not measured", tcp: UrlProbeIpv6TcpNotMeasured},
		{name: "no aaaa", tcp: UrlProbeIpv6TcpNotMeasured, noAaaa: true},
	}
	cases := 0
	for _, v4 := range h3Legs {
		for _, v6 := range ipv6Legs {
			// an IPv6 HTTP/3 leg follows only a passed IPv6 HTTPS leg
			v6H3Legs := []h3Leg{{name: "none"}}
			if v6.h3 {
				v6H3Legs = h3Legs
			}
			for _, v6H3 := range v6H3Legs {
				evidence := &UrlProbeEvidence{H3: v4.leg}
				if v6.tcp != "" {
					evidence.Ipv6 = &UrlProbeIpv6Evidence{Tcp: v6.tcp, Verdict: v6.verdict, NoAaaa: v6.noAaaa, H3: v6H3.leg}
				}
				want := int16(0)
				if v4.counted {
					want |= UrlProbeFailedLaterLegIpv4Udp
				}
				if v6.counted {
					want |= UrlProbeFailedLaterLegIpv6Tcp
				}
				if v6H3.counted {
					want |= UrlProbeFailedLaterLegIpv6Udp
				}
				name := "v4 h3 " + v4.name + ", v6 https " + v6.name + ", v6 h3 " + v6H3.name
				if got := evidence.FailedLaterLegs(0, 1); got != want {
					t.Errorf("%s: mask %d, want %d", name, got, want)
				}
				if got := evidence.FailedLaterLegs(0, 1); got&(UrlProbeFailedLaterLegIpv6Tcp|UrlProbeFailedLaterLegIpv6Udp) == UrlProbeFailedLaterLegIpv6Tcp|UrlProbeFailedLaterLegIpv6Udp {
					t.Errorf("%s: mask %d holds 2|4", name, got)
				}
				// a passing outcome, a recheck's 0/0 and an impossible total
				// never carry a mask
				for _, counts := range [][2]int{{1, 1}, {0, 0}, {0, 2}} {
					if got := evidence.FailedLaterLegs(counts[0], counts[1]); got != 0 {
						t.Errorf("%s: ok=%d total=%d has mask %d", name, counts[0], counts[1], got)
					}
				}
				cases++
			}
		}
	}
	// 7 IPv4 HTTP/3 legs by 6 IPv6 HTTPS legs with none after them, and by
	// the passed one with 7 IPv6 HTTP/3 legs
	if cases != 7*6+7*7 {
		t.Fatalf("covered %d combinations", cases)
	}
	if mask := (*UrlProbeEvidence)(nil).FailedLaterLegs(0, 1); mask != 0 {
		t.Fatalf("no evidence has mask %d", mask)
	}
}

// A failed IPv6 HTTPS leg beside a silent IPv6 HTTP/3 leg is evidence no
// prober can submit (validate refuses it); the mask still names the failed
// HTTPS leg alone, so 2|4 cannot occur whatever the record.
func TestUrlProbeFailedLaterLegsNeverHoldsBothIpv6Bits(t *testing.T) {
	evidence := &UrlProbeEvidence{Ipv6: &UrlProbeIpv6Evidence{
		Tcp: UrlProbeIpv6TcpFailed, Verdict: UrlProbeH3VerdictProvider,
		H3: &UrlProbeH3Evidence{Udp: UrlProbeH3UdpSilent, Verdict: UrlProbeH3VerdictProvider},
	}}
	if mask := evidence.FailedLaterLegs(0, 1); mask != UrlProbeFailedLaterLegIpv6Tcp {
		t.Fatalf("mask %d, want %d", mask, UrlProbeFailedLaterLegIpv6Tcp)
	}
}

// Evidence a check produced, judged as the ingest judges it: a silent IPv4
// HTTP/3 leg and a failed IPv6 HTTPS leg of a dual-stack load give 1|2 once
// both are the provider's; a silent IPv6 HTTP/3 leg after a passed IPv6 HTTPS
// leg gives 1|4 beside a silent IPv4 one; a stand-in's IPv6 legs count as the
// URL's own would; and the outcomes validate as the failures they are.
func TestUrlProbeFailedLaterLegsOfJudgedFixtureEvidence(t *testing.T) {
	judged := func(evidence *UrlProbeEvidence, verdict string) *UrlProbeEvidence {
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		var copied UrlProbeEvidence
		if err := json.Unmarshal(encoded, &copied); err != nil {
			t.Fatal(err)
		}
		if leg := copied.H3; leg != nil && leg.Udp == UrlProbeH3UdpSilent {
			leg.Verdict = verdict
		}
		if leg := copied.Ipv6; leg != nil {
			if leg.Tcp == UrlProbeIpv6TcpFailed {
				leg.Verdict = verdict
			}
			if leg.H3 != nil && leg.H3.Udp == UrlProbeH3UdpSilent {
				leg.H3.Verdict = verdict
			}
		}
		return &copied
	}
	silent := familyTestFails(FailureStageDialQuic)
	for _, c := range []struct {
		name    string
		path    *familyTestTransport
		catalog string
		want    int16
	}{
		{name: "v4 h3 silent", path: familyTestPath(familyTestContent(), familyTestContent(), silent, familyTestContent()), catalog: familyTestDualCatalog,
			want: UrlProbeFailedLaterLegIpv4Udp},
		{name: "v6 https failed", path: familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestContent()), catalog: familyTestDualCatalog,
			want: UrlProbeFailedLaterLegIpv6Tcp},
		{name: "v6 h3 silent", path: familyTestPath(familyTestContent(), familyTestContent(), familyTestContent(), silent), catalog: familyTestDualCatalog,
			want: UrlProbeFailedLaterLegIpv6Udp},
		{name: "v4 h3 silent and v6 https failed", path: familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), silent, familyTestContent()), catalog: familyTestDualCatalog,
			want: UrlProbeFailedLaterLegIpv4Udp | UrlProbeFailedLaterLegIpv6Tcp},
		{name: "v4 h3 silent and v6 h3 silent", path: familyTestPath(familyTestContent(), familyTestContent(), silent, silent), catalog: familyTestDualCatalog,
			want: UrlProbeFailedLaterLegIpv4Udp | UrlProbeFailedLaterLegIpv6Udp},
		{name: "stand-in v6 https failed", path: familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestContent()), catalog: familyTestStandInCatalog,
			want: UrlProbeFailedLaterLegIpv6Tcp},
	} {
		result := familyTestCheck(t, c.path, c.catalog, IpFamilyDualstack, false)
		if result.OkCount != 0 || result.Total != 1 {
			t.Fatalf("%s: ok=%d total=%d", c.name, result.OkCount, result.Total)
		}
		if c.catalog == familyTestStandInCatalog && (result.UrlProbeEvidence.Ipv6 == nil || result.UrlProbeEvidence.Ipv6.Destination == nil) {
			t.Fatalf("%s: the IPv6 legs loaded no stand-in", c.name)
		}
		if mask := result.UrlProbeEvidence.FailedLaterLegs(0, 1); mask != 0 {
			t.Errorf("%s: the prober's unjudged evidence has mask %d", c.name, mask)
		}
		provider := judged(result.UrlProbeEvidence, UrlProbeH3VerdictProvider)
		if err := provider.ValidateOutcome(0, 1, false); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if mask := provider.FailedLaterLegs(0, 1); mask != c.want {
			t.Errorf("%s: mask %d, want %d", c.name, mask, c.want)
		}
		if mask := judged(result.UrlProbeEvidence, UrlProbeH3VerdictPlace).FailedLaterLegs(0, 1); mask != 0 {
			t.Errorf("%s: place-judged legs have mask %d", c.name, mask)
		}
	}
}

// A country with UDP probes off makes no HTTP/3 leg, so a URL that declares
// h3 is judged on its HTTPS legs: only a failed IPv6 HTTPS leg can set a bit.
// A URL without h3 makes none either.
func TestUrlProbeFailedLaterLegsWithoutHttp3Legs(t *testing.T) {
	disabled := familyTestCheck(t, familyTestPath(familyTestContent(), familyTestFails("dial_tcp"), familyTestContent(), familyTestContent()), familyTestDualCatalog, IpFamilyDualstack, true)
	evidence := disabled.UrlProbeEvidence
	if !evidence.UdpProbesDisabled || evidence.H3 != nil || evidence.Ipv6 == nil || evidence.Ipv6.H3 != nil {
		t.Fatalf("UDP probes disabled made HTTP/3 legs: %+v", evidence)
	}
	evidence.Ipv6.Verdict = UrlProbeH3VerdictProvider
	if mask := evidence.FailedLaterLegs(0, 1); mask != UrlProbeFailedLaterLegIpv6Tcp {
		t.Fatalf("udp probes disabled: mask %d", mask)
	}
	plain := urlEvidenceTestSuccess()
	plain.ContentClassification, plain.FailureStage = "captcha", "response_content"
	if err := plain.ValidateOutcome(0, 1, false); err != nil {
		t.Fatal(err)
	}
	if mask := plain.FailedLaterLegs(0, 1); mask != 0 {
		t.Fatalf("an HTTPS failure of a URL without h3 has mask %d", mask)
	}
}

// The post-quantum key exchange names connect reports: the ML-KEM groups,
// hybrid or pure, by the name crypto/tls gives them, and nothing classical.
func TestUrlProbeKeyExchangePostQuantum(t *testing.T) {
	for _, c := range []struct {
		name        string
		postQuantum bool
	}{
		{name: "X25519MLKEM768", postQuantum: true},
		{name: tls.CurveID(0x11eb).String(), postQuantum: true},
		{name: tls.CurveID(0x11ed).String(), postQuantum: true},
		{name: tls.CurveID(0x0200).String(), postQuantum: true},
		{name: tls.CurveID(0x0201).String(), postQuantum: true},
		{name: tls.CurveID(0x0202).String(), postQuantum: true},
		{name: "X25519", postQuantum: false},
		{name: tls.CurveP256.String(), postQuantum: false},
		{name: "", postQuantum: false},
		{name: "x25519mlkem768", postQuantum: false},
	} {
		if got := UrlProbeKeyExchangePostQuantum(c.name); got != c.postQuantum {
			t.Errorf("%q: post-quantum %t, want %t", c.name, got, c.postQuantum)
		}
	}
}

// The end-to-end record's own invariants (validate): known outcomes and
// window readings, a bounded group name, a seal only with a group, positive
// only for a post-quantum session that sealed before the request, and never
// negative for one.
func TestUrlProbeE2eEvidenceValidation(t *testing.T) {
	for _, c := range []struct {
		name string
		e2e  UrlProbeE2eEvidence
		pass bool
	}{
		{name: "positive", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: UrlProbeWindowResizable}, pass: true},
		{name: "negative classical sealed", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqNegative, KeyExchange: "X25519", SealedBeforeRequest: true, Window: UrlProbeWindowFixed}, pass: true},
		{name: "negative post-quantum unsealed", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqNegative, KeyExchange: "X25519MLKEM768", Window: UrlProbeWindowResizable}, pass: true},
		{name: "negative no responder", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqNegative, Window: UrlProbeWindowFixed}, pass: true},
		{name: "unmeasured pending", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, Window: UrlProbeWindowNotTested}, pass: true},
		{name: "unmeasured sealed after the request", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, KeyExchange: "X25519MLKEM768", Window: UrlProbeWindowResizable}, pass: true},
		{name: "unknown outcome", e2e: UrlProbeE2eEvidence{Pq: "maybe", Window: UrlProbeWindowFixed}},
		{name: "no outcome", e2e: UrlProbeE2eEvidence{Window: UrlProbeWindowFixed}},
		{name: "unknown window", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, Window: "elastic"}},
		{name: "no window", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured}},
		{name: "positive classical", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqPositive, KeyExchange: "X25519", SealedBeforeRequest: true, Window: UrlProbeWindowResizable}},
		{name: "positive without a group", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqPositive, Window: UrlProbeWindowResizable}},
		{name: "positive sealed after the request", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqPositive, KeyExchange: "X25519MLKEM768", Window: UrlProbeWindowResizable}},
		{name: "negative post-quantum sealed", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqNegative, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: UrlProbeWindowResizable}},
		{name: "sealed without a group", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, SealedBeforeRequest: true, Window: UrlProbeWindowFixed}},
		{name: "group name too long", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, KeyExchange: strings.Repeat("x", 65), Window: UrlProbeWindowFixed}},
		{name: "group name not utf-8", e2e: UrlProbeE2eEvidence{Pq: UrlProbePqUnmeasured, KeyExchange: "\xff", Window: UrlProbeWindowFixed}},
	} {
		evidence := urlEvidenceTestSuccess()
		e2e := c.e2e
		evidence.E2e = &e2e
		if err := evidence.Validate(); (err == nil) != c.pass {
			t.Errorf("%s: accepted=%t want=%t: %v", c.name, err == nil, c.pass, err)
		}
	}
}

// A prober cannot claim positive without a passed IPv4 HTTPS leg: a success,
// or a failure on a later leg that only follows a passed HTTPS leg. An HTTPS
// failure, a security-only receipt and a negative outcome stand as they are.
func TestUrlProbeE2ePositiveNeedsAPassedIpv4HttpsLeg(t *testing.T) {
	positive := UrlProbeE2eEvidence{Pq: UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: UrlProbeWindowResizable}
	negative := UrlProbeE2eEvidence{Pq: UrlProbePqNegative, KeyExchange: "X25519", Window: UrlProbeWindowFixed}

	success := urlEvidenceTestSuccess()
	success.E2e = &positive
	if err := success.ValidateOutcome(1, 1, false); err != nil {
		t.Fatalf("a positive success: %v", err)
	}
	if success.ValidateOutcome(0, 0, false) == nil {
		t.Fatal("a positive security-only receipt was accepted")
	}

	failedHttps := urlEvidenceTestSuccess()
	failedHttps.ContentClassification, failedHttps.FailureStage = "captcha", "response_content"
	failedHttps.E2e = &positive
	if failedHttps.ValidateOutcome(0, 1, false) == nil {
		t.Fatal("a positive outcome whose IPv4 HTTPS leg failed was accepted")
	}
	failedHttps.E2e = &negative
	if err := failedHttps.ValidateOutcome(0, 1, false); err != nil {
		t.Fatalf("a negative outcome whose IPv4 HTTPS leg failed: %v", err)
	}

	// failed only on its silent IPv4 HTTP/3 leg: the HTTPS leg passed
	laterLeg := familyTestCheck(t, familyTestPath(familyTestContent(), familyTestContent(), familyTestFails(FailureStageDialQuic), familyTestContent()), familyTestDualCatalog, IpFamilyDualstack, false).UrlProbeEvidence
	laterLeg.E2e = &positive
	if err := laterLeg.ValidateOutcome(0, 1, false); err != nil {
		t.Fatalf("a positive outcome that failed on a later leg: %v", err)
	}
}

// The stored pq_outcome: positive and negative for a measured outcome, and
// unmeasured for no session, an unmeasured one, or no measured outcome.
func TestUrlProbePqOutcome(t *testing.T) {
	for _, c := range []struct {
		pq    string
		total int
		want  int16
	}{
		{pq: UrlProbePqPositive, total: 1, want: UrlProbePqOutcomePositive},
		{pq: UrlProbePqNegative, total: 1, want: UrlProbePqOutcomeNegative},
		{pq: UrlProbePqUnmeasured, total: 1, want: UrlProbePqOutcomeUnmeasured},
		{pq: UrlProbePqPositive, total: 0, want: UrlProbePqOutcomeUnmeasured},
		{pq: UrlProbePqNegative, total: 0, want: UrlProbePqOutcomeUnmeasured},
		{pq: "", total: 1, want: UrlProbePqOutcomeUnmeasured},
	} {
		evidence := urlEvidenceTestSuccess()
		if c.pq != "" {
			evidence.E2e = &UrlProbeE2eEvidence{Pq: c.pq, Window: UrlProbeWindowFixed}
		}
		if got := evidence.PqOutcome(c.total); got != c.want {
			t.Errorf("pq %q total %d: outcome %d, want %d", c.pq, c.total, got, c.want)
		}
	}
	if got := (*UrlProbeEvidence)(nil).PqOutcome(1); got != UrlProbePqOutcomeUnmeasured {
		t.Fatalf("no evidence: outcome %d", got)
	}
}

// A turn that opened no session sends the document a server that predates e2e
// knows, byte for byte; one that did carries the fields FP2-PROTOCOL.md §12.3
// names, and they survive the wire.
func TestUrlProbeE2eEvidenceWire(t *testing.T) {
	evidence := urlEvidenceTestSuccess()
	without, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "e2e") {
		t.Fatalf("a turn without a session sends e2e: %s", without)
	}
	evidence.E2e = &UrlProbeE2eEvidence{Pq: UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: UrlProbeWindowResizable}
	with, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	const field = `"e2e":{"pq":"positive","key_exchange":"X25519MLKEM768","sealed_before_request":true,"window":"resizable"}`
	if !strings.Contains(string(with), field) {
		t.Fatalf("e2e on the wire is %s", with)
	}
	var decoded UrlProbeEvidence
	if err := json.Unmarshal(with, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.E2e == nil || *decoded.E2e != *evidence.E2e {
		t.Fatalf("decoded e2e %+v", decoded.E2e)
	}
	if err := decoded.ValidateOutcome(1, 1, false); err != nil {
		t.Fatal(err)
	}
}
