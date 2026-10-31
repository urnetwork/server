package server

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// A connection whose handshake fails must close its socket. go-redis v9.22.0
// marked a failed handshake's connection closed before the pool closed it, and
// the pool's close then skipped the socket (redis/go-redis#3982): each timed
// out HELLO held its socket open until garbage collection. On 2026-10-25 a slow
// cluster node's handshakes timed out under the proxies' load, and the leaked
// sockets filled the node with tens of thousands of idle clients and exhausted
// the proxy hosts' ephemeral ports to it.

type closeTrackingConn struct {
	net.Conn
	closed atomic.Bool
}

func (self *closeTrackingConn) Close() error {
	self.closed.Store(true)
	return self.Conn.Close()
}

// Records every socket a pool dials through the synthetic peer.
type closeTrackingDialer struct {
	peer  *deadlineRedisPeer
	mutex sync.Mutex
	conns []*closeTrackingConn
}

func (self *closeTrackingDialer) dial(ctx context.Context, network string, addr string) (net.Conn, error) {
	conn, err := self.peer.dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	tracked := &closeTrackingConn{Conn: conn}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.conns = append(self.conns, tracked)
	return tracked, nil
}

// Returns how many sockets were dialed and how many of them are still open.
func (self *closeTrackingDialer) openCount() (dialed int, open int) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	for _, conn := range self.conns {
		if !conn.closed.Load() {
			open += 1
		}
	}
	return len(self.conns), open
}

// The real constructor's options for a pool, with synthetic net.Pipe sockets
// that cannot contact a deployment. The idle floor is zero so that every
// socket the test sees was dialed for a handshake.
func handshakeTestRedisOptions(t *testing.T, pool *safeRedisClient, dialer *closeTrackingDialer) *redis.Options {
	t.Helper()
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: false\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	defer popVault()
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 16\n"))
	defer popConfig()
	client, ok := pool.open().(*redis.Client)
	if !ok {
		t.Fatal("constructor did not make a standalone pool")
	}
	options := *client.Options()
	pool.close()
	options.Password = ""
	if options.MaxRetries == 0 {
		options.MaxRetries = -1 // a normalized zero would re-enable the default retries on reconstruction
	}
	// the pools' real read timeouts are seconds long; closing a failed
	// handshake's socket does not depend on them
	options.ReadTimeout, options.WriteTimeout = 100*time.Millisecond, 100*time.Millisecond
	options.Addr, options.Dialer = deadlineTestRedisAddress, dialer.dial
	return &options
}

func TestRedisPoolsCloseSocketsWhoseHandshakeFailed(t *testing.T) {
	pools := []struct {
		name string
		pool *safeRedisClient
	}{
		{"main", &safeRedisClient{}},
		{"no_command_retry", &safeRedisClient{disableCommandRetry: true}},
		{"deadline", &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true}},
		{"late_reply", &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, keepLateReplies: true}},
		{"authentication", &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}},
	}
	for _, p := range pools {
		t.Run(p.name, func(t *testing.T) {
			// the peer never answers HELLO
			dialer := &closeTrackingDialer{peer: newDeadlineRedisPeer(t, "hello")}
			client := redis.NewClient(handshakeTestRedisOptions(t, p.pool, dialer))
			t.Cleanup(func() { _ = client.Close() })

			for range 3 {
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				err := client.Get(ctx, "key").Err()
				cancel()
				if err == nil || errors.Is(err, redis.Nil) {
					t.Fatal("a command ran without a handshake", err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			subscription := client.SSubscribe(ctx, "channel")
			cancel()
			_ = subscription.Close()

			if dialer.peer.count("hello") == 0 {
				t.Fatal("no handshake was attempted")
			}
			// every failed handshake closes its socket before its caller returns;
			// the wait only absorbs scheduling
			deadline := time.Now().Add(2 * time.Second)
			for {
				dialed, open := dialer.openCount()
				if open == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%d of %d sockets stayed open after their handshakes failed", open, dialed)
				}
				select {
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
	}
}
