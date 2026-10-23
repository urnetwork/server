package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// A contract id from the millisecond of at.
func testContractIdAt(at time.Time) Id {
	id := NewId()
	floor := ContractIdFloor(at)
	copy(id[0:6], floor[0:6])
	return id
}

func testContractPartitionScalar[T any](t testing.TB, ctx context.Context, sql string, args ...any) (value T) {
	MaintenanceDb(ctx, func(conn PgConn) {
		Raise(conn.QueryRow(ctx, sql, args...).Scan(&value))
	})
	return
}

// One contract with a row in every contract table: escrow, close, sweep,
// report and extender. A settled contract carries its usage proof.
func testInsertContractRows(t testing.TB, ctx context.Context, contractId Id, settled bool) {
	Tx(ctx, func(tx PgTx) {
		closeTime := any(nil)
		outcome := any(nil)
		usage := any(nil)
		if settled {
			closeTime = NowUtc()
			outcome = "settled"
			usage = `{"version":1,"byte_count":1,"providers":[]}`
		}
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, close_time, outcome, provider_usage, payer_network_id
			) VALUES ($1, $2, $3, $4, $5, 1024, $6, $7, $8, $2)
			`,
			contractId, NewId(), NewId(), NewId(), NewId(), closeTime, outcome, usage,
		))
		balanceId := NewId()
		RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count, settled) VALUES ($1, $2, 1024, $3)`,
			contractId, balanceId, settled,
		))
		RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO contract_close (contract_id, party, used_transfer_byte_count) VALUES ($1, 'source', 1)`,
			contractId,
		))
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO transfer_escrow_sweep (contract_id, balance_id, network_id, payout_byte_count, payout_net_revenue_nano_cents)
			VALUES ($1, $2, $3, 1, 1)
			`,
			contractId, balanceId, NewId(),
		))
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_close_report (contract_id, party, report_id, used_transfer_byte_count, checkpoint)
			VALUES ($1, 'source', $2, 1, false)
			`,
			contractId, NewId(),
		))
		RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO contract_extender (contract_id, extender_id, party, client_id, network_id)
			VALUES ($1, $2, 'source', $3, $4)
			`,
			contractId, NewId(), NewId(), NewId(),
		))
	})
}

// The partition of table that holds contractId, by its physical relation.
func testContractPartitionOf(t testing.TB, ctx context.Context, table string, contractId Id) string {
	return testContractPartitionScalar[string](
		t,
		ctx,
		fmt.Sprintf(`SELECT tableoid::regclass::text FROM public.%s WHERE contract_id = $1 LIMIT 1`, quoteIdentifier(table)),
		contractId,
	)
}

// The failure of an expected error, without logging its stack.
func testContractPartitionRecover(callback func()) (err any) {
	defer func() {
		err = recover()
	}()
	callback()
	return
}

func testContractPartitionLogf(t testing.TB) func(string, ...any) {
	return func(format string, args ...any) {
		t.Logf(format, args...)
	}
}

// Exercises the cutover lifecycle against PostgreSQL with the migrated schema:
// dry runs change nothing, prepare is resumable, the swap keeps every existing
// row, index name, trigger and outside foreign key, new contracts route by id,
// and rollback and abort restore the plain tables.
func TestContractPartitionCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if IsContractPartitioned(ctx) {
			t.Skip("the cutover starts from plain contract tables")
		}
		logf := testContractPartitionLogf(t)
		now := NowUtc()
		cutoverWeek := ContractPartitionWeekStart(now).Add(2 * contractPartitionWeek)

		openContractId := NewId()
		settledContractId := NewId()
		testInsertContractRows(t, ctx, openContractId, false)
		testInsertContractRows(t, ctx, settledContractId, true)

		relkind := func(name string) string {
			return testContractPartitionScalar[string](t, ctx, `SELECT COALESCE((SELECT relkind::text FROM pg_class WHERE oid = to_regclass($1)), '')`, "public."+name)
		}
		indexDefinitions := func(table string) map[string]string {
			definitions := map[string]string{}
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(ctx, `SELECT c.relname, pg_get_indexdef(i.indexrelid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE i.indrelid = to_regclass($1)`, "public."+table)
				WithPgResult(result, err, func() {
					for result.Next() {
						var name, definition string
						Raise(result.Scan(&name, &definition))
						definitions[name] = contractIndexShape(definition)
					}
				})
			})
			return definitions
		}
		triggerNames := func(table string) []string {
			var names []string
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(ctx, `SELECT tgname FROM pg_trigger WHERE tgrelid = to_regclass($1) AND NOT tgisinternal ORDER BY tgname`, "public."+table)
				WithPgResult(result, err, func() {
					for result.Next() {
						var name string
						Raise(result.Scan(&name))
						names = append(names, name)
					}
				})
			})
			return names
		}
		foreignKeyValid := func(name string) (exists bool, valid bool) {
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = $1 AND contype = 'f' AND conparentid = 0`, name)
				WithPgResult(result, err, func() {
					if result.Next() {
						Raise(result.Scan(&valid))
						exists = true
					}
				})
			})
			return
		}

		plainIndexes := map[string]map[string]string{}
		plainTriggers := map[string][]string{}
		for _, table := range ContractPartitionTables {
			plainIndexes[table] = indexDefinitions(table)
			plainTriggers[table] = triggerNames(table)
		}

		// dry runs change nothing
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, DryRun: true, Logf: logf}), nil)
		connect.AssertEqual(t, relkind("transfer_contract_p"), "")
		_, prepared := preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, false)

		// a cutover week must start on a Monday and leave a day to prepare
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek.Add(time.Hour), Logf: logf}) != nil, true)
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: ContractPartitionWeekStart(now), Logf: logf}) != nil, true)

		// finalize refuses an unprepared cutover
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Finalize: true, Logf: logf}) != nil, true)

		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: logf}), nil)
		week, prepared := preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, true)
		connect.AssertEqual(t, week, cutoverWeek)
		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, relkind(contractStagedParentName(table)), "p")
			for i := 0; i <= ContractPartitionAheadWeeks; i += 1 {
				connect.AssertEqual(t, relkind(ContractPartitionName(table, cutoverWeek.Add(time.Duration(i)*contractPartitionWeek))), "r")
			}
		}
		// resumable
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: logf}), nil)
		// a different week needs an abort first
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek.Add(contractPartitionWeek), Logf: logf}) != nil, true)
		// the prepared bound still admits current contracts
		laterContractId := NewId()
		testInsertContractRows(t, ctx, laterContractId, false)

		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Finalize: true, DryRun: true, Logf: logf}), nil)
		connect.AssertEqual(t, IsContractPartitioned(ctx), false)
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Finalize: true, Logf: logf}), nil)
		connect.AssertEqual(t, IsContractPartitioned(ctx), true)

		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, relkind(table), "p")
			connect.AssertEqual(t, relkind(ContractLegacyPartitionName(table)), "r")
			connect.AssertEqual(t, relkind(contractStagedParentName(table)), "")
			// the parent's indexes carry the published names and definitions
			connect.AssertEqual(t, indexDefinitions(table), plainIndexes[table])
			legacyIndexes := indexDefinitions(ContractLegacyPartitionName(table))
			for name, definition := range plainIndexes[table] {
				connect.AssertEqual(t, legacyIndexes[name+contractLegacyIndexSuffix], definition)
			}
			// every trigger moved to the parent
			connect.AssertEqual(t, triggerNames(table), plainTriggers[table])
		}
		// rows from before the swap stay in the legacy partitions
		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, testContractPartitionOf(t, ctx, table, openContractId), ContractLegacyPartitionName(table))
			connect.AssertEqual(t, testContractPartitionOf(t, ctx, table, laterContractId), ContractLegacyPartitionName(table))
		}
		// a contract id of the cutover week routes to its weekly partition
		weeklyContractId := testContractIdAt(cutoverWeek.Add(time.Hour))
		testInsertContractRows(t, ctx, weeklyContractId, true)
		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, testContractPartitionOf(t, ctx, table, weeklyContractId), ContractPartitionName(table, cutoverWeek))
		}
		// past the last partition an insert fails
		farContractId := testContractIdAt(cutoverWeek.Add(time.Duration(ContractPartitionAheadWeeks+1) * contractPartitionWeek))
		connect.AssertEqual(t, testContractPartitionRecover(func() {
			testInsertContractRows(t, ctx, farContractId, false)
		}) != nil, true)

		// the usage guard fires through the parent on both kinds of partition
		for _, contractId := range []Id{testContractIdAt(cutoverWeek.Add(2 * time.Hour)), NewId()} {
			err := testContractPartitionRecover(func() {
				Tx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(
						ctx,
						`
						INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count, outcome, close_time)
						VALUES ($1, $2, $2, $2, $2, 1, 'settled', now())
						`,
						contractId, NewId(),
					))
				})
			})
			connect.AssertEqual(t, err != nil && strings.Contains(fmt.Sprint(err), "requires immutable provider usage"), true)
		}

		// deleting a settled contract archives its usage proof in both kinds of
		// partition
		for _, contractId := range []Id{settledContractId, weeklyContractId} {
			Tx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id = $1`, contractId))
			})
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id = $1)`, contractId), true)
		}

		// the report foreign key is gone; the outside one is valid
		exists, _ := foreignKeyValid(contractCloseReportForeignKeyName)
		connect.AssertEqual(t, exists, false)
		exists, valid := foreignKeyValid("legacy_settlement_intent_contract_id_fkey")
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, valid, true)

		// a TRUNCATE that names a partition is refused
		for _, name := range []string{ContractLegacyPartitionName("transfer_contract"), ContractPartitionName("transfer_contract", cutoverWeek), ContractLegacyPartitionName("transfer_escrow")} {
			err := testContractPartitionRecover(func() {
				MaintenanceTx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`TRUNCATE public.%s`, quoteIdentifier(name))))
				}, OptNoRetry())
			})
			connect.AssertEqual(t, err != nil, true)
		}

		// before the cutover week the legacy partition covers the weeks ahead
		// of it: maintenance creates nothing there and counts them as covered
		report, _ := MaintainContractPartitions(ctx, now, ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, len(report.Created), 0)
		connect.AssertEqual(t, ContractPartitionAheadWeeks <= report.FutureWeeks, true)

		// rollback refuses while a weekly partition holds a row
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Rollback: true, Logf: logf}) != nil, true)
		Tx(ctx, func(tx PgTx) {
			for _, table := range []string{"contract_extender", "contract_close_report", "transfer_escrow_sweep", "contract_close", "transfer_escrow"} {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`DELETE FROM public.%s WHERE contract_id = $1`, quoteIdentifier(table)), weeklyContractId))
			}
		})
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Rollback: true, Logf: logf}), nil)
		connect.AssertEqual(t, IsContractPartitioned(ctx), false)
		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, relkind(table), "r")
			connect.AssertEqual(t, relkind(ContractLegacyPartitionName(table)), "")
			connect.AssertEqual(t, indexDefinitions(table), plainIndexes[table])
			connect.AssertEqual(t, triggerNames(table), plainTriggers[table])
		}
		exists, valid = foreignKeyValid(contractCloseReportForeignKeyName)
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, valid, false)
		exists, valid = foreignKeyValid("legacy_settlement_intent_contract_id_fkey")
		connect.AssertEqual(t, exists, true)
		connect.AssertEqual(t, valid, true)
		connect.AssertEqual(t, testContractPartitionOf(t, ctx, "transfer_contract", openContractId), "transfer_contract")
		// new contracts are admitted again
		testInsertContractRows(t, ctx, NewId(), false)

		// prepare then abort leaves the plain tables
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverWeek: cutoverWeek, Logf: logf}), nil)
		connect.AssertEqual(t, MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Abort: true, Logf: logf}), nil)
		_, prepared = preparedContractCutoverWeek(ctx)
		connect.AssertEqual(t, prepared, false)
		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, relkind(contractStagedParentName(table)), "")
		}
	})
}
