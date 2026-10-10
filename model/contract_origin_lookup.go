package model

// Concurrent requests for one watched pair may share only a recent missing
// origin. The absence is a scheduling hint; final reads and every successful
// contract creation retain their own transaction and result.

import (
	"context"
	"sync"
	"time"
)

const contractOriginMissingReuseTimeout = 100 * time.Millisecond

type contractOriginLookup struct {
	done       chan struct{}
	generation uint64
	missing    bool
	validUntil time.Time
}

// The registry bounds these states by its live watched pairs. No timer or
// background worker owns the hint, and removing the last watch discards it.
type contractOriginLookupState struct {
	stateLock    sync.Mutex
	generation   uint64
	pending      *contractOriginLookup
	missingUntil time.Time
}

// Invalidate before publishing the event edge. A lookup started before a
// commit or subscription acknowledgement cannot republish that old absence.
func (self *contractOriginLookupState) invalidate() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.generation++
	self.missingUntil = time.Time{}
}

// Share only an exact missing-origin result, for at most one first-retry
// interval measured from the source read's start. A successful creation,
// operational error, cancellation, or panic is never another request's result.
// Force bypasses both the cached hint and an in-flight read at the final deadline.
func (self *ContractOriginWatch) Lookup(ctx context.Context, force bool, create func() (*TransferEscrow, error)) (*TransferEscrow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self == nil || force || self.owner.ctx.Err() != nil {
		return create()
	}
	state := &self.pair.lookup
	var pending *contractOriginLookup
	var shared *contractOriginLookup
	missing := false
	func() {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		now := time.Now()
		if now.Before(state.missingUntil) {
			missing = true
		} else if state.pending != nil {
			shared = state.pending
		} else {
			pending = &contractOriginLookup{
				done: make(chan struct{}), generation: state.generation,
				validUntil: now.Add(contractOriginMissingReuseTimeout),
			}
			state.pending = pending
		}
	}()
	if missing {
		return nil, ErrMissingCompanionOrigin
	}
	if shared != nil {
		completed := false
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-shared.done:
			completed = true
		case <-time.After(time.Until(shared.validUntil)):
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// An in-flight read cannot hold a follower past the hint's original
		// freshness bound. The follower owns any source work after that point.
		if !completed {
			return create()
		}
		func() {
			state.stateLock.Lock()
			defer state.stateLock.Unlock()
			missing = shared.missing && shared.generation == state.generation && time.Now().Before(shared.validUntil)
		}()
		if missing {
			return nil, ErrMissingCompanionOrigin
		}
		// Followers of a successful or unavailable read create independently;
		// the per-pair hint never serializes otherwise valid contract writes.
		return create()
	}
	// Closing the pending edge on panic keeps another caller's lifecycle
	// independent from the transaction owner that failed.
	defer func() {
		state.stateLock.Lock()
		defer state.stateLock.Unlock()
		state.pending = nil
		if pending.missing && pending.generation == state.generation && time.Now().Before(pending.validUntil) {
			state.missingUntil = pending.validUntil
		}
		close(pending.done)
	}()
	escrow, err := create()
	pending.missing = escrow == nil && err == ErrMissingCompanionOrigin
	return escrow, err
}
