// Exercises transport lease overlap, slot-local interleavings, and expiration
// against the fixture's isolated Redis database; no production state is used.
package model

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

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
		field, parsed := verifyClientEgressField(ip, settings)
		hashHex := parsed.address
		testVerifySetProvideModes(ctx, clientId)
		FeedVerifyConnectionEgress(ctx, clientId, oldId, ip, settings)
		server.Redis(ctx, func(r server.RedisClient) {
			now := server.NowUtc().UnixMilli()
			server.Raise(r.Eval(ctx, verifyReverseEgressLeaseScript,
				[]string{verifyClientEgressKey(clientId), verifyClientEgressLeaseKey(verifyClientEgressKey(clientId), hashHex)},
				oldId.String(), now, 0, field, "").Err())
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
		field, parsed := verifyClientEgressField(ip, settings)
		hashHex := parsed.address
		reverseKey := verifyClientEgressKey(clientId)
		testVerifySetProvideModes(ctx, clientId)
		now := server.NowUtc()
		server.Redis(ctx, func(r server.RedisClient) {
			updateVerifyEgressLease(ctx, r, reverseKey, clientId, field, verifyClientEgressValueSuffix(ip, settings), oldId.String(), now, now.Add(time.Minute))
			staleExpiry, err := r.HGet(ctx, reverseKey, field).Result()
			server.Raise(err)
			updateVerifyEgressLease(ctx, r, reverseKey, clientId, field, verifyClientEgressValueSuffix(ip, settings), nextId.String(), now.Add(2*time.Minute), now.Add(10*time.Minute))
			pruneVerifyExpiredEgressHashes(ctx, r, clientId, map[string]string{field: staleExpiry})
			owners, err := r.HGetAll(ctx, verifyClientEgressLeaseKey(reverseKey, hashHex)).Result()
			server.Raise(err)
			if len(owners) != 1 || owners[nextId.String()] == "" {
				t.Fatalf("expired transport survived or successor vanished: %v", owners)
			}
			for _, key := range []string{reverseKey, verifyClientEgressLeaseKey(reverseKey, hashHex), verifyEgressKeyFromHex(hashHex), verifyForwardEgressLeaseKey(hashHex)} {
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
			count, err := r.Exists(ctx, verifyClientEgressKey(clientId), verifyClientEgressLeaseKey(verifyClientEgressKey(clientId), hashHex)).Result()
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

type testVerifyEgressOwner struct {
	clientId     server.Id
	connectionId server.Id
}

// Seeds one address shared by many other clients, as behind one NAT, and
// already marked ambiguous. Every other owner is a distinct client; the odd
// ones were abandoned without a close and have expired, so only a whole-hash
// pass removes them. Returns each seeded owner's recorded deadline and the
// live owners.
func testVerifySeedMarkedEgress(
	ctx context.Context,
	settings *VerifySettings,
	ip netip.Addr,
	ownerCount int,
) (ownerDeadlines map[string]string, liveOwners []testVerifyEgressOwner) {
	hash := VerifyEgressIndexHashWithSettings(ip, settings)
	hashHex := hex.EncodeToString(hash[:])
	now := server.NowUtc()
	ownerDeadlines = map[string]string{}
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i := 0; i < ownerCount; i++ {
				owner := testVerifyEgressOwner{clientId: server.NewId(), connectionId: server.NewId()}
				deadline := now.Add(settings.EgressTtl)
				if i%2 == 1 {
					deadline = now.Add(-time.Minute)
				} else {
					liveOwners = append(liveOwners, owner)
				}
				field := owner.clientId.String() + "/" + owner.connectionId.String()
				ownerDeadlines[field] = strconv.FormatInt(deadline.UnixMilli(), 10)
				pipe.HSet(ctx, verifyForwardEgressLeaseKey(hashHex), field, ownerDeadlines[field])
			}
			pipe.PExpire(ctx, verifyForwardEgressLeaseKey(hashHex), settings.EgressTtl)
			pipe.Set(ctx, verifyEgressKeyFromHex(hashHex), verifyEgressAmbiguous, settings.EgressTtl)
			return nil
		})
		server.Raise(err)
	})
	return
}

// Reads the forward owners of one address and its forward entry (a client, the
// ambiguity marker, or empty) with that entry's TTL.
func testVerifyForwardEgress(ctx context.Context, ip netip.Addr, settings *VerifySettings) (leases map[string]string, forward string, forwardTtl time.Duration) {
	hash := VerifyEgressIndexHashWithSettings(ip, settings)
	hashHex := hex.EncodeToString(hash[:])
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		leases, err = r.HGetAll(ctx, verifyForwardEgressLeaseKey(hashHex)).Result()
		server.Raise(err)
		forward, err = r.Get(ctx, verifyEgressKeyFromHex(hashHex)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		forwardTtl, err = r.PTTL(ctx, verifyEgressKeyFromHex(hashHex)).Result()
		server.Raise(err)
	})
	return
}

// Counts the seeded owners whose recorded deadline is unchanged.
func testVerifyKeptOwnerCount(leases map[string]string, ownerDeadlines map[string]string) int {
	kept := 0
	for owner, deadline := range ownerDeadlines {
		if leases[owner] == deadline {
			kept += 1
		}
	}
	return kept
}

// Every client behind a shared address updates its forward lease on connect,
// on each refresh and on close. Once the address is marked ambiguous an update
// must touch only its own owner. Rescanning every owner per update makes the
// address's work quadratic in its owners, all on the address's one cluster
// slot: one shared address with thousands of churning clients saturated a
// Redis node. The expired seeded owners make such a rescan observable, since
// the whole-hash pass deletes them.
func TestVerifyEgressMarkedAddressUpdateTouchesOnlyItsOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("marked-address-egress-test")
		ip := netip.MustParseAddr("198.51.100.20")
		ownerDeadlines, _ := testVerifySeedMarkedEgress(ctx, settings, ip, 256)
		_, _, seededMarkerTtl := testVerifyForwardEgress(ctx, ip, settings)

		clientId, connectionId := server.NewId(), server.NewId()
		owner := clientId.String() + "/" + connectionId.String()
		testVerifySetProvideModes(ctx, clientId)
		steps := []struct {
			name   string
			update func()
			live   bool
		}{
			{name: "connect", update: func() { FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings) }, live: true},
			{name: "refresh", update: func() { FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings) }, live: true},
			{name: "close", update: func() { ClearVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings) }, live: false},
		}
		for _, step := range steps {
			step.update()
			leases, marker, markerTtl := testVerifyForwardEgress(ctx, ip, settings)
			if kept := testVerifyKeptOwnerCount(leases, ownerDeadlines); kept != len(ownerDeadlines) {
				t.Fatalf("%s on a marked address rewrote other owners: kept %d of %d", step.name, kept, len(ownerDeadlines))
			}
			if _, recorded := leases[owner]; recorded != step.live {
				t.Fatalf("%s on a marked address left its own owner recorded=%t", step.name, recorded)
			}
			if marker != verifyEgressAmbiguous || markerTtl <= 0 || seededMarkerTtl < markerTtl {
				t.Fatalf("%s on a marked address changed the marker: %q ttl=%s", step.name, marker, markerTtl)
			}
			if ResolveVerifyEgress(ctx, ip, settings) != nil || testVerifyEligible(ctx, clientId) {
				t.Fatalf("%s on a marked address attributed it to one client", step.name)
			}
		}
	})
}

// Once the marker lapses nobody is attributed, so no close can change the
// forward entry. Closes then, as when a shared address's clients disconnect
// together, must each withdraw only their own owner instead of rescanning
// every remaining one.
func TestVerifyEgressUnattributedCloseTouchesOnlyItsOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("unattributed-close-egress-test")
		ip := netip.MustParseAddr("198.51.100.22")
		hash := VerifyEgressIndexHashWithSettings(ip, settings)
		ownerDeadlines, liveOwners := testVerifySeedMarkedEgress(ctx, settings, ip, 256)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, verifyEgressKey(hash)).Err())
		})

		for _, closing := range liveOwners[:2] {
			ClearVerifyConnectionEgress(ctx, closing.clientId, closing.connectionId, ip, settings)
			delete(ownerDeadlines, closing.clientId.String()+"/"+closing.connectionId.String())
			leases, forward, _ := testVerifyForwardEgress(ctx, ip, settings)
			if kept := testVerifyKeptOwnerCount(leases, ownerDeadlines); kept != len(ownerDeadlines) || len(leases) != len(ownerDeadlines) {
				t.Fatalf("close on an unattributed address rewrote other owners: kept %d of %d, %d recorded", kept, len(ownerDeadlines), len(leases))
			}
			if forward != "" {
				t.Fatalf("close on an unattributed address set the forward entry to %q", forward)
			}
		}
		if ResolveVerifyEgress(ctx, ip, settings) != nil {
			t.Fatal("unattributed address resolved to one client")
		}
	})
}

// Deferring cleanup while marked must not strand the address. The marker's
// lapse is forced by deleting it. The first feed afterward prunes the expired
// owners and re-marks the address while other clients stay live; after they
// have all closed and the marker lapses again, the remaining client is
// attributed.
func TestVerifyEgressLapsedMarkerPrunesAndHeals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("lapsed-marker-egress-test")
		ip := netip.MustParseAddr("198.51.100.21")
		hash := VerifyEgressIndexHashWithSettings(ip, settings)
		lapseMarker := func() {
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Del(ctx, verifyEgressKey(hash)).Err())
			})
		}
		ownerDeadlines, liveOwners := testVerifySeedMarkedEgress(ctx, settings, ip, 64)
		clientId, connectionId := server.NewId(), server.NewId()
		testVerifySetProvideModes(ctx, clientId)

		lapseMarker()
		FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
		leases, marker, markerTtl := testVerifyForwardEgress(ctx, ip, settings)
		if kept := testVerifyKeptOwnerCount(leases, ownerDeadlines); kept != len(liveOwners) || len(leases) != len(liveOwners)+1 {
			t.Fatalf("first feed after the lapse did not prune exactly the expired owners: kept %d of %d live, %d recorded", kept, len(liveOwners), len(leases))
		}
		if marker != verifyEgressAmbiguous || markerTtl <= 0 || settings.EgressTtl < markerTtl {
			t.Fatalf("first feed after the lapse did not re-mark a shared address: %q ttl=%s", marker, markerTtl)
		}
		if ResolveVerifyEgress(ctx, ip, settings) != nil {
			t.Fatal("re-marked address attributed to one client")
		}

		for _, liveOwner := range liveOwners {
			ClearVerifyConnectionEgress(ctx, liveOwner.clientId, liveOwner.connectionId, ip, settings)
		}
		leases, marker, _ = testVerifyForwardEgress(ctx, ip, settings)
		if len(leases) != 1 || marker != verifyEgressAmbiguous {
			t.Fatalf("closes on a marked address: %d owners recorded, marker %q", len(leases), marker)
		}
		if ResolveVerifyEgress(ctx, ip, settings) != nil {
			t.Fatal("closes shortened the ambiguity window")
		}

		lapseMarker()
		FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
		requireVerifyEgressLeaseOwner(t, ctx, ip, clientId, settings)
		ClearVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
		if ResolveVerifyEgress(ctx, ip, settings) != nil || testVerifyEligible(ctx, clientId) {
			t.Fatal("last close retained attribution or eligibility")
		}
	})
}
