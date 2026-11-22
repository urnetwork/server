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

// Testing_DelayAuthRedisCommands holds every command and pipeline of the
// authentication pool for delay before it is sent, or until its context ends:
// a Redis that answers, but later than a check budget, as the proxy
// processes' in-process calls did on 2026-11-21 (5-15% of them past 250ms,
// before any connection was used). It reopens the pool with the hold; the
// returned function closes that pool, so the next call opens one without.
// Tests only, against the local test Redis.
func Testing_DelayAuthRedisCommands(delay time.Duration) (pop func()) {
	safeAuthClient.reset()
	client := safeAuthClient.open()
	client.AddHook(testingAuthRedisDelay{delay: delay})
	return func() {
		if safeAuthClient.current() == client {
			safeAuthClient.reset()
		}
	}
}

// Holds each command and pipeline for delay before it goes on, or ends it
// with its context's error, unsent, when the context ends first.
type testingAuthRedisDelay struct {
	delay time.Duration
}

// The connection handshake passes unheld: go-redis sends a new connection's
// HELLO, AUTH, SELECT, READONLY and CLIENT commands through the client's
// hooks too, and the stand-in holds what a caller sends, not each new
// connection's setup.
func testingAuthRedisHandshake(cmd redis.Cmder) bool {
	switch cmd.Name() {
	case "hello", "auth", "select", "readonly", "client":
		return true
	}
	return false
}

func (self testingAuthRedisDelay) hold(ctx context.Context) error {
	timer := time.NewTimer(self.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (self testingAuthRedisDelay) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self testingAuthRedisDelay) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if testingAuthRedisHandshake(cmd) {
			return next(ctx, cmd)
		}
		if err := self.hold(ctx); err != nil {
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (self testingAuthRedisDelay) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		handshake := true
		for _, cmd := range cmds {
			handshake = handshake && testingAuthRedisHandshake(cmd)
		}
		if handshake {
			return next(ctx, cmds)
		}
		if err := self.hold(ctx); err != nil {
			for _, cmd := range cmds {
				cmd.SetErr(err)
			}
			return err
		}
		return next(ctx, cmds)
	}
}
