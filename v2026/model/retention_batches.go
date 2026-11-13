package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Rows one retention transaction deletes, and how long one cleanup runs before
// it leaves the rest of a backlog to its next run. Vars so tests can drive the
// batch loop.
var retentionBatchSize = 10000
var retentionRunBudget = 30 * time.Second

// A retention target whose primary key is a time-ordered id (server.NewId,
// which leads with its creation millisecond): its expired rows are a range of
// the key below the id floor of the cutoff (server.ContractIdFloor), so the
// delete reads the primary key and needs no index on its time column. The
// time column is checked as well, so a row is deleted only when both its id
// and its time are older than the cutoff. Names are code constants.
type timeOrderedRetention struct {
	table string
	sql   string
}

func newTimeOrderedRetention(table string, idColumn string, timeColumn string) *timeOrderedRetention {
	return &timeOrderedRetention{
		table: table,
		sql: fmt.Sprintf(
			`
			DELETE FROM %[1]s
			WHERE %[2]s IN (
				SELECT %[2]s
				FROM %[1]s
				WHERE %[2]s < $1 AND %[3]s < $2
				ORDER BY %[2]s
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			`,
			table,
			idColumn,
			timeColumn,
		),
	}
}

// Deletes the rows older than minTime, oldest first and batchSize to a
// transaction, until a batch comes back short or deadline has passed; more is
// true when the deadline ended the run with rows left. Each committed batch is
// counted under the table.
func (self *timeOrderedRetention) RemoveBefore(ctx context.Context, minTime time.Time, batchSize int, deadline time.Time) (removedCount int64, more bool) {
	return removeRetentionBatches(ctx, self.table, batchSize, deadline, func(tx server.PgTx) int64 {
		tag := server.RaisePgResult(tx.Exec(ctx, self.sql, server.ContractIdFloor(minTime), minTime.UTC(), batchSize))
		return tag.RowsAffected()
	})
}

// A retention target whose time column has an index: its expired rows are a
// range of that index, oldest first, deleted by key. A row a writer holds is
// skipped for the next run, and the time is checked again on the row deleted.
type indexedRetention struct {
	table string
	sql   string
}

func newIndexedRetention(table string, keyColumn string, timeColumn string) *indexedRetention {
	return &indexedRetention{
		table: table,
		sql: fmt.Sprintf(
			`
			DELETE FROM %[1]s
			WHERE %[2]s IN (
				SELECT %[2]s
				FROM %[1]s
				WHERE %[3]s < $1
				ORDER BY %[3]s
				LIMIT $2
				FOR UPDATE SKIP LOCKED
			) AND %[3]s < $1
			`,
			table,
			keyColumn,
			timeColumn,
		),
	}
}

func (self *indexedRetention) RemoveBefore(ctx context.Context, minTime time.Time, batchSize int, deadline time.Time) (removedCount int64, more bool) {
	return removeRetentionBatches(ctx, self.table, batchSize, deadline, func(tx server.PgTx) int64 {
		tag := server.RaisePgResult(tx.Exec(ctx, self.sql, minTime.UTC(), batchSize))
		return tag.RowsAffected()
	})
}

// A retention target small enough to read whole: its expired rows are found by
// a scan and deleted by row address, batchSize to a transaction. For a table
// whose time column has no index and whose rows number in the thousands.
type scannedRetention struct {
	table string
	sql   string
}

// timeColumn holds a timestamp or a timestamptz; the cutoff is compared in UTC.
func newScannedRetention(table string, timeColumn string) *scannedRetention {
	return &scannedRetention{
		table: table,
		sql: fmt.Sprintf(
			`
			DELETE FROM %[1]s
			WHERE ctid = ANY(ARRAY(
				SELECT ctid
				FROM %[1]s
				WHERE %[2]s < $1
				LIMIT $2
			))
			`,
			table,
			timeColumn,
		),
	}
}

func (self *scannedRetention) RemoveBefore(ctx context.Context, minTime time.Time, batchSize int, deadline time.Time) (removedCount int64, more bool) {
	return removeRetentionBatches(ctx, self.table, batchSize, deadline, func(tx server.PgTx) int64 {
		tag := server.RaisePgResult(tx.Exec(ctx, self.sql, minTime.UTC(), batchSize))
		return tag.RowsAffected()
	})
}

// Runs one batch per maintenance transaction until a batch comes back short,
// deadline passes, or ctx ends.
func removeRetentionBatches(
	ctx context.Context,
	table string,
	batchSize int,
	deadline time.Time,
	batch func(server.PgTx) int64,
) (removedCount int64, more bool) {
	for {
		select {
		case <-ctx.Done():
			return removedCount, true
		default:
		}
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			batchCount = batch(tx)
		}, server.TxReadCommitted)
		countRetentionDeletedRows(table, batchCount)
		removedCount += batchCount
		if batchCount < int64(batchSize) {
			return removedCount, false
		}
		if !time.Now().Before(deadline) {
			return removedCount, true
		}
	}
}
