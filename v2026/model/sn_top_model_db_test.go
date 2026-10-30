package model

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

func snTopTestSnapshot(computedAt time.Time, tempoIndex int64, totalRanked int) *SnTopSnapshot {
	return &SnTopSnapshot{
		SnapshotId:     server.NewId(),
		TempoIndex:     tempoIndex,
		TempoSource:    SnTopTempoSourceChain,
		Block:          uint64(tempoIndex) * SnTopTempoBlocks,
		TempoStartTime: computedAt.Add(-SnTopTempoDuration),
		TempoEndTime:   computedAt,
		ComputedAt:     computedAt,
		TotalRanked:    totalRanked,
		HeadSeats:      SnTopHeadSeats,
		OpenSeats:      SnTopHeadSeats,
		BindingNote:    "closed",
		PresenceSource: "reliability",
	}
}

// Snapshots, their rows, the EMA state and the paging reads round-trip
// through the tables, and retention keeps what the board pages.
func TestSnTopStorageDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := time.Date(2026, 10, 27, 23, 30, 0, 0, time.UTC)

		// 450 ranked networks, every third claimed, and one unranked
		networkIds := []server.Id{}
		rows := []*SnTopNetworkProjection{}
		states := map[server.Id]SnTopUnitState{}
		for i := 1; i <= 450; i += 1 {
			networkId := server.NewId()
			networkIds = append(networkIds, networkId)
			rows = append(rows, &SnTopNetworkProjection{
				NetworkId:      networkId,
				Position:       i,
				ProjectedScore: float64(1000 - i),
				Label:          SnTopLabelEstimate,
				Measurable:     true,
				VerifyEligible: true,
				ClaimPrompt:    i <= SnTopHeadSeats,
				Claimed:        i%3 == 0,
			})
			states[networkId] = SnTopUnitState{Ema: float64(1000 - i), SoloEma: 1, LastTempoIndex: 10, LastPosition: i, MeasurableBits: 0x05}
		}
		unranked := server.NewId()
		rows = append(rows, &SnTopNetworkProjection{NetworkId: unranked, Label: SnTopLabelNotMeasurable, NotMeasurableReason: SnTopReasonSharedEgress})
		fleetKey := SnTopFleetKey{FleetId: [32]byte{1}, Hotkey: [32]byte{2}, Generation: 3, Uid: 77}
		exact := big.NewRat(9, 8)
		validatorScore := 1.125
		first := snTopTestSnapshot(day, 10, 450)
		WriteSnTopSnapshot(ctx, first, &SnTopProjection{
			Networks: rows,
			Fleets: []*SnTopFleetProjection{{
				Fleet: SnTopFleetInput{
					Key:            fleetKey,
					ValidToEpoch:   12,
					NetworkIds:     []server.Id{networkIds[2], networkIds[5]},
					ValidatorScore: &validatorScore,
				},
				ReplicaScore: 1.125,
				ReplicaExact: exact,
			}},
			NetworkStates: states,
			FleetStates:   map[SnTopFleetKey]SnTopUnitState{fleetKey: {Ema: 1.125, EmaExact: exact, LastTempoIndex: 10}},
		})

		latest := GetLatestSnTopSnapshot(ctx)
		connect.AssertEqual(t, latest.SnapshotId, first.SnapshotId)
		connect.AssertEqual(t, latest.TotalRanked, 450)
		connect.AssertEqual(t, latest.ComputedAt, day)
		if latest.ClaimedCutoff != nil || latest.PrecisionAt200 != nil || latest.ValidatorSummaryAt != nil {
			t.Fatal("unset nullable snapshot fields read back as values")
		}

		// paging stops at position 400
		page := GetSnTopPage(ctx, first.SnapshotId, 0, 3, SnTopFilterAll)
		connect.AssertEqual(t, len(page), 3)
		connect.AssertEqual(t, page[0].Position, 1)
		connect.AssertEqual(t, page[2].NetworkId, networkIds[2])
		tail := GetSnTopPage(ctx, first.SnapshotId, 398, 50, SnTopFilterAll)
		connect.AssertEqual(t, len(tail), 2)
		connect.AssertEqual(t, tail[1].Position, 400)
		connect.AssertEqual(t, len(GetSnTopPage(ctx, first.SnapshotId, 400, 50, SnTopFilterAll)), 0)
		claimed := GetSnTopPage(ctx, first.SnapshotId, 0, 200, SnTopFilterClaimed)
		connect.AssertEqual(t, len(claimed), 133)
		for _, row := range claimed {
			connect.AssertEqual(t, row.Claimed, true)
		}
		unclaimed := GetSnTopPage(ctx, first.SnapshotId, 0, 2, SnTopFilterUnclaimed)
		connect.AssertEqual(t, unclaimed[0].Position, 1)
		connect.AssertEqual(t, unclaimed[1].Position, 2)
		// any position reads back for `me`, the unranked row too
		connect.AssertEqual(t, GetSnTopNetworkRow(ctx, first.SnapshotId, networkIds[449]).Position, 450)
		unrankedRow := GetSnTopNetworkRow(ctx, first.SnapshotId, unranked)
		connect.AssertEqual(t, unrankedRow.Position, 0)
		connect.AssertEqual(t, unrankedRow.NotMeasurableReason, SnTopReasonSharedEgress)

		// a fleet that binds two networks lists under both
		fleets := GetSnTopFleets(ctx, first.SnapshotId, []server.Id{networkIds[2], networkIds[5], networkIds[8]})
		connect.AssertEqual(t, len(fleets[networkIds[2]]), 1)
		connect.AssertEqual(t, len(fleets[networkIds[5]]), 1)
		connect.AssertEqual(t, len(fleets[networkIds[8]]), 0)
		fleet := fleets[networkIds[2]][0]
		connect.AssertEqual(t, fleet.Key, fleetKey)
		connect.AssertEqual(t, fleet.ValidToEpoch, uint64(12))
		connect.AssertEqual(t, *fleet.ValidatorScore, 1.125)
		connect.AssertEqual(t, fleet.ValidatorRank, (*int)(nil))
		connect.AssertEqual(t, fleet.Selected, (*bool)(nil))

		// the EMA state round-trips, the exact value exactly
		priorNetworks, priorFleets := GetSnTopEmaState(ctx)
		connect.AssertEqual(t, len(priorNetworks), 450)
		connect.AssertEqual(t, priorNetworks[networkIds[0]], states[networkIds[0]])
		connect.AssertEqual(t, priorFleets[fleetKey].EmaExact.Cmp(exact), 0)

		// the next day's first snapshot records the previous day's ranks, and
		// replaces the EMA state: units it did not fold are dropped
		second := snTopTestSnapshot(day.Add(time.Hour), 11, 2)
		WriteSnTopSnapshot(ctx, second, &SnTopProjection{
			Networks: []*SnTopNetworkProjection{
				{NetworkId: networkIds[1], Position: 1, ProjectedScore: 5, Label: SnTopLabelEstimate},
				{NetworkId: networkIds[0], Position: 2, ProjectedScore: 4, Label: SnTopLabelEstimate},
			},
			NetworkStates: map[server.Id]SnTopUnitState{
				networkIds[1]: {Ema: 5, LastTempoIndex: 11, LastPosition: 1},
				networkIds[0]: {Ema: 4, LastTempoIndex: 11, LastPosition: 2},
			},
		})
		priorNetworks, priorFleets = GetSnTopEmaState(ctx)
		connect.AssertEqual(t, len(priorNetworks), 2)
		connect.AssertEqual(t, len(priorFleets), 0)
		history := GetSnTopNetworkHistory(ctx, networkIds[0], day.AddDate(0, 0, -5))
		connect.AssertEqual(t, len(history), 1)
		connect.AssertEqual(t, history[0].Day, time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC))
		connect.AssertEqual(t, history[0].Rank, 1)
		connect.AssertEqual(t, history[0].Score, 999.0)
		// the unranked row is never history
		connect.AssertEqual(t, len(GetSnTopNetworkHistory(ctx, unranked, day.AddDate(0, 0, -5))), 0)
		// a second snapshot the same day records nothing more
		third := snTopTestSnapshot(day.Add(2*time.Hour), 12, 0)
		WriteSnTopSnapshot(ctx, third, &SnTopProjection{})
		connect.AssertEqual(t, len(GetSnTopNetworkHistory(ctx, networkIds[0], day.AddDate(0, 0, -5))), 1)

		// retention: the oldest snapshot keeps only its paged top 400 once two
		// newer ones exist
		RemoveOldSnTopData(ctx, day.Add(3*time.Hour))
		connect.AssertEqual(t, GetSnTopNetworkRow(ctx, first.SnapshotId, networkIds[449]), (*SnTopNetworkRow)(nil))
		connect.AssertEqual(t, GetSnTopNetworkRow(ctx, first.SnapshotId, unranked), (*SnTopNetworkRow)(nil))
		connect.AssertEqual(t, GetSnTopNetworkRow(ctx, first.SnapshotId, networkIds[399]).Position, 400)
		// and everything past seven days goes, daily history past 90
		RemoveOldSnTopData(ctx, day.Add(8*24*time.Hour))
		connect.AssertEqual(t, GetSnTopSnapshot(ctx, first.SnapshotId), (*SnTopSnapshot)(nil))
		connect.AssertEqual(t, GetLatestSnTopSnapshot(ctx), (*SnTopSnapshot)(nil))
		connect.AssertEqual(t, len(GetSnTopPage(ctx, first.SnapshotId, 0, 10, SnTopFilterAll)), 0)
		connect.AssertEqual(t, len(GetSnTopFleets(ctx, first.SnapshotId, []server.Id{networkIds[2]})), 0)
		connect.AssertEqual(t, len(GetSnTopNetworkHistory(ctx, networkIds[0], day.AddDate(0, 0, -5))), 1)
		RemoveOldSnTopData(ctx, day.AddDate(0, 0, 92))
		connect.AssertEqual(t, len(GetSnTopNetworkHistory(ctx, networkIds[0], day.AddDate(0, 0, -5))), 0)
	})
}

// Trail hops: written once per hop however often a completion replays, read
// back by time with the seed hops left out, and retained seven days.
func TestSnTrailHopsDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
		networkId := server.NewId()
		seedClient, hopClient, lateClient := server.NewId(), server.NewId(), server.NewId()
		trail := &VerifyTrail{
			TrailId: server.NewId(),
			Hops: []*VerifyTrailHop{
				{ClientId: seedClient, Seed: true, ConfirmedMs: uint64(start.Add(time.Minute).UnixMilli()), EgressIpHash: [32]byte{1}},
				{ClientId: hopClient, NetworkId: &networkId, ConfirmedMs: uint64(start.Add(2 * time.Minute).UnixMilli()), EgressIpHash: [32]byte{2}},
				{ClientId: hopClient, NetworkId: &networkId, ConfirmedMs: uint64(start.Add(3 * time.Minute).UnixMilli()), EgressIpHash: [32]byte{2}},
				{ClientId: lateClient, ConfirmedMs: uint64(start.Add(2 * time.Hour).UnixMilli()), EgressIpHash: [32]byte{3}},
			},
		}
		hops := snTrailHopsOfCompleteTrail(trail, "e7", func(t time.Time) int64 { return t.Unix() / 4320 })
		connect.AssertEqual(t, len(hops), 4)
		connect.AssertEqual(t, hops[0].Seed, true)
		connect.AssertEqual(t, hops[1].HopIndex, 1)
		connect.AssertEqual(t, hops[1].HeadWindow, "e7")
		Testing_InsertSnTrailHops(ctx, hops)
		// a replay of the completion writes nothing twice
		Testing_InsertSnTrailHops(ctx, hops)

		evidence := GetSnTopTrailEvidence(ctx, start, start.Add(time.Hour))
		connect.AssertEqual(t, evidence.Hops, 2)
		connect.AssertEqual(t, len(evidence.Prefixes), 1)
		// one prefix, counted once
		connect.AssertEqual(t, evidence.Prefixes[hopClient], [][32]byte{{2}})
		connect.AssertEqual(t, evidence.Networks[hopClient], networkId)
		_, seedCounted := evidence.Prefixes[seedClient]
		connect.AssertEqual(t, seedCounted, false)

		RemoveOldSnTopData(ctx, start.Add(SnTrailHopRetention+time.Hour))
		connect.AssertEqual(t, GetSnTopTrailEvidence(ctx, start, start.Add(3*time.Hour)).Hops, 1)
	})
}
