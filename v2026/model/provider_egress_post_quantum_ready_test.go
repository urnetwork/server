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
		e2e := c.e2e
		evidence, conclusive := providerModernEvidenceOfE2e(&e2e, observedAt)
		if conclusive != c.conclusive {
			t.Fatalf("%s: conclusive %t, want %t", c.name, conclusive, c.conclusive)
		}
		connect.AssertEqual(t, evidence, c.evidence)
	}
}
