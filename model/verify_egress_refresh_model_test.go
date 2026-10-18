// The refresh feeds a chunk of proxy allocations in pipelined waves. Its
// leases, bijection and eligibility must match feeding each allocation by
// itself, on the fixture's isolated Redis database and PostgreSQL.
package model

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// What a refresh leaves observable for one allocation.
type testVerifyProxyEgressState struct {
	resolved          bool
	eligible          bool
	reverseFieldCount int64
}

func testVerifyProxyEgressStateOf(ctx context.Context, clientId server.Id, ip netip.Addr, settings *VerifySettings) (state testVerifyProxyEgressState) {
	resolved := ResolveVerifyEgress(ctx, ip, settings)
	state.resolved = resolved != nil && *resolved == clientId
	state.eligible = testVerifyEligible(ctx, clientId)
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		state.reverseFieldCount, err = r.HLen(ctx, verifyClientEgressKey(clientId)).Result()
		server.Raise(err)
	})
	return
}

func testVerifyInsertProxyClients(ctx context.Context, clientIds []server.Id, ips []netip.Addr) {
	clientIpv4s := make([]int64, len(ips))
	blocks := make([]string, len(ips))
	for index, ip := range ips {
		clientIpv4s[index] = Ipv4ToInt(ip)
		// a shared address is unique per block
		blocks[index] = fmt.Sprintf("synthetic-%d", index)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO proxy_client
			(proxy_id,client_id,instance_id,proxy_host,block,client_ipv4,proxy_client_json)
			SELECT seeded.client_id,seeded.client_id,seeded.client_id,'synthetic.example',seeded.block,seeded.client_ipv4,'{}'
			FROM unnest($1::uuid[],$2::bigint[],$3::varchar[]) AS seeded(client_id,client_ipv4,block)`,
			clientIds, clientIpv4s, blocks))
	})
}

// The same allocations are fed two ways: one at a time with FeedVerifyEgress,
// and from proxy_client by the refresh. A provider and a client without keys
// with their own addresses, two providers sharing one address, a provider with
// a second live address from a connection, and a provider with an expired
// reverse field must each end resolved, eligible and pruned alike. So must
// the per-family rule (§8.2): a provider with an IPv6 connection beside its
// allocation stays eligible, and so does one sharing its allocation's address
// while its own IPv6 /64 attributes, or while its one v2 entry, which a v2
// writer left, does. Both ways run twice, so the single feeds have seen each
// other's leases as the waves of one chunk do.
func TestRefreshVerifyProxyEgressMatchesSingleFeeds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("refresh-proxy-egress-test")

		type allocation struct {
			provider     bool
			address      int
			connectionIp string
			expiredField bool
			v2Ip         string
		}
		allocations := []allocation{
			{provider: true, address: 10},
			{provider: false, address: 11},
			{provider: true, address: 12},
			{provider: true, address: 12},
			{provider: true, address: 13, connectionIp: "203.0.113.%d"},
			{provider: true, address: 14, expiredField: true},
			{provider: true, address: 15, connectionIp: "2001:db8:%d::1"},
			{provider: true, address: 16, connectionIp: "2001:db8:%d::1"},
			{provider: true, address: 16},
			{provider: true, address: 17, v2Ip: "203.0.113.%d"},
			{provider: true, address: 17},
		}
		expected := []testVerifyProxyEgressState{
			{resolved: true, eligible: true, reverseFieldCount: 1},
			{resolved: true, eligible: false, reverseFieldCount: 1},
			{resolved: false, eligible: false, reverseFieldCount: 1},
			{resolved: false, eligible: false, reverseFieldCount: 1},
			{resolved: false, eligible: false, reverseFieldCount: 2},
			{resolved: true, eligible: true, reverseFieldCount: 1},
			{resolved: true, eligible: true, reverseFieldCount: 2},
			{resolved: false, eligible: true, reverseFieldCount: 2},
			{resolved: false, eligible: false, reverseFieldCount: 1},
			{resolved: false, eligible: true, reverseFieldCount: 1},
			{resolved: false, eligible: false, reverseFieldCount: 1},
		}

		// The first network is fed singly, the second by the refresh.
		networks := []string{"192.0.2.%d", "198.51.100.%d"}
		clientIds := make([][]server.Id, len(networks))
		ips := make([][]netip.Addr, len(networks))
		for networkIndex, network := range networks {
			for index, allocation := range allocations {
				clientId := server.NewId()
				clientIds[networkIndex] = append(clientIds[networkIndex], clientId)
				ips[networkIndex] = append(ips[networkIndex], netip.MustParseAddr(fmt.Sprintf(network, allocation.address)))
				if allocation.provider {
					SetProvide(ctx, clientId, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
				}
				if allocation.connectionIp != "" {
					connectionIp := netip.MustParseAddr(fmt.Sprintf(allocation.connectionIp, 10*networkIndex+index))
					FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), connectionIp, settings)
				}
				if allocation.v2Ip != "" {
					now := server.NowUtc()
					v2Ip := netip.MustParseAddr(fmt.Sprintf(allocation.v2Ip, 200+10*networkIndex+index))
					testVerifyFeedV2Address(ctx, clientId, v2Ip, server.NewId().String(), now, now.Add(settings.EgressTtl), settings)
				}
				if allocation.expiredField {
					staleIp := netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", 100+10*networkIndex+index))
					staleField, _ := verifyClientEgressField(staleIp, settings)
					server.Redis(ctx, func(r server.RedisClient) {
						server.Raise(r.HSet(ctx, verifyClientEgressKey(clientId), staleField,
							fmt.Sprintf("%d%s", server.NowUtc().Add(-time.Minute).UnixMilli(), verifyClientEgressValueSuffix(staleIp, settings))).Err())
					})
				}
			}
		}
		testVerifyInsertProxyClients(ctx, clientIds[1], ips[1])

		for range 2 {
			for index, clientId := range clientIds[0] {
				FeedVerifyEgress(ctx, clientId, ips[0][index], settings)
			}
			RefreshVerifyProxyEgress(ctx, settings)
		}

		for index := range allocations {
			single := testVerifyProxyEgressStateOf(ctx, clientIds[0][index], ips[0][index], settings)
			refreshed := testVerifyProxyEgressStateOf(ctx, clientIds[1][index], ips[1][index], settings)
			if single != expected[index] {
				t.Fatalf("allocation %d fed singly: %+v, want %+v", index, single, expected[index])
			}
			if refreshed != single {
				t.Fatalf("allocation %d refreshed: %+v, fed singly: %+v", index, refreshed, single)
			}
		}
	})
}

// A node that has not cached a lease script refuses its hash. The refresh must
// send the refused calls again with the source, so the leases still land, and
// the next refresh must find the scripts cached. Script variants with a
// comment unique to this run are never cached beforehand.
func TestRefreshVerifyProxyEgressLoadsUncachedLeaseScripts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("refresh-proxy-egress-script-test")

		reverseScript, forwardScript := verifyReverseEgressLeaseRedisScript, verifyForwardEgressLeaseRedisScript
		defer func() {
			verifyReverseEgressLeaseRedisScript, verifyForwardEgressLeaseRedisScript = reverseScript, forwardScript
		}()
		variant := fmt.Sprintf("\n-- synthetic uncached variant %s", server.NewId())
		verifyReverseEgressLeaseRedisScript = redis.NewScript(verifyReverseEgressLeaseScript + variant)
		verifyForwardEgressLeaseRedisScript = redis.NewScript(verifyForwardEgressLeaseScript + variant)

		clientIds := []server.Id{server.NewId(), server.NewId()}
		ips := []netip.Addr{netip.MustParseAddr("192.0.2.20"), netip.MustParseAddr("192.0.2.21")}
		SetProvide(ctx, clientIds[0], map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		testVerifyInsertProxyClients(ctx, clientIds, ips)

		var roundTripCounts []int
		observedCtx := Testing_WithVerifyProxyEgressRefreshObserver(ctx, func(_ []server.Id, roundTripCount int) {
			roundTripCounts = append(roundTripCounts, roundTripCount)
		})
		for range 2 {
			RefreshVerifyProxyEgress(observedCtx, settings)
			for index, clientId := range clientIds {
				state := testVerifyProxyEgressStateOf(ctx, clientId, ips[index], settings)
				want := testVerifyProxyEgressState{resolved: true, eligible: index == 0, reverseFieldCount: 1}
				if state != want {
					t.Fatalf("allocation %d after refresh %d: %+v, want %+v", index, len(roundTripCounts), state, want)
				}
			}
		}
		if len(roundTripCounts) != 2 || roundTripCounts[0] != roundTripCounts[1]+1 {
			t.Fatalf("uncached scripts must cost one more wave, once: round trips %v", roundTripCounts)
		}
	})
}
