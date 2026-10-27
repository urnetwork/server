package server

// The deadline pools against a node that answers late. go-redis closes a
// connection whose command failed with any timeout, so in the deadline pool,
// which puts its caller's deadline on its sockets, every reply or handshake
// that comes after the caller's deadline costs a new connection and HELLO on
// the node that was late. On 2026-10-26 the proxies' reservation calls opened
// up to 25k connections per second to single slow nodes that way, which held
// those nodes' main threads at 100% and halved their command throughput. The
// late-reply pool (OptRedisKeepLateReplies) waits such a reply out instead.
//
// Every socket is a net.Pipe inside a synctest bubble, so replies arrive at
// exact virtual times and each test counts dials and handshakes, not
// scheduling luck. No test reaches a deployment.

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
)

func lateReplyTestPool() *safeRedisClient {
	return &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, keepLateReplies: true}
}

// The production options of a standalone pool, with synthetic transport
// only: no dialer, RESP2 and no credential.
func deadlineTestPolicyOptions(t *testing.T, pool *safeRedisClient) redis.Options {
	t.Helper()
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 16\n"))
	defer popConfig()
	constructed, ok := pool.open().(*redis.Client)
	if !ok {
		t.Fatal("constructor did not make a standalone pool")
	}
	options := *constructed.Options()
	pool.close()
	// open() normalized the -1 retry sentinel to zero; a client rebuilt from
	// zero would restore go-redis' default retries
	options.MaxRetries = -1
	options.Protocol, options.DisableIdentity, options.Password = 2, true, ""
	options.Addr = deadlineTestRedisAddress
	return options
}

func installLateReplyTestClient(t *testing.T, client RedisClient) {
	t.Helper()
	original := safeLateReplyClient
	pool := lateReplyTestPool()
	pool.client = client
	safeLateReplyClient = pool
	t.Cleanup(func() { safeLateReplyClient = original; client.Close() })
}

// One deadline-bound GET whose caller waits 100ms, from start to return.
func deadlineTestLateGet(t *testing.T, options ...any) (time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := RedisWithDeadline(ctx, func(r RedisClient) error {
		if err := r.Get(ctx, "synthetic-late").Err(); !errors.Is(err, RedisNil) {
			return err
		}
		return nil
	}, append([]any{OptRedisNoPreflight()}, options...)...)
	return time.Since(start), err
}

// The deadline and authentication pools put their callers' deadlines on their
// sockets; the late-reply pool and the ordinary pools do not.
func TestRedisPoolsSocketDeadlineFromContext(t *testing.T) {
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 16\n"))
	defer popConfig()
	for _, p := range []struct {
		name string
		pool *safeRedisClient
		want bool
	}{
		{"main", &safeRedisClient{}, false},
		{"no_command_retry", &safeRedisClient{disableCommandRetry: true}, false},
		{"deadline", &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true}, true},
		{"late_reply", lateReplyTestPool(), false},
		{"authentication", &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}, true},
	} {
		client, ok := p.pool.open().(*redis.Client)
		if !ok {
			t.Fatalf("%s: constructor did not make a standalone pool", p.name)
		}
		got := client.Options().ContextTimeoutEnabled
		p.pool.close()
		if got != p.want || p.pool.socketDeadlineFromContext() != p.want {
			t.Errorf("%s: socket deadline from context = %t, want %t", p.name, got, p.want)
		}
	}
}

// OptRedisKeepLateReplies runs the call on the late-reply pool, and every
// other call stays on the deadline pool.
func TestRedisWithDeadlineKeepLateRepliesSelectsItsPool(t *testing.T) {
	lateOptions := deadlineTestPolicyOptions(t, lateReplyTestPool())
	synctest.Test(t, func(t *testing.T) {
		deadlinePeer, latePeer := newDeadlineRedisPeer(t, ""), newDeadlineRedisPeer(t, "")
		installDeadlineTestClient(t, redis.NewClient(deadlineTestRedisOptions(deadlinePeer)))
		lateOptions.Dialer = latePeer.dial
		installLateReplyTestClient(t, redis.NewClient(&lateOptions))
		for _, late := range []bool{true, false, true} {
			var options []any
			if late {
				options = append(options, OptRedisKeepLateReplies())
			}
			if _, err := deadlineTestLateGet(t, options...); err != nil {
				t.Fatal(late, err)
			}
		}
		if deadlinePeer.count("get") != 1 || latePeer.count("get") != 2 {
			t.Fatalf("calls reached the wrong pool: deadline gets=%d late gets=%d", deadlinePeer.count("get"), latePeer.count("get"))
		}
	})
}

// RedisReset closes the late-reply pool with the others.
func TestRedisResetClosesLateReplyPool(t *testing.T) {
	options := deadlineTestPolicyOptions(t, lateReplyTestPool())
	synctest.Test(t, func(t *testing.T) {
		peer := newDeadlineRedisPeer(t, "")
		options.Dialer = peer.dial
		installLateReplyTestClient(t, redis.NewClient(&options))
		if _, err := deadlineTestLateGet(t, OptRedisKeepLateReplies()); err != nil {
			t.Fatal(err)
		}
		RedisReset()
		if safeLateReplyClient.current() != nil {
			t.Fatal("reset retained the late-reply pool")
		}
	})
}

// The pool telemetry counts the late-reply pool with the others.
func TestRedisPoolStatsCountLateReplyPool(t *testing.T) {
	options := deadlineTestPolicyOptions(t, lateReplyTestPool())
	synctest.Test(t, func(t *testing.T) {
		peer := newDeadlineRedisPeer(t, "")
		options.Dialer = peer.dial
		installLateReplyTestClient(t, redis.NewClient(&options))
		conns := func(stats *redis.PoolStats) float64 { return float64(stats.TotalConns) }
		misses := func(stats *redis.PoolStats) float64 { return float64(stats.Misses) }
		connsBefore, missesBefore := redisPoolStat(conns), redisPoolStat(misses)
		if _, err := deadlineTestLateGet(t, OptRedisKeepLateReplies()); err != nil {
			t.Fatal(err)
		}
		if gotConns, gotMisses := redisPoolStat(conns)-connsBefore, redisPoolStat(misses)-missesBefore; gotConns != 1 || gotMisses != 1 {
			t.Fatalf("pool telemetry missed the late-reply pool: conns +%v misses +%v, want +1 and +1", gotConns, gotMisses)
		}
	})
}

// The deadline pool keeps its callers' deadlines on its sockets, which the
// handler cache's renewal and the liveness leases rely on: a late reply
// returns the caller at its deadline, and costs the connection.
func TestRedisWithDeadlineDefaultPoolReturnsAtCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const late = 300 * time.Millisecond
		peer := newDeadlineRedisPeer(t, "")
		peer.late = map[string]time.Duration{"get": late}
		installDeadlineTestClient(t, redis.NewClient(deadlineTestRedisOptions(peer)))

		const calls = 2
		for call := range calls {
			elapsed, err := deadlineTestLateGet(t)
			if !errors.Is(err, context.DeadlineExceeded) || elapsed != 100*time.Millisecond {
				t.Fatalf("call %d: err=%v elapsed=%s, want the deadline at 100ms", call, err, elapsed)
			}
		}
		if dials := peer.dials.Load(); dials != calls {
			t.Fatalf("dials=%d for %d late replies", dials, calls)
		}
	})
}

// A reply that comes after its caller's deadline ends that caller's
// operation, not the connection: each later caller is served on the same
// connection, without a dial or a HELLO. The caller waits for the reply its
// command is owed, within the pool's one-second socket timeout.
func TestRedisWithDeadlineLateReplyKeepsItsConnection(t *testing.T) {
	options := deadlineTestPolicyOptions(t, lateReplyTestPool())
	synctest.Test(t, func(t *testing.T) {
		const late = 300 * time.Millisecond
		peer := newDeadlineRedisPeer(t, "")
		peer.late = map[string]time.Duration{"get": late}
		options.Dialer = peer.dial
		client := redis.NewClient(&options)
		installLateReplyTestClient(t, client)

		const calls = 4
		elapsed := make([]time.Duration, calls)
		errs := make([]error, calls)
		for call := range calls {
			elapsed[call], errs[call] = deadlineTestLateGet(t, OptRedisKeepLateReplies())
		}
		if dials, hellos, gets := peer.dials.Load(), peer.count("hello"), peer.count("get"); dials != 1 || hellos != 1 || gets != calls {
			t.Fatalf("late replies cost connections: dials=%d hellos=%d gets=%d for %d calls", dials, hellos, gets, calls)
		}
		for call := range calls {
			if !errors.Is(errs[call], context.DeadlineExceeded) {
				t.Fatalf("call %d: a reply after the deadline was not the caller's deadline: %v", call, errs[call])
			}
			if elapsed[call] != late {
				t.Fatalf("call %d returned after %s, want its reply at %s", call, elapsed[call], late)
			}
		}
		if stats := client.PoolStats(); stats.Misses != 1 || stats.Hits != calls-1 || stats.TotalConns != 1 {
			t.Fatalf("pool did not reuse its connection: misses=%d hits=%d conns=%d", stats.Misses, stats.Hits, stats.TotalConns)
		}
	})
}

// A handshake that completes after its caller's deadline keeps its
// connection for the next caller. A node too slow to answer HELLO within the
// callers' budgets used to accept and handshake a new connection for every
// caller and finish none of them.
func TestRedisWithDeadlineLateHandshakeKeepsItsConnection(t *testing.T) {
	options := deadlineTestPolicyOptions(t, lateReplyTestPool())
	synctest.Test(t, func(t *testing.T) {
		const late = 300 * time.Millisecond
		peer := newDeadlineRedisPeer(t, "")
		peer.late = map[string]time.Duration{"hello": late}
		options.Dialer = peer.dial
		installLateReplyTestClient(t, redis.NewClient(&options))

		const calls = 4
		elapsed := make([]time.Duration, calls)
		errs := make([]error, calls)
		for call := range calls {
			elapsed[call], errs[call] = deadlineTestLateGet(t, OptRedisKeepLateReplies())
		}
		if dials, hellos := peer.dials.Load(), peer.count("hello"); dials != 1 || hellos != 1 {
			t.Fatalf("late handshakes cost connections: dials=%d hellos=%d for %d calls", dials, hellos, calls)
		}
		// The first caller's deadline passed during the handshake, which its
		// GET then followed: the caller reports its deadline, and the GET's
		// outcome is as unknown to it as a lost reply's.
		if !errors.Is(errs[0], context.DeadlineExceeded) || elapsed[0] != late || peer.count("get") != calls {
			t.Fatalf("first caller: err=%v elapsed=%s gets=%d", errs[0], elapsed[0], peer.count("get"))
		}
		for call := 1; call < calls; call++ {
			if errs[call] != nil || elapsed[call] != 0 {
				t.Fatalf("call %d on the handshaken connection: err=%v elapsed=%s", call, errs[call], elapsed[call])
			}
		}
	})
}

// A reply that never comes ends at the pool's one-second socket timeout, the
// bound of a caller's wait past its deadline, and closes its connection: only
// a connection whose reply was read serves another caller.
func TestRedisWithDeadlineMissingReplyEndsAtSocketTimeout(t *testing.T) {
	options := deadlineTestPolicyOptions(t, lateReplyTestPool())
	if options.ReadTimeout != time.Second || options.WriteTimeout != time.Second {
		t.Fatal("late-reply pool socket timeouts changed", options.ReadTimeout, options.WriteTimeout)
	}
	synctest.Test(t, func(t *testing.T) {
		peer := newDeadlineRedisPeer(t, "get")
		options.Dialer = peer.dial
		installLateReplyTestClient(t, redis.NewClient(&options))

		for call := range 2 {
			elapsed, err := deadlineTestLateGet(t, OptRedisKeepLateReplies())
			if !errors.Is(err, context.DeadlineExceeded) || elapsed != options.ReadTimeout {
				t.Fatalf("call %d: err=%v elapsed=%s, want the deadline at the socket timeout", call, err, elapsed)
			}
		}
		if dials, hellos := peer.dials.Load(), peer.count("hello"); dials != 2 || hellos != 2 {
			t.Fatalf("a connection without its reply was reused: dials=%d hellos=%d", dials, hellos)
		}
	})
}

// The production cluster client keeps the node connection a late reply
// arrives on.
func TestRedisWithDeadlineClusterLateReplyKeepsItsConnection(t *testing.T) {
	options := deadlineTestClusterPolicyOptions(t, lateReplyTestPool())
	synctest.Test(t, func(t *testing.T) {
		const late = 300 * time.Millisecond
		peer := newDeadlineRedisPeer(t, "")
		peer.late = map[string]time.Duration{"get": late}
		options.Dialer = peer.dial
		installLateReplyTestClient(t, redis.NewClusterClient(options))

		const calls = 4
		elapsed := make([]time.Duration, calls)
		errs := make([]error, calls)
		for call := range calls {
			elapsed[call], errs[call] = deadlineTestLateGet(t, OptRedisKeepLateReplies())
		}
		if dials, hellos, gets := peer.dials.Load(), peer.count("hello"), peer.count("get"); dials != 1 || hellos != 1 || gets != calls {
			t.Fatalf("late replies cost cluster connections: dials=%d hellos=%d gets=%d for %d calls", dials, hellos, gets, calls)
		}
		for call := range calls {
			if !errors.Is(errs[call], context.DeadlineExceeded) || elapsed[call] != late {
				t.Fatalf("call %d: err=%v elapsed=%s, want the deadline when the reply came at %s", call, errs[call], elapsed[call], late)
			}
		}
	})
}
