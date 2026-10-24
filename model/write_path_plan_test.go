// Plan checks for the write-path indexes of PARTITION-MAINTENANCE-PLAN.md
// Part B: each hot reader's own SQL, planned against the migrated schema.
package model

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/urnetwork/server"
)

type writePathTestPlanNode struct {
	NodeType  string                  `json:"Node Type"`
	IndexName string                  `json:"Index Name"`
	Plans     []writePathTestPlanNode `json:"Plans"`
}

// The indexes the plan of query reads. Sequential scans are off for the
// planning transaction only, so a test-sized table is planned the way a large
// one is; a reader whose predicate does not imply an index's predicate still
// cannot use that index.
func writePathTestPlanIndexes(t testing.TB, ctx context.Context, query string, args ...any) map[string]bool {
	t.Helper()
	indexes := map[string]bool{}
	server.Tx(ctx, func(tx server.PgTx) {
		clear(indexes)
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
			for _, child := range node.Plans {
				walk(child)
			}
		}
		for _, plan := range plans {
			walk(plan.Plan)
		}
	})
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

// The flush's oldest-age gauge reads a shard's journal through the
// (shard, create_time) index, oldest first, in either applied state. No index
// keys applied any more.
func TestTransferDebitOldestAgeReadsTheShardTimeIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, applied := range []bool{false, true} {
			indexes := writePathTestPlanIndexes(t, ctx, transferDebitOldestAgeSql, 3, applied)
			if len(indexes) != 1 || !indexes["transfer_debit_journal_shard_create_time"] {
				t.Fatalf("applied=%t: the age gauge reads %v, want transfer_debit_journal_shard_create_time", applied, writePathTestIndexNames(indexes))
			}
		}
	})
}
