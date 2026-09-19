package monitor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
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

// pg_get_indexdef is normalized to single spaces before it is compared with
// this catalog value. Pinning the complete definition (rather than only the
// relation name) prevents an invalid, not-yet-ready, partial, differently
// ordered, or otherwise look-alike index from arming the deadline scheduler.
const providerEgressHealthDeadlineIndexDefinition = "CREATE INDEX provider_egress_health_measured_at_client_id ON public.provider_egress_health USING btree (measured_at, client_id)"

const walletAuthChallengeAttemptAddressTimeIndexDefinition = "CREATE INDEX wallet_auth_challenge_attempt_client_address_hash_attempt_time ON public.wallet_auth_challenge_attempt USING btree (client_address_hash, attempt_time)"

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
	{name: "competition staging identity and initial guards", requiredVersion: 630, removedVersion: 652, rowColumn: 41},
	{name: "transfer_escrow_sweep.provider_payouts", requiredVersion: 631, rowColumn: 42},
	{name: "transfer_contract_unresolved_source_pair_create_time", requiredVersion: 632, rowColumn: 43},
	{name: "transfer_contract_unresolved_destination_pair_create_time", requiredVersion: 633, rowColumn: 44},
	{name: "transfer_contract_unresolved_payer_transfer_byte_count", requiredVersion: 634, rowColumn: 45},
	{name: "transfer_contract bounded open statistics", requiredVersion: 635, rowColumn: 46},
	{name: "network_onboarding_offer", requiredVersion: 636, rowColumn: 47},
	{name: "network_onboarding_apple_offer_code", requiredVersion: 637, rowColumn: 48},
	{name: "network_onboarding_apple_offer_code_available", requiredVersion: 638, rowColumn: 49},
	{name: "network_onboarding_event", requiredVersion: 639, rowColumn: 50},
	{name: "network_onboarding_event_network_id_at", requiredVersion: 640, rowColumn: 51},
	{name: "network_onboarding_event_name_at", requiredVersion: 641, rowColumn: 52},
	{name: "subscription_renewal.price_tier", requiredVersion: 642, rowColumn: 53},
	{name: "stripe_customer.billing_country", requiredVersion: 643, rowColumn: 54},
	{name: "network_onboarding", requiredVersion: 644, rowColumn: 55},
	{name: "network_onboarding_next_send_at", requiredVersion: 645, rowColumn: 56},
	{name: "network_onboarding_email", requiredVersion: 646, rowColumn: 57},
	{name: "network_onboarding_email_network_id_sent_at", requiredVersion: 647, rowColumn: 58},
	{name: "onboarding_results_daily", requiredVersion: 648, rowColumn: 59},
	{name: "network_onboarding_experiment_state", requiredVersion: 649, rowColumn: 60},
	{name: "network_onboarding_created_at", requiredVersion: 650, rowColumn: 61},
	{name: "signed client-key history tables and guards", requiredVersion: 651, rowColumn: 62},
	{name: "repeatable competition staging lifecycle", requiredVersion: 652, rowColumn: 63},
	{name: "competition_round.admission_closed_at and guard", requiredVersion: 653, rowColumn: 64},
	{name: "network_points_leaderboard_snapshot.epoch_metrics_available", requiredVersion: 654, rowColumn: 65},
	{name: "onboarding_email_tracker_daily", requiredVersion: 655, rowColumn: 66},
	{name: "network_onboarding_email_sent_at", requiredVersion: 656, rowColumn: 67},
	{name: "provider_egress_health measured_at/client_id deadline index", requiredVersion: 657, rowColumn: 68},
	{name: "network_client_connection.ip_version", requiredVersion: 658, rowColumn: 69},
	{name: "network_client_connection.ip_family_intent", requiredVersion: 659, rowColumn: 70},
	{name: "network_client_location_reliability.ipv4_proven", requiredVersion: 660, rowColumn: 71},
	{name: "network_client_location_reliability.ipv6_proven", requiredVersion: 661, rowColumn: 72},
	{name: "network_extender table and identity keys", requiredVersion: 662, rowColumn: 73},
	{name: "network_extender_address table and publish index", requiredVersion: 663, rowColumn: 74},
	{name: "network_extender_publish table and queue index", requiredVersion: 664, rowColumn: 75},
	{name: "network_client_connection.extender_id", requiredVersion: 665, rowColumn: 76},
	{name: "network_client_connection_client_id_connected_extender_id", requiredVersion: 666, rowColumn: 77},
	{name: "contract_extender table and primary key", requiredVersion: 667, rowColumn: 78},
	{name: "network_extender_address.dns_ports", requiredVersion: 668, rowColumn: 79},
	{name: "network_extender.location_id", requiredVersion: 669, rowColumn: 80},
	{name: "network_extender.city_location_id", requiredVersion: 670, rowColumn: 81},
	{name: "network_extender.region_location_id", requiredVersion: 671, rowColumn: 82},
	{name: "network_extender.country_location_id", requiredVersion: 672, rowColumn: 83},
	{name: "contract_extender.create_time", requiredVersion: 673, rowColumn: 84},
	{name: "contract_extender_create_time_contract_id", requiredVersion: 674, rowColumn: 85},
	{name: "wallet_auth_challenge_attempt_client_address_hash_attempt_time", requiredVersion: 675, rowColumn: 86},
	{name: "competition_round_baseline and append-only guards", requiredVersion: 676, rowColumn: 87},
	{name: "competition candidate absolute raw-score ordering", requiredVersion: 677, removedVersion: 678, rowColumn: 88},
	{name: "competition legacy/shared-control ranking policy", requiredVersion: 678, rowColumn: 89},
	{name: "network_extender_latency attestation table and identity key", requiredVersion: 679, rowColumn: 90},
	{name: "network_extender_latency_create_time retention index", requiredVersion: 680, rowColumn: 91},
	{name: "network_extender_latency_extender_id_create_time lookup index", requiredVersion: 681, rowColumn: 92},
	{name: "network_extender_activation history table and identity key", requiredVersion: 682, rowColumn: 93},
	{name: "network_extender_activation_extender_id_activate_time lookup index", requiredVersion: 683, rowColumn: 94},
}

func (migrationsProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	rows, err := env.runner.pg(ctx, `
		WITH version AS (
			SELECT coalesce(max(end_version_number), 0)::int AS value
			FROM migration_audit
			WHERE status = 'success'
		), index_artifact AS (
			SELECT table_relation.relname::text AS table_name,
			       index_relation.relname::text AS index_name,
			       regexp_replace(pg_get_indexdef(index_relation.oid), '[[:space:]]+', ' ', 'g') AS definition,
			       regexp_replace(
			           pg_get_expr(index_record.indpred, index_record.indrelid),
			           '[[:space:]]+', ' ', 'g'
			       ) AS predicate_definition,
			       index_record.indisvalid,
			       index_record.indisready
			FROM pg_index AS index_record
			JOIN pg_class AS index_relation ON index_relation.oid = index_record.indexrelid
			JOIN pg_class AS table_relation ON table_relation.oid = index_record.indrelid
			JOIN pg_namespace AS namespace ON namespace.oid = table_relation.relnamespace
			WHERE namespace.nspname = 'public'
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
		                       ('st_epoch', 'st_epoch_status', 'CREATE INDEX st_epoch_status ON public.st_epoch USING btree (deployment_key, status, epoch)'),
		                       ('st_publish', 'st_publish_epoch_kind', 'CREATE INDEX st_publish_epoch_kind ON public.st_publish USING btree (deployment_key, epoch, kind, create_time)'),
		                       ('st_event', 'st_event_kind_block', 'CREATE INDEX st_event_kind_block ON public.st_event USING btree (deployment_key, kind, block_number, log_index)'),
		                       ('st_payout_leaf', 'st_payout_leaf_client_epoch', 'CREATE INDEX st_payout_leaf_client_epoch ON public.st_payout_leaf USING btree (deployment_key, client_id, epoch, no_id)')
		               ) AS expected(table_name, index_name, definition)
		               WHERE EXISTS (
		                   SELECT 1 FROM index_artifact AS actual
		                   WHERE actual.table_name = expected.table_name
		                     AND actual.index_name = expected.index_name
		                     AND actual.definition = expected.definition
		                     AND actual.predicate_definition IS NULL
		                     AND actual.indisvalid AND actual.indisready
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
		             AND definition = 'CREATE UNIQUE INDEX st_transaction_intent_chain_account_nonce ON public.st_transaction_intent USING btree (chain_id, from_address, nonce)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_logical_generation'
		             AND definition = 'CREATE UNIQUE INDEX st_transaction_intent_logical_generation ON public.st_transaction_intent USING btree (logical_key, generation)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'st_transaction_intent'
		             AND index_name = 'st_transaction_intent_account_reconcile'
		             AND definition = 'CREATE INDEX st_transaction_intent_account_reconcile ON public.st_transaction_intent USING btree (chain_id, from_address, nonce) WHERE ((status)::text = ANY ((ARRAY[''prepared''::character varying, ''signed''::character varying, ''broadcast''::character varying, ''mined''::character varying, ''uncertain''::character varying])::text[]))'
		             AND predicate_definition = '((status)::text = ANY ((ARRAY[''prepared''::character varying, ''signed''::character varying, ''broadcast''::character varying, ''mined''::character varying, ''uncertain''::character varying])::text[]))'
		             AND indisvalid AND indisready
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
		             AND definition = 'CREATE UNIQUE INDEX st_transaction_intent_genesis_account_nonce ON public.st_transaction_intent USING btree (chain_id, genesis_hash, from_address, nonce)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
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
		             AND definition = 'CREATE INDEX st_transaction_intent_account_reconcile_v2 ON public.st_transaction_intent USING btree (chain_id, genesis_hash, from_address, nonce) WHERE ((status)::text = ANY ((ARRAY[''prepared''::character varying, ''signed''::character varying, ''broadcast''::character varying, ''mined''::character varying, ''uncertain''::character varying])::text[]))'
		             AND predicate_definition = '((status)::text = ANY ((ARRAY[''prepared''::character varying, ''signed''::character varying, ''broadcast''::character varying, ''mined''::character varying, ''uncertain''::character varying])::text[]))'
		             AND indisvalid AND indisready
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
		                 AND definition = 'CREATE INDEX st_fleet_binding_signature_network ON public.st_fleet_binding_signature USING btree (deployment_key, network_id, create_time DESC)'
		                 AND predicate_definition IS NULL
		                 AND indisvalid AND indisready
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
		             AND definition = 'CREATE INDEX transfer_contract_stream_id ON public.transfer_contract USING btree (stream_id) WHERE (stream_id IS NOT NULL)'
		             AND predicate_definition = '(stream_id IS NOT NULL)'
		             AND indisvalid AND indisready
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public' AND table_name = 'competition_round'
		                 AND column_name = 'staging' AND data_type = 'boolean'
		                 AND is_nullable = 'NO' AND column_default IS NULL
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'competition_round'
		                 AND constraint_name = 'competition_round_epoch_kind'
		                 AND constraint_type = 'c' AND validated
		                 AND definition LIKE '%epoch_number = 0%'
		                 AND definition LIKE '%epoch_number > 0%'
		           )
		           AND NOT EXISTS (
		               SELECT 1 FROM (VALUES
		                   ('competition_round', 'competition_round_staging_identity_immutable', 'competition_round_staging_identity_guard'),
		                   ('competition_candidate_review', 'competition_staging_candidate_review_blocked', 'competition_staging_candidate_review_guard'),
		                   ('competition_round', 'competition_staging_finalization_blocked', 'competition_staging_finalization_guard')
		               ) expected(table_name, trigger_name, function_name)
		               WHERE NOT EXISTS (
		                   SELECT 1 FROM pg_trigger trigger_record
		                   JOIN pg_class relation ON relation.oid = trigger_record.tgrelid
		                   JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		                   WHERE namespace.nspname = 'public' AND relation.relname = expected.table_name
		                     AND trigger_record.tgname = expected.trigger_name
		                     AND trigger_record.tgenabled = 'O'
		                     AND trigger_record.tgfoid = to_regprocedure('public.' || expected.function_name || '()')
		               )
		           )
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public' AND table_name = 'transfer_escrow_sweep'
		                 AND column_name = 'provider_payouts' AND data_type = 'jsonb'
		                 AND is_nullable = 'YES' AND column_default IS NULL
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'transfer_escrow_sweep'
		                 AND constraint_name = 'transfer_escrow_sweep_provider_payouts_shape'
		                 AND constraint_type = 'c'
		                 AND definition LIKE '%jsonb_typeof(provider_payouts)%'
		                 AND definition LIKE '%jsonb_array_length(provider_payouts) > 0%'
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'transfer_contract'
		             AND index_name = 'transfer_contract_unresolved_source_pair_create_time'
		             AND definition = 'CREATE INDEX transfer_contract_unresolved_source_pair_create_time ON public.transfer_contract USING btree (source_id, destination_id, create_time) INCLUDE (contract_id, companion_contract_id, transfer_byte_count, priority) WHERE ( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (source_id IS NOT NULL))'
		             AND predicate_definition = '( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (source_id IS NOT NULL))'
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'transfer_contract'
		             AND index_name = 'transfer_contract_unresolved_destination_pair_create_time'
		             AND definition = 'CREATE INDEX transfer_contract_unresolved_destination_pair_create_time ON public.transfer_contract USING btree (destination_id, source_id, create_time) INCLUDE (contract_id, companion_contract_id, transfer_byte_count, priority) WHERE ( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (destination_id IS NOT NULL))'
		             AND predicate_definition = '( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (destination_id IS NOT NULL))'
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'transfer_contract'
		             AND index_name = 'transfer_contract_unresolved_payer_transfer_byte_count'
		             AND definition = 'CREATE INDEX transfer_contract_unresolved_payer_transfer_byte_count ON public.transfer_contract USING btree (payer_network_id) INCLUDE (transfer_byte_count) WHERE ( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (payer_network_id IS NOT NULL))'
		             AND predicate_definition = '( CASE WHEN (outcome IS NULL) THEN (dispute = false) ELSE false END AND (payer_network_id IS NOT NULL))'
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1
		           FROM pg_attribute attribute_record
		           JOIN pg_class relation ON relation.oid = attribute_record.attrelid
		           JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		           WHERE namespace.nspname = 'public'
		             AND relation.relname = 'transfer_contract'
		             AND attribute_record.attname = 'open'
		             AND attribute_record.attstattarget = 300
		             AND coalesce(relation.reloptions, ARRAY[]::text[]) @> ARRAY[
		                 'autovacuum_analyze_scale_factor=0',
		                 'autovacuum_analyze_threshold=1000000'
		             ]::text[]
		       ),
		       to_regclass('public.network_onboarding_offer') IS NOT NULL,
		       to_regclass('public.network_onboarding_apple_offer_code') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding_apple_offer_code'
		             AND index_name = 'network_onboarding_apple_offer_code_available'
		             AND definition = 'CREATE INDEX network_onboarding_apple_offer_code_available ON public.network_onboarding_apple_offer_code USING btree (expires_at, code) WHERE (network_id IS NULL)'
		             AND predicate_definition = '(network_id IS NULL)'
		             AND indisvalid AND indisready
		       ),
		       to_regclass('public.network_onboarding_event') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding_event'
		             AND index_name = 'network_onboarding_event_network_id_at'
		             AND definition = 'CREATE INDEX network_onboarding_event_network_id_at ON public.network_onboarding_event USING btree (network_id, at)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding_event'
		             AND index_name = 'network_onboarding_event_name_at'
		             AND definition = 'CREATE INDEX network_onboarding_event_name_at ON public.network_onboarding_event USING btree (name, at)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'subscription_renewal'
		             AND column_name = 'price_tier' AND data_type = 'character varying'
		             AND character_maximum_length = 32 AND is_nullable = 'YES'
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'stripe_customer'
		             AND column_name = 'billing_country' AND data_type = 'character varying'
		             AND character_maximum_length = 2 AND is_nullable = 'YES'
		       ),
		       to_regclass('public.network_onboarding') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding'
		             AND index_name = 'network_onboarding_next_send_at'
		             AND definition = 'CREATE INDEX network_onboarding_next_send_at ON public.network_onboarding USING btree (next_send_at) WHERE (next_send_at IS NOT NULL)'
		             AND predicate_definition = '(next_send_at IS NOT NULL)'
		             AND indisvalid AND indisready
		       ),
		       to_regclass('public.network_onboarding_email') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding_email'
		             AND index_name = 'network_onboarding_email_network_id_sent_at'
		             AND definition = 'CREATE INDEX network_onboarding_email_network_id_sent_at ON public.network_onboarding_email USING btree (network_id, sent_at)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       to_regclass('public.onboarding_results_daily') IS NOT NULL,
		       to_regclass('public.network_onboarding_experiment_state') IS NOT NULL,
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding'
		             AND index_name = 'network_onboarding_created_at'
		             AND definition = 'CREATE INDEX network_onboarding_created_at ON public.network_onboarding USING btree (created_at)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       (
		           to_regclass('public.st_client_key_history') IS NOT NULL
		           AND to_regclass('public.st_client_key_head') IS NOT NULL
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'st_client_key_history' AND constraint_type = 'p'
		                 AND definition = 'PRIMARY KEY (client_id, generation)' AND validated
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'st_client_key_head' AND constraint_type = 'f'
		                 AND definition LIKE '%(client_id, generation)%st_client_key_history(client_id, generation)%'
		                 AND validated
		           )
		           AND NOT EXISTS (
		               SELECT 1 FROM (VALUES
		                   ('st_client_key_history', 'st_client_key_history_immutable', 'st_client_key_history_immutable_guard'),
		                   ('st_client_key_head', 'st_client_key_head_identity', 'st_client_key_head_identity_guard'),
		                   ('network_client', 'st_client_key_retire_on_client_delete', 'st_client_key_retire_deleted_client')
		               ) expected(table_name, trigger_name, function_name)
		               WHERE NOT EXISTS (
		                   SELECT 1 FROM pg_trigger trigger_record
		                   JOIN pg_class relation ON relation.oid = trigger_record.tgrelid
		                   JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		                   WHERE namespace.nspname = 'public' AND relation.relname = expected.table_name
		                     AND trigger_record.tgname = expected.trigger_name
		                     AND trigger_record.tgenabled = 'O'
		                     AND trigger_record.tgfoid = to_regprocedure('public.' || expected.function_name || '()')
		                     AND NOT trigger_record.tgisinternal
		               )
		           )
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'competition_round'
		                 AND constraint_name = 'competition_round_epoch_kind'
		                 AND constraint_type = 'c' AND validated
		                 AND definition LIKE '%epoch_number >= 0%'
		                 AND definition LIKE '%epoch_number > 0%'
		           )
		           AND EXISTS (
		               SELECT 1 FROM index_artifact
		               WHERE table_name = 'competition_round'
		                 AND index_name = 'competition_round_one_active_staging'
		                 AND definition = 'CREATE UNIQUE INDEX competition_round_one_active_staging ON public.competition_round USING btree (competition_id) WHERE ((staging = true) AND (canceled = false) AND (finalized_at IS NULL))'
		                 AND predicate_definition = '((staging = true) AND (canceled = false) AND (finalized_at IS NULL))'
		                 AND indisvalid AND indisready
		           )
		           AND NOT EXISTS (
		               SELECT 1 FROM pg_trigger trigger_record
		               JOIN pg_class relation ON relation.oid = trigger_record.tgrelid
		               JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		               WHERE namespace.nspname = 'public' AND relation.relname = 'competition_round'
		                 AND trigger_record.tgname = 'competition_staging_finalization_blocked'
		                 AND NOT trigger_record.tgisinternal
		           )
		       ),
		       (
		           EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public' AND table_name = 'competition_round'
		                 AND column_name = 'admission_closed_at'
		                 AND data_type = 'timestamp without time zone' AND is_nullable = 'YES'
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'competition_round'
		                 AND constraint_name = 'competition_round_admission_closed_kind'
		                 AND constraint_type = 'c' AND validated
		                 AND definition LIKE '%admission_closed_at%'
		           )
		           AND EXISTS (
		               SELECT 1 FROM pg_proc function_record
		               JOIN pg_namespace namespace ON namespace.oid = function_record.pronamespace
		               WHERE namespace.nspname = 'public'
		                 AND function_record.proname = 'competition_round_immutable_guard'
		                 AND pg_get_functiondef(function_record.oid) LIKE '%OLD.admission_closed_at%'
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public'
		             AND table_name = 'network_points_leaderboard_snapshot'
		             AND column_name = 'epoch_metrics_available'
		             AND data_type = 'boolean' AND is_nullable = 'NO'
		             AND column_default IN ('false', 'false::boolean', '''false''::boolean')
		       ),
		       (
		           to_regclass('public.onboarding_email_tracker_daily') IS NOT NULL
		           AND EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'onboarding_email_tracker_daily'
		                 AND column_name = 'attribution_ambiguous'
		                 AND data_type = 'bigint' AND is_nullable = 'NO'
		           )
		           AND NOT EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'onboarding_email_tracker_daily'
		                 AND column_name IN ('network_id', 'message_id', 'email_address')
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_onboarding_email'
		             AND index_name = 'network_onboarding_email_sent_at'
		             AND definition = 'CREATE INDEX network_onboarding_email_sent_at ON public.network_onboarding_email USING btree (sent_at, network_id, step)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'provider_egress_health'
		             AND index_name = 'provider_egress_health_measured_at_client_id'
		             AND definition = '`+providerEgressHealthDeadlineIndexDefinition+`'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_client_connection'
		             AND column_name = 'ip_version'
		             AND data_type = 'smallint' AND is_nullable = 'NO'
		             AND column_default IN ('0', '0::smallint', '''0''::smallint')
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_client_connection'
		             AND column_name = 'ip_family_intent'
		             AND data_type = 'smallint' AND is_nullable = 'NO'
		             AND column_default IN ('0', '0::smallint', '''0''::smallint')
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_client_location_reliability'
		             AND column_name = 'ipv4_proven'
		             AND data_type = 'boolean' AND is_nullable = 'NO'
		             AND column_default IN ('false', 'false::boolean', '''false''::boolean')
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_client_location_reliability'
		             AND column_name = 'ipv6_proven'
		             AND data_type = 'boolean' AND is_nullable = 'NO'
		             AND column_default IN ('false', 'false::boolean', '''false''::boolean')
		       ),
		       (
		           to_regclass('public.network_extender') IS NOT NULL
		           AND (
		               SELECT count(*) = 13
		               FROM (VALUES
		                   ('extender_id', 'uuid', 'NO'),
		                   ('network_id', 'uuid', 'NO'),
		                   ('client_id', 'uuid', 'NO'),
		                   ('public_key', 'bytea', 'NO'),
		                   ('create_time', 'timestamp without time zone', 'NO'),
		                   ('tcp_port', 'integer', 'NO'),
		                   ('udp_port', 'integer', 'NO'),
		                   ('dns_port', 'integer', 'NO'),
		                   ('dns_tld', 'character varying', 'NO'),
		                   ('country_code', 'character varying', 'NO'),
		                   ('active', 'boolean', 'NO'),
		                   ('revoke_time', 'timestamp without time zone', 'YES'),
		                   ('record_issue_time', 'timestamp without time zone', 'YES')
		               ) AS expected(column_name, data_type, is_nullable)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'network_extender'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		               )
		           )
		           AND (
		               SELECT count(*) = 2
		               FROM (VALUES
		                   ('p', 'PRIMARY KEY (extender_id)'),
		                   ('u', 'UNIQUE (public_key)')
		               ) AS expected(constraint_type, definition)
		               WHERE EXISTS (
		                   SELECT 1 FROM constraint_artifact AS actual
		                   WHERE actual.table_name = 'network_extender'
		                     AND actual.constraint_type = expected.constraint_type
		                     AND actual.definition = expected.definition
		                     AND actual.validated
		               )
		           )
		       ),
		       (
		           to_regclass('public.network_extender_address') IS NOT NULL
		           AND (
		               SELECT count(*) = 10
		               FROM (VALUES
		                   ('extender_id', 'uuid', 'NO'),
		                   ('ip_version', 'smallint', 'NO'),
		                   ('ip', 'inet', 'NO'),
		                   ('carriers', 'character varying', 'NO'),
		                   ('activate_time', 'timestamp without time zone', 'NO'),
		                   ('last_probe_time', 'timestamp without time zone', 'YES'),
		                   ('last_probe_success_time', 'timestamp without time zone', 'YES'),
		                   ('consecutive_probe_failures', 'integer', 'NO'),
		                   ('active', 'boolean', 'NO'),
		                   ('last_publish_time', 'timestamp without time zone', 'YES')
		               ) AS expected(column_name, data_type, is_nullable)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'network_extender_address'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		               )
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'network_extender_address'
		                 AND constraint_type = 'p'
		                 AND definition = 'PRIMARY KEY (extender_id, ip_version)'
		                 AND validated
		           )
		           AND EXISTS (
		               SELECT 1 FROM index_artifact
		               WHERE table_name = 'network_extender_address'
		                 AND index_name = 'network_extender_address_active_last_publish_time'
		                 AND definition = 'CREATE INDEX network_extender_address_active_last_publish_time ON public.network_extender_address USING btree (active, last_publish_time)'
		                 AND predicate_definition IS NULL
		                 AND indisvalid AND indisready
		           )
		       ),
		       (
		           to_regclass('public.network_extender_publish') IS NOT NULL
		           AND (
		               SELECT count(*) = 6
		               FROM (VALUES
		                   ('publish_id', 'uuid', 'NO'),
		                   ('extender_id', 'uuid', 'NO'),
		                   ('kind', 'smallint', 'NO'),
		                   ('message', 'bytea', 'NO'),
		                   ('create_time', 'timestamp without time zone', 'NO'),
		                   ('published_time', 'timestamp without time zone', 'YES')
		               ) AS expected(column_name, data_type, is_nullable)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'network_extender_publish'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		               )
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'network_extender_publish'
		                 AND constraint_type = 'p'
		                 AND definition = 'PRIMARY KEY (publish_id)'
		                 AND validated
		           )
		           AND EXISTS (
		               SELECT 1 FROM index_artifact
		               WHERE table_name = 'network_extender_publish'
		                 AND index_name = 'network_extender_publish_published_time_create_time'
		                 AND definition = 'CREATE INDEX network_extender_publish_published_time_create_time ON public.network_extender_publish USING btree (published_time, create_time)'
		                 AND predicate_definition IS NULL
		                 AND indisvalid AND indisready
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_client_connection'
		             AND column_name = 'extender_id'
		             AND data_type = 'uuid' AND is_nullable = 'YES'
		             AND column_default IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_client_connection'
		             AND index_name = 'network_client_connection_client_id_connected_extender_id'
		             AND definition = 'CREATE INDEX network_client_connection_client_id_connected_extender_id ON public.network_client_connection USING btree (client_id, connected, extender_id)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       (
		           to_regclass('public.contract_extender') IS NOT NULL
		           AND (
		               SELECT count(*) = 5
		               FROM (VALUES
		                   ('contract_id', 'uuid', 'NO'),
		                   ('extender_id', 'uuid', 'NO'),
		                   ('party', 'character varying', 'NO'),
		                   ('client_id', 'uuid', 'NO'),
		                   ('network_id', 'uuid', 'NO')
		               ) AS expected(column_name, data_type, is_nullable)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'contract_extender'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		               )
		           )
		           AND EXISTS (
		               SELECT 1 FROM information_schema.columns
		               WHERE table_schema = 'public' AND table_name = 'contract_extender'
		                 AND column_name = 'party'
		                 AND character_maximum_length = 16
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'contract_extender'
		                 AND constraint_type = 'p'
		                 AND definition = 'PRIMARY KEY (contract_id, extender_id, party)'
		                 AND validated
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_extender_address'
		             AND column_name = 'dns_ports'
		             AND data_type = 'character varying' AND is_nullable = 'NO'
		             AND column_default IN (
		                 quote_literal(''),
		                 quote_literal('') || '::character varying',
		                 quote_literal('') || '::text'
		             )
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_extender'
		             AND column_name = 'location_id'
		             AND data_type = 'uuid' AND is_nullable = 'YES'
		             AND column_default IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_extender'
		             AND column_name = 'city_location_id'
		             AND data_type = 'uuid' AND is_nullable = 'YES'
		             AND column_default IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_extender'
		             AND column_name = 'region_location_id'
		             AND data_type = 'uuid' AND is_nullable = 'YES'
		             AND column_default IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'network_extender'
		             AND column_name = 'country_location_id'
		             AND data_type = 'uuid' AND is_nullable = 'YES'
		             AND column_default IS NULL
		       ),
		       EXISTS (
		           SELECT 1 FROM information_schema.columns
		           WHERE table_schema = 'public' AND table_name = 'contract_extender'
		             AND column_name = 'create_time'
		             AND data_type = 'timestamp without time zone' AND is_nullable = 'NO'
		             AND column_default = 'now()'
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'contract_extender'
		             AND index_name = 'contract_extender_create_time_contract_id'
		             AND definition = 'CREATE INDEX contract_extender_create_time_contract_id ON public.contract_extender USING btree (create_time, contract_id)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'wallet_auth_challenge_attempt'
		             AND index_name = 'wallet_auth_challenge_attempt_client_address_hash_attempt_time'
		             AND definition = 'CREATE INDEX wallet_auth_challenge_attempt_client_address_hash_attempt_time ON public.wallet_auth_challenge_attempt USING btree (client_address_hash, attempt_time)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       (
		           to_regclass('public.competition_round_baseline') IS NOT NULL
		           AND (
		               SELECT count(*) = 13
		               FROM (VALUES
		                   ('round_id', 'uuid', 'NO'),
		                   ('baseline_json', 'bytea', 'NO'),
		                   ('baseline_sha256', 'character', 'NO'),
		                   ('source_job_id', 'uuid', 'NO'),
		                   ('source_attempt', 'integer', 'NO'),
		                   ('source_artifact_manifest_sha256', 'character', 'NO'),
		                   ('base_sha', 'character', 'NO'),
		                   ('providers_sha256', 'character', 'NO'),
		                   ('evaluator_image_digest', 'character varying', 'NO'),
		                   ('scorer_version', 'character varying', 'NO'),
		                   ('hardware_id', 'character varying', 'NO'),
		                   ('host_qualification_sha256', 'character', 'NO'),
		                   ('created_at', 'timestamp without time zone', 'NO')
		               ) AS expected(column_name, data_type, is_nullable)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'competition_round_baseline'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		               )
		           )
		           AND (
		               SELECT count(*) = 13
		               FROM information_schema.columns
		               WHERE table_schema = 'public'
		                 AND table_name = 'competition_round_baseline'
		           )
		           AND (
		               SELECT count(*) = 3
		               FROM (VALUES
		                   ('p', 'PRIMARY KEY (round_id)'),
		                   ('f', 'FOREIGN KEY (round_id) REFERENCES competition_round(round_id)'),
		                   ('f', 'FOREIGN KEY (source_job_id) REFERENCES competition_job(job_id)')
		               ) AS expected(constraint_type, definition)
		               WHERE EXISTS (
		                   SELECT 1 FROM constraint_artifact AS actual
		                   WHERE actual.table_name = 'competition_round_baseline'
		                     AND actual.constraint_type = expected.constraint_type
		                     AND actual.definition = expected.definition
		                     AND actual.validated
		               )
		           )
		           AND to_regprocedure('public.competition_round_baseline_insert_guard()') IS NOT NULL
		           AND (
		               SELECT count(*) = 2
		               FROM (VALUES
		                   ('competition_round_baseline_source_guard', 'public.competition_round_baseline_insert_guard()'),
		                   ('competition_round_baseline_append_only', 'public.competition_append_only_guard()')
		               ) AS expected(trigger_name, function_name)
		               WHERE EXISTS (
		                   SELECT 1
		                   FROM pg_trigger AS actual
		                   JOIN pg_class AS relation ON relation.oid = actual.tgrelid
		                   JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		                   WHERE namespace.nspname = 'public'
		                     AND relation.relname = 'competition_round_baseline'
		                     AND actual.tgname = expected.trigger_name
		                     AND actual.tgfoid = expected.function_name::regprocedure
		                     AND actual.tgenabled <> 'D'
		                     AND NOT actual.tgisinternal
		               )
		           )
		       ),
		       (
		           to_regprocedure('public.competition_candidate_review_insert_guard()') IS NOT NULL
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%ORDER BY (score_json->>''raw_score'')::numeric ASC%'
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%submitted_at, job_id%'
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) NOT LIKE '%normalized_score%'
		       ),
		       (
		           to_regprocedure('public.competition_candidate_review_insert_guard()') IS NOT NULL
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%ORDER BY CASE WHEN NOT EXISTS (%'
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%FROM competition_round_baseline WHERE round_id = NEW.round_id%'
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%THEN (score_json->>''normalized_score'')::numeric END DESC%'
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%(score_json->>''raw_score'')::numeric ASC%'
		           AND pg_get_functiondef(
		               'public.competition_candidate_review_insert_guard()'::regprocedure
		           ) LIKE '%submitted_at, job_id%'
		           AND to_regprocedure('public.competition_round_baseline_insert_guard()') IS NOT NULL
		           AND pg_get_functiondef(
		               'public.competition_round_baseline_insert_guard()'::regprocedure
		           ) LIKE '%competition round baseline cannot be attached after finalization or cancellation%'
		           AND pg_get_functiondef(
		               'public.competition_round_baseline_insert_guard()'::regprocedure
		           ) LIKE '%competition round baseline cannot replace legacy ranking policy%'
		           AND pg_get_functiondef(
		               'public.competition_round_baseline_insert_guard()'::regprocedure
		           ) LIKE '%state = ''running'' AND attempt_count >= NEW.source_attempt%'
		       ),
		       (
		           to_regclass('public.network_extender_latency') IS NOT NULL
		           AND (
		               SELECT count(*) = 7
		               FROM (VALUES
		                   ('latency_id', 'uuid', 'NO'),
		                   ('extender_id', 'uuid', 'NO'),
		                   ('client_id', 'uuid', 'NO'),
		                   ('probe_nonce', 'bytea', 'NO'),
		                   ('rtt_ms', 'integer', 'NO'),
		                   ('probe_time', 'timestamp without time zone', 'NO'),
		                   ('create_time', 'timestamp without time zone', 'NO')
		               ) AS expected(column_name, data_type, is_nullable)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'network_extender_latency'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		               )
		           )
		           AND (
		               SELECT count(*) = 2
		               FROM (VALUES
		                   ('p', 'PRIMARY KEY (latency_id)'),
		                   ('u', 'UNIQUE (extender_id, client_id, probe_nonce)')
		               ) AS expected(constraint_type, definition)
		               WHERE EXISTS (
		                   SELECT 1 FROM constraint_artifact AS actual
		                   WHERE actual.table_name = 'network_extender_latency'
		                     AND actual.constraint_type = expected.constraint_type
		                     AND actual.definition = expected.definition
		                     AND actual.validated
		               )
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_extender_latency'
		             AND index_name = 'network_extender_latency_create_time'
		             AND definition = 'CREATE INDEX network_extender_latency_create_time ON public.network_extender_latency USING btree (create_time)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_extender_latency'
		             AND index_name = 'network_extender_latency_extender_id_create_time'
		             AND definition = 'CREATE INDEX network_extender_latency_extender_id_create_time ON public.network_extender_latency USING btree (extender_id, create_time)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
		       ),
		       (
		           to_regclass('public.network_extender_activation') IS NOT NULL
		           AND (
		               SELECT count(*) = 10
		               FROM (VALUES
		                   ('activation_id', 'uuid', 'NO', NULL),
		                   ('extender_id', 'uuid', 'NO', NULL),
		                   ('activate_time', 'timestamp without time zone', 'NO', NULL),
		                   ('ip_version', 'integer', 'NO', NULL),
		                   ('client_address_hash', 'bytea', 'YES', NULL),
		                   ('country_code', 'character varying', 'NO', quote_literal('') || '::character varying'),
		                   ('location_id', 'uuid', 'YES', NULL),
		                   ('city_location_id', 'uuid', 'YES', NULL),
		                   ('region_location_id', 'uuid', 'YES', NULL),
		                   ('country_location_id', 'uuid', 'YES', NULL)
		               ) AS expected(column_name, data_type, is_nullable, column_default)
		               WHERE EXISTS (
		                   SELECT 1 FROM information_schema.columns AS actual
		                   WHERE actual.table_schema = 'public'
		                     AND actual.table_name = 'network_extender_activation'
		                     AND actual.column_name = expected.column_name
		                     AND actual.data_type = expected.data_type
		                     AND actual.is_nullable = expected.is_nullable
		                     AND actual.column_default IS NOT DISTINCT FROM expected.column_default
		                     AND actual.character_maximum_length IS NULL
		               )
		           )
		           AND EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'network_extender_activation'
		                 AND constraint_type = 'p'
		                 AND definition = 'PRIMARY KEY (activation_id)'
		                 AND validated
		           )
		       ),
		       EXISTS (
		           SELECT 1 FROM index_artifact
		           WHERE table_name = 'network_extender_activation'
		             AND index_name = 'network_extender_activation_extender_id_activate_time'
		             AND definition = 'CREATE INDEX network_extender_activation_extender_id_activate_time ON public.network_extender_activation USING btree (extender_id, activate_time)'
		             AND predicate_definition IS NULL
		             AND indisvalid AND indisready
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
				SELECT migration_index, trim(identity_sha256)
				FROM migration_catalog
				ORDER BY migration_catalog.migration_index;
			`)
			if catalogErr != nil {
				return nil, catalogErr
			}
			catalogIdentities := make(map[int]string, len(catalogRows))
			catalogComplete := len(catalogRows) == dbVersion
			for _, row := range catalogRows {
				if len(row) != 2 {
					catalogComplete = false
					break
				}
				index, parseErr := strconv.Atoi(row.str(0))
				_, duplicate := catalogIdentities[index]
				if parseErr != nil || index < 0 || dbVersion <= index || duplicate {
					catalogComplete = false
					break
				}
				catalogIdentities[index] = row.str(1)
			}
			if !catalogComplete || len(catalogIdentities) != dbVersion {
				missing = append(missing, "migration_catalog identities@v600")
			} else {
				compareCount := min(dbVersion, requiredHead)
				for index := 0; index < compareCount; index++ {
					expectedIdentity, identityErr := server.MigrationIdentity(index)
					if identityErr != nil {
						return nil, identityErr
					}
					if strings.TrimSpace(catalogIdentities[index]) != expectedIdentity {
						missing = append(missing, fmt.Sprintf("migration_catalog identity[%d]@v600", index))
						break
					}
				}
			}
		}
	}

	findings := make([]finding, 0, 2)
	if len(missing) > 0 {
		findings = append(findings, finding{
			probeId: "pg/migration-coherence", tier: tierPage,
			class: "migration-schema-drift", target: target, sustain: 1,
			symptom:   fmt.Sprintf("database migration audit is at version %d but %d published schema artifact(s) are absent or incompatible", dbVersion, len(missing)),
			mechanism: "The recorded head disagrees with a required published artifact or immutable migration identity. An absent, look-alike, or non-operative index is not proof that migration history was reordered: compare the original published identities and exact live schema definition before distinguishing skipped history from later schema drift. A service that trusts only the numeric head can execute against missing or incompatible schema, or replay an older non-idempotent migration into existing objects.",
			baseline:  fmt.Sprintf("Every required published artifact through recorded database version %d matches its operative definition; checked indexes are valid and ready, and migration versions never move after release.", dbVersion),
			observed:  fmt.Sprintf("db_version=%d code_required_version=%d missing=%s", dbVersion, requiredHead, strings.Join(missing, ",")),
			action:    "Stop dependent service activation. Compare original published migration identities with the exact live artifact definitions first. Restore each reordered published migration to its original index if that history defect is proved; append reviewed corrective migrations after the published sequence for schema drift, and apply the corrected stream. Do not edit migration_audit or create production objects by hand merely to clear this alert.",
			verify:    "The recorded head advances only through the corrected append-only stream, every required artifact matches its published operative definition and checked indexes are valid and ready, and a fresh migration-coherence run has no schema-drift alert.",
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
