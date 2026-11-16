// Idle health ownership is independent of slow admission/round reads. Every
// refresh performs a new authenticated self-check; cancellation physically joins
// that owner before baseline preparation or a job takes heartbeat custody.
package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Only failed-check invalidation has a cleanup budget. Healthy self-check and
// registration retain their existing caller/transport/database time budgets.
type competitionIdleHostHeartbeatSettings struct {
	FailedCheckCleanupTimeout time.Duration
	// Nil is the real independent finite cleanup context; tests force its edge.
	cleanupContextForTest func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

// Cleanup cannot publish positive eligibility or extend a job's original clock.
func defaultCompetitionIdleHostHeartbeatSettings() competitionIdleHostHeartbeatSettings {
	return competitionIdleHostHeartbeatSettings{FailedCheckCleanupTimeout: 8 * time.Second}
}

// Some authenticated reports pass ordinary eligibility but fail a separate
// private-probe/integrity gate. Re-registering those bytes would falsely publish
// positive eligibility. Only a genuinely negative report may withdraw a row;
// otherwise the existing row expires under its unchanged freshness bound.
func competitionFailedHostCheckCanInvalidate(settings *Settings, check HostSelfCheck) bool {
	return check.HostId != "" && !check.Eligible(settings)
}

// The private state owns immutable check values and conservative acknowledgements,
// not the worker's per-job confirmed lease field. stop transfers them after join.
type competitionIdleHostHeartbeat struct {
	worker     *Worker
	ctx        context.Context
	cancel     context.CancelFunc
	cancelWork context.CancelCauseFunc
	settings   competitionIdleHostHeartbeatSettings
	done       chan struct{}
	updated    chan struct{}
	stateLock  sync.Mutex
	check      HostSelfCheck
	confirmed  time.Time
	err        error
}

// One child belongs to one idle interval. Other workers and their source/runtime
// contexts never inherit this owner's failure or stop cancellation.
func newCompetitionIdleHostHeartbeat(parent context.Context, cancelWork context.CancelCauseFunc, worker *Worker, check HostSelfCheck, confirmed time.Time) (*competitionIdleHostHeartbeat, error) {
	settings := defaultCompetitionIdleHostHeartbeatSettings()
	if worker.idleHeartbeatSettings != nil {
		settings = *worker.idleHeartbeatSettings
	}
	if settings.FailedCheckCleanupTimeout <= 0 || worker.settings.WorkerHeartbeatSeconds <= 0 || cancelWork == nil {
		return nil, errors.New("idle evaluator heartbeat owner settings refused")
	}
	ctx, cancel := context.WithCancel(parent)
	owner := &competitionIdleHostHeartbeat{worker: worker, ctx: ctx, cancel: cancel, cancelWork: cancelWork, settings: settings, done: make(chan struct{}), updated: make(chan struct{}, 1), check: check, confirmed: confirmed}
	go owner.run()
	return owner, nil
}

// Latest means the last fully self-checked and acknowledged value, not an in-flight
// probe or a new heartbeat fabricated from an earlier cached containment record.
func (self *competitionIdleHostHeartbeat) latest() HostSelfCheck {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.check
}

// Cancellation closes the exact request through its original transport/database
// owner. The caller waits for all work and cleanup before reusing heartbeat state.
func (self *competitionIdleHostHeartbeat) stop() (HostSelfCheck, time.Time, error) {
	self.cancel()
	<-self.done
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.check, self.confirmed, self.err
}

// A health failure stops admission work promptly, without canceling a healthy
// sibling worker or granting authority to a partially acknowledged refresh.
func (self *competitionIdleHostHeartbeat) fail(err error) {
	self.stateLock.Lock()
	self.err = errors.Join(self.err, err)
	self.stateLock.Unlock()
	self.cancelWork(err)
}

// A concrete negative check may withdraw eligibility after work is canceled, but
// only a separately bounded joined cleanup owns that write. Unknown/canceled
// probes cannot manufacture either a positive check or an invalidation report.
func (self *competitionIdleHostHeartbeat) invalidate(check HostSelfCheck) {
	if !competitionFailedHostCheckCanInvalidate(self.worker.settings, check) {
		return
	}
	newContext := self.settings.cleanupContextForTest
	if newContext == nil {
		newContext = context.WithTimeout
	}
	ctx, cancel := newContext(context.WithoutCancel(self.ctx), self.settings.FailedCheckCleanupTimeout)
	defer cancel()
	if err := self.worker.store.RegisterHost(ctx, self.worker.settings, check); err != nil {
		self.fail(fmt.Errorf("invalidate failed idle evaluator check: %w", err))
	}
}

// No idle-side activity reads or mutates the existing per-job heartbeat bound.
// Full checks may overlap only read-only idle runtime/availability/round work;
// baseline/evaluation owners begin only after stop has physically joined.
func (self *competitionIdleHostHeartbeat) run() {
	defer close(self.done)
	ticker := time.NewTicker(time.Duration(self.worker.settings.WorkerHeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	tick := ticker.C
	if self.worker.hostTickForTest != nil {
		tick = self.worker.hostTickForTest
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-tick:
		}
		if self.ctx.Err() != nil {
			return
		}
		fresh, err := self.worker.selfCheck(self.ctx)
		if err != nil {
			// A complete negative check remains meaningful at a simultaneous stop.
			// A canceled transport with no authenticated check is only withdrawn.
			if self.ctx.Err() != nil && fresh.HostId == "" {
				return
			}
			self.fail(fmt.Errorf("competition evaluator lost idle self-check: %w", err))
			if fresh.HostId != "" {
				self.invalidate(fresh)
			}
			return
		}
		if self.ctx.Err() != nil {
			return
		}
		confirmed, err := self.worker.registerHostAcknowledged(self.ctx, fresh)
		if err != nil {
			if self.ctx.Err() != nil {
				return
			}
			self.fail(fmt.Errorf("refresh idle evaluator host: %w", err))
			return
		}
		self.stateLock.Lock()
		self.check, self.confirmed = fresh, confirmed
		self.stateLock.Unlock()
		select {
		case self.updated <- struct{}{}:
		default:
		}
	}
}
