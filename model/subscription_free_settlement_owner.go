// Missing escrow never erases an authoritative retained financial payer.
package model

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
)

// This custody refusal cannot authorize ordinary expiry's malformed fallback.
var errContractFreeSettlementOwner = errors.New("no-escrow settlement requires source-client ownership")

// The caller holds the contract row and has established escrow absence. Read
// the same retained authority as dispatch before any no-payout outcome write.
func validateContractFreeSettlementOwnerInTx(ctx context.Context, tx server.PgTx, contractId server.Id) error {
	owner, _, err := readContractCloseOwnerInConn(ctx, tx, contractId)
	if err != nil {
		return errors.Join(errContractFreeSettlementOwner, err)
	}
	if owner.Kind != ContractCloseOwnerSourceClient {
		return errContractFreeSettlementOwner
	}
	return nil
}
