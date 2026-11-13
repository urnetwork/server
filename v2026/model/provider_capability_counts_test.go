package model

import (
	"testing"
	"time"
)

// A provider's post-quantum signal (FP2-PROTOCOL.md §12.4): positive only
// when a positive outcome is the newest, negative when a negative one is,
// a tie included, none without an outcome.
func TestProviderPostQuantumSignal(t *testing.T) {
	at := time.Date(2026, 11, 12, 16, 0, 0, 0, time.UTC)
	earlier := at.Add(-time.Hour)
	for _, c := range []struct {
		name       string
		positiveAt *time.Time
		negativeAt *time.Time
		want       string
	}{
		{name: "none", want: ProviderPostQuantumSignalNone},
		{name: "positive only", positiveAt: &at, want: ProviderPostQuantumSignalPositive},
		{name: "negative only", negativeAt: &at, want: ProviderPostQuantumSignalNegative},
		{name: "positive newest", positiveAt: &at, negativeAt: &earlier, want: ProviderPostQuantumSignalPositive},
		{name: "negative newest", positiveAt: &earlier, negativeAt: &at, want: ProviderPostQuantumSignalNegative},
		{name: "tie", positiveAt: &at, negativeAt: &at, want: ProviderPostQuantumSignalNegative},
	} {
		counts := ProviderEgressHealthCounts{PostQuantumPositiveAt: c.positiveAt, PostQuantumNegativeAt: c.negativeAt}
		if got := providerPostQuantumSignal(counts); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// The counts start with every bucket, label pair and signal at zero, and a
// provider counts once in each bucket its decision admits, under its class's
// labels and its signal.
func TestProviderEgressCountsAddCapability(t *testing.T) {
	counts := &ProviderEgressCounts{
		CapabilityCounts:  newProviderEgressCapabilityCounts(),
		PostQuantumCounts: newProviderPostQuantumCounts(),
	}
	cells := 0
	for _, bucket := range ProviderEgressBuckets {
		for _, ip := range ProviderEgressCapabilityIps {
			for _, protocol := range ProviderEgressCapabilityProtocols {
				count, ok := counts.CapabilityCounts[bucket][ip][protocol]
				if !ok || count != 0 {
					t.Fatalf("%s %s+%s missing or nonzero", bucket, ip, protocol)
				}
				cells++
			}
		}
		for _, signal := range ProviderPostQuantumSignals {
			if count, ok := counts.PostQuantumCounts[bucket][signal]; !ok || count != 0 {
				t.Fatalf("%s %s missing or nonzero", bucket, signal)
			}
		}
	}
	if cells != 12 {
		t.Fatalf("%d capability cells, want 12", cells)
	}

	// class D in speed and online
	counts.addCapability(
		providerEgressDecision{speed: true, online: true},
		providerCapabilityClassification{class: providerCapabilityClassD, downgrades: ClientScoreCapabilityDowngradeUdp},
		ProviderPostQuantumSignalPositive,
	)
	// class A in all three
	counts.addCapability(
		providerEgressDecision{quality: true, speed: true, online: true},
		providerCapabilityClassification{class: providerCapabilityClassA, advertisedDualstack: true},
		ProviderPostQuantumSignalNone,
	)
	// in no bucket: counts nowhere
	counts.addCapability(
		providerEgressDecision{reason: ProviderExcludedNoSuccess},
		providerCapabilityClassification{class: providerCapabilityClassC, advertisedDualstack: true, downgrades: ClientScoreCapabilityDowngradeUdp},
		ProviderPostQuantumSignalNegative,
	)
	for _, c := range []struct {
		bucket   string
		ip       string
		protocol string
		want     int64
	}{
		{bucket: RankModeQuality, ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolTcp, want: 0},
		{bucket: RankModeSpeed, ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolTcp, want: 1},
		{bucket: ProviderEgressBucketOnline, ip: ProviderCapabilityIpIpv4, protocol: ProviderCapabilityProtocolTcp, want: 1},
		{bucket: RankModeQuality, ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolDualproto, want: 1},
		{bucket: RankModeSpeed, ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolDualproto, want: 1},
		{bucket: ProviderEgressBucketOnline, ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolDualproto, want: 1},
		{bucket: ProviderEgressBucketOnline, ip: ProviderCapabilityIpDualstack, protocol: ProviderCapabilityProtocolTcp, want: 0},
	} {
		if got := counts.CapabilityCounts[c.bucket][c.ip][c.protocol]; got != c.want {
			t.Errorf("%s %s+%s: %d, want %d", c.bucket, c.ip, c.protocol, got, c.want)
		}
	}
	for _, c := range []struct {
		bucket string
		signal string
		want   int64
	}{
		{bucket: RankModeQuality, signal: ProviderPostQuantumSignalPositive, want: 0},
		{bucket: RankModeSpeed, signal: ProviderPostQuantumSignalPositive, want: 1},
		{bucket: ProviderEgressBucketOnline, signal: ProviderPostQuantumSignalPositive, want: 1},
		{bucket: RankModeQuality, signal: ProviderPostQuantumSignalNone, want: 1},
		{bucket: ProviderEgressBucketOnline, signal: ProviderPostQuantumSignalNegative, want: 0},
	} {
		if got := counts.PostQuantumCounts[c.bucket][c.signal]; got != c.want {
			t.Errorf("%s %s: %d, want %d", c.bucket, c.signal, got, c.want)
		}
	}
}

// A v6-only provider is never classified (FP2-PROTOCOL.md §3.6): its
// advertised labels whatever its counts. A dual-stack and a v4-only one
// classify as classifyProviderCapability does.
func TestClassifyProviderCapabilityOfFamilies(t *testing.T) {
	now := time.Now()
	counts := capabilityTestCounts(now, 10, 20)
	counts.ForgivenIpv4, counts.ForgivenTcp, counts.ForgivenBoth = 10, 10, 10
	counts.NegativeUdp, counts.NegativeIpv6 = 10, 10
	settings := DefaultEgressIndexSettings()
	v6Only := classifyProviderCapabilityOfFamilies(counts, ClientScoreIpFamilyV6, now, settings)
	if v6Only.downgraded() || v6Only.class != providerCapabilityClassB {
		t.Fatalf("v6-only classified %s downgrades %d", v6Only.class, v6Only.downgrades)
	}
	dualstack := classifyProviderCapabilityOfFamilies(counts, ClientScoreIpFamilyV4|ClientScoreIpFamilyV6, now, settings)
	if dualstack != classifyProviderCapability(counts, true, now, settings) || dualstack.class != providerCapabilityClassB {
		t.Fatalf("dual-stack classified %s", dualstack.class)
	}
	v4Only := classifyProviderCapabilityOfFamilies(counts, ClientScoreIpFamilyV4, now, settings)
	if v4Only != classifyProviderCapability(counts, false, now, settings) || v4Only.class != providerCapabilityClassD {
		t.Fatalf("v4-only classified %s", v4Only.class)
	}
	// legacy, nothing proven: v4-only
	legacy := classifyProviderCapabilityOfFamilies(counts, 0, now, settings)
	if legacy != v4Only {
		t.Fatalf("legacy classified %s", legacy.class)
	}
}
