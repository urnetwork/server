package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

func testGetNetworkClientContractTime(ctx context.Context, clientId server.Id) (contractTime *time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT contract_time FROM network_client WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&contractTime))
			}
		})
	})
	return
}

func testSetNetworkClientContractTime(ctx context.Context, clientId server.Id, contractTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_client SET contract_time = $2 WHERE client_id = $1`,
			clientId,
			contractTime,
		))
	})
}

func testGetNetworkClientAuthTime(ctx context.Context, clientId server.Id) (authTime time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT auth_time FROM network_client WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&authTime))
			}
		})
	})
	return
}

func testSetNetworkClientAuthTime(ctx context.Context, clientId server.Id, authTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
			clientId,
			authTime,
		))
	})
}

// a top-level client with a child (the shape of a hosted proxy: the proxy
// client id plus its per-stream egress clients)
func testCreateParentChildClients(ctx context.Context, t testing.TB) (networkId server.Id, parentId server.Id, childId server.Id) {
	networkId = server.NewId()
	adminUserId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "statstest", adminUserId)
	userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    adminUserId,
	})

	parentResult, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			Description: "top level device",
			DeviceSpec:  "test",
		},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parentResult.Error, nil)
	parentId = *parentResult.ClientId

	childResult, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			Description:    "window client",
			DeviceSpec:     "test",
			SourceClientId: &parentId,
		},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, childResult.Error, nil)
	childId = *childResult.ClientId

	return
}

// the block users stat counts top-level identities by contract-creating
// usage: a contract created by a child client counts its top-level parent
// in the current block — the hosted proxy case, where the proxy client id
// itself never connects and all usage happens via child clients — and each
// contract creation flavor (no-escrow, escrow, companion) re-stamps the
// marker so an identity in continuous use is counted in every block it
// creates contracts in, not just the block it was created in.
func TestBlockUsersContractUsage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		Testing_ResetContractTimeStampGate()

		networkId, parentId, childId := testCreateParentChildClients(ctx, t)

		destNetworkId := server.NewId()
		destId := server.NewId()

		// no contracts yet: no users, no matter how recently clients authed
		blockStart := server.NowUtc()
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart), int64(0))

		// a no-escrow contract created by the child counts the parent
		_, err := CreateContractNoEscrow(ctx, networkId, childId, destNetworkId, destId, ByteCount(1024))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart), int64(1))
		// the child row itself is never stamped (it must not double count)
		connect.AssertEqual(t, testGetNetworkClientContractTime(ctx, childId) == nil, true)
		connect.AssertEqual(t, testGetNetworkClientContractTime(ctx, parentId) != nil, true)

		// escrow path: simulate a later block by backdating the marker, then
		// verify the escrow wrapper re-stamps it
		staleTime := server.NowUtc().Add(-2 * time.Hour)
		testSetNetworkClientContractTime(ctx, parentId, staleTime)
		Testing_ResetContractTimeStampGate()

		balanceCode, err := CreateBalanceCode(
			ctx,
			ByteCount(1024*1024*1024*1024),
			365*24*time.Hour,
			UsdToNanoCents(10.00),
			server.NewId().String(),
			"",
			"",
		)
		connect.AssertEqual(t, err, nil)
		_, err = RedeemBalanceCode(
			&RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: networkId,
			},
			ctx,
		)
		connect.AssertEqual(t, err, nil)

		blockStart2 := server.NowUtc()
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart2), int64(0))
		_, err = CreateTransferEscrow(ctx, networkId, childId, destNetworkId, destId, ByteCount(1024*1024))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart2), int64(1))

		// companion path: the destination is the paying side, so the reply
		// contract counts the destination child's parent
		testSetNetworkClientContractTime(ctx, parentId, staleTime)
		Testing_ResetContractTimeStampGate()
		blockStart3 := server.NowUtc()
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart3), int64(0))
		_, err = CreateCompanionTransferEscrow(ctx, destNetworkId, destId, networkId, childId, ByteCount(1024*1024), 5*time.Minute)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart3), int64(1))

		// a top-level client's own contract counts itself
		testSetNetworkClientContractTime(ctx, parentId, staleTime)
		Testing_ResetContractTimeStampGate()
		blockStart4 := server.NowUtc()
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart4), int64(0))
		_, err = CreateContractNoEscrow(ctx, networkId, parentId, destNetworkId, destId, ByteCount(1024))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, blockStart4), int64(1))

		// within the throttle interval the marker does not move (db-side
		// throttle, independent of the process-local gate)
		stampedTime := testGetNetworkClientContractTime(ctx, parentId)
		Testing_ResetContractTimeStampGate()
		_, err = CreateContractNoEscrow(ctx, networkId, childId, destNetworkId, destId, ByteCount(1024))
		connect.AssertEqual(t, err, nil)
		unmovedTime := testGetNetworkClientContractTime(ctx, parentId)
		connect.AssertEqual(t, unmovedTime.Equal(*stampedTime), true)

		// a window opening after the last stamp has no users
		connect.AssertEqual(t, CountTopLevelClientsWithContractSince(ctx, server.NowUtc().Add(time.Minute)), int64(0))
	})
}

// a child client's connect keeps the top-level identity's auth_time fresh,
// so an in-use hosted proxy — whose top-level client id never connects on
// its own — is not deactivated by the top-level idle reap.
func TestConnectNetworkClientBumpsParentAuthTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, parentId, childId := testCreateParentChildClients(ctx, t)

		staleTime := server.NowUtc().Add(-2 * time.Hour)
		testSetNetworkClientAuthTime(ctx, parentId, staleTime)
		testSetNetworkClientAuthTime(ctx, childId, staleTime)

		_, _, _, _, err := ConnectNetworkClient(ctx, childId, "1.2.3.4:5678", server.NewId())
		connect.AssertEqual(t, err, nil)

		parentAuthTime := testGetNetworkClientAuthTime(ctx, parentId)
		connect.AssertEqual(t, staleTime.Add(time.Hour).Before(parentAuthTime), true)

		// within the throttle interval a child connect leaves the parent alone
		recentTime := server.NowUtc().Add(-30 * time.Minute)
		testSetNetworkClientAuthTime(ctx, parentId, recentTime)
		_, _, _, _, err = ConnectNetworkClient(ctx, childId, "1.2.3.4:5679", server.NewId())
		connect.AssertEqual(t, err, nil)
		parentAuthTime = testGetNetworkClientAuthTime(ctx, parentId)
		connect.AssertEqual(t, parentAuthTime.Sub(recentTime).Abs() < time.Millisecond, true)

		// a top-level connect has no parent to bump and must not error
		_, _, _, _, err = ConnectNetworkClient(ctx, parentId, "1.2.3.4:5680", server.NewId())
		connect.AssertEqual(t, err, nil)
	})
}
