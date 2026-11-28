package controller

// A replica checkpoint follows signed measurements, rather than delivery time
// or the estimate's cadence. Missing predecessors leave the replica unavailable.

import (
	"context"
	"errors"

	"github.com/urfoundation/sn/v2026/protocol/headsummary"
	"github.com/urnetwork/server/v2026/model"
)

// A task pass bounds history recovery independently of the per-cut reader.
type snTopReplicaHistorySettings struct {
	// MaximumTransitions limits signed cuts per pass. Eight leaves room for
	// the estimate within the task's thirty-minute lifetime at the reader cap.
	// Non-positive disables advancement while retaining the checkpoint.
	MaximumTransitions int
}

func defaultSnTopReplicaHistorySettings() snTopReplicaHistorySettings {
	return snTopReplicaHistorySettings{MaximumTransitions: 8}
}

func snTopAdvanceReplica(ctx context.Context, summary *SnTopValidatorSummary, checkpoint *model.SnTopReplicaCheckpoint, prior map[model.SnTopFleetKey]model.SnTopUnitState) (*model.SnTopReplica, *model.SnTopReplicaCheckpoint, map[model.SnTopFleetKey]model.SnTopUnitState) {
	if summary == nil || summary.Stored == nil {
		return nil, checkpoint, prior
	}
	replica, next, states, _ := snTopAdvanceReplicaWithSources(ctx, summary.Stored, checkpoint, prior, defaultSnTopReplicaHistorySettings(), model.GetNextSnHeadSummary, snTopReplayHead)
	return replica, next, states
}

// A delayed summary can recover on the next task check without folding the
// network estimate twice. The model commits only fleet rows and exact state.
func snTopRefreshReplica(ctx context.Context, snapshot *model.SnTopSnapshot, summary *SnTopValidatorSummary) {
	if summary == nil || summary.Stored == nil {
		return
	}
	target := summary.Stored
	checkpoint := snapshot.ReplicaCheckpoint
	if snapshot.ReplicaComplete && checkpoint != nil && checkpoint.ValidatorHotkey == target.ValidatorHotkey && checkpoint.NativeEpoch == target.NativeEpoch && checkpoint.MeasurementHash == target.MeasurementArtifactHash {
		return
	}
	_, prior := model.GetSnTopEmaState(ctx)
	if checkpoint == nil {
		prior = nil
	}
	replica, checkpoint, states := snTopAdvanceReplica(ctx, summary, checkpoint, prior)
	fleets := model.GetSnTopSnapshotFleetInputs(ctx, snapshot.SnapshotId)
	fleets = snTopAttachChainData(fleets, summary, snTopSources.Incentive(ctx))
	projection := model.ProjectSnTop(&model.SnTopProjectionInput{Fleets: fleets, PriorFleets: states, Replica: replica})
	model.WriteSnTopReplica(ctx, snapshot.SnapshotId, checkpoint, summary.ReceivedAt, projection)
	agreement := snTopAgreement{}
	snTopMeasureReplica(&agreement, projection, summary)
	snTopReplicaAgreement.WithLabelValues("match").Set(float64(agreement.replicaMatches))
	snTopReplicaAgreement.WithLabelValues("mismatch").Set(float64(agreement.replicaMismatches))
	snTopClaimedCutoff.Set(snTopNullableGauge(projection.ClaimedCutoff))
	snTopResetLatestSnapshotCache()
}

// Each successful transition is retained for the caller's atomic commit. A
// later unavailable cut never turns missing evidence into an empty fold.
func snTopAdvanceReplicaWithSources(
	ctx context.Context,
	target *model.SnHeadSummary,
	checkpoint *model.SnTopReplicaCheckpoint,
	prior map[model.SnTopFleetKey]model.SnTopUnitState,
	settings snTopReplicaHistorySettings,
	nextSummary func(context.Context, uint16, [32]byte, *uint64, uint64) *model.SnHeadSummary,
	replay func(context.Context, *model.SnHeadSummary) (map[model.SnTopFleetKey]map[[32]byte]bool, error),
) (*model.SnTopReplica, *model.SnTopReplicaCheckpoint, map[model.SnTopFleetKey]model.SnTopUnitState, error) {
	if checkpoint == nil || checkpoint.ValidatorHotkey != target.ValidatorHotkey {
		checkpoint, prior = nil, nil
	}
	var replica *model.SnTopReplica
	for i := 0; i < settings.MaximumTransitions; i++ {
		if err := ctx.Err(); err != nil {
			return nil, checkpoint, prior, err
		}
		var stored *model.SnHeadSummary
		alreadyFolded := checkpoint != nil && checkpoint.NativeEpoch == target.NativeEpoch && checkpoint.MeasurementHash == target.MeasurementArtifactHash
		if alreadyFolded {
			stored = target
		} else {
			var after *uint64
			if checkpoint != nil {
				after = &checkpoint.NativeEpoch
			}
			stored = nextSummary(ctx, target.Netuid, target.ValidatorHotkey, after, target.NativeEpoch)
		}
		if stored == nil {
			return nil, checkpoint, prior, errors.New("head replica predecessor is unavailable")
		}
		signed, err := headsummary.Decode(stored.Body)
		if err != nil || signed.Schema != headsummary.SchemaV2 {
			return nil, checkpoint, prior, errors.New("head replica requires authenticated cut references")
		}
		if !alreadyFolded {
			previous := signed.PreviousMeasurementArtifactHash
			if (checkpoint == nil && (previous != "" || !signed.EvidenceEmptyPrior)) || (checkpoint != nil && (stored.NativeEpoch <= checkpoint.NativeEpoch || previous != checkpoint.MeasurementHash)) {
				return nil, checkpoint, prior, errors.New("head replica measurement predecessor is missing")
			}
		}
		prefixes, err := replay(ctx, stored)
		if err != nil {
			return nil, checkpoint, prior, err
		}
		alpha := signed.EvidencePolicy.Steering.HeadScoreEMA
		headMath := model.NewSnTopHeadMath(alpha.Numerator, alpha.Denominator)
		if alreadyFolded {
			replica = &model.SnTopReplica{States: prior, Raw: headMath.RawScores(prefixes), Live: map[model.SnTopFleetKey]bool{}, MaximumHeadFleets: int(signed.MaximumHeadFleets)}
			for key := range prefixes {
				replica.Live[key] = true
			}
			return replica, checkpoint, prior, nil
		}
		replica, err = model.FoldSnTopReplica(prefixes, prior, signed.NativeEpoch, int(signed.MaximumHeadFleets), headMath)
		if err != nil {
			return nil, checkpoint, prior, err
		}
		checkpoint = &model.SnTopReplicaCheckpoint{ValidatorHotkey: stored.ValidatorHotkey, NativeEpoch: signed.NativeEpoch, MeasurementHash: signed.MeasurementArtifactHash}
		prior = replica.States
		if checkpoint.NativeEpoch == target.NativeEpoch && checkpoint.MeasurementHash == target.MeasurementArtifactHash {
			return replica, checkpoint, prior, nil
		}
	}
	return nil, checkpoint, prior, errors.New("head replica history recovery is pending")
}
