package model

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

const completedTransferBalanceBatchSize = 256

// Discover the same indexed expiry set as the old retention delete, once.
// The existing index has only end_time: UUID keyset pages would repeatedly
// scan/sort a large equal-expiry cohort. Retained rows must not restart a page.
const completedTransferBalanceCandidatesSql = `
	SELECT balance_id FROM transfer_balance
	WHERE end_time <= $1
`

const completedTransferBalanceLockSql = `
	SELECT locked_balance.balance_id
	FROM unnest($1::uuid[]) AS candidate(balance_id)
	CROSS JOIN LATERAL (
		SELECT balance_id, end_time FROM transfer_balance
		WHERE balance_id=candidate.balance_id
		OFFSET 0 FOR UPDATE SKIP LOCKED
	) AS locked_balance
	WHERE locked_balance.end_time <= $2
`

const completedTransferBalanceDeleteSql = `
	DELETE FROM transfer_balance AS balance
	WHERE balance.balance_id=ANY($1)
		AND NOT EXISTS (
			SELECT 1 FROM prober_shard_run
			WHERE balance_id=balance.balance_id AND state<>'closed' OFFSET 0
		)
		AND NOT EXISTS (
			SELECT 1 FROM prober_identity
			WHERE singleton AND network_id=balance.network_id OFFSET 0
		)
		AND NOT EXISTS (
			SELECT 1 FROM transfer_escrow
			WHERE balance_id=balance.balance_id AND NOT settled OFFSET 0
		)
		AND NOT EXISTS (SELECT 1 FROM transfer_debit_journal WHERE balance_id=balance.balance_id)
	RETURNING balance.balance_id
`

// Expiry ends admission; it does not discharge escrow. Keep every unsettled
// row, including zero-byte anchors and terminal contracts whose settlement post
// has not finished. The old shared probe account is retained for separately
// fenced cleanup: it has no durable shard lifecycle that closes admission.
func removeCompletedTransferBalanceBatches(ctx context.Context, minTime time.Time) {
	// Stream the expiry range once; retain only one UUID batch in memory.
	// The reader holds no row locks. A second maintenance connection executes
	// short transactions, and their fresh snapshots recheck every candidate.
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, completedTransferBalanceCandidatesSql, minTime.UTC())
		server.WithPgResult(rows, err, func() {
			batch := make([]server.Id, 0, completedTransferBalanceBatchSize)
			flush := func() {
				if len(batch) > 0 {
					removeCompletedTransferBalanceBatch(ctx, batch, minTime)
					batch = batch[:0]
				}
			}
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				batch = append(batch, id)
				if len(batch) == completedTransferBalanceBatchSize {
					flush()
				}
			}
			flush()
		})
	})
}

func removeCompletedTransferBalanceBatch(ctx context.Context, candidates []server.Id, minTime time.Time) {
	if len(candidates) == 0 || len(candidates) > completedTransferBalanceBatchSize {
		panic("invalid completed transfer balance batch")
	}
	slices.SortFunc(candidates, func(a, b server.Id) int { return a.Cmp(b) })
	var deletedIds []server.Id
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		deletedIds = nil
		admitted, err := tryTransferBalanceOwnershipInTx(ctx, tx, candidates)
		server.Raise(err)
		if !admitted {
			return
		}
		var lockedIds []server.Id
		rows, err := tx.Query(ctx, completedTransferBalanceLockSql, candidates, minTime.UTC())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				lockedIds = append(lockedIds, id)
			}
		})
		if len(lockedIds) == 0 {
			return
		}
		// This must be a separate read-committed statement AFTER the balance
		// locks. A reservation/settlement committed before our lock must be
		// visible when absence is checked. The locked page already proved
		// expiry; repeating that predicate here permits a scan of the entire
		// expired index instead of primary-key reads of this page. Never lock
		// contracts underneath a balance; settlement owns the opposite order.
		rows, err = tx.Query(ctx, completedTransferBalanceDeleteSql, lockedIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				deletedIds = append(deletedIds, id)
			}
		})
	}, server.TxReadCommitted, server.OptNoRetry())
	refreshNetEscrow(ctx, deletedIds)
}
