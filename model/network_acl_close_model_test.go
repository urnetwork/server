//go:build sidecar_pending

package model

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// §8.6's one switch at the contract layer: a sidecar's bound client and the window client acting as it lose their open
// contracts' forwarding, each party gets close requests, a bystander's contract stands, and a retry changes nothing.
func TestCloseAclSubjectContracts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		sidecarId := server.NewId()
		sidecarClient, windowClient, counterparty, bystander := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, clientId := range []server.Id{sidecarClient, windowClient, counterparty, bystander} {
			Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET source_client_id = $2, acl_subject_client_id = $2 WHERE client_id = $1`,
				windowClient, sidecarClient))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO sidecar_identity (sidecar_id, network_id, identity_public_key, identity_alg, status, create_time)
				VALUES ($1, $2, $3, 'ed25519', 'active', $4)
			`, sidecarId, networkId, make([]byte, 32), server.NowUtc()))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
				VALUES ($1, 1, $2, $3, $4, $5)
			`, sidecarId, sidecarClient, make([]byte, 32), []byte{1}, server.NowUtc()))
		})
		contract := func(sourceId server.Id, destinationId server.Id) server.Id {
			contractId, err := CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 1024)
			if err != nil {
				t.Fatalf("CreateContractNoEscrow: %v", err)
			}
			if !HasOpenContractHole(ctx, sourceId, destinationId) {
				t.Fatal("a new contract with no contract-hole member")
			}
			return contractId
		}
		outbound := contract(sidecarClient, counterparty)
		inbound := contract(counterparty, windowClient)
		standing := contract(counterparty, bystander)

		closed, err := CloseAclSubjectContracts(ctx, networkId, sidecarId)
		if err != nil {
			t.Fatal(err)
		}
		wantClients := []server.Id{sidecarClient, windowClient}
		slices.SortFunc(wantClients, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		wantContracts := []server.Id{outbound, inbound}
		slices.SortFunc(wantContracts, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		if !slices.Equal(closed.ClientIds, wantClients) || !slices.Equal(closed.ContractIds, wantContracts) {
			t.Fatalf("closed %+v", closed)
		}
		if HasOpenContractHole(ctx, sidecarClient, counterparty) || HasOpenContractHole(ctx, counterparty, windowClient) {
			t.Error("a closed contract still forwards")
		}
		if !HasOpenContractHole(ctx, counterparty, bystander) {
			t.Error("a bystander's contract stopped")
		}
		if slices.Contains(closed.ContractIds, standing) {
			t.Error("a bystander's contract was closed")
		}

		// a retry before anyone takes the requests leaves the same requests and the same members
		if _, err := CloseAclSubjectContracts(ctx, networkId, sidecarId); err != nil {
			t.Fatal(err)
		}
		requests := TakeClientCloseRequests(ctx, counterparty, 16)
		slices.SortFunc(requests.ContractIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		slices.SortFunc(requests.PeerIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		if !slices.Equal(requests.ContractIds, wantContracts) || !slices.Equal(requests.PeerIds, wantClients) {
			t.Errorf("the counterparty's close requests %+v", requests)
		}
		if subject := TakeClientCloseRequests(ctx, windowClient, 16); !slices.Equal(subject.ContractIds, []server.Id{inbound}) {
			t.Errorf("the window client's close requests %+v", subject)
		}
		if !HasOpenContractHole(ctx, counterparty, bystander) {
			t.Error("a retry stopped a bystander's contract")
		}
	})
}
