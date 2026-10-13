package proxy

// A change read that raises must not end the watch or the reconcile of a proxy
// block. server.Db raises a refused read, such as a pooler wait timeout or a
// database that is shutting down, instead of returning it. The loops run on
// synthetic shard sockets and a change fixture that raises the same way, and
// all timing runs on the synctest clock.

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The change id of the one proxy client the fixture serves for a host.
const testProxyChangeReadChangeId = int64(7)

// Serves at most one proxy client per host and raises each host's queued
// failures before reading. Safe for concurrent use.
type testProxyChangeReadFailures struct {
	stateLock          sync.Mutex
	hostRaiseCounts    map[string]int
	hostProxyClients   map[string]*model.ProxyClient
	hostReadCounts     map[string]int
	hostFullReadCounts map[string]int
}

func newTestProxyChangeReadFailures() *testProxyChangeReadFailures {
	return &testProxyChangeReadFailures{
		hostRaiseCounts:    map[string]int{},
		hostProxyClients:   map[string]*model.ProxyClient{},
		hostReadCounts:     map[string]int{},
		hostFullReadCounts: map[string]int{},
	}
}

func (self *testProxyChangeReadFailures) getProxyClientsSince(
	ctx context.Context,
	proxyHost string,
	block string,
	changeId int64,
) (map[server.Id]*model.ProxyClient, int64, error) {
	raise, proxyClient := func() (bool, *model.ProxyClient) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.hostReadCounts[proxyHost] += 1
		if changeId == 0 {
			self.hostFullReadCounts[proxyHost] += 1
		}
		if 0 < self.hostRaiseCounts[proxyHost] {
			self.hostRaiseCounts[proxyHost] -= 1
			return true, nil
		}
		return false, self.hostProxyClients[proxyHost]
	}()
	if raise {
		// the value server.Db raised in production when the pooler refused the read
		panic(&pgconn.PgError{Severity: "FATAL", Code: "08P01", Message: "query_wait_timeout"})
	}
	proxyClients := map[server.Id]*model.ProxyClient{}
	if proxyClient != nil && changeId <= testProxyChangeReadChangeId {
		proxyClients[proxyClient.ProxyId] = proxyClient
	}
	return proxyClients, max(changeId, testProxyChangeReadChangeId), nil
}

func (self *testProxyChangeReadFailures) raise(proxyHost string, count int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.hostRaiseCounts[proxyHost] += count
}

func (self *testProxyChangeReadFailures) readCount(proxyHost string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hostReadCounts[proxyHost]
}

func (self *testProxyChangeReadFailures) fullReadCount(proxyHost string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.hostFullReadCounts[proxyHost]
}

// Opens synthetic sockets that never acknowledge, so no subscription wake
// adds a poll and every poll is a timed one.
func newTestProxyChangeReadSubscriptionSource() *server.RedisShardSubscriptionSource {
	return &server.RedisShardSubscriptionSource{
		Open: func(ctx context.Context, channel string) (server.RedisShardSubscription, error) {
			return &testProxyChangeSocket{
				channel: channel,
				events:  make(chan any, 64),
				closed:  make(chan struct{}),
			}, nil
		},
	}
}

// The first change read of the fq-host watch raises. On main the raise ended
// the watch goroutine for the life of the process: the timed poll never ran
// again, the waiting proxy client was never delivered (no warmup and no
// WireGuard peer), and the initial sync never completed.
func TestProxyClientNotificationWatchRetriesRaisedChangeRead(t *testing.T) {
	t.Setenv("WARP_HOST", "synthetic-proxy")
	t.Setenv("WARP_BLOCK", "g3")
	t.Setenv("WARP_DOMAIN", "example")
	synctest.Test(t, func(t *testing.T) {
		host := "synthetic-proxy"
		fqHost := "synthetic-proxy.example"

		settings := DefaultProxySettings()
		settings.ReconcileTimeout = 0

		proxyClient := &model.ProxyClient{
			ProxyId:   server.NewId(),
			ProxyHost: fqHost,
			Block:     "g3",
		}
		reads := newTestProxyChangeReadFailures()
		reads.hostProxyClients[fqHost] = proxyClient
		reads.raise(fqHost, 1)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		notification := newProxyClientNotification(
			ctx,
			settings,
			newTestProxyChangeReadSubscriptionSource(),
			reads.getProxyClientsSince,
		)
		deliveredProxyIds := make(chan server.Id, 16)
		sub := notification.AddProxyClientsCallback(func(proxyClients []*model.ProxyClient) error {
			for _, proxyClient := range proxyClients {
				deliveredProxyIds <- proxyClient.ProxyId
			}
			return nil
		})
		defer sub()

		synctest.Wait()
		if count := reads.readCount(fqHost); count != 1 {
			t.Fatalf("initial fq-host reads=%d, want 1", count)
		}
		if count := reads.readCount(host); count != 1 {
			t.Fatalf("initial host reads=%d, want 1", count)
		}
		select {
		case <-notification.InitialSyncDone():
			t.Fatal("the initial sync completed before the fq-host watch read its changes")
		default:
		}

		time.Sleep(settings.NotificationTimeout)
		synctest.Wait()
		if count := reads.readCount(fqHost); count != 2 {
			t.Fatalf("fq-host reads=%d after the timed poll, want 2: the watch ended after a raised change read", count)
		}
		select {
		case deliveredProxyId := <-deliveredProxyIds:
			if deliveredProxyId != proxyClient.ProxyId {
				t.Fatalf("delivered proxy id=%s, want %s", deliveredProxyId, proxyClient.ProxyId)
			}
		default:
			t.Fatal("the proxy client waiting behind the raised read was never delivered")
		}
		select {
		case <-notification.InitialSyncDone():
		default:
			t.Fatal("the initial sync did not complete after the retried read")
		}
	})
}

// The first full read of a reconcile round raises. On main the raise ended the
// reconcile goroutine, so no later round healed the WireGuard peers or removed
// the stale ones. The timed watch poll is pushed out of reach, so every read
// after the initial ones belongs to a reconcile round.
func TestProxyClientNotificationReconcileRetriesRaisedChangeRead(t *testing.T) {
	t.Setenv("WARP_HOST", "synthetic-proxy")
	t.Setenv("WARP_BLOCK", "g3")
	t.Setenv("WARP_DOMAIN", "example")
	synctest.Test(t, func(t *testing.T) {
		host := "synthetic-proxy"
		fqHost := "synthetic-proxy.example"

		settings := DefaultProxySettings()
		settings.NotificationTimeout = 10 * time.Hour

		proxyClient := &model.ProxyClient{
			ProxyId:   server.NewId(),
			ProxyHost: fqHost,
			Block:     "g3",
		}
		reads := newTestProxyChangeReadFailures()
		reads.hostProxyClients[fqHost] = proxyClient

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		notification := newProxyClientNotification(
			ctx,
			settings,
			newTestProxyChangeReadSubscriptionSource(),
			reads.getProxyClientsSince,
		)
		sub := notification.AddProxyClientsCallback(func(proxyClients []*model.ProxyClient) error {
			return nil
		})
		defer sub()
		var fullSyncStateLock sync.Mutex
		fullSyncProxyClientCounts := []int{}
		fullSyncSub := notification.AddProxyClientsFullSyncCallback(func(proxyClients []*model.ProxyClient, syncStartTime time.Time) {
			fullSyncStateLock.Lock()
			defer fullSyncStateLock.Unlock()
			fullSyncProxyClientCounts = append(fullSyncProxyClientCounts, len(proxyClients))
		})
		defer fullSyncSub()
		getFullSyncProxyClientCounts := func() []int {
			fullSyncStateLock.Lock()
			defer fullSyncStateLock.Unlock()
			return append([]int{}, fullSyncProxyClientCounts...)
		}

		synctest.Wait()
		select {
		case <-notification.InitialSyncDone():
		default:
			t.Fatal("the initial reads did not complete the initial sync")
		}
		// the watches' initial reads are the only full reads so far
		if count := reads.fullReadCount(host); count != 1 {
			t.Fatalf("host full reads=%d before the first round, want 1", count)
		}

		// the first round's first read is the host's, and it raises
		reads.raise(host, 1)
		time.Sleep(settings.ReconcileTimeout)
		synctest.Wait()
		if count := reads.fullReadCount(host); count != 2 {
			t.Fatalf("host full reads=%d after the first round, want 2", count)
		}
		if counts := getFullSyncProxyClientCounts(); len(counts) != 0 {
			t.Fatalf("full syncs=%v after a raised read, want none from partial data", counts)
		}

		time.Sleep(settings.ReconcileTimeout)
		synctest.Wait()
		if count := reads.fullReadCount(host); count != 3 {
			t.Fatalf("host full reads=%d after the second round, want 3: the reconcile ended after a raised change read", count)
		}
		counts := getFullSyncProxyClientCounts()
		if len(counts) != 1 || counts[0] != 1 {
			t.Fatalf("full syncs=%v after the second round, want one sync of 1 proxy client", counts)
		}
	})
}
