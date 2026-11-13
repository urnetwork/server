package model

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Which tags of a contract's endpoints are its extender parties
// (connect/EXTENDER.md J1, J2): only a tag that can be a relay (owner,
// 2026-11-11: "Any contract that comes from an extender should treat the
// extender as a 1/n party on the contract", and only contracts actually
// relayed through an extender earn that share). On 2026-11-11, 72% of the rows
// were an extender's own provider client tagged by its own address and 22%
// clients of its own network packed behind that address, none of them relayed
// (research extender-health-2026-11-11 §1.3, §2.3).
//
// Addresses are RFC 5737 and RFC 3849 documentation addresses and every id is
// generated.

// A client of `networkId` that holds a provide key of each mode given, as a
// provide stores them (SetProvide), connected from each address given.
func testContractRelayEndpoint(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	provideModes []ProvideMode,
	ips ...string,
) server.Id {
	t.Helper()
	clientId := server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{clientId: networkId})
	testContractRelayProvideKeys(ctx, clientId, provideModes...)
	for _, ip := range ips {
		testConnectFrom(t, ctx, clientId, ip)
	}
	return clientId
}

// Stores a provide key of each mode for clientId.
func testContractRelayProvideKeys(ctx context.Context, clientId server.Id, provideModes ...ProvideMode) {
	server.Tx(ctx, func(tx server.PgTx) {
		for _, provideMode := range provideModes {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO provide_key (client_id, provide_mode, secret_key)
				VALUES ($1, $2, $3)
				`,
				clientId,
				provideMode,
				[]byte(fmt.Sprintf("provide-key-%d", provideMode)),
			))
		}
	})
}

// The provide keys of a public provider: a device that provides publicly
// holds the public, friends and family, and network keys.
var testContractRelayPublicProvideModes = []ProvideMode{
	ProvideModeNetwork,
	ProvideModeFriendsAndFamily,
	ProvideModePublic,
}

// A tag of the extender's own provider client, of another client of its
// network, or of a public provider is no party: none is relayed. Before, every
// one was. A consumer of another network, and a provider of another network
// that provides to its own network only, which may reach the platform through
// an extender (J4), stay parties, and so does a consumer whose connection
// leaves from another address of the extender's ipv6 /64, which was untagged.
func TestContractExtenderPartiesAreOnlyTheTagsThatCanBeRelays(t *testing.T) {
	useSyntheticIpDb(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const transferByteCount = ByteCount(1024)

		extenderNetworkId := server.NewId()
		extenderClientId := server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{extenderClientId: extenderNetworkId})
		extender := testContractExtender(
			ctx,
			"relay-rules",
			extenderClientId,
			extenderNetworkId,
			"192.0.2.40",
			"2001:db8:40::40",
		)
		party := func(party ContractParty) testContractExtenderParty {
			return testContractExtenderParty{
				extenderId: extender.ExtenderId,
				party:      party,
				clientId:   extenderClientId,
				networkId:  extenderNetworkId,
			}
		}

		// the extender's own provider client, connected from its own address
		testConnectFrom(t, ctx, extenderClientId, "192.0.2.40")

		type endpoint struct {
			// the extender's network, else a network of its own
			extenderNetwork bool
			// the extender's own provider client
			extenderClient bool
			provideModes   []ProvideMode
			ips            []string
		}
		direct := endpoint{ips: []string{"198.51.100.40"}}
		for _, test := range []struct {
			name        string
			source      endpoint
			destination endpoint
			wantParties []testContractExtenderParty
		}{
			{
				name:        "the extender's own provider client",
				source:      direct,
				destination: endpoint{extenderClient: true},
				wantParties: []testContractExtenderParty{},
			},
			{
				name:        "a consumer of the extender's network behind its address",
				source:      endpoint{extenderNetwork: true, ips: []string{"192.0.2.40"}},
				destination: direct,
				wantParties: []testContractExtenderParty{},
			},
			{
				// a network provider, so it is the network alone that refuses
				// the tag
				name:        "a provider of the extender's network behind its address",
				source:      direct,
				destination: endpoint{extenderNetwork: true, provideModes: []ProvideMode{ProvideModeNetwork}, ips: []string{"192.0.2.40"}},
				wantParties: []testContractExtenderParty{},
			},
			{
				name:        "a public provider of another network on the extender's address",
				source:      direct,
				destination: endpoint{provideModes: testContractRelayPublicProvideModes, ips: []string{"192.0.2.40"}},
				wantParties: []testContractExtenderParty{},
			},
			{
				// the direction of a companion, whose source is the provider
				name:        "a public provider of another network on the extender's address as the source",
				source:      endpoint{provideModes: testContractRelayPublicProvideModes, ips: []string{"192.0.2.40"}},
				destination: direct,
				wantParties: []testContractExtenderParty{},
			},
			{
				name:        "a consumer of another network relayed by the extender",
				source:      endpoint{ips: []string{"192.0.2.40"}},
				destination: direct,
				wantParties: []testContractExtenderParty{party(ContractPartySource)},
			},
			{
				name:        "a network provider of another network relayed by the extender",
				source:      direct,
				destination: endpoint{provideModes: []ProvideMode{ProvideModeNetwork}, ips: []string{"192.0.2.40"}},
				wantParties: []testContractExtenderParty{party(ContractPartyDestination)},
			},
			{
				name:        "a relayed consumer and a public provider on the extender's address",
				source:      endpoint{ips: []string{"192.0.2.40"}},
				destination: endpoint{provideModes: testContractRelayPublicProvideModes, ips: []string{"192.0.2.40"}},
				wantParties: []testContractExtenderParty{party(ContractPartySource)},
			},
			{
				// the extender's onward dial left from a temporary address of
				// its /64
				name:        "a consumer relayed from another address of the extender's /64",
				source:      endpoint{ips: []string{"2001:db8:40::9e:1"}},
				destination: direct,
				wantParties: []testContractExtenderParty{party(ContractPartySource)},
			},
		} {
			endpointId := func(e endpoint) (server.Id, server.Id) {
				if e.extenderClient {
					return extenderNetworkId, extenderClientId
				}
				networkId := server.NewId()
				if e.extenderNetwork {
					networkId = extenderNetworkId
				}
				return networkId, testContractRelayEndpoint(t, ctx, networkId, e.provideModes, e.ips...)
			}
			sourceNetworkId, sourceId := endpointId(test.source)
			destinationNetworkId, destinationId := endpointId(test.destination)
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
				t.Fatalf("%s: create escrow: %v", test.name, err)
			}
			// every case is reported, not only the first that differs
			want := slices.Clone(test.wantParties)
			slices.SortFunc(want, compareTestContractExtenderParties)
			if got := testContractExtenderParties(t, ctx, escrow.ContractId); !slices.Equal(got, want) {
				t.Errorf("%s: extender parties = %v, want %v", test.name, got, want)
			}
		}
	})
}

// A public provider on an extender's address is paid its whole contract, and
// the SN usage credits it alone (J4). Before, the extender's network was paid
// half for a contract it never relayed.
func TestContractExtenderPayoutPaysAPublicProviderOnAnExtendersAddressWhole(t *testing.T) {
	useSyntheticIpDb(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const usedByteCount = ByteCount(121)

		extenderNetworkId := server.NewId()
		extenderClientId := server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{extenderClientId: extenderNetworkId})
		testContractExtender(ctx, "public-provider-payout", extenderClientId, extenderNetworkId, "203.0.113.60")

		originNetworkId := server.NewId()
		originClientId := testContractRelayEndpoint(t, ctx, originNetworkId, nil, "198.51.100.60")
		egressNetworkId := server.NewId()
		egressClientId := testContractRelayEndpoint(t, ctx, egressNetworkId, testContractRelayPublicProvideModes, "203.0.113.60")
		balance := addContractPayoutTestBalance(ctx, originNetworkId, usedByteCount)

		escrow, err := CreateTransferEscrow(
			ctx,
			originNetworkId,
			originClientId,
			egressNetworkId,
			egressClientId,
			usedByteCount,
		)
		if err != nil {
			t.Fatalf("create escrow: %v", err)
		}
		assertContractExtenderParties(t, ctx, "public provider", escrow.ContractId, []testContractExtenderParty{})
		if err := CloseContract(ctx, escrow.ContractId, originClientId, usedByteCount, false); err != nil {
			t.Fatalf("close origin: %v", err)
		}
		if err := CloseContract(ctx, escrow.ContractId, egressClientId, usedByteCount, false); err != nil {
			t.Fatalf("close egress: %v", err)
		}

		want := map[server.Id]contractPayoutTestAmount{
			egressNetworkId: {byteCount: usedByteCount, payout: NanoCents(usedByteCount)},
		}
		if got := contractPayoutTestAmounts(t, ctx, escrow.ContractId); !maps.Equal(got, want) {
			t.Fatalf("payout = %v, want %v", got, want)
		}
		assertContractPayoutTestProviderAllocations(t, ctx, escrow.ContractId, map[server.Id]int64{
			egressClientId: int64(usedByteCount),
		})
		assertContractPayoutTestBalanceConsumed(t, ctx, balance.BalanceId, escrow.ContractId, usedByteCount)
		assertStContractUsageSnapshotHops(t, ctx, escrow.ContractId, []ContractParticipant{
			{ClientId: egressClientId, NetworkId: egressNetworkId},
		}, usedByteCount, originNetworkId)
	})
}

// A connection from an ipv6 address in the /64 of an extender's active address
// carries that extender, as the extender's own does: an extender relays on the
// family its client arrived on (A7), and its host commonly sends from a
// temporary address of the /64 it activated from. Before, only the exact
// address was tagged, and such a relay earned nothing. A /64 that two
// extenders share tags each at its own address and nothing else in it, another
// /64 is another host, and an ipv4 address is still matched exactly.
func TestConnectNetworkClientTagsTheExtenderOfItsIpv6Prefix(t *testing.T) {
	useSyntheticIpDb(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extender := testExtenderCacheExtender(
			ctx,
			"prefix",
			true,
			testExtenderCacheAddress("192.0.2.80", true),
			testExtenderCacheAddress("2001:db8:80::10", true),
		)
		firstSharing := testExtenderCacheExtender(
			ctx,
			"shared-prefix-a",
			true,
			testExtenderCacheAddress("2001:db8:81::10", true),
		)
		secondSharing := testExtenderCacheExtender(
			ctx,
			"shared-prefix-b",
			true,
			testExtenderCacheAddress("2001:db8:81::20", true),
		)
		Testing_ResetExtenderAddressCache()

		clientId := server.NewId()
		for _, c := range []struct {
			name          string
			clientAddress string
			ipFamily      int
			want          *server.Id
		}{
			{name: "the exact ipv6 address", clientAddress: "[2001:db8:80::10]:443", ipFamily: 6, want: &extender.ExtenderId},
			{name: "a temporary address of the /64", clientAddress: "[2001:db8:80::9e:1]:443", ipFamily: 6, want: &extender.ExtenderId},
			{name: "another address of the /64", clientAddress: "[2001:db8:80:0:1234:5678:9abc:def0]:443", ipFamily: 6, want: &extender.ExtenderId},
			{name: "another /64 of the /48", clientAddress: "[2001:db8:80:1::10]:443", ipFamily: 6},
			{name: "the first of a shared /64", clientAddress: "[2001:db8:81::10]:443", ipFamily: 6, want: &firstSharing.ExtenderId},
			{name: "the second of a shared /64", clientAddress: "[2001:db8:81::20]:443", ipFamily: 6, want: &secondSharing.ExtenderId},
			{name: "another address of a shared /64", clientAddress: "[2001:db8:81::30]:443", ipFamily: 6},
			{name: "the exact ipv4 address", clientAddress: "192.0.2.80:443", ipFamily: 4, want: &extender.ExtenderId},
			{name: "an ipv4 neighbor", clientAddress: "192.0.2.81:443", ipFamily: 4},
		} {
			got := testConnectExtenderId(t, ctx, clientId, c.clientAddress, c.ipFamily)
			if c.want == nil {
				if got != nil {
					t.Errorf("%s: extender = %s, want none", c.name, got)
				}
				continue
			}
			if got == nil || *got != *c.want {
				t.Errorf("%s: extender = %v, want %s", c.name, got, *c.want)
			}
		}
	})
}
