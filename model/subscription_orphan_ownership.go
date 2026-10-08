// Orphan escrow retirement uses the same balance owner as consumption. The
// primary-key page is fixed before ownership; no newly visible key joins it.
package model

import (
	"context"

	"github.com/urnetwork/server"
)

func ownedTransferEscrowOrphanPageInTx(ctx context.Context, tx server.PgTx, first bool,
	cursor []any, limit int) (count int64, locked []string, targets []any, found, admitted bool) {
	query := `SELECT contract_id,balance_id FROM transfer_escrow ORDER BY contract_id,balance_id LIMIT $1`
	args := []any{limit}
	if !first {
		query = `SELECT contract_id,balance_id FROM transfer_escrow
 WHERE (contract_id,balance_id)>($1::uuid,$2::uuid) ORDER BY contract_id,balance_id LIMIT $3`
		args = []any{cursor[0], cursor[1], limit}
	}
	var contractIds, balanceIds []server.Id
	rows, err := tx.Query(ctx, query, args...)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var contractId, balanceId server.Id
			server.Raise(rows.Scan(&contractId, &balanceId))
			contractIds = append(contractIds, contractId)
			balanceIds = append(balanceIds, balanceId)
		}
	})
	count = int64(len(contractIds))
	if count == 0 {
		return 0, nil, nil, false, true
	}
	admitted, err = tryTransferBalanceOwnershipInTx(ctx, tx, balanceIds)
	server.Raise(err)
	if !admitted {
		return 0, nil, nil, false, false
	}
	rows, err = tx.Query(ctx, `SELECT matched.ctid::text
 FROM unnest($1::uuid[],$2::uuid[]) AS requested(contract_id,balance_id)
 CROSS JOIN LATERAL (
 SELECT escrow.ctid FROM transfer_escrow AS escrow
 WHERE escrow.contract_id=requested.contract_id AND escrow.balance_id=requested.balance_id
 AND NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=escrow.contract_id LIMIT 1 OFFSET 0)
 LIMIT 1 OFFSET 0 FOR UPDATE OF escrow
 ) AS matched`, contractIds, balanceIds)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var tuple string
			server.Raise(rows.Scan(&tuple))
			locked = append(locked, tuple)
		}
	})
	lastContract, lastBalance := contractIds[len(contractIds)-1], balanceIds[len(balanceIds)-1]
	return count, locked, []any{&lastContract, &lastBalance}, true, true
}
