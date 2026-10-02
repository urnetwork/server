// The two append-only migration series share one monitor row. Real catalog
// queries must retain their column ownership across the relocated boundary.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Visit the published main head and each hardening append using the exact
// production probe. A shifted SELECT expression fails before fault injection.
func TestMigrationsSignalMergedCatalogUsesExactColumns(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		for version := 741; version <= server.MigrationCount(); version++ {
			server.ApplyDbMigrationsUpTo(ctx, version)
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if drift != "" {
				t.Fatalf("coherent merged schema at version %d reported drift: %s", version, drift)
			}
		}
		for _, fault := range []struct{ apply, restore, artifact string }{
			{
				apply:    `ALTER TABLE provider_egress_health ALTER COLUMN security_measured_at SET DEFAULT now()`,
				restore:  `ALTER TABLE provider_egress_health ALTER COLUMN security_measured_at DROP DEFAULT`,
				artifact: "provider_egress_health.security_measured_at@v724",
			},
			{
				apply:    `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`,
				restore:  `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`,
				artifact: "transfer_contract immutable usage and terminal attribution guard@v745",
			},
			{
				apply:    `DROP INDEX prober_shard_run_active_slot`,
				restore:  `CREATE UNIQUE INDEX prober_shard_run_active_slot ON prober_shard_run(shard_index) WHERE state='active'`,
				artifact: "private prober shard ownership and cleanup fences@v752",
			},
			{
				apply:    `ALTER TABLE prober_shard_run DROP CONSTRAINT prober_shard_run_balance_id_key`,
				restore:  `ALTER TABLE prober_shard_run ADD CONSTRAINT prober_shard_run_balance_id_key UNIQUE(balance_id)`,
				artifact: "private prober shard ownership and cleanup fences@v752",
			},
			{
				apply:    `ALTER TABLE prober_shard_run ALTER COLUMN retired_client_ids DROP DEFAULT`,
				restore:  `ALTER TABLE prober_shard_run ALTER COLUMN retired_client_ids SET DEFAULT '{}'::uuid[]`,
				artifact: "private prober shard ownership and cleanup fences@v752",
			},
			{
				apply:    `CREATE OR REPLACE FUNCTION transfer_contract_escrow_revision() RETURNS trigger LANGUAGE plpgsql AS $body$` + server.NetEscrowContractsRevisionLegacyFunctionBodySql + `$body$`,
				restore:  `CREATE OR REPLACE FUNCTION transfer_contract_escrow_revision() RETURNS trigger LANGUAGE plpgsql AS $body$` + server.NetEscrowContractsRevisionFunctionBodySql + `$body$`,
				artifact: "net escrow contract revision point lookup@v753",
			},
			{
				apply:    `ALTER FUNCTION transfer_contract_escrow_revision() SECURITY DEFINER`,
				restore:  `ALTER FUNCTION transfer_contract_escrow_revision() SECURITY INVOKER`,
				artifact: "net escrow contract revision point lookup@v753",
			},
			{
				apply:    `ALTER TABLE transfer_balance_net_escrow_snapshot DROP CONSTRAINT transfer_balance_net_escrow_snapshot_reserved_byte_count_check`,
				restore:  `ALTER TABLE transfer_balance_net_escrow_snapshot ADD CONSTRAINT transfer_balance_net_escrow_snapshot_reserved_byte_count_check CHECK(reserved_byte_count>=0)`,
				artifact: "durable net escrow reservation snapshot@v754",
			},
			{
				apply:    `ALTER TABLE transfer_balance_net_escrow_snapshot ALTER COLUMN revision SET DEFAULT 0`,
				restore:  `ALTER TABLE transfer_balance_net_escrow_snapshot ALTER COLUMN revision DROP DEFAULT`,
				artifact: "durable net escrow reservation snapshot@v754",
			},
		} {
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, fault.apply))
			})
			_, drift := migrationPingDatabaseCheck(t, ctx)
			if !strings.Contains(drift, fault.artifact) {
				t.Fatalf("merged probe attributed a changed catalog object incorrectly: want %s, got %s", fault.artifact, drift)
			}
			server.Db(ctx, func(conn server.PgConn) {
				server.RaisePgResult(conn.Exec(ctx, fault.restore))
			})
			if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
				t.Fatalf("restored merged schema reported drift: %s", drift)
			}
		}
	})
}
