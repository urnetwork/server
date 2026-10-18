// Missing escrow never erases an authoritative retained financial payer.
package model

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
)

// This custody refusal cannot authorize ordinary expiry's malformed fallback.
var errContractFreeSettlementOwner = errors.New("no-escrow settlement requires source-client ownership or a payer contract that reserved no bytes")

// The caller holds the contract row and has established escrow absence. Read
// the same retained authority as dispatch before any no-payout outcome write.
// A payer's contract that reserved no bytes had nothing to escrow, so an
// unanchored zero-byte contract closes free under its payer too; a funded
// payer contract without escrow is lost custody and stays refused. No writer
// changes a contract's transfer byte count after its insert, so the count
// read here is the reservation the contract was created with.
func validateContractFreeSettlementOwnerInTx(ctx context.Context, tx server.PgTx, contractId server.Id) error {
	owner, _, err := readContractCloseOwnerInConn(ctx, tx, contractId)
	if err != nil {
		return errors.Join(errContractFreeSettlementOwner, err)
	}
	if owner.Kind == ContractCloseOwnerSourceClient {
		return nil
	}
	if owner.Kind != ContractCloseOwnerPayerNetwork {
		return errContractFreeSettlementOwner
	}
	var transferByteCount ByteCount
	if err := tx.QueryRow(ctx, `SELECT transfer_byte_count FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&transferByteCount); err != nil {
		return errors.Join(errContractFreeSettlementOwner, err)
	}
	if transferByteCount == 0 {
		return nil
	}
	return errContractFreeSettlementOwner
}
