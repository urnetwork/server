// Pure tests of a live announcement's failure handling: which raises a live
// read or write may absorb, and the bounded retry wait. The database paths are
// in transport_announce_sync_db_test.go and
// transport_announce_worker_db_test.go.
package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An announcement with only what the failure helpers read.
func newSyncHelpersAnnounceForTest(ctx context.Context, settings *ConnectionAnnounceSettings) *ConnectionAnnounce {
	return &ConnectionAnnounce{
		ctx:      ctx,
		settings: settings,
	}
}

// Captures what a call raised, or nil.
func raisedForTest(call func()) (raised any) {
	defer func() {
		raised = recover()
	}()
	call()
	return nil
}

// A read or write failure becomes the call's error; the transport is not the
// caller's to end.
func TestRecoverLiveCallReturnsTheFailure(t *testing.T) {
	announce := newSyncHelpersAnnounceForTest(context.Background(), DefaultConnectionAnnounceSettings())
	callFailure := errors.New("synthetic pool timeout")
	var callErr error
	if raised := raisedForTest(func() {
		callErr = announce.recoverLiveCall(func() {
			panic(callFailure)
		})
	}); raised != nil {
		t.Fatalf("a call failure raised %v out of the live call", raised)
	}
	if !errors.Is(callErr, callFailure) {
		t.Fatalf("call error %v, want the call failure", callErr)
	}
	if callErr = announce.recoverLiveCall(func() {}); callErr != nil {
		t.Fatalf("a call that did not raise returned %v", callErr)
	}
}

// A programming fault, a raise that is not an error, and any raise once the
// announcement has ended still raise, as before.
func TestRecoverLiveCallKeepsFaultsAndTheEndFatal(t *testing.T) {
	live := newSyncHelpersAnnounceForTest(context.Background(), DefaultConnectionAnnounceSettings())
	endedCtx, endedCancel := context.WithCancel(context.Background())
	endedCancel()
	ended := newSyncHelpersAnnounceForTest(endedCtx, DefaultConnectionAnnounceSettings())

	for _, c := range []struct {
		name     string
		announce *ConnectionAnnounce
		call     func()
	}{
		{name: "runtime fault", announce: live, call: func() {
			var values []int
			_ = values[1]
		}},
		{name: "non-error raise", announce: live, call: func() {
			panic("synthetic string raise")
		}},
		{name: "ended announcement", announce: ended, call: func() {
			panic(errors.New("synthetic call after the end"))
		}},
	} {
		if raised := raisedForTest(func() {
			c.announce.recoverLiveCall(c.call)
		}); raised == nil {
			t.Errorf("%s was absorbed by the live call, want it raised", c.name)
		}
	}
}

// A failed round or write retries after the sync interval, then the wait
// doubles up to its bound. Without a bound the wait stays the sync interval.
func TestConnectionAnnounceRetryTimeoutBacksOffWithinItsBound(t *testing.T) {
	settings := DefaultConnectionAnnounceSettings()
	settings.SyncConnectionTimeout = 30 * time.Second
	settings.SyncRetryMaxTimeout = 2 * time.Minute
	announce := newSyncHelpersAnnounceForTest(context.Background(), settings)
	for _, c := range []struct {
		failureCount int
		timeout      time.Duration
	}{
		{failureCount: 0, timeout: 30 * time.Second},
		{failureCount: 1, timeout: 30 * time.Second},
		{failureCount: 2, timeout: time.Minute},
		{failureCount: 3, timeout: 2 * time.Minute},
		{failureCount: 4, timeout: 2 * time.Minute},
		{failureCount: 100, timeout: 2 * time.Minute},
	} {
		if timeout := announce.retryTimeout(c.failureCount); timeout != c.timeout {
			t.Errorf("after %d failed rounds the wait is %s, want %s", c.failureCount, timeout, c.timeout)
		}
	}

	settings.SyncRetryMaxTimeout = 0
	for _, failureCount := range []int{0, 1, 5} {
		if timeout := announce.retryTimeout(failureCount); timeout != 30*time.Second {
			t.Errorf("without a bound, after %d failed rounds the wait is %s, want the sync interval", failureCount, timeout)
		}
	}
}
