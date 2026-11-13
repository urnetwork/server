// The local abort cannot absorb cancellation, SQL failures or unknown commits.
package model

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server"
)

// Only the exact local signal becomes a typed refusal at the owning boundary.
func TestContractPublicationDeadlineRecoveryRequiresExactSignal(t *testing.T) {
	for _, failure := range []any{context.Canceled, context.DeadlineExceeded, errRedisPublicationLostCommitReply, ErrProberShardRetired,
		errors.Join(ErrProberShardRetired, errors.New("synthetic joined owner failure")), &contractPublicationDeadlineAbort{},
		errors.New("synthetic unrelated statement failure"), 17} {
		original := &TransferEscrow{ContractId: server.NewId()}
		escrow := original
		originalErr := errors.New("synthetic earlier owner outcome")
		returnErr := originalErr
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			func() {
				defer recoverContractPublicationDeadline(&escrow, &returnErr)
				panic(failure)
			}()
		}()
		if recovered != failure || escrow != original || returnErr != originalErr {
			t.Errorf("unrelated panic identity or existing outcome changed: failure=%v recovered=%v escrow_preserved=%t error_preserved=%t", failure, recovered, escrow == original, returnErr == originalErr)
		}
	}
}

// This result describes refusal, never acknowledgement of the transaction rollback.
func TestContractPublicationDeadlineRecoveryClearsProvisionalResult(t *testing.T) {
	escrow := &TransferEscrow{ContractId: server.NewId()}
	var returnErr error
	func() {
		defer recoverContractPublicationDeadline(&escrow, &returnErr)
		panic(contractPublicationDeadlineAbort{})
	}()
	if escrow != nil || returnErr != ErrProberShardRetired {
		t.Fatal("local abort retained provisional success or lost its domain cause", escrow, returnErr)
	}
	if !errors.Is(contractPublicationDeadlineAbort{}, ErrProberShardRetired) {
		t.Fatal("transaction-level abort lost its ordinary domain identity")
	}
}
