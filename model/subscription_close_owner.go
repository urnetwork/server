// Close routing keeps financial payer networks separate from retained source
// clients. A missing cached payer never proves that a legacy escrow is free.
package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
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

func (self ContractCloseOwner) valid() bool {
	return self.Id != (server.Id{}) && (self.Kind == ContractCloseOwnerPayerNetwork || self.Kind == ContractCloseOwnerSourceClient)
}

func (self ContractCloseOwner) runOnce() *task.RunOnceOption {
	if self.Kind == ContractCloseOwnerPayerNetwork {
		return task.RunOnce("flush_legacy_payer_settlements", self.Id)
	}
	return task.RunOnce("flush_legacy_source_settlements", self.Id)
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

// Existing payer workers and new source workers use exactly the scheduler's
// selector before financial work. A stale queue hint is repaired under its
// private intent/contract locks and rediscovered under its actual owner.
// An explicit locked payer and a known empty escrow can be resolved without
// another query. Historical NULL-payer escrow alone needs the durable lookup.
func validateLegacyCloseOwnerHeaderInTx(ctx context.Context, tx server.PgTx, contractId, sourceId server.Id,
	payerNetworkId *server.Id, hasEscrow *bool) (ContractCloseOwner, bool, error) {
	var actual ContractCloseOwner
	var err error
	if payerNetworkId != nil || (hasEscrow != nil && !*hasEscrow) {
		actual, err = selectContractCloseOwner(sourceId, payerNetworkId, nil, false)
	} else {
		actual, sourceId, err = readContractCloseOwnerInConn(ctx, tx, contractId)
	}
	if err != nil {
		return actual, false, err
	}
	expected, scoped := ctx.Value(legacySettlementCloseScopeKey{}).(ContractCloseOwner)
	if !scoped || actual == expected {
		return actual, true, nil
	}
	var payer *server.Id
	if actual.Kind == ContractCloseOwnerPayerNetwork {
		payer = &actual.Id
	}
	_, err = tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=$2,source_client_id=$3 WHERE contract_id=$1`, contractId, payer, sourceId)
	return actual, false, err
}

// Free contracts have report/usage custody but no debit or payout to fund.
// Retire the intent and claim the outcome in the same transaction, matching
// the ordinary no-escrow close path even when reported bytes are positive.
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
	if _, err = tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, contractId); err != nil {
		return
	}
	closed, err = claimContractOutcomeInTx(ctx, tx, contractId, outcome)
	if err == nil && closed && clockByteCount > 0 {
		posts = append(posts, legacySettlementClockPost(ctx, clockByteCount))
	}
	return
}
