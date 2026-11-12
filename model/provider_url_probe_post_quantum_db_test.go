package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Which claimed turns measure post-quantum: a provider with no measured
// outcome in RefreshAge is asked on the turns whose claim ordinal is on the
// stride; a measured positive or negative outcome inside RefreshAge needs
// none; an unmeasured session, or a measured one older than RefreshAge, does
// not count. The decision is the claim's own: the same claim decides the same
// way however often it is marked.
func TestProviderUrlProbePostQuantumTurns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultProviderUrlProbePostQuantumSettings()
		now := server.NowUtc().Truncate(time.Microsecond)
		store := func(clientId server.Id, at time.Time, e2e *egresshealth.UrlProbeE2eEvidence) {
			health := fp2EvidenceTestOutcome(clientId, at, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", e2e)
			if !StoreProviderEgressHealth(ctx, health).Stored {
				t.Fatal("an outcome was not stored")
			}
		}
		never, offStride, positive, negative, unmeasured, stale := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		store(positive, now.Add(-time.Hour), &fp2EvidenceTestPositive)
		store(negative, now.Add(-2*time.Hour), &fp2EvidenceTestNegative)
		store(unmeasured, now.Add(-time.Hour), &fp2EvidenceTestUnmeasured)
		store(stale, now.Add(-settings.RefreshAge-time.Minute), &fp2EvidenceTestPositive)

		onStride := int64(settings.TurnStride) * 7
		claimed := func() []ProviderUrlProbeDue {
			return []ProviderUrlProbeDue{
				{ClientId: never, ClaimOrdinal: onStride},
				{ClientId: offStride, ClaimOrdinal: onStride + 1},
				{ClientId: positive, ClaimOrdinal: onStride},
				{ClientId: negative, ClaimOrdinal: onStride},
				{ClientId: unmeasured, ClaimOrdinal: onStride},
				{ClientId: stale, ClaimOrdinal: onStride},
			}
		}
		asked := func(providers []ProviderUrlProbeDue) map[server.Id]bool {
			out := map[server.Id]bool{}
			for _, provider := range providers {
				out[provider.ClientId] = provider.PostQuantum
			}
			return out
		}
		want := map[server.Id]bool{never: true, offStride: false, positive: false, negative: false, unmeasured: true, stale: true}
		for attempt := 0; attempt < 2; attempt++ {
			providers := claimed()
			markProviderUrlProbePostQuantumTurns(ctx, now, providers, settings)
			got := asked(providers)
			for clientId, postQuantum := range want {
				if got[clientId] != postQuantum {
					t.Fatalf("attempt %d: %s asked %t, want %t (all %v)", attempt, clientId, got[clientId], postQuantum, got)
				}
			}
		}

		// a stride of one asks every turn that needs a measurement
		every := DefaultProviderUrlProbePostQuantumSettings()
		every.TurnStride = 1
		providers := claimed()
		markProviderUrlProbePostQuantumTurns(ctx, now, providers, every)
		if got := asked(providers); !got[never] || !got[offStride] || !got[unmeasured] || !got[stale] || got[positive] || got[negative] {
			t.Fatalf("with a stride of one the claim asked %v", got)
		}
	})
}
