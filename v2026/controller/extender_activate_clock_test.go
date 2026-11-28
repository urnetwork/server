package controller

import (
	"context"
	"errors"
	"sync"
	"time"
)

// A fake clock for the probes of one activation (extenderActivationClock).
// Time moves only when a test moves it, and a context bounded on this clock
// ends with context.DeadlineExceeded at the instant the fake time reaches its
// deadline, so a fake probe that "takes" 9.5 s or waits out its budget does so
// at once and exactly, and no test waits on the real clock.
//
// Safe for concurrent use.
type testActivationClock struct {
	stateLock sync.Mutex
	now       time.Time
	// the deadlines not yet reached, with what ends each one's context
	timers map[*testActivationTimer]bool
}

type testActivationTimer struct {
	deadline time.Time
	cancel   context.CancelCauseFunc
}

func newTestActivationClock() *testActivationClock {
	return &testActivationClock{
		now:    time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC),
		timers: map[*testActivationTimer]bool{},
	}
}

// The clock an activation judges on.
func (self *testActivationClock) activationClock() *extenderActivationClock {
	return &extenderActivationClock{
		now:          self.Now,
		withDeadline: self.WithDeadline,
	}
}

func (self *testActivationClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// context.WithDeadline on the fake clock: the earlier of deadline and the
// parent's deadline, ended when the fake time reaches it.
func (self *testActivationClock) WithDeadline(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if parentDeadline, ok := ctx.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	cancelCtx, cancel := context.WithCancelCause(ctx)
	timer := &testActivationTimer{
		deadline: deadline,
		cancel:   cancel,
	}
	reached := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.now.Before(deadline) {
			return true
		}
		self.timers[timer] = true
		return false
	}()
	if reached {
		cancel(context.DeadlineExceeded)
	}
	deadlineCtx := &testActivationDeadlineContext{
		Context:  cancelCtx,
		deadline: deadline,
	}
	return deadlineCtx, func() {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			delete(self.timers, timer)
		}()
		cancel(context.Canceled)
	}
}

// Moves the fake time on, ending every context whose deadline it reaches.
func (self *testActivationClock) Advance(d time.Duration) {
	reachedTimers := func() []*testActivationTimer {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.now = self.now.Add(d)
		reachedTimers := []*testActivationTimer{}
		for timer := range self.timers {
			if !self.now.Before(timer.deadline) {
				reachedTimers = append(reachedTimers, timer)
				delete(self.timers, timer)
			}
		}
		return reachedTimers
	}()
	for _, timer := range reachedTimers {
		timer.cancel(context.DeadlineExceeded)
	}
}

// What a probe that never answers does: it waits until its context ends,
// which here is moving the fake time to the context's deadline, and returns
// the context's error. A context with no deadline would be waited on
// forever, so that is an error rather than a hung test.
func (self *testActivationClock) WaitOut(ctx context.Context) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return errors.New("the probe was given no deadline to wait out")
	}
	if d := deadline.Sub(self.Now()); 0 < d {
		self.Advance(d)
	}
	<-ctx.Done()
	return ctx.Err()
}

// A context ended by the fake clock. It reports its fake deadline, and once
// the fake time has reached it, the error a deadline gives.
type testActivationDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (self *testActivationDeadlineContext) Deadline() (time.Time, bool) {
	return self.deadline, true
}

func (self *testActivationDeadlineContext) Err() error {
	err := self.Context.Err()
	if err != nil && errors.Is(context.Cause(self.Context), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return err
}
