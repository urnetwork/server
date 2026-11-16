// Every payer, free or Pro, is granted contracts up to the one ceiling,
// MaxContractTransferByteCount.
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A free and a Pro payer are granted the same contract for the same request:
// the floor below it, the request itself up to the ceiling, and the ceiling
// above it, also when the ceiling is lowered. newContract asks the payer's
// escrow for that size and signs what the escrow returns. The Pro payer holds a
// real Pro balance, so sizing contracts by plan would show here.
func TestNewContractGrantsFreeAndProPayersOneCeiling(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()

		freeNetworkId := server.NewId()
		proNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, freeNetworkId, "freepayer", server.NewId())
		model.Testing_CreateNetwork(ctx, proNetworkId, "propayer", server.NewId())
		server.Tx(ctx, func(tx server.PgTx) {
			err := model.AddProTransferBalanceInTx(tx, ctx, proNetworkId, 1024*model.Gib, now, now.Add(30*24*time.Hour))
			connect.AssertEqual(t, err, nil)
		})
		connect.AssertEqual(t, model.UpdateProNetwork(ctx, proNetworkId), true)
		connect.AssertEqual(t, model.IsProNetwork(ctx, freeNetworkId), false)

		// the byte count newContract asks the payer's escrow for and signs, for
		// a public contract the payer's network sends
		grant := func(payerNetworkId server.Id, requested model.ByteCount) model.ByteCount {
			destinationNetworkId := server.NewId()
			grantedContractId := server.NewId()

			previousFind, previousCreate := findActiveClientPairNetworks, createTransferEscrow
			defer func() { findActiveClientPairNetworks, createTransferEscrow = previousFind, previousCreate }()
			findActiveClientPairNetworks = func(ctx context.Context, a server.Id, b server.Id) (*server.Id, *server.Id) {
				return &payerNetworkId, &destinationNetworkId
			}
			var escrowNetworkId server.Id
			var escrowRequest model.ByteCount
			createTransferEscrow = func(
				ctx context.Context,
				escrowSourceNetworkId server.Id,
				escrowSourceId server.Id,
				escrowDestinationNetworkId server.Id,
				escrowDestinationId server.Id,
				contractTransferByteCount model.ByteCount,
			) (*model.TransferEscrow, error) {
				escrowNetworkId = escrowSourceNetworkId
				escrowRequest = contractTransferByteCount
				return &model.TransferEscrow{
					ContractId:        grantedContractId,
					TransferByteCount: contractTransferByteCount,
					Priority:          model.UnpaidPriority,
				}, nil
			}

			contractId, count, _, _, _, err := newContract(
				ctx,
				server.NewId(),
				server.NewId(),
				nil,
				false,
				true,
				requested,
				model.ProvideModePublic,
				false,
				0,
				connect.DefaultContractManagerSettings(),
			)
			if err != nil {
				t.Fatalf("payer %s, %d byte request: %s", payerNetworkId, requested, err)
			}
			if escrowNetworkId != payerNetworkId {
				t.Fatalf("escrow charged %s, want the payer %s", escrowNetworkId, payerNetworkId)
			}
			if contractId != grantedContractId || count != escrowRequest {
				t.Fatalf("contract %s signs %d bytes, want %s at the %d byte escrow", contractId, count, grantedContractId, escrowRequest)
			}
			return count
		}

		check := func(name string, requested model.ByteCount, want model.ByteCount) {
			free := grant(freeNetworkId, requested)
			pro := grant(proNetworkId, requested)
			if free != want || pro != want {
				t.Errorf("%s: a %d byte request grants free %d and Pro %d, want %d for both", name, requested, free, pro, want)
			}
		}

		ceiling := MaxContractTransferByteCount
		check("below the floor", 1, MinContractTransferByteCount)
		check("under the ceiling", 64*model.Mib, 64*model.Mib)
		check("over the ceiling", 4*model.Gib, ceiling)

		func() {
			defer Testing_SetMaxContractTransferByteCount(4 * model.Mib)()
			check("over a lowered ceiling", 128*model.Mib, 4*model.Mib)
		}()
		// a ceiling set below the floor still grants a contract that fits a message
		func() {
			defer Testing_SetMaxContractTransferByteCount(1)()
			check("ceiling below the floor", 128*model.Mib, MinContractTransferByteCount)
		}()
		connect.AssertEqual(t, MaxContractTransferByteCount, ceiling)
	})
}
