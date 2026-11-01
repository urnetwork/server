package server

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var clientCloseTakeTestMutex sync.Mutex

// The external consumer tests use the real wrappers and constructor policies,
// with independently owned pipe sockets. No production API or config is changed.
func TestingClientCloseTakeRedisPools(t *testing.T, dial func(context.Context, string, string) (net.Conn, error), hook redis.Hook, run func()) {
	t.Helper()
	clientCloseTakeTestMutex.Lock()
	defer clientCloseTakeTestMutex.Unlock()
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 192.0.2.1:6379\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 2\nmax_retries: 2\n"))
	defer popConfig()
	makePool := func(disableCommandRetry bool) *safeRedisClient {
		pool := &safeRedisClient{disableCommandRetry: disableCommandRetry}
		constructed, ok := pool.open().(*redis.Client)
		if !ok {
			t.Fatal("synthetic fixture constructor was not standalone")
		}
		options := *constructed.Options()
		pool.close()
		if options.MaxRetries == 0 {
			// go-redis normalizes -1 to zero; copying zero would enable defaults.
			options.MaxRetries = -1
		}
		options.Addr = "192.0.2.1:6379"
		options.Dialer = dial
		// The peer models command outcomes, not pipelined library identity.
		options.DisableIdentity = true
		options.ReadTimeout = 500 * time.Millisecond
		options.WriteTimeout = 500 * time.Millisecond
		options.PoolTimeout = 500 * time.Millisecond
		client := redis.NewClient(&options)
		if hook != nil {
			client.AddHook(hook)
		}
		pool.client = client
		return pool
	}
	ordinary, once := makePool(false), makePool(true)
	previousOrdinary, previousOnce := safeClient, safeNoCommandRetryClient
	safeClient, safeNoCommandRetryClient = ordinary, once
	defer func() {
		ordinary.close()
		once.close()
		safeClient, safeNoCommandRetryClient = previousOrdinary, previousOnce
	}()
	run()
}
