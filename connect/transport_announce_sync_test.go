// Pure tests of the sync round's failure handling: which raises a live read
// may absorb, and the bounded retry wait. The database path is in
// transport_announce_sync_db_test.go.
package connect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An announcement with only what the sync round's helpers read.
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

// A read failure becomes the read's error; the transport is not the caller's
// to end.
func TestRecoverLiveReadReturnsTheReadFailure(t *testing.T) {
	announce := newSyncHelpersAnnounceForTest(context.Background(), DefaultConnectionAnnounceSettings())
	readFailure := errors.New("synthetic pool timeout")
	var readErr error
	if raised := raisedForTest(func() {
		readErr = announce.recoverLiveRead(func() {
			panic(readFailure)
		})
	}); raised != nil {
		t.Fatalf("a read failure raised %v out of the live read", raised)
	}
	if !errors.Is(readErr, readFailure) {
		t.Fatalf("read error %v, want the read failure", readErr)
	}
	if readErr = announce.recoverLiveRead(func() {}); readErr != nil {
		t.Fatalf("a read that did not raise returned %v", readErr)
	}
}

// A programming fault, a raise that is not an error, and any raise once the
// announcement has ended still raise, as before.
func TestRecoverLiveReadKeepsFaultsAndTheEndFatal(t *testing.T) {
	live := newSyncHelpersAnnounceForTest(context.Background(), DefaultConnectionAnnounceSettings())
	endedCtx, endedCancel := context.WithCancel(context.Background())
	endedCancel()
	ended := newSyncHelpersAnnounceForTest(endedCtx, DefaultConnectionAnnounceSettings())

	for _, c := range []struct {
		name     string
		announce *ConnectionAnnounce
		read     func()
	}{
		{name: "runtime fault", announce: live, read: func() {
			var values []int
			_ = values[1]
		}},
		{name: "non-error raise", announce: live, read: func() {
			panic("synthetic string raise")
		}},
		{name: "ended announcement", announce: ended, read: func() {
			panic(errors.New("synthetic read after the end"))
		}},
	} {
		if raised := raisedForTest(func() {
			c.announce.recoverLiveRead(c.read)
		}); raised == nil {
			t.Errorf("%s was absorbed by the live read, want it raised", c.name)
		}
	}
}

// A failed round retries at the next regular tick, then the wait doubles up
// to its bound. Without a bound the wait stays the sync interval.
func TestConnectionAnnounceSyncTimeoutBacksOffWithinItsBound(t *testing.T) {
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
		if timeout := announce.syncTimeout(c.failureCount); timeout != c.timeout {
			t.Errorf("after %d failed rounds the wait is %s, want %s", c.failureCount, timeout, c.timeout)
		}
	}

	settings.SyncRetryMaxTimeout = 0
	for _, failureCount := range []int{0, 1, 5} {
		if timeout := announce.syncTimeout(failureCount); timeout != 30*time.Second {
			t.Errorf("without a bound, after %d failed rounds the wait is %s, want the sync interval", failureCount, timeout)
		}
	}
}
