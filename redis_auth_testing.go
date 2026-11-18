package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// The dial attempts of a stalled authentication pool.
type TestingStalledAuthRedis struct {
	dials       atomic.Int64
	mastersLock sync.Mutex
	masters     map[string]bool
}

// Every dial attempt so far.
func (self *TestingStalledAuthRedis) Dials() int64 {
	return self.dials.Load()
}

// The distinct masters dialed so far.
func (self *TestingStalledAuthRedis) DialedMasters() int {
	self.mastersLock.Lock()
	defer self.mastersLock.Unlock()
	return len(self.masters)
}

func (self *TestingStalledAuthRedis) dial(ctx context.Context, network string, addr string) (net.Conn, error) {
	self.dials.Add(1)
	self.mastersLock.Lock()
	self.masters[addr] = true
	self.mastersLock.Unlock()
	<-ctx.Done()
	return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrDeadlineExceeded}
}

// Testing_StallAuthRedisDials replaces the authentication pool's client with
// a cluster client over nodes synthetic masters whose every dial attempt waits
// out its deadline and then fails as an i/o timeout: the fleet-wide bursts of
// "redis: connection pool: failed to dial ... i/o timeout" of 2026-11-18. The
// masters have TEST-NET-1 addresses and are never dialed; no socket is opened.
// It returns the dial attempts and a function that restores the previous
// client. Tests only.
func Testing_StallAuthRedisDials(nodes int) (stalled *TestingStalledAuthRedis, pop func()) {
	stalled = &TestingStalledAuthRedis{masters: map[string]bool{}}
	client := redis.NewClusterClient(testingStalledDialAuthOptions(nodes, stalled.dial))
	safeAuthClient.mutex.Lock()
	previous := safeAuthClient.client
	safeAuthClient.client = client
	safeAuthClient.mutex.Unlock()
	pop = func() {
		safeAuthClient.mutex.Lock()
		if safeAuthClient.client == client {
			safeAuthClient.client = previous
		}
		safeAuthClient.mutex.Unlock()
		_ = client.Close()
	}
	return
}

// The authentication pool's policy (safeRedisClient.open, authentication),
// over synthetic masters that split the slots evenly.
func testingStalledDialAuthOptions(nodes int, dial redisDialFunc) *redis.ClusterOptions {
	nodes = max(1, min(nodes, 250))
	addrs := make([]string, nodes)
	slots := make([]redis.ClusterSlot, nodes)
	for i := range nodes {
		addrs[i] = fmt.Sprintf("192.0.2.%d:6390", i+1)
		slots[i] = redis.ClusterSlot{
			Start: i * 16384 / nodes,
			End:   (i+1)*16384/nodes - 1,
			Nodes: []redis.ClusterNode{{Addr: addrs[i]}},
		}
	}
	const connections = 32
	return &redis.ClusterOptions{
		Addrs:                  addrs,
		ClusterSlots:           func(context.Context) ([]redis.ClusterSlot, error) { return slots, nil },
		Dialer:                 dial,
		Protocol:               2,
		DisableIdentity:        true,
		MaxRetries:             -1,
		MaxRedirects:           2,
		DialerRetries:          1,
		DialTimeout:            2 * time.Second,
		ReadTimeout:            2 * time.Second,
		WriteTimeout:           2 * time.Second,
		PoolTimeout:            2 * time.Second,
		PoolSize:               connections,
		MaxIdleConns:           connections,
		MaxActiveConns:         connections,
		ContextTimeoutEnabled:  true,
		DisableRoutingPolicies: true,
	}
}
