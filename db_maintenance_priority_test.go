// Database regressions for the bloat-triggered rebuild of the priority partial
// indexes. Each fixture creates a synthetic table under the production table
// name with the production partial index, and churns it the way contracts and
// escrows churn: every row enters the predicate at insert and all but one in
// fifty leave it, so every leaf page keeps a few live entries and vacuum can
// free none of them. The root-cause tests use only DbMaintenance, its options
// and SQL, so they ran unchanged on the code before the fix and failed there:
// transfer_escrow is never rebuilt as a whole and its partial index was not a
// priority index, and the one priority index was rebuilt only in its hash
// epoch and only when no transaction was ten minutes old. They now also set
// PriorityReindex, added later, since the rebuilds stay off until enabled.
package server

import (
	"context"
	"hash/fnv"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Rows in each churn fixture; one in fifty stays in the predicate.
const testDbMaintenanceChurnRowCount = 100000

func testDbMaintenanceRelationBytes(ctx context.Context, relationName string) int64 {
	var relationBytes int64
	Db(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(
			ctx,
			`SELECT pg_relation_size(to_regclass($1))`,
			pgx.Identifier{"public", relationName}.Sanitize(),
		).Scan(&relationBytes))
	})
	return relationBytes
}

// Vacuums so dead entries leave the index, and analyzes so its entry count and
// column statistics are current, as autovacuum would. The fixtures disable
// autovacuum so no worker holds their table during maintenance.
func testDbMaintenanceVacuumAnalyze(ctx context.Context, tableName string) {
	MaintenanceDb(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `VACUUM (ANALYZE) `+pgx.Identifier{"public", tableName}.Sanitize()))
	}, OptNoRetry())
}

// A transfer_escrow carrying the production unsettled partial index. With
// settle, all but one in fifty escrows settle, the way their contracts close.
func testDbMaintenanceCreateEscrowFixture(ctx context.Context, settle bool) {
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `
			CREATE TABLE transfer_escrow (
				contract_id uuid NOT NULL,
				balance_id uuid NOT NULL,
				balance_byte_count bigint NOT NULL,
				settled boolean NOT NULL DEFAULT false,
				PRIMARY KEY (contract_id, balance_id)
			) WITH (autovacuum_enabled = false);
			CREATE INDEX transfer_escrow_unsettled_balance_contract
				ON transfer_escrow (balance_id, contract_id)
				INCLUDE (balance_byte_count)
				WHERE settled = false
		`))
		// contract ids increase with n, as time-ordered ids do; sixteen balances
		RaisePgResult(conn.Exec(ctx, `
			INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count)
			SELECT
				lpad(to_hex(n), 32, '0')::uuid,
				('b' || lpad(to_hex(n % 16), 31, '0'))::uuid,
				n
			FROM generate_series(1, $1::bigint) n
		`, testDbMaintenanceChurnRowCount))
		if settle {
			RaisePgResult(conn.Exec(ctx, `UPDATE transfer_escrow SET settled = true WHERE balance_byte_count % 50 <> 0`))
		}
	})
	testDbMaintenanceVacuumAnalyze(ctx, "transfer_escrow")
}

// A transfer_contract carrying the production open partial index, with all but
// one in fifty contracts closed.
func testDbMaintenanceCreateOpenContractFixture(ctx context.Context) {
	Db(ctx, func(conn PgConn) {
		RaisePgResult(conn.Exec(ctx, `
			CREATE TABLE transfer_contract (
				contract_id uuid NOT NULL PRIMARY KEY,
				create_time timestamp NOT NULL,
				open boolean NOT NULL DEFAULT true,
				sequence bigint NOT NULL
			) WITH (autovacuum_enabled = false);
			CREATE INDEX transfer_contract_open_partial_create_time
				ON transfer_contract (create_time)
				WHERE open
		`))
		RaisePgResult(conn.Exec(ctx, `
			INSERT INTO transfer_contract (contract_id, create_time, sequence)
			SELECT
				lpad(to_hex(n), 32, '0')::uuid,
				timestamp '2026-01-01' + n * interval '1 millisecond',
				n
			FROM generate_series(1, $1::bigint) n
		`, testDbMaintenanceChurnRowCount))
		RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET open = false WHERE sequence % 50 <> 0`))
	})
	testDbMaintenanceVacuumAnalyze(ctx, "transfer_contract")
}

// Nothing rebuilt the escrow unsettled index: transfer_escrow is too large for
// the whole-table rotation, and the index was not a priority index, so it kept
// every page its churn ever split (on 2026-10-12, 125 GB for 7.6 GB of live
// entries). A full cycle of maintenance must rebuild it, alone, once it is
// bloated.
func TestDbMaintenanceRebuildsBloatedEscrowPartialIndex(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const indexName = "transfer_escrow_unsettled_balance_contract"
		testDbMaintenanceCreateEscrowFixture(ctx, true)

		beforeOid := testDbMaintenanceIndexOid(ctx, indexName)
		beforeBytes := testDbMaintenanceRelationBytes(ctx, indexName)
		beforePkeyOid := testDbMaintenanceIndexOid(ctx, "transfer_escrow_pkey")
		for epoch := uint64(0); epoch < DbReindexEpochs; epoch += 1 {
			outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
				Reindex:         true,
				Cleanup:         true,
				PriorityReindex: true,
			})
			if outcome.Stopped != "" {
				t.Fatalf("epoch %d outcome = %+v, want finished", epoch, *outcome)
			}
		}

		if afterOid := testDbMaintenanceIndexOid(ctx, indexName); afterOid == beforeOid {
			t.Fatalf("a full maintenance cycle left the bloated %s (%d bytes) unrebuilt", indexName, beforeBytes)
		}
		if afterBytes := testDbMaintenanceRelationBytes(ctx, indexName); beforeBytes < 4*afterBytes {
			t.Fatalf("%s is %d bytes after its rebuild, was %d", indexName, afterBytes, beforeBytes)
		}
		if afterPkeyOid := testDbMaintenanceIndexOid(ctx, "transfer_escrow_pkey"); afterPkeyOid != beforePkeyOid {
			t.Fatal("transfer_escrow was rebuilt as a whole")
		}
		if valid, invalid := testDbMaintenanceIndexes(ctx, "transfer_escrow"); len(invalid) != 0 || !slices.Contains(valid, indexName) {
			t.Fatalf("indexes after the rebuild: valid %v, invalid %v", valid, invalid)
		}
	})
}

// The one priority index was tried only in its hash epoch, behind the guard
// that defers every rebuild while any transaction is older than the table
// bound; on Main the hour-long payout and reliability transactions deferred
// both runs on 2026-10-12. A bloated priority index must be rebuilt behind a
// transaction that defers the table rotation.
func TestDbMaintenanceRebuildsBloatedPriorityIndexBehindATableDeferral(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const indexName = "transfer_contract_open_partial_create_time"
		testDbMaintenanceCreateOpenContractFixture(ctx)
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				CREATE TABLE synthetic_unrelated (id bigint PRIMARY KEY);
				INSERT INTO synthetic_unrelated SELECT generate_series(1, 100)
			`))
		})

		// the epoch whose hash rotation selected this index before the fix
		hash := fnv.New64()
		_, _ = hash.Write([]byte(indexName))
		epoch := hash.Sum64() % DbReindexEpochs

		beforeOid := testDbMaintenanceIndexOid(ctx, indexName)
		beforeBytes := testDbMaintenanceRelationBytes(ctx, indexName)
		// the holder's transaction is older than this table bound
		release := testDbMaintenanceHoldWriterLock(t, ctx, "synthetic_unrelated")
		defer release()
		outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
			Reindex:           true,
			Cleanup:           true,
			PriorityReindex:   true,
			MaxTransactionAge: time.Millisecond,
		})
		release()

		if afterOid := testDbMaintenanceIndexOid(ctx, indexName); afterOid == beforeOid {
			t.Fatalf("the bloated %s (%d bytes) was not rebuilt behind a table deferral: outcome %+v", indexName, beforeBytes, *outcome)
		}
		if afterBytes := testDbMaintenanceRelationBytes(ctx, indexName); beforeBytes < 4*afterBytes {
			t.Fatalf("%s is %d bytes after its rebuild, was %d", indexName, afterBytes, beforeBytes)
		}
		if !slices.Contains(outcome.DoneIndexes, indexName) {
			t.Fatalf("outcome %+v does not report the rebuilt index", *outcome)
		}
	})
}

// A priority index without churn stays below the bloat bound and is left
// alone in every epoch.
func TestDbMaintenanceLeavesCompactPriorityIndex(t *testing.T) {
	env := DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const indexName = "transfer_escrow_unsettled_balance_contract"
		testDbMaintenanceCreateEscrowFixture(ctx, false)

		beforeOid := testDbMaintenanceIndexOid(ctx, indexName)
		for epoch := uint64(0); epoch < DbReindexEpochs; epoch += 1 {
			outcome := DbMaintenance(ctx, epoch, &DbMaintenanceOptions{
				Reindex:         true,
				Cleanup:         true,
				PriorityReindex: true,
			})
			if outcome.Stopped != "" || 0 < len(outcome.DoneIndexes) {
				t.Fatalf("epoch %d outcome = %+v, want finished with no priority rebuild", epoch, *outcome)
			}
		}
		if afterOid := testDbMaintenanceIndexOid(ctx, indexName); afterOid != beforeOid {
			t.Fatalf("the compact %s was rebuilt", indexName)
		}
	})
}
