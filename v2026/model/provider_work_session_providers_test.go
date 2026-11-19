// The session journal for provider clients only (server
// db_provider_work_session_providers.go) through the real provide and
// connection owners: SetProvide, ConnectNetworkClient and
// DisconnectNetworkClient, with no signer.
package model

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The append's provider modes are the friends and family and public modes the
// provide keys store.
func TestProviderWorkSessionAppendReadsFriendsAndFamilyAndPublicKeys(t *testing.T) {
	providerAppend := server.ProviderWorkSessionProviderAppendBodySql()
	modes := fmt.Sprintf("provide_mode IN (%d,%d)", ProvideModeFriendsAndFamily, ProvideModePublic)
	if strings.Count(providerAppend, modes) != 2 || strings.Count(providerAppend, "provide_mode") != 2 {
		t.Fatalf("the append does not read exactly the %q keys at both gates:\n%s", modes, providerAppend)
	}
}

// A client of a network, with its connect day recorded, and a connection
// handler.
func newProviderWorkSessionProvidersClient(t testing.TB, ctx context.Context) (clientId server.Id, networkId server.Id, handlerId server.Id) {
	t.Helper()
	clientId, networkId = server.NewId(), server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{clientId: networkId})
	waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, server.NowUtc()))
	handlerId = CreateNetworkClientHandler(ctx)
	return
}

// The client's events as (kind, network) pairs in sequence order, and its head.
func providerWorkSessionProvidersJournal(t testing.TB, ctx context.Context, clientId server.Id) (events []string, head int64) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT kind, network_id FROM provider_work_session_event WHERE client_id = $1 ORDER BY sequence`, clientId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var kind string
				var networkId *server.Id
				server.Raise(result.Scan(&kind, &networkId))
				network := "-"
				if networkId != nil {
					network = networkId.String()
				}
				events = append(events, kind+"@"+network)
			}
		})
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT sequence FROM provider_work_session_head WHERE client_id = $1), 0)`, clientId).Scan(&head))
	})
	return
}

// Opens and closes one connection through the real owners.
func connectAndDisconnectProviderWorkSessionClient(t testing.TB, ctx context.Context, clientId server.Id, handlerId server.Id, address string) {
	t.Helper()
	connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, clientId, address, handlerId, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := DisconnectNetworkClient(ctx, connectionId); err != nil {
		t.Fatal(err)
	}
}

// Each client provides as SetProvide records it, then connects and
// disconnects through the real owners. A client with no key, with Stream only
// (a device that does not provide), or with Network and Stream (a hosted
// window client) gets no event and no head; a friends and family or a public
// provider gets an admit and a retire with its network.
func TestProviderWorkSessionJournalsOnlyProvidersThroughRealOwners(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		key := make([]byte, 32)
		for index, test := range []struct {
			name    string
			modes   map[ProvideMode][]byte
			journal bool
		}{
			{"no provide", map[ProvideMode][]byte{}, false},
			{"return traffic", map[ProvideMode][]byte{ProvideModeStream: key}, false},
			{"window client", map[ProvideMode][]byte{ProvideModeNetwork: key, ProvideModeStream: key}, false},
			{"friends and family", map[ProvideMode][]byte{ProvideModeFriendsAndFamily: key, ProvideModeNetwork: key, ProvideModeStream: key}, true},
			{"public", map[ProvideMode][]byte{ProvideModePublic: key, ProvideModeFriendsAndFamily: key, ProvideModeNetwork: key, ProvideModeStream: key}, true},
		} {
			clientId, networkId, handlerId := newProviderWorkSessionProvidersClient(t, ctx)
			SetProvide(ctx, clientId, test.modes)
			connectAndDisconnectProviderWorkSessionClient(t, ctx, clientId, handlerId, fmt.Sprintf("192.0.2.%d:10001", 40+index))
			events, head := providerWorkSessionProvidersJournal(t, ctx, clientId)
			want, wantHead := []string(nil), int64(0)
			if test.journal {
				want, wantHead = []string{"admit@" + networkId.String(), "retire@" + networkId.String()}, 2
			}
			if strings.Join(events, ",") != strings.Join(want, ",") || head != wantHead {
				t.Fatalf("%s: journal %v head %d, want %v head %d", test.name, events, head, want, wantHead)
			}
		}
	})
}

// A public provider that pauses while connected (its provide drops the
// public and friends and family keys) retires that connection with the admit's
// network; while paused its connections are not journaled, and once it
// provides again they are.
func TestProviderWorkSessionPausedProviderRetiresItsAdmittedConnection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		key := make([]byte, 32)
		providing := map[ProvideMode][]byte{ProvideModePublic: key, ProvideModeFriendsAndFamily: key, ProvideModeNetwork: key, ProvideModeStream: key}
		paused := map[ProvideMode][]byte{ProvideModeNetwork: key, ProvideModeStream: key}
		clientId, networkId, handlerId := newProviderWorkSessionProvidersClient(t, ctx)
		SetProvide(ctx, clientId, providing)
		connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, clientId, "192.0.2.50:10001", handlerId, 4)
		if err != nil {
			t.Fatal(err)
		}
		SetProvide(ctx, clientId, paused)
		if err := DisconnectNetworkClient(ctx, connectionId); err != nil {
			t.Fatal(err)
		}
		connectAndDisconnectProviderWorkSessionClient(t, ctx, clientId, handlerId, "192.0.2.51:10001")
		SetProvide(ctx, clientId, providing)
		connectAndDisconnectProviderWorkSessionClient(t, ctx, clientId, handlerId, "192.0.2.52:10001")
		events, head := providerWorkSessionProvidersJournal(t, ctx, clientId)
		network := "@" + networkId.String()
		want := []string{"admit" + network, "retire" + network, "admit" + network, "retire" + network}
		if strings.Join(events, ",") != strings.Join(want, ",") || head != 4 {
			t.Fatalf("journal %v head %d, want %v head 4", events, head, want)
		}
	})
}
