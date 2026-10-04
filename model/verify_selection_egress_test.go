// Provider assignment rechecks live address ownership instead of trusting a
// stale eligible-set member through expiry, ambiguity, or reconnect cleanup.
package model

import (
	"context"
	"encoding/hex"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A singleton set makes every stale-address failure deterministic. None may
// consume an eligibility token; a subsequent valid connection re-enrolls it.
func TestVerifySelectionRejectsStaleEgressMembership(t *testing.T) {
	for _, mutation := range []string{"expired-reverse", "missing-reverse", "missing-forward", "ambiguous-forward", "foreign-forward", "multiple-addresses"} {
		t.Run(mutation, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				settings := DefaultVerifySettings()
				settings.EgressHashKey = []byte("synthetic-selection-egress")
				clientId := server.NewId()
				ip := netip.MustParseAddr("192.0.2.17")
				hash := VerifyEgressIndexHashWithSettings(ip, settings)
				hashHex := hex.EncodeToString(hash[:])
				testVerifySetProvideModes(ctx, clientId)
				FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), ip, settings)
				server.Redis(ctx, func(r server.RedisClient) {
					switch mutation {
					case "expired-reverse":
						server.Raise(r.HSet(ctx, verifyClientEgressKey(clientId), hashHex, server.NowUtc().Add(-time.Second).UnixMilli()).Err())
					case "missing-reverse":
						server.Raise(r.Del(ctx, verifyClientEgressKey(clientId)).Err())
					case "missing-forward":
						server.Raise(r.Del(ctx, verifyEgressKey(hash)).Err())
					case "ambiguous-forward":
						server.Raise(r.Set(ctx, verifyEgressKey(hash), verifyEgressAmbiguous, settings.EgressTtl).Err())
					case "foreign-forward":
						server.Raise(r.Set(ctx, verifyEgressKey(hash), server.NewId().String(), settings.EgressTtl).Err())
					case "multiple-addresses":
						other := VerifyEgressIndexHashWithSettings(netip.MustParseAddr("192.0.2.18"), settings)
						server.Raise(r.HSet(ctx, verifyClientEgressKey(clientId), hex.EncodeToString(other[:]), server.NowUtc().Add(time.Minute).UnixMilli()).Err())
					}
				})
				if !testVerifyEligible(ctx, clientId) {
					t.Fatal("fixture did not retain stale candidate membership")
				}
				if next, _ := SampleVerifyNextHop(ctx, nil, settings); next != nil {
					t.Fatalf("unattributable provider was assigned: %s", *next)
				}
				if testVerifyEligible(ctx, clientId) {
					t.Fatal("stale candidate was not evicted")
				}
				server.Redis(ctx, func(r server.RedisClient) {
					count, err := r.Exists(ctx, verifyEligibilityTokenKey(clientId)).Result()
					server.Raise(err)
					if count != 0 {
						t.Fatal("unattributable candidate spent an eligibility token")
					}
				})
			})
		})
	}
}

// A reconnect restores sampling only after publishing both sides of the new
// lease; an old overlapping connection's later cleanup cannot withdraw it.
func TestVerifySelectionRechecksReconnectAndOverlap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		clientId, oldId, nextId := server.NewId(), server.NewId(), server.NewId()
		ip := netip.MustParseAddr("192.0.2.33")
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		ClearVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.SAdd(ctx, verifyEligibleKey, clientId.String()).Err())
		})
		if next, _ := SampleVerifyNextHop(ctx, nil, settings); next != nil {
			t.Fatal("disconnected source was assigned before its replacement lease")
		}
		FeedVerifyConnectionEgress(ctx, clientId, nextId, ip, settings)
		ClearVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		if next, _ := SampleVerifyNextHop(ctx, nil, settings); next == nil || *next != clientId {
			t.Fatalf("replacement source was not sampled: %v", next)
		}
		if source := ResolveVerifyEgress(ctx, ip, settings); source == nil || *source != clientId {
			t.Fatal("sampled replacement cannot pass source attribution")
		}
	})
}
