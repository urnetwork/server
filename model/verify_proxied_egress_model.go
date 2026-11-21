package model

// A provider whose platform transport declares that it leaves through a proxy
// (connect's HeaderEgressProxy, which connect sets on every transport it dials
// through a ConnectSettings.ProxySettings proxy, as an sn miner proxy slot
// does) connects from the proxy's address, not from its host's. The verify
// index still attributes it at that address, and the validator still measures
// it. But a proxy is not a host the provider runs, so the provider counts in
// none of the unit-based shares (sn WHITEPAPER.md §8.4 and §8.6.2,
// CAPTURETHEFLAG.md §4.2):
//
//   - its trail hops record the zero egress-IP-hash, which shows no head unit:
//     every validator path skips a zero hash (PinVerifyHopEgressIpHash);
//   - the SN Top board and capture the flag pin it no prefix
//     (getVerifyHeadPrefixPins), and capture the flag gives it no unit
//     (GetCtfProviderEgress);
//   - the extender censuses judge its records inactive
//     (DecideExtenderEligibility).
//
// The mark belongs to the client, not to an address: a proxy slot is one
// client, and every transport of it leaves through its proxy. It outlives the
// client's last declaring transport by a settlement epoch and the pin grace,
// so the epoch's censuses and every scoring window the client took part in
// read it after the client disconnects. A transport that declares nothing
// changes nothing.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// How long a mark outlives the transport that last wrote it: a mainnet
// settlement epoch (50400 blocks of 12 seconds) and the pin grace.
const VerifyProxiedEgressLifetime = time.Duration(verifyHeadHashEpochBlocks)*verifyHeadHashBlockTime + verifyHeadHashGrace

func verifyProxiedEgressKey(clientId server.Id) string {
	return fmt.Sprintf("{vproxy_%s}", clientId)
}

// MarkVerifyProxiedEgress records that a transport of the client declared it
// leaves through a proxy.
func MarkVerifyProxiedEgress(ctx context.Context, clientId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, verifyProxiedEgressKey(clientId), "1", VerifyProxiedEgressLifetime).Err())
	})
}

// FeedVerifyProxiedConnectionEgress is FeedVerifyConnectionEgress for a
// transport that declared a proxy egress. The mark is written first, so the
// index never attributes a declared transport whose mark is missing.
func FeedVerifyProxiedConnectionEgress(ctx context.Context, clientId, connectionId server.Id, ip netip.Addr, settings *VerifySettings) {
	MarkVerifyProxiedEgress(ctx, clientId)
	FeedVerifyConnectionEgress(ctx, clientId, connectionId, ip, settings)
}

// Whether the client is marked.
func verifyProxiedEgressMarked(ctx context.Context, r server.RedisClient, clientId server.Id) bool {
	count, err := r.Exists(ctx, verifyProxiedEgressKey(clientId)).Result()
	server.Raise(err)
	return 0 < count
}

// The marked clients of one chunk, in one pipeline.
func readVerifyProxiedEgress(ctx context.Context, r server.RedisClient, clientIds []server.Id) map[server.Id]bool {
	marked := map[server.Id]bool{}
	if len(clientIds) == 0 {
		return marked
	}
	cmds := make([]*redis.IntCmd, len(clientIds))
	_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, clientId := range clientIds {
			cmds[i] = pipe.Exists(ctx, verifyProxiedEgressKey(clientId))
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		server.Raise(err)
	}
	for i, clientId := range clientIds {
		count, cmdErr := cmds[i].Result()
		server.Raise(cmdErr)
		if 0 < count {
			marked[clientId] = true
		}
	}
	return marked
}

// GetVerifyProxiedEgress returns the marked clients among clientIds, read 512
// at a time.
func GetVerifyProxiedEgress(ctx context.Context, clientIds []server.Id) map[server.Id]bool {
	marked := map[server.Id]bool{}
	if len(clientIds) == 0 {
		return marked
	}
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(clientIds); start += 512 {
			for clientId := range readVerifyProxiedEgress(ctx, r, clientIds[start:min(start+512, len(clientIds))]) {
				marked[clientId] = true
			}
		}
	})
	return marked
}
