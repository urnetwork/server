// The two append-only migration series share one monitor row. Real catalog
// queries must retain their column ownership across the relocated boundary.
package monitor

import (
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
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
			{apply: `ALTER TABLE circle_transfer_request ALTER COLUMN review_required SET DEFAULT true`, restore: `ALTER TABLE circle_transfer_request ALTER COLUMN review_required SET DEFAULT false`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `ALTER TABLE circle_transfer_request DROP CONSTRAINT circle_transfer_request_idempotency_key_key`, restore: `ALTER TABLE circle_transfer_request ADD UNIQUE(idempotency_key)`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `ALTER TABLE circle_transfer_observation DROP CONSTRAINT circle_transfer_observation_digest_check`, restore: `ALTER TABLE circle_transfer_observation ADD CHECK(length(digest)=64)`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `ALTER TABLE circle_transfer_request DISABLE TRIGGER circle_transfer_request_guard`, restore: `ALTER TABLE circle_transfer_request ENABLE TRIGGER circle_transfer_request_guard`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `ALTER TABLE circle_transfer_observation DISABLE TRIGGER circle_transfer_observation_truncate_guard`, restore: `ALTER TABLE circle_transfer_observation ENABLE TRIGGER circle_transfer_observation_truncate_guard`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `CREATE OR REPLACE FUNCTION circle_transfer_request_guard() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$`, restore: `CREATE OR REPLACE FUNCTION circle_transfer_request_guard() RETURNS trigger LANGUAGE plpgsql AS $guard$` + circleTransferRequestGuardBody + `$guard$`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `ALTER TABLE circle_transfer_observation ALTER CONSTRAINT circle_transfer_observation_network_id_user_id_request_id_fkey DEFERRABLE`, restore: `ALTER TABLE circle_transfer_observation ALTER CONSTRAINT circle_transfer_observation_network_id_user_id_request_id_fkey NOT DEFERRABLE`, artifact: "Circle customer transfer custody and append-only observations@v762"},
			{apply: `ALTER TABLE account_payment DISABLE TRIGGER account_payment_submission_basis_guard`, restore: `ALTER TABLE account_payment ENABLE TRIGGER account_payment_submission_basis_guard`, artifact: "provider payment bonus provenance and submission guards@v756"},
			{apply: `ALTER TABLE provider_payout_boundary DISABLE TRIGGER provider_payout_boundary_truncate_guard`, restore: `ALTER TABLE provider_payout_boundary ENABLE TRIGGER provider_payout_boundary_truncate_guard`, artifact: "provider earning boundary immutable guards@v757"},
			{apply: `ALTER TABLE transfer_balance DISABLE TRIGGER transfer_balance_pending_debit_guard`, restore: `ALTER TABLE transfer_balance ENABLE TRIGGER transfer_balance_pending_debit_guard`, artifact: "asynchronous transfer debit journal and retention guard@v758"},
			{apply: `DROP INDEX transfer_debit_journal_shard`, restore: `CREATE INDEX transfer_debit_journal_shard ON transfer_debit_journal(shard,balance_id,contract_id)`, artifact: "asynchronous transfer debit journal and retention guard@v758"},
			{apply: `ALTER TABLE transfer_debit_journal ADD CONSTRAINT test_shared_fk FOREIGN KEY(balance_id) REFERENCES transfer_balance(balance_id)`, restore: `ALTER TABLE transfer_debit_journal DROP CONSTRAINT test_shared_fk`, artifact: "asynchronous transfer debit journal and retention guard@v758"},
			{apply: `ALTER TABLE test_balance_drain ALTER COLUMN drained_balance_byte_count DROP NOT NULL`, restore: `ALTER TABLE test_balance_drain ALTER COLUMN drained_balance_byte_count SET NOT NULL`, artifact: "acceptance-test balance drain audit@v759"},
			{apply: `ALTER TABLE test_balance_drain DROP CONSTRAINT test_balance_drain_pkey`, restore: `ALTER TABLE test_balance_drain ADD PRIMARY KEY(drain_id)`, artifact: "acceptance-test balance drain audit@v759"},
			{apply: `DROP INDEX test_balance_drain_network_id_end_time; CREATE INDEX test_balance_drain_network_id_end_time ON test_balance_drain(network_id,end_time) WHERE restore_time IS NOT NULL`, restore: `DROP INDEX test_balance_drain_network_id_end_time; CREATE INDEX test_balance_drain_network_id_end_time ON test_balance_drain(network_id,end_time) WHERE restore_time IS NULL`, artifact: "active acceptance-test balance drain lookup@v760"},
			{apply: `ALTER TABLE solana_payment_intent ALTER COLUMN expected_amount_micro SET DEFAULT 0`, restore: `ALTER TABLE solana_payment_intent ALTER COLUMN expected_amount_micro DROP DEFAULT`, artifact: "Solana payment amount reservations@v761"},
			{apply: `ALTER TABLE solana_unfulfilled_payment ALTER COLUMN sender_account TYPE varchar(65)`, restore: `ALTER TABLE solana_unfulfilled_payment ALTER COLUMN sender_account TYPE varchar(64)`, artifact: "Solana payment amount reservations@v761"},
			{apply: `ALTER TABLE solana_payment_amount_reservation DROP CONSTRAINT solana_payment_amount_reservation_pkey`, restore: `ALTER TABLE solana_payment_amount_reservation ADD PRIMARY KEY(amount_micro)`, artifact: "Solana payment amount reservations@v761"},
			{apply: `DROP INDEX solana_payment_intent_expected_amount_micro`, restore: `CREATE INDEX solana_payment_intent_expected_amount_micro ON solana_payment_intent(expected_amount_micro,expires_at) WHERE expected_amount_micro IS NOT NULL`, artifact: "Solana payment amount reservations@v761"},
			{apply: `ALTER TABLE legacy_settlement_intent ALTER COLUMN failure_code SET DEFAULT 'accounting'`, restore: `ALTER TABLE legacy_settlement_intent ALTER COLUMN failure_code SET DEFAULT 'none'`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `ALTER TABLE legacy_settlement_intent DROP CONSTRAINT legacy_settlement_intent_check`, restore: `ALTER TABLE legacy_settlement_intent ADD CHECK(shard=get_byte(uuid_send(contract_id),15)%16)`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `DROP INDEX legacy_settlement_intent_due`, restore: `CREATE INDEX legacy_settlement_intent_due ON legacy_settlement_intent(shard,next_attempt_time,contract_id)`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_legacy_settlement_owner`, restore: `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_legacy_settlement_owner`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `DROP TRIGGER transfer_contract_legacy_settlement_owner ON transfer_contract; CREATE TRIGGER transfer_contract_legacy_settlement_owner BEFORE UPDATE OF outcome,dispute ON transfer_contract FOR EACH ROW EXECUTE FUNCTION guard_legacy_settlement_intent_outcome()`, restore: `DROP TRIGGER transfer_contract_legacy_settlement_owner ON transfer_contract; CREATE TRIGGER transfer_contract_legacy_settlement_owner BEFORE UPDATE OF outcome ON transfer_contract FOR EACH ROW EXECUTE FUNCTION guard_legacy_settlement_intent_outcome()`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `CREATE OR REPLACE FUNCTION guard_legacy_settlement_intent_outcome() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$`, restore: `CREATE OR REPLACE FUNCTION guard_legacy_settlement_intent_outcome() RETURNS trigger LANGUAGE plpgsql AS $body$` + legacySettlementOutcomeGuardBody + `$body$`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `ALTER TABLE legacy_settlement_intent ALTER CONSTRAINT legacy_settlement_intent_contract_id_fkey DEFERRABLE`, restore: `ALTER TABLE legacy_settlement_intent ALTER CONSTRAINT legacy_settlement_intent_contract_id_fkey NOT DEFERRABLE`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `ALTER TABLE legacy_settlement_intent DROP CONSTRAINT legacy_settlement_intent_contract_id_fkey; ALTER TABLE legacy_settlement_intent ADD FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id) ON DELETE CASCADE`, restore: `ALTER TABLE legacy_settlement_intent DROP CONSTRAINT legacy_settlement_intent_contract_id_fkey; ALTER TABLE legacy_settlement_intent ADD FOREIGN KEY(contract_id) REFERENCES transfer_contract(contract_id)`, artifact: "legacy settlement intent and ownership guards@v763"},
			{apply: `ALTER TABLE legacy_settlement_intent ADD COLUMN test_shared_balance_id uuid REFERENCES transfer_balance(balance_id)`, restore: `ALTER TABLE legacy_settlement_intent DROP COLUMN test_shared_balance_id`, artifact: "legacy settlement intent and ownership guards@v763"},

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
				restore:  `CREATE OR REPLACE FUNCTION transfer_contract_escrow_revision() RETURNS trigger LANGUAGE plpgsql AS $body$` + server.NetEscrowContractsRedisRevisionFunctionBodySql + `$body$`,
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
		checkMigrationForeignKeyEnabledCatalog(t, ctx, "circle_transfer_observation", "circle_transfer_request", "Circle customer transfer custody and append-only observations@v762")
		checkMigrationForeignKeyEnabledCatalog(t, ctx, "legacy_settlement_intent", "transfer_contract", "legacy settlement intent and ownership guards@v763")
	})
}
