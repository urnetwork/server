package model

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The connect-time extender tag (connect/EXTENDER.md J1).
//
// Everything here is synthetic: RFC 5737 and RFC 3849 documentation addresses,
// and public keys derived from the extender's name so the unique key holds
// without a real key. What is under test is which connections carry an
// extender id, not what an extender is.

// One active address row on the family of ip.
func testExtenderCacheAddress(ip string, active bool) *NetworkExtenderAddress {
	addr := netip.MustParseAddr(ip)
	return &NetworkExtenderAddress{
		IpVersion:    server.IpVersionForAddr(addr),
		Ip:           addr,
		Carriers:     []string{connect.ExtenderCarrierTcp},
		ActivateTime: server.NowUtc(),
		Active:       active,
	}
}

// One extender with its addresses, stored as the activation would have.
func testExtenderCacheExtender(
	ctx context.Context,
	name string,
	active bool,
	addresses ...*NetworkExtenderAddress,
) *NetworkExtender {
	extender := &NetworkExtender{
		ExtenderId:  server.NewId(),
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   []byte("extender-cache-" + name),
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "US",
		Active:      active,
	}
	Testing_CreateNetworkExtender(ctx, extender, addresses)
	return extender
}

// The extender stored on one connection row, nil when the connection was not
// tagged.
func testConnectionExtenderId(
	t testing.TB,
	ctx context.Context,
	connectionId server.Id,
) *server.Id {
	t.Helper()
	var extenderId *server.Id
	found := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT extender_id
			FROM network_client_connection
			WHERE connection_id = $1
			`,
			connectionId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&extenderId))
			}
		})
	})
	if !found {
		t.Fatalf("connection %s is missing", connectionId)
	}
	return extenderId
}

// Connects clientId from clientAddress and returns the extender the connection
// was tagged with.
func testConnectExtenderId(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	clientAddress string,
	ipFamilyIntent int,
) *server.Id {
	t.Helper()
	handlerId := CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(
		ctx,
		clientId,
		clientAddress,
		handlerId,
		ipFamilyIntent,
	)
	if err != nil {
		t.Fatalf("connect %s from %s: %v", clientId, clientAddress, err)
	}
	return testConnectionExtenderId(t, ctx, connectionId)
}

func assertTaggedExtenderId(
	t testing.TB,
	name string,
	got *server.Id,
	want server.Id,
) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s: extender = %v, want %s", name, got, want)
	}
}

// A connection from an active address of an active extender carries that
// extender on both families; any other address carries none.
func TestConnectNetworkClientTagsAnActiveExtenderAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extender := testExtenderCacheExtender(
			ctx,
			"dual",
			true,
			testExtenderCacheAddress("192.0.2.10", true),
			testExtenderCacheAddress("2001:db8:e::10", true),
		)
		Testing_ResetExtenderAddressCache()

		clientId := server.NewId()
		assertTaggedExtenderId(
			t,
			"v4",
			testConnectExtenderId(t, ctx, clientId, "192.0.2.10:443", 4),
			extender.ExtenderId,
		)
		assertTaggedExtenderId(
			t,
			"v6",
			testConnectExtenderId(t, ctx, clientId, "[2001:db8:e::10]:443", 6),
			extender.ExtenderId,
		)

		// the same client dialing the platform directly
		if extenderId := testConnectExtenderId(t, ctx, clientId, "198.51.100.7:443", 4); extenderId != nil {
			t.Fatalf("direct connection extender = %s, want none", extenderId)
		}
		if extenderId := testConnectExtenderId(t, ctx, clientId, "[2001:db8:f::7]:443", 6); extenderId != nil {
			t.Fatalf("direct v6 connection extender = %s, want none", extenderId)
		}
	})
}

// An extender that is no longer active, and an address that is no longer
// active, are both invisible to the tag: the two flags are independent and the
// lookup requires both.
func TestConnectNetworkClientDoesNotTagAnInactiveExtenderOrAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		testExtenderCacheExtender(
			ctx,
			"revoked",
			false,
			testExtenderCacheAddress("203.0.113.5", true),
		)
		testExtenderCacheExtender(
			ctx,
			"deactivated-address",
			true,
			testExtenderCacheAddress("203.0.113.6", false),
		)
		Testing_ResetExtenderAddressCache()

		clientId := server.NewId()
		if extenderId := testConnectExtenderId(t, ctx, clientId, "203.0.113.5:443", 4); extenderId != nil {
			t.Fatalf("inactive extender tagged %s, want none", extenderId)
		}
		if extenderId := testConnectExtenderId(t, ctx, clientId, "203.0.113.6:443", 4); extenderId != nil {
			t.Fatalf("inactive address tagged %s, want none", extenderId)
		}
	})
}

// The cache is a cache: an address activated after a load is attributed only
// once the refresh has run. In production that is the 60 s window; here the
// refresh is forced so nothing waits on a clock.
func TestExtenderAddressCacheRefreshPicksUpANewAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		Testing_ResetExtenderAddressCache()
		clientId := server.NewId()
		// loads the empty directory
		if extenderId := testConnectExtenderId(t, ctx, clientId, "192.0.2.20:443", 4); extenderId != nil {
			t.Fatalf("unknown address tagged %s, want none", extenderId)
		}

		extender := testExtenderCacheExtender(
			ctx,
			"late",
			true,
			testExtenderCacheAddress("192.0.2.20", true),
		)
		if extenderId := testConnectExtenderId(t, ctx, clientId, "192.0.2.20:443", 4); extenderId != nil {
			t.Fatalf("address tagged %s before the refresh, want none", extenderId)
		}

		Testing_RefreshExtenderAddressCache()
		assertTaggedExtenderId(
			t,
			"refreshed",
			testConnectExtenderId(t, ctx, clientId, "192.0.2.20:443", 4),
			extender.ExtenderId,
		)

		// ActiveExtenderIdForAddress is the same lookup the connect path uses
		activeExtenderId, found := ActiveExtenderIdForAddress(netip.MustParseAddr("192.0.2.20"))
		if !found || activeExtenderId != extender.ExtenderId {
			t.Fatalf("lookup = %s/%t, want %s/true", activeExtenderId, found, extender.ExtenderId)
		}
		if _, found := ActiveExtenderIdForAddress(netip.MustParseAddr("192.0.2.21")); found {
			t.Fatal("an unknown address resolved to an extender")
		}
	})
}

// The cache serves one in-flight load at a time and fences every load to the
// generation it started in.
//
// Both rules only matter in a race, so they are driven through the state
// machine directly rather than through a timing window: a second caller must be
// told another load holds the claim, and a loader that started before a reset
// must neither publish into the replacement environment nor release the
// replacement's claim. Getting either wrong costs an attribution that nothing
// revisits, and nothing would report it.
func TestExtenderAddressCacheLoadsOnceAndFencesOldGenerations(t *testing.T) {
	state := newExtenderAddressCacheState()

	generation, started := state.startLoad()
	if !started {
		t.Fatal("the first load was not started")
	}
	if _, started := state.startLoad(); started {
		t.Fatal("a second load started while the first holds the claim")
	}

	// the loader that took the claim releases it
	state.finishLoad(generation)
	current, started := state.startLoad()
	if !started {
		t.Fatal("the claim was not released")
	}

	addrExtenderIds := map[netip.Addr]server.Id{
		netip.MustParseAddr("192.0.2.70"): server.NewId(),
	}
	if !state.publish(current, addrExtenderIds) {
		t.Fatal("the current generation could not publish")
	}
	if snapshot := state.snapshot.Load(); snapshot == nil || len(snapshot.addrExtenderIds) != 1 {
		t.Fatalf("published snapshot = %v", snapshot)
	}

	// a reset is a replacement environment: the snapshot is dropped, the claim
	// is cleared, and the generation that was running is canceled
	state.reset()
	if snapshot := state.snapshot.Load(); snapshot != nil {
		t.Fatal("a reset kept the old snapshot")
	}
	if state.current(current) {
		t.Fatal("the generation from before the reset is still current")
	}
	if state.publish(current, addrExtenderIds) {
		t.Fatal("a generation from before the reset published")
	}
	if state.snapshot.Load() != nil {
		t.Fatal("a generation from before the reset published into the replacement")
	}

	// a loader from before the reset that finishes late must not release the
	// replacement's claim
	replacement, started := state.startLoad()
	if !started {
		t.Fatal("the replacement could not start a load")
	}
	state.finishLoad(current)
	if _, started := state.startLoad(); started {
		t.Fatal("a stale loader released the current generation's claim")
	}
	state.finishLoad(replacement)
	if _, started := state.startLoad(); !started {
		t.Fatal("the replacement's claim was never released")
	}
}

// Addresses are keyed unmapped, so a v4 caller observed through a v4-mapped v6
// socket matches the v4 address row that was activated. The same extender
// relaying the same client must not lose its attribution because the platform
// happened to accept the connection on a dual-stack socket.
func TestActiveExtenderIdForAddressUnmapsAV4MappedCaller(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extender := testExtenderCacheExtender(
			ctx,
			"mapped",
			true,
			testExtenderCacheAddress("192.0.2.72", true),
		)
		Testing_ResetExtenderAddressCache()
		Testing_RefreshExtenderAddressCache()

		for _, ip := range []string{"192.0.2.72", "::ffff:192.0.2.72"} {
			extenderId, found := ActiveExtenderIdForAddress(netip.MustParseAddr(ip))
			if !found || extenderId != extender.ExtenderId {
				t.Fatalf("%s = %s/%t, want %s/true", ip, extenderId, found, extender.ExtenderId)
			}
		}
		if _, found := ActiveExtenderIdForAddress(netip.MustParseAddr("::ffff:192.0.2.73")); found {
			t.Fatal("an unknown mapped address resolved to an extender")
		}
	})
}

// Every connection made while a snapshot is current is tagged from that one
// snapshot. The snapshot is immutable and swapped whole, so concurrent connects
// read one consistent directory rather than a map another goroutine is filling.
func TestConnectNetworkClientTagsConcurrentConnectsFromOneSnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extender := testExtenderCacheExtender(
			ctx,
			"concurrent",
			true,
			testExtenderCacheAddress("192.0.2.74", true),
			testExtenderCacheAddress("2001:db8:e::74", true),
		)
		Testing_ResetExtenderAddressCache()
		// the snapshot is loaded before the barrier, so what the connects race
		// on is the read rather than the first load
		Testing_RefreshExtenderAddressCache()

		const connectCount = 16
		type concurrentConnect struct {
			connectionId server.Id
			err          error
		}
		connects := make([]concurrentConnect, connectCount)
		start := make(chan struct{})
		wait := sync.WaitGroup{}
		for i := range connectCount {
			clientId := server.NewId()
			clientAddress := "192.0.2.74:443"
			ipFamilyIntent := 4
			if i%2 == 1 {
				clientAddress = "[2001:db8:e::74]:443"
				ipFamilyIntent = 6
			}
			handlerId := CreateNetworkClientHandler(ctx)
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(
					ctx,
					clientId,
					clientAddress,
					handlerId,
					ipFamilyIntent,
				)
				connects[i] = concurrentConnect{connectionId: connectionId, err: err}
			}()
		}
		close(start)
		wait.Wait()

		for i, connect := range connects {
			if connect.err != nil {
				t.Fatalf("connect %d: %v", i, connect.err)
			}
			assertTaggedExtenderId(
				t,
				fmt.Sprintf("concurrent connect %d", i),
				testConnectionExtenderId(t, ctx, connect.connectionId),
				extender.ExtenderId,
			)
		}
	})
}

// A stale snapshot is served while the refresh runs behind it, so no connection
// waits on the directory once a process has one. A refresh that blocked the
// caller would put a database read on the connect path every minute.
func TestExtenderAddressCacheServesAStaleSnapshotWhileItRefreshes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extender := testExtenderCacheExtender(
			ctx,
			"stale",
			true,
			testExtenderCacheAddress("192.0.2.75", true),
		)
		Testing_ResetExtenderAddressCache()
		t.Cleanup(Testing_ResetExtenderAddressCache)

		// a snapshot older than the staleness window, holding an address that is
		// in no table at all: anything the cache serves from it can only have
		// come from the snapshot rather than from a read
		staleExtenderId := server.NewId()
		staleAddr := netip.MustParseAddr("192.0.2.99")
		currentExtenderAddressCache.snapshot.Store(&extenderAddressCacheSnapshot{
			addrExtenderIds: map[netip.Addr]server.Id{staleAddr: staleExtenderId},
			loadTime:        server.NowUtc().Add(-2 * extenderAddressCacheStaleAfter),
		})

		// the stale answer, not a nil one and not the database's
		got, found := ActiveExtenderIdForAddress(staleAddr)
		if !found || got != staleExtenderId {
			t.Fatalf("stale lookup = %s/%t, want %s/true", got, found, staleExtenderId)
		}
		if _, found := ActiveExtenderIdForAddress(netip.MustParseAddr("192.0.2.75")); found {
			t.Fatal("the stale snapshot answered with an address it does not hold")
		}

		// once a refresh has published, the directory is what answers
		Testing_RefreshExtenderAddressCache()
		got, found = ActiveExtenderIdForAddress(netip.MustParseAddr("192.0.2.75"))
		if !found || got != extender.ExtenderId {
			t.Fatalf("refreshed lookup = %s/%t, want %s/true", got, found, extender.ExtenderId)
		}
		if _, found := ActiveExtenderIdForAddress(staleAddr); found {
			t.Fatal("the refreshed snapshot still holds the stale address")
		}
	})
}
