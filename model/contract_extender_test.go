package model

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The extender parties of a contract (connect/EXTENDER.md J2): which extenders
// a contract records at creation, and that the rows go with the contract.
//
// Addresses are RFC 5737 and RFC 3849 documentation addresses and every id is
// generated, so nothing here names anything real.

// One row of contract_extender.
type testContractExtenderParty struct {
	extenderId server.Id
	party      ContractParty
	clientId   server.Id
	networkId  server.Id
}

func (self testContractExtenderParty) String() string {
	return fmt.Sprintf("%s/%s", self.extenderId, self.party)
}

func compareTestContractExtenderParties(
	a testContractExtenderParty,
	b testContractExtenderParty,
) int {
	if c := a.extenderId.Cmp(b.extenderId); c != 0 {
		return c
	}
	return strings.Compare(a.party, b.party)
}

// One active extender owned by a provider client and network, reachable at one
// address per family given. The address cache is refreshed so a connection
// made right after this is tagged.
func testContractExtender(
	ctx context.Context,
	name string,
	clientId server.Id,
	networkId server.Id,
	ips ...string,
) *NetworkExtender {
	addresses := []*NetworkExtenderAddress{}
	for _, ip := range ips {
		addresses = append(addresses, testExtenderCacheAddress(ip, true))
	}
	extender := &NetworkExtender{
		ExtenderId:  server.NewId(),
		NetworkId:   networkId,
		ClientId:    clientId,
		PublicKey:   []byte("contract-extender-" + name),
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

// Connects clientId from ip, the way an extender-relayed connection arrives:
// the platform observes the extender's address, not the client's.
func testConnectFrom(t testing.TB, ctx context.Context, clientId server.Id, ip string) {
	t.Helper()
	handlerId := CreateNetworkClientHandler(ctx)
	_, _, _, _, err := ConnectNetworkClient(
		ctx,
		clientId,
		net.JoinHostPort(ip, "443"),
		handlerId,
	)
	if err != nil {
		t.Fatalf("connect %s from %s: %v", clientId, ip, err)
	}
}

func testContractExtenderParties(
	t testing.TB,
	ctx context.Context,
	contractId server.Id,
) []testContractExtenderParty {
	t.Helper()
	parties := []testContractExtenderParty{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				extender_id,
				party,
				client_id,
				network_id
			FROM contract_extender
			WHERE contract_id = $1
			`,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var party testContractExtenderParty
				server.Raise(result.Scan(
					&party.extenderId,
					&party.party,
					&party.clientId,
					&party.networkId,
				))
				parties = append(parties, party)
			}
		})
	})
	slices.SortFunc(parties, compareTestContractExtenderParties)
	return parties
}

func assertContractExtenderParties(
	t testing.TB,
	ctx context.Context,
	name string,
	contractId server.Id,
	want []testContractExtenderParty,
) {
	t.Helper()
	slices.SortFunc(want, compareTestContractExtenderParties)
	got := testContractExtenderParties(t, ctx, contractId)
	if !slices.Equal(got, want) {
		t.Fatalf("%s: extender parties = %v, want %v", name, got, want)
	}
}

// Creates the two endpoint clients of a contract and returns them.
func testContractEndpoints(ctx context.Context) (
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
) {
	sourceNetworkId = server.NewId()
	sourceId = server.NewId()
	destinationNetworkId = server.NewId()
	destinationId = server.NewId()
	addContractPayoutTestClients(ctx, map[server.Id]server.Id{
		sourceId:      sourceNetworkId,
		destinationId: destinationNetworkId,
	})
	return
}

// The schema the tag and the parties depend on. The index is not decoration:
// the contract insert reads the connected connections of each endpoint by
// client id on the hottest path in the system, and the primary key is what
// makes one extender one row per contract and party.
func TestContractExtenderMigrationsApply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		columns := []string{}
		indexes := []string{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT column_name
				FROM information_schema.columns
				WHERE
					table_schema = 'public' AND
					(
						(table_name = 'contract_extender') OR
						(table_name = 'network_client_connection' AND column_name = 'extender_id')
					)
				ORDER BY table_name, column_name
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var columnName string
					server.Raise(result.Scan(&columnName))
					columns = append(columns, columnName)
				}
			})

			result, err = conn.Query(
				ctx,
				`
				SELECT indexname
				FROM pg_indexes
				WHERE
					schemaname = 'public' AND
					indexname IN (
						'contract_extender_pkey',
						'contract_extender_create_time_contract_id',
						'network_client_connection_client_id_connected_extender_id'
					)
				ORDER BY indexname
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var indexName string
					server.Raise(result.Scan(&indexName))
					indexes = append(indexes, indexName)
				}
			})
		})

		wantColumns := []string{
			// contract_extender
			"client_id",
			"contract_id",
			// the hour bucket key of the 24 hour contract counts
			// (connect/EXTENDER.md M3)
			"create_time",
			"extender_id",
			"network_id",
			"party",
			// network_client_connection
			"extender_id",
		}
		if !slices.Equal(columns, wantColumns) {
			t.Fatalf("extender party columns = %v, want %v", columns, wantColumns)
		}
		wantIndexes := []string{
			// the range scan the hourly count of contracts with an extender
			// party reads, instead of an existence probe per contract (M3)
			"contract_extender_create_time_contract_id",
			"contract_extender_pkey",
			"network_client_connection_client_id_connected_extender_id",
		}
		if !slices.Equal(indexes, wantIndexes) {
			t.Fatalf("extender party indexes = %v, want %v", indexes, wantIndexes)
		}
	})
}

// The extenders a contract records are the distinct active extenders of each
// endpoint's currently connected connections, one row per extender and party.
// Every case creates its own endpoint clients, so the connections of one case
// cannot leak into another.
func TestCreateTransferEscrowWritesTheExtenderParties(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const transferByteCount = ByteCount(1024)

		extenderAClientId := server.NewId()
		extenderANetworkId := server.NewId()
		extenderA := testContractExtender(
			ctx,
			"a",
			extenderAClientId,
			extenderANetworkId,
			"192.0.2.10",
			"2001:db8:a::10",
		)
		extenderBClientId := server.NewId()
		extenderBNetworkId := server.NewId()
		extenderB := testContractExtender(
			ctx,
			"b",
			extenderBClientId,
			extenderBNetworkId,
			"192.0.2.11",
		)

		partyA := func(party ContractParty) testContractExtenderParty {
			return testContractExtenderParty{
				extenderId: extenderA.ExtenderId,
				party:      party,
				clientId:   extenderAClientId,
				networkId:  extenderANetworkId,
			}
		}
		partyB := func(party ContractParty) testContractExtenderParty {
			return testContractExtenderParty{
				extenderId: extenderB.ExtenderId,
				party:      party,
				clientId:   extenderBClientId,
				networkId:  extenderBNetworkId,
			}
		}

		for _, test := range []struct {
			name           string
			sourceIps      []string
			destinationIps []string
			wantParties    []testContractExtenderParty
		}{
			{
				name:           "an extender on each side",
				sourceIps:      []string{"192.0.2.10"},
				destinationIps: []string{"192.0.2.11"},
				wantParties: []testContractExtenderParty{
					partyA(ContractPartySource),
					partyB(ContractPartyDestination),
				},
			},
			{
				name:           "one extender on both sides",
				sourceIps:      []string{"192.0.2.10"},
				destinationIps: []string{"192.0.2.10"},
				wantParties: []testContractExtenderParty{
					partyA(ContractPartySource),
					partyA(ContractPartyDestination),
				},
			},
			{
				name:           "both endpoints direct",
				sourceIps:      []string{"198.51.100.20"},
				destinationIps: []string{"198.51.100.21"},
				wantParties:    []testContractExtenderParty{},
			},
			{
				name:           "two connections through one extender",
				sourceIps:      []string{"192.0.2.10", "2001:db8:a::10"},
				destinationIps: []string{"198.51.100.21"},
				wantParties: []testContractExtenderParty{
					partyA(ContractPartySource),
				},
			},
			{
				name:           "an extender and a direct connection on one side",
				sourceIps:      []string{"192.0.2.10", "198.51.100.20"},
				destinationIps: []string{"192.0.2.11", "198.51.100.21"},
				wantParties: []testContractExtenderParty{
					partyA(ContractPartySource),
					partyB(ContractPartyDestination),
				},
			},
			{
				// zero to many per side: every active extender of an endpoint
				// counts, which is fuzzy per contract and averages right
				name:           "two extenders on one side",
				sourceIps:      []string{"192.0.2.10", "192.0.2.11"},
				destinationIps: []string{"198.51.100.21"},
				wantParties: []testContractExtenderParty{
					partyA(ContractPartySource),
					partyB(ContractPartySource),
				},
			},
			{
				name:           "one extender on both sides and another on one",
				sourceIps:      []string{"192.0.2.10"},
				destinationIps: []string{"192.0.2.10", "192.0.2.11"},
				wantParties: []testContractExtenderParty{
					partyA(ContractPartySource),
					partyA(ContractPartyDestination),
					partyB(ContractPartyDestination),
				},
			},
		} {
			sourceNetworkId, sourceId, destinationNetworkId, destinationId := testContractEndpoints(ctx)
			for _, ip := range test.sourceIps {
				testConnectFrom(t, ctx, sourceId, ip)
			}
			for _, ip := range test.destinationIps {
				testConnectFrom(t, ctx, destinationId, ip)
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
				t.Fatalf("%s: create escrow: %v", test.name, err)
			}
			assertContractExtenderParties(t, ctx, test.name, escrow.ContractId, test.wantParties)
		}
	})
}

// The no-escrow path writes the same parties. It is a separate insert from the
// escrow path, so it needs its own proof.
func TestCreateContractNoEscrowWritesTheExtenderParties(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extenderClientId := server.NewId()
		extenderNetworkId := server.NewId()
		extender := testContractExtender(
			ctx,
			"no-escrow",
			extenderClientId,
			extenderNetworkId,
			"203.0.113.10",
		)

		sourceNetworkId, sourceId, destinationNetworkId, destinationId := testContractEndpoints(ctx)
		testConnectFrom(t, ctx, sourceId, "198.51.100.30")
		testConnectFrom(t, ctx, destinationId, "203.0.113.10")

		contractId, err := CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			ByteCount(1024),
		)
		if err != nil {
			t.Fatalf("create no-escrow contract: %v", err)
		}
		assertContractExtenderParties(t, ctx, "no escrow", contractId, []testContractExtenderParty{
			{
				extenderId: extender.ExtenderId,
				party:      ContractPartyDestination,
				clientId:   extenderClientId,
				networkId:  extenderNetworkId,
			},
		})
	})
}

// A companion contract reverses the endpoints, so its own source and
// destination parties are the reverse of the origin contract's.
func TestCreateCompanionTransferEscrowWritesTheExtenderParties(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const transferByteCount = ByteCount(1024)

		extenderAClientId := server.NewId()
		extenderANetworkId := server.NewId()
		extenderA := testContractExtender(
			ctx,
			"companion-a",
			extenderAClientId,
			extenderANetworkId,
			"203.0.113.20",
		)
		extenderBClientId := server.NewId()
		extenderBNetworkId := server.NewId()
		extenderB := testContractExtender(
			ctx,
			"companion-b",
			extenderBClientId,
			extenderBNetworkId,
			"203.0.113.21",
		)

		sourceNetworkId, sourceId, destinationNetworkId, destinationId := testContractEndpoints(ctx)
		testConnectFrom(t, ctx, sourceId, "203.0.113.20")
		testConnectFrom(t, ctx, destinationId, "203.0.113.21")
		// the origin and its companion are both paid by the origin's source
		// network, since the companion's payer is its destination
		addContractPayoutTestBalance(ctx, sourceNetworkId, 2*transferByteCount)

		origin, err := CreateTransferEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			transferByteCount,
		)
		if err != nil {
			t.Fatalf("create origin escrow: %v", err)
		}
		assertContractExtenderParties(t, ctx, "origin", origin.ContractId, []testContractExtenderParty{
			{
				extenderId: extenderA.ExtenderId,
				party:      ContractPartySource,
				clientId:   extenderAClientId,
				networkId:  extenderANetworkId,
			},
			{
				extenderId: extenderB.ExtenderId,
				party:      ContractPartyDestination,
				clientId:   extenderBClientId,
				networkId:  extenderBNetworkId,
			},
		})

		// the return direction: the origin's destination is the companion's
		// source, so the parties swap with the endpoints
		companion, err := CreateCompanionTransferEscrow(
			ctx,
			destinationNetworkId,
			destinationId,
			sourceNetworkId,
			sourceId,
			transferByteCount,
			time.Hour,
		)
		if err != nil {
			t.Fatalf("create companion escrow: %v", err)
		}
		assertContractExtenderParties(t, ctx, "companion", companion.ContractId, []testContractExtenderParty{
			{
				extenderId: extenderB.ExtenderId,
				party:      ContractPartySource,
				clientId:   extenderBClientId,
				networkId:  extenderBNetworkId,
			},
			{
				extenderId: extenderA.ExtenderId,
				party:      ContractPartyDestination,
				clientId:   extenderAClientId,
				networkId:  extenderANetworkId,
			},
		})
	})
}

// The rows are deleted with the contract by the reaper's cascade, and the
// orphan sweep is the safety net for a contract that went away by any other
// path.
func TestContractExtenderRowsGoWithTheContract(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extenderClientId := server.NewId()
		extenderNetworkId := server.NewId()
		testContractExtender(
			ctx,
			"reaped",
			extenderClientId,
			extenderNetworkId,
			"203.0.113.30",
		)

		sourceNetworkId, sourceId, destinationNetworkId, destinationId := testContractEndpoints(ctx)
		testConnectFrom(t, ctx, sourceId, "203.0.113.30")

		reapedContractId, err := CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			ByteCount(1024),
		)
		if err != nil {
			t.Fatalf("create reaped contract: %v", err)
		}
		liveContractId, err := CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			ByteCount(1024),
		)
		if err != nil {
			t.Fatalf("create live contract: %v", err)
		}
		if got := len(testContractExtenderParties(t, ctx, reapedContractId)); got != 1 {
			t.Fatalf("reaped contract parties = %d, want 1", got)
		}

		// age the contract past the straggler horizon and make it due, which is
		// what the reaper's delete pass consumes
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET create_time = $2, reap_time = $3
				WHERE contract_id = $1
				`,
				reapedContractId,
				server.NowUtc().Add(-StragglerContractExpiration-24*time.Hour),
				server.NowUtc().Add(-time.Hour),
			))
		})
		RemoveCompletedContracts(ctx, server.NowUtc().Add(-24*time.Hour))

		if contractCount, _, _, _ := testingCountContractRows(ctx, reapedContractId); contractCount != 0 {
			t.Fatalf("reaped contract survived: %d rows", contractCount)
		}
		assertContractExtenderParties(t, ctx, "reaped", reapedContractId, []testContractExtenderParty{})
		if got := len(testContractExtenderParties(t, ctx, liveContractId)); got != 1 {
			t.Fatalf("live contract parties = %d, want 1", got)
		}

		// a row whose contract disappeared without the cascade
		orphanContractId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO contract_extender (
					contract_id,
					extender_id,
					party,
					client_id,
					network_id
				)
				VALUES ($1, $2, $3, $4, $5)
				`,
				orphanContractId,
				server.NewId(),
				ContractPartySource,
				extenderClientId,
				extenderNetworkId,
			))
		})

		removed, _, done := SweepOrphanContractData(ctx, SweepOrphanCursor{}, 0, 1)
		if !done || removed != 1 {
			t.Fatalf("extender orphan sweep removed/done = %d/%t, want 1/true", removed, done)
		}
		assertContractExtenderParties(t, ctx, "orphan", orphanContractId, []testContractExtenderParty{})
		if got := len(testContractExtenderParties(t, ctx, liveContractId)); got != 1 {
			t.Fatalf("live contract parties after the sweep = %d, want 1", got)
		}
	})
}

// The parties are what the operator believes NOW, not what it believed when
// the connection was tagged: a connection that has gone away and an extender
// that has since been revoked are both attribution nothing stands behind, and
// paying for either would credit a hop that is carrying nothing.
func TestContractExtenderPartiesSkipAClosedConnectionAndARevokedExtender(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		liveClientId := server.NewId()
		liveNetworkId := server.NewId()
		live := testContractExtender(ctx, "live", liveClientId, liveNetworkId, "203.0.113.40")
		testContractExtender(ctx, "closed", server.NewId(), server.NewId(), "203.0.113.41")
		revoked := testContractExtender(ctx, "revoked", server.NewId(), server.NewId(), "203.0.113.42")

		sourceNetworkId, sourceId, destinationNetworkId, destinationId := testContractEndpoints(ctx)
		testConnectFrom(t, ctx, sourceId, "203.0.113.40")
		closedConnectionId := testConnectFromWithConnectionId(t, ctx, sourceId, "203.0.113.41")
		testConnectFrom(t, ctx, sourceId, "203.0.113.42")

		// one endpoint connection goes away after it was tagged
		if err := DisconnectNetworkClient(ctx, closedConnectionId); err != nil {
			t.Fatalf("disconnect: %v", err)
		}
		// and one extender loses its last address, which revokes it
		outcome := RecordNetworkExtenderProbeResult(
			ctx,
			revoked.ExtenderId,
			4,
			false,
			server.NowUtc(),
			1,
			func(extender *NetworkExtender, issueTime time.Time) ([]byte, error) {
				return []byte("revocation"), nil
			},
		)
		if !outcome.ExtenderRevoked {
			t.Fatal("the extender was not revoked")
		}

		contractId, err := CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			ByteCount(1024),
		)
		if err != nil {
			t.Fatalf("create contract: %v", err)
		}
		assertContractExtenderParties(t, ctx, "live only", contractId, []testContractExtenderParty{
			{
				extenderId: live.ExtenderId,
				party:      ContractPartySource,
				clientId:   liveClientId,
				networkId:  liveNetworkId,
			},
		})
	})
}

// Connects clientId from ip and returns the connection, so a test can close it
// again.
func testConnectFromWithConnectionId(
	t testing.TB,
	ctx context.Context,
	clientId server.Id,
	ip string,
) server.Id {
	t.Helper()
	handlerId := CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := ConnectNetworkClient(
		ctx,
		clientId,
		net.JoinHostPort(ip, "443"),
		handlerId,
	)
	if err != nil {
		t.Fatalf("connect %s from %s: %v", clientId, ip, err)
	}
	return connectionId
}

// The orphan sweep pages contract_extender by its whole three column key, so a
// pass that runs out of budget mid-table resumes where it stopped rather than
// restarting the table or skipping the rest of it. The key is the one part of
// the step that is not shared with the tables beside it: it has three columns
// where they have two, and a cursor that did not round trip would silently
// leave rows behind forever.
func TestContractExtenderOrphanSweepResumesAcrossItsCursor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		extenderClientId := server.NewId()
		extenderNetworkId := server.NewId()
		testContractExtender(
			ctx,
			"sweep-cursor",
			extenderClientId,
			extenderNetworkId,
			"203.0.113.60",
		)
		sourceNetworkId, sourceId, destinationNetworkId, destinationId := testContractEndpoints(ctx)
		testConnectFrom(t, ctx, sourceId, "203.0.113.60")

		liveContractId, err := CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			ByteCount(1024),
		)
		if err != nil {
			t.Fatalf("create live contract: %v", err)
		}

		// several orphans, spread over the key so the order under the cursor is
		// not the insert order
		orphanContractIds := []server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			for range 3 {
				orphanContractId := server.NewId()
				orphanContractIds = append(orphanContractIds, orphanContractId)
				for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
						INSERT INTO contract_extender (
							contract_id,
							extender_id,
							party,
							client_id,
							network_id
						)
						VALUES ($1, $2, $3, $4, $5)
						`,
						orphanContractId,
						server.NewId(),
						party,
						extenderClientId,
						extenderNetworkId,
					))
				}
			}
		})

		// one row of budget per call, so every call but the last resumes from a
		// cursor the previous one produced
		removed := int64(0)
		cursor := SweepOrphanCursor{}
		callCount := 0
		for {
			callCount += 1
			if 64 < callCount {
				t.Fatal("the sweep never finished")
			}
			stepRemoved, endCursor, done := SweepOrphanContractData(ctx, cursor, 1, 1)
			removed += stepRemoved
			if done {
				break
			}
			cursor = endCursor
		}
		if removed != 6 {
			t.Fatalf("the sweep removed %d rows, want the 6 orphans", removed)
		}
		if callCount < 2 {
			t.Fatalf("the sweep finished in %d call, so no cursor was carried", callCount)
		}
		for _, orphanContractId := range orphanContractIds {
			assertContractExtenderParties(t, ctx, "orphan", orphanContractId, []testContractExtenderParty{})
		}
		if got := len(testContractExtenderParties(t, ctx, liveContractId)); got != 1 {
			t.Fatalf("the live contract's parties = %d, want 1", got)
		}
	})
}
