// The contract tables' published artifacts in both layouts: plain as migrated,
// and partitioned after the cutover (server.Testing_PartitionContracts). Each
// test builds its own layout, so it means the same with or without
// URNETWORK_TEST_CONTRACT_PARTITIONS.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The partition-aware forms apply to exactly the six contract tables: every
// other table keeps its published relkind check byte for byte, including the
// close evidence table and the contract partitions, which stay plain.
func TestMigrationsContractPartitionFormsCoverOnlyTheContractTables(t *testing.T) {
	for _, table := range server.ContractPartitionTables {
		if !isMigrationContractTable(table) || !strings.Contains(migrationFp2Table(table), "relkind='p'") {
			t.Fatalf("%s is not read as a possibly partitioned contract table", table)
		}
		if !strings.Contains(migrationContractTablesSql, "'"+table+"'") {
			t.Fatalf("%s is missing from the normalized index tables %s", table, migrationContractTablesSql)
		}
	}
	for _, table := range []string{
		"contract_close_report_evidence",
		"legacy_settlement_intent",
		"network_extender_release_request",
		server.ContractLegacyPartitionName("transfer_contract"),
		server.ContractPartitionName("contract_close_report", server.ContractPartitionWeekStart(server.NowUtc())),
	} {
		if isMigrationContractTable(table) {
			t.Fatalf("%s is read as a contract table", table)
		}
		want := fmt.Sprintf(`EXISTS (SELECT 1 FROM pg_class WHERE oid=to_regclass('public.%s')
		AND relkind='r' AND relpersistence='p')`, table)
		if actual := migrationFp2Table(table); actual != want {
			t.Fatalf("%s relkind check changed: %s", table, actual)
		}
	}
}

// A database migrated to the head by the test itself, so it stays plain until
// the test partitions it.
func migrationContractTestEnv() *server.TestEnv {
	environment := snMainnetMigrationTestEnv()
	environment.ApplyDbMigrations = false
	return environment
}

// The drift findings of the migration signal reading through tx.
func migrationContractTxDrift(ctx context.Context, tx server.PgTx) (string, error) {
	alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
	if err != nil {
		return "", err
	}
	drift := ""
	for _, alert := range alerts {
		if alert.Class == "migration-schema-drift" {
			drift = alert.Markdown()
		}
	}
	return drift, nil
}

// After the cutover every published artifact still reads present, including
// the indexes the parents render ON ONLY, the statistics settings that moved
// to the leaves and the close reports without their cascade key. Each real
// fault of the partitioned layout still reaches its owning artifact: a
// dropped, reshaped or never-attached parent index, a disabled parent trigger,
// a guard disabled or missing on one partition, a leaf without the analyze
// settings, and a restored cascade key or a stray index or trigger on a report
// partition. The retired legacy settlement key's clones are checked before its
// retirement (TestMigrationsContractPartitionedLegacySettlementKeyClones).
func TestMigrationsContractPartitionedCatalog(t *testing.T) {
	migrationContractTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrations(ctx)
		server.Testing_PartitionContracts(ctx)
		if !server.IsContractPartitioned(ctx) {
			t.Fatal("the cutover left the contract tables plain")
		}
		row, drift := migrationPingDatabaseCheck(t, ctx)
		if drift != "" {
			t.Fatalf("partitioned contract tables report drift at the head: %s", drift)
		}
		head := server.MigrationCount()
		for _, artifact := range migrationArtifacts {
			if artifact.removedVersion != 0 && artifact.removedVersion <= head {
				continue
			}
			if !migrationBool(row[artifact.rowColumn]) {
				t.Fatalf("%s@v%d reads absent on the partitioned layout", artifact.name, artifact.requiredVersion)
			}
		}
		// only the contract tables' parent indexes lose ONLY
		server.Db(ctx, func(conn server.PgConn) {
			for index, want := range map[string]string{
				"transfer_contract_stream_id":                 "CREATE INDEX transfer_contract_stream_id ON public.transfer_contract USING btree (stream_id) WHERE (stream_id IS NOT NULL)",
				"contract_close_report_pkey":                  "CREATE UNIQUE INDEX contract_close_report_pkey ON public.contract_close_report USING btree (contract_id, party, report_id)",
				"network_ping_target_extender_id_create_time": "CREATE INDEX network_ping_target_extender_id_create_time ON ONLY public.network_ping USING btree (target_extender_id, create_time)",
			} {
				var definition string
				server.Raise(conn.QueryRow(ctx, `SELECT `+migrationIndexDefinitionSql("index_relation", "table_relation.relname")+`
 FROM pg_index index_record
 JOIN pg_class index_relation ON index_relation.oid=index_record.indexrelid
 JOIN pg_class table_relation ON table_relation.oid=index_record.indrelid
 WHERE index_record.indexrelid=to_regclass('public.'||$1)`, index).Scan(&definition))
				if definition != want {
					t.Fatalf("%s reads %q, want %q", index, definition, want)
				}
			}
		})

		week := server.ContractPartitionWeekStart(server.NowUtc())
		contractWeek := server.ContractPartitionName("transfer_contract", week)
		reportLegacy := server.ContractLegacyPartitionName("contract_close_report")
		reportWeek := server.ContractPartitionName("contract_close_report", week)
		for _, fault := range []struct {
			name     string
			sql      string
			artifact string
		}{
			{
				name:     "parent index dropped",
				sql:      `DROP INDEX transfer_contract_stream_id`,
				artifact: "transfer_contract_stream_id@v629",
			},
			{
				name:     "parent index reshaped",
				sql:      `DROP INDEX contract_extender_create_time_contract_id; CREATE INDEX contract_extender_create_time_contract_id ON contract_extender (contract_id, create_time)`,
				artifact: "contract_extender_create_time_contract_id@v674",
			},
			{
				// the published definition without ONLY, but no partition has its copy
				name:     "parent index shell never attached",
				sql:      `DROP INDEX transfer_contract_audit_closed_null_day; CREATE INDEX transfer_contract_audit_closed_null_day ON ONLY transfer_contract (close_time, contract_id) WHERE outcome IS NULL AND close_time IS NOT NULL`,
				artifact: "closed-day audit lookup for contracts without an outcome@v810",
			},
			{
				name:     "usage time index reshaped",
				sql:      `DROP INDEX transfer_contract_usage_missing_time; CREATE INDEX transfer_contract_usage_missing_time ON transfer_contract (contract_id) WHERE close_time IS NULL`,
				artifact: "terminal usage missing timestamp index@v748",
			},
			{
				name:     "parent trigger disabled",
				sql:      `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`,
				artifact: "transfer_contract immutable usage and terminal attribution guard@v745",
			},
			{
				name:     "parent trigger disabled on the parent only",
				sql:      `ALTER TABLE ONLY transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`,
				artifact: "transfer_contract immutable usage and terminal attribution guard@v745",
			},
			{
				name:     "partition usage guard disabled",
				sql:      fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER transfer_contract_usage_guard`, contractWeek),
				artifact: "transfer_contract immutable usage and terminal attribution guard@v745",
			},
			{
				name:     "partition archive TRUNCATE guard dropped",
				sql:      `DROP TRIGGER transfer_contract_usage_archive_truncate_guard ON transfer_contract_legacy`,
				artifact: "st_provider_usage_archive exact atomic copy and append-only custody@v746",
			},
			{
				name:     "partition escrow TRUNCATE guard disabled",
				sql:      `ALTER TABLE transfer_escrow_legacy DISABLE TRIGGER transfer_escrow_revision_truncate_guard`,
				artifact: "net escrow durable snapshot revision and retention fences@v747",
			},
			{
				name:     "legacy partition usage guard disabled",
				sql:      `ALTER TABLE transfer_contract_legacy DISABLE TRIGGER transfer_contract_usage_guard`,
				artifact: "transfer_contract immutable usage and terminal attribution guard@v745",
			},
			{
				name:     "partition sweep owner guard disabled",
				sql:      fmt.Sprintf(`ALTER TABLE %s DISABLE TRIGGER payment_plan_sweep_owner_guard`, server.ContractPartitionName("transfer_escrow_sweep", week)),
				artifact: "durable payout plan assignment and sweep owner guard@v814",
			},
			{
				name:     "leaf without its analyze threshold",
				sql:      fmt.Sprintf(`ALTER TABLE %s RESET (autovacuum_analyze_threshold)`, contractWeek),
				artifact: "transfer_contract bounded open statistics@v635",
			},
			{
				name:     "legacy leaf with an unbounded open target",
				sql:      `ALTER TABLE transfer_contract_legacy ALTER COLUMN open SET STATISTICS 10000`,
				artifact: "transfer_contract bounded open statistics@v635",
			},
			{
				name:     "parent open target unbounded",
				sql:      `ALTER TABLE ONLY transfer_contract ALTER COLUMN open SET STATISTICS 10000`,
				artifact: "transfer_contract bounded open statistics@v635",
			},
			{
				name:     "cascade report key restored",
				sql:      `ALTER TABLE contract_close_report ADD CONSTRAINT contract_close_report_contract_id_fkey FOREIGN KEY (contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`,
				artifact: "logical contract close report receipts@v764",
			},
			{
				name:     "report check dropped",
				sql:      `ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_used_transfer_byte_count_check`,
				artifact: "logical contract close report receipts@v764",
			},
			{
				name:     "report partition unique index",
				sql:      fmt.Sprintf(`CREATE UNIQUE INDEX synthetic_report_id ON %s (report_id)`, reportLegacy),
				artifact: "logical contract close report receipts@v764",
			},
			{
				name:     "report partition trigger",
				sql:      fmt.Sprintf(`CREATE TRIGGER synthetic_report_guard BEFORE UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION transfer_contract_usage_guard()`, reportWeek),
				artifact: "logical contract close report receipts@v764",
			},
		} {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				drift, err := migrationContractTxDrift(ctx, tx)
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				if !strings.Contains(drift, fault.artifact) {
					t.Fatalf("%s did not reach %s: %s", fault.name, fault.artifact, drift)
				}
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("rolled-back partitioned faults left drift: %s", drift)
		}
	})
}

// Until its retirement the legacy settlement key is cloned onto every contract
// partition. The test role cannot disable PostgreSQL's internal triggers, so
// the state of each action trigger cloned onto one partition is projected into
// the signal's own query, as for the parent's.
func TestMigrationsContractPartitionedLegacySettlementKeyClones(t *testing.T) {
	migrationContractTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, server.LegacySettlementIntentRetirementMigrationVersion()-1)
		server.Testing_PartitionContracts(ctx)
		if !server.IsContractPartitioned(ctx) {
			t.Fatal("the cutover left the contract tables plain")
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("partitioned contract tables report drift before the retirement: %s", drift)
		}
		contractWeek := server.ContractPartitionName("transfer_contract", server.ContractPartitionWeekStart(server.NowUtc()))
		var triggers []uint32
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT t.oid FROM pg_catalog.pg_trigger t
 JOIN pg_constraint k ON k.oid=t.tgconstraint
 WHERE k.conrelid=to_regclass('public.legacy_settlement_intent') AND k.confrelid=to_regclass('public.'||$1)
 AND k.contype='f' AND k.conparentid<>0 AND t.tgisinternal ORDER BY t.oid`, contractWeek)
			server.Raise(err)
			defer rows.Close()
			for rows.Next() {
				var oid uint32
				server.Raise(rows.Scan(&oid))
				triggers = append(triggers, oid)
			}
			server.Raise(rows.Err())
		})
		if len(triggers) != 2 {
			t.Fatalf("%s carries %d cloned legacy settlement key triggers, want its delete and update actions", contractWeek, len(triggers))
		}
		for _, oid := range triggers {
			for _, enabled := range []string{"D", "R", "A"} {
				source := &migrationFKTriggerProjectionSource{oid: oid, enabled: enabled}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
				if err != nil {
					t.Fatal(err)
				}
				drift := ""
				for _, alert := range alerts {
					if alert.Class == "migration-schema-drift" {
						drift = alert.Markdown()
					}
				}
				if enabled == "A" {
					if drift != "" {
						t.Fatalf("an always-enabled partition key trigger reports drift: %s", drift)
					}
				} else if !strings.Contains(drift, "legacy settlement intent and ownership guards@v763") {
					t.Fatalf("a %s partition key trigger did not reach the legacy settlement artifact: %s", enabled, drift)
				}
			}
		}
	})
}

// The cascade key from contract_close_report is required while the contract
// tables are plain, prepared cutover included, and its absence is accepted
// only once they are partitioned, where the cutover drops it by owner
// decision. A prepared cutover's staged parents and legacy bound checks are
// not drift.
func TestMigrationsContractCloseReportKeyAbsentOnlyWhenPartitioned(t *testing.T) {
	migrationContractTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrations(ctx)
		if server.IsContractPartitioned(ctx) {
			t.Fatal("the migrated contract tables are partitioned")
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("plain contract tables report drift at the head: %s", drift)
		}
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			if _, err := tx.Exec(ctx, `ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_contract_id_fkey`); err != nil {
				t.Fatal(err)
			}
			drift, err := migrationContractTxDrift(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(drift, "logical contract close report receipts@v764") {
				t.Fatalf("plain close reports without their cascade key report no drift: %s", drift)
			}
		})

		prepare := server.ContractPartitionMigrateOptions{
			CutoverWeek: server.ContractPartitionWeekStart(server.NowUtc()).Add(14 * 24 * time.Hour),
			Logf:        t.Logf,
		}
		if err := server.MigrateContractsToPartitions(ctx, prepare); err != nil {
			t.Fatal(err)
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("a prepared contract cutover reports drift: %s", drift)
		}
		snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
			if _, err := tx.Exec(ctx, `ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_contract_id_fkey`); err != nil {
				t.Fatal(err)
			}
			drift, err := migrationContractTxDrift(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(drift, "logical contract close report receipts@v764") {
				t.Fatalf("prepared close reports without their cascade key report no drift: %s", drift)
			}
		})
		if err := server.MigrateContractsToPartitions(ctx, server.ContractPartitionMigrateOptions{Abort: true, Logf: t.Logf}); err != nil {
			t.Fatal(err)
		}

		server.Testing_PartitionContracts(ctx)
		var keys int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid=to_regclass('public.contract_close_report') AND contype='f'`).Scan(&keys))
		})
		if keys != 0 {
			t.Fatalf("the cutover kept %d foreign keys on contract_close_report", keys)
		}
		if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
			t.Fatalf("partitioned close reports without the cascade key report drift: %s", drift)
		}
	})
}
