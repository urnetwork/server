// The per-run bound of the priority index rebuilds and their deference to a
// vacuum of their table, which the fix added; the fixtures are in
// db_maintenance_priority_test.go.
package server

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A run rebuilds at most MaxPriorityRebuilds bloated priority indexes, smallest
// first, and a continuation of the epoch skips the ones already done.
func TestDbMaintenancePriorityRebuildsAreBoundedSmallestFirst(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const escrowIndexName = "transfer_escrow_unsettled_balance_contract"
		const contractIndexName = "transfer_contract_open_partial_create_time"
		testDbMaintenanceCreateEscrowFixture(ctx, true)
		testDbMaintenanceCreateOpenContractFixture(ctx)
		escrowBytes := testDbMaintenanceRelationBytes(ctx, escrowIndexName)
		contractBytes := testDbMaintenanceRelationBytes(ctx, contractIndexName)
		if escrowBytes == contractBytes {
			t.Fatalf("both fixtures are %d bytes; the order is undefined", escrowBytes)
		}
		smallerIndexName, largerIndexName := escrowIndexName, contractIndexName
		if contractBytes < escrowBytes {
			smallerIndexName, largerIndexName = contractIndexName, escrowIndexName
		}

		smallerOid := testDbMaintenanceIndexOid(ctx, smallerIndexName)
		largerOid := testDbMaintenanceIndexOid(ctx, largerIndexName)
		outcome := DbMaintenance(ctx, 0, &DbMaintenanceOptions{
			Reindex:             true,
			Cleanup:             true,
			PriorityReindex:     true,
			MaxPriorityRebuilds: 1,
		})
		if !slices.Equal(outcome.DoneIndexes, []string{smallerIndexName}) {
			t.Fatalf("bounded run outcome = %+v, want only %s done", *outcome, smallerIndexName)
		}
		if testDbMaintenanceIndexOid(ctx, smallerIndexName) == smallerOid {
			t.Fatalf("the smaller %s was not rebuilt", smallerIndexName)
		}
		if testDbMaintenanceIndexOid(ctx, largerIndexName) != largerOid {
			t.Fatalf("the larger %s was rebuilt beyond the bound", largerIndexName)
		}

		smallerOid = testDbMaintenanceIndexOid(ctx, smallerIndexName)
		outcome = DbMaintenance(ctx, 0, &DbMaintenanceOptions{
			Reindex:             true,
			Cleanup:             true,
			PriorityReindex:     true,
			MaxPriorityRebuilds: 1,
			DoneIndexes:         outcome.DoneIndexes,
		})
		if !slices.Equal(outcome.DoneIndexes, []string{smallerIndexName, largerIndexName}) {
			t.Fatalf("continuation outcome = %+v, want %s then %s done", *outcome, smallerIndexName, largerIndexName)
		}
		if testDbMaintenanceIndexOid(ctx, smallerIndexName) != smallerOid {
			t.Fatalf("the continuation rebuilt %s again", smallerIndexName)
		}
		if testDbMaintenanceIndexOid(ctx, largerIndexName) == largerOid {
			t.Fatalf("the continuation did not rebuild %s", largerIndexName)
		}
	})
}

// A priority index whose table is being vacuumed is left for a later run: its
// rebuild would cancel a regular autovacuum or wait behind an anti-wraparound
// one. A transaction holding the vacuum's lock stands in for the vacuum.
func TestDbMaintenanceSkipsPriorityRebuildWhileItsTableIsVacuumed(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const indexName = "transfer_escrow_unsettled_balance_contract"
		testDbMaintenanceCreateEscrowFixture(ctx, true)
		beforeOid := testDbMaintenanceIndexOid(ctx, indexName)

		conn, err := AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			conn.Release()
			t.Fatal(err)
		}
		released := false
		release := func() {
			if released {
				return
			}
			released = true
			_ = tx.Rollback(context.WithoutCancel(ctx))
			conn.Release()
		}
		t.Cleanup(release)
		RaisePgResult(tx.Exec(ctx, `LOCK TABLE `+pgx.Identifier{"public", "transfer_escrow"}.Sanitize()+` IN SHARE UPDATE EXCLUSIVE MODE`))

		outcome := DbMaintenance(ctx, 0, &DbMaintenanceOptions{
			Reindex:         true,
			Cleanup:         true,
			PriorityReindex: true,
		})
		release()
		if outcome.Stopped != "" || slices.Contains(outcome.DoneIndexes, indexName) {
			t.Fatalf("outcome while the table was held = %+v, want finished without %s", *outcome, indexName)
		}
		if testDbMaintenanceIndexOid(ctx, indexName) != beforeOid {
			t.Fatalf("%s was rebuilt while its table was held", indexName)
		}

		outcome = DbMaintenance(ctx, 0, &DbMaintenanceOptions{
			Reindex:         true,
			Cleanup:         true,
			PriorityReindex: true,
		})
		if !slices.Equal(outcome.DoneIndexes, []string{indexName}) {
			t.Fatalf("outcome after the release = %+v, want %s done", *outcome, indexName)
		}
		if testDbMaintenanceIndexOid(ctx, indexName) == beforeOid {
			t.Fatalf("%s was not rebuilt once its table was free", indexName)
		}
	})
}
