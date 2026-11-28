// Plan checks for the write-path indexes of PARTITION-MAINTENANCE-PLAN.md
// Part B: each hot reader's own SQL, planned against the migrated schema.
package model

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

type writePathTestPlanNode struct {
	NodeType     string                  `json:"Node Type"`
	RelationName string                  `json:"Relation Name"`
	IndexName    string                  `json:"Index Name"`
	Plans        []writePathTestPlanNode `json:"Plans"`
}

// The indexes the plan of query reads, and the tables it scans sequentially.
// Sequential scans are off for the planning transaction only, so a test-sized
// table is planned the way a large one is. A partial index serves a reader only
// when the reader's predicate implies the index predicate; otherwise the
// reader still scans the table.
func writePathTestPlan(t testing.TB, ctx context.Context, query string, args ...any) (indexes map[string]bool, sequentialTables map[string]bool) {
	t.Helper()
	indexes = map[string]bool{}
	sequentialTables = map[string]bool{}
	server.Tx(ctx, func(tx server.PgTx) {
		clear(indexes)
		clear(sequentialTables)
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+query, args...).Scan(&raw))
		var plans []struct {
			Plan writePathTestPlanNode `json:"Plan"`
		}
		server.Raise(json.Unmarshal(raw, &plans))
		var walk func(node writePathTestPlanNode)
		walk = func(node writePathTestPlanNode) {
			if node.IndexName != "" {
				indexes[node.IndexName] = true
			}
			if node.NodeType == "Seq Scan" {
				sequentialTables[node.RelationName] = true
			}
			for _, child := range node.Plans {
				walk(child)
			}
		}
		for _, plan := range plans {
			walk(plan.Plan)
		}
	})
	return
}

// The indexes the plan of query reads (writePathTestPlan).
func writePathTestPlanIndexes(t testing.TB, ctx context.Context, query string, args ...any) map[string]bool {
	t.Helper()
	indexes, _ := writePathTestPlan(t, ctx, query, args...)
	return indexes
}

// The plan's index names in order, for failure messages.
func writePathTestIndexNames(indexes map[string]bool) []string {
	names := make([]string, 0, len(indexes))
	for name := range indexes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// The plan's indexes with each partition's index named by the partitioned
// index it belongs to, so a plan reads the same on the plain journal and on
// its daily partitions (server.MigrateTransferDebitJournalToPartitions).
func writePathTestRootIndexes(t testing.TB, ctx context.Context, indexes map[string]bool) map[string]bool {
	t.Helper()
	roots := map[string]bool{}
	for name := range indexes {
		var root string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT coalesce(pg_partition_root(to_regclass($1))::regclass::text, $1)`, "public."+name).Scan(&root))
		})
		roots[strings.TrimPrefix(root, "public.")] = true
	}
	return roots
}

// The flush's oldest-age gauge reads a shard's journal through the
// (shard, create_time) index, oldest first, in either applied state. No index
// keys applied any more. On the daily partitions it reads each partition's
// copy of that index.
func TestTransferDebitOldestAgeReadsTheShardTimeIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, applied := range []bool{false, true} {
			indexes := writePathTestRootIndexes(t, ctx, writePathTestPlanIndexes(t, ctx, transferDebitOldestAgeSql, 3, applied))
			if len(indexes) != 1 || !indexes["transfer_debit_journal_shard_create_time"] {
				t.Fatalf("applied=%t: the age gauge reads %v, want transfer_debit_journal_shard_create_time", applied, writePathTestIndexNames(indexes))
			}
		}
	})
}
