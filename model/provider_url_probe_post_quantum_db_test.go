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

// A provider without the end-to-end responder, such as the July 2026.7.2 CLI
// build: its post-quantum turn's session stays unmeasured, and its
// acknowledgements advertise no receive window (window fixed). That turn
// counts as asked for RefreshAge, as a measured outcome does: the provider is
// not asked again within four hours of it, and is asked on the stride once it
// is older, about twice in each eight-hour window. Before, it was asked on
// every turn on the stride, every third turn, forever. The outcome itself is
// still stored unmeasured. Only that pair counts: an unmeasured session whose
// turn was not acknowledged (not_tested), or was acknowledged with a window
// (resizable), still leaves its provider to be asked. A provider that
// concluded keeps its cadence, and only turns on the stride are asked.
func TestProviderUrlProbePostQuantumTurnWithoutAResponderCountsAsAsked(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultProviderUrlProbePostQuantumSettings()
		now := server.NowUtc().Truncate(time.Microsecond)
		store := func(clientId server.Id, at time.Time, e2e *egresshealth.UrlProbeE2eEvidence) ProviderEgressHealthStored {
			health := fp2EvidenceTestOutcome(clientId, at, egresshealth.UrlProbeH3UdpPassed, egresshealth.UrlProbeIpv6TcpPassed, "", "", e2e)
			stored := StoreProviderEgressHealth(ctx, health)
			if !stored.Stored {
				t.Fatal("an outcome was not stored")
			}
			return stored
		}
		withoutResponder := egresshealth.UrlProbeE2eEvidence{Pq: egresshealth.UrlProbePqUnmeasured, Window: egresshealth.UrlProbeWindowFixed}
		resizable := egresshealth.UrlProbeE2eEvidence{Pq: egresshealth.UrlProbePqUnmeasured, Window: egresshealth.UrlProbeWindowResizable}
		recent, older, offStride, notTested, unsealed, positive, stalePositive := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		if stored := store(recent, now.Add(-time.Hour), &withoutResponder); stored.PqOutcome != egresshealth.UrlProbePqOutcomeUnmeasured {
			t.Fatalf("a turn without a responder stored pq_outcome %d", stored.PqOutcome)
		}
		// ordinary turns between post-quantum turns carry no session
		store(recent, now.Add(-30*time.Minute), nil)
		store(older, now.Add(-settings.RefreshAge-time.Minute), &withoutResponder)
		store(older, now.Add(-time.Hour), nil)
		store(offStride, now.Add(-time.Hour), &fp2EvidenceTestUnmeasured)
		store(notTested, now.Add(-time.Hour), &fp2EvidenceTestUnmeasured)
		store(unsealed, now.Add(-time.Hour), &resizable)
		store(positive, now.Add(-time.Hour), &fp2EvidenceTestPositive)
		store(stalePositive, now.Add(-settings.RefreshAge-time.Minute), &fp2EvidenceTestPositive)

		onStride := int64(settings.TurnStride) * 11
		providers := []ProviderUrlProbeDue{
			{ClientId: recent, ClaimOrdinal: onStride},
			{ClientId: older, ClaimOrdinal: onStride},
			{ClientId: offStride, ClaimOrdinal: onStride + 1},
			{ClientId: notTested, ClaimOrdinal: onStride},
			{ClientId: unsealed, ClaimOrdinal: onStride},
			{ClientId: positive, ClaimOrdinal: onStride},
			{ClientId: stalePositive, ClaimOrdinal: onStride},
		}
		want := map[server.Id]bool{recent: false, older: true, offStride: false, notTested: true, unsealed: true, positive: false, stalePositive: true}
		markProviderUrlProbePostQuantumTurns(ctx, now, providers, settings)
		for _, provider := range providers {
			if provider.PostQuantum != want[provider.ClientId] {
				t.Errorf("%s asked %t, want %t", provider.ClientId, provider.PostQuantum, want[provider.ClientId])
			}
		}
	})
}
