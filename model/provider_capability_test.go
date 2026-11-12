package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Window counts with the newest run a minute before now, so the evidence is
// current.
func capabilityTestCounts(now time.Time, ok int, total int) ProviderEgressHealthCounts {
	return ProviderEgressHealthCounts{
		MeasuredAt:      now.Add(-time.Minute),
		FirstMeasuredAt: now.Add(-4 * time.Hour),
		OKCount:         ok,
		Total:           total,
	}
}

// Each class, at the exact 4/5 boundary of its forgiven runs and one forgiven
// run short of it (FP2-PROTOCOL.md §3.3, §9): at 4·T = 5·OK' the class
// passes, at 4·T = 5·OK' + 4 (one run fewer) it does not, and the provider
// moves to the next class in the decided order.
func TestClassifyProviderCapabilityRatioBoundaryPerClass(t *testing.T) {
	now := time.Now()
	settings := DefaultEgressIndexSettings()
	// 20 runs: 16 passes (OK' = 16) is the boundary, 4·20 = 5·16
	type testCase struct {
		name                string
		advertisedDualstack bool
		forgivenIpv4        int
		forgivenTcp         int
		forgivenBoth        int
		wantClass           providerCapabilityClass
		wantDowngrades      uint8
	}
	cases := []testCase{
		// 16 of 20 pass outright: never downgraded
		{name: "A at the boundary", advertisedDualstack: true, wantClass: providerCapabilityClassA},
		{name: "advertised B at the boundary", advertisedDualstack: false, wantClass: providerCapabilityClassB},
	}
	for _, c := range cases {
		counts := capabilityTestCounts(now, 16, 20)
		classification := classifyProviderCapability(counts, c.advertisedDualstack, now, settings)
		if classification.class != c.wantClass || classification.downgrades != 0 || !classification.classPassed {
			t.Errorf("%s: got %s downgrades %d passed %t", c.name, classification.class, classification.downgrades, classification.classPassed)
		}
	}

	// the strict ratio fails by one: 15 of 20
	cases = []testCase{
		{name: "B from dual-stack", advertisedDualstack: true, forgivenIpv4: 1, forgivenTcp: 0, forgivenBoth: 1, wantClass: providerCapabilityClassB, wantDowngrades: ClientScoreCapabilityDowngradeIpv6},
		{name: "C from dual-stack", advertisedDualstack: true, forgivenIpv4: 0, forgivenTcp: 1, forgivenBoth: 1, wantClass: providerCapabilityClassC, wantDowngrades: ClientScoreCapabilityDowngradeUdp},
		{name: "D from dual-stack", advertisedDualstack: true, forgivenIpv4: 0, forgivenTcp: 0, forgivenBoth: 1, wantClass: providerCapabilityClassD, wantDowngrades: ClientScoreCapabilityDowngradeIpv6 | ClientScoreCapabilityDowngradeUdp},
		{name: "D from v4-only", advertisedDualstack: false, forgivenIpv4: 0, forgivenTcp: 1, forgivenBoth: 1, wantClass: providerCapabilityClassD, wantDowngrades: ClientScoreCapabilityDowngradeUdp},
	}
	for _, c := range cases {
		counts := capabilityTestCounts(now, 15, 20)
		counts.ForgivenIpv4, counts.ForgivenTcp, counts.ForgivenBoth = c.forgivenIpv4, c.forgivenTcp, c.forgivenBoth
		classification := classifyProviderCapability(counts, c.advertisedDualstack, now, settings)
		if classification.class != c.wantClass || classification.downgrades != c.wantDowngrades || !classification.classPassed {
			t.Errorf("%s at the boundary: got %s downgrades %d passed %t", c.name, classification.class, classification.downgrades, classification.classPassed)
		}
	}

	// one forgiven run short: 14 of 20 and one forgiven run, OK' = 15, so
	// 4·20 = 5·15 + 5 and no class passes; with negative evidence on neither
	// axis the provider keeps its advertised labels
	for _, advertisedDualstack := range []bool{true, false} {
		counts := capabilityTestCounts(now, 14, 20)
		counts.ForgivenIpv4, counts.ForgivenTcp, counts.ForgivenBoth = 1, 1, 1
		classification := classifyProviderCapability(counts, advertisedDualstack, now, settings)
		if classification.classPassed || classification.downgrades != 0 {
			t.Errorf("dual-stack %t one short: got %s downgrades %d passed %t", advertisedDualstack, classification.class, classification.downgrades, classification.classPassed)
		}
	}
}

// 4·T = 5·OK' - 1 passes, and 4·T = 5·OK' + 1 does not, for the forgiving
// classes, at a total where the boundary is not a whole multiple of five.
func TestClassifyProviderCapabilityRatioBoundaryOffByOne(t *testing.T) {
	now := time.Now()
	settings := DefaultEgressIndexSettings()
	// T = 6: 4·6 = 24 = 5·5 - 1 passes with OK' = 5, and 5·4 = 20 = 24 - 4
	// fails with OK' = 4
	for _, c := range []struct {
		forgiven int
		passes   bool
	}{
		{forgiven: 2, passes: true},
		{forgiven: 1, passes: false},
	} {
		counts := capabilityTestCounts(now, 3, 6)
		counts.ForgivenTcp = c.forgiven
		counts.ForgivenBoth = c.forgiven
		classification := classifyProviderCapability(counts, false, now, settings)
		if classification.classPassed != c.passes {
			t.Errorf("forgiven %d: passed %t, want %t", c.forgiven, classification.classPassed, c.passes)
		}
		if c.passes && classification.class != providerCapabilityClassD {
			t.Errorf("forgiven %d: class %s, want D", c.forgiven, classification.class)
		}
	}
}

// A mask-4 run, UDP over IPv6, is forgiven by dropping either capability:
// the decided order drops IPv6 first, so it goes to B before C (§3.3).
func TestClassifyProviderCapabilityMaskFourGoesToBBeforeC(t *testing.T) {
	now := time.Now()
	counts := capabilityTestCounts(now, 7, 10)
	// three mask-4 runs: forgiven by B, C and D alike
	counts.ForgivenIpv4, counts.ForgivenTcp, counts.ForgivenBoth = 3, 3, 3
	counts.NegativeUdp = 3
	counts.NegativeIpv6 = 3
	classification := classifyProviderCapability(counts, true, now, DefaultEgressIndexSettings())
	if classification.class != providerCapabilityClassB || classification.downgrades != ClientScoreCapabilityDowngradeIpv6 {
		t.Fatalf("mask-4 runs classified %s downgrades %d, want B", classification.class, classification.downgrades)
	}
	reason := classification.reason(counts)
	if reason.ipv4Count != 3 || reason.tcpCount != 0 {
		t.Fatalf("mask-4 B reason counts tcp %d ipv4 %d, want 0 and 3", reason.tcpCount, reason.ipv4Count)
	}
}

// Fewer than two runs, or no current evidence, is no negative measurement:
// the advertised labels (§3.6).
func TestClassifyProviderCapabilityTooFewRunsKeepsTheAdvertisedLabels(t *testing.T) {
	now := time.Now()
	settings := DefaultEgressIndexSettings()
	for _, advertisedDualstack := range []bool{true, false} {
		counts := capabilityTestCounts(now, 0, 1)
		counts.ForgivenTcp, counts.ForgivenBoth, counts.NegativeUdp = 1, 1, 1
		classification := classifyProviderCapability(counts, advertisedDualstack, now, settings)
		if classification.downgraded() || classification.classPassed {
			t.Errorf("one run, dual-stack %t: downgraded to %s", advertisedDualstack, classification.class)
		}
		// stale evidence
		stale := capabilityTestCounts(now, 0, 5)
		stale.MeasuredAt = now.Add(-ProviderEgressHealthMaxAge)
		stale.ForgivenTcp, stale.ForgivenBoth, stale.NegativeUdp = 5, 5, 5
		classification = classifyProviderCapability(stale, advertisedDualstack, now, settings)
		if classification.downgraded() {
			t.Errorf("stale evidence, dual-stack %t: downgraded to %s", advertisedDualstack, classification.class)
		}
	}
}

// No class passes: only the axes with negative evidence drop (§3.3, §3.6),
// and a run that failed only its IPv4 HTTPS leg is evidence of neither.
func TestClassifyProviderCapabilityNoClassPassesDropsOnlyTheAxesWithEvidence(t *testing.T) {
	now := time.Now()
	settings := DefaultEgressIndexSettings()
	for _, c := range []struct {
		name                string
		advertisedDualstack bool
		negativeIp          int
		negativeUdp         int
		want                providerCapabilityClass
	}{
		{name: "dual-stack, no evidence", advertisedDualstack: true, want: providerCapabilityClassA},
		{name: "dual-stack, IPv6 evidence", advertisedDualstack: true, negativeIp: 2, want: providerCapabilityClassB},
		{name: "dual-stack, UDP evidence", advertisedDualstack: true, negativeUdp: 2, want: providerCapabilityClassC},
		{name: "dual-stack, both", advertisedDualstack: true, negativeIp: 1, negativeUdp: 1, want: providerCapabilityClassD},
		{name: "v4-only, no evidence", advertisedDualstack: false, want: providerCapabilityClassB},
		// a v4-only provider has no IPv6 label to drop
		{name: "v4-only, IPv6 evidence", advertisedDualstack: false, negativeIp: 2, want: providerCapabilityClassB},
		{name: "v4-only, UDP evidence", advertisedDualstack: false, negativeUdp: 2, want: providerCapabilityClassD},
	} {
		// 2 of 10: HTTPS failures, no class passes
		counts := capabilityTestCounts(now, 2, 10)
		counts.NegativeIp, counts.NegativeUdp = c.negativeIp, c.negativeUdp
		classification := classifyProviderCapability(counts, c.advertisedDualstack, now, settings)
		if classification.classPassed || classification.class != c.want {
			t.Errorf("%s: got %s passed %t, want %s", c.name, classification.class, classification.classPassed, c.want)
		}
		if classification.downgrades != providerCapabilityDowngradesOf(c.advertisedDualstack, c.want) {
			t.Errorf("%s: downgrades %d", c.name, classification.downgrades)
		}
	}
}

// A country whose UDP probes are off (udp_probes: false) never makes an
// HTTP/3 leg, so its failed runs carry no mask and its providers stay
// dualproto, whatever their ratio (D7.4).
func TestClassifyProviderCapabilityWithoutUdpLegsStaysDualproto(t *testing.T) {
	now := time.Now()
	for _, advertisedDualstack := range []bool{true, false} {
		classification := classifyProviderCapability(capabilityTestCounts(now, 1, 10), advertisedDualstack, now, DefaultEgressIndexSettings())
		if classification.downgrades&ClientScoreCapabilityDowngradeUdp != 0 {
			t.Errorf("dual-stack %t: UDP dropped without an HTTP/3 leg", advertisedDualstack)
		}
	}
}

// The classified counts add the class's forgiven runs to the successes,
// overall and in the general half from its own counts, and leave totals and
// times alone; a provider that is not downgraded keeps its counts.
func TestProviderCapabilityClassifiedCounts(t *testing.T) {
	now := time.Now()
	counts := capabilityTestCounts(now, 10, 20)
	counts.ForgivenTcp, counts.ForgivenBoth = 8, 8
	counts.General = &ProviderEgressHealthCounts{MeasuredAt: counts.MeasuredAt, FirstMeasuredAt: counts.FirstMeasuredAt, OKCount: 6, Total: 12, ForgivenTcp: 5, ForgivenBoth: 5}
	classification := classifyProviderCapability(counts, false, now, DefaultEgressIndexSettings())
	if classification.class != providerCapabilityClassD {
		t.Fatalf("class %s, want D", classification.class)
	}
	classified := classification.classifiedCounts(counts)
	if classified.OKCount != 18 || classified.Total != 20 || classified.General.OKCount != 11 || classified.General.Total != 12 {
		t.Fatalf("classified counts %d/%d general %d/%d", classified.OKCount, classified.Total, classified.General.OKCount, classified.General.Total)
	}
	if counts.OKCount != 10 || counts.General.OKCount != 6 {
		t.Fatal("classifying changed the strict counts")
	}
	strict := capabilityTestCounts(now, 18, 20)
	strict.ForgivenTcp = 2
	notDowngraded := classifyProviderCapability(strict, false, now, DefaultEgressIndexSettings())
	if notDowngraded.downgraded() || notDowngraded.classifiedCounts(strict).OKCount != 18 {
		t.Fatal("a provider passing the strict ratio was downgraded or given forgiven runs")
	}
}

// Labels from the record (§4.3): ip dualstack only for a proven dual-stack
// record whose IPv6 was not dropped; protocol tcp only when UDP was dropped;
// a record an older exporter wrote decodes with no downgrade.
func TestClientScoreCapabilityLabels(t *testing.T) {
	for _, c := range []struct {
		ipFamilies   uint8
		downgrades   uint8
		wantIp       string
		wantProtocol string
		wantClass    providerCapabilityClass
	}{
		{ipFamilies: ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6, wantIp: ProviderCapabilityIpDualstack, wantProtocol: ProviderCapabilityProtocolDualproto, wantClass: providerCapabilityClassA},
		{ipFamilies: ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6, downgrades: ClientScoreCapabilityDowngradeIpv6, wantIp: ProviderCapabilityIpIpv4, wantProtocol: ProviderCapabilityProtocolDualproto, wantClass: providerCapabilityClassB},
		{ipFamilies: ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6, downgrades: ClientScoreCapabilityDowngradeUdp, wantIp: ProviderCapabilityIpDualstack, wantProtocol: ProviderCapabilityProtocolTcp, wantClass: providerCapabilityClassC},
		{ipFamilies: ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6, downgrades: ClientScoreCapabilityDowngradeIpv6 | ClientScoreCapabilityDowngradeUdp, wantIp: ProviderCapabilityIpIpv4, wantProtocol: ProviderCapabilityProtocolTcp, wantClass: providerCapabilityClassD},
		{ipFamilies: ClientScoreIpFamilyV4, wantIp: ProviderCapabilityIpIpv4, wantProtocol: ProviderCapabilityProtocolDualproto, wantClass: providerCapabilityClassB},
		{ipFamilies: ClientScoreIpFamilyV4, downgrades: ClientScoreCapabilityDowngradeUdp, wantIp: ProviderCapabilityIpIpv4, wantProtocol: ProviderCapabilityProtocolTcp, wantClass: providerCapabilityClassD},
		// legacy: nothing proven reads as v4-only
		{ipFamilies: 0, wantIp: ProviderCapabilityIpIpv4, wantProtocol: ProviderCapabilityProtocolDualproto, wantClass: providerCapabilityClassB},
	} {
		score := &ClientScore{IpFamilies: c.ipFamilies, CapabilityDowngrades: c.downgrades}
		ip, protocol := score.CapabilityLabels()
		if ip != c.wantIp || protocol != c.wantProtocol || score.capabilityClass() != c.wantClass {
			t.Errorf("families %d downgrades %d: got %s+%s (%s)", c.ipFamilies, c.downgrades, ip, protocol, score.capabilityClass())
		}
	}
	if (&ClientScore{IpFamilies: ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6}).IpFamily() != connect.IpFamilyDualstack {
		t.Fatal("dual-stack families do not read as dualstack")
	}
}

// One sentence per dropped capability, tcp first, with its count; none for
// a provider that is not downgraded (D11.6, §5.5).
func TestProviderCapabilityReason(t *testing.T) {
	counts := ProviderEgressHealthCounts{NegativeUdp: 4, NegativeIpv6: 2, NegativeIp: 1}
	for _, c := range []struct {
		downgrades    uint8
		wantTcpCount  int
		wantIpv4Count int
		wantText      string
	}{
		{downgrades: 0},
		{downgrades: ClientScoreCapabilityDowngradeUdp, wantTcpCount: 4, wantText: "4 of these checks failed only over HTTP/3 (UDP), so this device is offered for TCP traffic only. Some app builds and some networks do not route UDP."},
		{downgrades: ClientScoreCapabilityDowngradeIpv6, wantIpv4Count: 2, wantText: "2 of these checks failed only over IPv6, so this device is offered for IPv4 traffic only."},
		{downgrades: ClientScoreCapabilityDowngradeUdp | ClientScoreCapabilityDowngradeIpv6, wantTcpCount: 4, wantIpv4Count: 2, wantText: "4 of these checks failed only over HTTP/3 (UDP), so this device is offered for TCP traffic only. Some app builds and some networks do not route UDP. 2 of these checks failed only over IPv6, so this device is offered for IPv4 traffic only."},
	} {
		classification := providerCapabilityClassification{advertisedDualstack: true, downgrades: c.downgrades, class: providerCapabilityClassFor(true, c.downgrades)}
		reason := classification.reason(counts)
		if reason.tcpCount != c.wantTcpCount || reason.ipv4Count != c.wantIpv4Count || reason.text != c.wantText {
			t.Errorf("downgrades %d: got %+v", c.downgrades, reason)
		}
	}
}
