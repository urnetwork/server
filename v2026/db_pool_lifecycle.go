package server

import (
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Wrapper ownership includes setup, callback, commit and rollback until release
// starts. Cleanup contains only wrapper releases known to require disposal;
// pool-internal Ping/lifetime disposal and raw maintenance leases are outside
// this observation. These values are not a partition of pgx's acquired gauge.
type pgPoolWrapperSnapshot struct {
	owned           int64
	releasing       int64
	cleanupPending  int64
	trackingDropped uint64
}

// Keep at most MaxConns cleanup channels per actual pool generation. Sampling
// and releases prune completed channels; no observer goroutine, timer, query or
// connection lifetime change is needed. A full registry reports lost tracking
// instead of silently treating an unobserved cleanup as complete.
type pgPoolWrapperLifecycle struct {
	pool            *pgxpool.Pool
	stateLock       sync.Mutex
	owned           int64
	releasing       int64
	limit           int
	pending         map[<-chan struct{}]struct{}
	trackingDropped *atomic.Uint64
}

func (self *safePgPool) observeBorrow(pool *pgxpool.Pool) *pgPoolWrapperLifecycle {
	// Do not take safePgPool.mutex after Acquire: reset can hold that mutex
	// while waiting for this exact borrower to release its connection.
	self.lifecycleLock.Lock()
	defer self.lifecycleLock.Unlock()
	if self.lifecycle == nil || self.lifecycle.pool != pool {
		self.lifecycle = &pgPoolWrapperLifecycle{
			pool: pool, limit: int(pool.Stat().MaxConns()),
			pending:         make(map[<-chan struct{}]struct{}),
			trackingDropped: &self.lifecycleDropped,
		}
	}
	self.lifecycle.stateLock.Lock()
	self.lifecycle.owned++
	self.lifecycle.stateLock.Unlock()
	return self.lifecycle
}

func (self *safePgPool) wrapperSnapshot(pool *pgxpool.Pool) pgPoolWrapperSnapshot {
	self.lifecycleLock.Lock()
	lifecycle := self.lifecycle
	self.lifecycleLock.Unlock()
	if lifecycle == nil || lifecycle.pool != pool {
		return pgPoolWrapperSnapshot{trackingDropped: self.lifecycleDropped.Load()}
	}
	return lifecycle.snapshot()
}

func (self *pgPoolWrapperLifecycle) beginRelease() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.owned--
	self.releasing++
}

// Call only after the existing Release/discard operation returns. A panic in
// that operation remains visible as an unfinished release, without changing
// the original recovery or disposal path.
func (self *pgPoolWrapperLifecycle) finishRelease(cleanup <-chan struct{}, needsCleanup bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.releasing--
	self.pruneCompleted()
	if !needsCleanup {
		return
	}
	select {
	case <-cleanup:
		return
	default:
	}
	if _, tracked := self.pending[cleanup]; tracked {
		return
	}
	if len(self.pending) >= self.limit {
		self.trackingDropped.Add(1)
		return
	}
	self.pending[cleanup] = struct{}{}
}

// Called with stateLock held. Channel state only: never touch a released Conn.
func (self *pgPoolWrapperLifecycle) pruneCompleted() {
	for cleanup := range self.pending {
		select {
		case <-cleanup:
			delete(self.pending, cleanup)
		default:
		}
	}
}

func (self *pgPoolWrapperLifecycle) snapshot() pgPoolWrapperSnapshot {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.pruneCompleted()
	return pgPoolWrapperSnapshot{
		owned: self.owned, releasing: self.releasing,
		cleanupPending: int64(len(self.pending)), trackingDropped: self.trackingDropped.Load(),
	}
}
