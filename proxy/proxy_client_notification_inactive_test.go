package proxy

// A change range whose proxies are all no longer deliverable (their client was
// deactivated, or their row removed) must not hold the watch's cursor: the read
// reports the newest change it covered, and the watch moves up to it. Before
// the active filter the read delivered every change it found, so the cursor
// always advanced; filtering without moving the cursor would re-read the same
// range on every poll, which on a (host, block) whose proxies are all inactive
// is its whole change history every NotificationTimeout.

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Answers each poll with no deliverable proxy client, as a range of inactive
// proxies reads, reporting the newest change id it covered, and records the
// change id each poll started from.
type testProxyInactiveChangeReads struct {
	stateLock    sync.Mutex
	newestChange int64
	startIds     map[string][]int64
}

func (self *testProxyInactiveChangeReads) getProxyClientsSince(
	ctx context.Context,
	proxyHost string,
	block string,
	changeId int64,
) (map[server.Id]*model.ProxyClient, int64, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.startIds[proxyHost] = append(self.startIds[proxyHost], changeId)
	return map[server.Id]*model.ProxyClient{}, max(changeId, self.newestChange), nil
}

func (self *testProxyInactiveChangeReads) starts(proxyHost string) []int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]int64(nil), self.startIds[proxyHost]...)
}

func TestProxyClientNotificationMovesPastInactiveChanges(t *testing.T) {
	t.Setenv("WARP_HOST", "synthetic-proxy")
	t.Setenv("WARP_BLOCK", "g7")
	t.Setenv("WARP_DOMAIN", "example")
	synctest.Test(t, func(t *testing.T) {
		settings := DefaultProxySettings()
		settings.NotificationTimeout = time.Minute
		settings.ReconcileTimeout = 0
		subscriptionSource := &server.RedisShardSubscriptionSource{
			Open: func(ctx context.Context, channel string) (server.RedisShardSubscription, error) {
				return &testProxyChangeSocket{channel: channel, events: make(chan any, 64), closed: make(chan struct{})}, nil
			},
		}
		reads := &testProxyInactiveChangeReads{newestChange: 41, startIds: map[string][]int64{}}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		notification := newProxyClientNotification(ctx, settings, subscriptionSource, reads.getProxyClientsSince)
		synctest.Wait()
		select {
		case <-notification.InitialSyncDone():
		default:
			t.Fatal("an initial read of only inactive proxies did not complete the initial sync")
		}

		time.Sleep(3 * settings.NotificationTimeout)
		synctest.Wait()
		starts := reads.starts("synthetic-proxy")
		if len(starts) < 3 {
			t.Fatalf("the watch polled %d times in three notification timeouts", len(starts))
		}
		if starts[0] != 0 {
			t.Fatalf("the first poll started from change %d, want 0", starts[0])
		}
		for i, start := range starts[1:] {
			if start < reads.newestChange {
				t.Fatalf("poll %d started from change %d, behind the newest change %d the first poll covered: the watch re-reads a range it can never deliver", i+1, start, reads.newestChange)
			}
		}
	})
}
