package server

// These roots own the default resolver and deny every DNS dial before the
// real mock-client constructor runs. They are deliberately nonparallel and
// never start TestEnv.Run, a service connection, or a database command.

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// Resolver callbacks can run concurrently for address families. The guard
// owns no socket; its atomic count remains valid until all lookups return.
type testEnvRedisDNSGuard struct {
	dispatchCount atomic.Int64
	resolver      *net.Resolver
}

// Installs before any constructor and restores after all later client cleanup.
func newTestEnvRedisDNSGuard(t *testing.T) *testEnvRedisDNSGuard {
	t.Helper()
	guard := &testEnvRedisDNSGuard{}
	guard.resolver = &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		guard.dispatchCount.Add(1)
		return nil, errors.New("owned Redis DNS guard denies network dispatch")
	}}
	previous := net.DefaultResolver
	net.DefaultResolver = guard.resolver
	t.Cleanup(func() {
		if net.DefaultResolver != guard.resolver {
			t.Error("owned Redis DNS resolver was replaced before cleanup")
		}
		net.DefaultResolver = previous
	})
	return guard
}

// A deny/count canary excludes a broken observation seam as zero-DNS evidence.
func TestTestEnvRedisMockDNSGuardDeniesResolution(t *testing.T) {
	guard := newTestEnvRedisDNSGuard(t)
	if _, err := net.DefaultResolver.LookupIPAddr(t.Context(), "urnetwork-owned-dns-canary.invalid."); err == nil || guard.dispatchCount.Load() == 0 {
		t.Fatalf("owned resolver canary did not observe and deny DNS: calls=%d error=%v", guard.dispatchCount.Load(), err)
	}
}

// The unchanged fixture's command and Dialer hooks are too late for Options
// endpoint discovery. The failure is the counted attempted dispatch, not time.
func TestTestEnvRedisMockConstructorRejectsDNSBeforeHooks(t *testing.T) {
	guard := newTestEnvRedisDNSGuard(t)
	fixture := newTestEnvRedisDispatchFixture(t)
	if calls := guard.dispatchCount.Load(); calls != 0 {
		t.Fatalf("Redis mock constructor dispatched DNS before command hooks: calls=%d", calls)
	}
	if len(fixture.snapshot()) != 0 {
		t.Fatal("Redis mock constructor unexpectedly dispatched a command")
	}
	teardown := fixture.setup(2)
	requireTestEnvRedisDispatchPair(t, fixture.snapshot(), 2)
	teardown()
	requireTestEnvRedisDispatchPair(t, fixture.snapshot()[2:], 2)
	if calls := guard.dispatchCount.Load(); calls != 0 {
		t.Fatalf("Redis mock replacement constructor dispatched DNS before command hooks: calls=%d", calls)
	}
}

// Literal-IP constructors are the adjacent existing mock pattern. Neither
// standalone nor cluster construction should consult DNS or open transport.
func TestTestEnvRedisMockLiteralConstructorsAvoidDNS(t *testing.T) {
	guard := newTestEnvRedisDNSGuard(t)
	var dialCount atomic.Int64
	deny := func(context.Context, string, string) (net.Conn, error) {
		dialCount.Add(1)
		return nil, errors.New("owned Redis literal constructor denies transport")
	}
	standalone := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, Dialer: deny})
	cluster := redis.NewClusterClient(&redis.ClusterOptions{Addrs: []string{"127.0.0.1:1"}, MaxRetries: -1, MaxRedirects: -1, Dialer: deny})
	if err := standalone.Close(); err != nil {
		t.Error(err)
	}
	if err := cluster.Close(); err != nil {
		t.Error(err)
	}
	if guard.dispatchCount.Load() != 0 || dialCount.Load() != 0 {
		t.Fatalf("literal constructor attempted network discovery: dns=%d dial=%d", guard.dispatchCount.Load(), dialCount.Load())
	}
}

// Disabling maintenance notifications alone does not disable Options endpoint
// autodetection. This positive adjacency pins the necessary explicit endpoint.
func TestTestEnvRedisMockDisabledMaintenanceStillDetectsDNS(t *testing.T) {
	guard := newTestEnvRedisDNSGuard(t)
	var dialCount atomic.Int64
	client := redis.NewClient(&redis.Options{
		Addr: "owned-redis.invalid:6379", MaxRetries: -1,
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			dialCount.Add(1)
			return nil, errors.New("owned disabled-maintenance constructor denies transport")
		},
	})
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if guard.dispatchCount.Load() == 0 || dialCount.Load() != 0 {
		t.Fatalf("disabled-maintenance constructor boundary differs: dns=%d dial=%d", guard.dispatchCount.Load(), dialCount.Load())
	}
}
