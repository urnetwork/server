// Runs the stats-landmine and vacuum-health reads (SIGNALS.md §2.3 and §2.4)
// on the PostgreSQL 18 TestEnv before and after the contract partition
// cutover.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Executes a monitor statement as is, rendering every cell as text the way
// psql does.
type postgresNativeSource struct {
	*syntheticSource
	t testing.TB
}

func (self *postgresNativeSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	return postgresNativeRows(self.t, ctx, query), nil
}

func postgresNativeRows(t testing.TB, ctx context.Context, query string) []Row {
	t.Helper()
	rows := []Row{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, query, pgx.QueryExecModeSimpleProtocol)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				values, err := result.Values()
				server.Raise(err)
				row := make(Row, len(values))
				for i, value := range values {
					if value != nil {
						row[i] = fmt.Sprint(value)
					}
				}
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// The parent's open-column target is 300 after the migrations; partitioned,
// every leaf copies it and the partitioned indexes count as the three
// structural shapes; a leaf with another target fires with its name.
func TestStatsLandmineNativePartitionTargets(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		read := func() Row {
			rows := postgresNativeRows(t, ctx, pgStatsLandmineSQL)
			if len(rows) != 1 || len(rows[0]) != 7 {
				t.Fatalf("unexpected stats-landmine frame: %v", rows)
			}
			return rows[0]
		}
		run := func() Alerts {
			settings := syntheticSettings(&postgresNativeSource{syntheticSource: &syntheticSource{}, t: t})
			settings.Now = time.Now
			alerts, err := NewPlannerFlipsSignal().Run(ctx, settings)
			if err != nil {
				t.Fatal(err)
			}
			return alerts
		}
		if !server.IsContractPartitioned(ctx) {
			if row := read(); row[2] != "3" || row[3] != "300" || row[4] != "0" || row[5] != "0" || row[6] != "" {
				t.Fatalf("plain stats-landmine row %v", row)
			}
			if alerts := run(); len(alerts) != 0 {
				t.Fatalf("the migrated plain table alerted: %s", alerts.Markdown())
			}
			server.Testing_PartitionContracts(ctx)
		}
		row := read()
		if row[2] != "3" || row[3] != "300" || row[4] == "0" || row[5] != "0" || row[6] != "" {
			t.Fatalf("partitioned stats-landmine row %v", row)
		}
		if alerts := run(); len(alerts) != 0 {
			t.Fatalf("the partitioned table alerted: %s", alerts.Markdown())
		}
		leaf := server.ContractPartitionName("transfer_contract", server.ContractPartitionWeekStart(server.NowUtc()).Add(14*24*time.Hour))
		contractPartitionNativeExec(t, ctx, "ALTER TABLE public."+leaf+" ALTER COLUMN open SET STATISTICS 100")
		if row := read(); row[5] != "1" || row[6] != leaf+"=100" {
			t.Fatalf("a leaf target change was not read: %v", row)
		}
		alert := contractPartitionNativeAlert(t, run(), "stats-landmine")
		if !strings.Contains(alert.Observed, "partition_target_mismatches=1 first_partition_target_mismatch="+leaf+"=100") {
			t.Fatalf("the leaf target change did not fire: %s", alert.Observed)
		}
	})
}

// Dead tuples in a contract partition report under the contract table with the
// table's total; a plain table reports under its own name. The statement runs
// with its 10M floor lowered so a test's churn crosses it.
func TestVacuumHealthNativeGroupsContractPartitions(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		query := strings.ReplaceAll(pgVacuumHealthSQL, "10000000", "100")
		churn := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count)
					SELECT (substr(replace(contract_id_floor(clock_timestamp())::text, '-', ''), 1, 12) || substr(md5(n::text || random()::text), 1, 20))::uuid,
					       gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 1
					FROM generate_series(1, 2000) n
					`,
				))
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE transfer_byte_count = 1`))
			})
		}
		// The deleting backend reports its counts when it goes idle, at most
		// a few seconds later.
		await := func(match func(Row) bool) Row {
			deadline := time.Now().Add(30 * time.Second)
			for {
				for _, row := range postgresNativeRows(t, ctx, query) {
					if match(row) {
						return row
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("the dead tuples never reached the vacuum read")
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
		if !server.IsContractPartitioned(ctx) {
			churn()
			row := await(func(row Row) bool { return row[0] == "transfer_contract" })
			if len(row) != 25 || row[21] != "transfer_contract" || row[23] != "1" {
				t.Fatalf("plain vacuum row %v", row)
			}
			server.Testing_PartitionContracts(ctx)
		}
		// ids of the current millisecond land in the current week's partition;
		// a plain table's dead tuples stay with it as the legacy partition
		churn()
		var leaves string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)::text FROM pg_partition_tree('public.transfer_contract'::regclass) WHERE isleaf`).Scan(&leaves))
		})
		row := await(func(row Row) bool { return row[21] == "transfer_contract" && row[0] != "transfer_contract" })
		if table, _, _, ok := server.ParseContractPartitionName(row[0]); !ok || table != "transfer_contract" || row[23] != leaves {
			t.Fatalf("partitioned vacuum row %v, want a transfer_contract partition of %s", row, leaves)
		}
		for _, other := range postgresNativeRows(t, ctx, query) {
			if table, _, _, ok := server.ParseContractPartitionName(other[0]); ok && other[21] != table {
				t.Fatalf("a contract partition reported under %s: %v", other[21], other)
			}
		}
	})
}

// The unchanged data reads of contract tables (the open-contract set, the
// creation rate, contracts to inactive destinations) run on the partitioned
// parents as on the plain tables: each signal completes a real read.
func TestContractDataReadsNativeOnBothLayouts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		observe := func(layout string) {
			for _, signal := range []Signal{NewOpenContractsSignal(), NewContractRateSignal(), NewStaleContractsSignal()} {
				reads := 0
				source := &postgresNativeCountingSource{postgresNativeSource: postgresNativeSource{syntheticSource: &syntheticSource{}, t: t}, reads: &reads}
				settings := syntheticSettings(source)
				settings.Now = time.Now
				if _, err := signal.Run(ctx, settings); err != nil || reads == 0 {
					t.Fatalf("%s: %s did not complete a read: %v", layout, signal.Key(), err)
				}
			}
		}
		if !server.IsContractPartitioned(ctx) {
			observe("plain")
			server.Testing_PartitionContracts(ctx)
		}
		observe("partitioned")
	})
}

// Counts the reads the signal makes.
type postgresNativeCountingSource struct {
	postgresNativeSource
	reads *int
}

func (self *postgresNativeCountingSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	*self.reads += 1
	return self.postgresNativeSource.PostgreSQL(ctx, query)
}
