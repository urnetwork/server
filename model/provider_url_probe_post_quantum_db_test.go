package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Which claimed turns measure post-quantum: a provider with no measured
// outcome in RefreshAge is asked, one turn per RetryInterval; a measured
// positive or negative outcome inside RefreshAge needs none; an unmeasured
// session, or a measured one older than RefreshAge, does not count.
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
		never, positive, negative, unmeasured, stale := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		store(positive, now.Add(-time.Hour), &fp2EvidenceTestPositive)
		store(negative, now.Add(-2*time.Hour), &fp2EvidenceTestNegative)
		store(unmeasured, now.Add(-time.Hour), &fp2EvidenceTestUnmeasured)
		store(stale, now.Add(-settings.RefreshAge-time.Minute), &fp2EvidenceTestPositive)

		claimed := func() []ProviderUrlProbeDue {
			return []ProviderUrlProbeDue{{ClientId: never}, {ClientId: positive}, {ClientId: negative}, {ClientId: unmeasured}, {ClientId: stale}}
		}
		asked := func(providers []ProviderUrlProbeDue) map[server.Id]bool {
			out := map[server.Id]bool{}
			for _, provider := range providers {
				out[provider.ClientId] = provider.PostQuantum
			}
			return out
		}

		first := claimed()
		markProviderUrlProbePostQuantumTurns(ctx, now, first, settings)
		want := map[server.Id]bool{never: true, positive: false, negative: false, unmeasured: true, stale: true}
		if got := asked(first); len(got) != len(want) || got[never] != want[never] || got[positive] != want[positive] ||
			got[negative] != want[negative] || got[unmeasured] != want[unmeasured] || got[stale] != want[stale] {
			t.Fatalf("first claim asked %v, want %v", got, want)
		}

		// within the retry interval a provider's next turn is not asked again
		second := claimed()
		markProviderUrlProbePostQuantumTurns(ctx, now.Add(time.Minute), second, settings)
		for clientId, postQuantum := range asked(second) {
			if postQuantum {
				t.Fatalf("%s was asked again within the retry interval", clientId)
			}
		}

		// once its turn key expires, a provider that still needs a measurement
		// is asked again
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, providerUrlProbePostQuantumTurnKey(unmeasured)).Err())
		})
		third := claimed()
		markProviderUrlProbePostQuantumTurns(ctx, now.Add(2*time.Minute), third, settings)
		if got := asked(third); !got[unmeasured] || got[never] || got[positive] {
			t.Fatalf("after the interval the claim asked %v", got)
		}

		// no retry interval asks every turn that needs a measurement
		every := DefaultProviderUrlProbePostQuantumSettings()
		every.RetryInterval = 0
		fourth := claimed()
		markProviderUrlProbePostQuantumTurns(ctx, now.Add(3*time.Minute), fourth, every)
		if got := asked(fourth); !got[never] || !got[unmeasured] || !got[stale] || got[positive] || got[negative] {
			t.Fatalf("without a retry interval the claim asked %v", got)
		}
	})
}
