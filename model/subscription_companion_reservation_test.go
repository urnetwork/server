// Probe-owned return contracts must respect the owner's reservation size even
// when an ordinary provider requests its default, much larger successor.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// Creates funded, synthetic endpoints and optionally marks the payer as the
// internal prober. The persisted prober client deliberately differs from the
// active tunnel client, as it does for derived clients and credential rotation.
func companionReservationFixture(ctx context.Context, prober bool) (
	payerNetworkId server.Id,
	payerId server.Id,
	providerNetworkId server.Id,
	providerId server.Id,
) {
	payerNetworkId, payerId = server.NewId(), server.NewId()
	providerNetworkId, providerId = server.NewId(), server.NewId()
	testingCreatePaymentClient(ctx, payerNetworkId, payerId)
	testingCreatePaymentClient(ctx, providerNetworkId, providerId)
	for _, networkId := range []server.Id{payerNetworkId, providerNetworkId} {
		server.Raise(AddBasicTransferBalance(ctx, networkId, 1024*1024*1024, server.NowUtc().Add(-time.Hour), server.NowUtc().Add(time.Hour)))
	}
	if prober {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO prober_identity (singleton, network_id, client_id)
				VALUES (true, $1, $2)
				ON CONFLICT (singleton) DO UPDATE SET
					network_id = excluded.network_id,
					client_id = excluded.client_id
			`, payerNetworkId, server.NewId()))
		})
	}
	return
}

// Asserts both the persisted promise and its real escrow, rather than trusting
// an in-memory result that could disagree with the signed contract.
func assertCompanionReservation(t testing.TB, ctx context.Context, escrow *TransferEscrow, want ByteCount) {
	t.Helper()
	if escrow == nil {
		t.Fatal("companion returned no escrow")
	}
	if escrow.TransferByteCount != want {
		t.Errorf("returned reservation = %d, want %d", escrow.TransferByteCount, want)
	}
	server.Db(ctx, func(conn server.PgConn) {
		var promise, reserved ByteCount
		server.Raise(conn.QueryRow(ctx, `
			SELECT transfer_byte_count,
				(SELECT COALESCE(sum(balance_byte_count), 0) FROM transfer_escrow WHERE contract_id = $1)
			FROM transfer_contract WHERE contract_id = $1
		`, escrow.ContractId).Scan(&promise, &reserved))
		if promise != want || reserved != want {
			t.Errorf("durable promise/reservation = %d/%d, want %d/%d", promise, reserved, want, want)
		}
	})
}

// Reproduces the remote provider's 1/33/128 MiB contract ramp against a short
// probe that keeps every own-direction contract at 1 MiB.
func TestProberCompanionReservationUsesOriginBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		payerNetworkId, payerId, providerNetworkId, providerId := companionReservationFixture(ctx, true)
		const opening ByteCount = 1024 * 1024
		origin, err := CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, opening)
		if err != nil {
			t.Fatal(err)
		}
		before := GetActiveTransferBalanceByteCount(ctx, payerNetworkId)
		for _, request := range []ByteCount{opening, opening + (128*opening-opening)/4, 128 * opening} {
			companion, err := CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, request, time.Hour)
			if err != nil {
				t.Fatalf("request %d: %v", request, err)
			}
			assertCompanionReservation(t, ctx, companion, opening)
			if companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
				t.Fatal("reservation changed the companion's origin")
			}
		}
		if consumed := before - GetActiveTransferBalanceByteCount(ctx, payerNetworkId); consumed != 3*opening {
			t.Errorf("three return contracts reserved %d bytes from the payer, want %d", consumed, 3*opening)
		}
	})
}

// A probe's real larger origin still permits a larger return contract. An
// ordinary account may reserve an asymmetric download larger than its upload.
func TestCompanionReservationPreservesFullProbeAndOrdinaryAccounts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const opening ByteCount = 1024 * 1024
		for _, test := range []struct {
			name   string
			prober bool
			origin ByteCount
		}{
			{name: "ordinary without prober", origin: opening},
			{name: "full prober", prober: true, origin: 128 * opening},
			{name: "ordinary with another prober", origin: opening},
		} {
			payerNetworkId, payerId, providerNetworkId, providerId := companionReservationFixture(ctx, test.prober)
			if _, err := CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, test.origin); err != nil {
				t.Fatalf("%s origin: %v", test.name, err)
			}
			companion, err := CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 128*opening, time.Hour)
			if err != nil {
				t.Fatalf("%s return: %v", test.name, err)
			}
			assertCompanionReservation(t, ctx, companion, 128*opening)
		}
	})
}

// Growing a full probe's own ramp must raise its return reservation without
// changing the earliest stream anchor or borrowing a different pair's budget.
func TestProberCompanionReservationFollowsOriginRamp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		payerNetworkId, payerId, providerNetworkId, providerId := companionReservationFixture(ctx, true)
		const opening ByteCount = 1024 * 1024
		origin, err := CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, opening)
		if err != nil {
			t.Fatal(err)
		}
		// A large reservation for another destination cannot raise this pair.
		otherProviderId := server.NewId()
		Testing_CreateDevice(ctx, providerNetworkId, server.NewId(), otherProviderId, "synthetic-other-provider", "synthetic")
		if _, err := CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, otherProviderId, 128*opening); err != nil {
			t.Fatal(err)
		}
		companion, err := CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 128*opening, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, companion, opening)
		grownOrigin, err := CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, 128*opening)
		if err != nil {
			t.Fatal(err)
		}
		companion, err = CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 128*opening, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, companion, 128*opening)
		if companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatal("larger return reservation changed the earliest origin anchor")
		}
		// The same linger window admits closed anchors; closing both origins
		// must not erase the larger reservation while late replies can arrive.
		for _, origin := range []*TransferEscrow{origin, grownOrigin} {
			CloseContract(ctx, origin.ContractId, payerId, 0, false)
			CloseContract(ctx, origin.ContractId, providerId, 0, false)
		}
		companion, err = CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 128*opening, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		assertCompanionReservation(t, ctx, companion, 128*opening)
	})
}

// Full and blackhole tunnels can share a parent and provider simultaneously.
// The generator authenticates with SourceClientId, not ClientId, so the real
// auth path must mint distinct transfer identities whose reservations remain
// isolated even though the payer network is shared.
func TestProberCompanionReservationSeparatesDerivedProbeTunnels(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		payerNetworkId, parentId, providerNetworkId, providerId := companionReservationFixture(ctx, true)
		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: payerNetworkId,
			UserId:    server.NewId(),
			ClientId:  &parentId,
		})
		defer clientSession.Cancel()
		clients := []server.Id{}
		for range 2 {
			result, err := AuthNetworkClient(&AuthNetworkClientArgs{
				SourceClientId: &parentId,
				Description:    "synthetic probe tunnel",
				DeviceSpec:     "synthetic probe",
			}, clientSession)
			if err != nil || result == nil || result.Error != nil || result.ClientId == nil {
				t.Fatalf("derived client auth failed: %v", err)
			}
			clients = append(clients, *result.ClientId)
		}
		if clients[0] == clients[1] || clients[0] == parentId || clients[1] == parentId {
			t.Fatal("independent probe authentications reused a transfer client identity")
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, clientId := range clients {
				var sourceId server.Id
				server.Raise(conn.QueryRow(ctx, `SELECT source_client_id FROM network_client WHERE client_id = $1`, clientId).Scan(&sourceId))
				if sourceId != parentId {
					t.Fatal("probe client did not derive from the shared parent")
				}
			}
		})
		const opening ByteCount = 1024 * 1024
		for index, originCount := range []ByteCount{opening, 128 * opening} {
			if _, err := CreateTransferEscrow(ctx, payerNetworkId, clients[index], providerNetworkId, providerId, originCount); err != nil {
				t.Fatal(err)
			}
		}
		for index, want := range []ByteCount{opening, 128 * opening} {
			companion, err := CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, clients[index], 128*opening, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			assertCompanionReservation(t, ctx, companion, want)
		}
	})
}

// The bound must follow both origin lookup branches: recently closed plain
// origins and a companion origin used by the encrypted reply carrier.
func TestProberCompanionReservationClosedAndChainedOrigins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const opening ByteCount = 1024 * 1024
		for _, chained := range []bool{false, true} {
			payerNetworkId, payerId, providerNetworkId, providerId := companionReservationFixture(ctx, true)
			var origin *TransferEscrow
			var err error
			if chained {
				if _, err = CreateTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, opening); err != nil {
					t.Fatal(err)
				}
				origin, err = CreateCompanionTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, opening, time.Hour)
			} else {
				origin, err = CreateTransferEscrow(ctx, payerNetworkId, payerId, providerNetworkId, providerId, opening)
			}
			if err != nil {
				t.Fatal(err)
			}
			CloseContract(ctx, origin.ContractId, payerId, 0, false)
			CloseContract(ctx, origin.ContractId, providerId, 0, false)
			companion, err := CreateCompanionTransferEscrow(ctx, providerNetworkId, providerId, payerNetworkId, payerId, 128*opening, time.Hour)
			if err != nil {
				t.Fatalf("chained=%t: %v", chained, err)
			}
			assertCompanionReservation(t, ctx, companion, opening)
			if companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
				t.Fatalf("chained=%t: matched the wrong origin", chained)
			}
		}
	})
}
