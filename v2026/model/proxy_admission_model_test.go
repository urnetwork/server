// Pins the admission-only Redis boundary and preserves the legacy reader path.
package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Observes the actual pool selected by the model without contacting Redis.
// The socket-level deadline is separately exercised through the HTTP handler.
type proxyAdmissionRedisHook struct {
	deadlinePool bool
	block        bool
	entered      chan bool
	release      <-chan struct{}
}

// Every operation in these cases is intercepted before any connection opens.
func (self *proxyAdmissionRedisHook) DialHook(redis.DialHook) redis.DialHook {
	return func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("unexpected admission test socket")
	}
}

// Reports which production pool reached the command and preserves its context.
func (self *proxyAdmissionRedisHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		if command.Name() != "get" && command.Name() != "eval" {
			return next(ctx, command)
		}
		self.entered <- self.deadlinePool
		if self.block {
			select {
			case <-ctx.Done():
			case <-self.release:
			}
			command.SetErr(ctx.Err())
			return ctx.Err()
		}
		command.(*redis.StringCmd).SetVal("{}")
		return nil
	}
}

// No pipeline is used by these admission reads.
func (self *proxyAdmissionRedisHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Installs observers before either pool dials; both retain their real options.
func installProxyAdmissionRedisHooks(t *testing.T, block bool, entered chan bool, release <-chan struct{}) {
	t.Helper()
	server.RedisReset()
	t.Cleanup(server.RedisReset)
	t.Cleanup(server.Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.1:6379\npassword: synthetic\ndb: 0\n")))
	t.Cleanup(server.Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 8\n")))
	server.Redis(t.Context(), func(client server.RedisClient) {
		client.AddHook(&proxyAdmissionRedisHook{block: block, entered: entered, release: release})
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		client.AddHook(&proxyAdmissionRedisHook{deadlinePool: true, block: block, entered: entered, release: release})
		return nil
	}, server.OptRedisNoPreflight()); err != nil {
		t.Fatal(err)
	}
}

// A timed-out entitlement read must not become either a denial or fail-open
// authorization, and must use the Redis socket boundary that honors deadlines.
func TestProxyFeatureAdmissionRedisReadPreservesDeadline(t *testing.T) {
	config := Pro()
	previous := config.EnforceFeatures
	config.EnforceFeatures = true
	t.Cleanup(func() { config.EnforceFeatures = previous })
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan bool, 1), make(chan struct{})
		releaseRead := sync.OnceFunc(func() { close(release) })
		defer releaseRead()
		installProxyAdmissionRedisHooks(t, true, entered, release)
		proxyId, networkId := server.NewId(), server.NewId()
		proxyNetworkCacheMutex.Lock()
		proxyNetworkCache[proxyId] = networkId
		proxyNetworkCacheMutex.Unlock()
		t.Cleanup(func() {
			proxyNetworkCacheMutex.Lock()
			delete(proxyNetworkCache, proxyId)
			proxyNetworkCacheMutex.Unlock()
		})
		ctx, cancel := context.WithTimeout(server.WithoutPostgres(t.Context()), time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			raised := server.HandleError(func() { _ = ProxyFeatureAllowedForAdmission(ctx, proxyId, FeatureSocksProxy) })
			err, _ := raised.(error)
			result <- err
		}()
		if deadlinePool := <-entered; !deadlinePool {
			t.Error("feature admission used Redis sockets that ignore caller deadlines")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired plan lookup returned a decision instead of its deadline: %v", err)
		}
		if _, found := getProNetworkLocal(networkId); found || server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("expired entitlement read filled the cache or fell through to PostgreSQL")
		}
	})
}

// Refresh's cache write stays within the same request budget as its read.
func TestProxyFeatureAdmissionRedisRefreshPreservesDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan bool, 1), make(chan struct{})
		releaseRead := sync.OnceFunc(func() { close(release) })
		defer releaseRead()
		installProxyAdmissionRedisHooks(t, true, entered, release)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			raised := server.HandleError(func() {
				_, _ = setProNetworkCached(proxyAdmissionContext(ctx), server.NewId(), proEntitlement{pro: true, version: 1})
			})
			err, _ := raised.(error)
			result <- err
		}()
		if deadlinePool := <-entered; !deadlinePool {
			t.Error("admission refresh used Redis sockets that ignore caller deadlines")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired entitlement write lost its deadline: %v", err)
		}
	})
}

// Existing configuration readers, including callers of the admission API that
// have no request deadline, retain their ordinary Redis behavior.
func TestProxyAdmissionPreservesLegacyRedisReaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan bool, 1)
		installProxyAdmissionRedisHooks(t, false, entered, nil)
		for _, read := range []func(context.Context, server.Id) *ProxyDeviceConfig{GetProxyDeviceConfig, GetProxyDeviceConfigForAdmission} {
			if config := read(t.Context(), server.NewId()); config == nil {
				t.Fatal("legacy reader lost its configuration")
			}
			if deadlinePool := <-entered; deadlinePool {
				t.Fatal("legacy caller was moved to the admission deadline pool")
			}
		}
	})
}
