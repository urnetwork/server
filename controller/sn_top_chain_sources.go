package controller

// The SN Top board's chain inputs, from the chain sync (owner decision
// 2026-10-29, LEADERTOP): the live claimed fleets (SnClaimedFleets), the
// reference validator's newest head summary (SnLatestHeadSummary) and the
// newest on-chain incentive census (SnLatestIncentive). Only live fleets
// count as claimed: their hotkey still holds the UID.

import (
	"context"
	"math/big"
	"sort"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// snTopFleetEpoch is the settlement epoch whose bindings the tempo folds:
// the tempo's own, or 0 when only the wall clock knows the tempo, which the
// claimed fleet read takes as the epoch the st mirror is in now.
func snTopFleetEpoch(tempo model.SnTopTempo) uint64 {
	if tempo.Source == model.SnTopTempoSourceClock {
		return 0
	}
	return tempo.SettlementEpoch
}

// snTopChainClaimedFleets lists the live claimed fleets of the settlement
// epoch, with their member clients and the networks those belong to.
func snTopChainClaimedFleets(ctx context.Context, settlementEpoch uint64) []model.SnTopFleetInput {
	var claimed *model.SnClaimedFleets
	var err error
	if settlementEpoch == 0 {
		var ok bool
		claimed, ok, err = SnClaimedFleetsNow(ctx)
		if !ok {
			return nil
		}
	} else {
		claimed, err = SnClaimedFleets(ctx, settlementEpoch)
	}
	if err != nil || claimed == nil {
		return nil
	}
	return snTopFleetInputs(claimed.LiveFleets())
}

func snTopFleetInputs(liveFleets []*model.SnClaimedFleet) []model.SnTopFleetInput {
	fleets := []model.SnTopFleetInput{}
	for _, fleet := range liveFleets {
		input := model.SnTopFleetInput{
			Key:          model.SnTopFleetKey(fleet.Key),
			ValidToEpoch: fleet.ValidToEpoch,
		}
		networks := map[server.Id]bool{}
		for _, member := range fleet.Members {
			input.ClientIds = append(input.ClientIds, member.ClientId)
			if member.NetworkId != nil && !networks[*member.NetworkId] {
				networks[*member.NetworkId] = true
				input.NetworkIds = append(input.NetworkIds, *member.NetworkId)
			}
		}
		fleets = append(fleets, input)
	}
	return fleets
}

// snTopChainValidatorSummary is the reference validator's newest head
// summary, one entry per eligible UID in rank order.
func snTopChainValidatorSummary(ctx context.Context) *SnTopValidatorSummary {
	summary, err := SnLatestHeadSummary(ctx)
	if err != nil || summary == nil {
		return nil
	}
	return snTopValidatorSummaryFromModel(summary)
}

func snTopValidatorSummaryFromModel(summary *model.SnHeadSummary) *SnTopValidatorSummary {
	out := &SnTopValidatorSummary{
		Stored:              summary,
		NativeEpoch:         summary.NativeEpoch,
		NativeSnapshotBlock: summary.NativeSnapshotBlock,
		SettlementEpoch:     summary.SettlementEpoch,
		MaximumHeadFleets:   int(summary.MaximumHeadFleets),
		EntryCount:          summary.EntryCount,
		ReceivedAt:          summary.ReceivedAt,
	}
	entries := map[uint16]*SnTopValidatorSummaryEntry{}
	for _, fleet := range summary.Fleets {
		entry := entries[fleet.Key.Uid]
		if entry == nil {
			numerator, numeratorOk := new(big.Int).SetString(fleet.ScoreNumerator, 10)
			denominator, denominatorOk := new(big.Int).SetString(fleet.ScoreDenominator, 10)
			if !numeratorOk || !denominatorOk || denominator.Sign() <= 0 {
				glog.Infof("[sntop]head summary uid %d has a malformed score\n", fleet.Key.Uid)
				continue
			}
			entry = &SnTopValidatorSummaryEntry{
				Rank:     fleet.Rank,
				Uid:      fleet.Key.Uid,
				Hotkey:   fleet.Key.Hotkey,
				Score:    new(big.Rat).SetFrac(numerator, denominator),
				Selected: fleet.Selected,
			}
			entries[fleet.Key.Uid] = entry
		}
		entry.Fleets = append(entry.Fleets, SnTopValidatorSummaryFleet{FleetId: fleet.Key.FleetId, Generation: fleet.Key.Generation})
	}
	for _, entry := range entries {
		out.Entries = append(out.Entries, *entry)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Rank < out.Entries[j].Rank })
	return out
}

// snTopChainIncentive is the newest incentive census: each UID's stored
// incentive and, where it was read, its holder.
func snTopChainIncentive(ctx context.Context) *SnTopIncentive {
	snapshot, err := SnLatestIncentive(ctx)
	if err != nil || snapshot == nil {
		return nil
	}
	return snTopIncentiveFromModel(snapshot)
}

func snTopIncentiveFromModel(snapshot *model.SnIncentiveSnapshot) *SnTopIncentive {
	incentive := &SnTopIncentive{
		At:      snapshot.ReadAt,
		ByUid:   map[uint16]uint16{},
		Holders: map[uint16][32]byte{},
		Stale:   snapshot.Stale,
	}
	for _, row := range snapshot.Uids {
		if row == nil {
			continue
		}
		incentive.ByUid[row.Uid] = row.Incentive
		if row.HolderRead {
			incentive.Holders[row.Uid] = row.Hotkey
		}
	}
	return incentive
}
