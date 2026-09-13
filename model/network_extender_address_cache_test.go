package model

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
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
