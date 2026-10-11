// Contract creation deducts first. newContract always runs escrow admission
// for public and companion contracts; only escrow's exhausted balance refusal,
// while the contract degradation valve is open, falls back to a zero escrow
// creator. Every other refusal stands, including a data cap or test drain
// refusal that shares the exhausted message, and the valve is read only after
// an exhausted refusal. Database free.
package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The calls one newContract made to the replaced creators and valve.
type zeroEscrowRoutingCalls struct {
	escrowRequests []model.ByteCount
	freeRequests   []model.ByteCount
	valveReads     int
}

// Replaces the lifecycle lookup, the valve and the four creators. The escrow
// creators return escrowResult or escrowErr; the zero escrow creators return
// freeResult. Restore with the result.
func withZeroEscrowRoutingCreators(
	sourceNetworkId server.Id,
	destinationNetworkId server.Id,
	escrowResult *model.TransferEscrow,
	escrowErr error,
	freeResult *model.TransferEscrow,
	valveOpen bool,
	calls *zeroEscrowRoutingCalls,
) func() {
	previousFind, previousValve := findActiveClientPairNetworks, zeroContractCost
	previousPublic, previousCompanion := createTransferEscrow, createCompanionTransferEscrow
	previousFreePublic, previousFreeCompanion := createZeroEscrowContract, createZeroEscrowCompanionContract
	findActiveClientPairNetworks = func(ctx context.Context, a server.Id, b server.Id) (*server.Id, *server.Id) {
		return &sourceNetworkId, &destinationNetworkId
	}
	zeroContractCost = func(context.Context) bool {
		calls.valveReads += 1
		return valveOpen
	}
	escrow := func(contractTransferByteCount model.ByteCount) (*model.TransferEscrow, error) {
		calls.escrowRequests = append(calls.escrowRequests, contractTransferByteCount)
		if escrowErr != nil {
			return nil, escrowErr
		}
		return escrowResult, nil
	}
	free := func(contractTransferByteCount model.ByteCount) (*model.TransferEscrow, error) {
		calls.freeRequests = append(calls.freeRequests, contractTransferByteCount)
		return freeResult, nil
	}
	createTransferEscrow = func(_ context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount) (*model.TransferEscrow, error) {
		return escrow(contractTransferByteCount)
	}
	createCompanionTransferEscrow = func(_ context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount, _ time.Duration) (*model.TransferEscrow, error) {
		return escrow(contractTransferByteCount)
	}
	createZeroEscrowContract = func(_ context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount) (*model.TransferEscrow, error) {
		return free(contractTransferByteCount)
	}
	createZeroEscrowCompanionContract = func(_ context.Context, _ server.Id, _ server.Id, _ server.Id, _ server.Id, contractTransferByteCount model.ByteCount, _ time.Duration) (*model.TransferEscrow, error) {
		return free(contractTransferByteCount)
	}
	return func() {
		findActiveClientPairNetworks, zeroContractCost = previousFind, previousValve
		createTransferEscrow, createCompanionTransferEscrow = previousPublic, previousCompanion
		createZeroEscrowContract, createZeroEscrowCompanionContract = previousFreePublic, previousFreeCompanion
	}
}

// For public and companion contracts alike: a funded payer escrows and the
// valve is never read; an exhausted payer is free only while the valve is open
// and refused while it is closed; a data cap or drain refusal, which shares the
// exhausted message but not its identity, and any other error are refused
// without reading the valve. Escrow and free creation are both asked for the
// free payer's 64 MiB cap of a 128 MiB request, and the controller signs what
// the creator returns: its size and priority, never the trusted priority.
func TestNewContractFallsBackOnlyForExhaustedBalance(t *testing.T) {
	defer model.Testing_SetMaxContractTransferByteCount(64*model.Mib, 0)()
	lookup := &proLookup{}
	defer withProLookup(lookup)()

	exhausted := fmt.Errorf("%w (%d).", model.ErrContractBalanceExhausted, 0)
	// the message data cap and test drain refusals share with exhaustion
	capped := errors.New("Insufficient balance (0).")
	failed := errors.New("synthetic escrow failure")
	capSize := 64 * model.Mib
	originContractId := server.NewId()
	expirationTime := time.UnixMilli(2_000_000_000_000).UTC()

	for _, companion := range []bool{false, true} {
		for _, test := range []struct {
			name          string
			escrowErr     error
			valveOpen     bool
			wantFree      bool
			wantErr       error
			wantValveRead bool
		}{
			{name: "funded, valve open", valveOpen: true},
			{name: "funded, valve closed"},
			{name: "exhausted, valve open", escrowErr: exhausted, valveOpen: true, wantFree: true, wantValveRead: true},
			{name: "exhausted, valve closed", escrowErr: exhausted, wantErr: exhausted, wantValveRead: true},
			{name: "data cap or drain, valve open", escrowErr: capped, valveOpen: true, wantErr: capped},
			{name: "other failure, valve open", escrowErr: failed, valveOpen: true, wantErr: failed},
		} {
			name := fmt.Sprintf("companion=%t %s", companion, test.name)
			escrowResult := &model.TransferEscrow{
				ContractId:          server.NewId(),
				CompanionContractId: &originContractId,
				ExpirationTime:      expirationTime,
				TransferByteCount:   2 * model.Mib,
				Priority:            model.PaidPriority,
			}
			freeResult := &model.TransferEscrow{
				ContractId:          server.NewId(),
				CompanionContractId: &originContractId,
				ExpirationTime:      expirationTime,
				TransferByteCount:   capSize,
				Priority:            model.UnpaidPriority,
				Balances:            []*model.TransferEscrowBalance{},
			}
			calls := &zeroEscrowRoutingCalls{}
			restore := withZeroEscrowRoutingCreators(server.NewId(), server.NewId(), escrowResult, test.escrowErr, freeResult, test.valveOpen, calls)
			provideMode := model.ProvideModePublic
			if companion {
				provideMode = model.ProvideModeStream
			}
			contractId, count, priority, _, _, err := newContract(context.Background(), server.NewId(), server.NewId(), nil,
				companion, !companion, 128*model.Mib, provideMode, false, 0, connect.DefaultContractManagerSettings())
			restore()

			if len(calls.escrowRequests) != 1 || calls.escrowRequests[0] != capSize {
				t.Errorf("%s: escrow requests %v, want one at the %d cap", name, calls.escrowRequests, capSize)
			}
			if (calls.valveReads != 0) != test.wantValveRead {
				t.Errorf("%s: valve read %d times, want read=%t", name, calls.valveReads, test.wantValveRead)
			}
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) || len(calls.freeRequests) != 0 {
					t.Errorf("%s: err=%v free=%v, want %v and no free contract", name, err, calls.freeRequests, test.wantErr)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			want := escrowResult
			if test.wantFree {
				want = freeResult
				if len(calls.freeRequests) != 1 || calls.freeRequests[0] != capSize {
					t.Errorf("%s: free requests %v, want one at the %d cap", name, calls.freeRequests, capSize)
				}
			} else if len(calls.freeRequests) != 0 {
				t.Errorf("%s: a payer escrow funded was given a free contract", name)
			}
			if contractId != want.ContractId || count != want.TransferByteCount || priority != want.Priority || priority == model.TrustedPriority {
				t.Errorf("%s: contract %s signs %d bytes at priority %d, want %s %d %d", name, contractId, count, priority, want.ContractId, want.TransferByteCount, want.Priority)
			}
		}
	}
}
