package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// The modern evidence a URL turn's end-to-end observation records, pure: only
// the turn's positive outcome is post-quantum. A post-quantum group that
// never sealed is a negative outcome (FP2-PROTOCOL.md §12.2), and records
// its group and window but not post-quantum, so the evidence strict
// selection reads while the post-quantum marker is down counts only a
// positive quality probe handshake (owner, 2026-11-13; §12.8). An
// inconclusive session records nothing.
func TestProviderModernEvidenceOfE2eIsPostQuantumOnlyWhenPositive(t *testing.T) {
	observedAt := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name       string
		e2e        egresshealth.UrlProbeE2eEvidence
		conclusive bool
		evidence   ProviderModernEvidence
	}{
		{
			name: "positive",
			e2e: egresshealth.UrlProbeE2eEvidence{
				Pq: egresshealth.UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: egresshealth.UrlProbeWindowResizable,
			},
			conclusive: true,
			evidence: ProviderModernEvidence{
				E2eNegotiated: true, KeyExchange: "X25519MLKEM768", PostQuantum: true, WindowAdvertised: true, ObservedAt: observedAt,
			},
		},
		{
			name: "a post-quantum group that never sealed",
			e2e: egresshealth.UrlProbeE2eEvidence{
				Pq: egresshealth.UrlProbePqNegative, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: false, Window: egresshealth.UrlProbeWindowResizable,
			},
			conclusive: true,
			evidence: ProviderModernEvidence{
				E2eNegotiated: true, KeyExchange: "X25519MLKEM768", PostQuantum: false, WindowAdvertised: true, ObservedAt: observedAt,
			},
		},
		{
			name: "a classical group",
			e2e: egresshealth.UrlProbeE2eEvidence{
				Pq: egresshealth.UrlProbePqNegative, KeyExchange: "X25519", SealedBeforeRequest: true, Window: egresshealth.UrlProbeWindowFixed,
			},
			conclusive: true,
			evidence: ProviderModernEvidence{
				E2eNegotiated: true, KeyExchange: "X25519", PostQuantum: false, WindowAdvertised: false, ObservedAt: observedAt,
			},
		},
		{
			name: "no responder",
			e2e: egresshealth.UrlProbeE2eEvidence{
				Pq: egresshealth.UrlProbePqNegative, Window: egresshealth.UrlProbeWindowFixed,
			},
			conclusive: true,
			evidence: ProviderModernEvidence{
				E2eNegotiated: false, PostQuantum: false, WindowAdvertised: false, ObservedAt: observedAt,
			},
		},
		{
			name: "unmeasured",
			e2e: egresshealth.UrlProbeE2eEvidence{
				Pq: egresshealth.UrlProbePqUnmeasured, KeyExchange: "X25519MLKEM768", Window: egresshealth.UrlProbeWindowResizable,
			},
			conclusive: false,
		},
		{
			name: "positive with no acknowledgement read",
			e2e: egresshealth.UrlProbeE2eEvidence{
				Pq: egresshealth.UrlProbePqPositive, KeyExchange: "X25519MLKEM768", SealedBeforeRequest: true, Window: egresshealth.UrlProbeWindowNotTested,
			},
			conclusive: false,
		},
	} {
		// none of these is a sealed turn, so the provider's country is never
		// asked and decides nothing
		for _, exempt := range []bool{false, true} {
			e2e := c.e2e
			asked := 0
			evidence, conclusive := providerModernEvidenceOfE2e(&egresshealth.UrlProbeEvidence{MeasuredAt: observedAt, RequestWritten: true, E2e: &e2e}, func() bool {
				asked += 1
				return exempt
			})
			if conclusive != c.conclusive {
				t.Fatalf("%s: conclusive %t, want %t", c.name, conclusive, c.conclusive)
			}
			connect.AssertEqual(t, evidence, c.evidence)
			connect.AssertEqual(t, asked, 0)
		}
	}
}

// A turn whose post-quantum session sealed while its IPv4 HTTPS leg failed
// before writing a request is post-quantum evidence only for a provider whose
// published country the URL-success guard exempts, with the window it read
// (owner, 2026-11-24). The same turn in any other country, an unsealed turn,
// a sealed turn that wrote its request, one with no acknowledgement read and
// one on a classical group record nothing; only a sealed turn without a
// request asks for the country.
func TestProviderModernEvidenceOfASealedTurnIsPostQuantumOnlyInAnExemptCountry(t *testing.T) {
	observedAt := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	sealed := egresshealth.UrlProbeE2eEvidence{
		Pq: egresshealth.UrlProbePqUnmeasured, KeyExchange: "X25519MLKEM768", Window: egresshealth.UrlProbeWindowResizable, Sealed: true,
	}
	sealedFixed := sealed
	sealedFixed.Window = egresshealth.UrlProbeWindowFixed
	unsealed := sealed
	unsealed.Sealed = false
	sealedUnacknowledged := sealed
	sealedUnacknowledged.Window = egresshealth.UrlProbeWindowNotTested
	sealedClassical := sealed
	sealedClassical.KeyExchange = "X25519"
	for _, c := range []struct {
		name           string
		e2e            egresshealth.UrlProbeE2eEvidence
		requestWritten bool
		exempt         bool
		asked          bool
		conclusive     bool
		evidence       ProviderModernEvidence
	}{
		{
			name: "sealed without a request, exempt country", e2e: sealed, exempt: true, asked: true, conclusive: true,
			evidence: ProviderModernEvidence{E2eNegotiated: true, KeyExchange: "X25519MLKEM768", PostQuantum: true, WindowAdvertised: true, ObservedAt: observedAt},
		},
		{
			name: "sealed without a request, fixed window, exempt country", e2e: sealedFixed, exempt: true, asked: true, conclusive: true,
			evidence: ProviderModernEvidence{E2eNegotiated: true, KeyExchange: "X25519MLKEM768", PostQuantum: true, WindowAdvertised: false, ObservedAt: observedAt},
		},
		{name: "sealed without a request, country not exempt", e2e: sealed, asked: true},
		{name: "unsealed without a request, exempt country", e2e: unsealed, exempt: true},
		{name: "unsealed without a request, country not exempt", e2e: unsealed},
		{name: "sealed with its request written, exempt country", e2e: sealed, requestWritten: true, exempt: true},
		{name: "sealed without a request, no acknowledgement read, exempt country", e2e: sealedUnacknowledged, exempt: true},
		{name: "sealed classical without a request, exempt country", e2e: sealedClassical, exempt: true},
	} {
		e2e := c.e2e
		asked := 0
		evidence, conclusive := providerModernEvidenceOfE2e(&egresshealth.UrlProbeEvidence{MeasuredAt: observedAt, RequestWritten: c.requestWritten, E2e: &e2e}, func() bool {
			asked += 1
			return c.exempt
		})
		if conclusive != c.conclusive {
			t.Fatalf("%s: conclusive %t, want %t", c.name, conclusive, c.conclusive)
		}
		connect.AssertEqual(t, evidence, c.evidence)
		if (0 < asked) != c.asked || 1 < asked {
			t.Fatalf("%s: asked for the country %d times, want asked %t", c.name, asked, c.asked)
		}
	}
	// no session at all records nothing
	if _, conclusive := providerModernEvidenceOfE2e(&egresshealth.UrlProbeEvidence{MeasuredAt: observedAt}, func() bool { return true }); conclusive {
		t.Fatal("a turn without a session recorded evidence")
	}
}

// The guard lists a country as exempt as GetProviderOnlineSuccessExemptCountries
// does: only a trusted guard the export applied, for a country it measured
// and exempted. A missing or disabled guard, an unmeasured country and one
// with too many successes are not listed, though the rule's own reading
// (exempt) keeps the providers of a country without a guard or a
// measurement.
func TestProviderOnlineSuccessGuardListsOnlyMeasuredExemptCountries(t *testing.T) {
	guard := &providerOnlineSuccessBaseline{
		computeTime: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
		enabled:     true,
		floor:       0.36,
		countryCodeCounts: map[string]providerOnlineSuccessCount{
			"ru": {Measured: 5, Succeeded: 1, NoSuccess: 4, Exempt: true},
			"us": {Measured: 10, Succeeded: 9, NoSuccess: 1},
		},
	}
	connect.AssertEqual(t, guard.listsExempt("ru"), true)
	connect.AssertEqual(t, guard.listsExempt("us"), false)
	connect.AssertEqual(t, guard.listsExempt("kz"), false)
	connect.AssertEqual(t, guard.exempt("kz"), true)
	connect.AssertEqual(t, guard.listsExempt(""), false)

	disabled := *guard
	disabled.enabled = false
	connect.AssertEqual(t, disabled.listsExempt("ru"), false)

	var none *providerOnlineSuccessBaseline
	connect.AssertEqual(t, none.listsExempt("ru"), false)
	connect.AssertEqual(t, none.exempt("ru"), true)

	// every country the gauge lists, and no other
	listed := map[string]bool{}
	for _, country := range guard.exemptCountries() {
		listed[country.CountryCode] = true
	}
	for _, countryCode := range []string{"ru", "us", "kz", ""} {
		connect.AssertEqual(t, guard.listsExempt(countryCode), listed[countryCode])
	}
}
