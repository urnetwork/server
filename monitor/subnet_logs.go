// SIGNALS.md §22.0 bounded incremental finalized-log census. A probe-owned
// frontier advances only through finalized blocks it actually read; each run
// reads at most the configured chunks, so a backlog stays an explicit
// incomplete frontier instead of an unbounded eth_getLogs scan. The first run
// starts at the later of the deployment block and the reviewed initial
// window; earlier history is not replayed, and readers say so.
package monitor

import (
	"context"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

// The frontier is the next block to read; zero means not yet started.
type subnetLogFrontier struct {
	stateLock sync.Mutex
	next      uint64
	origin    uint64
}

// One planned read: an inclusive range and whether it reaches the checkpoint.
type subnetLogPlan struct {
	from      uint64
	to        uint64
	empty     bool
	complete  bool
	truncated bool
}

func newSubnetLogFrontier() *subnetLogFrontier {
	return &subnetLogFrontier{}
}

// Plans this run's range. A checkpoint below the frontier reads nothing.
func (self *subnetLogFrontier) plan(checkpoint, deployBlock uint64, limits subnetLimits) subnetLogPlan {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	start := self.next
	truncated := false
	if start == 0 {
		start = deployBlock
		if checkpoint >= limits.initialLogWindowBlocks && start < checkpoint-limits.initialLogWindowBlocks+1 {
			start = checkpoint - limits.initialLogWindowBlocks + 1
			truncated = true
		}
		self.origin = start
	} else {
		truncated = self.origin > deployBlock
	}
	if start > checkpoint {
		return subnetLogPlan{empty: true, complete: true, truncated: truncated}
	}
	span := limits.logChunkBlocks * uint64(limits.maxLogRequests)
	end := checkpoint
	complete := true
	if end-start+1 > span {
		end = start + span - 1
		complete = false
	}
	return subnetLogPlan{from: start, to: end, complete: complete, truncated: truncated}
}

// The first block the frontier read, or zero before its first plan.
func (self *subnetLogFrontier) originBlock() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.origin
}

// Records that every block through to was read.
func (self *subnetLogFrontier) advance(to uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if to+1 > self.next {
		self.next = to + 1
	}
}

// Reads one address's logs over the plan in configured chunks. A chunk at the
// reply limit is halved; a single block over the limit stays unavailable.
func subnetFetchLogs(ctx context.Context, reader subnetEvmReader, address common.Address, topics [][]common.Hash, plan subnetLogPlan, limits subnetLimits) ([]ethtypes.Log, error) {
	if plan.empty {
		return nil, nil
	}
	logs := []ethtypes.Log{}
	var fetch func(from, to uint64) error
	fetch = func(from, to uint64) error {
		part, err := reader.logs(ctx, address, topics, from, to)
		var readErr *subnetReadError
		if err != nil && errors.As(err, &readErr) && readErr.reason == "logs-overflow" && from < to {
			middle := from + (to-from)/2
			if err := fetch(from, middle); err != nil {
				return err
			}
			return fetch(middle+1, to)
		}
		if err != nil {
			return err
		}
		logs = append(logs, part...)
		return nil
	}
	for from := plan.from; from <= plan.to; from += limits.logChunkBlocks {
		to := min(from+limits.logChunkBlocks-1, plan.to)
		if err := fetch(from, to); err != nil {
			return nil, err
		}
		if to == plan.to {
			break
		}
	}
	return logs, nil
}
