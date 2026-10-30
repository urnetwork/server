package controller

// Recovery follows the signed measurement chain, including real scheduler gaps
// and delayed delivery, without inventing zero-score epochs or refolding retries.

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

type snTopReplicaHistoryFixture struct {
	key       model.SnTopFleetKey
	summaries []*model.SnHeadSummary
	counts    map[string]int
}

func newSnTopReplicaHistoryFixture(t *testing.T, epochs []uint64, counts []int) *snTopReplicaHistoryFixture {
	t.Helper()
	fixture := newSnTopReplicaTestFixture(t)
	history := &snTopReplicaHistoryFixture{key: model.SnTopFleetKey{FleetId: [32]byte{0x32}, Hotkey: [32]byte{0x33}, Generation: 2, Uid: 7}, counts: map[string]int{}}
	previous := ""
	for i, epoch := range epochs {
		fixture.summary.NativeEpoch = epoch
		fixture.summary.MeasurementArtifactHash = fmt.Sprintf("sha256:%064x", i+1)
		fixture.summary.PreviousMeasurementArtifactHash = previous
		fixture.summary.EvidenceEmptyPrior = previous == ""
		fixture.sign(t)
		history.summaries = append(history.summaries, fixture.stored)
		history.counts[fixture.stored.MeasurementArtifactHash] = counts[i]
		previous = fixture.stored.MeasurementArtifactHash
	}
	return history
}

func (self *snTopReplicaHistoryFixture) next(ctx context.Context, netuid uint16, hotkey [32]byte, after *uint64, through uint64) *model.SnHeadSummary {
	for _, summary := range self.summaries {
		if summary.Netuid == netuid && summary.ValidatorHotkey == hotkey && (after == nil || *after < summary.NativeEpoch) && summary.NativeEpoch <= through {
			return summary
		}
	}
	return nil
}

func (self *snTopReplicaHistoryFixture) replay(ctx context.Context, summary *model.SnHeadSummary) (map[model.SnTopFleetKey]map[[32]byte]bool, error) {
	prefixes := map[[32]byte]bool{}
	for i := 0; i < self.counts[summary.MeasurementArtifactHash]; i++ {
		prefixes[[32]byte{byte(i + 1)}] = true
	}
	return map[model.SnTopFleetKey]map[[32]byte]bool{self.key: prefixes}, nil
}

func TestSnTopReplicaRecoversEverySignedTransition(t *testing.T) {
	history := newSnTopReplicaHistoryFixture(t, []uint64{7, 8, 9}, []int{2, 0, 4})
	legacy := map[model.SnTopFleetKey]model.SnTopUnitState{history.key: {Ema: 20, EmaExact: big.NewRat(20, 1), LastTempoIndex: 2000}}
	replica, checkpoint, states, err := snTopAdvanceReplicaWithSources(t.Context(), history.summaries[2], nil, legacy, defaultSnTopReplicaHistorySettings(), history.next, history.replay)
	if err != nil || replica == nil || checkpoint.NativeEpoch != 9 || states[history.key].EmaExact.Cmp(big.NewRat(17, 8)) != 0 {
		t.Fatalf("signed history not recovered from genesis: replica=%v checkpoint=%+v states=%v err=%v", replica, checkpoint, states, err)
	}
}

func TestSnTopReplicaDoesNotInventSkippedValidatorEpochs(t *testing.T) {
	history := newSnTopReplicaHistoryFixture(t, []uint64{7, 12}, []int{4, 0})
	_, checkpoint, states, err := snTopAdvanceReplicaWithSources(t.Context(), history.summaries[1], nil, nil, defaultSnTopReplicaHistorySettings(), history.next, history.replay)
	if err != nil || checkpoint.NativeEpoch != 12 || states[history.key].EmaExact.Cmp(big.NewRat(3, 1)) != 0 {
		t.Fatalf("skipped native epochs became invented folds: checkpoint=%+v states=%v err=%v", checkpoint, states, err)
	}
}

func TestSnTopReplicaWaitsForMissingPredecessor(t *testing.T) {
	history := newSnTopReplicaHistoryFixture(t, []uint64{7, 8, 9}, []int{4, 2, 0})
	_, checkpoint, states, err := snTopAdvanceReplicaWithSources(t.Context(), history.summaries[0], nil, nil, defaultSnTopReplicaHistorySettings(), history.next, history.replay)
	if err != nil {
		t.Fatal(err)
	}
	history.summaries = []*model.SnHeadSummary{history.summaries[0], history.summaries[2]}
	replica, next, retained, err := snTopAdvanceReplicaWithSources(t.Context(), history.summaries[1], checkpoint, states, defaultSnTopReplicaHistorySettings(), history.next, history.replay)
	if err == nil || replica != nil || *next != *checkpoint || retained[history.key].EmaExact.Cmp(big.NewRat(4, 1)) != 0 {
		t.Fatalf("missing predecessor advanced state: replica=%v checkpoint=%+v states=%v err=%v", replica, next, retained, err)
	}
	// With no authenticated bootstrap, a retained later summary is unavailable.
	history.summaries = history.summaries[1:]
	replica, next, retained, err = snTopAdvanceReplicaWithSources(t.Context(), history.summaries[0], nil, states, defaultSnTopReplicaHistorySettings(), history.next, history.replay)
	if err == nil || replica != nil || next != nil || len(retained) != 0 {
		t.Fatalf("missing genesis reused approximate state: replica=%v checkpoint=%+v states=%v err=%v", replica, next, retained, err)
	}
}

func TestSnTopReplicaResumesBoundedHistoryAndDoesNotRefoldRetry(t *testing.T) {
	history := newSnTopReplicaHistoryFixture(t, []uint64{7, 8, 9}, []int{2, 0, 4})
	settings := snTopReplicaHistorySettings{MaximumTransitions: 2}
	target := history.summaries[2]
	replica, checkpoint, states, err := snTopAdvanceReplicaWithSources(t.Context(), target, nil, nil, settings, history.next, history.replay)
	if err == nil || replica != nil || checkpoint.NativeEpoch != 8 || states[history.key].EmaExact.Cmp(big.NewRat(3, 2)) != 0 {
		t.Fatalf("partial replay lost checkpoint: checkpoint=%+v states=%v err=%v", checkpoint, states, err)
	}
	replica, checkpoint, states, err = snTopAdvanceReplicaWithSources(t.Context(), target, checkpoint, states, settings, history.next, history.replay)
	if err != nil || replica == nil || checkpoint.NativeEpoch != 9 || states[history.key].EmaExact.Cmp(big.NewRat(17, 8)) != 0 {
		t.Fatalf("partial replay failed to resume: checkpoint=%+v states=%v err=%v", checkpoint, states, err)
	}
	replica, checkpoint, states, err = snTopAdvanceReplicaWithSources(t.Context(), target, checkpoint, states, settings, history.next, history.replay)
	if err != nil || replica == nil || states[history.key].EmaExact.Cmp(big.NewRat(17, 8)) != 0 {
		t.Fatalf("retry refolded measurement: checkpoint=%+v states=%v err=%v", checkpoint, states, err)
	}
}

func TestSnTopReplicaEmptyPredecessorDoesNotAuthenticateMigratedEma(t *testing.T) {
	fixture := newSnTopReplicaTestFixture(t)
	fixture.summary.EvidenceEmptyPrior = false
	fixture.sign(t)
	history := &snTopReplicaHistoryFixture{summaries: []*model.SnHeadSummary{fixture.stored}, counts: map[string]int{}}
	replica, checkpoint, states, err := snTopAdvanceReplicaWithSources(t.Context(), fixture.stored, nil, nil, defaultSnTopReplicaHistorySettings(), history.next, history.replay)
	if err == nil || replica != nil || checkpoint != nil || len(states) != 0 {
		t.Fatalf("migrated prior accepted as empty: replica=%v checkpoint=%+v states=%v err=%v", replica, checkpoint, states, err)
	}
}
