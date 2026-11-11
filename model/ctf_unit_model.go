package model

// Capture the flag's units and distinct addresses (sn CAPTURETHEFLAG.md §4.2,
// WHITEPAPER.md §8.4, §8.6.2), from the verify index (verify_model.go,
// verify_head_model.go).
//
// A provider's unit is the egress prefix the verify head model pins for it in
// the epoch: the keyed /29 of its IPv4 address first, else the /48 of its IPv6
// address, under the egress hash key, exactly the hash a confirmed hop of that
// window records. An extender's unit is the same hash of its active IPv4
// address, else its active IPv6 address. Two candidates of a role are
// distinct when they share no keyed IPv4 address and no keyed IPv6 /64: the
// exact-address identities of the verify index, never a raw address.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The verify head model's window of a settlement epoch, which pins hold a
// provider's prefix for (verifyHeadHashWindow).
func CtfVerifyHeadWindow(epoch uint64) string {
	return fmt.Sprintf("e%d", epoch)
}

// A miner's unit, its family and its keyed addresses.
type CtfEgress struct {
	// nil when the miner has no unit
	Unit *[32]byte
	// 4 or 6, 0 when unknown
	UnitFamily int
	// the keyed IPv4 addresses and IPv6 /64s it holds, ascending
	Addresses [][32]byte
}

// GetCtfProviderEgress reads each provider's unit for an epoch and the keyed
// addresses it holds now. The unit is the prefix pinned in the epoch's window
// (a trail's pin, else the SN Top board's); with pin, a provider that has
// neither gets the board's pin of its live candidate, as the board pins it.
// The family is that of the live address whose prefix the pin is, else unknown.
// The reads are pipelined 512 providers at a time.
func GetCtfProviderEgress(ctx context.Context, clientIds []server.Id, settings *VerifySettings, epoch uint64, pin bool) map[server.Id]*CtfEgress {
	egresses := map[server.Id]*CtfEgress{}
	if len(clientIds) == 0 {
		return egresses
	}
	pins := getVerifyHeadPrefixPins(ctx, clientIds, settings, CtfVerifyHeadWindow(epoch), pin)
	nowMs := uint64(server.NowUtc().UnixMilli())
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(clientIds); start += 512 {
			chunk := clientIds[start:min(start+512, len(clientIds))]
			cmds := make([][]*redis.MapStringStringCmd, len(chunk))
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, clientId := range chunk {
					for _, reverseKey := range verifyClientEgressKeys(clientId) {
						cmds[i] = append(cmds[i], pipe.HGetAll(ctx, reverseKey))
					}
				}
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			for i, clientId := range chunk {
				versions := []map[string]string{}
				for _, cmd := range cmds[i] {
					fields, _ := cmd.Result()
					live, _ := splitVerifyEgressFields(fields, nowMs)
					versions = append(versions, live)
				}
				egress := ctfEgressOfLive(newVerifyLiveEgress(versions[0], versions[1]))
				if pinned, ok := pins[clientId]; ok {
					unit := pinned
					egress.Unit = &unit
					egress.UnitFamily = ctfPinFamily(newVerifyLiveEgress(versions[0], versions[1]), pinned)
				}
				egresses[clientId] = egress
			}
		}
	})
	return egresses
}

// The keyed addresses of a provider's live egress: each IPv4 address and each
// IPv6 /64, in either family, ascending.
func ctfEgressOfLive(live verifyLiveEgress) *CtfEgress {
	egress := &CtfEgress{}
	seen := map[string]bool{}
	for _, family := range []int{4, 6} {
		for _, network := range live.networks(family) {
			if seen[network] {
				continue
			}
			seen[network] = true
			if decoded, err := hex.DecodeString(network); err == nil && len(decoded) == 32 {
				egress.Addresses = append(egress.Addresses, [32]byte(decoded))
			}
		}
	}
	slices.SortFunc(egress.Addresses, func(a, b [32]byte) int { return slices.Compare(a[:], b[:]) })
	return egress
}

// The family of the live address whose stored prefix hash is the pin, IPv4
// first, or 0 when no live address has it.
func ctfPinFamily(live verifyLiveEgress, pinned [32]byte) int {
	pinnedHex := hex.EncodeToString(pinned[:])
	for _, family := range []int{4, 6} {
		for _, address := range live.byFamily[family] {
			if address.score == pinnedHex {
				return family
			}
		}
	}
	return 0
}

// CtfExtenderEgress is an extender's unit and keyed addresses from its active
// addresses (§8.6.2): the prefix of its IPv4 address, else of its IPv6
// address, and the keyed IPv4 address and IPv6 /64 it holds.
func CtfExtenderEgress(ipv4 *netip.Addr, ipv6 *netip.Addr, settings *VerifySettings) *CtfEgress {
	egress := &CtfEgress{}
	for _, ip := range []*netip.Addr{ipv4, ipv6} {
		if ip == nil || !ip.IsValid() {
			continue
		}
		address := ip.Unmap()
		if egress.Unit == nil {
			unit := VerifyEgressIpHashWithSettings(address, settings)
			egress.Unit = &unit
			egress.UnitFamily = 4
			if address.Is6() {
				egress.UnitFamily = 6
			}
		}
		egress.Addresses = append(egress.Addresses, verifyEgressNetworkHash(address, settings))
	}
	slices.SortFunc(egress.Addresses, func(a, b [32]byte) int { return slices.Compare(a[:], b[:]) })
	return egress
}
