package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func installAuthTestClient(t *testing.T, client RedisClient) {
	t.Helper()
	previous := safeAuthClient
	safeAuthClient = &safeRedisClient{client: client, disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}
	t.Cleanup(func() { safeAuthClient = previous; _ = client.Close() })
}

// The real constructor owns timeout, retry and capacity policy. The copied
// options use synthetic net.Pipe sockets; they cannot contact a deployment.
func authTestRedisOptions(t *testing.T, peer *deadlineRedisPeer) *redis.Options {
	t.Helper()
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 32\n"))
	defer popConfig()
	pool := &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}
	client, ok := pool.open().(*redis.Client)
	if !ok {
		t.Fatal("authentication constructor did not make standalone pool")
	}
	options := *client.Options()
	pool.close()
	if !options.ContextTimeoutEnabled || options.MaxRetries != 0 || options.DialerRetries != 1 || options.PoolSize != 32 || options.MaxActiveConns != 32 || options.MinIdleConns != 0 || options.ReadTimeout != 2*time.Second || options.WriteTimeout != 2*time.Second || options.PoolTimeout != 2*time.Second || options.DialTimeout != 2*time.Second {
		t.Fatal("authentication pool lost isolated normal capacity or finite policy")
	}
	options.Protocol, options.DisableIdentity, options.Password = 2, true, ""
	options.MaxRetries = -1 // normalized zero would re-enable defaults on reconstruction
	options.Addr, options.Dialer = "retirement.invalid:6379", peer.dial
	return &options
}

func TestRedisAuthNoPreflightAndTotalBudget(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "ping")
	installAuthTestClient(t, redis.NewClient(authTestRedisOptions(t, peer)))
	start := time.Now()
	calls := 0
	err := RedisAuth(t.Context(), func(ctx context.Context, r RedisClient) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Before(start.Add(1900*time.Millisecond)) || deadline.After(time.Now().Add(2*time.Second)) {
			t.Fatal("authentication callback did not share finite total budget", deadline)
		}
		return r.Eval(ctx, "return 0", nil).Err()
	})
	if err != nil || calls != 1 || peer.count("ping") != 0 || peer.count("eval") != 1 || peer.dials.Load() != 1 {
		t.Fatal("authentication performed preflight or replay", err, calls, peer.count("ping"), peer.count("eval"), peer.dials.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = RedisAuth(ctx, func(context.Context, RedisClient) error { calls++; return nil }); !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("cancelled authentication admitted work", err, calls)
	}
}

func TestRedisAuthNativeStallsKeepCallerDeadlineAndDoNotReplay(t *testing.T) {
	for _, phase := range []string{"hello", "write", "get", "eval"} {
		t.Run(phase, func(t *testing.T) {
			peer := newDeadlineRedisPeer(t, phase)
			installAuthTestClient(t, redis.NewClient(authTestRedisOptions(t, peer)))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			parentDeadline, _ := ctx.Deadline()
			err := RedisAuth(ctx, func(operationCtx context.Context, r RedisClient) error {
				deadline, ok := operationCtx.Deadline()
				if !ok || !deadline.Equal(parentDeadline) {
					t.Fatal("caller deadline extended", deadline, parentDeadline)
				}
				if phase == "eval" {
					return r.Eval(operationCtx, "return 0", nil).Err()
				}
				return r.Get(operationCtx, "synthetic-auth-marker").Err()
			})
			if err == nil || peer.dials.Load() != 1 || peer.count("ping") != 0 || peer.count("get") > 1 || peer.count("eval") > 1 {
				t.Fatal("fault was swallowed, preflighted or replayed", err, peer.dials.Load())
			}
		})
	}
}

func TestRedisAuthPoolExhaustionSharesDeadline(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "")
	options := authTestRedisOptions(t, peer)
	options.PoolSize, options.MaxActiveConns = 1, 1
	client := redis.NewClient(options)
	installAuthTestClient(t, client)
	held := client.Conn()
	defer held.Close()
	if err := held.Get(t.Context(), "warm").Err(); !errors.Is(err, RedisNil) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := RedisAuth(ctx, func(operationCtx context.Context, r RedisClient) error { return r.Get(operationCtx, "queued").Err() })
	if err == nil || peer.dials.Load() != 1 || peer.count("get") != 1 {
		t.Fatal("pool exhaustion escaped deadline or created extra I/O", err, peer.dials.Load(), peer.count("get"))
	}
}

func TestRedisAuthClusterRedirectBoundAndColdMetadata(t *testing.T) {
	peer := newDeadlineRedisPeer(t, "command")
	peer.redirect = true
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: true\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 32\n"))
	pool := &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}
	client := pool.open().(*redis.ClusterClient)
	options := *client.Options()
	pool.close()
	popConfig()
	popVault()
	if options.MaxRedirects != 2 || options.MaxRetries > 0 || !options.DisableRoutingPolicies || options.PoolSize != 32 || options.ReadTimeout != 2*time.Second {
		t.Fatalf("cluster authentication lost finite routing policy: redirects=%d retries=%d routingDisabled=%t pool=%d readTimeout=%s", options.MaxRedirects, options.MaxRetries, options.DisableRoutingPolicies, options.PoolSize, options.ReadTimeout)
	}
	options.Protocol, options.DisableIdentity, options.Password, options.MaxRetries = 2, true, "", -1
	options.Addrs = []string{"retirement.invalid:6379"}
	options.Dialer = peer.dial
	options.ClusterSlots = func(context.Context) ([]redis.ClusterSlot, error) {
		return []redis.ClusterSlot{{Start: 0, End: 16383, Nodes: []redis.ClusterNode{{Addr: "retirement.invalid:6379"}}}}, nil
	}
	installAuthTestClient(t, redis.NewClusterClient(&options))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := RedisAuth(ctx, func(operationCtx context.Context, r RedisClient) error {
		return r.Get(operationCtx, "synthetic-auth-marker").Err()
	})
	if err == nil || peer.count("get") < 1 || peer.count("get") > 3 || peer.count("command") != 0 || peer.count("ping") != 0 {
		t.Fatal("redirect escaped cap or detached metadata/preflight", err, peer.count("get"), peer.count("command"), peer.count("ping"))
	}
}

func TestRedisAuthResetDoesNotRetainPriorDatabaseScope(t *testing.T) {
	oldAuth := safeAuthClient
	safeAuthClient = &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}
	t.Cleanup(func() { safeAuthClient.close(); safeAuthClient = oldAuth })
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 16\n"))
	defer popConfig()
	popFirst := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 1\n"))
	first := safeAuthClient.open().(*redis.Client)
	if first.Options().DB != 1 {
		t.Fatal("wrong initial database")
	}
	RedisReset()
	if safeAuthClient.current() != nil {
		t.Fatal("reset retained authentication pool from prior database scope")
	}
	popFirst()
	popSecond := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 2\n"))
	defer popSecond()
	second := safeAuthClient.open().(*redis.Client)
	if second == first || second.Options().DB != 2 {
		t.Fatal("new scope reused prior authentication authority")
	}
}
