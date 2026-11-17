// The unresolved predicate's statistics object
// (server/db_transfer_contract_unresolved_statistics.go) against the drained
// open set: without it the payer open-sum reads the payer's whole history
// through the composite index; with the migration and the operator's sample,
// both payer reads return to their partial index.
package model

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/server"
)

// The plan test explains the read the code sends.
func TestOpenTransferByteCountReadIsThePlanTestedRead(t *testing.T) {
	if openTransferByteCountSql != unresolvedPayerOpenSumSql {
		t.Fatal("GetOpenTransferByteCount's read differs from the read its plan test explains")
	}
}

func unresolvedPredicateStatisticsExec(ctx context.Context, sql string) (err error) {
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		_, err = conn.Exec(ctx, sql)
	}, server.OptReadWrite(), server.OptNoRetry())
	return
}

func unresolvedPredicateStatisticsExists(t testing.TB, ctx context.Context) (exists bool) {
	t.Helper()
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_statistic_ext WHERE stxname = $1 AND stxrelid = 'transfer_contract'::regclass)`,
			server.TransferContractUnresolvedPredicateStatisticsName).Scan(&exists))
	}, server.OptReadOnly())
	return
}

// Before the migration, the drained open set sends the open-sum to the
// composite skip scan, custom and generic. The migration's statement and the
// operator's two-column sample give the planner the predicate's real frequency
// (1,000 of 80,000), and every payer read returns to the partial index.
func TestUnresolvedPredicateStatisticsReturnThePayerReadsToTheirPartialIndex(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if version := server.DbVersion(ctx); version < server.TransferContractUnresolvedPredicateStatisticsVersion() {
			t.Fatalf("migrated to %d, before the statistics migration %d", version, server.TransferContractUnresolvedPredicateStatisticsVersion())
		}
		if !unresolvedPredicateStatisticsExists(t, ctx) {
			t.Fatal("the migrated schema has no statistics object on the unresolved predicate")
		}
		buildDrainedOpenSet(t, ctx)

		// the schema before the migration, analyzed as autovacuum leaves it
		server.Raise(unresolvedPredicateStatisticsExec(ctx,
			`DROP STATISTICS IF EXISTS public.`+server.TransferContractUnresolvedPredicateStatisticsName))
		server.Raise(unresolvedPredicateStatisticsExec(ctx, `ANALYZE transfer_contract`))
		for _, read := range unresolvedPayerReads()[:2] {
			scans, cost := unresolvedPayerPlan(t, ctx, read.sql, read.args...)
			t.Logf("before, %s: %v cost %.1f", read.name, scans, cost)
			if !slices.Contains(scans, openPayerCompositeIndex) || slices.Contains(scans, unresolvedPayerIndex) {
				t.Fatalf("before the migration %s reads transfer_contract through %v, want the composite skip scan of %s", read.name, scans, openPayerCompositeIndex)
			}
		}

		server.Raise(unresolvedPredicateStatisticsExec(ctx, server.TransferContractUnresolvedPredicateStatisticsSql))
		server.Raise(unresolvedPredicateStatisticsExec(ctx, server.TransferContractUnresolvedPredicateAnalyzeSql))

		// The object's data is the plain table's own, and its true frequency is
		// the open share, 1,000 of 80,000 (0.0125). The two-column sample takes
		// 30,000 of the 80,000 rows, so the frequency is a sample's: its standard
		// deviation is about 0.0005, and the bounds are twenty of them away.
		var trueFrequency *float64
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT (
					SELECT freq::float8 FROM pg_stats_ext_exprs,
						unnest(most_common_vals::text::boolean[], most_common_freqs) AS mcv(value, freq)
					WHERE statistics_name = $1 AND tablename = 'transfer_contract' AND NOT inherited AND value
				)`,
				server.TransferContractUnresolvedPredicateStatisticsName).Scan(&trueFrequency))
		}, server.OptReadOnly())
		if trueFrequency == nil || *trueFrequency < 0.0025 || 0.0225 < *trueFrequency {
			t.Fatalf("the predicate's statistics give a true frequency of %v, want about 0.0125", trueFrequency)
		}

		for _, read := range unresolvedPayerReads() {
			scans, cost := unresolvedPayerPlan(t, ctx, read.sql, read.args...)
			t.Logf("after, %s: %v cost %.1f", read.name, scans, cost)
			if len(scans) == 0 || slices.ContainsFunc(scans, func(scan string) bool { return scan != unresolvedPayerIndex }) {
				t.Errorf("after the migration %s reads transfer_contract through %v, want only %s", read.name, scans, unresolvedPayerIndex)
			}
		}
	})
}

// A running vacuum holds SHARE UPDATE EXCLUSIVE on transfer_contract, and
// CREATE STATISTICS waits for it holding a snapshot. The migration's statement
// gives up after its five-second lock bound instead, and creates nothing; once
// the object exists, the statement takes no lock at all.
func TestUnresolvedPredicateStatisticsMigrationBoundsItsLockWait(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Raise(unresolvedPredicateStatisticsExec(ctx,
			`DROP STATISTICS IF EXISTS public.`+server.TransferContractUnresolvedPredicateStatisticsName))

		// while another session holds the lock a vacuum holds
		holding := func(callback func()) {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE transfer_contract IN SHARE UPDATE EXCLUSIVE MODE`))
				callback()
			}, server.OptReadWrite(), server.OptNoRetry())
		}

		holding(func() {
			err := unresolvedPredicateStatisticsExec(ctx, server.TransferContractUnresolvedPredicateStatisticsSql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("the migration behind a held lock returned %v, want lock_not_available (55P03)", err)
			}
		})
		if unresolvedPredicateStatisticsExists(t, ctx) {
			t.Fatal("the refused migration created the statistics object")
		}

		server.Raise(unresolvedPredicateStatisticsExec(ctx, server.TransferContractUnresolvedPredicateStatisticsSql))
		if !unresolvedPredicateStatisticsExists(t, ctx) {
			t.Fatal("the migration did not create the statistics object")
		}

		holding(func() {
			if err := unresolvedPredicateStatisticsExec(ctx, server.TransferContractUnresolvedPredicateStatisticsSql); err != nil {
				t.Fatalf("the migration with the object present waited for the table: %v", err)
			}
		})
	})
}
