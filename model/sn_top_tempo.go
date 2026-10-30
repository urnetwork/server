package model

// The SN Top board's tempo clock (LEADERTOP.md §8, owner decision
// 2026-10-29). A tempo is 360 subtensor blocks, about 72 minutes; the board
// folds one per tempo.
//
// The clock is anchored on the chain the server already mirrors: a block and
// its time, and the coordinator settlement epoch containing it (st_epoch:
// epoch, start block and length). Tempos are a 360-block cadence from each
// settlement epoch's start block, so a tempo never spans two settlement
// epochs and its index is epoch·⌈epoch blocks/360⌉ + tempos since the start.
// Without any chain mirror the cadence falls back to the wall clock at 12
// seconds a block. Validator summaries authorize independent sequence cuts;
// their delivery times never rebase this network-estimate cadence.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

const (
	SnTopTempoBlocks = 360
	SnTopBlockTime   = 12 * time.Second
	// a tempo's nominal length
	SnTopTempoDuration = SnTopTempoBlocks * SnTopBlockTime
	// a settlement epoch's length when the mirror does not carry it (sn
	// deploy/mainnet/policy-v1.yml: 50400 blocks, seven days)
	snTopDefaultEpochBlocks = 50400
)

const (
	// legacy receipt-time estimate source; never used for new estimates
	SnTopTempoSourceSummary = "summary"
	// the chain head the st sync reads
	SnTopTempoSourceChain = "chain"
	// the st event sync's high-water block and the mirrored epoch
	SnTopTempoSourceStEpoch = "st_epoch"
	// no chain: the wall clock
	SnTopTempoSourceClock = "clock"
)

// SnTopTempoAnchor pins the tempo cadence to the chain: a block, its time and
// the settlement epoch that contains it.
type SnTopTempoAnchor struct {
	Source          string `json:"source"`
	Block           uint64 `json:"block"`
	TimeMs          int64  `json:"time_ms"`
	Epoch           uint64 `json:"epoch"`
	EpochStartBlock uint64 `json:"epoch_start_block"`
	EpochBlocks     uint64 `json:"epoch_blocks"`
}

// SnTopTempo is one tempo: blocks [StartBlock, EndBlock), and when.
type SnTopTempo struct {
	Source          string
	Index           int64
	StartBlock      uint64
	EndBlock        uint64
	StartTime       time.Time
	EndTime         time.Time
	SettlementEpoch uint64
}

func (self *SnTopTempoAnchor) epochBlocks() uint64 {
	if self.EpochBlocks == 0 {
		return snTopDefaultEpochBlocks
	}
	return self.EpochBlocks
}

func (self *SnTopTempoAnchor) tempoTime() time.Time {
	return time.UnixMilli(self.TimeMs).UTC()
}

// BlockAt is the block produced at t, at 12 seconds a block from the anchor.
func (self *SnTopTempoAnchor) BlockAt(t time.Time) uint64 {
	offset := t.Sub(self.tempoTime()) / SnTopBlockTime
	if offset < 0 && self.Block < uint64(-offset) {
		return 0
	}
	return uint64(int64(self.Block) + int64(offset))
}

// TimeAt is when block b is produced, from the anchor.
func (self *SnTopTempoAnchor) TimeAt(b uint64) time.Time {
	return self.tempoTime().Add(time.Duration(int64(b)-int64(self.Block)) * SnTopBlockTime)
}

// TempoOfBlock is the tempo that contains block b.
func (self *SnTopTempoAnchor) TempoOfBlock(b uint64) SnTopTempo {
	epochBlocks := self.epochBlocks()
	perEpoch := (epochBlocks + SnTopTempoBlocks - 1) / SnTopTempoBlocks
	epoch, start := self.Epoch, self.EpochStartBlock
	if start <= b {
		k := (b - start) / epochBlocks
		epoch, start = epoch+k, start+k*epochBlocks
	} else {
		k := (start - b + epochBlocks - 1) / epochBlocks
		if epoch < k || start < k*epochBlocks {
			// before the first epoch: the cadence from block 0
			k = min(epoch, start/epochBlocks)
		}
		epoch, start = epoch-k, start-k*epochBlocks
		if b < start {
			start = b - b%SnTopTempoBlocks
		}
	}
	j := (b - start) / SnTopTempoBlocks
	tempoStart := start + j*SnTopTempoBlocks
	tempoEnd := min(tempoStart+SnTopTempoBlocks, start+epochBlocks)
	return SnTopTempo{
		Source:          self.Source,
		Index:           int64(epoch*perEpoch + j),
		StartBlock:      tempoStart,
		EndBlock:        tempoEnd,
		StartTime:       self.TimeAt(tempoStart),
		EndTime:         self.TimeAt(tempoEnd),
		SettlementEpoch: epoch,
	}
}

// snTopClockTempoOf is the wall-clock cadence's tempo containing t.
func snTopClockTempoOf(t time.Time) SnTopTempo {
	block := uint64(t.Unix()) / uint64(SnTopBlockTime/time.Second)
	index := block / SnTopTempoBlocks
	startBlock := index * SnTopTempoBlocks
	return SnTopTempo{
		Source:     SnTopTempoSourceClock,
		Index:      int64(index),
		StartBlock: startBlock,
		EndBlock:   startBlock + SnTopTempoBlocks,
		StartTime:  time.Unix(int64(startBlock)*int64(SnTopBlockTime/time.Second), 0).UTC(),
		EndTime:    time.Unix(int64(startBlock+SnTopTempoBlocks)*int64(SnTopBlockTime/time.Second), 0).UTC(),
	}
}

// SnTopTempoAt is the tempo in progress at t; anchor nil uses the wall clock.
func SnTopTempoAt(anchor *SnTopTempoAnchor, t time.Time) SnTopTempo {
	if anchor == nil {
		return snTopClockTempoOf(t)
	}
	return anchor.TempoOfBlock(anchor.BlockAt(t))
}

// SnTopCompletedTempo is the last tempo that ended at or before t.
func SnTopCompletedTempo(anchor *SnTopTempoAnchor, t time.Time) SnTopTempo {
	current := SnTopTempoAt(anchor, t)
	if anchor == nil {
		return snTopClockTempoOf(current.StartTime.Add(-time.Second))
	}
	if current.StartBlock == 0 {
		return current
	}
	return anchor.TempoOfBlock(current.StartBlock - 1)
}

const snTopTempoAnchorKey = "sn_top_tempo_anchor"

// an anchor outlives the task that refreshes it every few minutes
const snTopTempoAnchorTtl = 24 * time.Hour

// SetSnTopTempoAnchor shares the board's anchor with the trail hop writers.
func SetSnTopTempoAnchor(ctx context.Context, anchor *SnTopTempoAnchor) {
	anchorJson, err := json.Marshal(anchor)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, snTopTempoAnchorKey, string(anchorJson), snTopTempoAnchorTtl).Err())
	})
}

// GetSnTopTempoAnchor is nil before the board's task first ran.
func GetSnTopTempoAnchor(ctx context.Context) (anchor *SnTopTempoAnchor) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, snTopTempoAnchorKey).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		decoded := &SnTopTempoAnchor{}
		if json.Unmarshal([]byte(value), decoded) == nil && decoded.TimeMs != 0 {
			anchor = decoded
		}
	})
	return
}

// SnTopTempoIndexAt is the board's tempo index at t, from the shared anchor.
func SnTopTempoIndexAt(ctx context.Context, t time.Time) int64 {
	return SnTopTempoAt(GetSnTopTempoAnchor(ctx), t).Index
}

// GetStChainSyncAnchor is the st event sync's high-water block and when it
// was written: a block and its time for the tempo anchor when the chain RPC
// is unavailable.
func GetStChainSyncAnchor(ctx context.Context, deploymentKey StDeploymentKey) (block uint64, at time.Time, ok bool) {
	if deploymentKey == "" {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT high_water_block, update_time FROM st_chain_sync WHERE deployment_key = $1 AND singleton_id = 1`,
			string(deploymentKey),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var highWater int64
				server.Raise(result.Scan(&highWater, &at))
				if 0 < highWater {
					block, at, ok = uint64(highWater), at.UTC(), true
				}
			}
		})
	})
	return
}
