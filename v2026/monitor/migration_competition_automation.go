// Published automation artifacts retain their original shape until an explicit
// successor changes it. All predicates are catalog reads within §8.9's query.
package monitor

import (
	"fmt"
	"strings"
)

// The successor's own contract becomes mandatory at the same recorded head.
func migrationCompetitionBefore(version int, predicate string) string {
	return fmt.Sprintf("((SELECT value FROM version) >= %d OR %s)", version, predicate)
}

// Deferred commit guards must keep the operative event and exact function,
// including the deferred constraint-trigger semantics their writers require.
func migrationCompetitionDeferredTrigger(table, trigger, function string, mask int) string {
	return strings.Replace(snMainnetMigrationTrigger(table, trigger, function, mask),
		"AND NOT t.tgdeferrable AND NOT t.tginitdeferred",
		"AND t.tgdeferrable AND t.tginitdeferred", 1)
}

// Dropping a serial column's default does not remove its owned sequence:
// the later trigger assigns it only after acquiring the scope lock.
func migrationCompetitionSequence(table, column string) string {
	return fmt.Sprintf(`EXISTS(SELECT 1 FROM pg_class s
 JOIN pg_namespace n ON n.oid=s.relnamespace
 JOIN pg_sequence q ON q.seqrelid=s.oid
 JOIN pg_depend d ON d.classid='pg_class'::regclass AND d.objid=s.oid
 JOIN pg_attribute a ON a.attrelid=d.refobjid AND a.attnum=d.refobjsubid
 WHERE n.nspname='public' AND s.relname=%s AND s.relkind='S' AND s.relpersistence='p'
 AND d.refclassid='pg_class'::regclass AND d.deptype='a'
 AND a.attrelid=to_regclass(%s) AND a.attname=%s AND NOT a.attisdropped
 AND q.seqtypid='bigint'::regtype AND q.seqincrement=1 AND q.seqmin=1
 AND q.seqmax=9223372036854775807 AND q.seqstart=1 AND q.seqcache=1 AND NOT q.seqcycle)`,
		snMainnetMigrationLiteral(table+"_"+column+"_seq"),
		snMainnetMigrationLiteral("public."+table), snMainnetMigrationLiteral(column))
}

var competitionMigration842ArtifactQuery = "(" +
	snMainnetMigrationColumns("competition_round",
		snMainnetMigrationColumn{name: "automation_sealed_at", kind: "timestamp without time zone"},
		snMainnetMigrationColumn{name: "automation_publication_at", kind: "timestamp without time zone"}) + " AND " +
	migrationFp2Table("competition_evaluation_seal") + " AND " +
	snMainnetMigrationColumns("competition_evaluation_seal",
		snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "sealed_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "publication_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "seal_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "seal_json", kind: "jsonb", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_evaluation_seal", "PRIMARY KEY (round_id)", "CHECK ((seal_sha256 ~ '^[0-9a-f]{64}$'::text))", "CHECK ((jsonb_typeof(seal_json) = 'object'::text))", "CHECK ((sealed_at <= publication_at))") + " AND " +
	snMainnetMigrationForeignKey("competition_evaluation_seal", "competition_round", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_evaluation_seal", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)") + " AND " +
	migrationFp2Table("competition_reward_decision") + " AND " +
	snMainnetMigrationColumns("competition_reward_decision",
		snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "revision", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "decision_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "previous_decision_sha256", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "decision_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_reward_decision", "UNIQUE (decision_sha256)", "PRIMARY KEY (round_id, revision)", "CHECK ((revision > 0))", "CHECK ((decision_sha256 ~ '^[0-9a-f]{64}$'::text))") + " AND " +
	snMainnetMigrationForeignKey("competition_reward_decision", "competition_evaluation_seal", "FOREIGN KEY (round_id) REFERENCES competition_evaluation_seal(round_id)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_reward_decision", "FOREIGN KEY (round_id) REFERENCES competition_evaluation_seal(round_id)") + " AND " +
	migrationFp2Table("competition_integrity_review") + " AND " +
	migrationCompetitionBefore(845, snMainnetMigrationColumns("competition_integrity_review",
		snMainnetMigrationColumn{name: "review_cursor", kind: "bigint", notNull: true, defaultExpression: "nextval('competition_integrity_review_review_cursor_seq'::regclass)"})) + " AND " +
	migrationCompetitionSequence("competition_integrity_review", "review_cursor") + " AND " +
	snMainnetMigrationColumns("competition_integrity_review",
		snMainnetMigrationColumn{name: "review_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "job_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "actor_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "request_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "review_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_integrity_review", "UNIQUE (review_cursor)", "PRIMARY KEY (review_id)", "UNIQUE (round_id, actor_id, idempotency_key)") + " AND " +
	snMainnetMigrationForeignKey("competition_integrity_review", "competition_evaluation_seal", "FOREIGN KEY (round_id) REFERENCES competition_evaluation_seal(round_id)") + " AND " +
	snMainnetMigrationForeignKey("competition_integrity_review", "competition_job", "FOREIGN KEY (job_id) REFERENCES competition_job(job_id)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_integrity_review", "FOREIGN KEY (round_id) REFERENCES competition_evaluation_seal(round_id)", "FOREIGN KEY (job_id) REFERENCES competition_job(job_id)") + " AND " +
	migrationFp2Table("competition_reward_outbox") + " AND " +
	migrationCompetitionBefore(844, snMainnetMigrationColumns("competition_reward_outbox",
		snMainnetMigrationColumn{name: "event_cursor", kind: "bigint", notNull: true, defaultExpression: "nextval('competition_reward_outbox_event_cursor_seq'::regclass)"})) + " AND " +
	migrationCompetitionSequence("competition_reward_outbox", "event_cursor") + " AND " +
	snMainnetMigrationColumns("competition_reward_outbox",
		snMainnetMigrationColumn{name: "event_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "revision", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "payload_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "payload_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "publication_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_reward_outbox", "UNIQUE (event_cursor)", "PRIMARY KEY (event_id)", "UNIQUE (round_id, revision)") + " AND " +
	snMainnetMigrationForeignKey("competition_reward_outbox", "competition_reward_decision", "FOREIGN KEY (round_id, revision) REFERENCES competition_reward_decision(round_id, revision)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_reward_outbox", "FOREIGN KEY (round_id, revision) REFERENCES competition_reward_decision(round_id, revision)") + " AND " +
	migrationFp2Table("competition_settlement_receipt") + " AND " +
	snMainnetMigrationColumns("competition_settlement_receipt",
		snMainnetMigrationColumn{name: "event_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "actor_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "status", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "payload_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "receipt_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_settlement_receipt", "PRIMARY KEY (event_id, actor_id, status)", "CHECK (((status)::text = ANY ((ARRAY['received'::character varying, 'applied'::character varying, 'confirmed'::character varying, 'rejected'::character varying])::text[])))") + " AND " +
	snMainnetMigrationForeignKey("competition_settlement_receipt", "competition_reward_outbox", "FOREIGN KEY (event_id) REFERENCES competition_reward_outbox(event_id)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_settlement_receipt", "FOREIGN KEY (event_id) REFERENCES competition_reward_outbox(event_id)") + " AND " +
	migrationFp2Table("competition_source_activation") + " AND " +
	migrationCompetitionBefore(845, snMainnetMigrationConstraints("competition_source_activation", "UNIQUE (environment, competition_id, season_id, target_epoch)")) + " AND " +
	snMainnetMigrationColumns("competition_source_activation",
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "target_epoch", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "intent_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "intent_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "activated_at", kind: "timestamp without time zone"}) + " AND " +
	snMainnetMigrationConstraints("competition_source_activation", "PRIMARY KEY (environment, competition_id, season_id, generation)", "CHECK ((generation > 0))", "CHECK (((target_epoch >= 1) AND (target_epoch <= 6)))") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_source_activation") + " AND " +
	migrationFp2Table("competition_source_readiness") + " AND " +
	snMainnetMigrationColumns("competition_source_readiness",
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "role", kind: "character varying(16)", notNull: true},
		snMainnetMigrationColumn{name: "receipt_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_source_readiness", "PRIMARY KEY (environment, competition_id, season_id, generation, role)", "CHECK (((role)::text = ANY ((ARRAY['api'::character varying, 'worker'::character varying])::text[])))") + " AND " +
	snMainnetMigrationForeignKey("competition_source_readiness", "competition_source_activation", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_source_readiness", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)") + " AND " +
	migrationFp2Table("competition_source_pointer") + " AND " +
	snMainnetMigrationColumns("competition_source_pointer",
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_source_pointer", "PRIMARY KEY (environment, competition_id, season_id)") + " AND " +
	snMainnetMigrationForeignKey("competition_source_pointer", "competition_source_activation", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_source_pointer", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)") + " AND " +
	migrationFp2Table("competition_supervisor_lease") + " AND " +
	snMainnetMigrationColumns("competition_supervisor_lease",
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "owner_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "fencing_token", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "lease_expires_at", kind: "timestamp without time zone", notNull: true},
		snMainnetMigrationColumn{name: "heartbeat_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_supervisor_lease", "PRIMARY KEY (environment, competition_id, season_id)", "CHECK ((fencing_token > 0))") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_supervisor_lease") + " AND " +
	migrationFp2Table("competition_supervisor_transition") + " AND " +
	snMainnetMigrationColumns("competition_supervisor_transition",
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "target_epoch", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "revision", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "state", kind: "character varying(64)", notNull: true},
		snMainnetMigrationColumn{name: "transition_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "updated_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_supervisor_transition", "PRIMARY KEY (environment, competition_id, season_id, target_epoch, revision)", "CHECK (((target_epoch >= 1) AND (target_epoch <= 7)))", "CHECK ((revision > 0))") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_supervisor_transition") + " AND " +
	migrationFp2Index("competition_integrity_review", "competition_integrity_review_latest", "round_id, job_id, review_cursor DESC", "", false) + " AND " +
	snMainnetMigrationFunction("competition_v2_score_eligible(jsonb,double precision,double precision)", "boolean", "sql", "i", false, competitionMigration842CompetitionV2ScoreEligibleBody, "score", "baseline_raw", "margin") + " AND " +
	snMainnetMigrationFunction("competition_v2_evidence_guard()", "trigger", "plpgsql", "v", false, competitionMigration842CompetitionV2EvidenceGuardBody) + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_round_guard()", "trigger", "plpgsql", "v", false, competitionMigration842CompetitionV2RoundGuardBody)) + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_seal_guard()", "trigger", "plpgsql", "v", false, competitionMigration842CompetitionV2SealGuardBody)) + " AND " +
	migrationCompetitionBefore(843, snMainnetMigrationFunction("competition_v2_decision_guard()", "trigger", "plpgsql", "v", false, competitionMigration842CompetitionV2DecisionGuardBody)) + " AND " +
	snMainnetMigrationFunction("competition_v2_activation_guard()", "trigger", "plpgsql", "v", false, competitionMigration842CompetitionV2ActivationGuardBody) + " AND " +
	snMainnetMigrationTrigger("competition_job", "competition_v2_job_fence", "competition_v2_evidence_guard", 31) + " AND " +
	snMainnetMigrationTrigger("competition_round_baseline", "competition_v2_baseline_fence", "competition_v2_evidence_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_round", "competition_v2_round_fence", "competition_v2_round_guard", 19) + " AND " +
	snMainnetMigrationTrigger("competition_evaluation_seal", "competition_v2_seal_insert", "competition_v2_seal_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_reward_decision", "competition_v2_decision_insert", "competition_v2_decision_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_source_activation", "competition_v2_activation_immutable", "competition_v2_activation_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_evaluation_seal", "competition_v2_seal_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_reward_decision", "competition_v2_decision_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_review_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_reward_outbox", "competition_v2_outbox_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_settlement_receipt", "competition_v2_receipt_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_source_readiness", "competition_v2_readiness_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_supervisor_transition", "competition_v2_transition_immutable", "competition_append_only_guard", 27) + ")"

var competitionMigration843ArtifactQuery = "(" +
	migrationFp2Table("competition_automation_hold") + " AND " +
	snMainnetMigrationColumns("competition_automation_hold",
		snMainnetMigrationColumn{name: "hold_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "kind", kind: "character varying(32)", notNull: true},
		snMainnetMigrationColumn{name: "revision", kind: "integer", notNull: true},
		snMainnetMigrationColumn{name: "active", kind: "boolean", notNull: true},
		snMainnetMigrationColumn{name: "actor_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "request_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "hold_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_automation_hold", "PRIMARY KEY (hold_id)", "UNIQUE (environment, competition_id, season_id, kind, revision)", "UNIQUE (environment, competition_id, season_id, actor_id, idempotency_key)", "CHECK (((kind)::text = ANY ((ARRAY['admission'::character varying, 'evaluation'::character varying, 'source_activation'::character varying, 'reward_delivery'::character varying])::text[])))", "CHECK ((revision > 0))") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_automation_hold") + " AND " +
	migrationFp2Table("competition_automation_incident") + " AND " +
	snMainnetMigrationColumns("competition_automation_incident",
		snMainnetMigrationColumn{name: "incident_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "round_id", kind: "uuid", notNull: true},
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "invalidates_evaluation", kind: "boolean", notNull: true},
		snMainnetMigrationColumn{name: "actor_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "idempotency_key", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "request_sha256", kind: "character(64)", notNull: true},
		snMainnetMigrationColumn{name: "incident_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_automation_incident", "PRIMARY KEY (incident_id)", "UNIQUE (round_id, actor_id, idempotency_key)") + " AND " +
	snMainnetMigrationForeignKey("competition_automation_incident", "competition_round", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_automation_incident", "FOREIGN KEY (round_id) REFERENCES competition_round(round_id)") + " AND " +
	migrationFp2Index("competition_automation_incident", "competition_automation_incident_round", "round_id", "", false) + " AND " +
	snMainnetMigrationFunction("competition_v2_decision_guard()", "trigger", "plpgsql", "v", false, competitionMigration843CompetitionV2DecisionGuardBody) + " AND " +
	snMainnetMigrationTrigger("competition_automation_hold", "competition_automation_hold_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_automation_incident", "competition_automation_incident_immutable", "competition_append_only_guard", 27) + ")"

var competitionMigration844ArtifactQuery = "(" +
	snMainnetMigrationColumns("competition_reward_outbox",
		snMainnetMigrationColumn{name: "event_cursor", kind: "bigint", notNull: true}) + " AND " +
	snMainnetMigrationFunction("competition_v2_scope_lock(text,text,text)", "void", "sql", "v", false, competitionMigration844CompetitionV2ScopeLockBody, "env", "competition", "season") + " AND " +
	snMainnetMigrationFunction("competition_v2_require_source_lease(text,text,text)", "void", "plpgsql", "v", false, competitionMigration844CompetitionV2RequireSourceLeaseBody, "env", "competition", "season") + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_source_authorized(jsonb,text,text,text)", "void", "plpgsql", "v", false, competitionMigration844CompetitionV2SourceAuthorizedBody, "intent", "env", "competition", "season")) + " AND " +
	migrationCompetitionBefore(845, snMainnetMigrationFunction("competition_v2_source_insert_or_activate_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2SourceInsertOrActivateGuardBody)) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_readiness_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2SourceReadinessGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_pointer_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2SourcePointerGuardBody) + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_round_insert_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2RoundInsertGuardBody)) + " AND " +
	snMainnetMigrationFunction("competition_v2_review_identity_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2ReviewIdentityGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_outbox_identity_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2OutboxIdentityGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_decision_outbox_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2DecisionOutboxGuardBody) + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_receipt_identity_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2ReceiptIdentityGuardBody)) + " AND " +
	snMainnetMigrationFunction("competition_v2_seal_control_guard()", "trigger", "plpgsql", "v", false, competitionMigration844CompetitionV2SealControlGuardBody) + " AND " +
	snMainnetMigrationTrigger("competition_source_activation", "competition_v2_source_authority", "competition_v2_source_insert_or_activate_guard", 23) + " AND " +
	snMainnetMigrationTrigger("competition_source_readiness", "competition_v2_readiness_identity", "competition_v2_source_readiness_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_source_pointer", "competition_v2_pointer_authority", "competition_v2_source_pointer_guard", 31) + " AND " +
	snMainnetMigrationTrigger("competition_round", "competition_v2_round_source", "competition_v2_round_insert_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_review_identity", "competition_v2_review_identity_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_reward_outbox", "competition_v2_outbox_identity", "competition_v2_outbox_identity_guard", 7) + " AND " +
	migrationCompetitionDeferredTrigger("competition_reward_decision", "competition_v2_decision_outbox", "competition_v2_decision_outbox_guard", 5) + " AND " +
	snMainnetMigrationTrigger("competition_settlement_receipt", "competition_v2_receipt_identity", "competition_v2_receipt_identity_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_evaluation_seal", "competition_v2_seal_control", "competition_v2_seal_control_guard", 7) + ")"

var competitionMigration845ArtifactQuery = "(" +
	snMainnetMigrationColumns("competition_integrity_review",
		snMainnetMigrationColumn{name: "review_cursor", kind: "bigint", notNull: true}) + " AND " +
	migrationFp2Table("competition_source_abandonment") + " AND " +
	snMainnetMigrationColumns("competition_source_abandonment",
		snMainnetMigrationColumn{name: "environment", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "competition_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "season_id", kind: "character varying(128)", notNull: true},
		snMainnetMigrationColumn{name: "generation", kind: "bigint", notNull: true},
		snMainnetMigrationColumn{name: "abandonment_json", kind: "jsonb", notNull: true},
		snMainnetMigrationColumn{name: "created_at", kind: "timestamp without time zone", notNull: true}) + " AND " +
	snMainnetMigrationConstraints("competition_source_abandonment", "PRIMARY KEY (environment, competition_id, season_id, generation)") + " AND " +
	snMainnetMigrationForeignKey("competition_source_abandonment", "competition_source_activation", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)") + " AND " +
	snMainnetMigrationForeignKeyCensus("competition_source_abandonment", "FOREIGN KEY (environment, competition_id, season_id, generation) REFERENCES competition_source_activation(environment, competition_id, season_id, generation)") + " AND " +
	snMainnetMigrationColumns("competition_source_activation",
		snMainnetMigrationColumn{name: "abandoned_at", kind: "timestamp without time zone"}) + " AND " +
	`NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid=to_regclass('public.competition_source_activation') AND contype='u')` + " AND " +
	migrationFp2Index("competition_source_activation", "competition_source_target_active", "environment, competition_id, season_id, target_epoch", "(abandoned_at IS NULL)", true) + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_source_abandonment_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2SourceAbandonmentGuardBody)) + " AND " +
	migrationCompetitionBefore(847, snMainnetMigrationFunction("competition_v2_source_insert_or_activate_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2SourceInsertOrActivateGuardBody)) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_abandonment_committed_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2SourceAbandonmentCommittedGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_not_abandoned_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2SourceNotAbandonedGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_review_decision_committed_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2ReviewDecisionCommittedGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_ledger_scope_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2LedgerScopeGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_worker_write_guard()", "trigger", "plpgsql", "v", false, competitionMigration845CompetitionV2WorkerWriteGuardBody) + " AND " +
	snMainnetMigrationTrigger("competition_source_abandonment", "competition_v2_abandonment_immutable", "competition_append_only_guard", 27) + " AND " +
	snMainnetMigrationTrigger("competition_source_abandonment", "competition_v2_abandonment_identity", "competition_v2_source_abandonment_guard", 7) + " AND " +
	migrationCompetitionDeferredTrigger("competition_source_abandonment", "competition_v2_abandonment_committed", "competition_v2_source_abandonment_committed_guard", 5) + " AND " +
	snMainnetMigrationTrigger("competition_source_readiness", "competition_v2_readiness_not_abandoned", "competition_v2_source_not_abandoned_guard", 7) + " AND " +
	migrationCompetitionDeferredTrigger("competition_integrity_review", "competition_v2_review_decision_committed", "competition_v2_review_decision_committed_guard", 5) + " AND " +
	snMainnetMigrationTrigger("competition_integrity_review", "competition_v2_review_aa_scope", "competition_v2_ledger_scope_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_reward_decision", "competition_v2_decision_aa_scope", "competition_v2_ledger_scope_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_job", "competition_v2_worker_execution", "competition_v2_worker_write_guard", 23) + ")"

var competitionMigration846ArtifactQuery = "(" +
	migrationFp2Index("competition_source_activation", "competition_source_reserved_round", "((intent_json ->> 'target_round_id'::text))", "((intent_json ? 'target_round_id'::text) AND ((intent_json ->> 'target_round_id'::text) <> '00000000-0000-0000-0000-000000000000'::text))", true) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_target_guard()", "trigger", "plpgsql", "v", false, competitionMigration846CompetitionV2SourceTargetGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_readiness_target_guard()", "trigger", "plpgsql", "v", false, competitionMigration846CompetitionV2ReadinessTargetGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_round_reserved_identity_guard()", "trigger", "plpgsql", "v", false, competitionMigration846CompetitionV2RoundReservedIdentityGuardBody) + " AND " +
	snMainnetMigrationTrigger("competition_source_activation", "competition_v2_source_target", "competition_v2_source_target_guard", 23) + " AND " +
	snMainnetMigrationTrigger("competition_source_readiness", "competition_v2_readiness_target", "competition_v2_readiness_target_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_round", "competition_v2_round_reserved_identity", "competition_v2_round_reserved_identity_guard", 7) + ")"

var competitionMigration847ArtifactQuery = "(" +
	competitionStagingLifecycleSharedArtifactQuery + " AND " +
	migrationFp2Index("competition_round", "competition_round_epoch_identity", "competition_id, staging, epoch_number", "((NOT staging) OR (NOT COALESCE((((policy_json -> 'automation_policy'::text) ->> 'enabled'::text))::boolean, false)))", true) + " AND " +
	migrationFp2Index("competition_round", "competition_round_v2_staging_epoch", "competition_id, (((policy_json -> 'automation_policy'::text) ->> 'environment'::text)), (((policy_json -> 'automation_policy'::text) ->> 'season_id'::text)), epoch_number", "(staging AND COALESCE((((policy_json -> 'automation_policy'::text) ->> 'enabled'::text))::boolean, false))", true) + " AND " +
	migrationFp2Index("competition_round", "competition_round_one_active_staging", "competition_id", "(staging AND (NOT canceled) AND CASE WHEN COALESCE((((policy_json -> 'automation_policy'::text) ->> 'enabled'::text))::boolean, false) THEN (automation_sealed_at IS NULL) ELSE (finalized_at IS NULL) END)", true) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_mode(jsonb)", "void", "plpgsql", "v", false, competitionMigration847CompetitionV2SourceModeBody, "intent") + " AND " +
	snMainnetMigrationFunction("competition_v2_score_eligible_mode(jsonb,double precision,double precision,boolean)", "boolean", "sql", "i", false, competitionMigration847CompetitionV2ScoreEligibleModeBody, "score", "baseline_raw", "margin", "staging_mode") + " AND " +
	snMainnetMigrationFunction("competition_v2_source_authorized(jsonb,text,text,text)", "void", "plpgsql", "v", false, competitionMigration847CompetitionV2SourceAuthorizedBody, "intent", "env", "competition", "season") + " AND " +
	snMainnetMigrationFunction("competition_v2_source_insert_or_activate_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2SourceInsertOrActivateGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_seal_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2SealGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_source_abandonment_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2SourceAbandonmentGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_receipt_identity_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2ReceiptIdentityGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_round_insert_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2RoundInsertGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_readiness_mode_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2ReadinessModeGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_pointer_mode_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2PointerModeGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_decision_mode_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2DecisionModeGuardBody) + " AND " +
	snMainnetMigrationFunction("competition_v2_round_guard()", "trigger", "plpgsql", "v", false, competitionMigration847CompetitionV2RoundGuardBody) + " AND " +
	snMainnetMigrationTrigger("competition_source_readiness", "competition_v2_readiness_mode", "competition_v2_readiness_mode_guard", 7) + " AND " +
	snMainnetMigrationTrigger("competition_source_pointer", "competition_v2_pointer_mode", "competition_v2_pointer_mode_guard", 23) + " AND " +
	snMainnetMigrationTrigger("competition_reward_decision", "competition_v2_decision_mode", "competition_v2_decision_mode_guard", 7) + ")"

var competitionMigration848ArtifactQuery = "(" +
	migrationFp2Index("account_feedback", "account_feedback_network_id_feedback_time", "network_id, feedback_time", "", false) + ")"

// Exact bodies are copied from their immutable published migration, not
// reconstructed from the live database being checked.
const competitionMigration842CompetitionV2ScoreEligibleBody = `
 SELECT COALESCE(
  score @> '{"placeable":true,"takeover_eligible":true,"significance":{"statistically_significant":true,"recommended_next_epoch_takeover_margin_supported":true,"replicate_count":9}}'::jsonb
  AND score->'gates' ?& ARRAY['G1_success','G2_volume','G3_path_integrity','G4_matchmaking','G5_stability','G6_resources']
  AND NOT EXISTS (SELECT 1 FROM jsonb_each(score->'gates') gate WHERE NOT COALESCE((gate.value->>'passed')::boolean,false))
  AND (score->>'raw_score')::double precision > 0
  AND (score->>'raw_score')::double precision <= baseline_raw * (1-margin)
  AND abs((score->'significance'->>'takeover_margin_percent')::double precision - margin*100) <= 0.000000001,
 false)
`

const competitionMigration842CompetitionV2EvidenceGuardBody = `
DECLARE target_round uuid; epoch_round competition_round%ROWTYPE;
BEGIN
 target_round := CASE WHEN TG_OP = 'DELETE' THEN OLD.round_id ELSE NEW.round_id END;
 SELECT * INTO epoch_round FROM competition_round WHERE round_id = target_round FOR UPDATE;
 IF COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
  IF EXISTS(SELECT 1 FROM competition_evaluation_seal WHERE round_id = target_round) THEN
   RAISE EXCEPTION 'v2 sealed evaluation evidence is immutable';
  END IF;
  IF TG_TABLE_NAME = 'competition_job' AND TG_OP <> 'DELETE' THEN
   IF NEW.state = 'running' AND current_setting('urnetwork.competition_v2_worker',true) IS DISTINCT FROM 'on' THEN
    RAISE EXCEPTION 'legacy worker cannot claim v2 work';
   END IF;
  END IF;
 END IF;
 IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END `

const competitionMigration842CompetitionV2RoundGuardBody = `
BEGIN
 IF COALESCE((OLD.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
  IF NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL THEN
   RAISE EXCEPTION 'v2 must not manufacture legacy finalization';
  END IF;
  IF (OLD.automation_sealed_at IS NOT NULL AND
     (OLD.automation_sealed_at IS DISTINCT FROM NEW.automation_sealed_at OR OLD.automation_publication_at IS DISTINCT FROM NEW.automation_publication_at)) OR
     ((NEW.automation_sealed_at IS DISTINCT FROM OLD.automation_sealed_at OR NEW.automation_publication_at IS DISTINCT FROM OLD.automation_publication_at) AND
     NOT EXISTS(SELECT 1 FROM competition_evaluation_seal WHERE round_id=NEW.round_id AND sealed_at=NEW.automation_sealed_at AND publication_at=NEW.automation_publication_at)) THEN
   RAISE EXCEPTION 'v2 publication requires its immutable seal';
  END IF;
 END IF;
 RETURN NEW;
END `

const competitionMigration842CompetitionV2SealGuardBody = `
DECLARE epoch_round competition_round%ROWTYPE; baseline_raw double precision; expected_count integer; entry jsonb; expected_eligible boolean;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 IF NOT FOUND OR epoch_round.staging OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR
 NOT COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) OR NEW.sealed_at < epoch_round.closes_at OR NEW.publication_at < epoch_round.reveal_at OR
 EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND state IN ('queued','running')) THEN
  RAISE EXCEPTION 'v2 seal requires closed fully drained production round';
 END IF;
 SELECT count(*) INTO expected_count FROM competition_job WHERE round_id=NEW.round_id;
 IF jsonb_array_length(NEW.seal_json->'entries') <> expected_count OR
 (SELECT count(DISTINCT e->>'job_id') FROM jsonb_array_elements(NEW.seal_json->'entries') e) <> expected_count THEN
  RAISE EXCEPTION 'v2 seal inventory is incomplete';
 END IF;
 IF EXISTS(SELECT 1 FROM (
   SELECT j.job_id,row_number() OVER(ORDER BY (j.score_json->>'raw_score')::double precision ASC NULLS LAST,j.submitted_at,j.job_id) AS rank
   FROM competition_job j WHERE j.round_id=NEW.round_id) ranked
   JOIN jsonb_array_elements(NEW.seal_json->'entries') e ON (e->>'job_id')::uuid=ranked.job_id
   WHERE (e->>'rank')::integer<>ranked.rank) THEN
  RAISE EXCEPTION 'v2 sealed ranking differs from exact raw order';
 END IF;
 SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
 FROM competition_round_baseline b, jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=NEW.round_id;
 FOR entry IN SELECT value FROM jsonb_array_elements(NEW.seal_json->'entries') LOOP
  IF NOT EXISTS(SELECT 1 FROM competition_job j WHERE j.round_id=NEW.round_id AND j.job_id=(entry->>'job_id')::uuid
    AND j.state=entry->>'state' AND j.patch_sha256=entry->>'patch_sha256'
    AND j.completed_at=(entry->>'completed_at')::timestamptz AND j.submitted_at=(entry->>'submitted_at')::timestamptz
    AND (j.score_json IS NOT DISTINCT FROM entry->'score')) THEN
   RAISE EXCEPTION 'v2 sealed evidence differs from terminal job';
  END IF;
  expected_eligible := entry->>'state'='succeeded' AND competition_v2_score_eligible(entry->'score',baseline_raw,(epoch_round.policy_json->'evaluation_policy'->>'takeover_margin')::double precision);
  IF expected_eligible AND epoch_round.policy_json->'automation_policy'->>'repeat_reward_policy'='exclude_exact_identity' AND EXISTS(
   SELECT 1 FROM competition_reward_decision d JOIN competition_job j ON j.job_id=(d.decision_json->>'selected_job_id')::uuid
   JOIN competition_round r ON r.round_id=d.round_id WHERE r.competition_id=epoch_round.competition_id
   AND r.policy_json->'automation_policy'->>'environment'=epoch_round.policy_json->'automation_policy'->>'environment'
   AND r.policy_json->'automation_policy'->>'season_id'=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND r.policy_json->'automation_policy'->>'source_lock_sha256'=epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
   AND j.patch_sha256=entry->>'patch_sha256') THEN expected_eligible:=false; END IF;
  IF COALESCE((entry->>'eligible')::boolean,false) IS DISTINCT FROM expected_eligible THEN
   RAISE EXCEPTION 'v2 sealed eligibility contradicts scorer evidence';
  END IF;
 END LOOP;
 RETURN NEW;
END `

const competitionMigration842CompetitionV2DecisionGuardBody = `
DECLARE prior competition_reward_decision%ROWTYPE; chosen uuid; seal jsonb;
BEGIN
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT * INTO prior FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF NEW.revision <> COALESCE(prior.revision,0)+1 OR NEW.previous_decision_sha256 <> COALESCE(prior.decision_sha256,'') THEN
  RAISE EXCEPTION 'v2 reward revision predecessor conflict';
 END IF;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 SELECT (e->>'job_id')::uuid INTO chosen FROM jsonb_array_elements(seal->'entries') e
 WHERE (e->>'eligible')::boolean AND COALESCE((SELECT r.review_json->'request'->>'reward_disposition'
  FROM competition_integrity_review r WHERE r.round_id=NEW.round_id AND r.job_id=(e->>'job_id')::uuid
   AND r.review_json->'request'->>'reward_disposition' <> 'unchanged'
  ORDER BY review_cursor DESC LIMIT 1),'approved') <> 'disqualified'
 ORDER BY (e->>'rank')::integer LIMIT 1;
 IF chosen IS DISTINCT FROM (NEW.decision_json->>'selected_job_id')::uuid OR
 (NEW.decision_json->>'revision')::integer <> NEW.revision OR NEW.decision_json->>'seal_sha256' <> seal->>'seal_sha256' THEN
  RAISE EXCEPTION 'v2 reward recipient differs from immutable eligible ranking';
 END IF;
 RETURN NEW;
END `

const competitionMigration842CompetitionV2ActivationGuardBody = `
BEGIN
 IF TG_OP='DELETE' OR OLD.environment IS DISTINCT FROM NEW.environment OR OLD.competition_id IS DISTINCT FROM NEW.competition_id OR
 OLD.season_id IS DISTINCT FROM NEW.season_id OR OLD.generation IS DISTINCT FROM NEW.generation OR OLD.target_epoch IS DISTINCT FROM NEW.target_epoch OR
 OLD.intent_json IS DISTINCT FROM NEW.intent_json OR OLD.intent_sha256 IS DISTINCT FROM NEW.intent_sha256 OR OLD.created_at IS DISTINCT FROM NEW.created_at OR
 (OLD.activated_at IS NOT NULL AND OLD.activated_at IS DISTINCT FROM NEW.activated_at) THEN
  RAISE EXCEPTION 'v2 source activation intent is immutable';
 END IF;
 RETURN NEW;
END `

const competitionMigration843CompetitionV2DecisionGuardBody = `
DECLARE prior competition_reward_decision%ROWTYPE; chosen uuid; seal jsonb; invalidated boolean;
BEGIN
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT * INTO prior FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF NEW.revision <> COALESCE(prior.revision,0)+1 OR NEW.previous_decision_sha256 <> COALESCE(prior.decision_sha256,'') THEN
  RAISE EXCEPTION 'v2 reward revision predecessor conflict';
 END IF;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 SELECT EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=NEW.round_id AND invalidates_evaluation) INTO invalidated;
 IF invalidated THEN
  IF NEW.decision_json->>'action' IS DISTINCT FROM 'invalidated' OR NEW.decision_json->>'selected_job_id' IS NOT NULL THEN
   RAISE EXCEPTION 'invalidated evaluation cannot elect a reward recipient';
  END IF;
 ELSE
  IF NEW.decision_json->>'action' = 'invalidated' THEN
   RAISE EXCEPTION 'invalidation requires an authenticated incident record';
  END IF;
  SELECT (e->>'job_id')::uuid INTO chosen FROM jsonb_array_elements(seal->'entries') e
  WHERE (e->>'eligible')::boolean AND COALESCE((SELECT r.review_json->'request'->>'reward_disposition'
   FROM competition_integrity_review r WHERE r.round_id=NEW.round_id AND r.job_id=(e->>'job_id')::uuid
    AND r.review_json->'request'->>'reward_disposition' <> 'unchanged'
   ORDER BY review_cursor DESC LIMIT 1),'approved') <> 'disqualified'
  ORDER BY (e->>'rank')::integer LIMIT 1;
  IF chosen IS DISTINCT FROM (NEW.decision_json->>'selected_job_id')::uuid THEN
   RAISE EXCEPTION 'v2 reward recipient differs from immutable eligible ranking';
  END IF;
 END IF;
 IF (NEW.decision_json->>'revision')::integer <> NEW.revision OR NEW.decision_json->>'seal_sha256' <> seal->>'seal_sha256' THEN
  RAISE EXCEPTION 'v2 reward revision or seal identity conflict';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2ScopeLockBody = `
 SELECT pg_advisory_xact_lock(hashtextextended('competition-automation:'||env||':'||competition||':'||season,0))
`

const competitionMigration844CompetitionV2RequireSourceLeaseBody = `
BEGIN
 PERFORM competition_v2_scope_lock(env,competition,season);
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=env AND competition_id=competition AND season_id=season
  AND owner_id=current_setting('urnetwork.competition_v2_owner',true)
  AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true)
  AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'v2 source transition requires the current fenced supervisor lease';
 END IF;
 IF COALESCE((SELECT active FROM competition_automation_hold WHERE environment=env AND competition_id=competition AND season_id=season AND kind='source_activation' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'v2 source activation is held';
 END IF;
END `

const competitionMigration844CompetitionV2SourceAuthorizedBody = `
DECLARE previous jsonb; previous_generation bigint; review competition_integrity_review%ROWTYPE;
 source_review_id uuid; reward_disposition text; evaluated_round competition_round%ROWTYPE;
 candidate_score jsonb; recommendation double precision; choice_at timestamp; predecessor_id uuid; lock_content text;
BEGIN
 SELECT generation INTO previous_generation FROM competition_source_pointer WHERE environment=env AND competition_id=competition AND season_id=season;
 IF COALESCE(previous_generation,0) IS DISTINCT FROM (intent->>'expected_generation')::bigint THEN
  RAISE EXCEPTION 'v2 source pointer compare-and-swap conflict';
 END IF;
 IF intent->>'environment' IS DISTINCT FROM env OR intent->>'competition_id' IS DISTINCT FROM competition OR intent->>'season_id' IS DISTINCT FROM season
  OR NOT COALESCE((intent->>'generation')::bigint>COALESCE(previous_generation,0),false)
  OR (intent->>'generation')::bigint IS DISTINCT FROM (SELECT COALESCE(max(generation),0)+1 FROM competition_source_activation
   WHERE environment=env AND competition_id=competition AND season_id=season AND generation<(intent->>'generation')::bigint)
  OR jsonb_typeof(intent->'commits') IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'commits'))<>11
  OR NOT COALESCE(intent->'commits' ?& ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','operator-proxy','warp','gvisor'],false)
  OR EXISTS(SELECT 1 FROM jsonb_each_text(intent->'commits') c WHERE c.value !~ '^[0-9a-f]{40}$') THEN
  RAISE EXCEPTION 'v2 source scope or complete repository identity is invalid';
 END IF;
 SELECT '{"schema":1,"development_snapshot":false,"repositories":{'||string_agg(to_json(c.key)::text||':'||to_json(c.value)::text,',' ORDER BY c.key)||'}}'
 INTO lock_content FROM jsonb_each_text(intent->'commits') c;
 IF intent->>'source_lock_sha256' IS DISTINCT FROM encode(sha256(convert_to(lock_content,'UTF8')),'hex')
  OR intent->>'manifest_sha256' IS DISTINCT FROM encode(sha256(decode(intent->>'manifest','base64')),'hex')
  OR COALESCE(intent->>'manifest','')=''
  OR COALESCE(intent->>'manifest_commit','') !~ '^[0-9a-f]{40}$'
  OR COALESCE(intent->>'config_expected_commit','') !~ '^[0-9a-f]{40}$'
  OR COALESCE(intent->>'source_decision_digest','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'publication_journal_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'threshold_evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'evaluator_image_digest','') !~ '^sha256:[0-9a-f]{64}$'
  OR COALESCE(intent->>'host_qualification_sha256','') !~ '^[0-9a-f]{64}$'
  OR NOT COALESCE((intent->>'threshold')::double precision>=0 AND (intent->>'threshold')::double precision<1,false)
  OR NOT COALESCE((intent->>'prepared_at')::timestamp>'1970-01-01'::timestamp,false)
  OR jsonb_typeof(intent->'git_refs') IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'git_refs'))<>12
  OR EXISTS(SELECT 1 FROM jsonb_each_text(intent->'commits') c WHERE
   intent->'git_refs'->c.key->>'commit' IS DISTINCT FROM c.value
   OR COALESCE(intent->'git_refs'->c.key->>'expected_commit','') !~ '^[0-9a-f]{40}$')
  OR EXISTS(SELECT 1 FROM jsonb_each(intent->'git_refs') r WHERE COALESCE(r.value->>'branch','')='' OR r.value->>'branch' LIKE '-%' OR strpos(r.value->>'branch','..')>0
   OR r.value->>'branch' ~ '[[:space:]~^:?*]' OR strpos(r.value->>'branch','[')>0 OR strpos(r.value->>'branch',E'\\\\')>0)
  OR intent->'git_refs'->'config'->>'commit' IS DISTINCT FROM intent->>'manifest_commit'
  OR intent->'git_refs'->'config'->>'expected_commit' IS DISTINCT FROM intent->>'config_expected_commit' THEN
  RAISE EXCEPTION 'v2 source content or publication recovery identity is invalid';
 END IF;
 IF previous_generation IS NULL THEN
  IF intent->>'kind' IS DISTINCT FROM 'baseline' OR (intent->>'target_epoch')::integer IS DISTINCT FROM 1 OR (intent->>'source_generation')::integer IS DISTINCT FROM 0
   OR intent->>'source_lock_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_lock',true)
   OR intent->'commits'->>'server' IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_base',true)
   OR (intent->>'threshold')::double precision IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_margin',true)::double precision
   OR COALESCE(current_setting('urnetwork.competition_v2_policy_approval',true),'') !~ '^[0-9a-f]{64}$' THEN
   RAISE EXCEPTION 'v2 baseline must match prospectively approved bootstrap configuration';
  END IF;
  RETURN;
 END IF;
 SELECT intent_json INTO previous FROM competition_source_activation WHERE environment=env AND competition_id=competition AND season_id=season AND generation=previous_generation AND activated_at IS NOT NULL;
 IF NOT FOUND OR (intent->>'target_epoch')::integer IS DISTINCT FROM (previous->>'target_epoch')::integer+1 OR intent->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256' THEN
  RAISE EXCEPTION 'v2 source transition does not extend its active predecessor';
 END IF;
 SELECT (s.seal_json->>'source_choice_at')::timestamp,r.round_id INTO choice_at,predecessor_id FROM competition_evaluation_seal s JOIN competition_round r USING(round_id)
 WHERE r.competition_id=competition AND NOT r.staging AND r.epoch_number=(intent->>'target_epoch')::integer-1
  AND r.policy_json->'automation_policy'->>'environment'=env AND r.policy_json->'automation_policy'->>'season_id'=season;
 IF NOT FOUND OR (intent->>'prepared_at')::timestamp<choice_at OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=predecessor_id AND invalidates_evaluation) THEN
  RAISE EXCEPTION 'v2 source choice requires the preceding immutable seal and deadline';
 END IF;
 IF intent->>'kind'='reviewed_source_carry_forward' THEN
  IF intent->'commits' IS DISTINCT FROM previous->'commits' OR intent->>'source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
   OR intent->'source_generation' IS DISTINCT FROM previous->'source_generation' OR intent->'threshold' IS DISTINCT FROM previous->'threshold'
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM previous->>'threshold_evidence_sha256' OR intent->>'review_id' IS NOT NULL
   OR COALESCE(intent->>'reason','') NOT IN ('review_pending','promotion_not_ready','no_reward_winner','readiness_fallback') THEN
   RAISE EXCEPTION 'v2 carry-forward cannot alter reviewed source or its threshold';
  END IF;
 ELSIF intent->>'kind'='reviewed_source_promotion' THEN
  SELECT * INTO review FROM competition_integrity_review WHERE review_id=(intent->>'review_id')::uuid;
  IF NOT FOUND OR review.review_json->'request'->>'source_disposition' IS DISTINCT FROM 'approved'
   OR review.request_sha256 IS DISTINCT FROM intent->>'review_decision_digest'
   OR review.review_json->'request'->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
   OR review.review_json->'request'->>'patch_sha256' IS DISTINCT FROM intent->>'approved_patch_sha256'
   OR review.created_at>choice_at OR NOT (intent->>'build_ready_at' IS NOT NULL AND (intent->>'build_ready_at')::timestamp<=choice_at)
   OR (intent->>'source_generation')::integer IS DISTINCT FROM (previous->>'source_generation')::integer+1
   OR intent->>'source_lock_sha256'=previous->>'source_lock_sha256' THEN
   RAISE EXCEPTION 'v2 promotion requires genuine exact-source approval ready by the deadline';
  END IF;
  SELECT * INTO evaluated_round FROM competition_round WHERE round_id=review.round_id;
  IF evaluated_round.competition_id IS DISTINCT FROM competition OR evaluated_round.policy_json->'automation_policy'->>'environment' IS DISTINCT FROM env
   OR evaluated_round.policy_json->'automation_policy'->>'season_id' IS DISTINCT FROM season
   OR evaluated_round.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluated_image_digest'
   OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=review.round_id AND invalidates_evaluation) THEN
   RAISE EXCEPTION 'v2 promotion evidence belongs to another or invalidated evaluation';
  END IF;
  SELECT review_id INTO source_review_id FROM competition_integrity_review WHERE round_id=review.round_id AND job_id=review.job_id
   AND review_json->'request'->>'source_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
  SELECT review_json->'request'->>'reward_disposition' INTO reward_disposition FROM competition_integrity_review WHERE round_id=review.round_id AND job_id=review.job_id
   AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
  IF source_review_id IS DISTINCT FROM review.review_id OR COALESCE(reward_disposition,'') NOT IN ('approved','restored') THEN
   RAISE EXCEPTION 'v2 source approval is no longer current';
  END IF;
  SELECT score_json INTO candidate_score FROM competition_job WHERE job_id=review.job_id AND round_id=review.round_id AND state='succeeded';
  recommendation:=GREATEST((previous->>'threshold')::double precision,(candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_percent')::double precision/100);
  IF NOT COALESCE((candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_supported')::boolean,false)
   OR (intent->>'threshold')::double precision IS DISTINCT FROM recommendation
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM review.review_json->'request'->>'score_sha256'
   OR ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server') THEN
   RAISE EXCEPTION 'v2 promotion changed protected dependencies or transplanted threshold evidence';
  END IF;
 ELSE RAISE EXCEPTION 'unsupported v2 source decision'; END IF;
END `

const competitionMigration844CompetitionV2SourceInsertOrActivateGuardBody = `
BEGIN
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.generation IS DISTINCT FROM (NEW.intent_json->>'generation')::bigint OR NEW.target_epoch IS DISTINCT FROM (NEW.intent_json->>'target_epoch')::integer
  OR NEW.intent_sha256 IS DISTINCT FROM NEW.intent_json->>'intent_sha256' THEN
  RAISE EXCEPTION 'v2 source columns disagree with immutable intent';
 END IF;
 IF TG_OP='INSERT' AND NEW.activated_at IS NOT NULL THEN RAISE EXCEPTION 'v2 source must be staged before activation'; END IF;
 PERFORM competition_v2_source_authorized(NEW.intent_json,NEW.environment,NEW.competition_id,NEW.season_id);
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'manifest_sha256'=NEW.intent_json->>'manifest_sha256' AND r.receipt_json->>'source_lock_sha256'=NEW.intent_json->>'source_lock_sha256'
   AND (r.receipt_json->>'ready')::boolean)<>2 THEN
  RAISE EXCEPTION 'v2 source activation requires exact api and worker readiness receipts';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2SourceReadinessGuardBody = `
DECLARE intent jsonb;
BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF NOT FOUND OR NEW.receipt_json->>'environment' IS DISTINCT FROM NEW.environment OR NEW.receipt_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.receipt_json->>'season_id' IS DISTINCT FROM NEW.season_id OR (NEW.receipt_json->>'generation')::bigint IS DISTINCT FROM NEW.generation
  OR NEW.receipt_json->>'role' IS DISTINCT FROM NEW.role OR COALESCE(NEW.receipt_json->>'actor_id','')=''
  OR NEW.receipt_json->>'manifest_commit' IS DISTINCT FROM intent->>'manifest_commit'
  OR NEW.receipt_json->>'manifest_sha256' IS DISTINCT FROM intent->>'manifest_sha256'
  OR NEW.receipt_json->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR NEW.receipt_json->>'qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR COALESCE(NEW.receipt_json->>'runtime_image_digest','') !~ '^sha256:[0-9a-f]{64}$' OR NOT COALESCE((NEW.receipt_json->>'ready')::boolean,false) THEN
  RAISE EXCEPTION 'v2 readiness receipt does not attest the exact source manifest';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2SourcePointerGuardBody = `
DECLARE intent jsonb; previous_generation bigint;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'v2 source pointer cannot be deleted'; END IF;
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF TG_OP='UPDATE' AND (OLD.environment IS DISTINCT FROM NEW.environment OR OLD.competition_id IS DISTINCT FROM NEW.competition_id OR OLD.season_id IS DISTINCT FROM NEW.season_id) THEN
  RAISE EXCEPTION 'v2 activation pointer scope is immutable';
 END IF;
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND activated_at IS NOT NULL;
 SELECT generation INTO previous_generation FROM competition_source_pointer WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id FOR UPDATE;
 IF intent IS NULL OR (intent->>'expected_generation')::bigint IS DISTINCT FROM COALESCE(previous_generation,0) THEN
  RAISE EXCEPTION 'v2 activation pointer must compare-and-swap its exact ready predecessor';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2RoundInsertGuardBody = `
DECLARE policy jsonb; intent jsonb; activated timestamp; expected_epoch integer;
BEGIN
 policy:=NEW.policy_json->'automation_policy';
 IF NOT COALESCE((policy->>'enabled')::boolean,false) THEN RETURN NEW; END IF;
 PERFORM competition_v2_require_source_lease(policy->>'environment',NEW.competition_id,policy->>'season_id');
 SELECT a.intent_json,a.activated_at INTO intent,activated FROM competition_source_pointer p JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
 WHERE p.environment=policy->>'environment' AND p.competition_id=NEW.competition_id AND p.season_id=policy->>'season_id';
 IF NOT FOUND OR activated IS NULL OR NEW.staging OR NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL
  OR (policy->>'version')::integer IS DISTINCT FROM 2 OR policy->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2'
  OR (policy->>'source_activation_generation')::bigint IS DISTINCT FROM (intent->>'generation')::bigint
  OR (policy->>'source_generation')::integer IS DISTINCT FROM (intent->>'source_generation')::integer
  OR policy->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR NEW.policy_json->>'base_sha' IS DISTINCT FROM intent->'commits'->>'server'
  OR NEW.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluator_image_digest'
  OR (NEW.policy_json->'evaluation_policy'->>'takeover_margin')::double precision IS DISTINCT FROM (intent->>'threshold')::double precision
  OR NEW.policy_json->'evaluation_policy'->>'host_qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR NEW.epoch_number IS DISTINCT FROM (intent->>'target_epoch')::integer
  OR NEW.closes_at-NEW.opens_at<>interval '7 days' OR NEW.reveal_at<NEW.closes_at OR NEW.opens_at<activated
  OR NEW.opens_at<(intent->>'prepared_at')::timestamp+interval '16 hours' THEN
  RAISE EXCEPTION 'v2 round requires the exact activated source and qualified full-window schedule';
 END IF;
 SELECT COALESCE(max(epoch_number),0)+1 INTO expected_epoch FROM competition_round WHERE competition_id=NEW.competition_id AND NOT staging;
 IF NEW.epoch_number<>expected_epoch OR NEW.epoch_number>6 OR (NEW.epoch_number>1 AND NOT EXISTS(
  SELECT 1 FROM competition_round r WHERE r.competition_id=NEW.competition_id AND NOT r.staging AND r.epoch_number=NEW.epoch_number-1
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation)
   AND (r.finalized_at IS NOT NULL OR EXISTS(SELECT 1 FROM competition_evaluation_seal s WHERE s.round_id=r.round_id)))) THEN
  RAISE EXCEPTION 'v2 round predecessor is not sealed or season is complete';
 END IF;
 IF COALESCE((SELECT active FROM competition_automation_hold WHERE environment=policy->>'environment' AND competition_id=NEW.competition_id AND season_id=policy->>'season_id' AND kind='admission' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'v2 admission is held';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2ReviewIdentityGuardBody = `
DECLARE seal jsonb; entry jsonb; request jsonb; previous competition_integrity_review%ROWTYPE; reward_revision integer; round_policy jsonb;
BEGIN
 SELECT policy_json INTO round_policy FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(round_policy->'automation_policy'->>'environment',round_policy->>'competition_id',round_policy->'automation_policy'->>'season_id');
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 request:=NEW.review_json->'request';
 SELECT value INTO entry FROM jsonb_array_elements(seal->'entries') WHERE (value->>'job_id')::uuid=NEW.job_id;
 SELECT COALESCE(max(revision),0) INTO reward_revision FROM competition_reward_decision WHERE round_id=NEW.round_id;
 IF NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_v2_review_actor',true) OR entry IS NULL OR entry->>'state'<>'succeeded' OR (request->>'expected_revision')::integer IS DISTINCT FROM reward_revision
  OR (request->>'job_id')::uuid IS DISTINCT FROM NEW.job_id OR (NEW.review_json->>'round_id')::uuid IS DISTINCT FROM NEW.round_id
  OR (NEW.review_json->>'review_id')::uuid IS DISTINCT FROM NEW.review_id OR NEW.review_json->>'actor_id' IS DISTINCT FROM NEW.actor_id
  OR request->>'idempotency_key' IS DISTINCT FROM NEW.idempotency_key OR NEW.review_json->>'request_sha256' IS DISTINCT FROM NEW.request_sha256
  OR request->>'patch_sha256' IS DISTINCT FROM entry->>'patch_sha256' OR request->>'base_source_lock_sha256' IS DISTINCT FROM entry->>'base_source_lock_sha256'
  OR request->>'score_sha256' IS DISTINCT FROM entry->>'score_sha256' OR request->>'artifact_manifest_sha256' IS DISTINCT FROM entry->>'artifact_manifest_sha256'
  OR request->>'baseline_sha256' IS DISTINCT FROM seal->>'baseline_sha256' OR request->>'criteria_version' IS DISTINCT FROM round_policy->'automation_policy'->>'review_criteria_version'
  OR COALESCE(request->>'reward_disposition','') NOT IN ('approved','disqualified','restored','unchanged') OR COALESCE(request->>'source_disposition','') NOT IN ('approved','rejected','deferred','unchanged')
  OR jsonb_array_length(request->'findings')<>6
  OR (SELECT count(DISTINCT f->>'criterion') FROM jsonb_array_elements(request->'findings') f WHERE f->>'criterion' IN ('measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety'))<>6
  OR octet_length(NEW.review_json::text)>131072 THEN
  RAISE EXCEPTION 'v2 review must bind exact sealed evidence and current reward revision';
 END IF;
 SELECT * INTO previous FROM competition_integrity_review WHERE round_id=NEW.round_id AND job_id=NEW.job_id AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
 IF (request->>'reward_disposition'='approved' AND previous.review_json->'request'->>'reward_disposition'='disqualified') OR
  (request->>'reward_disposition'='restored' AND (previous.review_json->'request'->>'reward_disposition' IS DISTINCT FROM 'disqualified'
   OR (request->>'supersedes_review_id')::uuid IS DISTINCT FROM previous.review_id OR COALESCE(request->>'appeal_id','')='')) THEN
  RAISE EXCEPTION 'v2 restoration requires an explicit appeal of the latest reward finding';
 END IF;
 IF request->>'source_disposition'='approved' AND request->>'reward_disposition' NOT IN ('approved','restored') THEN
  RAISE EXCEPTION 'v2 source approval requires an affirmative integrity finding';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2OutboxIdentityGuardBody = `
DECLARE decision jsonb; publication timestamp;
BEGIN
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=NEW.round_id AND revision=NEW.revision;
 PERFORM competition_v2_scope_lock(decision->>'environment',decision->>'competition_id',decision->>'season_id');
 SELECT publication_at INTO publication FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 IF decision IS NULL OR NEW.payload_json IS DISTINCT FROM decision OR NEW.publication_at IS DISTINCT FROM publication THEN
  RAISE EXCEPTION 'v2 outbox event differs from immutable decision or publication boundary';
 END IF;
 NEW.event_cursor:=nextval('competition_reward_outbox_event_cursor_seq');
 RETURN NEW;
END `

const competitionMigration844CompetitionV2DecisionOutboxGuardBody = `
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_reward_outbox WHERE round_id=NEW.round_id AND revision=NEW.revision) THEN
  RAISE EXCEPTION 'v2 reward decision and durable event must commit atomically';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2ReceiptIdentityGuardBody = `
DECLARE event competition_reward_outbox%ROWTYPE; receipt jsonb;
BEGIN
 SELECT * INTO event FROM competition_reward_outbox WHERE event_id=NEW.event_id;
 receipt:=NEW.receipt_json;
 IF NOT FOUND OR NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_v2_settlement_actor',true)
  OR NEW.actor_id IS DISTINCT FROM receipt->>'actor_id' OR NEW.status IS DISTINCT FROM receipt->>'status'
  OR NEW.payload_sha256 IS DISTINCT FROM event.payload_sha256 OR NEW.payload_sha256 IS DISTINCT FROM receipt->>'payload_sha256'
  OR (receipt->>'event_id')::uuid IS DISTINCT FROM NEW.event_id OR (receipt->>'round_id')::uuid IS DISTINCT FROM event.round_id
  OR (receipt->>'revision')::integer IS DISTINCT FROM event.revision
  OR receipt->>'decision_sha256' IS DISTINCT FROM event.payload_json->>'decision_sha256'
  OR receipt->>'environment' IS DISTINCT FROM event.payload_json->>'environment'
  OR receipt->>'competition_id' IS DISTINCT FROM event.payload_json->>'competition_id'
  OR receipt->>'season_id' IS DISTINCT FROM event.payload_json->>'season_id'
  OR NEW.created_at<event.publication_at THEN
  RAISE EXCEPTION 'v2 settlement receipt does not bind its scoped published event';
 END IF;
 RETURN NEW;
END `

const competitionMigration844CompetitionV2SealControlGuardBody = `
DECLARE baseline_raw double precision; baseline_digest text; round_policy jsonb;
BEGIN
 SELECT policy_json INTO round_policy FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(round_policy->'automation_policy'->>'environment',round_policy->>'competition_id',round_policy->'automation_policy'->>'season_id');
 SELECT baseline_sha256 INTO baseline_digest FROM competition_round_baseline WHERE round_id=NEW.round_id;
 SELECT percentile_disc(0.5) WITHIN GROUP(ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
 FROM competition_round_baseline b,jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=NEW.round_id;
 IF (NEW.seal_json->>'baseline_raw_score')::double precision IS DISTINCT FROM COALESCE(baseline_raw,0)
  OR NEW.seal_json->>'baseline_sha256' IS DISTINCT FROM baseline_digest
  OR (NEW.seal_json->>'takeover_margin')::double precision IS DISTINCT FROM (round_policy->'evaluation_policy'->>'takeover_margin')::double precision THEN
  RAISE EXCEPTION 'v2 seal control median and margin must match immutable scoring evidence';
 END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2SourceAbandonmentGuardBody = `
DECLARE intent jsonb; activated timestamp; abandoned timestamp; pointer_generation bigint;
BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id
  AND owner_id=current_setting('urnetwork.competition_v2_owner',true) AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'v2 source abandonment requires the current fenced supervisor';
 END IF;
 SELECT intent_json,activated_at,abandoned_at INTO intent,activated,abandoned FROM competition_source_activation
 WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation FOR UPDATE;
 SELECT generation INTO pointer_generation FROM competition_source_pointer WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id;
 IF intent IS NULL OR intent->>'kind' IS DISTINCT FROM 'reviewed_source_promotion' OR activated IS NOT NULL OR abandoned IS NOT NULL
  OR pointer_generation IS DISTINCT FROM (intent->>'expected_generation')::bigint
  OR NEW.abandonment_json->>'environment' IS DISTINCT FROM NEW.environment OR NEW.abandonment_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.abandonment_json->>'season_id' IS DISTINCT FROM NEW.season_id OR (NEW.abandonment_json->>'generation')::bigint IS DISTINCT FROM NEW.generation
  OR NEW.abandonment_json->>'intent_sha256' IS DISTINCT FROM intent->>'intent_sha256'
  OR NEW.abandonment_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_v2_owner',true)
  OR NEW.abandonment_json->>'fencing_token' IS DISTINCT FROM current_setting('urnetwork.competition_v2_fence',true)
  OR (NEW.abandonment_json->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at
  OR COALESCE(NEW.abandonment_json->>'reason','') NOT IN ('publication_failed','readiness_failed','build_failed','source_approval_revoked')
  OR COALESCE(NEW.abandonment_json->>'unpublished_proof_sha256','') !~ '^[0-9a-f]{64}$'
  OR EXISTS(SELECT 1 FROM competition_source_readiness WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation)
  OR EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND NOT staging AND epoch_number=(intent->>'target_epoch')::integer) THEN
  RAISE EXCEPTION 'v2 fallback must retain an exact verified-unpublished unactivated promotion';
 END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2SourceInsertOrActivateGuardBody = `
BEGIN
 IF TG_OP='UPDATE' AND OLD.abandoned_at IS NOT NULL THEN RAISE EXCEPTION 'v2 abandoned source intent is immutable'; END IF;
 IF TG_OP='UPDATE' AND OLD.abandoned_at IS NULL AND NEW.abandoned_at IS NOT NULL THEN
  PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
  IF NEW.activated_at IS NOT NULL OR NOT EXISTS(SELECT 1 FROM competition_source_abandonment a
   WHERE a.environment=NEW.environment AND a.competition_id=NEW.competition_id AND a.season_id=NEW.season_id AND a.generation=NEW.generation AND a.created_at=NEW.abandoned_at
    AND a.abandonment_json->>'actor_id'=current_setting('urnetwork.competition_v2_owner',true)
    AND a.abandonment_json->>'fencing_token'=current_setting('urnetwork.competition_v2_fence',true)) THEN
   RAISE EXCEPTION 'v2 abandonment requires its exact durable fenced proof';
  END IF;
  RETURN NEW;
 END IF;
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.generation IS DISTINCT FROM (NEW.intent_json->>'generation')::bigint OR NEW.target_epoch IS DISTINCT FROM (NEW.intent_json->>'target_epoch')::integer
  OR NEW.intent_sha256 IS DISTINCT FROM NEW.intent_json->>'intent_sha256' OR NEW.abandoned_at IS NOT NULL THEN
  RAISE EXCEPTION 'v2 source columns disagree with immutable intent';
 END IF;
 IF TG_OP='INSERT' AND NEW.activated_at IS NOT NULL THEN RAISE EXCEPTION 'v2 source must be staged before activation'; END IF;
 PERFORM competition_v2_source_authorized(NEW.intent_json,NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.intent_json->>'kind'='reviewed_source_promotion' AND NOT EXISTS(
  SELECT 1 FROM competition_integrity_review r JOIN competition_evaluation_seal s USING(round_id),jsonb_array_elements(s.seal_json->'entries') e
  WHERE r.review_id=(NEW.intent_json->>'review_id')::uuid AND (e->>'job_id')::uuid=r.job_id AND COALESCE((e->>'eligible')::boolean,false)
   AND competition_v2_score_eligible(e->'score',(s.seal_json->>'baseline_raw_score')::double precision,(s.seal_json->>'takeover_margin')::double precision)) THEN
  RAISE EXCEPTION 'v2 source promotion requires an eligible immutable sealed candidate';
 END IF;
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'manifest_sha256'=NEW.intent_json->>'manifest_sha256' AND r.receipt_json->>'source_lock_sha256'=NEW.intent_json->>'source_lock_sha256'
   AND (r.receipt_json->>'ready')::boolean)<>2 THEN
  RAISE EXCEPTION 'v2 source activation requires exact api and worker readiness receipts';
 END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2SourceAbandonmentCommittedGuardBody = `
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND abandoned_at=NEW.created_at) THEN
  RAISE EXCEPTION 'v2 abandonment audit and marker must commit atomically';
 END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2SourceNotAbandonedGuardBody = `
BEGIN
 IF EXISTS(SELECT 1 FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND abandoned_at IS NOT NULL) THEN
  RAISE EXCEPTION 'v2 abandoned source cannot receive readiness';
 END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2ReviewDecisionCommittedGuardBody = `
DECLARE elected uuid; selected uuid; decision_action text;
BEGIN
 SELECT (decision_json->>'selected_job_id')::uuid,decision_json->>'action' INTO elected,decision_action FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 IF EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=NEW.round_id AND invalidates_evaluation) THEN
  IF decision_action IS DISTINCT FROM 'invalidated' OR elected IS NOT NULL THEN RAISE EXCEPTION 'v2 invalidation requires its null-recipient revision'; END IF;
  RETURN NEW;
 END IF;
 SELECT (e->>'job_id')::uuid INTO selected FROM competition_evaluation_seal s,jsonb_array_elements(s.seal_json->'entries') e
 WHERE s.round_id=NEW.round_id AND COALESCE((e->>'eligible')::boolean,false)
 AND COALESCE((SELECT r.review_json->'request'->>'reward_disposition' FROM competition_integrity_review r
  WHERE r.round_id=NEW.round_id AND r.job_id=(e->>'job_id')::uuid AND r.review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY r.review_cursor DESC LIMIT 1),'unchanged')<>'disqualified'
 ORDER BY (e->>'rank')::integer LIMIT 1;
 IF elected IS DISTINCT FROM selected THEN RAISE EXCEPTION 'v2 review and replacement election must commit atomically'; END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2LedgerScopeGuardBody = `
DECLARE epoch_round competition_round%ROWTYPE;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 IF TG_TABLE_NAME='competition_integrity_review' THEN NEW.review_cursor:=nextval('competition_integrity_review_review_cursor_seq'); END IF;
 RETURN NEW;
END `

const competitionMigration845CompetitionV2WorkerWriteGuardBody = `
DECLARE automated boolean;
BEGIN
 SELECT COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false) INTO automated FROM competition_round WHERE round_id=NEW.round_id;
 IF automated AND current_setting('urnetwork.competition_v2_worker',true) IS DISTINCT FROM 'on' THEN
  IF (TG_OP='INSERT' AND NEW.state<>'queued') OR (TG_OP='UPDATE' AND OLD.state IN ('queued','running') AND
   (OLD.state IS DISTINCT FROM NEW.state OR OLD.lease_owner IS DISTINCT FROM NEW.lease_owner OR OLD.lease_expires_at IS DISTINCT FROM NEW.lease_expires_at
    OR OLD.score_json IS DISTINCT FROM NEW.score_json OR OLD.completed_at IS DISTINCT FROM NEW.completed_at)) THEN
   RAISE EXCEPTION 'legacy worker cannot complete or change v2 execution state';
  END IF;
 END IF;
 RETURN NEW;
END `

const competitionMigration846CompetitionV2SourceTargetGuardBody = `
DECLARE source_review competition_integrity_review%ROWTYPE;
BEGIN
 IF TG_OP='UPDATE' AND NEW.abandoned_at IS NOT NULL THEN RETURN NEW; END IF;
 IF COALESCE(NEW.intent_json->>'target_round_id','') !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR NEW.intent_json->>'target_round_id'='00000000-0000-0000-0000-000000000000' THEN
  RAISE EXCEPTION 'v2 source must reserve its exact future round identity before qualification';
 END IF;
 IF TG_OP='INSERT' AND EXISTS(SELECT 1 FROM competition_round WHERE round_id=(NEW.intent_json->>'target_round_id')::uuid) THEN
  RAISE EXCEPTION 'v2 source cannot reinterpret an already committed round';
 END IF;
 IF NEW.intent_json->>'kind'='reviewed_source_promotion' THEN
  SELECT * INTO source_review FROM competition_integrity_review WHERE review_id=(NEW.intent_json->>'review_id')::uuid;
  IF NOT FOUND OR EXISTS(SELECT 1 FROM competition_integrity_review r WHERE r.round_id=source_review.round_id AND r.job_id=source_review.job_id
   AND r.review_json->'request'->>'reward_disposition'='disqualified' AND r.review_cursor>=source_review.review_cursor) THEN
   RAISE EXCEPTION 'v2 disqualification revokes prior source approval; a fresh explicit source approval is required';
  END IF;
 END IF;
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'target_round_id'=NEW.intent_json->>'target_round_id' AND COALESCE((r.receipt_json->>'ready')::boolean,false))<>2 THEN
  RAISE EXCEPTION 'v2 activation requires API and worker qualification for the exact reserved round';
 END IF;
 RETURN NEW;
END `

const competitionMigration846CompetitionV2ReadinessTargetGuardBody = `
DECLARE target_id text;
BEGIN
 SELECT intent_json->>'target_round_id' INTO target_id FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF target_id IS NULL OR target_id='00000000-0000-0000-0000-000000000000' OR NEW.receipt_json->>'target_round_id' IS DISTINCT FROM target_id THEN
  RAISE EXCEPTION 'v2 readiness must name the exact prospectively reserved round';
 END IF;
 RETURN NEW;
END `

const competitionMigration846CompetitionV2RoundReservedIdentityGuardBody = `
DECLARE policy jsonb; target_id uuid;
BEGIN
 policy:=NEW.policy_json->'automation_policy';
 IF NOT COALESCE((policy->>'enabled')::boolean,false) THEN RETURN NEW; END IF;
 PERFORM competition_v2_scope_lock(policy->>'environment',NEW.competition_id,policy->>'season_id');
 SELECT (intent_json->>'target_round_id')::uuid INTO target_id FROM competition_source_activation
 WHERE environment=policy->>'environment' AND competition_id=NEW.competition_id AND season_id=policy->>'season_id'
  AND generation=(policy->>'source_activation_generation')::bigint AND activated_at IS NOT NULL AND abandoned_at IS NULL;
 IF target_id IS NULL OR target_id='00000000-0000-0000-0000-000000000000'::uuid OR NEW.round_id IS DISTINCT FROM target_id THEN
  RAISE EXCEPTION 'v2 round identity must be reserved before source preparation and qualification';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2SourceModeBody = `
DECLARE staging_mode boolean; window_seconds integer; preparation_seconds integer;
BEGIN
 staging_mode:=COALESCE((intent->>'staging')::boolean,false);
 window_seconds:=COALESCE((intent->>'staging_round_window_seconds')::integer,0);
 preparation_seconds:=COALESCE((intent->>'staging_preparation_seconds')::integer,0);
 IF staging_mode IS DISTINCT FROM COALESCE(nullif(current_setting('urnetwork.competition_v2_staging',true),'')::boolean,false)
  OR window_seconds IS DISTINCT FROM COALESCE(nullif(current_setting('urnetwork.competition_v2_staging_window',true),'')::integer,0)
  OR preparation_seconds IS DISTINCT FROM COALESCE(nullif(current_setting('urnetwork.competition_v2_staging_preparation',true),'')::integer,0)
  OR (staging_mode AND (window_seconds<60 OR window_seconds>604800 OR preparation_seconds<0 OR preparation_seconds>57600 OR
    (intent->>'threshold')::double precision IS DISTINCT FROM 0::double precision))
  OR (NOT staging_mode AND (window_seconds<>0 OR preparation_seconds<>0 OR NOT COALESCE((intent->>'threshold')::double precision>0,false)))
  OR EXISTS(SELECT 1 FROM jsonb_each(intent->'git_refs') r WHERE
    (staging_mode AND ((r.key='config' AND r.value->>'branch' IS DISTINCT FROM 'main') OR
      (r.key<>'config' AND r.value->>'branch' IS DISTINCT FROM 'sim-latency-staging'))) OR
    (NOT staging_mode AND r.value->>'branch'='sim-latency-staging')) THEN
  RAISE EXCEPTION 'v2 source mode, isolated branch or frozen staging schedule disagrees with its configured authority';
 END IF;
END `

const competitionMigration847CompetitionV2ScoreEligibleModeBody = `
 SELECT CASE WHEN NOT staging_mode THEN competition_v2_score_eligible(score,baseline_raw,margin) ELSE COALESCE(
  margin=0 AND baseline_raw>0 AND baseline_raw<'Infinity'::double precision
  AND score @> '{"placeable":true,"significance":{"replicate_count":9,"takeover_margin_percent":0}}'::jsonb
  AND score->'gates' ?& ARRAY['G1_success','G2_volume','G3_path_integrity','G4_matchmaking','G5_stability','G6_resources']
  AND NOT EXISTS(SELECT 1 FROM jsonb_each(score->'gates') gate WHERE NOT COALESCE((gate.value->>'passed')::boolean,false))
  AND (score->>'raw_score')::double precision>0 AND (score->>'raw_score')::double precision<'Infinity'::double precision,false) END
`

const competitionMigration847CompetitionV2SourceAuthorizedBody = `
DECLARE previous jsonb; previous_generation bigint; review competition_integrity_review%ROWTYPE;
 source_review_id uuid; reward_disposition text; evaluated_round competition_round%ROWTYPE;
 candidate_score jsonb; recommendation double precision; choice_at timestamp; predecessor_id uuid; lock_content text;
BEGIN
 PERFORM competition_v2_source_mode(intent);
 SELECT generation INTO previous_generation FROM competition_source_pointer WHERE environment=env AND competition_id=competition AND season_id=season;
 IF COALESCE(previous_generation,0) IS DISTINCT FROM (intent->>'expected_generation')::bigint THEN
  RAISE EXCEPTION 'v2 source pointer compare-and-swap conflict';
 END IF;
 IF intent->>'environment' IS DISTINCT FROM env OR intent->>'competition_id' IS DISTINCT FROM competition OR intent->>'season_id' IS DISTINCT FROM season
  OR NOT COALESCE((intent->>'generation')::bigint>COALESCE(previous_generation,0),false)
  OR (intent->>'generation')::bigint IS DISTINCT FROM (SELECT COALESCE(max(generation),0)+1 FROM competition_source_activation
   WHERE environment=env AND competition_id=competition AND season_id=season AND generation<(intent->>'generation')::bigint)
  OR jsonb_typeof(intent->'commits') IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'commits'))<>11
  OR NOT COALESCE(intent->'commits' ?& ARRAY['server','connect','sdk','proxy','glog','goidenticons','userwireguard','sn','operator-proxy','warp','gvisor'],false)
  OR EXISTS(SELECT 1 FROM jsonb_each_text(intent->'commits') c WHERE c.value !~ '^[0-9a-f]{40}$') THEN
  RAISE EXCEPTION 'v2 source scope or complete repository identity is invalid';
 END IF;
 SELECT '{"schema":1,"development_snapshot":false,"repositories":{'||string_agg(to_json(c.key)::text||':'||to_json(c.value)::text,',' ORDER BY c.key)||'}}'
 INTO lock_content FROM jsonb_each_text(intent->'commits') c;
 IF intent->>'source_lock_sha256' IS DISTINCT FROM encode(sha256(convert_to(lock_content,'UTF8')),'hex')
  OR intent->>'manifest_sha256' IS DISTINCT FROM encode(sha256(decode(intent->>'manifest','base64')),'hex')
  OR COALESCE(intent->>'manifest','')=''
  OR COALESCE(intent->>'manifest_commit','') !~ '^[0-9a-f]{40}$'
  OR COALESCE(intent->>'config_expected_commit','') !~ '^[0-9a-f]{40}$'
  OR COALESCE(intent->>'source_decision_digest','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'publication_journal_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'threshold_evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(intent->>'evaluator_image_digest','') !~ '^sha256:[0-9a-f]{64}$'
  OR COALESCE(intent->>'host_qualification_sha256','') !~ '^[0-9a-f]{64}$'
  OR NOT COALESCE((intent->>'threshold')::double precision>=0 AND (intent->>'threshold')::double precision<1,false)
  OR NOT COALESCE((intent->>'prepared_at')::timestamp>'1970-01-01'::timestamp,false)
  OR jsonb_typeof(intent->'git_refs') IS DISTINCT FROM 'object'
  OR (SELECT count(*) FROM jsonb_object_keys(intent->'git_refs'))<>12
  OR EXISTS(SELECT 1 FROM jsonb_each_text(intent->'commits') c WHERE
   intent->'git_refs'->c.key->>'commit' IS DISTINCT FROM c.value
   OR COALESCE(intent->'git_refs'->c.key->>'expected_commit','') !~ '^[0-9a-f]{40}$')
  OR EXISTS(SELECT 1 FROM jsonb_each(intent->'git_refs') r WHERE COALESCE(r.value->>'branch','')='' OR r.value->>'branch' LIKE '-%' OR strpos(r.value->>'branch','..')>0
   OR r.value->>'branch' ~ '[[:space:]~^:?*]' OR strpos(r.value->>'branch','[')>0 OR strpos(r.value->>'branch',E'\\\\')>0)
  OR intent->'git_refs'->'config'->>'commit' IS DISTINCT FROM intent->>'manifest_commit'
  OR intent->'git_refs'->'config'->>'expected_commit' IS DISTINCT FROM intent->>'config_expected_commit' THEN
  RAISE EXCEPTION 'v2 source content or publication recovery identity is invalid';
 END IF;
 IF previous_generation IS NULL THEN
  IF intent->>'kind' IS DISTINCT FROM 'baseline' OR (intent->>'target_epoch')::integer IS DISTINCT FROM 1 OR (intent->>'source_generation')::integer IS DISTINCT FROM 0
   OR intent->>'source_lock_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_lock',true)
   OR intent->'commits'->>'server' IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_base',true)
   OR (intent->>'threshold')::double precision IS DISTINCT FROM current_setting('urnetwork.competition_v2_bootstrap_margin',true)::double precision
   OR COALESCE(current_setting('urnetwork.competition_v2_policy_approval',true),'') !~ '^[0-9a-f]{64}$' THEN
   RAISE EXCEPTION 'v2 baseline must match prospectively approved bootstrap configuration';
  END IF;
  RETURN;
 END IF;
 SELECT intent_json INTO previous FROM competition_source_activation WHERE environment=env AND competition_id=competition AND season_id=season AND generation=previous_generation AND activated_at IS NOT NULL;
 IF NOT FOUND OR COALESCE((intent->>'staging')::boolean,false) IS DISTINCT FROM COALESCE((previous->>'staging')::boolean,false)
  OR COALESCE((intent->>'staging_round_window_seconds')::integer,0) IS DISTINCT FROM COALESCE((previous->>'staging_round_window_seconds')::integer,0)
  OR COALESCE((intent->>'staging_preparation_seconds')::integer,0) IS DISTINCT FROM COALESCE((previous->>'staging_preparation_seconds')::integer,0)
  OR (intent->>'target_epoch')::integer IS DISTINCT FROM (previous->>'target_epoch')::integer+1 OR intent->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256' THEN
  RAISE EXCEPTION 'v2 source transition does not extend its active predecessor';
 END IF;
 SELECT (s.seal_json->>'source_choice_at')::timestamp,r.round_id INTO choice_at,predecessor_id FROM competition_evaluation_seal s JOIN competition_round r USING(round_id)
 WHERE r.competition_id=competition AND r.staging=COALESCE((intent->>'staging')::boolean,false) AND r.epoch_number=(intent->>'target_epoch')::integer-1
  AND r.policy_json->'automation_policy'->>'environment'=env AND r.policy_json->'automation_policy'->>'season_id'=season;
 IF NOT FOUND OR (intent->>'prepared_at')::timestamp<choice_at OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=predecessor_id AND invalidates_evaluation) THEN
  RAISE EXCEPTION 'v2 source choice requires the preceding immutable seal and deadline';
 END IF;
 IF intent->>'kind'='reviewed_source_carry_forward' THEN
  IF intent->'commits' IS DISTINCT FROM previous->'commits' OR intent->>'source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
   OR intent->'source_generation' IS DISTINCT FROM previous->'source_generation' OR intent->'threshold' IS DISTINCT FROM previous->'threshold'
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM previous->>'threshold_evidence_sha256' OR intent->>'review_id' IS NOT NULL
   OR COALESCE(intent->>'reason','') NOT IN ('review_pending','promotion_not_ready','no_reward_winner','readiness_fallback') THEN
   RAISE EXCEPTION 'v2 carry-forward cannot alter reviewed source or its threshold';
  END IF;
 ELSIF intent->>'kind'='reviewed_source_promotion' THEN
  SELECT * INTO review FROM competition_integrity_review WHERE review_id=(intent->>'review_id')::uuid;
  IF NOT FOUND OR review.review_json->'request'->>'source_disposition' IS DISTINCT FROM 'approved'
   OR review.request_sha256 IS DISTINCT FROM intent->>'review_decision_digest'
   OR review.review_json->'request'->>'base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
   OR review.review_json->'request'->>'patch_sha256' IS DISTINCT FROM intent->>'approved_patch_sha256'
   OR review.created_at>choice_at OR NOT (intent->>'build_ready_at' IS NOT NULL AND (intent->>'build_ready_at')::timestamp<=choice_at)
   OR (intent->>'source_generation')::integer IS DISTINCT FROM (previous->>'source_generation')::integer+1
   OR intent->>'source_lock_sha256'=previous->>'source_lock_sha256' THEN
   RAISE EXCEPTION 'v2 promotion requires genuine exact-source approval ready by the deadline';
  END IF;
  SELECT * INTO evaluated_round FROM competition_round WHERE round_id=review.round_id;
  IF evaluated_round.staging IS DISTINCT FROM COALESCE((intent->>'staging')::boolean,false) OR evaluated_round.competition_id IS DISTINCT FROM competition OR evaluated_round.policy_json->'automation_policy'->>'environment' IS DISTINCT FROM env
   OR evaluated_round.policy_json->'automation_policy'->>'season_id' IS DISTINCT FROM season
   OR evaluated_round.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluated_image_digest'
   OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=review.round_id AND invalidates_evaluation) THEN
   RAISE EXCEPTION 'v2 promotion evidence belongs to another or invalidated evaluation';
  END IF;
  SELECT review_id INTO source_review_id FROM competition_integrity_review WHERE round_id=review.round_id AND job_id=review.job_id
   AND review_json->'request'->>'source_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
  SELECT review_json->'request'->>'reward_disposition' INTO reward_disposition FROM competition_integrity_review WHERE round_id=review.round_id AND job_id=review.job_id
   AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1;
  IF source_review_id IS DISTINCT FROM review.review_id OR COALESCE(reward_disposition,'') NOT IN ('approved','restored') THEN
   RAISE EXCEPTION 'v2 source approval is no longer current';
  END IF;
  SELECT score_json INTO candidate_score FROM competition_job WHERE job_id=review.job_id AND round_id=review.round_id AND state='succeeded';
  recommendation:=CASE WHEN COALESCE((intent->>'staging')::boolean,false) THEN 0 ELSE GREATEST((previous->>'threshold')::double precision,(candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_percent')::double precision/100) END;
  IF (NOT COALESCE((intent->>'staging')::boolean,false) AND NOT COALESCE((candidate_score->'significance'->>'recommended_next_epoch_takeover_margin_supported')::boolean,false))
   OR (intent->>'threshold')::double precision IS DISTINCT FROM recommendation
   OR intent->>'threshold_evidence_sha256' IS DISTINCT FROM review.review_json->'request'->>'score_sha256'
   OR ((intent->'commits')-'server') IS DISTINCT FROM ((previous->'commits')-'server') THEN
   RAISE EXCEPTION 'v2 promotion changed protected dependencies or transplanted threshold evidence';
  END IF;
 ELSE RAISE EXCEPTION 'unsupported v2 source decision'; END IF;
END `

const competitionMigration847CompetitionV2SourceInsertOrActivateGuardBody = `
BEGIN
 IF TG_OP='UPDATE' AND OLD.abandoned_at IS NOT NULL THEN RAISE EXCEPTION 'v2 abandoned source intent is immutable'; END IF;
 IF TG_OP='UPDATE' AND OLD.abandoned_at IS NULL AND NEW.abandoned_at IS NOT NULL THEN
  PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
  IF NEW.activated_at IS NOT NULL OR NOT EXISTS(SELECT 1 FROM competition_source_abandonment a
   WHERE a.environment=NEW.environment AND a.competition_id=NEW.competition_id AND a.season_id=NEW.season_id AND a.generation=NEW.generation AND a.created_at=NEW.abandoned_at
    AND a.abandonment_json->>'actor_id'=current_setting('urnetwork.competition_v2_owner',true)
    AND a.abandonment_json->>'fencing_token'=current_setting('urnetwork.competition_v2_fence',true)) THEN
   RAISE EXCEPTION 'v2 abandonment requires its exact durable fenced proof';
  END IF;
  RETURN NEW;
 END IF;
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.generation IS DISTINCT FROM (NEW.intent_json->>'generation')::bigint OR NEW.target_epoch IS DISTINCT FROM (NEW.intent_json->>'target_epoch')::integer
  OR NEW.intent_sha256 IS DISTINCT FROM NEW.intent_json->>'intent_sha256' OR NEW.abandoned_at IS NOT NULL THEN
  RAISE EXCEPTION 'v2 source columns disagree with immutable intent';
 END IF;
 IF TG_OP='INSERT' AND NEW.activated_at IS NOT NULL THEN RAISE EXCEPTION 'v2 source must be staged before activation'; END IF;
 PERFORM competition_v2_source_authorized(NEW.intent_json,NEW.environment,NEW.competition_id,NEW.season_id);
 IF NEW.intent_json->>'kind'='reviewed_source_promotion' AND NOT EXISTS(
  SELECT 1 FROM competition_integrity_review r JOIN competition_evaluation_seal s USING(round_id),jsonb_array_elements(s.seal_json->'entries') e
  WHERE r.review_id=(NEW.intent_json->>'review_id')::uuid AND (e->>'job_id')::uuid=r.job_id AND COALESCE((e->>'eligible')::boolean,false)
   AND competition_v2_score_eligible_mode(e->'score',(s.seal_json->>'baseline_raw_score')::double precision,(s.seal_json->>'takeover_margin')::double precision,COALESCE((NEW.intent_json->>'staging')::boolean,false))) THEN
  RAISE EXCEPTION 'v2 source promotion requires an eligible immutable sealed candidate';
 END IF;
 IF TG_OP='UPDATE' AND OLD.activated_at IS NULL AND NEW.activated_at IS NOT NULL AND
  (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=NEW.environment AND r.competition_id=NEW.competition_id AND r.season_id=NEW.season_id AND r.generation=NEW.generation
   AND r.receipt_json->>'manifest_sha256'=NEW.intent_json->>'manifest_sha256' AND r.receipt_json->>'source_lock_sha256'=NEW.intent_json->>'source_lock_sha256'
   AND (r.receipt_json->>'ready')::boolean)<>2 THEN
  RAISE EXCEPTION 'v2 source activation requires exact api and worker readiness receipts';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2SealGuardBody = `
DECLARE epoch_round competition_round%ROWTYPE; baseline_raw double precision; expected_count integer; entry jsonb; expected_eligible boolean;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 IF NOT FOUND OR epoch_round.staging IS DISTINCT FROM COALESCE((epoch_round.policy_json->'automation_policy'->>'staging')::boolean,false)
 OR epoch_round.staging IS DISTINCT FROM COALESCE((NEW.seal_json->>'staging')::boolean,false) OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR
 NOT COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) OR NEW.sealed_at < epoch_round.closes_at OR NEW.publication_at < epoch_round.reveal_at OR
 EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND state IN ('queued','running')) THEN
  RAISE EXCEPTION 'v2 seal requires closed fully drained exact-mode round';
 END IF;
 SELECT count(*) INTO expected_count FROM competition_job WHERE round_id=NEW.round_id;
 IF jsonb_array_length(NEW.seal_json->'entries') <> expected_count OR
 (SELECT count(DISTINCT e->>'job_id') FROM jsonb_array_elements(NEW.seal_json->'entries') e) <> expected_count THEN
  RAISE EXCEPTION 'v2 seal inventory is incomplete';
 END IF;
 IF EXISTS(SELECT 1 FROM (
   SELECT j.job_id,row_number() OVER(ORDER BY (j.score_json->>'raw_score')::double precision ASC NULLS LAST,j.submitted_at,j.job_id) AS rank
   FROM competition_job j WHERE j.round_id=NEW.round_id) ranked
   JOIN jsonb_array_elements(NEW.seal_json->'entries') e ON (e->>'job_id')::uuid=ranked.job_id
   WHERE (e->>'rank')::integer<>ranked.rank) THEN
  RAISE EXCEPTION 'v2 sealed ranking differs from exact raw order';
 END IF;
 SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
 FROM competition_round_baseline b, jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=NEW.round_id;
 FOR entry IN SELECT value FROM jsonb_array_elements(NEW.seal_json->'entries') LOOP
  IF NOT EXISTS(SELECT 1 FROM competition_job j WHERE j.round_id=NEW.round_id AND j.job_id=(entry->>'job_id')::uuid
    AND j.state=entry->>'state' AND j.patch_sha256=entry->>'patch_sha256'
    AND j.completed_at=(entry->>'completed_at')::timestamptz AND j.submitted_at=(entry->>'submitted_at')::timestamptz
    AND (j.score_json IS NOT DISTINCT FROM entry->'score')) THEN
   RAISE EXCEPTION 'v2 sealed evidence differs from terminal job';
  END IF;
  expected_eligible := entry->>'state'='succeeded' AND competition_v2_score_eligible_mode(entry->'score',baseline_raw,(epoch_round.policy_json->'evaluation_policy'->>'takeover_margin')::double precision,epoch_round.staging);
  IF expected_eligible AND epoch_round.policy_json->'automation_policy'->>'repeat_reward_policy'='exclude_exact_identity' AND EXISTS(
   SELECT 1 FROM competition_reward_decision d JOIN competition_job j ON j.job_id=(d.decision_json->>'selected_job_id')::uuid
   JOIN competition_round r ON r.round_id=d.round_id WHERE r.competition_id=epoch_round.competition_id
   AND r.policy_json->'automation_policy'->>'environment'=epoch_round.policy_json->'automation_policy'->>'environment'
   AND r.policy_json->'automation_policy'->>'season_id'=epoch_round.policy_json->'automation_policy'->>'season_id'
   AND r.policy_json->'automation_policy'->>'source_lock_sha256'=epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
   AND j.patch_sha256=entry->>'patch_sha256') THEN expected_eligible:=false; END IF;
  IF COALESCE((entry->>'eligible')::boolean,false) IS DISTINCT FROM expected_eligible THEN
   RAISE EXCEPTION 'v2 sealed eligibility contradicts scorer evidence';
  END IF;
 END LOOP;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2SourceAbandonmentGuardBody = `
DECLARE intent jsonb; activated timestamp; abandoned timestamp; pointer_generation bigint;
BEGIN
 PERFORM competition_v2_scope_lock(NEW.environment,NEW.competition_id,NEW.season_id);
 IF NOT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id
  AND owner_id=current_setting('urnetwork.competition_v2_owner',true) AND fencing_token::text=current_setting('urnetwork.competition_v2_fence',true) AND clock_timestamp()<lease_expires_at) THEN
  RAISE EXCEPTION 'v2 source abandonment requires the current fenced supervisor';
 END IF;
 SELECT intent_json,activated_at,abandoned_at INTO intent,activated,abandoned FROM competition_source_activation
 WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation FOR UPDATE;
 SELECT generation INTO pointer_generation FROM competition_source_pointer WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id;
 IF intent IS NULL OR intent->>'kind' IS DISTINCT FROM 'reviewed_source_promotion' OR activated IS NOT NULL OR abandoned IS NOT NULL
  OR pointer_generation IS DISTINCT FROM (intent->>'expected_generation')::bigint
  OR NEW.abandonment_json->>'environment' IS DISTINCT FROM NEW.environment OR NEW.abandonment_json->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR NEW.abandonment_json->>'season_id' IS DISTINCT FROM NEW.season_id OR (NEW.abandonment_json->>'generation')::bigint IS DISTINCT FROM NEW.generation
  OR NEW.abandonment_json->>'intent_sha256' IS DISTINCT FROM intent->>'intent_sha256'
  OR NEW.abandonment_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_v2_owner',true)
  OR NEW.abandonment_json->>'fencing_token' IS DISTINCT FROM current_setting('urnetwork.competition_v2_fence',true)
  OR (NEW.abandonment_json->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at
  OR COALESCE(NEW.abandonment_json->>'reason','') NOT IN ('publication_failed','readiness_failed','build_failed','source_approval_revoked')
  OR COALESCE(NEW.abandonment_json->>'unpublished_proof_sha256','') !~ '^[0-9a-f]{64}$'
  OR EXISTS(SELECT 1 FROM competition_source_readiness WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation)
  OR EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND staging=COALESCE((intent->>'staging')::boolean,false) AND epoch_number=(intent->>'target_epoch')::integer
   AND policy_json->'automation_policy'->>'environment'=NEW.environment AND policy_json->'automation_policy'->>'season_id'=NEW.season_id) THEN
  RAISE EXCEPTION 'v2 fallback must retain an exact verified-unpublished unactivated promotion';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2ReceiptIdentityGuardBody = `
DECLARE event competition_reward_outbox%ROWTYPE; receipt jsonb;
BEGIN
 SELECT * INTO event FROM competition_reward_outbox WHERE event_id=NEW.event_id;
 receipt:=NEW.receipt_json;
 IF NOT FOUND OR NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_v2_settlement_actor',true)
  OR NEW.actor_id IS DISTINCT FROM receipt->>'actor_id' OR NEW.status IS DISTINCT FROM receipt->>'status'
  OR NEW.payload_sha256 IS DISTINCT FROM event.payload_sha256 OR NEW.payload_sha256 IS DISTINCT FROM receipt->>'payload_sha256'
  OR (receipt->>'event_id')::uuid IS DISTINCT FROM NEW.event_id OR (receipt->>'round_id')::uuid IS DISTINCT FROM event.round_id
  OR (receipt->>'revision')::integer IS DISTINCT FROM event.revision
  OR receipt->>'decision_sha256' IS DISTINCT FROM event.payload_json->>'decision_sha256'
  OR receipt->>'environment' IS DISTINCT FROM event.payload_json->>'environment'
  OR receipt->>'competition_id' IS DISTINCT FROM event.payload_json->>'competition_id'
  OR receipt->>'season_id' IS DISTINCT FROM event.payload_json->>'season_id'
  OR (COALESCE((event.payload_json->>'staging')::boolean,false) AND
   (event.payload_json->>'settlement_mode' IS DISTINCT FROM 'shadow' OR COALESCE(receipt->>'chain_transaction','')<>'' OR receipt->>'applied_at' IS NOT NULL OR NEW.status NOT IN ('received','rejected')))
  OR NEW.created_at<event.publication_at THEN
  RAISE EXCEPTION 'v2 settlement receipt does not bind its scoped published event';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2RoundInsertGuardBody = `
DECLARE policy jsonb; intent jsonb; activated timestamp; expected_epoch integer; window_seconds integer; preparation_seconds integer;
BEGIN
 policy:=NEW.policy_json->'automation_policy';
 PERFORM pg_advisory_xact_lock(hashtextextended('competition-round-v1',0));
 IF NOT NEW.staging AND EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND staging AND NOT canceled
  AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false) AND automation_sealed_at IS NULL) THEN
  RAISE EXCEPTION 'v2 round cannot strand another active execution mode';
 END IF;
 IF NOT COALESCE((policy->>'enabled')::boolean,false) THEN RETURN NEW; END IF;
 IF NEW.staging AND EXISTS(SELECT 1 FROM competition_round WHERE competition_id=NEW.competition_id AND NOT staging) THEN
  RAISE EXCEPTION 'v2 round cannot strand another active execution mode';
 END IF;
 PERFORM competition_v2_require_source_lease(policy->>'environment',NEW.competition_id,policy->>'season_id');
 SELECT a.intent_json,a.activated_at INTO intent,activated FROM competition_source_pointer p JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
 WHERE p.environment=policy->>'environment' AND p.competition_id=NEW.competition_id AND p.season_id=policy->>'season_id' AND a.abandoned_at IS NULL;
 IF NOT FOUND OR activated IS NULL THEN RAISE EXCEPTION 'v2 round requires its exact active source pointer'; END IF;
 PERFORM competition_v2_source_mode(intent);
 window_seconds:=CASE WHEN NEW.staging THEN COALESCE((policy->>'staging_round_window_seconds')::integer,0) ELSE 604800 END;
 preparation_seconds:=CASE WHEN NEW.staging THEN COALESCE((policy->>'staging_preparation_seconds')::integer,0) ELSE 57600 END;
 IF NEW.staging IS DISTINCT FROM COALESCE((policy->>'staging')::boolean,false)
  OR NEW.staging IS DISTINCT FROM COALESCE((intent->>'staging')::boolean,false)
  OR COALESCE((policy->>'staging_round_window_seconds')::integer,0) IS DISTINCT FROM COALESCE((intent->>'staging_round_window_seconds')::integer,0)
  OR COALESCE((policy->>'staging_preparation_seconds')::integer,0) IS DISTINCT FROM COALESCE((intent->>'staging_preparation_seconds')::integer,0)
  OR NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL OR NEW.admission_closed_at IS NOT NULL
  OR (policy->>'version')::integer IS DISTINCT FROM 2 OR policy->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2'
  OR (policy->>'source_activation_generation')::bigint IS DISTINCT FROM (intent->>'generation')::bigint
  OR (policy->>'source_generation')::integer IS DISTINCT FROM (intent->>'source_generation')::integer
  OR policy->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR NEW.policy_json->>'base_sha' IS DISTINCT FROM intent->'commits'->>'server'
  OR NEW.policy_json->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluator_image_digest'
  OR (NEW.policy_json->'evaluation_policy'->>'takeover_margin')::double precision IS DISTINCT FROM (intent->>'threshold')::double precision
  OR NEW.policy_json->'evaluation_policy'->>'host_qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR NEW.epoch_number IS DISTINCT FROM (intent->>'target_epoch')::integer
  OR NEW.closes_at-NEW.opens_at<>make_interval(secs=>window_seconds) OR NEW.reveal_at<NEW.closes_at OR NEW.opens_at<activated
  OR NEW.opens_at<(intent->>'prepared_at')::timestamp+make_interval(secs=>preparation_seconds)
  OR (NEW.staging AND (policy->>'settlement_mode' IS DISTINCT FROM 'shadow'
   OR (NEW.policy_json->'evaluation_policy'->>'takeover_margin')::double precision IS DISTINCT FROM 0::double precision
   OR (NEW.policy_json->'evaluation_policy'->>'replicates')::integer IS DISTINCT FROM 9
   OR (NEW.policy_json->'season_policy'->>'submission_fee_usd')::double precision IS DISTINCT FROM 0::double precision
   OR (NEW.policy_json->'season_policy'->>'epoch_count')::integer IS DISTINCT FROM 6
   OR (NEW.policy_json->'season_policy'->>'submission_window_seconds')::integer IS DISTINCT FROM window_seconds
   OR (NEW.policy_json->'season_policy'->>'preparation_window_seconds')::integer IS DISTINCT FROM preparation_seconds)) THEN
  RAISE EXCEPTION 'v2 round requires the exact activated mode, zero-fee shadow staging policy and qualified full-window schedule';
 END IF;
 SELECT COALESCE(max(epoch_number),0)+1 INTO expected_epoch FROM competition_round
 WHERE competition_id=NEW.competition_id AND staging=NEW.staging AND
  (NOT NEW.staging OR (COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false)
   AND policy_json->'automation_policy'->>'environment'=policy->>'environment' AND policy_json->'automation_policy'->>'season_id'=policy->>'season_id'));
 IF NEW.epoch_number<>expected_epoch OR NEW.epoch_number<1 OR NEW.epoch_number>6 OR (NEW.epoch_number>1 AND NOT EXISTS(
  SELECT 1 FROM competition_round r WHERE r.competition_id=NEW.competition_id AND r.staging=NEW.staging AND r.epoch_number=NEW.epoch_number-1
   AND r.policy_json->'automation_policy'->>'environment'=policy->>'environment' AND r.policy_json->'automation_policy'->>'season_id'=policy->>'season_id'
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation)
   AND (r.finalized_at IS NOT NULL OR EXISTS(SELECT 1 FROM competition_evaluation_seal s WHERE s.round_id=r.round_id)))) THEN
  RAISE EXCEPTION 'v2 round predecessor is not sealed in the exact mode and season, or season is complete';
 END IF;
 IF COALESCE((SELECT active FROM competition_automation_hold WHERE environment=policy->>'environment' AND competition_id=NEW.competition_id AND season_id=policy->>'season_id' AND kind='admission' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'v2 admission is held';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2ReadinessModeGuardBody = `
DECLARE intent jsonb;
BEGIN
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF intent IS NULL OR COALESCE((NEW.receipt_json->>'staging')::boolean,false) IS DISTINCT FROM COALESCE((intent->>'staging')::boolean,false) THEN
  RAISE EXCEPTION 'v2 readiness mode differs from its exact source intent';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2PointerModeGuardBody = `
DECLARE intent jsonb; previous jsonb;
BEGIN
 SELECT intent_json INTO intent FROM competition_source_activation WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation;
 IF intent IS NULL THEN RAISE EXCEPTION 'v2 source pointer mode is unavailable'; END IF;
 PERFORM competition_v2_source_mode(intent);
 IF TG_OP='UPDATE' THEN
  SELECT intent_json INTO previous FROM competition_source_activation WHERE environment=OLD.environment AND competition_id=OLD.competition_id AND season_id=OLD.season_id AND generation=OLD.generation;
  IF COALESCE((intent->>'staging')::boolean,false) IS DISTINCT FROM COALESCE((previous->>'staging')::boolean,false) THEN
   RAISE EXCEPTION 'v2 source pointer cannot change execution mode';
  END IF;
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2DecisionModeGuardBody = `
DECLARE epoch_round competition_round%ROWTYPE; seal jsonb;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 IF COALESCE((NEW.decision_json->>'staging')::boolean,false) IS DISTINCT FROM epoch_round.staging
  OR COALESCE((seal->>'staging')::boolean,false) IS DISTINCT FROM epoch_round.staging
  OR (epoch_round.staging AND (NEW.decision_json->>'settlement_mode' IS DISTINCT FROM 'shadow'
   OR epoch_round.policy_json->'automation_policy'->>'settlement_mode' IS DISTINCT FROM 'shadow')) THEN
  RAISE EXCEPTION 'v2 reward decision cannot change its sealed staging or settlement mode';
 END IF;
 RETURN NEW;
END `

const competitionMigration847CompetitionV2RoundGuardBody = `
BEGIN
 IF COALESCE((OLD.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
  IF NEW.admission_closed_at IS DISTINCT FROM OLD.admission_closed_at THEN
   RAISE EXCEPTION 'v2 admission must retain its frozen full window';
  END IF;
  IF NEW.finalized_at IS NOT NULL OR NEW.winner_job_id IS NOT NULL THEN
   RAISE EXCEPTION 'v2 must not manufacture legacy finalization';
  END IF;
  IF (OLD.automation_sealed_at IS NOT NULL AND
     (OLD.automation_sealed_at IS DISTINCT FROM NEW.automation_sealed_at OR OLD.automation_publication_at IS DISTINCT FROM NEW.automation_publication_at)) OR
     ((NEW.automation_sealed_at IS DISTINCT FROM OLD.automation_sealed_at OR NEW.automation_publication_at IS DISTINCT FROM OLD.automation_publication_at) AND
     NOT EXISTS(SELECT 1 FROM competition_evaluation_seal WHERE round_id=NEW.round_id AND sealed_at=NEW.automation_sealed_at AND publication_at=NEW.automation_publication_at)) THEN
   RAISE EXCEPTION 'v2 publication requires its immutable seal';
  END IF;
 END IF;
 RETURN NEW;
END `

// The v847 successor retains the epoch constraint and the absence of the
// obsolete finalization blocker; only the active-staging index changes.
const competitionStagingLifecycleSharedArtifactQuery = `(
		           EXISTS (
		               SELECT 1 FROM constraint_artifact
		               WHERE table_name = 'competition_round'
		                 AND constraint_name = 'competition_round_epoch_kind'
		                 AND constraint_type = 'c' AND validated
		                 AND definition LIKE '%epoch_number >= 0%'
		                 AND definition LIKE '%epoch_number > 0%'
		           )
		           AND NOT EXISTS (
		               SELECT 1 FROM pg_trigger trigger_record
		               JOIN pg_class relation ON relation.oid = trigger_record.tgrelid
		               JOIN pg_namespace namespace ON namespace.oid = relation.relnamespace
		               WHERE namespace.nspname = 'public' AND relation.relname = 'competition_round'
		                 AND trigger_record.tgname = 'competition_staging_finalization_blocked'
		                 AND NOT trigger_record.tgisinternal
		           )
		       )`
