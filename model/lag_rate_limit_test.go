package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A prefix counter's key is the peppered hash of the /24 (/48): every address
// of the prefix shares it, and no form of the address or the prefix appears
// in it (lag.dev/PLAN.md A1, A6, amendment 9).
func TestLagPrefixRateLimitKey(t *testing.T) {
	for _, c := range []struct {
		addr  string
		same  string
		other string
		texts []string
	}{
		{"203.0.113.77", "203.0.113.200", "203.0.114.77", []string{"203.0.113", "cb0071"}},
		{"2001:db8:1234:5678::1", "2001:db8:1234:ffff::2", "2001:db8:1235::1", []string{"2001:db8", "20010db81234"}},
	} {
		addr := netip.MustParseAddr(c.addr)
		key := lagPrefixRateLimitKey("test", addr)
		if lagPrefixRateLimitKey("test", netip.MustParseAddr(c.same)) != key {
			t.Errorf("%s and %s are one caller, but their keys differ", c.addr, c.same)
		}
		if lagPrefixRateLimitKey("test", netip.MustParseAddr(c.other)) == key {
			t.Errorf("%s and %s are different callers, but share a key", c.addr, c.other)
		}
		for _, text := range append(c.texts, c.addr, hex.EncodeToString(addr.AsSlice())) {
			if strings.Contains(key, text) {
				t.Errorf("the key %q names %s as %q", key, c.addr, text)
			}
		}
	}
}

// A signing agent's counter key is a hash of its Signature-Agent value: one
// agent shares it wherever its browsers run, two agents never do, and the
// value itself is not in it (amendment 12).
func TestLagSignatureRateLimitKey(t *testing.T) {
	key := lagSignatureRateLimitKey("track", `"https://chatgpt.com"`)
	if lagSignatureRateLimitKey("track", `"https://chatgpt.com"`) != key {
		t.Error("one agent's key is not stable")
	}
	if lagSignatureRateLimitKey("track", `"https://agent.example"`) == key {
		t.Error("two agents share a key")
	}
	if lagSignatureRateLimitKey("session", `"https://chatgpt.com"`) == key {
		t.Error("two limits share a key")
	}
	for _, text := range []string{"chatgpt", "https"} {
		if strings.Contains(key, text) {
			t.Errorf("the key %q names the agent as %q", key, text)
		}
	}
	if !strings.HasPrefix(key, "{lag_track_signature_") {
		t.Errorf("the key %q is not in the lag limits' hash slot form", key)
	}
}

// A counter expires at the end of the window it counts, however late in the
// window its last count came, so no counter outlives the hour it limits
// (lag.dev/PLAN.md A6). A TTL of the whole window from each count would keep
// a counter last counted near the end of its window for almost two.
func TestLagRateLimitCounterExpiresAtItsWindowEnd(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const window = time.Hour
		windowMillis := window.Milliseconds()
		for {
			clientId := server.NewId()
			before := server.NowUtc()
			for range 2 {
				limit, err := CheckLagClientRateLimit(ctx, "test", clientId, 10, window)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, limit.Allowed, true)
			}
			windowIndex := before.UnixMilli() / windowMillis
			if server.NowUtc().UnixMilli()/windowMillis != windowIndex {
				// the counts fell in two windows; count again in the next
				continue
			}
			key := fmt.Sprintf("{lag_test_client_%s}_%d", clientId, windowIndex)
			var count int64
			var expireTime time.Duration
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				count, err = r.Get(ctx, key).Int64()
				server.Raise(err)
				expireTime, err = r.PExpireTime(ctx, key).Result()
				server.Raise(err)
			})
			connect.AssertEqual(t, count, int64(2))
			connect.AssertEqual(t, expireTime, time.Duration((windowIndex+1)*windowMillis)*time.Millisecond)
			return
		}
	})
}
