// Close ownership keeps financial payer networks separate from retained source
// clients. A missing cached payer never proves that a legacy escrow is free.
package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

type ContractCloseOwnerKind string

const (
	ContractCloseOwnerPayerNetwork ContractCloseOwnerKind = "payer-network"
	ContractCloseOwnerSourceClient ContractCloseOwnerKind = "source-client"
)

var errContractCloseOwnerUnresolved = errors.New("contract close owner is unresolved")

// The kind is part of the identity even when the two UUID values collide.
type ContractCloseOwner struct {
	Kind ContractCloseOwnerKind `json:"kind"`
	Id   server.Id              `json:"id"`
}

// Explicit retained payer metadata remains authoritative. Otherwise actual
// escrow grants identify the payer; only an absent escrow permits source scope.
func selectContractCloseOwner(sourceId server.Id, payerNetworkId *server.Id, escrowNetworkIds []server.Id, hasEscrow bool) (ContractCloseOwner, error) {
	if sourceId == (server.Id{}) {
		return ContractCloseOwner{}, fmt.Errorf("%w: source client is missing", errContractCloseOwnerUnresolved)
	}
	if payerNetworkId != nil {
		if *payerNetworkId == (server.Id{}) {
			return ContractCloseOwner{}, fmt.Errorf("%w: payer is empty", errContractCloseOwnerUnresolved)
		}
		return ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: *payerNetworkId}, nil
	}
	if hasEscrow {
		if len(escrowNetworkIds) != 1 || escrowNetworkIds[0] == (server.Id{}) {
			return ContractCloseOwner{}, fmt.Errorf("%w: legacy financial payer", errContractCloseOwnerUnresolved)
		}
		return ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: escrowNetworkIds[0]}, nil
	}
	return ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: sourceId}, nil
}

// Reuse the caller's connection. Retained contract/escrow identities survive
// client deletion; network_client is deliberately absent from this lookup.
func readContractCloseOwnerInConn(ctx context.Context, query server.PgCanQuery, contractId server.Id) (owner ContractCloseOwner, sourceId server.Id, err error) {
	var payerNetworkId *server.Id
	var escrowNetworkIds []server.Id
	var hasEscrow bool
	rows, err := query.Query(ctx, server.ContractCloseOwnerReadSql, contractId)
	if err != nil {
		return
	}
	defer rows.Close()
	if !rows.Next() {
		err = rows.Err()
		if err == nil {
			err = pgx.ErrNoRows
		}
		return
	}
	err = rows.Scan(&sourceId, &payerNetworkId, &escrowNetworkIds, &hasEscrow)
	if err != nil {
		return
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return
	}
	owner, err = selectContractCloseOwner(sourceId, payerNetworkId, escrowNetworkIds, hasEscrow)
	return
}

// Free contracts have report/usage custody but no debit or payout to fund.
// Claim the outcome as the ordinary no-escrow close path does, even when
// reported bytes are positive.
func settleLegacyContractWithoutEscrowInTx(ctx context.Context, tx server.PgTx, contractId server.Id,
	outcome ContractOutcome, clearDispute bool) (posts []func() any, closed bool, err error) {
	if err = validateContractFreeSettlementOwnerInTx(ctx, tx, contractId); err != nil {
		return
	}
	var clockByteCount ByteCount
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party='destination'),0)`, contractId).Scan(&clockByteCount); err != nil {
		return
	}
	if clearDispute {
		_, err = tx.Exec(ctx, `UPDATE transfer_contract SET dispute=false,close_time=clock_timestamp() AT TIME ZONE 'UTC' WHERE contract_id=$1`, contractId)
		if err != nil {
			return
		}
	}
	closed, err = claimContractOutcomeInTx(ctx, tx, contractId, outcome)
	if err == nil && closed && clockByteCount > 0 {
		posts = append(posts, legacySettlementClockPost(ctx, clockByteCount))
	}
	return
}
