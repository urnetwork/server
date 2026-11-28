//go:build linux || darwin

package server

// The pools' skip of go-redis's per-checkout socket check (LBALTOPT2 R19;
// third_party/go-redis/URNETWORK.md), against the local Redis: which pools
// skip it, a fresh connection skipping it, and a pooled connection Redis
// closed found by the check past the window, or within it by the command,
// which the pool's retries replay, so neither reaches the caller as an
// error. The checks are counted through the connection's RawConn.Read,
// which go-redis's peek goes through, and Redis closes the connection with
// CLIENT KILL, as a restart or a kill closes it.

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Pools that retry their commands skip the check within a second; pools
// that don't, which -1 and 0 retries mean, check every checkout.
func TestRedisSkipConnCheckWithinOnlyWhereCommandsRetry(t *testing.T) {
	for _, c := range []struct {
		maxRetries int
		want       time.Duration
	}{
		{maxRetries: 4, want: time.Second},
		{maxRetries: 1, want: time.Second},
		{maxRetries: 0, want: 0},
		{maxRetries: -1, want: 0},
	} {
		if got := redisSkipConnCheckWithin(c.maxRetries); got != c.want {
			t.Errorf("%d retries: %s, want %s", c.maxRetries, got, c.want)
		}
	}
}

// The pool's connections, each counting its socket checks and writes.
type redisConnCheckDialer struct {
	dial      func(ctx context.Context, network string, address string) (net.Conn, error)
	stateLock sync.Mutex
	conns     []*redisConnCheckCountedConn
}

// See redis.Options.Dialer.
func (self *redisConnCheckDialer) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	conn, err := self.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	countedConn := &redisConnCheckCountedConn{
		Conn: conn,
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.conns = append(self.conns, countedConn)
	return countedConn, nil
}

// The connections dialed so far, in order.
func (self *redisConnCheckDialer) dialed() []*redisConnCheckCountedConn {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*redisConnCheckCountedConn(nil), self.conns...)
}

// A connection whose socket checks and writes are counted.
type redisConnCheckCountedConn struct {
	net.Conn
	checks atomic.Int64
	writes atomic.Int64
}

// See net.Conn.
func (self *redisConnCheckCountedConn) Write(p []byte) (int, error) {
	self.writes.Add(1)
	return self.Conn.Write(p)
}

// See syscall.Conn: go-redis's check peeks through the RawConn's Read.
func (self *redisConnCheckCountedConn) SyscallConn() (syscall.RawConn, error) {
	rawConn, err := self.Conn.(syscall.Conn).SyscallConn()
	if err != nil {
		return nil, err
	}
	return &redisConnCheckCountedRawConn{
		RawConn: rawConn,
		checks:  &self.checks,
	}, nil
}

// A RawConn counting its reads, the socket checks.
type redisConnCheckCountedRawConn struct {
	syscall.RawConn
	checks *atomic.Int64
}

// See syscall.RawConn.
func (self *redisConnCheckCountedRawConn) Read(f func(fd uintptr) bool) error {
	self.checks.Add(1)
	return self.RawConn.Read(f)
}

// Whether conn's socket has read its peer's close, by a peek that consumes
// nothing and isn't counted.
func redisConnCheckPeerClosed(conn net.Conn) bool {
	rawConn, err := conn.(syscall.Conn).SyscallConn()
	if err != nil {
		return true
	}
	closed := false
	rawConn.Read(func(fd uintptr) bool {
		var buf [1]byte
		n, _, err := syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		closed = n == 0 && err == nil
		return true
	})
	return closed
}

// The default pool's options for the local Redis, which a test's own
// clients connect with. A cluster's skips the test.
func redisConnCheckProductionOptions(t testing.TB) *redis.Options {
	t.Helper()
	var production *redis.Options
	Redis(context.Background(), func(client RedisClient) {
		standalone, ok := client.(*redis.Client)
		if !ok {
			return
		}
		production = standalone.Options()
	})
	if production == nil {
		t.Skip("the local Redis is a cluster")
	}
	return production
}

// A client of the local Redis with one pooled connection from dialer, the
// window and retries given, and the server's ttl hook, as the pools have it.
func newRedisConnCheckClient(production *redis.Options, dialer *redisConnCheckDialer, window time.Duration, maxRetries int) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:                production.Addr,
		Username:            production.Username,
		Password:            production.Password,
		DB:                  production.DB,
		Dialer:              dialer.dialContext,
		MaxRetries:          maxRetries,
		PoolSize:            1,
		MaxIdleConns:        1,
		DialTimeout:         5 * time.Second,
		ReadTimeout:         5 * time.Second,
		WriteTimeout:        5 * time.Second,
		PoolTimeout:         5 * time.Second,
		SkipConnCheckWithin: window,
	})
	client.AddHook(redisTtlWarnHook{})
	return client
}

// Has Redis close the client's one pooled connection, and waits until the
// connection's socket has read the close.
func killRedisConnCheckPooledConn(t testing.TB, client *redis.Client, conn *redisConnCheckCountedConn) {
	t.Helper()
	ctx := context.Background()
	id, err := client.ClientID(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	var killed int64
	Redis(ctx, func(killer RedisClient) {
		killed, err = killer.ClientKillByFilter(ctx, "ID", strconv.FormatInt(id, 10)).Result()
	})
	if err != nil || killed != 1 {
		t.Fatalf("CLIENT KILL killed %d: %v", killed, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !redisConnCheckPeerClosed(conn.Conn) {
		if time.Now().After(deadline) {
			t.Fatal("the pooled connection never read Redis's close")
		}
		time.Sleep(time.Millisecond)
	}
}

// Each pool's window: a second for the pool whose commands retry, none for
// the pools that don't.
func TestRedisPoolsSkipTheConnCheckOnlyWhereCommandsRetry(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		window := func(client RedisClient) time.Duration {
			switch typed := client.(type) {
			case *redis.Client:
				return typed.Options().SkipConnCheckWithin
			case *redis.ClusterClient:
				return typed.Options().SkipConnCheckWithin
			default:
				t.Fatalf("unexpected client type %T", client)
				return 0
			}
		}
		var windows [5]time.Duration
		Redis(ctx, func(client RedisClient) {
			windows[0] = window(client)
		})
		RedisDoOnce(ctx, func(client RedisClient) {
			windows[1] = window(client)
		})
		deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		RedisWithDeadline(deadlineCtx, func(client RedisClient) error {
			windows[2] = window(client)
			return nil
		})
		RedisWithDeadline(deadlineCtx, func(client RedisClient) error {
			windows[3] = window(client)
			return nil
		}, OptRedisKeepLateReplies())
		RedisAuth(deadlineCtx, func(ctx context.Context, client RedisClient) error {
			windows[4] = window(client)
			return nil
		})
		if windows != [5]time.Duration{time.Second, 0, 0, 0, 0} {
			t.Fatalf("windows of the default, once, deadline, late-reply and auth pools: %v", windows)
		}
	})
}

// Against the local Redis: a fresh connection skips the check command after
// command, where a pool without the window checks every checkout; a pooled
// connection Redis closes within the window is found by the command, which
// the pool's retries replay on a new connection, without an error, after
// which every checkout checks for the window; one closed longer ago is found
// by the check before any command, as is one in a pool without retries,
// which checks every checkout.
//
// Counts start once a connection has served its first command: go-redis
// peeks a new connection's socket for RESP3 push notifications while it
// initializes it, and at its first release, as at the release of one put
// back more than 5 s before, which the window doesn't change.
func TestRedisDeadPooledConnectionIsHandled(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		production := redisConnCheckProductionOptions(t)
		key := fmt.Sprintf("lbaltopt2_r19_%d", time.Now().UnixNano())
		Redis(ctx, func(client RedisClient) {
			Raise(client.Set(ctx, key, "v", time.Minute).Err())
		})

		get := func(client *redis.Client) {
			t.Helper()
			value, err := client.Get(ctx, key).Result()
			if err != nil || value != "v" {
				t.Fatalf("GET: %q, %v", value, err)
			}
		}

		// within the window, by the command
		dialer := &redisConnCheckDialer{dial: production.Dialer}
		client := newRedisConnCheckClient(production, dialer, time.Second, 4)
		defer client.Close()
		get(client)
		first := dialer.dialed()[0]
		checks := first.checks.Load()
		for range 5 {
			get(client)
		}
		if checked := first.checks.Load() - checks; checked != 0 {
			t.Fatalf("a connection put back within the window was checked %d times", checked)
		}
		killRedisConnCheckPooledConn(t, client, first)
		checks, writes := first.checks.Load(), first.writes.Load()
		// found by the command, which the pool's retries replay
		get(client)
		if conns := dialer.dialed(); len(conns) != 2 {
			t.Fatalf("%d connections, want the closed one and its replacement", len(conns))
		}
		if checked, written := first.checks.Load()-checks, first.writes.Load()-writes; checked != 0 || written != 1 {
			t.Fatalf("the closed connection was checked %d times and written %d, want 0 and the one command", checked, written)
		}
		// the removal has the next checkout check, though the connection is
		// fresh
		second := dialer.dialed()[1]
		checks = second.checks.Load()
		get(client)
		if checked := second.checks.Load() - checks; checked != 1 {
			t.Fatalf("the checkout after a removal checked %d times, want 1", checked)
		}

		// past the window, by the check
		dialer = &redisConnCheckDialer{dial: production.Dialer}
		window := 100 * time.Millisecond
		client = newRedisConnCheckClient(production, dialer, window, 4)
		defer client.Close()
		get(client)
		first = dialer.dialed()[0]
		killRedisConnCheckPooledConn(t, client, first)
		checks, writes = first.checks.Load(), first.writes.Load()
		time.Sleep(window + 50*time.Millisecond)
		get(client)
		if checked, written := first.checks.Load()-checks, first.writes.Load()-writes; checked != 1 || written != 0 {
			t.Fatalf("the closed connection was checked %d times and written %d, want 1 and none", checked, written)
		}

		// a pool without retries checks every checkout, and finds the closed
		// connection before its command
		dialer = &redisConnCheckDialer{dial: production.Dialer}
		client = newRedisConnCheckClient(production, dialer, redisSkipConnCheckWithin(-1), -1)
		defer client.Close()
		get(client)
		first = dialer.dialed()[0]
		checks = first.checks.Load()
		for range 5 {
			get(client)
		}
		if checked := first.checks.Load() - checks; checked != 5 {
			t.Fatalf("a pool without the window checked %d of 5 checkouts", checked)
		}
		killRedisConnCheckPooledConn(t, client, first)
		checks, writes = first.checks.Load(), first.writes.Load()
		get(client)
		if checked, written := first.checks.Load()-checks, first.writes.Load()-writes; checked != 1 || written != 0 {
			t.Fatalf("the closed connection of a pool without retries was checked %d more times and written %d, want 1 and none", checked, written)
		}
	})
}

// The process's user and system CPU time so far.
func redisConnCheckCpuTime() time.Duration {
	var usage syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// One GET a command against the local Redis, through a pool with the
// server's ttl hook, checking every checkout as upstream and skipping the
// check within a second as the default pool now does: serially, so each
// command checks out the one idle connection, and from GOMAXPROCS
// goroutines over as many connections. cpu-ns/op is the process's CPU time
// per command; ns/op is mostly the round trip. Against a Redis a millisecond
// away, as the local one was on 2026-11-20, the runtime's park and wake
// around each round trip vary by more than the check costs, so the
// checkout's own saving is timed apart, with nothing blocking, by
// third_party/go-redis's BenchmarkUrnetworkGetPut.
func BenchmarkRedisConnCheck(b *testing.B) {
	if err := preflightTestEnvironment(probeTestEnvironmentService); err != nil {
		b.Skipf("local integration test preflight: %v", err)
	}
	teardown := (&TestEnv{ApplyDbMigrations: false}).setup()
	defer teardown()
	ctx := context.Background()
	production := redisConnCheckProductionOptions(b)
	key := fmt.Sprintf("lbaltopt2_r19_bench_%d", time.Now().UnixNano())
	Redis(ctx, func(client RedisClient) {
		Raise(client.Set(ctx, key, "v", time.Hour).Err())
	})

	for _, c := range []struct {
		name     string
		window   time.Duration
		parallel bool
	}{
		{name: "check_every_checkout", window: 0},
		{name: "skip_within_1s", window: time.Second},
		{name: "check_every_checkout_parallel", window: 0, parallel: true},
		{name: "skip_within_1s_parallel", window: time.Second, parallel: true},
	} {
		b.Run(c.name, func(b *testing.B) {
			poolSize := 1
			if c.parallel {
				// one connection for each of RunParallel's goroutines
				poolSize = runtime.GOMAXPROCS(0)
			}
			client := redis.NewClient(&redis.Options{
				Addr:                production.Addr,
				Username:            production.Username,
				Password:            production.Password,
				DB:                  production.DB,
				Dialer:              production.Dialer,
				MaxRetries:          4,
				PoolSize:            poolSize,
				MinIdleConns:        poolSize,
				MaxIdleConns:        poolSize,
				SkipConnCheckWithin: c.window,
			})
			client.AddHook(redisTtlWarnHook{})
			defer client.Close()
			if err := client.Get(ctx, key).Err(); err != nil {
				b.Fatal(err)
			}
			cpuBefore := redisConnCheckCpuTime()
			b.ResetTimer()
			if c.parallel {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if err := client.Get(ctx, key).Err(); err != nil {
							b.Error(err)
							return
						}
					}
				})
			} else {
				for range b.N {
					if err := client.Get(ctx, key).Err(); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(redisConnCheckCpuTime()-cpuBefore)/float64(b.N), "cpu-ns/op")
		})
	}
	Redis(ctx, func(client RedisClient) {
		client.Del(ctx, key)
	})
}
