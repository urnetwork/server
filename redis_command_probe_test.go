package server

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The real go-redis client speaks RESP over the existing net.Pipe peer. No
// resolver, external socket, fixture database, or Main credential is used.
func newRedisCommandProbeClient(t *testing.T) (*deadlineRedisPeer, *redis.Client, *safeRedisClient) {
	t.Helper()
	peer := newDeadlineRedisPeer(t, "")
	client := redis.NewClient(deadlineTestRedisOptions(peer))
	t.Cleanup(func() { client.Close() })
	return peer, client, &safeRedisClient{client: client}
}

func redisCommandProbePanic(run func()) (result any) {
	defer func() { result = recover() }()
	run()
	return nil
}

func redisCommandProbeRetry() DbRetryOptions {
	options := OptRetryDefault()
	options.retryMinTimeout = time.Millisecond
	options.retryMaxTimeout = time.Millisecond
	options.endRetryTimeout = time.Second
	return options
}

// A settled connection supplies exactly 100 useful commands. Count protocol
// commands rather than timing assertions; the baseline sends 100 extra PINGs.
func TestRedisCommandProbeHealthyCallbacks(t *testing.T) {
	peer, client, pool := newRedisCommandProbeClient(t)
	ctx := t.Context()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	beforePing, beforeSet := peer.count("ping"), peer.count("set")
	beforeDials := peer.dials.Load()
	const count = 100
	for range count {
		redisWithClient(ctx, pool, func(r RedisClient) {
			Raise(r.Set(ctx, "command-probe", "value", 0).Err())
		}, OptNoRetry())
	}
	pings, work := peer.count("ping")-beforePing, peer.count("set")-beforeSet
	if work != count || peer.dials.Load() != beforeDials {
		t.Fatalf("warm command ownership changed: work=%d extra_dials=%d", work, peer.dials.Load()-beforeDials)
	}
	t.Logf("healthy_callbacks=%d ping_roundtrips=%d useful_roundtrips=%d", count, pings, work)
	if pings != 0 {
		t.Fatalf("healthy callbacks paid %d unnecessary Redis PING round trips", pings)
	}
}

// Connection recovery belongs to the command and existing callback retry
// boundary. A failed dial cannot have applied the eventual write. The direct
// command path may enter the callback before that first dial fails.
func TestRedisCommandProbeReconnect(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	options := deadlineTestRedisOptions(peer)
	var dials atomic.Int64
	options.Dialer = func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("synthetic i/o timeout")}
		}
		return peer.dial(ctx, network, address)
	}
	client := redis.NewClient(options)
	defer client.Close()
	calls := 0
	redisWithClient(t.Context(), &safeRedisClient{client: client}, func(r RedisClient) {
		calls++
		Raise(r.Set(t.Context(), "reconnect-command", "once", 0).Err())
	}, redisCommandProbeRetry())
	if dials.Load() != 2 || peer.count("set") != 1 || calls < 1 || calls > 2 {
		t.Fatalf("recovery changed write ownership: dials=%d callbacks=%d writes=%d", dials.Load(), calls, peer.count("set"))
	}
	t.Logf("failed_first_dial_then_recovered callbacks=%d dials=%d committed_work=%d", calls, dials.Load(), peer.count("set"))
}

type redisCommandProbeLostReply struct{ dropped atomic.Bool }

func (h *redisCommandProbeLostReply) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *redisCommandProbeLostReply) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h *redisCommandProbeLostReply) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		err := next(ctx, command)
		if err == nil && command.Name() == "incrby" && !h.dropped.Swap(true) {
			return errors.New("synthetic connection reset by peer after applying increment")
		}
		return err
	}
}

// Lose the acknowledgement after the peer accepted a non-idempotent command.
// OptNoRetry must not replay the callback. The real command retry setting is
// independent and remains unchanged by the wrapper optimization.
func TestRedisCommandProbeNoRetryAppliedWrite(t *testing.T) {
	peer, client, pool := newRedisCommandProbeClient(t)
	client.AddHook(&redisCommandProbeLostReply{})
	calls := 0
	got := redisCommandProbePanic(func() {
		redisWithClient(t.Context(), pool, func(r RedisClient) {
			calls++
			Raise(r.Do(t.Context(), "incrby", "owned-counter", 1).Err())
		}, OptNoRetry())
	})
	if got == nil || calls != 1 || peer.count("incrby") != 1 || client.Options().MaxRetries != 0 {
		t.Fatalf("ambiguous increment replayed: callbacks=%d applied=%d command_retries=%d panic=%v", calls, peer.count("incrby"), client.Options().MaxRetries, got)
	}
}

func TestRedisCommandProbeCanceledBeforeAdmission(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_retry", true: "default_retry"}[retry], func(t *testing.T) {
			peer, _, pool := newRedisCommandProbeClient(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			options := OptNoRetry()
			if retry {
				options = redisCommandProbeRetry()
			}
			calls := 0
			got := redisCommandProbePanic(func() {
				redisWithClient(ctx, pool, func(RedisClient) { calls++ }, options)
			})
			err, _ := got.(error)
			if calls != 0 || !errors.Is(err, context.Canceled) || errors.Is(err, DbContextDoneError) != retry {
				t.Fatalf("canceled admission changed: callbacks=%d retry=%v panic=%v", calls, retry, got)
			}
			if peer.dials.Load() != 0 {
				t.Fatal("pre-canceled callback dialed Redis")
			}
		})
	}
}

func TestRedisCommandProbeCancellationRetainsPhysicalCause(t *testing.T) {
	_, _, pool := newRedisCommandProbeClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	physical := errors.New("synthetic connection reset by peer")
	calls := 0
	got := redisCommandProbePanic(func() {
		redisWithClient(ctx, pool, func(RedisClient) {
			calls++
			cancel()
			panic(physical)
		}, redisCommandProbeRetry())
	})
	err, _ := got.(error)
	if calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, physical) || !errors.Is(err, DbContextDoneError) {
		t.Fatalf("retry cancellation lost original cause: callbacks=%d panic=%v", calls, got)
	}
}

func TestRedisCommandProbeCallbackOutcomes(t *testing.T) {
	t.Run("handled_nil_and_pipeline", func(t *testing.T) {
		peer, _, pool := newRedisCommandProbeClient(t)
		calls := 0
		redisWithClient(t.Context(), pool, func(r RedisClient) {
			calls++
			if err := r.Get(t.Context(), "missing").Err(); !errors.Is(err, RedisNil) {
				t.Fatalf("missing Redis value changed: %v", err)
			}
			_, err := r.Pipelined(t.Context(), func(pipe redis.Pipeliner) error {
				pipe.Set(t.Context(), "one", "1", 0)
				pipe.Set(t.Context(), "two", "2", 0)
				return nil
			})
			Raise(err)
		}, redisCommandProbeRetry())
		if calls != 1 || peer.count("get") != 1 || peer.count("set") != 2 {
			t.Fatal("handled missing value or pipeline changed callback ownership")
		}
	})
	for _, tc := range []struct {
		name string
		want any
	}{
		{"redis_nil", RedisNil},
		{"application_error", errors.New("application callback refused")},
		{"pool_timeout", errors.New("redis: connection pool timeout")},
		{"non_error_panic", "callback sentinel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, pool := newRedisCommandProbeClient(t)
			calls := 0
			got := redisCommandProbePanic(func() {
				redisWithClient(t.Context(), pool, func(RedisClient) { calls++; panic(tc.want) }, redisCommandProbeRetry())
			})
			if calls != 1 || got != tc.want {
				t.Fatalf("permanent callback outcome changed: callbacks=%d want=%v got=%v", calls, tc.want, got)
			}
		})
	}
}
