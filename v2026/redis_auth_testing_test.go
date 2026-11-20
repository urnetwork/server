package server

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The stalled-dial pool other packages' tests stand in for the 2026-11-18
// outage with must keep the authentication pool's own timeouts, retries and
// routing, or those tests would measure a pool production does not have.
func TestStalledDialAuthRedisKeepsTheAuthenticationPolicy(t *testing.T) {
	popVault := Vault.PushSimpleResource("redis.yml", []byte("cluster: true\nauthority: 127.0.0.1:1\npassword: synthetic\ndb: 0\n"))
	popConfig := Config.PushSimpleResource("redis.yml", []byte("min_connections: 0\nmax_connections: 32\n"))
	pool := &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}
	policy := *pool.open().(*redis.ClusterClient).Options()
	pool.close()
	popConfig()
	popVault()
	stalled := testingStalledDialAuthOptions(48, (&TestingStalledAuthRedis{masters: map[string]bool{}}).dial)
	if policy.MaxRetries > 0 || stalled.MaxRetries > 0 ||
		policy.MaxRedirects != stalled.MaxRedirects ||
		policy.DialerRetries != stalled.DialerRetries ||
		policy.DialTimeout != stalled.DialTimeout ||
		policy.ReadTimeout != stalled.ReadTimeout ||
		policy.WriteTimeout != stalled.WriteTimeout ||
		policy.PoolTimeout != stalled.PoolTimeout ||
		policy.PoolSize != stalled.PoolSize ||
		policy.ContextTimeoutEnabled != stalled.ContextTimeoutEnabled ||
		policy.DisableRoutingPolicies != stalled.DisableRoutingPolicies {
		t.Fatalf("the stalled pool drifted from the authentication pool: authentication %+v, stalled %+v", policy, *stalled)
	}
	slots, err := stalled.ClusterSlots(t.Context())
	if err != nil || len(slots) != 48 || slots[0].Start != 0 || slots[47].End != 16383 {
		t.Fatalf("48 masters do not cover every slot: %v %v", slots, err)
	}
	for i := 1; i < len(slots); i++ {
		if slots[i].Start != slots[i-1].End+1 || slots[i].Nodes[0].Addr == slots[i-1].Nodes[0].Addr {
			t.Fatalf("master %d does not follow master %d: %v %v", i, i-1, slots[i-1], slots[i])
		}
	}
}

// The stand-in reproduces the outage: every master's dial waits, a caller's
// deadline ends its command, and the dial itself fails as an i/o timeout. No
// socket is opened.
func TestStalledDialAuthRedisReproducesDialTimeouts(t *testing.T) {
	stalled, pop := Testing_StallAuthRedisDials(48)
	defer pop()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := RedisAuth(ctx, func(ctx context.Context, r RedisClient) error {
		return r.HGet(ctx, "{rv_synthetic_0}", "n:user").Err()
	})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("a stalled dial answered %v after %s", err, time.Since(start))
	}
	if stalled.Dials() < 1 || stalled.DialedMasters() != 1 {
		t.Fatalf("the command dialed %d times, %d masters", stalled.Dials(), stalled.DialedMasters())
	}
	// the dial attempt, with its own deadline, fails as the outage's did
	dialer := testingStalledDialAuthOptions(1, (&TestingStalledAuthRedis{masters: map[string]bool{}}).dial).Dialer
	dialCtx, cancelDial := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancelDial()
	_, dialErr := dialer(dialCtx, "tcp", "192.0.2.1:6390")
	var netErr net.Error
	if !errors.As(dialErr, &netErr) || !netErr.Timeout() || !strings.Contains(dialErr.Error(), "i/o timeout") {
		t.Fatalf("the stalled dial failed with %v, not an i/o timeout", dialErr)
	}
}
