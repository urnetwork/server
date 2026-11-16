package controller

// Network estimates use the chain cadence. A signed validator result can
// arrive later without moving that cadence or selecting different trail rows.

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Delivery and retry timing do not authorize a different evidence interval.
func TestSnTopSummaryDeliveryDoesNotMoveNetworkWindow(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	anchor := &model.SnTopTempoAnchor{
		Source: model.SnTopTempoSourceChain, Block: 36_000, TimeMs: now.UnixMilli(),
		Epoch: 7, EpochStartBlock: 35_280, EpochBlocks: 50_400,
	}
	summary := &SnTopValidatorSummary{
		NativeEpoch: 100, NativeSnapshotBlock: 36_000, SettlementEpoch: 7, ReceivedAt: now,
	}
	first := snTopFoldTempo(anchor, summary, nil, now)
	delayed := *summary
	delayed.ReceivedAt = now.Add(20 * time.Minute)
	second := snTopFoldTempo(anchor, &delayed, nil, delayed.ReceivedAt)
	connect.AssertEqual(t, first.StartTime, second.StartTime)
	connect.AssertEqual(t, first.EndTime, second.EndTime)
	connect.AssertEqual(t, first.Source, model.SnTopTempoSourceChain)
}

func TestSnTopCadenceSurvivesAnchorRefreshAndLoss(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	anchor := &model.SnTopTempoAnchor{Source: model.SnTopTempoSourceChain, Block: 36_000, TimeMs: now.UnixMilli(), Epoch: 7, EpochStartBlock: 35_280, EpochBlocks: 50_400}
	priorTempo := anchor.TempoOfBlock(35_280)
	previous := &model.SnTopSnapshot{TempoSource: priorTempo.Source, TempoIndex: priorTempo.Index, Block: priorTempo.EndBlock, TempoStartTime: priorTempo.StartTime, TempoEndTime: priorTempo.EndTime}
	refreshed := *anchor
	refreshed.TimeMs = now.Add(20 * time.Minute).UnixMilli()
	refreshed.Source = model.SnTopTempoSourceStEpoch
	next := snTopFoldTempo(&refreshed, nil, previous, now.Add(time.Hour))
	connect.AssertEqual(t, next.Source, previous.TempoSource)
	connect.AssertEqual(t, next.Index, previous.TempoIndex+1)
	connect.AssertEqual(t, next.StartTime, previous.TempoEndTime)
	missing := snTopFoldTempo(nil, nil, previous, now.Add(time.Hour))
	connect.AssertEqual(t, missing.Source, previous.TempoSource)
	connect.AssertEqual(t, missing.Index, previous.TempoIndex)
	// Inconsistent epoch metadata cannot skip directly to a newer interval.
	refreshed.Epoch += 1
	invalid := snTopFoldTempo(&refreshed, nil, previous, now.Add(time.Hour))
	connect.AssertEqual(t, invalid.Index, previous.TempoIndex)
}

func TestSnTopClockCadenceDoesNotRebaseWhenChainAppears(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prior := model.SnTopCompletedTempo(nil, now.Add(-3*model.SnTopTempoDuration))
	previous := &model.SnTopSnapshot{TempoSource: prior.Source, TempoIndex: prior.Index, Block: prior.EndBlock, TempoStartTime: prior.StartTime, TempoEndTime: prior.EndTime}
	anchor := &model.SnTopTempoAnchor{Source: model.SnTopTempoSourceChain, Block: 36_000, TimeMs: now.UnixMilli(), Epoch: 7, EpochStartBlock: 35_280, EpochBlocks: 50_400}
	next := snTopFoldTempo(anchor, nil, previous, now)
	connect.AssertEqual(t, next.Source, model.SnTopTempoSourceClock)
	connect.AssertEqual(t, next.Index, prior.Index+1)
	connect.AssertEqual(t, next.StartTime, prior.EndTime)
}

func TestSnTopExpiredRecoveryEvidenceDoesNotBecomeZero(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	prior := model.SnTopCompletedTempo(nil, now.Add(-model.SnTrailHopRetention-2*model.SnTopTempoDuration))
	previous := &model.SnTopSnapshot{TempoSource: prior.Source, TempoIndex: prior.Index, TempoEndTime: prior.EndTime}
	tempo := snTopFoldTempo(nil, nil, previous, now)
	fold, reason := snTopShouldFold(tempo, previous, now)
	connect.AssertEqual(t, fold, false)
	connect.AssertEqual(t, reason, "evidence_expired")
}

func TestSnTopReplicaWireDistinguishesUnavailableFromZero(t *testing.T) {
	for _, available := range []bool{false, true} {
		wire := snTopFleetFromModel(&model.SnTopFleetRow{ReplicaAvailable: available})
		if available {
			if wire.ReplicaScore == nil || *wire.ReplicaScore != 0 {
				t.Fatalf("verified zero lost: %+v", wire)
			}
		} else if wire.ReplicaScore != nil {
			t.Fatalf("unavailable replica exposed as zero: %+v", wire)
		}
	}
}
