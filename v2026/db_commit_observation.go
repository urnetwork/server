package server

// Counts acknowledged transaction events without optional posts or external
// work. Counts describe this process's commit replies, not database timestamps
// or crash-durable history. Collectors must bind the process lifetime and reject
// unstable snapshots, overflow, or uncertainty/untracked growth in a window.

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

const txCommitCounterLimit = 16

// Zero value is ready for use. Snapshot and updates by distinct transaction
// owners are safe concurrently. Do not copy after first use. Callers provide a
// fixed set of counters; no event identities, callbacks or registry are retained.
type TxCommitCounter struct {
	confirmed atomic.Uint64
	uncertain atomic.Uint64
	untracked atomic.Uint64
	overflow  atomic.Bool
	writers   atomic.Int64
	revision  atomic.Uint64
}

// Reads confirmed replies without retrying a multi-field snapshot. Concurrent
// publication cannot transiently replace the count with zero. The value is
// monotonic until overflow; Snapshot retains the separate coverage signals.
func (self *TxCommitCounter) ConfirmedCount() uint64 {
	if self == nil {
		return 0
	}
	return self.confirmed.Load()
}

// Uncertain counts potential events whose commit reply is unknown. Untracked
// counts rejected registrations, including raw/savepoint owners and capacity.
// These are coverage signals, not committed events or independent transactions.
type TxCommitCounterSnapshot struct {
	Confirmed uint64 `json:"confirmed"`
	Uncertain uint64 `json:"uncertain"`
	Untracked uint64 `json:"untracked"`
	Overflow  bool   `json:"overflow"`
	Stable    bool   `json:"stable"`
}

// A fixed retry bound keeps observation independent of write traffic. A false
// Stable value makes the returned diagnostic values ineligible for subtraction.
// Different counters have independent snapshot boundaries.
func (self *TxCommitCounter) Snapshot() (snapshot TxCommitCounterSnapshot) {
	if self == nil {
		return
	}
	for range 8 {
		if self.writers.Load() != 0 {
			continue
		}
		revision := self.revision.Load()
		snapshot = TxCommitCounterSnapshot{
			Confirmed: self.confirmed.Load(),
			Uncertain: self.uncertain.Load(),
			Untracked: self.untracked.Load(),
			Overflow:  self.overflow.Load(),
		}
		if self.revision.Load() == revision && self.writers.Load() == 0 {
			snapshot.Stable = true
			return
		}
	}
	return
}

// A fixed number of atomic operations keeps observation off the retry path.
// Values are monotonic until arithmetic wraps; a sticky overflow signal then
// invalidates every later snapshot, including values after that wrap.
func (self *TxCommitCounter) add(target *atomic.Uint64, delta uint64) {
	self.writers.Add(1)
	defer self.writers.Add(-1)
	if self.revision.Add(1) == 0 {
		self.overflow.Store(true)
	}
	if target.Add(delta) < delta {
		self.overflow.Store(true)
	}
}

// The transaction owner alone registers events. One lazily allocated, fixed
// array belongs to one attempt; retries cannot retain an earlier attempt's work.
type txCommitObservations struct {
	counts [txCommitCounterLimit]txCommitCount
	length int
	sealed bool
}

type txCommitCount struct {
	counter *TxCommitCounter
	delta   uint64
}

// Register after a successful row-changing statement, never before the write or
// from an optional post. Repeated registration of the same counter is additive.
// False is an observation gap, never a reason to alter the financial result.
// Savepoints/raw transactions intentionally cannot borrow the outer owner: a
// later savepoint rollback would otherwise publish events that did not commit.
// As with pgx itself, registration on the same transaction is not concurrent.
// The callback must leave Commit/Rollback to the server transaction owner.
func AddTxCommitCount(tx PgTx, counter *TxCommitCounter, delta uint64) bool {
	if counter == nil {
		return false
	}
	if delta == 0 {
		return true
	}
	owner, ok := tx.(*postCommitPgTx)
	if !ok || owner == nil {
		counter.add(&counter.untracked, delta)
		return false
	}
	if owner.commitObservations == nil {
		owner.commitObservations = &txCommitObservations{}
	}
	observations := owner.commitObservations
	if observations.sealed {
		counter.add(&counter.untracked, delta)
		return false
	}
	for i := 0; i < observations.length; i++ {
		count := &observations.counts[i]
		if count.counter == counter {
			if ^uint64(0)-count.delta < delta {
				counter.add(&counter.untracked, delta)
				return false
			}
			count.delta += delta
			return true
		}
	}
	if observations.length == len(observations.counts) {
		counter.add(&counter.untracked, delta)
		return false
	}
	observations.counts[observations.length] = txCommitCount{counter: counter, delta: delta}
	observations.length++
	return true
}

// Observe immediately after the commit call, before timing, connection cleanup
// or optional post dispatch can fail. The original result/panic is unchanged.
// Only explicit rollback evidence discards events; unfamiliar failures remain
// uncertain even when they might in fact have rolled back.
func commitObservedTx(ctx context.Context, tx *postCommitPgTx) (err error) {
	if tx.commitObservations == nil {
		return tx.Commit(ctx)
	}
	returned := false
	defer func() {
		if !returned {
			tx.commitObservations.finish(false, false)
		}
	}()
	err = tx.Commit(ctx)
	returned = true
	rolledBack := errors.Is(err, pgx.ErrTxCommitRollback) || canRetryCommitError(err)
	tx.commitObservations.finish(err == nil, rolledBack)
	return
}

// One call seals the attempt, even if a caller accidentally repeats completion.
// Rollback before Commit is never published and the attempt becomes unreachable.
func (self *txCommitObservations) finish(confirmed, rolledBack bool) {
	if self == nil || self.sealed {
		return
	}
	self.sealed = true
	for i := 0; i < self.length; i++ {
		count := self.counts[i]
		if confirmed {
			count.counter.add(&count.counter.confirmed, count.delta)
		} else if !rolledBack {
			count.counter.add(&count.counter.uncertain, count.delta)
		}
		self.counts[i] = txCommitCount{}
	}
}
