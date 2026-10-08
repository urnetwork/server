// Every grant, legacy revision and optional snapshot writer shares the actual
// balance identity. Queue/payer identity cannot exclude a native debit writer.
package model

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

var errTransferBalanceOwnershipBusy = errors.New("transfer balance ownership is busy")

func transferBalanceOwnershipKeys(balanceIds []server.Id) []server.PgOwnershipKey {
	keys := make([]server.PgOwnershipKey, len(balanceIds))
	for index, id := range balanceIds {
		keys[index] = server.NewPgOwnershipKey("transfer_balance", id)
	}
	return keys
}

// An empty reservation has no shared balance resource. Otherwise refusal
// precedes every grant, revision, snapshot or owned escrow mutation. The caller
// must end its no-retry transaction; the server retires any partial ownership.
func tryTransferBalanceOwnershipInTx(ctx context.Context, tx server.PgTx, balanceIds []server.Id) (bool, error) {
	if len(balanceIds) == 0 {
		return true, nil
	}
	return server.TryTxOwnership(ctx, tx, transferBalanceOwnershipKeys(balanceIds))
}

// Read exact contract tuples after private contract/intent custody. Include
// missing grants: their legacy escrow can still advance a retained revision.
// The complete key set is proportional to the supplied contracts' reservations;
// no payload-size fallback is allowed to bypass the common balance owner.
func tryContractTransferBalanceOwnershipInTx(ctx context.Context, tx server.PgTx, contractIds []server.Id) (bool, error) {
	balanceIds, err := contractTransferBalanceIdsInTx(ctx, tx, contractIds)
	if err != nil {
		return false, err
	}
	return tryTransferBalanceOwnershipInTx(ctx, tx, balanceIds)
}

// Financial commits also publish their durable mirror and immutable provider
// owners. Declare those keys before grant SQL; publication never grows the set.
func legacyFinancialOwnershipKeys(contractIds, balanceIds []server.Id) []server.PgOwnershipKey {
	keys := transferBalanceOwnershipKeys(balanceIds)
	for _, id := range balanceIds {
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("legacy_net_escrow_mirror", id)))
	}
	for _, id := range contractIds {
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", id)))
	}
	return keys
}

func tryLegacyFinancialOwnershipInTx(ctx context.Context, tx server.PgTx, contractIds []server.Id) (bool, error) {
	balanceIds, err := contractTransferBalanceIdsInTx(ctx, tx, contractIds)
	if err != nil {
		return false, err
	}
	return server.TryTxOwnership(ctx, tx, legacyFinancialOwnershipKeys(contractIds, balanceIds))
}

func contractTransferBalanceIdsInTx(ctx context.Context, tx server.PgTx, contractIds []server.Id) ([]server.Id, error) {
	if len(contractIds) == 0 {
		return nil, nil
	}
	var balanceIds []server.Id
	rows, err := tx.Query(ctx, `SELECT DISTINCT escrow.balance_id
 FROM unnest($1::uuid[]) AS requested(contract_id)
 CROSS JOIN LATERAL (SELECT balance_id FROM transfer_escrow
 WHERE contract_id=requested.contract_id OFFSET 0) AS escrow ORDER BY escrow.balance_id`, contractIds)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id server.Id
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		balanceIds = append(balanceIds, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return balanceIds, nil
}
