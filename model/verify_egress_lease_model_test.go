// Exercises transport lease overlap, slot-local interleavings, and expiration
// against the fixture's isolated Redis database; no production state is used.
package model

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Requires the existing source bijection and eligibility gate to agree.
func requireVerifyEgressLeaseOwner(t testing.TB, ctx context.Context, ip netip.Addr, clientId server.Id, settings *VerifySettings) {
	t.Helper()
	got := ResolveVerifyEgress(ctx, ip, settings)
	if got == nil || *got != clientId {
		t.Fatalf("live overlapping transport lost source attribution: got=%v want=%s", got, clientId)
	}
	if !testVerifyEligible(ctx, clientId) {
		t.Fatal("live overlapping transport lost eligible membership")
	}
}

// H1, H3, and a replacement generation can overlap at one client/address;
// either cleanup order must retain the surviving owner without another refresh.
func TestVerifyConnectionEgressOverlap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("deterministic-connection-egress-test")
		for order := 0; order < 3; order++ {
			clientId := server.NewId()
			connectionIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
			ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", order+1))
			testVerifySetProvideModes(ctx, clientId)
			for _, connectionId := range connectionIds {
				FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
			}
			for index := 0; index < 2; index++ {
				connectionId := connectionIds[(order+index)%len(connectionIds)]
				ClearVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
				ClearVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
				requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
			}
			ClearVerifyConnectionEgress(ctx, clientId, connectionIds[(order+2)%3], ip, settings)
			if ResolveVerifyEgress(ctx, ip, settings) != nil || testVerifyEligible(ctx, clientId) {
				t.Fatal("last transport cleanup retained attribution or eligibility")
			}
		}
	})
}

// Force a replacement feed between the old cleanup's two Redis slots. Neither
// forward nor reverse cleanup may erase it, including a repeated stale cleanup.
func TestVerifyConnectionEgressInterleavedCleanup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("interleaved-connection-egress-test")
		clientId, oldId, nextId := server.NewId(), server.NewId(), server.NewId()
		ip := netip.MustParseAddr("192.0.2.11")
		hash := VerifyEgressIndexHashWithSettings(ip, settings)
		hashHex := hex.EncodeToString(hash[:])
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			now := server.NowUtc().UnixMilli()
			server.Raise(r.Eval(ctx, verifyReverseEgressLeaseScript,
				[]string{verifyClientEgressKey(clientId), verifyClientEgressLeaseKey(clientId, hashHex)},
				oldId.String(), now, 0, hashHex).Err())
		})
		FeedVerifyConnectionEgress(ctx, clientId, nextId, ip, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Eval(ctx, verifyForwardEgressLeaseScript,
				[]string{verifyEgressKeyFromHex(hashHex), verifyForwardEgressLeaseKey(hashHex)},
				clientId.String(), oldId.String(), server.NowUtc().UnixMilli(), 0, verifyEgressAmbiguous).Err())
		})
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		ClearVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		ClearVerifyConnectionEgress(ctx, clientId, nextId, ip, settings)
	})
}

// Advancing the operation clock expires abandoned owners without sleeps. A
// stale expiry snapshot also cannot delete an entry renewed by another owner.
func TestVerifyConnectionEgressExpiryAndRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("expiry-connection-egress-test")
		clientId, oldId, nextId := server.NewId(), server.NewId(), server.NewId()
		ip := netip.MustParseAddr("192.0.2.12")
		hash := VerifyEgressIndexHashWithSettings(ip, settings)
		hashHex := hex.EncodeToString(hash[:])
		testVerifySetProvideModes(ctx, clientId)
		now := server.NowUtc()
		server.Redis(ctx, func(r server.RedisClient) {
			updateVerifyEgressLease(ctx, r, clientId, hashHex, oldId.String(), now, now.Add(time.Minute))
			staleExpiry, err := r.HGet(ctx, verifyClientEgressKey(clientId), hashHex).Result()
			server.Raise(err)
			updateVerifyEgressLease(ctx, r, clientId, hashHex, nextId.String(), now.Add(2*time.Minute), now.Add(10*time.Minute))
			pruneVerifyExpiredEgressHashes(ctx, r, clientId, map[string]string{hashHex: staleExpiry})
			owners, err := r.HGetAll(ctx, verifyClientEgressLeaseKey(clientId, hashHex)).Result()
			server.Raise(err)
			if len(owners) != 1 || owners[nextId.String()] == "" {
				t.Fatalf("expired transport survived or successor vanished: %v", owners)
			}
			for _, key := range []string{verifyClientEgressKey(clientId), verifyClientEgressLeaseKey(clientId, hashHex), verifyEgressKeyFromHex(hashHex), verifyForwardEgressLeaseKey(hashHex)} {
				ttl, err := r.PTTL(ctx, key).Result()
				server.Raise(err)
				if ttl <= 0 || ttl > 10*time.Minute {
					t.Fatalf("lease key %s has unbounded or expired TTL %s", key, ttl)
				}
			}
		})
		updateVerifyEligibleMembership(ctx, clientId)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		ClearVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		ClearVerifyConnectionEgress(ctx, clientId, nextId, ip, settings)
		if ResolveVerifyEgress(ctx, ip, settings) != nil {
			t.Fatal("expired original transport resurrected after successor cleanup")
		}
	})
}

// Allocation removal shares an address with live transports; client reaping
// removes all owners while keeping another client's reassigned forward entry.
func TestVerifyConnectionEgressProxyAndReapOwnership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("proxy-connection-egress-test")
		clientId, connectionId, survivorId := server.NewId(), server.NewId(), server.NewId()
		ip := netip.MustParseAddr("192.0.2.13")
		hash := VerifyEgressIndexHashWithSettings(ip, settings)
		hashHex := hex.EncodeToString(hash[:])
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyEgress(ctx, clientId, ip, settings)
		FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
		clearVerifyProxyEgressForClient(ctx, clientId)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		FeedVerifyEgress(ctx, clientId, ip, settings)
		ClearVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, verifyEgressKeyFromHex(hashHex), survivorId.String(), time.Minute).Err())
			server.Raise(r.HSet(ctx, verifyForwardEgressLeaseKey(hashHex), survivorId.String()+"/proxy", server.NowUtc().Add(time.Minute).UnixMilli()).Err())
		})
		removeReapedClientRedisState(ctx, []server.Id{clientId})
		server.Redis(ctx, func(r server.RedisClient) {
			owner, err := r.Get(ctx, verifyEgressKeyFromHex(hashHex)).Result()
			server.Raise(err)
			if owner != survivorId.String() {
				t.Fatal("reaping erased reassigned forward owner")
			}
			leases, err := r.HGetAll(ctx, verifyForwardEgressLeaseKey(hashHex)).Result()
			server.Raise(err)
			if len(leases) != 1 || leases[survivorId.String()+"/proxy"] == "" {
				t.Fatalf("reaping retained dead owners or erased survivor: %v", leases)
			}
			count, err := r.Exists(ctx, verifyClientEgressKey(clientId), verifyClientEgressLeaseKey(clientId, hashHex)).Result()
			server.Raise(err)
			if count != 0 {
				t.Fatal("reaping retained reverse ownership")
			}
		})
	})
}

// A different address still fails the single-address gate; different clients
// sharing one address remain ambiguous even after one claimant disconnects.
func TestVerifyConnectionEgressKeepsBijectionStrict(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("strict-connection-egress-test")
		clientId, otherId := server.NewId(), server.NewId()
		firstId, secondId, thirdId := server.NewId(), server.NewId(), server.NewId()
		ip, otherIp := netip.MustParseAddr("192.0.2.14"), netip.MustParseAddr("192.0.2.15")
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyConnectionEgress(ctx, clientId, firstId, ip, settings)
		FeedVerifyConnectionEgress(ctx, clientId, secondId, otherIp, settings)
		if ResolveVerifyEgress(ctx, ip, settings) != nil || testVerifyEligible(ctx, clientId) {
			t.Fatal("multiple live addresses passed the bijection")
		}
		ClearVerifyConnectionEgress(ctx, clientId, secondId, otherIp, settings)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		FeedVerifyConnectionEgress(ctx, otherId, thirdId, ip, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			hash := VerifyEgressIndexHashWithSettings(ip, settings)
			server.Raise(r.Del(ctx, verifyEgressKey(hash)).Err())
		})
		FeedVerifyConnectionEgress(ctx, clientId, firstId, ip, settings)
		if ResolveVerifyEgress(ctx, ip, settings) != nil {
			t.Fatal("expired ambiguity marker selected one of two live owners")
		}
		ClearVerifyConnectionEgress(ctx, otherId, thirdId, ip, settings)
		FeedVerifyConnectionEgress(ctx, clientId, firstId, ip, settings)
		if ResolveVerifyEgress(ctx, ip, settings) != nil {
			t.Fatal("conflicting client cleanup erased the ambiguity marker")
		}
	})
}
