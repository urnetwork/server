// Registered function lanes cannot be hidden by another function's older
// backlog. One indexed lane turn alternates with the original global queue.
// A share lane also takes some of the global queue's turns, so a target that
// a dominant older backlog would otherwise reach once per full rotation keeps
// a minimum claim rate.
//
// A share lane's claims drain it from its head, and the drained rows stay in
// its index as dead entries until vacuum removes them. While a long snapshot
// holds the horizon, a scan from the head visits each of them in the heap. A
// share lane's claims therefore read from a floor: the first due row the last
// claim of that lane saw, at most claimShareFloorLag before now. A share
// target's rows enter its lane at their publication time, so no due row falls
// below the floor. One claim per claimShareFloorReset reads the lane from its
// start, so a row published further in the past waits at most that long.
//
// A read from the start is the one read that can cross every drained row.
// Until one has completed, the lane has no floor and takes no ordinary turns,
// so a start that cannot be read fails one claim per claimShareFloorReset
// instead of every share turn.
package task

import (
	"time"
)

// The floor stays this far behind now, longer than clock skew between
// publishers and a publishing transaction.
const claimShareFloorLag = time.Minute

// How often a share lane's claim reads the lane from its start.
const claimShareFloorReset = 5 * time.Minute

// Where a share lane's claims start reading it, once a claim has read the
// lane, and when the next claim reads it from its start. Guarded by the
// worker's stateLock.
type taskClaimShareFloor struct {
	block   int64
	known   bool
	resetAt time.Time
}

// Rotation is shared by this worker's Run and EvalTasks callers. An isolated
// lane handed from refill to the next claim belongs only to that Run's poll.
func (self *TaskWorker) nextClaimFunction(options taskClaimOptions) string {
	if !self.settings.FairClaimFunctions {
		return ""
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.draining || self.runCtx.Err() != nil || len(self.claimFunctionNames) == 0 {
		return ""
	}
	if options.poll != nil && !options.ordinaryOnly && options.poll.isolatedFunction != "" {
		name := options.poll.isolatedFunction
		options.poll.isolatedFunction = ""
		return name
	}
	turn := self.claimFunctionTurn
	self.claimFunctionTurn++
	if turn%2 != 0 {
		return self.nextClaimShareWithLock()
	}
	name := self.claimFunctionNames[(turn/2)%uint64(len(self.claimFunctionNames))]
	canonical := self.claimTargetName(name)
	if limit := self.settings.TargetClaimLimits[canonical]; limit != 0 && limit <= self.claimTargetCounts[canonical] {
		return ""
	}
	return name
}

// One target's minimum share of the ordinary turns.
type taskClaimShare struct {
	laneName   string
	targetName string
	period     int
}

// An ordinary turn gives each share lane one credit, up to its period. The
// first lane by name holding a full period spends it on this turn; the others
// keep their credit for the next ordinary turn. A saturated target, or a lane
// without a floor whose next read from the start is not due, keeps its full
// credit and leaves the turn to the ordinary queue. Empty without shares.
func (self *TaskWorker) nextClaimShareWithLock() string {
	now := self.claimNow()
	turnName := ""
	for _, share := range self.claimShares {
		credit := min(self.claimShareCredits[share.laneName]+1, share.period)
		if turnName == "" && credit == share.period {
			limit := self.settings.TargetClaimLimits[share.targetName]
			floor := self.claimShareFloors[share.laneName]
			if (limit == 0 || self.claimTargetCounts[share.targetName] < limit) && (floor.known || !now.Before(floor.resetAt)) {
				turnName = share.laneName
				credit = 0
			}
		}
		self.claimShareCredits[share.laneName] = credit
	}
	return turnName
}

// Whether the lane is a share lane, the floor its claim reads from (zero for
// the lane's start), and whether this claim is the lane's periodic full read.
// A rotation turn of a lane without a floor also reads from the start.
func (self *TaskWorker) claimShareFloor(laneName string, now time.Time) (share bool, floorBlock int64, reset bool) {
	if laneName == "" {
		return false, 0, false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	floor := self.claimShareFloors[laneName]
	if floor == nil {
		return false, 0, false
	}
	if !now.Before(floor.resetAt) {
		floor.resetAt = now.Add(claimShareFloorReset)
		return true, 0, true
	}
	return true, floor.block, false
}

// A claim saw firstBlock as the lane's first due row at or past its floor, or
// nil when none was due. A full read sets the floor; other reads only raise
// it, since each saw no due row below its own observation.
func (self *TaskWorker) observeClaimShareFloor(laneName string, firstBlock *int64, nowBlock int64, reset bool) {
	block := nowBlock - int64(claimShareFloorLag/time.Second)/BlockSizeSeconds
	if firstBlock != nil {
		block = min(block, *firstBlock)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	floor := self.claimShareFloors[laneName]
	if floor == nil {
		return
	}
	if reset || !floor.known {
		floor.block = block
	} else {
		floor.block = max(floor.block, block)
	}
	floor.known = true
}
