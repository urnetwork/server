// The root cause (sn WHITEPAPER.md §8.4): a slot whose transports leave
// through a proxy connects from the proxy's address, so a fleet that bought a
// proxy per slot would show each proxy's /29 as its own head unit. A slot
// whose transports declared a proxy egress is marked, and every unit-based
// share reads the mark:
//   - its hops record the zero hash, which the validator skips;
//   - the SN Top board and capture the flag pin it nothing and give it no unit;
//   - the /sn/head fallback scores it by no hash.
//
// The verify index still attributes it, and a direct host and a client that
// declared nothing are unchanged. Fixtures use RFC 5737 documentation
// addresses in the fixture's isolated Redis database.
package model

import (
	"context"
	"fmt"
	"net/netip"
	"testing"

	"github.com/urfoundation/sn/protocol/headscore"

	"github.com/urnetwork/server"
)

// The score epoch of the fixtures, and its window.
const testVerifyProxiedEpoch = 9001

// A host's direct slot and six proxy slots, each proxy in its own /29, and
// another fleet's direct host. The fleet shows its host's /29 alone: the six
// proxies add no breadth.
func TestVerifyProxiedEgressFleetBuysNoHeadBreadth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("proxied-egress-head-test")
		window := CtfVerifyHeadWindow(testVerifyProxiedEpoch)

		host, other := server.NewId(), server.NewId()
		hostIp, otherIp := netip.MustParseAddr("192.0.2.17"), netip.MustParseAddr("203.0.113.9")
		proxied := make([]server.Id, 6)
		proxyIps := make([]netip.Addr, 6)
		for i := range proxied {
			proxied[i] = server.NewId()
			proxyIps[i] = netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", 8*(i+1)))
		}
		for _, clientId := range append([]server.Id{host, other}, proxied...) {
			testVerifySetProvideModes(ctx, clientId)
		}
		FeedVerifyConnectionEgress(ctx, host, server.NewId(), hostIp, settings)
		FeedVerifyConnectionEgress(ctx, other, server.NewId(), otherIp, settings)
		for i, clientId := range proxied {
			FeedVerifyProxiedConnectionEgress(ctx, clientId, server.NewId(), proxyIps[i], settings)
		}

		// attribution is unchanged: each slot's proxy address resolves to it
		requireVerifyEgressLeaseOwner(t, ctx, hostIp, host, settings)
		requireVerifyEgressLeaseOwner(t, ctx, otherIp, other, settings)
		for i, clientId := range proxied {
			requireVerifyEgressLeaseOwner(t, ctx, proxyIps[i], clientId, settings)
		}

		// each provider's confirmed hop in the window, collected per fleet as
		// the validator collects claims: a zero hash is no claim
		hostHash, otherHash := VerifyEgressIpHashWithSettings(hostIp, settings), VerifyEgressIpHashWithSettings(otherIp, settings)
		census := map[string]map[[32]byte]bool{"fleet": {}, "other": {}}
		claim := func(fleet string, clientId server.Id, ip netip.Addr) [32]byte {
			recorded := PinVerifyHopEgressIpHash(ctx, clientId, ip, window, false, settings)
			if recorded != ([32]byte{}) {
				census[fleet][recorded] = true
			}
			return recorded
		}
		if recorded := claim("fleet", host, hostIp); recorded != hostHash {
			t.Fatalf("the direct slot recorded %x, want its /29 %x", recorded, hostHash)
		}
		if recorded := claim("other", other, otherIp); recorded != otherHash {
			t.Fatalf("a client that declared nothing recorded %x, want its /29 %x", recorded, otherHash)
		}
		for i, clientId := range proxied {
			if recorded := claim("fleet", clientId, proxyIps[i]); recorded != ([32]byte{}) {
				t.Fatalf("proxy slot %d recorded its proxy's prefix %x, want the zero hash", i, recorded)
			}
		}
		raw := headscore.RawScores(census)
		if raw["fleet"].Cmp(raw["other"]) != 0 || raw["fleet"].RatString() != "1" {
			t.Fatalf("raw scores: the fleet with six proxies %s, the other fleet %s; want 1 each", raw["fleet"].RatString(), raw["other"].RatString())
		}
		// a proxied slot's hop sets no pin
		server.Redis(ctx, func(r server.RedisClient) {
			for i, clientId := range proxied {
				count, err := r.Exists(ctx, verifyHeadHashKey(clientId, window)).Result()
				server.Raise(err)
				if count != 0 {
					t.Fatalf("proxy slot %d has a pin", i)
				}
			}
		})
	})
}

// The board, capture the flag and the /sn/head fallback give a marked
// provider no unit, even one an earlier hop pinned before the mark; a direct
// host and a client that declared nothing keep theirs.
func TestVerifyProxiedEgressHasNoBoardCtfOrFallbackUnit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("proxied-egress-board-test")
		window := CtfVerifyHeadWindow(testVerifyProxiedEpoch)

		host, other, proxied, late := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		hostIp, otherIp := netip.MustParseAddr("192.0.2.33"), netip.MustParseAddr("203.0.113.41")
		proxyIp, lateIp := netip.MustParseAddr("198.51.100.57"), netip.MustParseAddr("198.51.100.73")
		all := []server.Id{host, other, proxied, late}
		for _, clientId := range all {
			testVerifySetProvideModes(ctx, clientId)
		}
		FeedVerifyConnectionEgress(ctx, host, server.NewId(), hostIp, settings)
		FeedVerifyConnectionEgress(ctx, other, server.NewId(), otherIp, settings)
		FeedVerifyProxiedConnectionEgress(ctx, proxied, server.NewId(), proxyIp, settings)
		// the late slot was pinned at its proxy's prefix before it declared
		// the proxy, as under a server that did not read the mark
		lateConnectionId := server.NewId()
		FeedVerifyConnectionEgress(ctx, late, lateConnectionId, lateIp, settings)
		if recorded := PinVerifyHopEgressIpHash(ctx, late, lateIp, window, false, settings); recorded != VerifyEgressIpHashWithSettings(lateIp, settings) {
			t.Fatalf("the late slot's first hop recorded %x", recorded)
		}
		FeedVerifyProxiedConnectionEgress(ctx, late, lateConnectionId, lateIp, settings)
		if recorded := PinVerifyHopEgressIpHash(ctx, late, lateIp, window, false, settings); recorded != ([32]byte{}) {
			t.Fatalf("the late slot's hop after its mark recorded %x, want the zero hash", recorded)
		}

		marked := GetVerifyProxiedEgress(ctx, all)
		if len(marked) != 2 || !marked[proxied] || !marked[late] {
			t.Fatalf("marked %v, want the two proxy slots", marked)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, verifyProxiedEgressKey(proxied)).Result()
			server.Raise(err)
			if ttl <= settings.EgressTtl {
				t.Fatalf("the mark lives %s, not past the lease's %s", ttl, settings.EgressTtl)
			}
		})

		hostHash, otherHash := VerifyEgressIpHashWithSettings(hostIp, settings), VerifyEgressIpHashWithSettings(otherIp, settings)
		pins := getVerifyHeadPrefixPins(ctx, all, settings, window, true)
		if len(pins) != 2 || pins[host] != hostHash || pins[other] != otherHash {
			t.Fatalf("board pins %v, want the direct host's and the other client's alone", pins)
		}

		for _, pin := range []bool{true, false} {
			egresses := GetCtfProviderEgress(ctx, all, settings, testVerifyProxiedEpoch, pin)
			for _, clientId := range []server.Id{proxied, late} {
				if egress := egresses[clientId]; egress == nil || egress.Unit != nil || !egress.Proxied {
					t.Fatalf("capture the flag (pin %v) gave a proxy slot %+v", pin, egress)
				}
			}
			if egress := egresses[host]; egress == nil || egress.Unit == nil || *egress.Unit != hostHash || egress.Proxied {
				t.Fatalf("capture the flag (pin %v) gave the direct host %+v", pin, egress)
			}
			if egress := egresses[other]; egress == nil || egress.Unit == nil || *egress.Unit != otherHash || egress.Proxied {
				t.Fatalf("capture the flag (pin %v) gave the other client %+v", pin, egress)
			}
		}

		live := GetVerifyLiveEgressHashes(ctx, all)
		if len(live) != 2 || len(live[host]) != 1 || len(live[other]) != 1 {
			t.Fatalf("the /sn/head fallback's hashes %v, want the direct host's and the other client's alone", live)
		}
	})
}
