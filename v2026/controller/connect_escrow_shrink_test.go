// The controller must sign the escrow's granted size, which shrinks to fit the
// payer's balance, not the requested size.
package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Deterministic, database-free: the escrow grants less than the request (it
// shrank to fit the payer's balance), and newContract must return the granted
// size, which CreateContract signs into the StoredContract. Returning the
// clamped request would promise the sender capacity that is not in escrow.
func TestNewContractReturnsGrantedEscrowSize(t *testing.T) {
	sourceNetworkId := server.NewId()
	sourceId := server.NewId()
	destinationNetworkId := server.NewId()
	destinationId := server.NewId()
	grantedContractId := server.NewId()
	granted := 3 * model.Mib
	requested := 128 * model.Mib

	previousFind, previousCreate := findActiveClientPairNetworks, createTransferEscrow
	defer func() { findActiveClientPairNetworks, createTransferEscrow = previousFind, previousCreate }()
	findActiveClientPairNetworks = func(ctx context.Context, a server.Id, b server.Id) (*server.Id, *server.Id) {
		return &sourceNetworkId, &destinationNetworkId
	}
	var escrowRequest model.ByteCount
	createTransferEscrow = func(
		ctx context.Context,
		escrowSourceNetworkId server.Id,
		escrowSourceId server.Id,
		escrowDestinationNetworkId server.Id,
		escrowDestinationId server.Id,
		contractTransferByteCount model.ByteCount,
	) (*model.TransferEscrow, error) {
		escrowRequest = contractTransferByteCount
		return &model.TransferEscrow{
			ContractId:        grantedContractId,
			TransferByteCount: granted,
			Priority:          model.PaidPriority,
		}, nil
	}

	contractId, count, priority, streamId, err := newContract(
		context.Background(),
		sourceId,
		destinationId,
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
		t.Fatal(err)
	}
	if escrowRequest != min(requested, MaxContractTransferByteCount) {
		t.Fatalf("escrow requested %d, want %d", escrowRequest, min(requested, MaxContractTransferByteCount))
	}
	if contractId != grantedContractId || priority != model.PaidPriority || streamId != nil {
		t.Fatalf("contract = %s priority %d stream %v", contractId, priority, streamId)
	}
	if count != granted {
		t.Fatalf("controller would sign %d bytes, want granted escrow %d", count, granted)
	}
}
