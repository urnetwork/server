// Taskworker startup puts the contract expiry sweep on a fresh pass, without
// disturbing a run in flight or repeating a pass that a sibling start began.
package work

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// A pass whose epoch is this recent was started by a sibling's restart or by
// the sweep itself; restarting it again would only repeat its progress. A
// rolling restart of every worker therefore resets the sweep once.
const closeExpiredContractsFreshPassFence = 15 * time.Minute

// Chooses the replacement for one block's retained args, or false to keep them.
// A pass records its epoch when it first visits its backlog: the sweep's
// recent_after, or a legacy cursor's scan_before. Fresh args, a pass within the
// fence, and a sweep that has visited only its fresh window, which has no epoch
// yet, are kept. Unreadable args restart the pass.
func closeExpiredContractsFreshPassArgs(argsJson string, blockIndex int, now time.Time) (string, bool) {
	var args CloseExpiredContractsArgs
	if json.Unmarshal([]byte(argsJson), &args) == nil {
		var epoch time.Time
		switch {
		case args.Sweep != nil:
			epoch = args.Sweep.RecentAfter
		case args.Cursor != nil:
			epoch = args.Cursor.ScanBefore
		default:
			return "", false
		}
		if epoch.IsZero() || now.Sub(epoch) < closeExpiredContractsFreshPassFence {
			return "", false
		}
	}
	fresh, err := json.Marshal(&CloseExpiredContractsArgs{
		BlockSize:  DefaultCloseExpiredContractsBlockSize,
		BlockIndex: blockIndex,
	})
	if err != nil {
		panic(err)
	}
	return string(fresh), true
}

// One guarded attempt to start a block's sweep on a fresh pass, in the caller's
// read committed transaction. A leased row is left to its run in flight, which
// keeps its args and checkpoint; the caller retries after that run hands back.
// The replacement refuses the handback of a worker whose lease lapsed.
func ResetCloseExpiredContractsSweepInTx(ctx context.Context, tx server.PgTx, blockIndex int, now time.Time) task.RunOnceArgsReplacement {
	blockSize := DefaultCloseExpiredContractsBlockSize
	blockIndex = blockIndex % blockSize
	return task.ReplaceUnleasedRunOnceArgsInTx(ctx, tx, closeExpiredContractsRunOnce(blockSize, blockIndex), now,
		func(argsJson string) (string, bool) {
			return closeExpiredContractsFreshPassArgs(argsJson, blockIndex, now)
		})
}
