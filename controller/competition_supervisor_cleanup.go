// A requested supervisor shutdown cancels work immediately but may renew its
// same live parent while the original child joins. Fence loss never drains.
package controller

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Admission and finish each belong to the single reconciliation caller; the
// cancellation goroutine observes them under the lock. No other stage drains.
type competitionSupervisorRenewalDrain struct {
	ctx            context.Context
	parent         context.Context
	cancel         context.CancelCauseFunc
	stateLock      sync.Mutex
	workerActive   bool
	workerFinished chan struct{}
	finished       chan struct{}
	joined         chan struct{}
}

// Non-positive grace preserves the original parent context. A positive grace
// starts only when requested cancellation finds the original worker active.
func newCompetitionSupervisorRenewalDrain(parent context.Context, grace time.Duration) *competitionSupervisorRenewalDrain {
	self := &competitionSupervisorRenewalDrain{ctx: parent, parent: parent, workerFinished: make(chan struct{})}
	if grace <= 0 {
		return self
	}
	self.ctx, self.cancel = context.WithCancelCause(context.WithoutCancel(parent))
	self.finished, self.joined = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(self.joined)
		select {
		case <-parent.Done():
			drain := func() bool {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				return self.workerActive && parent.Err() == context.Canceled && !errors.Is(context.Cause(parent), ErrLeaseLost)
			}()
			if drain {
				select {
				case <-time.After(grace):
				case <-self.workerFinished:
				case <-self.finished:
				}
			}
			self.cancel(context.Cause(parent))
		case <-self.finished:
		}
	}()
	return self
}

// Admission is before the original worker call, never after parent shutdown.
func (self *competitionSupervisorRenewalDrain) beginWorker() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := self.parent.Err(); err != nil {
		return err
	}
	if err := self.ctx.Err(); err != nil {
		return err
	}
	self.workerActive = true
	return nil
}

// The same child returning ends drain immediately, including successful exits
// after cancellation. Nothing can use the remaining grace to launch new work.
func (self *competitionSupervisorRenewalDrain) endWorker() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.workerActive = false
		close(self.workerFinished)
	}()
	if self.cancel != nil && self.parent.Err() != nil {
		self.cancel(context.Cause(self.parent))
	}
}

// Reconciliation still joins the phase owner; no timer goroutine escapes it.
func (self *competitionSupervisorRenewalDrain) close() {
	if self.cancel != nil {
		self.cancel(nil)
		close(self.finished)
		<-self.joined
	}
}

// Work obeys its parent directly, preserving deadline errors. Joining the
// authority callback prevents
// cancellation callbacks from escaping the reconciliation's owner boundary.
func competitionSupervisorWorkContext(parent, authority context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	joined := make(chan struct{})
	stop := context.AfterFunc(authority, func() {
		cancel(context.Cause(authority))
		close(joined)
	})
	return ctx, func() {
		if !stop() {
			<-joined
		}
		cancel(nil)
	}
}
