package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/controller"
)

// A retry wait the test steps by hand. Each call hands over its delay and holds
// until it is released or its context ends, so no test depends on a clock.
type reservedUploadTestWait struct {
	delays  chan time.Duration
	release chan struct{}
}

func newReservedUploadTestWait() *reservedUploadTestWait {
	return &reservedUploadTestWait{delays: make(chan time.Duration), release: make(chan struct{})}
}

func (self *reservedUploadTestWait) wait(ctx context.Context, delay time.Duration) error {
	select {
	case self.delays <- delay:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-self.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// The delay the lane's next retry asks for. A lane that stopped trying is the
// defect, so its end is a failure, not a hang.
func (self *reservedUploadTestWait) nextDelay(t testing.TB, lane *reservedUploadLane, attempts *atomic.Int32) time.Duration {
	t.Helper()
	select {
	case delay := <-self.delays:
		return delay
	case <-lane.done:
		t.Fatalf("the lane stopped trying after %d failed starts, without an admission", attempts.Load())
		return 0
	}
}

func requireReservedUploadGauge(t testing.TB, want float64) {
	t.Helper()
	if got := testutil.ToFloat64(reservedUploadStartupFailedGauge); got != want {
		t.Fatalf("startup failure gauge = %v, want %v", got, want)
	}
}

// On 2026-11-01 one process's only startup attempt failed, and its lane stayed
// off for the 22 hours until the next deploy replaced it.
func TestReservedUploadStartupRetriesUntilTheLaneIsUp(t *testing.T) {
	t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
	up := &controller.StReservedAttemptUpload{}
	var attempts atomic.Int32
	wait := newReservedUploadTestWait()
	lane := startReservedUploadLane(t.Context(), func(context.Context) (*controller.StReservedAttemptUpload, error) {
		if attempts.Add(1) <= 3 {
			return nil, errors.New("dial tcp 192.0.2.9:9944: i/o timeout")
		}
		return up, nil
	}, wait.wait)
	defer lane.Close()
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second} {
		if delay := wait.nextDelay(t, lane, &attempts); delay != want {
			t.Fatalf("retry delay = %s, want %s", delay, want)
		}
		requireReservedUploadGauge(t, 1)
		if admission, starting := lane.Admission(); admission != nil || !starting {
			t.Fatalf("a lane still retrying reported admission=%v starting=%t", admission, starting)
		}
		wait.release <- struct{}{}
	}
	<-lane.done
	requireReservedUploadGauge(t, 0)
	if admission, starting := lane.Admission(); admission != up || starting {
		t.Fatalf("a started lane reported admission=%v starting=%t", admission, starting)
	}
	if got := attempts.Load(); got != 4 {
		t.Fatalf("start attempts = %d, want 4", got)
	}
}

// Backoff doubles to a bound, and a failure the process cannot outlive, such
// as a pinned config newer than this build, keeps its one retry a minute.
func TestReservedUploadStartupRetryBacksOffToItsBound(t *testing.T) {
	t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
	var attempts atomic.Int32
	wait := newReservedUploadTestWait()
	lane := startReservedUploadLane(t.Context(), func(context.Context) (*controller.StReservedAttemptUpload, error) {
		attempts.Add(1)
		return nil, errors.New("decode validator config: field runtime_successor_profile not found")
	}, wait.wait)
	defer lane.Close()
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, time.Minute, time.Minute} {
		if delay := wait.nextDelay(t, lane, &attempts); delay != want {
			t.Fatalf("retry delay = %s, want %s", delay, want)
		}
		wait.release <- struct{}{}
	}
	requireReservedUploadGauge(t, 1)
}

// A drain ends the retrying at once. The lane is still only not running, so
// uploads the drain has yet to finish keep their retryable answer.
func TestReservedUploadStartupRetryEndsWithItsContext(t *testing.T) {
	t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int32
	wait := newReservedUploadTestWait()
	lane := startReservedUploadLane(ctx, func(context.Context) (*controller.StReservedAttemptUpload, error) {
		attempts.Add(1)
		return nil, errors.New("dial tcp 192.0.2.9:9944: i/o timeout")
	}, wait.wait)
	defer lane.Close()
	wait.nextDelay(t, lane, &attempts)
	cancel()
	<-lane.done
	if got := attempts.Load(); got != 1 {
		t.Fatalf("start attempts = %d after the drain began, want 1", got)
	}
	if admission, starting := lane.Admission(); admission != nil || !starting {
		t.Fatalf("a draining lane reported admission=%v starting=%t", admission, starting)
	}
	requireReservedUploadGauge(t, 1)
}

// The router's close joins the retrying even when its context still runs.
func TestReservedUploadLaneCloseJoinsTheRetry(t *testing.T) {
	t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
	var attempts atomic.Int32
	wait := newReservedUploadTestWait()
	lane := startReservedUploadLane(t.Context(), func(context.Context) (*controller.StReservedAttemptUpload, error) {
		attempts.Add(1)
		return nil, errors.New("dial tcp 192.0.2.9:9944: i/o timeout")
	}, wait.wait)
	wait.nextDelay(t, lane, &attempts)
	lane.Close()
	select {
	case <-lane.done:
	default:
		t.Fatal("close returned with the retry still running")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("start attempts = %d after close, want 1", got)
	}
}

// A panic in one attempt is one failed attempt. It must not take the api block
// down, since only the reserved lane depends on this start.
func TestReservedUploadStartupPanicIsAFailedAttempt(t *testing.T) {
	t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
	up := &controller.StReservedAttemptUpload{}
	var attempts atomic.Int32
	wait := newReservedUploadTestWait()
	lane := startReservedUploadLane(t.Context(), func(context.Context) (*controller.StReservedAttemptUpload, error) {
		if attempts.Add(1) == 1 {
			panic(errors.New("synthetic startup panic"))
		}
		return up, nil
	}, wait.wait)
	defer lane.Close()
	wait.nextDelay(t, lane, &attempts)
	requireReservedUploadGauge(t, 1)
	wait.release <- struct{}{}
	<-lane.done
	requireReservedUploadGauge(t, 0)
	if admission, _ := lane.Admission(); admission != up {
		t.Fatalf("the lane did not come up after a panicked attempt: %v", admission)
	}
}

// A startup failure disables only the reserved lane, and says the lane is
// coming. A lane that is not configured has nothing to retry, which is a
// standing refusal rather than a wait.
func TestReservedUploadStartupFailureDisablesOnlyTheReservedLane(t *testing.T) {
	t.Cleanup(func() { reservedUploadStartupFailedGauge.Set(0) })
	var attempts atomic.Int32
	failed := startReservedUploadLane(t.Context(), func(context.Context) (*controller.StReservedAttemptUpload, error) {
		attempts.Add(1)
		return nil, errors.New("decode validator config: field runtime_successor_profile not found")
	}, newReservedUploadTestWait().wait)
	defer failed.Close()
	if admission, starting := failed.Admission(); admission != nil || !starting {
		t.Fatalf("a failed startup reported admission=%v starting=%t", admission, starting)
	}
	requireReservedUploadGauge(t, 1)
	unconfiguredAttempts := 0
	unconfigured := startReservedUploadLane(t.Context(), func(context.Context) (*controller.StReservedAttemptUpload, error) {
		unconfiguredAttempts++
		return nil, nil
	}, func(context.Context, time.Duration) error {
		t.Error("an unconfigured lane waited to retry")
		return errors.New("unexpected wait")
	})
	defer unconfigured.Close()
	<-unconfigured.done
	if admission, starting := unconfigured.Admission(); admission != nil || starting {
		t.Fatalf("an unconfigured lane reported admission=%v starting=%t", admission, starting)
	}
	requireReservedUploadGauge(t, 0)
	if unconfiguredAttempts != 1 {
		t.Fatalf("an unconfigured lane started %d times, want 1", unconfiguredAttempts)
	}
}
