// Runs the plan-wall battery's snapshot (SIGNALS.md 5.8) on the PostgreSQL 18
// TestEnv before and after the contract partition cutover: transfer_contract's
// idx_scan rows keep the published index names once its scans are counted on
// the partitions' leaf indexes.
package monitor

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/urnetwork/server"
)

// The index names of the snapshot's idx rows (pg_stat_statements, the other
// half of the snapshot, is an extension the test database need not load).
func batteryNativeIndexNames(t testing.TB, ctx context.Context) []string {
	t.Helper()
	names := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, planWallIndexSnapshotSql)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var kind, name, count, total string
				server.Raise(result.Scan(&kind, &name, &count, &total))
				if kind != "idx" {
					continue
				}
				if _, err := strconv.ParseInt(count, 10, 64); err != nil || total != "0" {
					t.Fatalf("idx row %s is not a whole count: %s %s", name, count, total)
				}
				names = append(names, name)
			}
		})
	})
	slices.Sort(names)
	return names
}

// The indexes declared on transfer_contract itself: the plain table's, or the
// partitioned parent's published ones.
func batteryNativeDeclaredIndexes(t testing.TB, ctx context.Context) []string {
	t.Helper()
	names := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT index_class.relname
			FROM pg_index index_state
			JOIN pg_class index_class ON index_class.oid = index_state.indexrelid
			WHERE index_state.indrelid = to_regclass('public.transfer_contract')
		`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				server.Raise(result.Scan(&name))
				names = append(names, name)
			}
		})
	})
	slices.Sort(names)
	return names
}

func TestPlanWallSnapshotPublishedIndexesNative(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if !server.IsContractPartitioned(ctx) {
			if got, want := batteryNativeIndexNames(t, ctx), batteryNativeDeclaredIndexes(t, ctx); !slices.Equal(got, want) {
				t.Fatalf("plain idx rows %v, want %v", got, want)
			}
			server.Testing_PartitionContracts(ctx)
		}
		got, want := batteryNativeIndexNames(t, ctx), batteryNativeDeclaredIndexes(t, ctx)
		if len(want) == 0 || !slices.Equal(got, want) {
			t.Fatalf("partitioned idx rows %v, want the published %v", got, want)
		}
		if !slices.Contains(got, "transfer_contract_unresolved_source_pair_create_time") {
			t.Fatalf("the source-pair role is missing from %v", got)
		}
	})
}
