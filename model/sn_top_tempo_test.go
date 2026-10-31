package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Tempos are 360 blocks from each settlement epoch's start, so one never
// spans two epochs, and the index counts epoch·⌈blocks/360⌉ plus the tempos
// since the start.
func TestSnTopTempoCadenceFromTheSettlementEpoch(t *testing.T) {
	anchorTime := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	anchor := &SnTopTempoAnchor{
		Source:          SnTopTempoSourceChain,
		Block:           1_000_000,
		TimeMs:          anchorTime.UnixMilli(),
		Epoch:           7,
		EpochStartBlock: 999_000,
		EpochBlocks:     50400,
	}
	tempo := anchor.TempoOfBlock(1_000_000)
	// 1000 blocks into epoch 7: the third tempo (j = 2)
	connect.AssertEqual(t, tempo.Index, int64(7*140+2))
	connect.AssertEqual(t, tempo.StartBlock, uint64(999_720))
	connect.AssertEqual(t, tempo.EndBlock, uint64(1_000_080))
	connect.AssertEqual(t, tempo.SettlementEpoch, uint64(7))
	connect.AssertEqual(t, tempo.EndTime, anchorTime.Add(80*SnTopBlockTime))
	connect.AssertEqual(t, tempo.EndTime.Sub(tempo.StartTime), SnTopTempoDuration)

	// the last tempo of epoch 7 ends where epoch 8 starts
	last := anchor.TempoOfBlock(999_000 + 50400 - 1)
	connect.AssertEqual(t, last.Index, int64(7*140+139))
	connect.AssertEqual(t, last.EndBlock, uint64(999_000+50400))
	next := anchor.TempoOfBlock(999_000 + 50400)
	connect.AssertEqual(t, next.Index, int64(8*140))
	connect.AssertEqual(t, next.SettlementEpoch, uint64(8))

	// an earlier epoch
	earlier := anchor.TempoOfBlock(999_000 - 1)
	connect.AssertEqual(t, earlier.Index, int64(6*140+139))
	connect.AssertEqual(t, earlier.SettlementEpoch, uint64(6))

	// an epoch length that is not a multiple of 360 ends with a short tempo
	// and the indexes stay contiguous
	short := &SnTopTempoAnchor{Block: 1000, TimeMs: anchorTime.UnixMilli(), Epoch: 1, EpochStartBlock: 1000, EpochBlocks: 1000}
	tail := short.TempoOfBlock(1999)
	connect.AssertEqual(t, tail.Index, int64(1*3+2))
	connect.AssertEqual(t, tail.EndBlock-tail.StartBlock, uint64(280))
	connect.AssertEqual(t, short.TempoOfBlock(2000).Index, int64(2*3))
}

func TestSnTopCompletedTempo(t *testing.T) {
	anchorTime := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	anchor := &SnTopTempoAnchor{Block: 1_000_000, TimeMs: anchorTime.UnixMilli(), Epoch: 7, EpochStartBlock: 999_000, EpochBlocks: 50400}
	completed := SnTopCompletedTempo(anchor, anchorTime)
	connect.AssertEqual(t, completed.Index, int64(7*140+1))
	connect.AssertEqual(t, completed.EndBlock, uint64(999_720))
	connect.AssertEqual(t, completed.EndTime.Before(anchorTime), true)
	// the block clock moves with time from the anchor
	connect.AssertEqual(t, anchor.BlockAt(anchorTime.Add(time.Hour)), uint64(1_000_300))
	connect.AssertEqual(t, SnTopTempoAt(anchor, anchorTime.Add(time.Hour)).Index, int64(7*140+3))

	// without an anchor: the wall clock at 12 seconds a block
	clock := SnTopCompletedTempo(nil, anchorTime)
	connect.AssertEqual(t, clock.Source, SnTopTempoSourceClock)
	connect.AssertEqual(t, clock.EndTime.Sub(clock.StartTime), SnTopTempoDuration)
	connect.AssertEqual(t, clock.EndTime.After(anchorTime), false)
	connect.AssertEqual(t, anchorTime.Sub(clock.EndTime) < SnTopTempoDuration, true)
	connect.AssertEqual(t, SnTopTempoAt(nil, anchorTime).Index, clock.Index+1)
}
