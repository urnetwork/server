// Every taskworker start puts the contract expiry sweep on a fresh pass. A
// retained pass keeps the lanes it completed, so rows it left behind can stay
// open across restarts; a fresh pass walks every open contract from the oldest.
package taskworker

import (
	"context"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
	"github.com/urnetwork/server/taskworker/work"
)

const (
	// A stopping container's claim ends within its run budget or its lease.
	// Retrying this often for this long finds the gap between that handback
	// and the next claim.
	closeExpiredFreshPassRetryInterval = 500 * time.Millisecond
	closeExpiredFreshPassRetryLimit    = 10 * time.Minute
)

// Tests drive the retry loop through its context instead of a global.
type closeExpiredFreshPassHooks struct {
	after     func(time.Duration) <-chan time.Time
	attempted func(blockIndex int, outcome task.RunOnceArgsReplacement)
}

type closeExpiredFreshPassHooksKey struct{}

// Runs after the scheduling transaction commits, so it holds no run-once row of
// that transaction. Each block is tried once now. A block whose row a lease
// holds keeps retrying in the background until that run hands back, the retry
// limit passes or ctx ends. A run in flight is never disturbed: its handback
// lands first, then the next attempt replaces the args its successor would run.
// A sibling start within the fence finds the new pass and keeps it.
func startCloseExpiredContractsFreshPass(ctx context.Context) {
	hooks, _ := ctx.Value(closeExpiredFreshPassHooksKey{}).(*closeExpiredFreshPassHooks)
	blockIndexes := make([]int, 0, work.DefaultCloseExpiredContractsBlockSize)
	for blockIndex := range work.DefaultCloseExpiredContractsBlockSize {
		blockIndexes = append(blockIndexes, blockIndex)
	}
	leasedBlockIndexes := attemptCloseExpiredContractsFreshPass(ctx, hooks, blockIndexes)
	if len(leasedBlockIndexes) == 0 {
		return
	}
	go server.HandleError(func() {
		retryCloseExpiredContractsFreshPass(ctx, hooks, leasedBlockIndexes)
	})
}

// Returns the blocks to retry: a leased or concurrently changed row, or an
// attempt that failed. Each attempt is its own bounded transaction.
func attemptCloseExpiredContractsFreshPass(ctx context.Context, hooks *closeExpiredFreshPassHooks, blockIndexes []int) (retryBlockIndexes []int) {
	for _, blockIndex := range blockIndexes {
		outcome := task.RunOnceArgsChanged
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				boundStartupStatementsInTx(ctx, tx)
				outcome = work.ResetCloseExpiredContractsSweepInTx(ctx, tx, blockIndex, server.NowUtc())
			}, server.TxReadCommitted, server.OptNoRetry())
		}, func(err error) {
			outcome = task.RunOnceArgsChanged
			glog.Infof("[close-expired]startup fresh pass attempt failed block=%d: %v\n", blockIndex, err)
		})
		if hooks != nil && hooks.attempted != nil {
			hooks.attempted(blockIndex, outcome)
		}
		switch outcome {
		case task.RunOnceArgsReplaced:
			glog.Infof("[close-expired]startup started a fresh sweep pass block=%d\n", blockIndex)
		case task.RunOnceArgsLeased, task.RunOnceArgsChanged:
			retryBlockIndexes = append(retryBlockIndexes, blockIndex)
		}
	}
	return
}

// Bounded by attempts, so the limit holds under a test timer too.
func retryCloseExpiredContractsFreshPass(ctx context.Context, hooks *closeExpiredFreshPassHooks, blockIndexes []int) {
	after := time.After
	if hooks != nil && hooks.after != nil {
		after = hooks.after
	}
	for attempt := 1; 0 < len(blockIndexes); attempt += 1 {
		if closeExpiredFreshPassRetryLimit < time.Duration(attempt)*closeExpiredFreshPassRetryInterval {
			glog.Infof("[close-expired]startup fresh pass stopped retrying; leased blocks=%v\n", blockIndexes)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-after(closeExpiredFreshPassRetryInterval):
		}
		blockIndexes = attemptCloseExpiredContractsFreshPass(ctx, hooks, blockIndexes)
	}
}
