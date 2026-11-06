// Confirmed authority expires independently of blocked database completion.
// Only a timely real acknowledgment may move its conservative monotonic bound.
package controller

import (
	"context"
	"sync"
	"time"
)

// Methods are concurrent-safe. Timer generations suppress callbacks from a
// stopped window; a fired or elapsed window can never be revived by a late ack.
type competitionConfirmedAuthority struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	now        func() time.Time
	stateLock  sync.Mutex
	expiresAt  time.Time
	deadline   time.Time
	generation uint64
	timer      *time.Timer
	closed     bool
	expired    bool
}

// The sampled monotonic instant precedes the authority-clock read, so time
// spent observing the clock cannot silently enlarge the confirmed window.
func newCompetitionConfirmedAuthority(parent context.Context, expiresAt time.Time, now func() time.Time) (*competitionConfirmedAuthority, error) {
	if now == nil {
		// Database timestamps are microsecond-truncated; mapping a truncated
		// clock back to a timer could overstate the remaining authority.
		now = time.Now
	}
	ctx, cancel := context.WithCancelCause(parent)
	self := &competitionConfirmedAuthority{ctx: ctx, cancel: cancel, now: now}
	if err := self.confirm(expiresAt); err != nil {
		cancel(err)
		return nil, err
	}
	return self, nil
}

// Both the original monotonic deadline and the authority clock must remain
// live. A backward clock step cannot revive or lengthen the old window.
func (self *competitionConfirmedAuthority) confirm(expiresAt time.Time) error {
	observedAt := time.Now()
	now := self.now()
	return func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if err := self.ctx.Err(); err != nil {
			return err
		}
		if self.closed || expiresAt.IsZero() || !now.Before(expiresAt) ||
			(!self.expiresAt.IsZero() && (!now.Before(self.expiresAt) || !time.Now().Before(self.deadline))) {
			return ErrLeaseLost
		}
		deadline := observedAt.Add(expiresAt.Sub(now))
		if !self.deadline.IsZero() {
			// An unchanged DB timestamp must never grow merely because the
			// local wall clock moved backward between successful reads.
			if conservative := self.deadline.Add(expiresAt.Sub(self.expiresAt)); conservative.Before(deadline) {
				deadline = conservative
			}
		}
		if !time.Now().Before(deadline) {
			return ErrLeaseLost
		}
		self.generation++
		generation := self.generation
		self.expiresAt, self.deadline = expiresAt, deadline
		if self.timer != nil {
			self.timer.Stop()
		}
		self.timer = time.AfterFunc(time.Until(deadline), func() { self.expire(generation) })
		return nil
	}()
}

// A callback already queued before Stop must recheck its exact window under
// the state lock. Cancellation runs outside that lock, after closing authority.
func (self *competitionConfirmedAuthority) expire(generation uint64) {
	expired := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || self.generation != generation {
			return false
		}
		self.closed = true
		self.expired = true
		return true
	}()
	if expired {
		self.cancel(ErrLeaseLost)
	}
}

// Canceling the owner invalidates any queued callback without waiting for
// database cleanup; the caller still joins its actual worker/renewal owners.
func (self *competitionConfirmedAuthority) close() {
	expired := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.closed && !time.Now().Before(self.deadline) {
			self.expired = true
		}
		self.closed = true
		self.generation++
		if self.timer != nil {
			self.timer.Stop()
		}
		return self.expired
	}()
	if expired {
		self.cancel(ErrLeaseLost)
	} else {
		self.cancel(nil)
	}
}
