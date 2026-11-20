// Runs the DbMaintenance progress row's published-index resolution on the
// PostgreSQL 18 TestEnv, on plain contract tables and after the partition
// cutover, with a real leaf index and a _ccnew copy of it.
package monitor

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The published index the progress query reports for indexName.
func taskPublishedIndexNative(t testing.TB, ctx context.Context, indexName string) string {
	t.Helper()
	var published string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT `+dbMaintenancePublishedIndexSql("to_regclass($1)::oid"),
			"public."+indexName,
		).Scan(&published))
	})
	return published
}

// A copy of indexName under copyName with the same definition, as REINDEX
// CONCURRENTLY builds it before its swap.
func taskCopyIndexNative(t testing.TB, ctx context.Context, indexName string, copyName string) {
	t.Helper()
	var definition string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(to_regclass($1))`, "public."+indexName).Scan(&definition))
	})
	prefix := "CREATE INDEX " + indexName + " ON "
	if !strings.HasPrefix(definition, prefix) {
		t.Fatalf("unexpected definition of %s: %s", indexName, definition)
	}
	contractPartitionNativeExec(t, ctx, "CREATE INDEX "+copyName+" ON "+strings.TrimPrefix(definition, prefix))
}

// Plain indexes and their copies have no published index; after the cutover a
// legacy leaf, a weekly leaf and a copy of a leaf resolve to the partitioned
// index they belong to.
func TestTaskDbMaintenancePublishedIndexNative(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		const published = "transfer_escrow_unsettled_balance_contract"
		if !server.IsContractPartitioned(ctx) {
			taskCopyIndexNative(t, ctx, published, published+"_ccnew")
			for _, name := range []string{published, published + "_ccnew", "transfer_escrow_pkey"} {
				if got := taskPublishedIndexNative(t, ctx, name); got != "" {
					t.Fatalf("plain index %s resolved to %q", name, got)
				}
			}
			contractPartitionNativeExec(t, ctx, "DROP INDEX public."+published+"_ccnew")
			server.Testing_PartitionContracts(ctx)
		}

		legacyLeaf := published + "_l"
		if got := taskPublishedIndexNative(t, ctx, legacyLeaf); got != published {
			t.Fatalf("legacy leaf %s resolved to %q", legacyLeaf, got)
		}
		taskCopyIndexNative(t, ctx, legacyLeaf, legacyLeaf+"_ccnew")
		if got := taskPublishedIndexNative(t, ctx, legacyLeaf+"_ccnew"); got != published {
			t.Fatalf("the copy of %s resolved to %q", legacyLeaf, got)
		}

		var weeklyLeaf string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`
				SELECT leaf.relname
				FROM pg_inherits inheritance
				JOIN pg_class leaf ON leaf.oid = inheritance.inhrelid
				JOIN pg_index leaf_index ON leaf_index.indexrelid = leaf.oid
				JOIN pg_class leaf_table ON leaf_table.oid = leaf_index.indrelid
				WHERE inheritance.inhparent = to_regclass($1) AND leaf_table.relname <> 'transfer_escrow_legacy'
				ORDER BY leaf_table.relname
				LIMIT 1
				`,
				"public."+published,
			).Scan(&weeklyLeaf))
		})
		if got := taskPublishedIndexNative(t, ctx, weeklyLeaf); got != published {
			t.Fatalf("weekly leaf %s resolved to %q", weeklyLeaf, got)
		}
		if got := taskPublishedIndexNative(t, ctx, "account_payment_pkey"); got != "" {
			t.Fatalf("a plain table's index resolved to %q", got)
		}
		if !server.IsDbMaintenancePriorityIndex(taskPublishedIndexNative(t, ctx, legacyLeaf+"_ccnew")) {
			t.Fatal("the resolved index is not a priority index")
		}
	})
}
