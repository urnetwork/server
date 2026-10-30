package controller

// Presence counts elapsed reliability blocks, even when a provider used more
// than one address during the same block.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Two addresses in the same half-tempo must not turn half presence into full.
func TestSnTopPresenceCountsDistinctBlocksDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		networkId, clientSession := fixture.network(t, "presence")
		defer clientSession.Cancel()
		presentUntil := fixture.start.Add(35 * time.Minute)
		clientId := fixture.provider(t, networkId, "198.51.100.9", presentUntil)
		model.AddClientReliabilityStatsRange(ctx, networkId, clientId, [32]byte{0xff}, fixture.start, presentUntil, &model.ClientReliabilityStats{
			ConnectionEstablishedCount: 1,
			ProvideEnabledCount:        1,
			ReceiveMessageCount:        1,
		})

		presence := model.GetSnTopPresence(ctx, fixture.start, fixture.end)
		connect.AssertEqual(t, presence.CoveredBlocks, int64(72))
		snTopDbNear(t, presence.Clients[clientId], 0.5)
		connect.AssertEqual(t, presence.ClientNetworks[clientId], networkId)
		connect.AssertEqual(t, presence.ProvidingNetworks[networkId], true)
	})
}

func TestSnTopPresenceOnlyHistoricalClientSurvivesCurrentIneligibilityDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		fixture.settings.CohortSize = 0
		networkId, clientSession := fixture.network(t, "historical")
		defer clientSession.Cancel()
		clientId := fixture.provider(t, networkId, "198.51.100.9", fixture.end.Add(-time.Minute))
		_, pinned := model.GetVerifyHeadPrefixPins(ctx, []server.Id{clientId}, fixture.settings)
		connect.AssertEqual(t, len(pinned), 1)
		model.RemoveVerifyEgressForClient(ctx, clientId)
		tempo := snTopTempoAnchor(ctx).TempoOfBlock(snTopDbEpochStart + 1440)
		inputs := snTopGatherInputs(ctx, fixture.settings, tempo, nil, nil)
		connect.AssertEqual(t, len(inputs.projection.Providers), 1)
		provider := inputs.projection.Providers[0]
		connect.AssertEqual(t, provider.ClientId, clientId)
		connect.AssertEqual(t, provider.Eligible, false)
		connect.AssertEqual(t, provider.HasPrefix, true)
		connect.AssertEqual(t, provider.PrefixHash, pinned[clientId])
		snTopDbNear(t, model.ProjectSnTop(inputs.projection).NetworkStates[networkId].Ema, 1)
	})
}

func TestSnTopHistoricalPinCannotBeCreatedFromCurrentAddressDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		networkId, clientSession := fixture.network(t, "pin")
		defer clientSession.Cancel()
		oldClient := fixture.provider(t, networkId, "198.51.100.9", fixture.end.Add(-time.Minute))
		_, original := model.GetVerifyHeadPrefixPins(ctx, []server.Id{oldClient}, fixture.settings)
		connect.AssertEqual(t, len(original), 1)
		model.RemoveVerifyEgressForClient(ctx, oldClient)
		model.SetStEpochSummaryCache(ctx, fixture.cfg.DeploymentKey(), &model.StEpochSummary{Epoch: snTopDbEpoch + 1, StartBlock: snTopDbEpochStart + 50400, TEpochBlocks: 50400}, time.Hour)
		currentClient := fixture.provider(t, networkId, "198.51.100.25", fixture.end.Add(-time.Minute))
		_, current := model.GetVerifyHeadPrefixPins(ctx, []server.Id{currentClient}, fixture.settings)
		connect.AssertEqual(t, len(current), 1)
		tempo := &model.SnTopTempo{Source: model.SnTopTempoSourceChain, SettlementEpoch: snTopDbEpoch, StartTime: fixture.start, EndTime: fixture.end}
		historical := model.GetSnTopPrefixPins(ctx, []server.Id{oldClient, currentClient}, fixture.settings, *tempo)
		connect.AssertEqual(t, len(historical), 1)
		connect.AssertEqual(t, historical[oldClient], original[oldClient])
		// A clock interval has no authenticated epoch mapping. Even a current
		// epoch pin and a live IP cannot be substituted for its earlier window.
		clockPins := model.GetSnTopPrefixPins(ctx, []server.Id{oldClient, currentClient}, fixture.settings, model.SnTopTempo{Source: model.SnTopTempoSourceClock, StartTime: fixture.start.Add(-model.SnTopTempoDuration), EndTime: fixture.end.Add(-model.SnTopTempoDuration)})
		connect.AssertEqual(t, len(clockPins), 0)
	})
}
