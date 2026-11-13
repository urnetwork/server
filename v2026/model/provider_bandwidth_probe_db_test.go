// The active bandwidth probe's draw against the database.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// A draw as large as the population reads every connected, valid provider
// once, and marks due exactly the active top-level public providers without a
// CDN figure or with one older than the refresh time. An operator figure
// alone does not make a provider fresh: every sample takes the CDN target,
// the operator one only where it is configured. A smaller draw keeps that
// many of the same population, and never an unbounded one.
func TestProviderBandwidthProbeSampleMarksDueProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Microsecond)
		unmeasured, fresh, stale, operatorOnly, freshToo := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		networkOnly, derived, inactive, disconnected := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, clientId := range []server.Id{unmeasured, fresh, stale, operatorOnly, freshToo} {
			capabilityProbeTestProvider(ctx, clientId, true, nil, true, true)
		}
		capabilityProbeTestProvider(ctx, networkOnly, true, nil, true, false)
		parent := server.NewId()
		capabilityProbeTestProvider(ctx, derived, true, &parent, true, true)
		capabilityProbeTestProvider(ctx, inactive, false, nil, true, true)
		capabilityProbeTestProvider(ctx, disconnected, true, nil, false, true)
		figure := func(clientId server.Id, source string, age time.Duration) {
			at := now.Add(-age)
			StoreProviderBandwidth(ctx, &ProviderBandwidth{
				ClientId:        clientId,
				BytesPerSecond:  1e6,
				Source:          source,
				SampleByteCount: 16 * 1024 * 1024,
				WindowStart:     at,
				WindowEnd:       at,
			})
		}
		figure(fresh, ProviderBandwidthSourceActiveCDN, time.Hour)
		figure(fresh, ProviderBandwidthSourceActiveOperator, time.Hour)
		figure(stale, ProviderBandwidthSourceActiveCDN, 10*24*time.Hour)
		figure(operatorOnly, ProviderBandwidthSourceActiveOperator, time.Hour)
		figure(freshToo, ProviderBandwidthSourceActiveCDN, 2*time.Hour)

		sorted := func(candidates []ProviderBandwidthProbeCandidate) []ProviderBandwidthProbeCandidate {
			candidates = slices.Clone(candidates)
			slices.SortFunc(candidates, func(a ProviderBandwidthProbeCandidate, b ProviderBandwidthProbeCandidate) int {
				return a.ClientId.Cmp(b.ClientId)
			})
			return candidates
		}
		refreshBefore := now.Add(-7 * 24 * time.Hour)
		want := sorted([]ProviderBandwidthProbeCandidate{
			{ClientId: unmeasured, Due: true},
			{ClientId: fresh, Due: false},
			{ClientId: stale, Due: true},
			{ClientId: operatorOnly, Due: true},
			{ClientId: freshToo, Due: false},
			{ClientId: networkOnly, Due: false},
			{ClientId: derived, Due: false},
			{ClientId: inactive, Due: false},
		})
		connect.AssertEqual(t, sorted(GetProviderBandwidthProbeSample(ctx, 100, refreshBefore)), want)

		// a later refresh time makes the fresh figures due too
		later := map[server.Id]bool{}
		for _, candidate := range GetProviderBandwidthProbeSample(ctx, 100, now) {
			later[candidate.ClientId] = candidate.Due
		}
		connect.AssertEqual(t, later[fresh], true)
		connect.AssertEqual(t, later[freshToo], true)
		connect.AssertEqual(t, later[networkOnly], false)

		// a smaller draw keeps that many of the population, each once
		population := map[server.Id]bool{}
		for _, candidate := range want {
			population[candidate.ClientId] = true
		}
		small := GetProviderBandwidthProbeSample(ctx, 3, refreshBefore)
		connect.AssertEqual(t, len(small), 3)
		seen := map[server.Id]bool{}
		for _, candidate := range small {
			if !population[candidate.ClientId] || seen[candidate.ClientId] {
				t.Fatalf("the draw returned %s outside the population or twice", candidate.ClientId)
			}
			seen[candidate.ClientId] = true
		}

		// a draw size outside the bounds is clamped, never unbounded
		connect.AssertEqual(t, len(GetProviderBandwidthProbeSample(ctx, 0, refreshBefore)), 1)
	})
}
