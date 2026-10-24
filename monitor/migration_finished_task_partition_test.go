// finished_task's three migration contracts: their versions and lifetimes,
// and, against PostgreSQL, their reading at each migration prefix, on the
// plain table, through a prepared cutover and on the partitioned layout.
package monitor

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The migrations' versions, in list order.
func finishedTaskPartitionVersions() []int {
	return []int{
		server.FinishedTaskIdentityIndexMigrationVersion(),
		server.FinishedTaskPrimaryKeyMigrationVersion(),
		server.FinishedTaskPostRetryIndexMigrationVersion(),
	}
}

// The three contracts sit at their migrations' consecutive versions, each in
// its positional column. The unique index's contract ends where the primary
// key takes the index over, and the key's and the failed Post index's never
// end. Each reads the catalogs directly; the failed Post index accepts the
// partitioned parent's ON ONLY form. Once the suffix reaches these versions,
// it carries exactly these contracts.
func TestMigrationsFinishedTaskPartitionContracts(t *testing.T) {
	versions := finishedTaskPartitionVersions()
	contracts := finishedTaskPartitionContracts()
	if len(contracts) != len(versions) {
		t.Fatalf("%d contracts for %d migrations", len(contracts), len(versions))
	}
	queries := []string{finishedTaskIdentityIndexArtifactQuery, finishedTaskPrimaryKeyArtifactQuery, finishedTaskPostRetryIndexArtifactQuery}
	for index, contract := range contracts {
		version := versions[index]
		if version <= 0 || (0 < index && version != versions[index-1]+1) {
			t.Fatalf("migration versions %v are not consecutive", versions)
		}
		if contract.artifact.requiredVersion != version || contract.artifact.rowColumn != version-589 || contract.query != queries[index] {
			t.Fatalf("contract %d is %+v", index, contract.artifact)
		}
		if strings.Contains(contract.query, "index_artifact") || strings.Contains(contract.query, "constraint_artifact") ||
			!strings.Contains(contract.query, "to_regclass('public.finished_task')") || strings.Contains(contract.query, ";") {
			t.Fatalf("contract %d does not read the catalogs directly:\n%s", index, contract.query)
		}
	}
	if contracts[0].artifact.removedVersion != versions[1] || contracts[1].artifact.removedVersion != 0 || contracts[2].artifact.removedVersion != 0 {
		t.Fatalf("contract lifetimes %+v %+v %+v", contracts[0].artifact, contracts[1].artifact, contracts[2].artifact)
	}
	for _, want := range []struct {
		query string
		text  string
	}{
		{finishedTaskIdentityIndexArtifactQuery, "'CREATE UNIQUE INDEX finished_task_task_id_run_end_time ON public.finished_task USING btree (task_id, run_end_time)'"},
		{finishedTaskPrimaryKeyArtifactQuery, "'PRIMARY KEY (task_id, run_end_time)'"},
		{finishedTaskPostRetryIndexArtifactQuery, `'^(CREATE INDEX \S+ ON )ONLY '`},
		{finishedTaskPostRetryIndexArtifactQuery, "'CREATE INDEX finished_task_post_retry ON public.finished_task USING btree (task_id) WHERE ((post_error IS NOT NULL) AND (NOT post_completed))'"},
	} {
		if !strings.Contains(want.query, want.text) {
			t.Fatalf("a contract lacks %s", want.text)
		}
	}
	if strings.Contains(finishedTaskPrimaryKeyArtifactQuery, "finished_task_pkey") {
		t.Fatal("the primary key's contract pins its index's name")
	}
	if last := migrationCurrentSuffixContracts[len(migrationCurrentSuffixContracts)-1]; versions[len(versions)-1] <= last.artifact.requiredVersion {
		for index, version := range versions {
			wired := migrationCurrentSuffixTestContract(t, version)
			if wired.artifact != contracts[index].artifact || wired.query != contracts[index].query {
				t.Fatalf("the suffix carries %+v at version %d, not the finished_task contract", wired.artifact, version)
			}
		}
	}
}

// Whether a contract's predicate holds through querier, a connection or a
// transaction.
func finishedTaskContractPresent(t testing.TB, ctx context.Context, querier server.PgQuerier, query string) (present bool) {
	t.Helper()
	server.Raise(querier.QueryRow(ctx, "SELECT "+query).Scan(&present))
	return
}

// Each contract against the database's catalogs, by connection.
func finishedTaskContractsPresent(t testing.TB, ctx context.Context) (present []bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		for _, contract := range finishedTaskPartitionContracts() {
			present = append(present, finishedTaskContractPresent(t, ctx, conn, contract.query))
		}
	})
	return
}

// Each migration prefix reads as the contracts' lifetimes say: none before the
// unique index, the index alone at its version, then the primary key in its
// place, then the failed Post index too. Before its migration each contract is
// false, never an error.
func TestMigrationsFinishedTaskPartitionContractsAtEachPrefix(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		versions := finishedTaskPartitionVersions()
		for _, head := range []int{versions[0] - 1, versions[0], versions[1], versions[2]} {
			server.ApplyDbMigrationsUpTo(ctx, head)
			want := []bool{
				versions[0] <= head && head < versions[1],
				versions[1] <= head,
				versions[2] <= head,
			}
			if present := finishedTaskContractsPresent(t, ctx); !slices.Equal(present, want) {
				t.Fatalf("at version %d the contracts read %v, want %v", head, present, want)
			}
		}
	})
}

// The published contracts after a merge wires them, by their artifact names.
func finishedTaskPartitionArtifactNames() (names []string) {
	for _, contract := range finishedTaskPartitionContracts() {
		names = append(names, fmt.Sprintf("%s@v%d", contract.artifact.name, contract.artifact.requiredVersion))
	}
	return
}

// Whether the coherence query carries the finished_task contracts yet.
func finishedTaskPartitionContractsWired() bool {
	versions := finishedTaskPartitionVersions()
	last := migrationCurrentSuffixContracts[len(migrationCurrentSuffixContracts)-1]
	return versions[len(versions)-1] <= last.artifact.requiredVersion
}

// On a database migrated to the head, the key and the failed Post index read
// present, and the unique index, renamed into the key, reads absent, which is
// no drift past its removal. They keep reading so through a prepared cutover
// and on the partitioned layout (URNETWORK_TEST_FINISHED_TASK_PARTITIONS=1
// starts there), and the coherence signal reports no drift in any of the
// three. On the partitioned layout a dropped failed Post index or primary key
// reads false, and, once the contracts are wired, reaches its own artifact.
func TestMigrationsFinishedTaskPartitionContractsInEveryLayout(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		head := []bool{false, true, true}
		requireLayout := func(layout string) {
			if present := finishedTaskContractsPresent(t, ctx); !slices.Equal(present, head) {
				t.Fatalf("the %s layout reads %v, want %v", layout, present, head)
			}
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatalf("the %s layout reports drift: %s", layout, drift)
			}
		}
		if !server.IsFinishedTaskPartitioned(ctx) {
			requireLayout("plain")
			cutoverDay := server.PartitionGranularityDaily.AddRanges(server.PartitionGranularityDaily.RangeStart(server.NowUtc()), 2)
			logf := func(format string, args ...any) { t.Logf(format, args...) }
			if err := server.MigrateFinishedTasksToPartitions(ctx, server.FinishedTaskPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: logf}); err != nil {
				t.Fatal(err)
			}
			requireLayout("prepared")
			if err := server.MigrateFinishedTasksToPartitions(ctx, server.FinishedTaskPartitionMigrateOptions{Abort: true, Logf: logf}); err != nil {
				t.Fatal(err)
			}
			server.Testing_PartitionFinishedTasks(ctx, server.TestingFinishedTaskCutoverDay(server.NowUtc()))
		}
		if !server.IsFinishedTaskPartitioned(ctx) {
			t.Fatal("the database did not cut over")
		}
		requireLayout("partitioned")

		names := finishedTaskPartitionArtifactNames()
		for _, fault := range []struct {
			name     string
			sql      string
			contract int
		}{
			{name: "failed Post index dropped", sql: `DROP INDEX finished_task_post_retry`, contract: 2},
			{name: "primary key dropped", sql: `ALTER TABLE finished_task DROP CONSTRAINT finished_task_pkey`, contract: 1},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				if finishedTaskContractPresent(t, ctx, tx, finishedTaskPartitionContracts()[fault.contract].query) {
					t.Fatalf("%s still reads present", fault.name)
				}
				if !finishedTaskPartitionContractsWired() {
					return
				}
				drift, err := migrationContractTxDrift(ctx, tx)
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				if !strings.Contains(drift, names[fault.contract]) {
					t.Fatalf("%s did not reach %s: %s", fault.name, names[fault.contract], drift)
				}
			})
		}
		requireLayout("partitioned, after the rolled-back faults")
	})
}
