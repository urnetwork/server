// While the contract degradation valve makes contracts zero cost, newContract
// must route public and companion contracts to the zero escrow creators and
// never to an escrow creator: escrow admission holds a PostgreSQL transaction
// across Redis reservation, which starves the connection pool under a hot
// payer. Database free.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Replaces the four creators: an escrow creator fails the test, and a zero
// escrow creator records the size it was asked for. Restore with the result.
func withZeroEscrowRoutingCreators(
	t testing.TB,
	sourceNetworkId server.Id,
	destinationNetworkId server.Id,
	publicEscrow *model.TransferEscrow,
	companionEscrow *model.TransferEscrow,
	publicRequests *[]model.ByteCount,
	companionRequests *[]model.ByteCount,
) func() {
	previousFind := findActiveClientPairNetworks
	previousPublic, previousCompanion := createTransferEscrow, createCompanionTransferEscrow
	previousZeroPublic, previousZeroCompanion := createZeroEscrowContract, createZeroEscrowCompanionContract
	findActiveClientPairNetworks = func(ctx context.Context, a server.Id, b server.Id) (*server.Id, *server.Id) {
		return &sourceNetworkId, &destinationNetworkId
	}
	createTransferEscrow = func(context.Context, server.Id, server.Id, server.Id, server.Id, model.ByteCount) (*model.TransferEscrow, error) {
		t.Error("the valve is open but newContract ran public escrow admission")
		return nil, errors.New("synthetic escrow refusal")
	}
	createCompanionTransferEscrow = func(context.Context, server.Id, server.Id, server.Id, server.Id, model.ByteCount, time.Duration) (*model.TransferEscrow, error) {
		t.Error("the valve is open but newContract ran companion escrow admission")
		return nil, errors.New("synthetic escrow refusal")
	}
	createZeroEscrowContract = func(ctx context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount) (*model.TransferEscrow, error) {
		*publicRequests = append(*publicRequests, contractTransferByteCount)
		return publicEscrow, nil
	}
	createZeroEscrowCompanionContract = func(ctx context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount, _ time.Duration) (*model.TransferEscrow, error) {
		*companionRequests = append(*companionRequests, contractTransferByteCount)
		return companionEscrow, nil
	}
	return func() {
		findActiveClientPairNetworks = previousFind
		createTransferEscrow, createCompanionTransferEscrow = previousPublic, previousCompanion
		createZeroEscrowContract, createZeroEscrowCompanionContract = previousZeroPublic, previousZeroCompanion
	}
}

// A free payer's 128 MiB request is sized at the 64 MiB free cap, as escrow
// sizes it, and the controller signs the zero escrow result: its size, its
// derived priority (paid here, never the trusted network priority) and its
// deadline. No escrow creator runs for either kind of contract.
func TestNewContractZeroEscrowNeverRunsEscrowAdmission(t *testing.T) {
	previousZeroContractCost := zeroContractCost
	defer func() { zeroContractCost = previousZeroContractCost }()
	zeroContractCost = func(context.Context) bool { return true }
	defer model.Testing_SetMaxContractTransferByteCount(64*model.Mib, 0)()
	lookup := &proLookup{}
	defer withProLookup(lookup)()

	sourceNetworkId := server.NewId()
	sourceId := server.NewId()
	destinationNetworkId := server.NewId()
	destinationId := server.NewId()
	originContractId := server.NewId()
	expirationTime := time.UnixMilli(2_000_000_000_000).UTC()
	capped := 64 * model.Mib
	publicEscrow := &model.TransferEscrow{
		ContractId:        server.NewId(),
		ExpirationTime:    expirationTime,
		TransferByteCount: capped,
		Priority:          model.PaidPriority,
		Balances:          []*model.TransferEscrowBalance{},
	}
	companionEscrow := &model.TransferEscrow{
		ContractId:          server.NewId(),
		CompanionContractId: &originContractId,
		ExpirationTime:      expirationTime,
		TransferByteCount:   capped,
		Priority:            model.UnpaidPriority,
		Balances:            []*model.TransferEscrowBalance{},
	}
	var publicRequests []model.ByteCount
	var companionRequests []model.ByteCount
	defer withZeroEscrowRoutingCreators(t, sourceNetworkId, destinationNetworkId, publicEscrow, companionEscrow, &publicRequests, &companionRequests)()

	requested := 128 * model.Mib
	contractId, count, priority, streamId, returnedExpirationTime, err := newContract(
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
	if len(publicRequests) != 1 || publicRequests[0] != capped {
		t.Fatalf("public zero escrow requests %v, want one at the free cap %d", publicRequests, capped)
	}
	if contractId != publicEscrow.ContractId || count != capped || priority != model.PaidPriority || streamId != nil ||
		!returnedExpirationTime.Equal(expirationTime) {
		t.Fatalf("public contract %s signs %d bytes at priority %d, want the zero escrow result", contractId, count, priority)
	}
	if len(lookup.lookupNetworkIds) != 1 || lookup.lookupNetworkIds[0] != sourceNetworkId {
		t.Fatalf("plan lookups %v, want the would-be payer, the source network", lookup.lookupNetworkIds)
	}

	contractId, count, priority, streamId, _, err = newContract(
		context.Background(),
		sourceId,
		destinationId,
		nil,
		true,
		false,
		requested,
		model.ProvideModeStream,
		false,
		0,
		connect.DefaultContractManagerSettings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(companionRequests) != 1 || companionRequests[0] != capped {
		t.Fatalf("companion zero escrow requests %v, want one at the free cap %d", companionRequests, capped)
	}
	if contractId != companionEscrow.ContractId || count != capped || priority != model.UnpaidPriority || streamId != nil {
		t.Fatalf("companion contract %s signs %d bytes at priority %d, want the zero escrow result", contractId, count, priority)
	}
	if len(lookup.lookupNetworkIds) != 2 || lookup.lookupNetworkIds[1] != destinationNetworkId {
		t.Fatalf("plan lookups %v, want the companion's would-be payer, the destination network", lookup.lookupNetworkIds)
	}
}

// The control: while the valve is closed the same calls take the escrow creators
// and never a zero escrow creator.
func TestNewContractZeroEscrowOffRunsEscrowAdmission(t *testing.T) {
	previousZeroContractCost := zeroContractCost
	defer func() { zeroContractCost = previousZeroContractCost }()
	zeroContractCost = func(context.Context) bool { return false }

	sourceNetworkId := server.NewId()
	destinationNetworkId := server.NewId()
	previousFind := findActiveClientPairNetworks
	previousPublic, previousCompanion := createTransferEscrow, createCompanionTransferEscrow
	previousZeroPublic, previousZeroCompanion := createZeroEscrowContract, createZeroEscrowCompanionContract
	defer func() {
		findActiveClientPairNetworks = previousFind
		createTransferEscrow, createCompanionTransferEscrow = previousPublic, previousCompanion
		createZeroEscrowContract, createZeroEscrowCompanionContract = previousZeroPublic, previousZeroCompanion
	}()
	findActiveClientPairNetworks = func(ctx context.Context, a server.Id, b server.Id) (*server.Id, *server.Id) {
		return &sourceNetworkId, &destinationNetworkId
	}
	originContractId := server.NewId()
	escrowCount := 0
	createTransferEscrow = func(_ context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount) (*model.TransferEscrow, error) {
		escrowCount += 1
		return &model.TransferEscrow{ContractId: server.NewId(), TransferByteCount: contractTransferByteCount, Priority: model.PaidPriority}, nil
	}
	createCompanionTransferEscrow = func(_ context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount, _ time.Duration) (*model.TransferEscrow, error) {
		escrowCount += 1
		return &model.TransferEscrow{ContractId: server.NewId(), CompanionContractId: &originContractId, TransferByteCount: contractTransferByteCount, Priority: model.PaidPriority}, nil
	}
	createZeroEscrowContract = func(context.Context, server.Id, server.Id, server.Id, server.Id, model.ByteCount) (*model.TransferEscrow, error) {
		t.Error("the valve is closed but newContract skipped public escrow")
		return nil, errors.New("synthetic zero escrow refusal")
	}
	createZeroEscrowCompanionContract = func(context.Context, server.Id, server.Id, server.Id, server.Id, model.ByteCount, time.Duration) (*model.TransferEscrow, error) {
		t.Error("the valve is closed but newContract skipped companion escrow")
		return nil, errors.New("synthetic zero escrow refusal")
	}

	for _, companion := range []bool{false, true} {
		provideMode := model.ProvideModePublic
		if companion {
			provideMode = model.ProvideModeStream
		}
		if _, _, _, _, _, err := newContract(context.Background(), server.NewId(), server.NewId(), nil, companion, !companion,
			model.Mib, provideMode, false, 0, connect.DefaultContractManagerSettings()); err != nil {
			t.Fatalf("companion=%t: %v", companion, err)
		}
	}
	if escrowCount != 2 {
		t.Fatalf("escrow creators ran %d times, want 2", escrowCount)
	}
}
