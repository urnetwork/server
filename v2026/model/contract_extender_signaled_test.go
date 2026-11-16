package model

import (
	"context"
	"regexp"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The extender parties of a contract whose endpoint is tagged with an extender
// reached over the webrtc carrier alone (connect/EXTENDER.md J2, S8). Such an
// address is no first contact: a client reaches it only by signaling the
// extender through a connection of its own client to the platform, and a
// window client is derived from that client. So an endpoint whose connections,
// and whose source client's, all arrived through the extender shares its
// address, a host behind the same nat, and was relayed by no one.
//
// On 2026-11-16 one extender on the webrtc carrier alone was a party to every
// contract of 84 clients of two other networks, on a release that cannot dial
// the webrtc carrier, whose every connection and whose parents' every
// connection arrived from the extender's address: 248 contracts and 235 MB
// credited to the extender in 4 hours. Before, every such endpoint made the
// extender a 1/n party (owner, 2026-11-11: only real relays earn).
//
// Addresses are RFC 5737 and RFC 3849 documentation addresses and every id is
// generated.

// One extender of its own network whose addresses list carriers.
func testSignaledContractExtender(
	ctx context.Context,
	name string,
	carriers []string,
	ips ...string,
) *NetworkExtender {
	networkId := server.NewId()
	clientId := server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{clientId: networkId})
	addresses := []*NetworkExtenderAddress{}
	for _, ip := range ips {
		address := testExtenderCacheAddress(ip, true)
		address.Carriers = carriers
		addresses = append(addresses, address)
	}
	extender := &NetworkExtender{
		ExtenderId:  server.NewId(),
		NetworkId:   networkId,
		ClientId:    clientId,
		PublicKey:   []byte("signaled-extender-" + name),
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "US",
		Active:      true,
	}
	Testing_CreateNetworkExtender(ctx, extender, addresses)
	Testing_RefreshExtenderAddressCache()
	return extender
}

// A client of networkId derived from sourceClientId, as a window client is.
func testSignaledDerivedClient(ctx context.Context, networkId server.Id, sourceClientId server.Id) server.Id {
	clientId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_client (client_id, network_id, active, source_client_id)
			VALUES ($1, $2, true, $3)
			`,
			clientId,
			networkId,
			sourceClientId,
		))
	})
	return clientId
}

// How a contract's source endpoint is connected.
type testSignaledEndpoint struct {
	// the addresses of the client the source is derived from, connected
	// first; none for a top-level source
	parentIps []string
	// the source's own addresses, connected in order
	ips []string
}

// Connects a source of a network of its own as endpoint says, creates a
// contract from it to destinationId, and returns the contract's extender
// parties.
func testSignaledContractParties(
	t testing.TB,
	ctx context.Context,
	name string,
	endpoint testSignaledEndpoint,
	destinationNetworkId server.Id,
	destinationId server.Id,
) []testContractExtenderParty {
	t.Helper()
	const transferByteCount = ByteCount(1024)
	sourceNetworkId := server.NewId()
	sourceId := server.NewId()
	if 0 < len(endpoint.parentIps) {
		parentId := testContractRelayEndpoint(t, ctx, sourceNetworkId, nil, endpoint.parentIps...)
		sourceId = testSignaledDerivedClient(ctx, sourceNetworkId, parentId)
	} else {
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: sourceNetworkId})
	}
	for _, ip := range endpoint.ips {
		testConnectFrom(t, ctx, sourceId, ip)
	}
	addContractPayoutTestBalance(ctx, sourceNetworkId, transferByteCount)
	escrow, err := CreateTransferEscrow(
		ctx,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		transferByteCount,
	)
	if err != nil {
		t.Fatalf("%s: create escrow: %v", name, err)
	}
	return testContractExtenderParties(t, ctx, escrow.ContractId)
}

// An extender reached over the webrtc carrier alone is a party of a contract
// only when its endpoint, or the client the endpoint is derived from, holds a
// connected connection that did not arrive through it. Before, it was a party
// of every contract of every client of another network behind its address.
func TestSignaledOnlyExtenderIsAPartyOnlyOfASignaledClientsContracts(t *testing.T) {
	useSyntheticIpDb(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extender := testSignaledContractExtender(
			ctx,
			"parties",
			[]string{connect.ExtenderCarrierWebRtc},
			"192.0.2.50",
			"2001:db8:50::50",
		)
		testSignaledContractExtender(
			ctx,
			"parties-other",
			[]string{connect.ExtenderCarrierTcp},
			"203.0.113.51",
		)
		relayed := []testContractExtenderParty{{
			extenderId: extender.ExtenderId,
			party:      ContractPartySource,
			clientId:   extender.ClientId,
			networkId:  extender.NetworkId,
		}}

		destinationNetworkId := server.NewId()
		destinationId := testContractRelayEndpoint(t, ctx, destinationNetworkId, testContractRelayPublicProvideModes, "198.51.100.59")

		for _, c := range []struct {
			name        string
			endpoint    testSignaledEndpoint
			wantParties []testContractExtenderParty
		}{
			{
				name:        "a client behind the extender's nat with no other connection",
				endpoint:    testSignaledEndpoint{ips: []string{"192.0.2.50"}},
				wantParties: []testContractExtenderParty{},
			},
			{
				name:        "a client behind the extender's nat on both families",
				endpoint:    testSignaledEndpoint{ips: []string{"192.0.2.50", "2001:db8:50::50"}},
				wantParties: []testContractExtenderParty{},
			},
			{
				// the extender's ipv6 address is matched by its /64, so a
				// neighbour on the same lan is tagged too
				name:        "a client on the extender's /64 with no other connection",
				endpoint:    testSignaledEndpoint{ips: []string{"2001:db8:50::9e:1", "192.0.2.50"}},
				wantParties: []testContractExtenderParty{},
			},
			{
				name:        "a window client whose parent is behind the extender's nat too",
				endpoint:    testSignaledEndpoint{parentIps: []string{"192.0.2.50"}, ips: []string{"192.0.2.50"}},
				wantParties: []testContractExtenderParty{},
			},
			{
				name:        "a client relayed beside its own connection elsewhere",
				endpoint:    testSignaledEndpoint{ips: []string{"198.51.100.50", "192.0.2.50"}},
				wantParties: relayed,
			},
			{
				name:        "a client relayed beside its connection through another extender",
				endpoint:    testSignaledEndpoint{ips: []string{"203.0.113.51", "192.0.2.50"}},
				wantParties: relayed,
			},
			{
				name:        "a window client relayed beside its parent's connection elsewhere",
				endpoint:    testSignaledEndpoint{parentIps: []string{"198.51.100.52"}, ips: []string{"192.0.2.50"}},
				wantParties: relayed,
			},
			{
				name:        "a client relayed from the extender's /64 beside its own connection elsewhere",
				endpoint:    testSignaledEndpoint{ips: []string{"2001:db8:53::1", "2001:db8:50::9e:1"}},
				wantParties: relayed,
			},
		} {
			got := testSignaledContractParties(t, ctx, c.name, c.endpoint, destinationNetworkId, destinationId)
			// only this extender's party is judged here; the tcp extender's,
			// where the source is also its caller, is the relay rule's
			// (TestContractExtenderPartiesAreOnlyTheTagsThatCanBeRelays)
			got = slices.DeleteFunc(got, func(party testContractExtenderParty) bool {
				return party.extenderId != extender.ExtenderId
			})
			if !slices.Equal(got, c.wantParties) {
				t.Errorf("%s: extender parties = %v, want %v", c.name, got, c.wantParties)
			}
		}
	})
}

// The rule is the webrtc carrier's alone: an extender with an active address
// that is a first contact -- tcp, quic or dns beside webrtc -- is a party of a
// contract of a client behind its address, as before, since a client may dial
// that address directly. So is one that lists no carrier, which is judged by
// tcp, and one whose addresses all lapsed.
func TestFirstContactExtenderKeepsTheRelayRule(t *testing.T) {
	useSyntheticIpDb(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		destinationNetworkId := server.NewId()
		destinationId := testContractRelayEndpoint(t, ctx, destinationNetworkId, testContractRelayPublicProvideModes, "198.51.100.69")

		for i, c := range []struct {
			name     string
			carriers []string
			ip       string
		}{
			{name: "dns beside webrtc", carriers: []string{connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc}, ip: "192.0.2.61"},
			{name: "quic and dns beside webrtc", carriers: []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc}, ip: "192.0.2.62"},
			{name: "tcp beside webrtc", carriers: []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}, ip: "192.0.2.63"},
			{name: "tcp", carriers: []string{connect.ExtenderCarrierTcp}, ip: "192.0.2.64"},
			{name: "no carrier listed", carriers: []string{}, ip: "192.0.2.65"},
		} {
			extender := testSignaledContractExtender(ctx, "first-contact-"+string(rune('a'+i)), c.carriers, c.ip)
			want := []testContractExtenderParty{{
				extenderId: extender.ExtenderId,
				party:      ContractPartySource,
				clientId:   extender.ClientId,
				networkId:  extender.NetworkId,
			}}
			got := testSignaledContractParties(t, ctx, c.name, testSignaledEndpoint{ips: []string{c.ip}}, destinationNetworkId, destinationId)
			if !slices.Equal(got, want) {
				t.Errorf("%s: extender parties = %v, want %v", c.name, got, want)
			}
		}
	})
}

// Which carriers make an address one reached over the webrtc carrier alone:
// webrtc with none of tcp, quic and dns. An address that lists no carrier is
// judged by tcp, as every address was before webrtc.
func TestExtenderAddressSignaledOnly(t *testing.T) {
	for _, c := range []struct {
		carriers []string
		want     bool
	}{
		{carriers: []string{connect.ExtenderCarrierWebRtc}, want: true},
		{carriers: []string{connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc}, want: false},
		{carriers: []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc}, want: false},
		{carriers: []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierWebRtc}, want: false},
		{carriers: []string{connect.ExtenderCarrierTcp}, want: false},
		{carriers: []string{}, want: false},
		{carriers: nil, want: false},
	} {
		if got := ExtenderAddressSignaledOnly(c.carriers); got != c.want {
			t.Errorf("%v: signaled only = %t, want %t", c.carriers, got, c.want)
		}
	}
}

// The carriers patterns the party statement holds are the model's, and over
// the carriers columns an address stores they judge an address reached over
// the webrtc carrier alone exactly as ExtenderAddressSignaledOnly does.
func TestContractExtenderCarriersPatternsAreTheModels(t *testing.T) {
	if contractExtenderWebRtcCarriersSql != extenderWebRtcCarriersPattern {
		t.Fatalf("the statement's webrtc pattern is %s, want %s", contractExtenderWebRtcCarriersSql, extenderWebRtcCarriersPattern)
	}
	if contractExtenderFirstContactCarriersSql != extenderFirstContactCarriersPattern {
		t.Fatalf("the statement's first contact pattern is %s, want %s", contractExtenderFirstContactCarriersSql, extenderFirstContactCarriersPattern)
	}
	webRtc := regexp.MustCompile(contractExtenderWebRtcCarriersSql)
	firstContact := regexp.MustCompile(contractExtenderFirstContactCarriersSql)
	for _, carriers := range [][]string{
		{connect.ExtenderCarrierWebRtc},
		{connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc},
		{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns, connect.ExtenderCarrierWebRtc},
		{connect.ExtenderCarrierTcp, connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns},
		{connect.ExtenderCarrierTcp},
		{},
	} {
		column := joinExtenderCarriers(carriers)
		got := webRtc.MatchString(column) && !firstContact.MatchString(column)
		if want := ExtenderAddressSignaledOnly(carriers); got != want {
			t.Errorf("%q: the statement judges signaled only %t, want %t", column, got, want)
		}
	}
}
