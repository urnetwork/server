package server

import (
	"context"
	"strings"
	"testing"
)

// The egress key columns go on a table find-providers reads, so their ADD
// COLUMN gives up after 2 s rather than queue the readers behind its lock.
func TestProxyUniqueExitEgressKeyMigrationBoundsItsLockWait(t *testing.T) {
	if !strings.Contains(proxyUniqueExitEgressKeyColumnsSql, "SET LOCAL lock_timeout = '2s';") {
		t.Fatalf("the migration does not bound its lock wait at 2 s: %s", proxyUniqueExitEgressKeyColumnsSql)
	}
	for _, sql := range []string{
		proxyUniqueExitEgressKeyColumnsSql,
		proxyUniqueExitTableSql,
		proxyUniqueExitNetworkIndexSql,
		proxyExitAssignmentTableSql,
		proxyExitAssignmentNetworkEgressKeyLiveIndexSql,
		proxyExitAssignmentProxyLiveIndexSql,
		proxyExitAssignmentProviderLiveIndexSql,
		proxyExitAssignmentAssignedAtLiveIndexSql,
		proxyExitAssignmentProxyReleasedIndexSql,
		proxyExitAssignmentNetworkReleasedIndexSql,
		proxyExitAssignmentReleasedAtIndexSql,
	} {
		upper := strings.ToUpper(sql)
		for _, destructive := range []string{"DROP ", "DELETE ", "UPDATE ", "TRUNCATE", "ALTER COLUMN", "RENAME"} {
			if strings.Contains(upper, destructive) {
				t.Fatalf("unique-exit migration is not additive (%q): %s", destructive, sql)
			}
		}
	}
}

// The unique-exit migrations are in the list, in order, one contiguous block:
// the monitor's contracts version each by its position.
func TestProxyUniqueExitMigrationVersionsAreContiguous(t *testing.T) {
	v := ProxyUniqueExitMigrationVersion()
	versions := []int{
		v.EgressKeyColumns,
		v.UniqueExitTable,
		v.UniqueExitNetworkIndex,
		v.AssignmentTable,
		v.AssignmentNetworkEgressKeyLiveIndex,
		v.AssignmentProxyLiveIndex,
		v.AssignmentProviderLiveIndex,
		v.AssignmentAssignedAtLiveIndex,
		v.AssignmentProxyReleasedIndex,
		v.AssignmentNetworkReleasedIndex,
		v.AssignmentReleasedAtIndex,
	}
	for i, version := range versions {
		if version == 0 {
			t.Fatalf("unique-exit migration %d is not in the migration list", i)
		}
		if 0 < i && version != versions[i-1]+1 {
			t.Fatalf("unique-exit migrations are not contiguous: %v", versions)
		}
	}
}

// The definition PostgreSQL reports for the network live index, as the
// monitor's contract pins it (monitor/migration_proxy_unique_exit.go).
const proxyExitAssignmentNetworkLiveIndexTestDefinition = "CREATE INDEX proxy_exit_assignment_network_id_live ON public.proxy_exit_assignment USING btree (network_id) WHERE ((state)::text = ANY ((ARRAY['pending'::character varying, 'active'::character varying, 'replacing'::character varying])::text[]))"

const proxyExitAssignmentNetworkLiveIndexTestPredicate = "((state)::text = ANY ((ARRAY['pending'::character varying, 'active'::character varying, 'replacing'::character varying])::text[]))"

// The network live index is in the migration list as an online build after a
// recovery drop of its own name only: the table stays writable while it
// builds, and nothing else is dropped.
func TestProxyExitAssignmentNetworkLiveIndexMigrationIsOnline(t *testing.T) {
	version := ProxyExitAssignmentNetworkLiveIndexMigrationVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != proxyExitAssignmentNetworkLiveIndexMigration {
		t.Fatalf("the network live index migration is not in the list (version %d)", version)
	}
	steps := proxyExitAssignmentNetworkLiveIndexMigration.productionSqlSteps()
	if len(steps) != 2 ||
		strings.Join(strings.Fields(steps[0]), " ") != "DROP INDEX CONCURRENTLY IF EXISTS proxy_exit_assignment_network_id_live" ||
		!strings.HasPrefix(strings.Join(strings.Fields(steps[1]), " "), "CREATE INDEX CONCURRENTLY proxy_exit_assignment_network_id_live ON proxy_exit_assignment (network_id) WHERE state IN ('pending', 'active', 'replacing')") {
		t.Fatalf("the network live index migration is not a recovery drop and an online build: %q", steps)
	}
	if strings.Contains(strings.ToUpper(proxyExitAssignmentNetworkLiveIndexMigration.auditSql), "CONCURRENTLY") {
		t.Fatalf("the audit form must run in a transaction: %s", proxyExitAssignmentNetworkLiveIndexMigration.auditSql)
	}
}

// After every migration proxy_exit_assignment carries the network live index
// exactly as the monitor's contract pins it, valid and ready.
func TestProxyExitAssignmentNetworkLiveIndexAfterMigrations(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		MaintenanceDb(ctx, func(conn PgConn) {
			definition, predicate, valid := writePathTestIndexShape(t, ctx, conn, "proxy_exit_assignment_network_id_live")
			if definition != proxyExitAssignmentNetworkLiveIndexTestDefinition || predicate != proxyExitAssignmentNetworkLiveIndexTestPredicate || !valid {
				t.Fatalf("proxy_exit_assignment_network_id_live = %q predicate %q valid=%t, want %q",
					definition, predicate, valid, proxyExitAssignmentNetworkLiveIndexTestDefinition)
			}
		}, OptReadOnly())
	})
}

// The migration recovers from an interrupted or look-alike build: its recovery
// step drops a same-name index over every row, the build makes the exact index,
// again on replay, and its audit form makes the same index.
func TestProxyExitAssignmentNetworkLiveIndexMigrationRecovers(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		check := func(step string) {
			MaintenanceDb(ctx, func(conn PgConn) {
				definition, predicate, valid := writePathTestIndexShape(t, ctx, conn, "proxy_exit_assignment_network_id_live")
				if definition != proxyExitAssignmentNetworkLiveIndexTestDefinition || predicate != proxyExitAssignmentNetworkLiveIndexTestPredicate || !valid {
					t.Fatalf("after %s proxy_exit_assignment_network_id_live = %q predicate %q valid=%t", step, definition, predicate, valid)
				}
			}, OptReadOnly(), OptNoRetry())
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, proxyExitAssignmentTableSql))
			// same-name residue over every row, as a hand-made build could leave
			RaisePgResult(conn.Exec(ctx, `CREATE INDEX proxy_exit_assignment_network_id_live ON proxy_exit_assignment (network_id)`))
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, proxyExitAssignmentNetworkLiveIndexMigration))
		}, OptReadWrite(), OptNoRetry())
		check("recovery")
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(executeOnlineSqlMigrationOnConn(ctx, conn, proxyExitAssignmentNetworkLiveIndexMigration))
		}, OptReadWrite(), OptNoRetry())
		check("replay")
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, proxyExitAssignmentNetworkLiveIndexMigration.auditSql))
		})
		check("the audit form")
	})
}
