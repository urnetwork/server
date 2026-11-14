// Network and friends-and-family contracts escrow from their source's balance
// like public contracts (owner, 2026-11-10: "all intra-traffic should count
// against the network data balance", "a yes this deducts from the sender's
// transfer balance"). newContract runs escrow admission for them; only
// escrow's exhausted refusal, while the contract degradation valve is open,
// falls back to a zero escrow contract, and every other refusal stands. The
// controller signs what the creator returned: its shrunk size and its escrow
// priority, never the trusted priority these contracts were signed with when
// they carried no escrow. Database free.
package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestNewContractEscrowsNetworkAndFriendsAndFamilyContracts(t *testing.T) {
	defer Testing_SetMaxContractTransferByteCount(64 * model.Mib)()

	exhausted := fmt.Errorf("%w (%d).", model.ErrContractBalanceExhausted, 0)
	// the message data cap and test drain refusals share with exhaustion
	capped := errors.New("Insufficient balance (0).")
	failed := errors.New("synthetic escrow failure")
	capSize := 64 * model.Mib
	expirationTime := time.UnixMilli(2_000_000_000_000).UTC()

	for _, mode := range []struct {
		name        string
		provideMode model.ProvideMode
		sameNetwork bool
	}{
		{name: "network", provideMode: model.ProvideModeNetwork, sameNetwork: true},
		{name: "friends and family", provideMode: model.ProvideModeFriendsAndFamily},
	} {
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
			name := fmt.Sprintf("%s: %s", mode.name, test.name)
			sourceNetworkId := server.NewId()
			destinationNetworkId := server.NewId()
			if mode.sameNetwork {
				destinationNetworkId = sourceNetworkId
			}
			// escrow shrank to fit a balance below the request
			escrowResult := &model.TransferEscrow{
				ContractId:        server.NewId(),
				ExpirationTime:    expirationTime,
				TransferByteCount: 2 * model.Mib,
				Priority:          model.PaidPriority,
			}
			freeResult := &model.TransferEscrow{
				ContractId:        server.NewId(),
				ExpirationTime:    expirationTime,
				TransferByteCount: capSize,
				Priority:          model.UnpaidPriority,
				Balances:          []*model.TransferEscrowBalance{},
			}
			calls := &zeroEscrowRoutingCalls{}
			restore := withZeroEscrowRoutingCreators(sourceNetworkId, destinationNetworkId, escrowResult, test.escrowErr, freeResult, test.valveOpen, calls)
			contractId, count, priority, err := newContractRecovering(func() (server.Id, model.ByteCount, model.Priority, error) {
				contractId, count, priority, _, _, err := newContract(context.Background(), server.NewId(), server.NewId(), nil,
					false, true, 128*model.Mib, mode.provideMode, false, 0, connect.DefaultContractManagerSettings())
				return contractId, count, priority, err
			})
			restore()

			if len(calls.escrowRequests) != 1 || calls.escrowRequests[0] != capSize {
				t.Errorf("%s: escrow requests %v, want one at the %d cap", name, calls.escrowRequests, capSize)
				continue
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

// A creation path that bypasses the replaced creators and reaches the
// database reports that as an error instead of ending the test.
func newContractRecovering(create func() (server.Id, model.ByteCount, model.Priority, error)) (contractId server.Id, count model.ByteCount, priority model.Priority, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("creation bypassed escrow admission: %v", recovered)
		}
	}()
	return create()
}
