//go:build sidecar_pending

package controller

// QUOTA-M2-08 at the controller (DESIGN.md 20.6): a Network contract between two marked sidecar
// clients of one network reserves from that network and signs the grant, a companion normalized to
// Network included, while an unmarked pair beside it is untouched; a spent network answers
// InsufficientBalance; and a warm network's relayed contract takes no more Postgres connections than
// an unmarked one. Under -tags sidecar_pending, which links the marks' MG0 and the journal's MG2.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Four clients of one network, the first two marked, and a balance for the network.
func relayedTestNetwork(t testing.TB, ctx context.Context, balance model.ByteCount) (networkId server.Id, clientIds []server.Id) {
	t.Helper()
	networkId, sourceId, destinationId := contractPairTestClients(ctx)
	clientIds = []server.Id{sourceId, destinationId}
	for range 2 {
		clientId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		clientIds = append(clientIds, clientId)
	}
	if 0 < balance {
		now := server.NowUtc()
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             now.Add(-time.Hour),
			EndTime:               now.Add(24 * time.Hour),
			StartBalanceByteCount: balance,
			BalanceByteCount:      balance,
		})
	}
	return
}

func relayedContractPayer(ctx context.Context, contractId server.Id) (payerNetworkId *server.Id, transferByteCount model.ByteCount) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id, transfer_byte_count FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&payerNetworkId, &transferByteCount))
	})
	return
}

func relayedPairContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, contractRelayedPairKey{}, true)
}

func TestRelayedPairReservesAndAnUnmarkedPairDoesNot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer model.Testing_SetRelayedReservationSettings(model.DefaultRelayedReservationSettings())()
		networkId, clientIds := relayedTestNetwork(t, ctx, 1*model.Gib)
		settings := connect.DefaultContractManagerSettings()
		want := min(MaxContractTransferByteCount, max(MinContractTransferByteCount, 1*model.Gib/8))

		// marked: reserved, the grant signed and the payer on the row
		contractId, signed, _, _, _, err := newContract(relayedPairContext(ctx), clientIds[0], clientIds[1], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, signed, want)
		payer, stored := relayedContractPayer(ctx, contractId)
		connect.AssertEqual(t, payer != nil && *payer == networkId, true)
		connect.AssertEqual(t, stored, want)

		// a companion normalized to Network takes the same path, its usage origin the destination
		contractId, signed, _, _, _, err = newContract(relayedPairContext(ctx), clientIds[1], clientIds[0], nil, false, false,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, settings)
		if err != nil {
			t.Fatal(err)
		}
		payer, _ = relayedContractPayer(ctx, contractId)
		connect.AssertEqual(t, payer != nil && *payer == networkId, true)
		connect.AssertEqual(t, signed <= want, true)

		// an unmarked pair of the same network: today's Network contract, unbilled
		contractId, signed, _, _, _, err = newContract(ctx, clientIds[2], clientIds[3], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, settings)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, signed, MaxContractTransferByteCount)
		payer, _ = relayedContractPayer(ctx, contractId)
		connect.AssertEqual(t, payer == nil, true)

		// friends and family is never relayed
		contractId, _, _, _, _, err = newContract(relayedPairContext(ctx), clientIds[0], clientIds[1], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeFriendsAndFamily, false, 0, settings)
		if err != nil {
			t.Fatal(err)
		}
		payer, _ = relayedContractPayer(ctx, contractId)
		connect.AssertEqual(t, payer == nil, true)
	})
}

// A marked pair on a network with nothing available is InsufficientBalance, never a route
// rejection's cause.
func TestRelayedSpentNetworkIsInsufficientBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer model.Testing_SetRelayedReservationSettings(model.DefaultRelayedReservationSettings())()
		_, clientIds := relayedTestNetwork(t, ctx, 0)
		_, _, _, _, _, err := newContract(relayedPairContext(ctx), clientIds[0], clientIds[1], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, connect.DefaultContractManagerSettings())
		if !errors.Is(err, model.ErrContractBalanceExhausted) {
			t.Fatalf("%v, want an exhausted balance", err)
		}
		connect.AssertEqual(t, contractResultError(err), protocol.ContractError_InsufficientBalance)
		connect.AssertEqual(t, contractRejectionFailureClass(err), "insufficient_balance")
	})
}

// On a warm network a relayed contract takes as many Postgres connections as an unmarked Network
// contract: the reservation adds no round trip (20.6's cost row).
func TestRelayedWarmContractAddsNoPostgresRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer model.Testing_SetRelayedReservationSettings(&model.RelayedReservationSettings{
			Enabled:                       true,
			DrawOrderCacheTtl:             time.Hour,
			DrawOrderCacheMaxNetworkCount: 8192,
		})()
		_, clientIds := relayedTestNetwork(t, ctx, 1*model.Gib)
		settings := connect.DefaultContractManagerSettings()
		// warm the network's balances
		if _, _, _, _, _, err := newContract(relayedPairContext(ctx), clientIds[0], clientIds[1], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, settings); err != nil {
			t.Fatal(err)
		}
		loads := model.Testing_RelayedDrawOrderCacheLoads()

		before := contractPairAcquireCount(t)
		if _, _, _, _, _, err := newContract(ctx, clientIds[2], clientIds[3], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, settings); err != nil {
			t.Fatal(err)
		}
		unmarked := contractPairAcquireCount(t) - before

		before = contractPairAcquireCount(t)
		if _, _, _, _, _, err := newContract(relayedPairContext(ctx), clientIds[0], clientIds[1], nil, false, true,
			MaxContractTransferByteCount, model.ProvideModeNetwork, false, 0, settings); err != nil {
			t.Fatal(err)
		}
		relayed := contractPairAcquireCount(t) - before
		connect.AssertEqual(t, model.Testing_RelayedDrawOrderCacheLoads(), loads)
		if relayed > unmarked {
			t.Fatalf("a warm relayed contract took %.0f connections, an unmarked one %.0f", relayed, unmarked)
		}
	})
}
