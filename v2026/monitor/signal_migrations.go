package monitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Signal migrations implements SIGNALS.md §8.9. Migration versions are an
// append-only production protocol: the recorded head and the schema artifacts
// published at each version must agree before dependent services roll.
func NewMigrationsSignal() Signal {
	return &signalAdapter{
		number: "8.9",
		key:    "migrations",
		name:   "Database migration and schema coherence",
		probe:  migrationsProbe{},
	}
}

type migrationsProbe struct{}

func (migrationsProbe) id() string             { return "pg/migration-coherence" }
func (migrationsProbe) tier() string           { return tierWarn }
func (migrationsProbe) cadence() time.Duration { return time.Minute }

type migrationArtifact struct {
	name            string
	requiredVersion int
	// removedVersion is the first head where a later published migration
	// intentionally supersedes this artifact. That removal has its own
	// persistent entry below, so both sides of the transition are checked.
	removedVersion int
	rowColumn      int
}

var migrationArtifacts = []migrationArtifact{
	{name: "competition_round", requiredVersion: 588, rowColumn: 1},
	{name: "competition_job_immutable_guard", requiredVersion: 589, rowColumn: 2},
	{name: "competition_round.providers_sha256", requiredVersion: 590, rowColumn: 3},
	{name: "competition_round.epoch_number", requiredVersion: 591, rowColumn: 4},
	{name: "competition_job.api_image_digest", requiredVersion: 592, rowColumn: 5},
	{name: "competition_candidate_review", requiredVersion: 593, rowColumn: 6},
	{name: "transfer_escrow_balance_contract", requiredVersion: 594, rowColumn: 7},
	{name: "account_payment.contract_retention_cursor+pending", requiredVersion: 595, rowColumn: 8},
	{name: "account_payment_contract_retention_pending", requiredVersion: 596, rowColumn: 9},
	{name: "transfer_escrow_sweep_payment_contract", requiredVersion: 597, rowColumn: 10},
	{name: "migration_catalog", requiredVersion: 599, rowColumn: 11},
	{name: "transfer_escrow_unsettled_balance_contract", requiredVersion: 601, rowColumn: 12},
	{name: "client_reliability_running_window.degraded_classification_version", requiredVersion: 602, rowColumn: 13},
	{name: "client_reliability_running_window classification write guard", requiredVersion: 603, rowColumn: 14},
	{name: "provider_egress_health TLS authentication failure guard", requiredVersion: 604, rowColumn: 15},
	{name: "st_fleet_binding_signature", requiredVersion: 605, rowColumn: 16},
	{name: "st_epoch_notification", requiredVersion: 606, rowColumn: 17},
	{name: "network.points_leaderboard_public", requiredVersion: 607, rowColumn: 18},
	{name: "network.emoji_tag", requiredVersion: 608, rowColumn: 19},
	{name: "network_points_leaderboard_snapshot", requiredVersion: 609, rowColumn: 20},
	{name: "network_points_leaderboard", requiredVersion: 610, rowColumn: 21},
	{name: "network_points_leaderboard_pos_points", requiredVersion: 611, rowColumn: 22},
	{name: "network_points_leaderboard_pos_blocks", requiredVersion: 612, rowColumn: 23},
	{name: "network_points_leaderboard_pos_streak", requiredVersion: 613, rowColumn: 24},
	{name: "ST mirror deployment identity", requiredVersion: 614, rowColumn: 25},
	{name: "st_transaction_intent deployment/logical generation identity", requiredVersion: 615, rowColumn: 26},
	{name: "st_transaction_intent_chain_account_nonce", requiredVersion: 616, removedVersion: 621, rowColumn: 27},
	{name: "st_transaction_intent_logical_generation", requiredVersion: 617, rowColumn: 28},
	{name: "st_transaction_intent_account_reconcile", requiredVersion: 618, removedVersion: 622, rowColumn: 29},
	{name: "st_transaction_intent.genesis_hash", requiredVersion: 619, rowColumn: 30},
	{name: "st_transaction_intent_genesis_account_nonce", requiredVersion: 620, rowColumn: 31},
	{name: "st_transaction_intent_chain_account_nonce removed", requiredVersion: 621, rowColumn: 32},
	{name: "st_transaction_intent_account_reconcile removed", requiredVersion: 622, rowColumn: 33},
	{name: "st_transaction_intent_account_reconcile_v2", requiredVersion: 623, rowColumn: 34},
	{name: "ST transaction terminal status constraints", requiredVersion: 624, rowColumn: 35},
	{name: "st_transaction_attempt.kind", requiredVersion: 625, rowColumn: 36},
	{name: "legacy profile/deployment nonce constraint removed", requiredVersion: 626, rowColumn: 37},
	{name: "ST signature/notification deployment identity", requiredVersion: 627, rowColumn: 38},
	{name: "transfer_contract.stream_id+contract_participant", requiredVersion: 628, rowColumn: 39},
	{name: "transfer_contract_stream_id", requiredVersion: 629, rowColumn: 40},
}

func (migrationsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH version AS (
			SELECT coalesce(max(end_version_number), 0)::int AS value
			FROM migration_audit
			WHERE status = 'success'
		), index_artifact AS (
			SELECT tablename::text AS table_name,
			       indexname::text AS index_name,
			       regexp_replace(indexdef, '[[:space:]]+', ' ', 'g') AS definition
			FROM pg_indexes
			WHERE schemaname = 'public'
		), constraint_artifact AS (
			SELECT relation.relname::text AS table_name,
			       constraint_record.conname::text AS constraint_name,
			       constraint_record.contype::text AS constraint_type,
			       regexp_replace(
			           pg_get_constraintdef(constraint_record.oid),
			           '[[:space:]]+', ' ', 'g'
			       ) AS definition,
			       constraint_record.convalidated AS validated
			FROM pg_constraint AS constraint_record
			JOIN pg_class AS relation ON relation.oid = constraint_record.conrelid
			JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
			WHERE namespace.nspname = 'public'
		)
		SELECT version.value,
		       to_regclass('public.competition_round') IS NOT NULL,
		       to_regprocedure('public.competition_job_immutable_guard()') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'competition_round'
		             AND column_name = 'providers_sha256'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'competition_round'
		             AND column_name = 'epoch_number'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'competition_job'
		             AND column_name = 'api_image_digest'
		       ),
		       to_regclass('public.competition_candidate_review') IS NOT NULL,
		       to_regclass('public.transfer_escrow_balance_contract') IS NOT NULL,
		       (
		           SELECT count(*) = 2 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'account_payment'
		             AND column_name IN ('contract_retention_cursor', 'contract_retention_pending')
		       ),
		       to_regclass('public.account_payment_contract_retention_pending') IS NOT NULL,
		       to_regclass('public.transfer_escrow_sweep_payment_contract') IS NOT NULL,
		       to_regclass('public.migration_catalog') IS NOT NULL,
		       to_regclass('public.transfer_escrow_unsettled_balance_contract') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public'
		             AND table_name = 'client_reliability_running_window'
		             AND column_name = 'degraded_classification_version'
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'client_reliability_running_window'
		                 AND column_name = 'degraded_classification_write_token'
		           )
		           AND to_regprocedure('public.client_reliability_running_window_classification_guard()') IS NOT NULL
		           AND EXISTS (
		               SELECT 1
		               FROM pg_trigger AS tr
		               JOIN pg_class AS rel ON rel.oid = tr.tgrelid
		               JOIN pg_namespace AS nsp ON nsp.oid = rel.relnamespace
		               WHERE nsp.nspname = 'public'
		                 AND rel.relname = 'client_reliability_running_window'
		                 AND tr.tgname = 'client_reliability_running_window_classification_guard'
		                 AND NOT tr.tgisinternal
		           )
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'provider_egress_health'
		                 AND column_name = 'tls_authentication_failure'
		           )
		           AND to_regclass('public.provider_egress_health_tls_authentication_failed') IS NOT NULL
		       ),
		       (
		           to_regclass('public.st_fleet_binding_signature') IS NOT NULL
		           AND to_regclass('public.st_fleet_binding_signature_network') IS NOT NULL
		       ),
		       to_regclass('public.st_epoch_notification') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network'
		             AND column_name = 'points_leaderboard_public'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network'
		             AND column_name = 'emoji_tag'
		       ),
		       to_regclass('public.network_points_leaderboard_snapshot') IS NOT NULL,
		       to_regclass('public.network_points_leaderboard') IS NOT NULL,
		       to_regclass('public.network_points_leaderboard_pos_points') IS NOT NULL,
		       to_regclass('public.network_points_leaderboard_pos_blocks') IS NOT NULL,
		       to_regclass('public.network_points_leaderboard_pos_streak') IS NOT NULL,
		       (
		           (
		               SELECT count(*) = 7
		               FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name IN (
		                     'st_epoch', 'st_payout_leaf', 'st_publish', 'st_event',
		                     'st_chain_sync', 'st_head_binding', 'st_payout_artifact'
		                 )
		                 AND column_name = 'deployment_key'
		                 AND is_nullable = 'NO'
		                 AND column_default IS NULL
		           )
		           AND (
		               SELECT count(*) = 7
		               FROM (
		                   VALUES
		                       ('st_epoch', 'p', 'PRIMARY KEY (deployment_key, epoch)'),
		                       ('st_payout_leaf', 'p', 'PRIMARY KEY (deployment_key, epoch, no_id, leaf_index)'),
		                       ('st_payout_leaf', 'u', 'UNIQUE (deployment_key, epoch, no_id, coldkey)'),
		                       ('st_event', 'p', 'PRIMARY KEY (deployment_key, block_number, log_index)'),
		                       ('st_chain_sync', 'p', 'PRIMARY KEY (deployment_key, singleton_id)'),
		                       ('st_head_binding', 'p', 'PRIMARY KEY (deployment_key, ckey)'),
		                       ('st_payout_artifact', 'p', 'PRIMARY KEY (deployment_key, epoch, no_id)')
		               ) AS expected(table_name, constraint_type, definition)
		               WHERE EXISTS (
		                   SELECT 1 FROM constraint_artifact AS actual
		                   WHERE actual.table_name = expected.table_name
		                     AND actual.constraint_type = expected.constraint_type
		                     AND actual.definition = expected.definition
		                     AND actual.validated
		               )
		           )
		           AND (
		               SELECT count(*) = 4
		               FROM (
		                   VALUES
		                       ('st_epoch', 'st_epoch_status', '(deployment_key, status, epoch)'),
		                       ('st_publish', 'st_publish_epoch_kind', '(deployment_key, epoch, kind, create_time)'),
		                       ('st_event', 'st_event_kind_block', '(deployment_key, kind, block_number, log_index)'),
		                       ('st_payout_leaf', 'st_payout_leaf_client_epoch', '(deployment_key, client_id, epoch, no_id)')
		               ) AS expected(table_name, index_name, key_shape)
		               WHERE EXISTS (
		                   SELECT 1 FROM index_artifact AS actual
		                   WHERE actual.table_name = expected.table_name
		                     AND actual.index_name = expected.index_name
		                     AND actual.definition LIKE '%' || expected.key_shape || '%'
		               )
		           )
		       ),
		       (
		           (
		               SELECT count(*) = 3
		               FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'st_transaction_intent'
		                 AND column_name IN ('deployment_key', 'logical_key', 'generation')
		                 AND is_nullable = 'NO'
		                 AND column_default IS NULL
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'st_transaction_intent'
		                 AND constraint_name = 'st_transaction_intent_generation_check'
		                 AND constraint_type = 'c'
		                 AND definition LIKE '%generation >= 0%'
		                 AND validated
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_chain_account_nonce'
		             AND definition LIKE 'CREATE UNIQUE INDEX %'
		             AND definition LIKE '%(chain_id, from_address, nonce)%'
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_logical_generation'
		             AND definition LIKE 'CREATE UNIQUE INDEX %'
		             AND definition LIKE '%(logical_key, generation)%'
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_account_reconcile'
		             AND definition LIKE '%(chain_id, from_address, nonce)%'
		             AND definition LIKE '%WHERE%'
		             AND definition LIKE '%prepared%'
		             AND definition LIKE '%signed%'
		             AND definition LIKE '%broadcast%'
		             AND definition LIKE '%mined%'
		             AND definition LIKE '%uncertain%'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public'
		             AND table_name = 'st_transaction_intent'
		             AND column_name = 'genesis_hash'
		             AND data_type = 'character varying'
		             AND character_maximum_length = 66
		             AND is_nullable = 'NO'
		             AND column_default IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_genesis_account_nonce'
		             AND definition LIKE 'CREATE UNIQUE INDEX %'
		             AND definition LIKE '%(chain_id, genesis_hash, from_address, nonce)%'
		       ),
		       NOT EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE index_name = 'st_transaction_intent_chain_account_nonce'
		       ),
		       NOT EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE index_name = 'st_transaction_intent_account_reconcile'
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_account_reconcile_v2'
		             AND definition LIKE '%(chain_id, genesis_hash, from_address, nonce)%'
		             AND definition LIKE '%WHERE%'
		             AND definition LIKE '%prepared%'
		             AND definition LIKE '%signed%'
		             AND definition LIKE '%broadcast%'
		             AND definition LIKE '%mined%'
		             AND definition LIKE '%uncertain%'
		       ),
		       (
		           SELECT count(*) = 2
		           FROM constraint_artifact
		           WHERE (
		               (table_name = 'st_transaction_intent'
		                AND constraint_name = 'st_transaction_intent_status_check')
		               OR (table_name = 'st_transaction_attempt'
		                   AND constraint_name = 'st_transaction_attempt_status_check')
		           )
		             AND constraint_type = 'c'
		             AND definition LIKE '%reverted%'
		             AND definition LIKE '%invalid%'
		             AND definition LIKE '%canceled%'
		             AND definition LIKE '%superseded%'
		             AND validated
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'st_transaction_attempt'
		                 AND column_name = 'kind'
		                 AND data_type = 'character varying'
		                 AND character_maximum_length = 16
		                 AND is_nullable = 'NO'
		                 AND column_default IS NULL
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'st_transaction_attempt'
		                 AND constraint_name = 'st_transaction_attempt_kind_check'
		                 AND constraint_type = 'c'
		                 AND definition LIKE '%execution%'
		                 AND definition LIKE '%cancellation%'
		                 AND validated
		           )
		       ),
		       NOT EXISTS (
		           SELECT 1 FROM constraint_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND constraint_name = 'st_transaction_intent_profile_deployment_id_chain_id_from_a_key'
		       ),
		       (
		           (
		               SELECT count(*) = 2
		               FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name IN ('st_fleet_binding_signature', 'st_epoch_notification')
		                 AND column_name = 'deployment_key'
		                 AND is_nullable = 'NO'
		                 AND column_default IS NULL
		           )
		           AND (
		               SELECT count(*) = 2
		               FROM (
		                   VALUES
		                       ('st_fleet_binding_signature', 'PRIMARY KEY (deployment_key, client_id, generation)'),
		                       ('st_epoch_notification', 'PRIMARY KEY (deployment_key, epoch)')
		               ) AS expected(table_name, definition)
		               WHERE EXISTS (
		                   SELECT 1 FROM constraint_artifact AS actual
		                   WHERE actual.table_name = expected.table_name
		                     AND actual.constraint_type = 'p'
		                     AND actual.definition = expected.definition
		                     AND actual.validated
		               )
		           )
		           AND EXISTS (
		               SELECT 1 FROM index_artifact
		               WHERE table_name = 'st_fleet_binding_signature'
		                 AND index_name = 'st_fleet_binding_signature_network'
		                 AND definition LIKE '%(deployment_key, network_id, create_time DESC)%'
		           )
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'transfer_contract'
		                 AND column_name = 'stream_id'
		                 AND data_type = 'uuid'
		           )
		           AND to_regclass('public.contract_participant') IS NOT NULL
		           AND (
		               SELECT count(*) = 3
		               FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'contract_participant'
		                 AND column_name IN ('stream_id', 'client_id', 'network_id')
		                 AND data_type = 'uuid'
		                 AND is_nullable = 'NO'
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'contract_participant'
		                 AND constraint_type = 'p'
		                 AND definition = 'PRIMARY KEY (stream_id, client_id)'
		                 AND validated
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'transfer_contract'
		             AND index_name = 'transfer_contract_stream_id'
		             AND definition LIKE '%(stream_id)%'
		             AND definition LIKE '%stream_id IS NOT NULL%'
		       )
		FROM version;
	`)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("migration coherence query returned %d rows, want 1", len(rows))
	}

	target := pgTarget(env)
	dbVersion := atoiRow(rows[0], 0)
	requiredHead := server.MigrationCount()
	missing := make([]string, 0)
	for _, artifact := range migrationArtifacts {
		isPublished := artifact.requiredVersion <= dbVersion
		isStillRequired := artifact.removedVersion == 0 || dbVersion < artifact.removedVersion
		if isPublished && isStillRequired && !migrationBool(rows[0].str(artifact.rowColumn)) {
			missing = append(missing, fmt.Sprintf("%s@v%d", artifact.name, artifact.requiredVersion))
		}
	}
	if 600 <= dbVersion {
		if !migrationBool(rows[0].str(11)) {
			missing = append(missing, "migration_catalog identities@v600")
		} else {
			catalogRows, catalogErr := env.runner.pg(ctx, `
				SELECT count(*)::int,
				       coalesce(min(migration_index), -1)::int,
				       coalesce(max(migration_index), -1)::int
				FROM migration_catalog;
			`)
			if catalogErr != nil {
				return nil, catalogErr
			}
			if len(catalogRows) != 1 || atoiRow(catalogRows[0], 0) != dbVersion ||
				atoiRow(catalogRows[0], 1) != 0 || atoiRow(catalogRows[0], 2) != dbVersion-1 {
				missing = append(missing, "migration_catalog identities@v600")
			}
		}
	}

	findings := make([]finding, 0, 2)
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "pg/migration-coherence", tier: tierPage,
			class: "migration-schema-drift", target: target, sustain: 1,
			symptom:   fmt.Sprintf("database migration audit is at version %d but %d published schema artifact(s) are absent", dbVersion, len(missing)),
			mechanism: "A migration version was reordered, skipped, removed, or marked successful without leaving its published schema. A service that trusts only the numeric head can then execute code against missing columns or indexes, or replay an older non-idempotent migration into objects that already exist.",
			baseline:  fmt.Sprintf("Every published artifact through recorded database version %d exists; migration versions never move after release.", dbVersion),
			observed:  fmt.Sprintf("db_version=%d code_required_version=%d missing=%s", dbVersion, requiredHead, strings.Join(missing, ",")),
			action:    "Stop dependent service activation. Restore every published migration to its original index, append new migrations after the published sequence, and apply that corrected stream. Do not edit migration_audit or create production objects by hand merely to clear this alert.",
			verify:    "The recorded head advances only through the corrected append-only stream, every required artifact exists at its published version, and a fresh migration-coherence run has no schema-drift alert.",
			playbook:  "SIGNALS.md §8.9",
		})
	} else {
		findings = append(findings, healthyFinding("pg/migration-coherence", tierPage, "migration-schema-drift", target))
	}

	if dbVersion < requiredHead {
		findings = append(findings, finding{
			probeId: "pg/migration-coherence", tier: tierWarn,
			class: "migration-behind", target: target, sustain: 1,
			symptom:   fmt.Sprintf("database migration head %d is %d version(s) behind the code-required head %d", dbVersion, requiredHead-dbVersion, requiredHead),
			mechanism: "The checked source tree contains schema-dependent code newer than the production database. Starting that code before its append-only migrations finish can turn a safe online rollout into missing-column, missing-index, or duplicate-object failures.",
			baseline:  fmt.Sprintf("The database is at migration version %d before dependent services from this source tree become active.", requiredHead),
			observed:  fmt.Sprintf("db_version=%d code_required_version=%d lag=%d", dbVersion, requiredHead, requiredHead-dbVersion),
			action:    "Run the database migration phase from the exact service commit and require it to reach the code-required head before activating dependent taskworkers or APIs. If the numeric head advances while an artifact remains absent, treat migration-schema-drift as the blocking incident.",
			verify:    fmt.Sprintf("migration_audit reaches version %d and every versioned artifact in this signal exists before dependent services start.", requiredHead),
			playbook:  "SIGNALS.md §8.9",
		})
	} else {
		findings = append(findings, healthyFinding("pg/migration-coherence", tierWarn, "migration-behind", target))
	}

	return findings, nil
}

func migrationBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "yes":
		return true
	default:
		return false
	}
}
