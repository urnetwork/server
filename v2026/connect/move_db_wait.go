package connect

import (
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The DB guard's reading of this process's Postgres pool (MOVE.md §5.5): the
// mean acquire wait between two readings, which the mover takes once a tick.
// A pool so saturated that no acquire completed between the readings, with
// every connection checked out, reads as having waited the whole time between
// them, since its waiters are still queued. Safe for concurrent use.
type moveDbPoolWait struct {
	// the pool's acquire counters, false until the pool opens
	acquireStat func() (server.DbPoolAcquireStat, bool)
	now         func() time.Time

	stateLock    sync.Mutex
	previous     server.DbPoolAcquireStat
	previousOk   bool
	previousTime time.Time
}

// The guard's reading of the main pool.
func newMoveDbPoolWait() *moveDbPoolWait {
	return &moveDbPoolWait{
		acquireStat: server.DbPoolAcquireStats,
		now:         time.Now,
	}
}

// The mean wait of the acquires since the previous reading. Zero on the first
// reading, while the pool is not open, after a reset restarted the counts, and
// when nothing waited.
func (self *moveDbPoolWait) PoolAcquireWait() time.Duration {
	stat, ok := self.acquireStat()
	now := self.now()

	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	wait := time.Duration(0)
	if ok && self.previousOk {
		acquireCount := stat.AcquireCount - self.previous.AcquireCount
		acquireDuration := stat.AcquireDuration - self.previous.AcquireDuration
		switch {
		case 0 < acquireCount && 0 <= acquireDuration:
			wait = acquireDuration / time.Duration(acquireCount)
		case acquireCount == 0 && 0 < stat.MaxConns && stat.MaxConns <= stat.AcquiredConns:
			wait = max(0, now.Sub(self.previousTime))
		}
	}
	self.previous, self.previousOk, self.previousTime = stat, ok, now
	return wait
}
