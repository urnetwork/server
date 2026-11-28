// A proxied or bridged address counts the same as an owned one (owner
// decision 2026-11-21, which reverted the proxied-egress mark of 2026-11-19
// and the exit-differs mark of 2026-11-20). The reverted code wrote
// {vproxy_<client>} for a provider whose transport declared a proxy egress or
// whose probed exit differed from its networks, for a settlement epoch and the
// pin grace, and every unit-based share read it. Marks written before the
// release outlive it. So these tests write the mark exactly as the reverted
// code did and check that nothing reads it:
//   - each proxy slot's hop records its proxy's prefix and pins it, so six
//     proxies of one host add six units of head breadth;
//   - the SN Top board and capture the flag pin a marked provider its prefix
//     and give it its unit, and the /sn/head fallback scores it by its hash.
//
// Each test fails on the code before the revert, which gave a marked provider
// the zero hash, no pin, no unit and no fallback hash. Fixtures use RFC 5737
// documentation addresses in the fixture's isolated Redis database.
package model

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urfoundation/sn/v2026/protocol/headscore"

	"github.com/urnetwork/server/v2026"
)

// The score epoch of the fixtures, and its window.
const testVerifyLeftoverMarkEpoch = 9002

// Writes the mark as the reverted MarkVerifyProxiedEgress wrote it: "1" under
// {vproxy_<client>}, for a settlement epoch and the pin grace.
func testWriteLeftoverProxiedEgressMark(ctx context.Context, clientId server.Id) {
	lifetime := time.Duration(verifyHeadHashEpochBlocks)*verifyHeadHashBlockTime + verifyHeadHashGrace
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, fmt.Sprintf("{vproxy_%s}", clientId), "1", lifetime).Err())
	})
}

// A host's direct slot and six proxy slots, each proxy in its own /29 and
// each slot marked, and another fleet's direct host. Each slot's hop records
// its proxy's prefix and pins it, so the fleet shows seven units.
func TestVerifyLeftoverProxiedEgressMarksAddHeadBreadth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("leftover-proxied-egress-head-test")
		window := CtfVerifyHeadWindow(testVerifyLeftoverMarkEpoch)

		host, other := server.NewId(), server.NewId()
		hostIp, otherIp := netip.MustParseAddr("192.0.2.17"), netip.MustParseAddr("203.0.113.9")
		slots := make([]server.Id, 6)
		proxyIps := make([]netip.Addr, 6)
		for i := range slots {
			slots[i] = server.NewId()
			proxyIps[i] = netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", 8*(i+1)))
		}
		for _, clientId := range append([]server.Id{host, other}, slots...) {
			testVerifySetProvideModes(ctx, clientId)
		}
		FeedVerifyConnectionEgress(ctx, host, server.NewId(), hostIp, settings)
		FeedVerifyConnectionEgress(ctx, other, server.NewId(), otherIp, settings)
		for i, clientId := range slots {
			testWriteLeftoverProxiedEgressMark(ctx, clientId)
			FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), proxyIps[i], settings)
			requireVerifyEgressLeaseOwner(t, ctx, proxyIps[i], clientId, settings)
		}

		// each provider's confirmed hop in the window, collected per fleet as
		// the validator collects claims: a zero hash is no claim
		census := map[string]map[[32]byte]bool{"fleet": {}, "other": {}}
		claim := func(fleet string, clientId server.Id, ip netip.Addr) {
			t.Helper()
			recorded := PinVerifyHopEgressIpHash(ctx, clientId, ip, window, false, settings)
			if want := VerifyEgressIpHashWithSettings(ip, settings); recorded != want {
				t.Fatalf("the hop of %s recorded %x, want its /29 %x", ip, recorded, want)
			}
			census[fleet][recorded] = true
		}
		claim("fleet", host, hostIp)
		claim("other", other, otherIp)
		for i, clientId := range slots {
			claim("fleet", clientId, proxyIps[i])
		}
		raw := headscore.RawScores(census)
		if raw["fleet"].RatString() != "7" || raw["other"].RatString() != "1" {
			t.Fatalf("raw scores: the fleet with six proxy slots %s, the other fleet %s; want 7 and 1", raw["fleet"].RatString(), raw["other"].RatString())
		}
		// each marked slot's hop set its pin
		server.Redis(ctx, func(r server.RedisClient) {
			for i, clientId := range slots {
				count, err := r.Exists(ctx, verifyHeadHashKey(clientId, window)).Result()
				server.Raise(err)
				if count != 1 {
					t.Fatalf("marked proxy slot %d has no pin", i)
				}
			}
		})
	})
}

// The board, capture the flag and the /sn/head fallback give a marked
// provider its unit: one marked before any hop, and one an earlier hop pinned
// before its mark.
func TestVerifyLeftoverProxiedEgressMarksKeepBoardCtfAndFallbackUnits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := testVerifyFamilySettings("leftover-proxied-egress-board-test")
		window := CtfVerifyHeadWindow(testVerifyLeftoverMarkEpoch)

		host, slot, late := server.NewId(), server.NewId(), server.NewId()
		hostIp, slotIp, lateIp := netip.MustParseAddr("192.0.2.33"), netip.MustParseAddr("198.51.100.57"), netip.MustParseAddr("198.51.100.73")
		all := []server.Id{host, slot, late}
		for _, clientId := range all {
			testVerifySetProvideModes(ctx, clientId)
		}
		FeedVerifyConnectionEgress(ctx, host, server.NewId(), hostIp, settings)
		testWriteLeftoverProxiedEgressMark(ctx, slot)
		FeedVerifyConnectionEgress(ctx, slot, server.NewId(), slotIp, settings)
		// the late slot was pinned at its prefix before it was marked
		FeedVerifyConnectionEgress(ctx, late, server.NewId(), lateIp, settings)
		lateHash := VerifyEgressIpHashWithSettings(lateIp, settings)
		if recorded := PinVerifyHopEgressIpHash(ctx, late, lateIp, window, false, settings); recorded != lateHash {
			t.Fatalf("the late slot's first hop recorded %x", recorded)
		}
		testWriteLeftoverProxiedEgressMark(ctx, late)
		if recorded := PinVerifyHopEgressIpHash(ctx, late, lateIp, window, false, settings); recorded != lateHash {
			t.Fatalf("the late slot's hop after its mark recorded %x, want its pin %x", recorded, lateHash)
		}

		want := map[server.Id][32]byte{
			host: VerifyEgressIpHashWithSettings(hostIp, settings),
			slot: VerifyEgressIpHashWithSettings(slotIp, settings),
			late: lateHash,
		}
		pins := getVerifyHeadPrefixPins(ctx, all, settings, window, true)
		if len(pins) != len(want) {
			t.Fatalf("board pins %v, want one for each provider", pins)
		}
		for clientId, hash := range want {
			if pins[clientId] != hash {
				t.Fatalf("the board pinned %s at %x, want %x", clientId, pins[clientId], hash)
			}
		}

		for _, pin := range []bool{true, false} {
			egresses := GetCtfProviderEgress(ctx, all, settings, testVerifyLeftoverMarkEpoch, pin)
			for clientId, hash := range want {
				if egress := egresses[clientId]; egress == nil || egress.Unit == nil || *egress.Unit != hash {
					t.Fatalf("capture the flag (pin %v) gave %s %+v, want the unit %x", pin, clientId, egress, hash)
				}
			}
		}

		live := GetVerifyLiveEgressHashes(ctx, all)
		for _, clientId := range all {
			if len(live[clientId]) != 1 {
				t.Fatalf("the /sn/head fallback's hashes %v, want one for each provider", live)
			}
		}
	})
}
