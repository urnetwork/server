package model

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

const completedTransferBalanceBatchSize = 256

// Discovery keeps at most 16,777,216 raw UUIDs in an anonymous local file.
// An oversized cohort refuses before deletion; it needs an explicit budget
// revision or indexed pagination, not a restart at the same retained page.
const completedTransferBalanceSpoolByteLimit int64 = 256 << 20

var errCompletedTransferBalanceSpoolCapacity = errors.New("completed transfer balance discovery exceeds its spool byte limit")

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
	removeCompletedTransferBalanceBatchesWithByteLimit(ctx, minTime, completedTransferBalanceSpoolByteLimit)
}

func removeCompletedTransferBalanceBatchesWithByteLimit(ctx context.Context, minTime time.Time, byteLimit int64) {
	if byteLimit <= 0 || completedTransferBalanceSpoolByteLimit < byteLimit {
		panic("invalid completed transfer balance spool byte limit")
	}
	server.Raise(ctx.Err())
	spool, err := os.CreateTemp("", "urnetwork-balance-retention-*")
	server.Raise(err)
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()
	// Unlink while open: cancellation, panic and process exit cannot leave a
	// named file of balance IDs. CreateTemp already restricts access to 0600.
	server.Raise(os.Remove(spool.Name()))
	writer := bufio.NewWriterSize(spool, completedTransferBalanceBatchSize*len(server.Id{}))
	var written int64
	// Keep the one indexed discovery pass and its snapshot, but fully drain
	// and release its connection before any transaction or mirror refresh.
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		// A safe connection retry must replace, not append to, a partial pass.
		writer.Reset(spool)
		server.Raise(spool.Truncate(0))
		_, err := spool.Seek(0, io.SeekStart)
		server.Raise(err)
		written = 0
		rows, err := conn.Query(ctx, completedTransferBalanceCandidatesSql, minTime.UTC())
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				server.Raise(ctx.Err())
				var id server.Id
				server.Raise(rows.Scan(&id))
				if byteLimit-written < int64(len(id)) {
					panic(errCompletedTransferBalanceSpoolCapacity)
				}
				n, err := writer.Write(id[:])
				server.Raise(err)
				if n != len(id) {
					panic(io.ErrShortWrite)
				}
				written += int64(n)
			}
		})
	})
	server.Raise(writer.Flush())
	server.Raise(ctx.Err())
	_, err = spool.Seek(0, io.SeekStart)
	server.Raise(err)
	reader := bufio.NewReaderSize(spool, completedTransferBalanceBatchSize*len(server.Id{}))
	batch := make([]server.Id, 0, completedTransferBalanceBatchSize)
	for read := int64(0); read < written; read += int64(len(server.Id{})) {
		server.Raise(ctx.Err())
		var id server.Id
		_, err := io.ReadFull(reader, id[:])
		server.Raise(err)
		batch = append(batch, id)
		if len(batch) == completedTransferBalanceBatchSize {
			removeCompletedTransferBalanceBatch(ctx, batch, minTime)
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		server.Raise(ctx.Err())
		removeCompletedTransferBalanceBatch(ctx, batch, minTime)
	}
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
