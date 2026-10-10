// Execute the owning migration probe against published DDL and rolled-back
// schema faults; a catalog label without an operative query is insufficient.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func migrationCurrentSuffixTestContract(t testing.TB, version int) snMainnetMigrationContract {
	t.Helper()
	for _, contract := range migrationCurrentSuffixContracts {
		if contract.artifact.requiredVersion == version {
			return contract
		}
	}
	t.Fatalf("missing current suffix contract at version %d", version)
	return snMainnetMigrationContract{}
}

func TestMigrationsCurrentSuffixExactSchemaPrefixes(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		for head := 793; head <= 813; head++ {
			server.ApplyDbMigrationsUpTo(ctx, head)
			row, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Fatalf("published prefix %d reports schema drift: %s", head, drift)
			}
			for _, contract := range migrationCurrentSuffixContracts {
				artifact := contract.artifact
				if len(row) <= artifact.rowColumn {
					t.Fatalf("prefix %d omitted artifact column %d", head, artifact.rowColumn)
				}
				if present := migrationBool(row[artifact.rowColumn]); present != (artifact.requiredVersion <= head) {
					t.Fatalf("prefix %d artifact %d=%t", head, artifact.requiredVersion, present)
				}
			}
		}
	})
}

func TestMigrationsCurrentSuffixSchemaFaultsReachSignal(t *testing.T) {
	faults := []struct {
		name    string
		version int
		sql     string
	}{
		{"wrong tier type", 794, `ALTER TABLE network_extender ALTER COLUMN directory_tier TYPE integer`},
		{"changed legacy tier default", 794, `ALTER TABLE network_extender ALTER COLUMN directory_tier SET DEFAULT 1`},
		{"missing request ledger", 794, `DROP TABLE network_extender_release_request`},
		{"missing release identity", 794, `ALTER TABLE network_extender_release DROP CONSTRAINT network_extender_release_pkey`},
		{"missing block report lookup", 794, `DROP INDEX network_extender_block_report_extender_id_country_code_report_time`},
		{"reordered release lookup", 795, `DROP INDEX network_extender_release_identity_epoch; CREATE INDEX network_extender_release_identity_epoch ON network_extender_release(identity,extender_id,epoch)`},
		{"non-durable client limit", 797, `ALTER TABLE network_top_level_client_limit SET UNLOGGED`},
		{"nullable client limit", 797, `ALTER TABLE network_top_level_client_limit ALTER COLUMN top_level_client_limit DROP NOT NULL`},
		{"required optional monthly cap", 798, `ALTER TABLE network_client_data_cap ALTER COLUMN monthly_byte_limit SET NOT NULL`},
		{"invented usage default", 798, `ALTER TABLE network_client_data_cap ALTER COLUMN total_used_byte_count SET DEFAULT 1`},
		{"cap lookup requires both limits", 798, `DROP INDEX network_client_data_cap_network_id_client_id; CREATE INDEX network_client_data_cap_network_id_client_id ON network_client_data_cap(network_id,client_id) WHERE monthly_byte_limit IS NOT NULL AND total_byte_limit IS NOT NULL`},
		{"missing paused cap lookup", 798, `DROP INDEX network_client_data_cap_monthly_paused`},
		{"wrong usage count type", 798, `ALTER TABLE network_client_data_usage ALTER COLUMN used_byte_count TYPE integer`},
		{"missing drain deduplication", 798, `ALTER TABLE network_client_data_usage_drain DROP CONSTRAINT network_client_data_usage_drain_pkey`},
		{"missing rollup cursor", 798, `DROP TABLE network_client_data_usage_rollup`},
		{"wrong lead email width", 799, `ALTER TABLE services_lead ALTER COLUMN email TYPE varchar(255)`},
		{"invented lead notification default", 799, `ALTER TABLE services_lead ALTER COLUMN notify_time SET DEFAULT now()`},
		{"missing lead retention lookup", 799, `DROP INDEX services_lead_create_time`},
		{"wrong ACL group width", 800, `ALTER TABLE network_client_acl_group ALTER COLUMN acl_group TYPE varchar(33)`},
		{"reordered ACL owner lookup", 800, `DROP INDEX network_client_acl_group_network_id; CREATE INDEX network_client_acl_group_network_id ON network_client_acl_group(client_id,network_id)`},
		{"missing Embed identity", 801, `ALTER TABLE network_embed DROP CONSTRAINT network_embed_pkey`},
		{"nullable enablement time", 801, `ALTER TABLE network_embed ALTER COLUMN enable_time DROP NOT NULL`},
		{"missing retained disable time", 802, `ALTER TABLE network_embed DROP COLUMN disable_time`},
		{"required disable time", 802, `ALTER TABLE network_embed ALTER COLUMN disable_time SET NOT NULL`},
		{"wrong client session type", 806, `ALTER TABLE network_client ALTER COLUMN session_id TYPE text`},
		{"defaulted session create time", 806, `ALTER TABLE network_client ALTER COLUMN session_create_time SET DEFAULT now()`},
		{"required auth-code session", 806, `ALTER TABLE auth_code ALTER COLUMN origin_session_id SET NOT NULL`},
		{"wrong operation action width", 806, `ALTER TABLE network_session_operation ALTER COLUMN action TYPE varchar(17)`},
		{"changed session target default", 806, `ALTER TABLE network_session_operation ALTER COLUMN target_session_ids SET DEFAULT '{}'::jsonb`},
		{"changed quota reservation default", 806, `ALTER TABLE network_session_operation ALTER COLUMN quota_reserved SET DEFAULT false`},
		{"recovery misses enforced operations", 806, `DROP INDEX network_session_operation_recovery; CREATE INDEX network_session_operation_recovery ON network_session_operation(update_time,network_id,operation_id) WHERE status='prepared'`},
		{"missing redemption deduplication", 806, `ALTER TABLE auth_code_redemption DROP CONSTRAINT auth_code_redemption_request_id_key`},
		{"missing session index repair cursor", 806, `DROP TABLE network_session_index_outbox`},
		{"client session index includes inactive", 807, `DROP INDEX network_client_session_active; CREATE INDEX network_client_session_active ON network_client(network_id,session_id,client_id) WHERE session_id IS NOT NULL`},
		{"client session index has extra included key", 807, `DROP INDEX network_client_session_active; CREATE INDEX network_client_session_active ON network_client(network_id,session_id,client_id) INCLUDE (active) WHERE active AND session_id IS NOT NULL`},
		{"auth-code session index reordered", 808, `DROP INDEX auth_code_origin_session; CREATE INDEX auth_code_origin_session ON auth_code(origin_session_id,network_id) WHERE active AND origin_session_id IS NOT NULL`},
		{"missing auth-code session index", 808, `DROP INDEX auth_code_origin_session`},
		{"missing function poll index", 809, `DROP INDEX pending_task_function_poll_order`},
		{"function poll normalization misses repeated version segments", 809, `DROP INDEX pending_task_function_poll_order; CREATE INDEX pending_task_function_poll_order ON pending_task(regexp_replace(function_name, '/v[0-9]+', ''), available_block, run_priority DESC, run_max_time_seconds DESC, task_id)`},
		{"function poll priority precedes due range", 809, `DROP INDEX pending_task_function_poll_order; CREATE INDEX pending_task_function_poll_order ON pending_task(regexp_replace(function_name, '/v[0-9]+', '', 'g'), run_priority DESC, available_block, run_max_time_seconds DESC, task_id)`},
		{"function poll priority direction reversed", 809, `DROP INDEX pending_task_function_poll_order; CREATE INDEX pending_task_function_poll_order ON pending_task(regexp_replace(function_name, '/v[0-9]+', '', 'g'), available_block, run_priority, run_max_time_seconds DESC, task_id)`},
		{"function poll index has an unowned predicate", 809, `DROP INDEX pending_task_function_poll_order; CREATE INDEX pending_task_function_poll_order ON pending_task(regexp_replace(function_name, '/v[0-9]+', '', 'g'), available_block, run_priority DESC, run_max_time_seconds DESC, task_id) WHERE run_priority > 0`},
		{"function poll index has an extra included key", 809, `DROP INDEX pending_task_function_poll_order; CREATE INDEX pending_task_function_poll_order ON pending_task(regexp_replace(function_name, '/v[0-9]+', '', 'g'), available_block, run_priority DESC, run_max_time_seconds DESC, task_id) INCLUDE (function_name)`},
		{"missing closed-day audit index", 810, `DROP INDEX transfer_contract_audit_closed_null_day`},
		{"closed-day audit key order reversed", 810, `DROP INDEX transfer_contract_audit_closed_null_day; CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(contract_id,close_time) WHERE outcome IS NULL AND close_time IS NOT NULL`},
		{"closed-day audit includes completed outcomes", 810, `DROP INDEX transfer_contract_audit_closed_null_day; CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(close_time,contract_id) WHERE close_time IS NOT NULL`},
		{"closed-day audit includes open contracts", 810, `DROP INDEX transfer_contract_audit_closed_null_day; CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(close_time,contract_id) WHERE outcome IS NULL`},
		{"closed-day audit has an extra included key", 810, `DROP INDEX transfer_contract_audit_closed_null_day; CREATE INDEX transfer_contract_audit_closed_null_day ON transfer_contract(close_time,contract_id) INCLUDE (outcome) WHERE outcome IS NULL AND close_time IS NOT NULL`},
		{"missing grant receipts", 811, `DROP TABLE transfer_balance_grant_run`},
		{"grant receipt allows duplicate run", 811, `ALTER TABLE transfer_balance_grant_run DROP CONSTRAINT transfer_balance_grant_run_pkey`},
		{"grant receipt kind has wrong width", 811, `ALTER TABLE transfer_balance_grant_run ALTER COLUMN grant_kind TYPE varchar(33)`},
		{"deadline guard reverted to predecessor", 812, `CREATE OR REPLACE FUNCTION transfer_contract_usage_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$` + server.ContractUsageGuardOriginalFunctionBodySql + `$guard$`},
		{"missing legacy checkpoint bound", 813, `ALTER TABLE contract_close DROP COLUMN legacy_checkpoint_byte_count`},
		{"legacy checkpoint bound truncates bytes", 813, `ALTER TABLE contract_close ALTER COLUMN legacy_checkpoint_byte_count TYPE integer`},
		{"invented legacy checkpoint bound", 813, `ALTER TABLE contract_close ALTER COLUMN legacy_checkpoint_byte_count SET DEFAULT 0`},
		{"missing identified checkpoint total", 813, `ALTER TABLE contract_close DROP COLUMN identified_checkpoint_byte_count`},
		{"identified checkpoint total truncates bytes", 813, `ALTER TABLE contract_close ALTER COLUMN identified_checkpoint_byte_count TYPE integer`},
		{"invented identified checkpoint total", 813, `ALTER TABLE contract_close ALTER COLUMN identified_checkpoint_byte_count SET DEFAULT 0`},
	}
	var observed []snMainnetMigrationAlertObservation
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("healthy suffix reports drift: %s", drift)
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				artifact := migrationCurrentSuffixTestContract(t, fault.version).artifact
				observed = append(observed, snMainnetMigrationAlertObservation{
					alerts: alerts, want: fmt.Sprintf("%s@v%d", artifact.name, fault.version),
				})
			})
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back suffix faults left drift: %s", drift)
		}
	})
	if len(observed) != len(faults) {
		t.Fatalf("observed %d schema faults, want %d", len(observed), len(faults))
	}
	for index, observation := range observed {
		if alert := requireAlertClass(t, observation.alerts, "migration-schema-drift"); !strings.Contains(alert.Markdown(), observation.want) {
			t.Fatalf("%s lost its owning migration: %s", faults[index].name, alert.Markdown())
		}
	}
}

// Project only index build state from the actual migrated index definition.
// No privileged PostgreSQL catalog mutation is needed to model interruption.
func TestMigrationsCurrentSuffixConcurrentIndexReadiness(t *testing.T) {
	snMainnetMigrationTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Db(ctx, func(conn server.PgConn) {
			for _, index := range []struct {
				version     int
				table, name string
			}{
				{807, "network_client", "network_client_session_active"},
				{808, "auth_code", "auth_code_origin_session"},
				{809, "pending_task", "pending_task_function_poll_order"},
				{810, "transfer_contract", "transfer_contract_audit_closed_null_day"},
			} {
				var definition string
				var predicate *string
				server.Raise(conn.QueryRow(ctx, `SELECT pg_get_indexdef(indexrelid), pg_get_expr(indpred,indrelid)
 FROM pg_index WHERE indexrelid=to_regclass('public.'||$1)`, index.name).Scan(&definition, &predicate))
				guard := migrationCurrentSuffixTestContract(t, index.version).query
				for _, state := range []struct{ valid, ready, want bool }{
					{valid: true, ready: true, want: true}, {valid: false, ready: true}, {valid: true, ready: false},
				} {
					var matched bool
					server.Raise(conn.QueryRow(ctx, `WITH index_artifact AS (
 SELECT $1::text AS table_name, $2::text AS index_name,
 $3::text AS definition, $4::text AS predicate_definition,
 $5::boolean AS indisvalid, $6::boolean AS indisready) SELECT `+guard,
						index.table, index.name, definition, predicate, state.valid, state.ready).Scan(&matched))
					if matched != state.want {
						t.Fatalf("version %d index valid=%t ready=%t matched=%t", index.version, state.valid, state.ready, matched)
					}
				}
			}
		})
	})
}
