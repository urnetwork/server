// The index diet (owner decision 2026-11-01): drops of contract and sweep
// indexes on Main that no current reader needs. The operator drops each one
// first, by hand, with the same DROP INDEX CONCURRENTLY IF EXISTS on a direct
// session; its migration then records the drop, so the schema audit never
// recreates the index, and runs as a no-op. Each is a named value the
// migration list holds, so its version is its position in the list wherever
// a merge places it: the monitor's catalog contracts and the tests read a
// version through IndexDietMigrationVersion and never name one by number.
// The list in db_migrations.go keeps each drop's reasons.
package server

import (
	"fmt"
)

// One index diet migration, by the index it drops.
type IndexDietMigration int

const (
	IndexDietReapPendingIndexDrop IndexDietMigration = iota
	IndexDietStreamIndexDrop
	IndexDietSweepPaymentIndexDrop
)

// The drop of transfer_contract_reap_pending_create_time.
var reapPendingIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS transfer_contract_reap_pending_create_time`,
	`DROP INDEX IF EXISTS transfer_contract_reap_pending_create_time`,
)

// The drop of transfer_contract_stream_id.
var streamIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS transfer_contract_stream_id`,
	`DROP INDEX IF EXISTS transfer_contract_stream_id`,
)

// The drop of transfer_escrow_sweep_payment_id.
var sweepPaymentIndexDropMigration = newOnlineSqlMigration(
	`DROP INDEX CONCURRENTLY IF EXISTS transfer_escrow_sweep_payment_id`,
	`DROP INDEX IF EXISTS transfer_escrow_sweep_payment_id`,
)

var indexDietMigrations = map[IndexDietMigration]*OnlineSqlMigration{
	IndexDietReapPendingIndexDrop:  reapPendingIndexDropMigration,
	IndexDietStreamIndexDrop:       streamIndexDropMigration,
	IndexDietSweepPaymentIndexDrop: sweepPaymentIndexDropMigration,
}

// The index each index diet migration drops.
var indexDietDroppedIndexes = map[IndexDietMigration]string{
	IndexDietReapPendingIndexDrop:  "transfer_contract_reap_pending_create_time",
	IndexDietStreamIndexDrop:       "transfer_contract_stream_id",
	IndexDietSweepPaymentIndexDrop: "transfer_escrow_sweep_payment_id",
}

// The version a database reaches once the migration has applied: its position
// in the migration list plus one.
func IndexDietMigrationVersion(migration IndexDietMigration) int {
	target, ok := indexDietMigrations[migration]
	if !ok {
		panic(fmt.Errorf("unknown index diet migration %d", migration))
	}
	for index, listed := range migrations {
		if listed == target {
			return index + 1
		}
	}
	panic(fmt.Errorf("index diet migration %d is not in the migration list", migration))
}
