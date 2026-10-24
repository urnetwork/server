// The URL/ARIN migration contracts on provider_egress_health_history once its
// operator cutover partitions it by day (PARTITION-MAINTENANCE-PLAN.md C2).
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The partition-aware forms cover the history: its table check admits a
// partitioned parent of durable leaves, its index definitions are read
// without ONLY, and its receipt identity takes either key.
func TestMigrationFp2HistoryPartitionForms(t *testing.T) {
	if !strings.Contains(migrationFp2Table(migrationEgressHealthHistoryTable), "relkind='p'") {
		t.Fatal("the history is not read as a possibly partitioned table")
	}
	if !strings.Contains(migrationPartitionableTablesSql, "'provider_egress_health_history'") {
		t.Fatal("the history's partitioned indexes keep ONLY")
	}
	for _, table := range server.ContractPartitionTables {
		if !strings.Contains(migrationPartitionableTablesSql, "'"+table+"'") {
			t.Fatalf("%s left the partitionable tables", table)
		}
	}
	for _, want := range []string{"PRIMARY KEY (run_id)", "PRIMARY KEY (run_id, measured_at)", "USING btree (run_id)", "USING btree (run_id, measured_at)"} {
		if !strings.Contains(migrationEgressHealthHistoryKey, want) {
			t.Fatalf("the receipt identity lost %q", want)
		}
	}
	if !strings.Contains(strings.Join(migrationFp2ArtifactQueries, " "), migrationEgressHealthHistoryKey) {
		t.Fatal("the v723 contract does not use the layout-aware identity")
	}
}

// After a real cutover every FP2 contract that held on the plain history still
// holds, and so does the measured-quota index contract.
func TestMigrationFp2CatalogAcceptsPartitionedHistory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := migrationFp2TestQuery(t)
		read := func() []bool {
			var matches []bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, query).Scan(&matches))
			})
			return matches
		}
		before := read()
		now := server.NowUtc()
		for _, options := range []model.ProviderEgressHealthHistoryCutoverOptions{{}, {Swap: true}} {
			options.Logf = t.Logf
			if err := model.MigrateProviderEgressHealthHistoryToPartitions(ctx, now, options); err != nil {
				t.Fatal(err)
			}
		}
		var partitioned bool
		server.Db(ctx, func(conn server.PgConn) {
			// the contract's layout test is an expression, so it is selected
			server.Raise(conn.QueryRow(ctx, "SELECT "+migrationEgressHealthHistoryPartitionedSql).Scan(&partitioned))
		})
		if !partitioned {
			t.Fatal("the cutover did not partition the history")
		}
		after := read()
		for index := range before {
			if before[index] != after[index] {
				t.Fatalf("v%d contract changed across the cutover: %t then %t", index+722, before[index], after[index])
			}
		}
		var quota bool
		server.Db(ctx, func(conn server.PgConn) {
			prefix := query[:strings.LastIndex(query, "SELECT ARRAY[")]
			server.Raise(conn.QueryRow(ctx, prefix+"SELECT "+migrationUrlProbeMeasuredQuotaArtifactQuery).Scan(&quota))
		})
		if !quota {
			t.Fatal("the measured-quota index contract failed on the partitioned history")
		}
	})
}
