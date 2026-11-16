package api

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// The first retry comes soon after a failed start, which is usually a chain
// endpoint this process could not reach for a moment. The bound keeps a lane
// off for at most about this long after the cause clears, and is small enough
// that a failure no retry can fix costs one dial and one log line a minute.
const (
	reservedUploadRetryMinimumDelay = 5 * time.Second
	reservedUploadRetryMaximumDelay = 1 * time.Minute
)

// The reserved validator upload lane of one api process. The first start runs
// before the router is exposed. A start that fails disables only this lane:
// every other route serves, and the lane keeps trying until it starts or its
// context ends. One failure at startup had left a block's lane off until the
// next deploy, 22 hours on 2026-11-01, and the validator's uploads that reached
// the block were refused the whole time.
type reservedUploadLane struct {
	running  atomic.Pointer[controller.StReservedAttemptUpload]
	retrying atomic.Bool
	cancel   context.CancelFunc
	// closed when no further start can run
	done chan struct{}
}

// A reserved upload admission the running API cannot start, such as a pinned
// validator config newer than this build's validator package, disables only
// the reserved upload lane, while the lane retries. Failing NewRouter instead
// made every restarting API block exit on one configuration mismatch.
func startReservedAttemptUpload(ctx context.Context, start func(context.Context) (*controller.StReservedAttemptUpload, error)) *reservedUploadLane {
	return startReservedUploadLane(ctx, start, waitReservedUploadRetry)
}

func startReservedUploadLane(
	ctx context.Context,
	start func(context.Context) (*controller.StReservedAttemptUpload, error),
	wait func(context.Context, time.Duration) error,
) *reservedUploadLane {
	laneCtx, cancel := context.WithCancel(ctx)
	self := &reservedUploadLane{cancel: cancel, done: make(chan struct{})}
	if self.attempt(laneCtx, start) {
		close(self.done)
		return self
	}
	go server.HandleError(func() {
		defer close(self.done)
		self.retry(laneCtx, start, wait)
	})
	return self
}

// Waits out the backoff before each attempt, so a context that ended stops
// the lane without another dial.
func (self *reservedUploadLane) retry(
	ctx context.Context,
	start func(context.Context) (*controller.StReservedAttemptUpload, error),
	wait func(context.Context, time.Duration) error,
) {
	delay := reservedUploadRetryMinimumDelay
	for {
		if err := wait(ctx, delay); err != nil || ctx.Err() != nil {
			return
		}
		if self.attempt(ctx, start) {
			return
		}
		delay = min(2*delay, reservedUploadRetryMaximumDelay)
	}
}

// One start. True when the lane has nothing left to try: it is running, or no
// configuration asks for it. A panicking start is a failed attempt.
func (self *reservedUploadLane) attempt(ctx context.Context, start func(context.Context) (*controller.StReservedAttemptUpload, error)) bool {
	var reservedUpload *controller.StReservedAttemptUpload
	var err error
	server.HandleError(func() {
		reservedUpload, err = start(ctx)
	}, func(cause error) {
		err = cause
	})
	if err != nil {
		reservedUpload.Close()
		self.retrying.Store(true)
		reservedUploadStartupFailedGauge.Set(1)
		glog.Infof("[api]reserved validator staging unavailable: %s\n", err)
		return false
	}
	self.running.Store(reservedUpload)
	retried := self.retrying.Swap(false)
	reservedUploadStartupFailedGauge.Set(0)
	if retried {
		glog.Infof("[api]reserved validator staging started after retrying\n")
	}
	return true
}

// The lane as the upload route asks for it on each request. A failed lane stays
// "starting" through a drain, which ends only its retrying: the uploads the
// drain has yet to finish are still answered as a process without its lane.
func (self *reservedUploadLane) Admission() (*controller.StReservedAttemptUpload, bool) {
	if self == nil {
		return nil, false
	}
	running := self.running.Load()
	return running, running == nil && self.retrying.Load()
}

// Ends the retrying, joins a start in flight, and closes what started. Called
// after the routes have drained, so no request still holds the admission.
func (self *reservedUploadLane) Close() {
	if self == nil {
		return
	}
	self.cancel()
	<-self.done
	self.running.Load().Close()
}

func waitReservedUploadRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
