// Prospective intake authority never impersonates grading activation. This
// unregistered additive migration preserves every legacy guard branch intact.
package server

import (
	"fmt"
	"strings"
)

// The owner appends this exact SQL only after independent qualification. No
// source/provider guard or historical migration is regenerated in place here.
func CompetitionIntakeFirstMigrationSql() string {
	round, reserved, baseline := CompetitionIntakeFirstLegacyBodiesSql()
	wrap := func(name, marker, old, prefix string) string {
		return fmt.Sprintf("\nCREATE OR REPLACE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $%s$%s%s;\nEND $%s$;\n", name, marker, prefix, old, marker)
	}
	return competitionIntakeFirstTablesSql + competitionIntakeFirstAuthoritySql + competitionIntakeFirstRuntimeSql +
		wrap("competition_v2_round_insert_guard", "intake_round_wrapper", round, competitionIntakeFirstRoundPrefixSql) +
		wrap("competition_v2_round_reserved_identity_guard", "intake_reserved_wrapper", reserved, competitionIntakeFirstRoundPrefixSql) +
		wrap("competition_round_baseline_insert_guard", "intake_baseline_wrapper", baseline, competitionIntakeFirstBaselinePrefixSql) +
		wrap("competition_work_coverage_round_guard", "intake_coverage_round_wrapper", CompetitionIntakeFirstLegacyCoverageRoundBodySql(), competitionIntakeFirstCoverageRoundPrefixSql)
}

// Positional identity remains zero until integration appends this exact SQL;
// no new monitor may infer an ordinal from another branch's unpublished tail.
func CompetitionIntakeFirstMigrationVersion() int {
	version := 0
	sql := CompetitionIntakeFirstMigrationSql()
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			if version != 0 {
				panic("intake-first migration is registered more than once")
			}
			version = index + 1
		}
	}
	return version
}

// These exact published bodies remain contiguous nested legacy branches.
func CompetitionIntakeFirstLegacyBodiesSql() (round, reserved, baseline string) {
	_, round, _, _, _, _, _, _ = CompetitionSnowTwoMemberWinnerReviewBodiesSql()
	declaration := "CREATE FUNCTION competition_v2_round_reserved_identity_guard()"
	start := strings.Index(competitionAutomationTargetMigration, declaration)
	if start < 0 {
		panic("published reserved-round identity guard is absent")
	}
	parts := strings.Split(competitionAutomationTargetMigration[start:], "$guard$")
	if len(parts) < 3 {
		panic("published reserved-round identity guard is unterminated")
	}
	reserved = parts[1]
	_, _, _, _, baseline = CompetitionDistributedEvaluationBodiesSql()
	return
}

// New immutable records include the actual retained Go bytes. PostgreSQL JSONB
// formatting is not claimed to equal a typed Go canonical encoding.
const competitionIntakeFirstTablesSql = `
SET LOCAL lock_timeout = '5s';
CREATE TABLE competition_source_operator_handoff (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 target_epoch integer NOT NULL CHECK(target_epoch BETWEEN 2 AND 6), revision bigint NOT NULL CHECK(revision>0),
 idempotency_key varchar(128) NOT NULL, handoff_json jsonb NOT NULL,
 request_canonical bytea NOT NULL CHECK(octet_length(request_canonical) BETWEEN 1 AND 1048576), created_at timestamp NOT NULL,
 PRIMARY KEY(environment,competition_id,season_id,target_epoch,revision),
 UNIQUE(environment,competition_id,season_id,idempotency_key)
);
CREATE TABLE competition_source_intake_activation (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL,
 generation bigint NOT NULL, target_round_id uuid NOT NULL UNIQUE, target_epoch integer NOT NULL CHECK(target_epoch BETWEEN 2 AND 6),
 grant_json jsonb NOT NULL, grant_sha256 char(64) NOT NULL CHECK(grant_sha256 ~ '^[0-9a-f]{64}$'),
 round_policy_json jsonb NOT NULL, grant_canonical bytea NOT NULL, round_policy_canonical bytea NOT NULL, api_readiness_canonical bytea NOT NULL,
 created_at timestamp NOT NULL,
 CHECK(octet_length(grant_canonical) BETWEEN 1 AND 1048576),
 CHECK(octet_length(round_policy_canonical) BETWEEN 1 AND 1048576),
 CHECK(octet_length(api_readiness_canonical) BETWEEN 1 AND 1048576),
 PRIMARY KEY(environment,competition_id,season_id,generation),
 FOREIGN KEY(environment,competition_id,season_id,generation) REFERENCES competition_source_activation(environment,competition_id,season_id,generation)
);
CREATE TABLE competition_source_intake_pointer (
 environment varchar(128) NOT NULL, competition_id varchar(128) NOT NULL, season_id varchar(128) NOT NULL, generation bigint NOT NULL,
 PRIMARY KEY(environment,competition_id,season_id),
 FOREIGN KEY(environment,competition_id,season_id,generation) REFERENCES competition_source_intake_activation(environment,competition_id,season_id,generation)
);
CREATE TRIGGER competition_source_operator_handoff_append_only BEFORE UPDATE OR DELETE ON competition_source_operator_handoff FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_source_operator_handoff_no_truncate BEFORE TRUNCATE ON competition_source_operator_handoff FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_source_intake_activation_append_only BEFORE UPDATE OR DELETE ON competition_source_intake_activation FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_source_intake_activation_no_truncate BEFORE TRUNCATE ON competition_source_intake_activation FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_source_intake_pointer_no_truncate BEFORE TRUNCATE ON competition_source_intake_pointer FOR EACH STATEMENT EXECUTE FUNCTION competition_append_only_guard();
`

// All fresh authorizations take a physical shared fence as well as the scope
// lock: waiting under repeatable-read must not preserve an older review view.
const competitionIntakeFirstAuthoritySql = `
CREATE FUNCTION competition_v2_intake_policy() RETURNS jsonb LANGUAGE plpgsql AS $intake_policy$
DECLARE policy jsonb;
BEGIN
 policy:=NULLIF(current_setting('urnetwork.competition_intake_first_policy',true),'')::jsonb;
 IF policy IS NULL OR jsonb_typeof(policy) IS DISTINCT FROM 'object'
  OR policy->>'kind' IS DISTINCT FROM 'published-source-intake/v1' OR policy->'from_epoch' IS DISTINCT FROM '2'::jsonb
  OR COALESCE(policy->>'approval_sha256','') !~ '^[0-9a-f]{64}$' THEN
  RAISE EXCEPTION 'intake-first requires its exact prospective deployment approval';
 END IF;
 RETURN policy;
END $intake_policy$;

CREATE FUNCTION competition_v2_intake_handoff_valid(intent jsonb, handoff jsonb, positive boolean)
RETURNS boolean LANGUAGE plpgsql AS $intake_handoff_valid$
DECLARE request jsonb; product jsonb; predecessor uuid; decision jsonb; selected_kind text; selected_lock text;
BEGIN
 request:=handoff->'request'; product:=intent;
 IF NULLIF(intent->'trusted_instrument','null'::jsonb) IS NOT NULL THEN
  selected_kind:=intent->'trusted_instrument'->>'selected_product_kind';
  selected_lock:=intent->'trusted_instrument'->>'selected_product_source_lock_sha256';
  product:=intent||jsonb_build_object('kind',selected_kind);
 ELSE selected_kind:=intent->>'kind'; selected_lock:=intent->>'source_lock_sha256'; END IF;
 IF handoff->'schema' IS DISTINCT FROM '1'::jsonb OR handoff->>'kind' IS DISTINCT FROM 'source-operator-handoff/v1'
  OR handoff->>'environment' IS DISTINCT FROM intent->>'environment' OR handoff->>'competition_id' IS DISTINCT FROM intent->>'competition_id'
  OR handoff->>'season_id' IS DISTINCT FROM intent->>'season_id' OR request->'target_epoch' IS DISTINCT FROM intent->'target_epoch'
  OR request->>'product_source_lock_sha256' IS DISTINCT FROM selected_lock OR COALESCE(selected_lock,'') !~ '^[0-9a-f]{64}$'
  OR request->>'reward_decision_sha256' IS DISTINCT FROM intent->>'reward_decision_digest'
  OR COALESCE(request->>'evidence_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(handoff->>'actor_id','') !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
  OR NOT COALESCE((handoff->>'revision')::bigint=(request->>'expected_revision')::bigint+1,false) THEN RETURN false; END IF;
 SELECT round_id INTO predecessor FROM competition_round WHERE competition_id=intent->>'competition_id' AND NOT staging
  AND epoch_number=(intent->>'target_epoch')::integer-1 AND policy_json->'automation_policy'->>'environment'=intent->>'environment'
  AND policy_json->'automation_policy'->>'season_id'=intent->>'season_id';
 IF predecessor IS NULL OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=predecessor AND invalidates_evaluation) THEN RETURN false; END IF;
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=predecessor ORDER BY revision DESC LIMIT 1;
 IF NOT FOUND OR decision->>'decision_sha256' IS DISTINCT FROM request->>'reward_decision_sha256' THEN RETURN false; END IF;
 IF NOT competition_v2_winner_source_authorized(product,predecessor) THEN RETURN false; END IF;
 IF selected_kind='reviewed_source_promotion' THEN
  RETURN COALESCE(request->>'disposition' IN ('approve_promotion','reject_promotion')
   AND (NOT positive OR request->>'disposition'='approve_promotion')
   AND request->>'selected_job_id'=decision->>'selected_job_id' AND request->>'review_id'=intent->>'review_id'
   AND request->>'patch_sha256'=intent->>'approved_patch_sha256' AND request->>'review_decision_sha256'=intent->>'review_decision_digest'
   AND EXISTS(SELECT 1 FROM competition_integrity_review r WHERE r.review_id=(intent->>'review_id')::uuid AND r.round_id=predecessor
    AND r.job_id::text=decision->>'selected_job_id' AND r.request_sha256=intent->>'review_decision_digest'
    AND r.review_json->'request'->>'source_disposition'='approved'
    AND r.review_json->'request'->>'reward_disposition' IN ('approved','restored')
    AND NOT EXISTS(SELECT 1 FROM competition_integrity_review later WHERE later.round_id=r.round_id AND later.job_id=r.job_id AND later.review_cursor>r.review_cursor
     AND (later.review_json->'request'->>'source_disposition'<>'unchanged' OR later.review_json->'request'->>'reward_disposition'<>'unchanged'))),false);
 END IF;
 RETURN selected_kind='reviewed_source_carry_forward' AND intent->>'reason'='no_reward_winner'
  AND decision->>'selected_job_id' IS NULL AND request->>'disposition'='approve_no_winner_carry'
  AND request->>'selected_job_id' IS NULL AND request->>'review_id' IS NULL AND request->>'patch_sha256' IS NULL AND request->>'review_decision_sha256' IS NULL;
END $intake_handoff_valid$;

CREATE FUNCTION competition_v2_intake_require_handoff(intent jsonb) RETURNS void LANGUAGE plpgsql AS $intake_require_handoff$
DECLARE handoff jsonb;
BEGIN
 SELECT handoff_json INTO handoff FROM competition_source_operator_handoff WHERE environment=intent->>'environment'
  AND competition_id=intent->>'competition_id' AND season_id=intent->>'season_id' AND target_epoch=(intent->>'target_epoch')::integer ORDER BY revision DESC LIMIT 1;
 IF NOT FOUND OR NOT COALESCE(competition_v2_intake_handoff_valid(intent,handoff,true),false) THEN
  RAISE EXCEPTION 'intake source requires the current explicit genuine product handoff';
 END IF;
END $intake_require_handoff$;

CREATE FUNCTION competition_v2_intake_handoff_guard() RETURNS trigger LANGUAGE plpgsql AS $intake_handoff_guard$
DECLARE intent jsonb; revision bigint; configured jsonb;
BEGIN
 configured:=competition_v2_intake_policy();
 PERFORM competition_finalization_touch_scope(NEW.environment,NEW.competition_id,NEW.season_id);
 IF current_setting('urnetwork.competition_source_handoff_capability',true) IS DISTINCT FROM 'source_handoff'
  OR NEW.handoff_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_source_handoff_actor',true)
  OR COALESCE(NEW.handoff_json->>'actor_id','')='' THEN RAISE EXCEPTION 'manual source handoff requires its dedicated authenticated capability'; END IF;
 SELECT a.intent_json INTO intent FROM competition_source_activation a WHERE a.environment=NEW.environment AND a.competition_id=NEW.competition_id
  AND a.season_id=NEW.season_id AND a.target_epoch=NEW.target_epoch AND a.abandoned_at IS NULL ORDER BY a.generation DESC LIMIT 1;
 IF NOT FOUND THEN RAISE EXCEPTION 'manual handoff requires an existing exact source intent'; END IF;
 PERFORM competition_v2_source_authorized(intent,NEW.environment,NEW.competition_id,NEW.season_id);
 SELECT COALESCE(max(h.revision),0) INTO revision FROM competition_source_operator_handoff h WHERE h.environment=NEW.environment
  AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id AND h.target_epoch=NEW.target_epoch;
 IF NEW.revision<>revision+1 OR (NEW.handoff_json->>'revision')::bigint IS DISTINCT FROM NEW.revision
  OR (NEW.handoff_json->'request'->>'expected_revision')::bigint IS DISTINCT FROM revision
  OR NEW.handoff_json->'request'->>'idempotency_key' IS DISTINCT FROM NEW.idempotency_key
  OR convert_from(NEW.request_canonical,'UTF8')::jsonb IS DISTINCT FROM NEW.handoff_json->'request'
  OR NEW.handoff_json->>'request_sha256' IS DISTINCT FROM encode(sha256(NEW.request_canonical),'hex')
  OR (NEW.handoff_json->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at
  OR NEW.created_at>timezone('UTC',clock_timestamp())+interval '1 millisecond'
  OR NOT COALESCE(competition_v2_intake_handoff_valid(intent,NEW.handoff_json,false),false) THEN
  RAISE EXCEPTION 'manual source handoff is stale, malformed or targets a different product';
 END IF;
 RETURN NEW;
END $intake_handoff_guard$;
CREATE TRIGGER competition_source_operator_handoff_identity BEFORE INSERT ON competition_source_operator_handoff FOR EACH ROW EXECUTE FUNCTION competition_v2_intake_handoff_guard();

CREATE FUNCTION competition_v2_intake_grant_guard() RETURNS trigger LANGUAGE plpgsql AS $intake_grant_guard$
DECLARE intent jsonb; api jsonb; intake_grant jsonb; policy jsonb; automatic jsonb; configured jsonb;
BEGIN
 configured:=competition_v2_intake_policy();
 PERFORM competition_finalization_touch_scope(NEW.environment,NEW.competition_id,NEW.season_id);
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 SELECT a.intent_json INTO intent FROM competition_source_activation a WHERE a.environment=NEW.environment AND a.competition_id=NEW.competition_id
  AND a.season_id=NEW.season_id AND a.generation=NEW.generation AND a.abandoned_at IS NULL;
 IF NOT FOUND THEN RAISE EXCEPTION 'intake grant requires its exact existing source intent'; END IF;
 PERFORM competition_v2_source_authorized(intent,NEW.environment,NEW.competition_id,NEW.season_id);
 PERFORM competition_v2_intake_require_handoff(intent);
 SELECT receipt_json INTO api FROM competition_source_readiness WHERE environment=NEW.environment AND competition_id=NEW.competition_id
  AND season_id=NEW.season_id AND generation=NEW.generation AND role='api';
 IF NOT FOUND THEN RAISE EXCEPTION 'intake grant requires the genuine independent API acknowledgment'; END IF;
 intake_grant:=NEW.grant_json; policy:=NEW.round_policy_json; automatic:=policy->'automation_policy';
 IF convert_from(NEW.grant_canonical,'UTF8')::jsonb IS DISTINCT FROM intake_grant OR NEW.grant_sha256 IS DISTINCT FROM encode(sha256(NEW.grant_canonical),'hex')
  OR convert_from(NEW.round_policy_canonical,'UTF8')::jsonb IS DISTINCT FROM policy OR intake_grant->>'round_policy_sha256' IS DISTINCT FROM encode(sha256(NEW.round_policy_canonical),'hex')
  OR convert_from(NEW.api_readiness_canonical,'UTF8')::jsonb IS DISTINCT FROM api OR intake_grant->>'api_readiness_sha256' IS DISTINCT FROM encode(sha256(NEW.api_readiness_canonical),'hex')
  OR intake_grant->'schema' IS DISTINCT FROM '1'::jsonb OR intake_grant->>'kind' IS DISTINCT FROM 'published-source-intake/v1' OR intake_grant->'policy' IS DISTINCT FROM configured
  OR intake_grant->>'environment' IS DISTINCT FROM NEW.environment OR intake_grant->>'competition_id' IS DISTINCT FROM NEW.competition_id OR intake_grant->>'season_id' IS DISTINCT FROM NEW.season_id
  OR (intake_grant->>'generation')::bigint IS DISTINCT FROM NEW.generation OR (intake_grant->>'target_epoch')::integer IS DISTINCT FROM NEW.target_epoch
  OR intake_grant->>'target_round_id' IS DISTINCT FROM NEW.target_round_id::text OR intake_grant->>'intent_sha256' IS DISTINCT FROM intent->>'intent_sha256'
  OR (intake_grant->>'expected_generation')::bigint IS DISTINCT FROM (intent->>'expected_generation')::bigint
  OR NEW.target_round_id::text IS DISTINCT FROM intent->>'target_round_id' OR NEW.target_epoch IS DISTINCT FROM (intent->>'target_epoch')::integer
  OR NEW.target_epoch<2 OR NEW.target_epoch>6 OR COALESCE((intent->>'staging')::boolean,false)
  OR (intake_grant->>'created_at')::timestamp IS DISTINCT FROM NEW.created_at OR NEW.created_at<(intent->>'prepared_at')::timestamp
  OR NEW.created_at>timezone('UTC',clock_timestamp())+interval '1 millisecond'
  OR NOT COALESCE((intake_grant->>'retain_until')::timestamp>=(intake_grant->>'season_ends_at')::timestamp AND (intake_grant->>'season_ends_at')::timestamp>NEW.created_at,false)
  OR intake_grant ? 'opens_at' OR intake_grant ? 'closes_at'
  OR NOT COALESCE((SELECT t.state IN ('preparation_pending','preparation_blocked','preparation_ready','round_created')
   AND (t.transition_json->>'source_activation_generation')::bigint=NEW.generation FROM competition_supervisor_transition t
   WHERE t.environment=NEW.environment AND t.competition_id=NEW.competition_id AND t.season_id=NEW.season_id AND t.target_epoch=NEW.target_epoch ORDER BY t.revision DESC LIMIT 1),false)
  OR api->>'role' IS DISTINCT FROM 'api' OR api->'ready' IS DISTINCT FROM 'true'::jsonb
  OR api->>'environment' IS DISTINCT FROM NEW.environment OR api->>'competition_id' IS DISTINCT FROM NEW.competition_id OR api->>'season_id' IS DISTINCT FROM NEW.season_id
  OR (api->>'generation')::bigint IS DISTINCT FROM NEW.generation OR api->>'target_round_id' IS DISTINCT FROM NEW.target_round_id::text
  OR api->>'manifest_commit' IS DISTINCT FROM intent->>'manifest_commit' OR api->>'manifest_sha256' IS DISTINCT FROM intent->>'manifest_sha256'
  OR api->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256' OR api->>'qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR api->>'runtime_image_digest' IS DISTINCT FROM intake_grant->>'api_runtime_image_digest' OR COALESCE(api->>'runtime_image_digest','') !~ '^sha256:[0-9a-f]{64}$'
  OR COALESCE(api->>'actor_id','') !~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$'
  OR (api->>'recorded_at')::timestamp IS DISTINCT FROM (SELECT created_at FROM competition_source_readiness WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id AND generation=NEW.generation AND role='api')
  OR policy->'schema' IS DISTINCT FROM '2'::jsonb OR policy->>'competition_id' IS DISTINCT FROM NEW.competition_id
  OR policy->>'base_sha' IS DISTINCT FROM intent->'commits'->>'server' OR policy->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluator_image_digest'
  OR COALESCE(policy->>'evaluator_command','') NOT LIKE '/%' OR COALESCE(policy->>'evaluator_command_sha256','') !~ '^[0-9a-f]{64}$'
  OR jsonb_typeof(policy->'patch_policy') IS DISTINCT FROM 'object' OR jsonb_typeof(policy->'evaluation_policy') IS DISTINCT FROM 'object'
  OR policy->'score_schema' IS DISTINCT FROM '1'::jsonb
  OR NOT COALESCE((policy->'patch_policy'->>'max_patch_bytes')::integer>0,false)
  OR jsonb_typeof(policy->'patch_policy'->'allowed_paths') IS DISTINCT FROM 'array'
  OR jsonb_typeof(policy->'patch_policy'->'forbidden_paths') IS DISTINCT FROM 'array'
  OR NOT COALESCE(jsonb_array_length(policy->'patch_policy'->'allowed_paths')>0 AND jsonb_array_length(policy->'patch_policy'->'forbidden_paths')>0,false)
  OR COALESCE(policy->'evaluation_policy'->>'simulator_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(policy->'evaluation_policy'->>'scorer_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(policy->'evaluation_policy'->>'hardware_id','')=''
  OR policy->>'scorer_version' IS DISTINCT FROM 'sim-latency-score/3'
  OR policy->'evaluation_policy'->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
  OR jsonb_typeof(policy->'evaluation_policy'->'work_coverage_policy') IS DISTINCT FROM 'object'
  OR policy->'evaluation_policy'->'work_coverage_policy' IS DISTINCT FROM intent->'work_coverage_policy'
  OR (NULLIF(intent->'trusted_instrument','null'::jsonb) IS NOT NULL AND
   (intent->'trusted_instrument'->'approval'->'work_coverage_policy' IS DISTINCT FROM intent->'work_coverage_policy'
    OR intent->'trusted_instrument'->'build_receipt'->'work_coverage_policy' IS DISTINCT FROM intent->'work_coverage_policy'))
  OR automatic->'enabled' IS DISTINCT FROM 'true'::jsonb OR automatic->'version' IS DISTINCT FROM '2'::jsonb
  OR automatic->>'environment' IS DISTINCT FROM NEW.environment OR automatic->>'season_id' IS DISTINCT FROM NEW.season_id
  OR automatic->>'required_api_capability' IS DISTINCT FROM 'competition.reward.v2.runtime-blob.v1'
  OR (automatic->>'source_activation_generation')::bigint IS DISTINCT FROM NEW.generation OR automatic->'source_generation' IS DISTINCT FROM intent->'source_generation'
  OR automatic->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR COALESCE(automatic->>'winner_review_policy','') IS DISTINCT FROM COALESCE(intent->>'winner_review_policy','')
  OR COALESCE(automatic->'staging','false'::jsonb) IS DISTINCT FROM 'false'::jsonb OR (policy->'evaluation_policy'->>'replicates')::integer IS DISTINCT FROM 9
  OR policy->'evaluation_policy'->'takeover_margin' IS DISTINCT FROM intent->'threshold'
  OR policy->'evaluation_policy'->>'host_qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR COALESCE(policy->'evaluation_policy'->>'g2_volume_policy','') IS DISTINCT FROM COALESCE(intent->>'g2_volume_policy','')
  OR COALESCE(policy->'evaluation_policy'->>'simulator_sha256','') IS DISTINCT FROM COALESCE(intent->>'simulator_sha256','')
  OR COALESCE(policy->'evaluation_policy'->>'scorer_sha256','') IS DISTINCT FROM COALESCE(intent->>'scorer_sha256','')
  OR (policy->'season_policy'->>'submission_window_seconds')::integer IS DISTINCT FROM 604800
  OR (policy->'season_policy'->>'epoch_count')::integer IS DISTINCT FROM 6 THEN
  RAISE EXCEPTION 'intake grant must bind real complete frozen source, rules, image and API evidence';
 END IF;
 IF EXISTS(SELECT 1 FROM competition_automation_hold h WHERE h.environment=NEW.environment AND h.competition_id=NEW.competition_id AND h.season_id=NEW.season_id
  AND h.kind IN ('source_activation','admission') AND h.active AND h.revision=(SELECT max(last.revision) FROM competition_automation_hold last WHERE last.environment=h.environment AND last.competition_id=h.competition_id AND last.season_id=h.season_id AND last.kind=h.kind)) THEN
  RAISE EXCEPTION 'intake grant is held';
 END IF;
 RETURN NEW;
END $intake_grant_guard$;
CREATE TRIGGER competition_source_intake_activation_identity BEFORE INSERT ON competition_source_intake_activation FOR EACH ROW EXECUTE FUNCTION competition_v2_intake_grant_guard();

CREATE FUNCTION competition_v2_intake_pointer_guard() RETURNS trigger LANGUAGE plpgsql AS $intake_pointer_guard$
DECLARE intake_grant jsonb; prior bigint;
BEGIN
 IF TG_OP='DELETE' THEN RAISE EXCEPTION 'intake pointer cannot be deleted'; END IF;
 PERFORM competition_finalization_touch_scope(NEW.environment,NEW.competition_id,NEW.season_id);
 PERFORM competition_v2_require_source_lease(NEW.environment,NEW.competition_id,NEW.season_id);
 SELECT a.grant_json INTO intake_grant FROM competition_source_intake_activation a WHERE a.environment=NEW.environment AND a.competition_id=NEW.competition_id AND a.season_id=NEW.season_id AND a.generation=NEW.generation;
 SELECT generation INTO prior FROM competition_source_pointer WHERE environment=NEW.environment AND competition_id=NEW.competition_id AND season_id=NEW.season_id;
 IF intake_grant IS NULL OR prior IS DISTINCT FROM (intake_grant->>'expected_generation')::bigint OR NEW.generation<=prior
  OR (TG_OP='UPDATE' AND (NEW.environment IS DISTINCT FROM OLD.environment OR NEW.competition_id IS DISTINCT FROM OLD.competition_id OR NEW.season_id IS DISTINCT FROM OLD.season_id OR NEW.generation<=OLD.generation)) THEN
  RAISE EXCEPTION 'intake pointer requires exact monotonic predecessor compare-and-swap';
 END IF;
 RETURN NEW;
END $intake_pointer_guard$;
CREATE TRIGGER competition_source_intake_pointer_identity BEFORE INSERT OR UPDATE OR DELETE ON competition_source_intake_pointer FOR EACH ROW EXECUTE FUNCTION competition_v2_intake_pointer_guard();

CREATE FUNCTION competition_v2_intake_round_authorized(value jsonb) RETURNS void LANGUAGE plpgsql AS $intake_round_authorized$
DECLARE intake_grant competition_source_intake_activation%ROWTYPE; intent jsonb; automatic jsonb; expected_epoch integer; observed_at timestamp;
BEGIN
 SELECT * INTO intake_grant FROM competition_source_intake_activation WHERE target_round_id=(value->>'round_id')::uuid;
 IF NOT FOUND THEN RAISE EXCEPTION 'new intake round lacks its immutable source-purpose grant'; END IF;
 automatic:=value->'policy_json'->'automation_policy';
 PERFORM pg_advisory_xact_lock(hashtextextended('competition-round-v1',0));
 PERFORM competition_finalization_touch_scope(intake_grant.environment,intake_grant.competition_id,intake_grant.season_id);
 PERFORM competition_v2_require_source_lease(intake_grant.environment,intake_grant.competition_id,intake_grant.season_id);
 IF intake_grant.grant_json->'policy' IS DISTINCT FROM competition_v2_intake_policy() THEN RAISE EXCEPTION 'intake opening uses a different prospective approval'; END IF;
 SELECT a.intent_json INTO intent FROM competition_source_activation a JOIN competition_source_intake_pointer p USING(environment,competition_id,season_id,generation)
  WHERE a.environment=intake_grant.environment AND a.competition_id=intake_grant.competition_id AND a.season_id=intake_grant.season_id AND a.generation=intake_grant.generation AND a.abandoned_at IS NULL;
 IF NOT FOUND THEN RAISE EXCEPTION 'intake round source pointer is stale or abandoned'; END IF;
 PERFORM competition_v2_source_authorized(intent,intake_grant.environment,intake_grant.competition_id,intake_grant.season_id);
 PERFORM competition_v2_intake_require_handoff(intent);
 observed_at:=timezone('UTC',clock_timestamp());
 IF value->'staging' IS DISTINCT FROM 'false'::jsonb OR value->>'competition_id' IS DISTINCT FROM intake_grant.competition_id
  OR (value->>'epoch_number')::integer IS DISTINCT FROM intake_grant.target_epoch OR value->'policy_json' IS DISTINCT FROM intake_grant.round_policy_json
  OR value->'policy_json'->'evaluation_policy'->'work_coverage_policy' IS DISTINCT FROM intent->'work_coverage_policy'
  OR value->>'finalized_at' IS NOT NULL OR value->>'winner_job_id' IS NOT NULL OR value->>'admission_closed_at' IS NOT NULL
  OR value->>'automation_sealed_at' IS NOT NULL OR value->>'automation_publication_at' IS NOT NULL OR COALESCE((value->>'canceled')::boolean,false)
  OR (value->>'opens_at')::timestamp<timezone('UTC',transaction_timestamp()) OR (value->>'opens_at')::timestamp>observed_at
  OR (value->>'opens_at')::timestamp<intake_grant.created_at
  OR (value->>'opens_at')::timestamp<(intent->>'prepared_at')::timestamp+make_interval(secs=>(intake_grant.round_policy_json->'season_policy'->>'preparation_window_seconds')::integer)
  OR ((intent->>'opens_at')::timestamp>'1970-01-01'::timestamp)
  OR (value->>'closes_at')::timestamp-(value->>'opens_at')::timestamp<>interval '7 days'
  OR (value->>'reveal_at')::timestamp<(value->>'closes_at')::timestamp
  OR (value->>'closes_at')::timestamp>(intake_grant.grant_json->>'season_ends_at')::timestamp
  OR (value->>'reveal_at')::timestamp>=(intake_grant.grant_json->>'retain_until')::timestamp THEN
  RAISE EXCEPTION 'intake opening requires the actual complete post-handoff week inside season and retention';
 END IF;
 SELECT COALESCE(max(epoch_number),0)+1 INTO expected_epoch FROM competition_round WHERE competition_id=intake_grant.competition_id AND NOT staging;
 IF intake_grant.target_epoch IS DISTINCT FROM expected_epoch OR intake_grant.target_epoch NOT BETWEEN 2 AND 6
  OR EXISTS(SELECT 1 FROM competition_round WHERE competition_id=intake_grant.competition_id AND staging AND NOT canceled
   AND COALESCE((policy_json->'automation_policy'->>'enabled')::boolean,false) AND automation_sealed_at IS NULL)
  OR NOT EXISTS(SELECT 1 FROM competition_round r JOIN competition_evaluation_seal s USING(round_id) WHERE r.competition_id=intake_grant.competition_id AND NOT r.staging
   AND r.epoch_number=intake_grant.target_epoch-1 AND r.policy_json->'automation_policy'->>'environment'=intake_grant.environment
   AND r.policy_json->'automation_policy'->>'season_id'=intake_grant.season_id
   AND NOT EXISTS(SELECT 1 FROM competition_automation_incident i WHERE i.round_id=r.round_id AND i.invalidates_evaluation))
  OR COALESCE((SELECT active FROM competition_automation_hold WHERE environment=intake_grant.environment AND competition_id=intake_grant.competition_id AND season_id=intake_grant.season_id AND kind='admission' ORDER BY revision DESC LIMIT 1),false) THEN
  RAISE EXCEPTION 'intake opening is held, has an invalid predecessor or exceeds its season';
 END IF;
END $intake_round_authorized$;
`

// The optional branch is before, not inside, the exact legacy nested block.
const competitionIntakeFirstRoundPrefixSql = `
BEGIN
 IF EXISTS(SELECT 1 FROM competition_source_intake_activation WHERE target_round_id=NEW.round_id) THEN
  PERFORM competition_v2_intake_round_authorized(to_jsonb(NEW));
  RETURN NEW;
 END IF;
`

// Coverage's independent round guard otherwise requires a grading pointer too.
// The new insertion path authenticates the complete same source/approval/grant
// policy; its original insertion and every update remain the exact legacy body.
const competitionIntakeFirstCoverageRoundPrefixSql = `
DECLARE intake_source jsonb; coverage jsonb;
BEGIN
 IF TG_OP='INSERT' AND EXISTS(SELECT 1 FROM competition_source_intake_activation WHERE target_round_id=NEW.round_id) THEN
  PERFORM competition_v2_intake_round_authorized(to_jsonb(NEW));
  SELECT a.intent_json INTO intake_source FROM competition_source_intake_activation g JOIN competition_source_activation a
   USING(environment,competition_id,season_id,generation) WHERE g.target_round_id=NEW.round_id AND a.abandoned_at IS NULL;
  coverage:=NEW.policy_json->'evaluation_policy'->'work_coverage_policy';
  IF NOT COALESCE(competition_work_coverage_policy_valid(coverage),false) OR coverage IS DISTINCT FROM intake_source->'work_coverage_policy'
   OR NEW.policy_json->>'scorer_version' IS DISTINCT FROM 'sim-latency-score/3'
   OR NEW.policy_json->'evaluation_policy'->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
   OR (NULLIF(intake_source->'trusted_instrument','null'::jsonb) IS NOT NULL AND
    (coverage IS DISTINCT FROM intake_source->'trusted_instrument'->'approval'->'work_coverage_policy'
     OR coverage IS DISTINCT FROM intake_source->'trusted_instrument'->'build_receipt'->'work_coverage_policy')) THEN
   RAISE EXCEPTION 'intake coverage must bind the complete frozen source, grant and independent instrument approval';
  END IF;
  RETURN NEW;
 END IF;
`

// These predicates read durable runtime, baseline and no-op evidence; a source
// grant or API-only acknowledgment can never authorize a candidate clock.
const competitionIntakeFirstRuntimeSql = `
CREATE FUNCTION competition_v2_intake_runtime_ready(target uuid) RETURNS boolean LANGUAGE plpgsql AS $intake_runtime_ready$
DECLARE epoch_round competition_round%ROWTYPE; intake_grant competition_source_intake_activation%ROWTYPE; intent jsonb;
BEGIN
 SELECT * INTO intake_grant FROM competition_source_intake_activation WHERE target_round_id=target;
 IF NOT FOUND THEN RETURN true; END IF;
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=target;
 IF NOT FOUND OR epoch_round.canceled OR epoch_round.finalized_at IS NOT NULL OR epoch_round.automation_sealed_at IS NOT NULL
  OR epoch_round.policy_json IS DISTINCT FROM intake_grant.round_policy_json THEN RETURN false; END IF;
 SELECT a.intent_json INTO intent FROM competition_source_activation a JOIN competition_source_pointer p USING(environment,competition_id,season_id,generation)
  WHERE a.environment=intake_grant.environment AND a.competition_id=intake_grant.competition_id AND a.season_id=intake_grant.season_id
   AND a.generation=intake_grant.generation AND a.activated_at IS NOT NULL AND a.abandoned_at IS NULL;
 IF NOT FOUND OR intent->>'intent_sha256' IS DISTINCT FROM intake_grant.grant_json->>'intent_sha256'
  OR intent->>'target_round_id' IS DISTINCT FROM target::text
  OR intent->>'source_lock_sha256' IS DISTINCT FROM epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
  OR intent->>'evaluator_image_digest' IS DISTINCT FROM epoch_round.policy_json->>'evaluator_image_digest'
  OR (SELECT count(*) FROM competition_source_readiness r WHERE r.environment=intake_grant.environment AND r.competition_id=intake_grant.competition_id
   AND r.season_id=intake_grant.season_id AND r.generation=intake_grant.generation AND r.role IN ('api','worker')
   AND r.receipt_json->>'role'=r.role AND r.receipt_json->'ready'='true'::jsonb
   AND r.receipt_json->>'environment'=intake_grant.environment AND r.receipt_json->>'competition_id'=intake_grant.competition_id AND r.receipt_json->>'season_id'=intake_grant.season_id
   AND (r.receipt_json->>'generation')::bigint=intake_grant.generation AND r.receipt_json->>'target_round_id'=target::text
   AND r.receipt_json->>'source_lock_sha256'=intent->>'source_lock_sha256' AND r.receipt_json->>'manifest_commit'=intent->>'manifest_commit'
   AND r.receipt_json->>'manifest_sha256'=intent->>'manifest_sha256' AND r.receipt_json->>'qualification_sha256'=intent->>'host_qualification_sha256'
   AND COALESCE(r.receipt_json->>'runtime_image_digest','') ~ '^sha256:[0-9a-f]{64}$'
   AND (r.receipt_json->>'recorded_at')::timestamp=r.created_at
   AND (r.role<>'api' OR r.receipt_json=convert_from(intake_grant.api_readiness_canonical,'UTF8')::jsonb))<>2 THEN RETURN false; END IF;
 IF EXISTS(SELECT 1 FROM competition_automation_hold h WHERE h.environment=intake_grant.environment AND h.competition_id=intake_grant.competition_id AND h.season_id=intake_grant.season_id
  AND h.kind='evaluation' AND h.active AND h.revision=(SELECT max(last.revision) FROM competition_automation_hold last WHERE last.environment=h.environment AND last.competition_id=h.competition_id AND last.season_id=h.season_id AND last.kind=h.kind)) THEN RETURN false; END IF;
 PERFORM competition_v2_intake_require_handoff(intent);
 RETURN true;
END $intake_runtime_ready$;

CREATE FUNCTION competition_v2_intake_baseline_valid(target uuid, content bytea, digest text, base text, providers text, image text, scorer text, hardware text, qualification text)
RETURNS boolean LANGUAGE plpgsql AS $intake_baseline_valid$
DECLARE epoch_round competition_round%ROWTYPE; control jsonb;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=target;
 IF NOT FOUND OR content IS NULL OR octet_length(content) NOT BETWEEN 1 AND 1048576 OR digest IS DISTINCT FROM encode(sha256(content),'hex')
  OR base IS DISTINCT FROM epoch_round.policy_json->>'base_sha' OR providers IS DISTINCT FROM epoch_round.providers_sha256
  OR image IS DISTINCT FROM epoch_round.policy_json->>'evaluator_image_digest' OR scorer IS DISTINCT FROM epoch_round.policy_json->>'scorer_version'
  OR hardware IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'hardware_id'
  OR qualification IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->>'host_qualification_sha256' THEN RETURN false; END IF;
 control:=convert_from(content,'UTF8')::jsonb;
 IF control->>'round_id' IS DISTINCT FROM target::text OR control->>'kind' IS DISTINCT FROM 'sim-latency-score-baseline'
  OR control->'score_schema' IS DISTINCT FROM epoch_round.policy_json->'score_schema'
  OR control->>'scorer_version' IS DISTINCT FROM scorer
  OR control->'work_coverage_policy' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->'work_coverage_policy'
  OR COALESCE(control->>'g2_volume_policy','') IS DISTINCT FROM COALESCE(epoch_round.policy_json->'evaluation_policy'->>'g2_volume_policy','')
  OR control->'takeover_margin' IS DISTINCT FROM epoch_round.policy_json->'evaluation_policy'->'takeover_margin'
  OR jsonb_typeof(control->'replicates') IS DISTINCT FROM 'array' THEN RETURN false; END IF;
 RETURN jsonb_array_length(control->'replicates')=9 AND (epoch_round.policy_json->'evaluation_policy'->>'replicates')::integer=9
  AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(control->'replicates') r WHERE
   jsonb_typeof(r->'work_coverage') IS DISTINCT FROM 'object'
   OR NOT COALESCE((r->>'raw_score')::double precision>0 AND (r->>'request_count')::bigint>0 AND (r->>'received_bytes')::bigint>0,false));
END $intake_baseline_valid$;

CREATE FUNCTION competition_v2_intake_grading_ready(target uuid) RETURNS boolean LANGUAGE plpgsql AS $intake_grading_ready$
DECLARE epoch_round competition_round%ROWTYPE; baseline competition_round_baseline%ROWTYPE; policy jsonb;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_source_intake_activation WHERE target_round_id=target) THEN RETURN true; END IF;
 IF NOT competition_v2_intake_runtime_ready(target) THEN RETURN false; END IF;
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=target;
 SELECT * INTO baseline FROM competition_round_baseline WHERE round_id=target;
 IF NOT FOUND OR NOT competition_v2_intake_baseline_valid(target,baseline.baseline_json,baseline.baseline_sha256,baseline.base_sha,
  baseline.providers_sha256,baseline.evaluator_image_digest,baseline.scorer_version,baseline.hardware_id,baseline.host_qualification_sha256) THEN RETURN false; END IF;
 policy:=epoch_round.policy_json->'distributed_evaluation_policy';
 IF policy IS NOT NULL AND policy<>'null'::jsonb THEN
  IF jsonb_typeof(policy->'hosts') IS DISTINCT FROM 'array' OR jsonb_array_length(policy->'hosts')<2 THEN RETURN false; END IF;
  IF EXISTS(SELECT 1 FROM jsonb_array_elements(policy->'hosts') host WHERE NOT EXISTS(
   SELECT 1 FROM competition_executor_equivalence e JOIN competition_executor_qualification_run q ON q.round_id=e.round_id AND q.host_id=e.host_id
   WHERE e.round_id=target AND e.host_id=host->>'host_id' AND e.baseline_sha256=baseline.baseline_sha256
    AND e.receipt_json->>'round_id'=target::text AND e.receipt_json->>'host_id'=host->>'host_id'
    AND e.receipt_json->>'baseline_sha256'=baseline.baseline_sha256
    AND e.receipt_json->>'source_lock_sha256'=epoch_round.policy_json->'automation_policy'->>'source_lock_sha256'
    AND e.receipt_json->>'evaluator_image_digest'=epoch_round.policy_json->>'evaluator_image_digest'
    AND e.receipt_json->>'providers_sha256'=epoch_round.providers_sha256
    AND e.receipt_json->>'qualification_sha256'=host->>'qualification_sha256'
    AND e.receipt_json->>'policy_approval_sha256'=policy->>'approval_sha256'
    AND e.receipt_json->'passed'='true'::jsonb AND e.receipt_json->'noop'='true'::jsonb AND e.receipt_json->'replicates'='9'::jsonb
    AND e.receipt_json->>'qualification_job_id'=q.job_id::text
    AND (e.receipt_json->>'qualification_started_at')::timestamp=q.started_at AND (e.receipt_json->>'qualification_deadline_at')::timestamp=q.deadline_at
    AND e.created_at<=q.deadline_at AND COALESCE(e.receipt_json->>'archive_manifest_sha256','') ~ '^[0-9a-f]{64}$')) THEN RETURN false; END IF;
 END IF;
 RETURN true;
END $intake_grading_ready$;

CREATE FUNCTION competition_v2_intake_execution_guard() RETURNS trigger LANGUAGE plpgsql AS $intake_execution_guard$
DECLARE epoch_round competition_round%ROWTYPE; execute_candidate boolean;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM competition_source_intake_activation WHERE target_round_id=NEW.round_id) THEN RETURN NEW; END IF;
 IF TG_OP='INSERT' THEN
  execute_candidate:=NEW.state='running' OR NEW.attempt_count<>0 OR NEW.started_at IS NOT NULL
   OR NEW.evaluation_cycle<>0 OR NEW.cycle_started_at IS NOT NULL;
 ELSE
  execute_candidate:=(NEW.state='running' AND OLD.state IS DISTINCT FROM 'running')
   OR NEW.attempt_count IS DISTINCT FROM OLD.attempt_count OR NEW.started_at IS DISTINCT FROM OLD.started_at
   OR NEW.evaluation_cycle IS DISTINCT FROM OLD.evaluation_cycle OR NEW.cycle_started_at IS DISTINCT FROM OLD.cycle_started_at;
  -- This is permission to queue the already audited recovery metadata, not
  -- permission to claim it or start its new cycle clock. The independently
  -- retained recovery insert/immutable/atomic guards still run unchanged.
  IF OLD.evaluation_cycle=0 AND NEW.evaluation_cycle=1 AND OLD.state='failed' AND NEW.state='queued'
   AND NEW.attempt_count=OLD.attempt_count AND NEW.started_at IS NOT DISTINCT FROM OLD.started_at
   AND NEW.cycle_started_at IS NULL AND OLD.cycle_started_at IS NULL AND NEW.cycle_attempt_offset=OLD.attempt_count
   AND EXISTS(SELECT 1 FROM competition_job_reevaluation a WHERE a.job_id=OLD.job_id AND a.round_id=OLD.round_id
    AND a.transaction_id=pg_current_xact_id() AND a.original_terminal_json=to_jsonb(OLD)
    AND a.operator_id=current_setting('urnetwork.competition_reevaluation_operator',true)
    AND a.request_sha256=current_setting('urnetwork.competition_reevaluation_request',true) AND NEW.available_at=a.authorized_at
    AND (to_jsonb(NEW)-ARRAY['evaluation_cycle','cycle_attempt_offset','cycle_started_at','state','completed_at',
      'score_json','eval_error_json','artifact_manifest_json','artifact_manifest_sha256','available_at'])
     =(to_jsonb(OLD)-ARRAY['evaluation_cycle','cycle_attempt_offset','cycle_started_at','state','completed_at',
      'score_json','eval_error_json','artifact_manifest_json','artifact_manifest_sha256','available_at'])) THEN
   execute_candidate:=false;
  END IF;
 END IF;
 IF execute_candidate THEN
  SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
  PERFORM competition_finalization_touch_scope(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
  IF NOT COALESCE(competition_v2_intake_grading_ready(NEW.round_id),false) THEN
   RAISE EXCEPTION 'intake admission cannot start candidate clocks before actual runtime quorum, full R9 and host admission';
  END IF;
 END IF;
 RETURN NEW;
END $intake_execution_guard$;
CREATE TRIGGER competition_01_intake_execution BEFORE INSERT OR UPDATE ON competition_job FOR EACH ROW EXECUTE FUNCTION competition_v2_intake_execution_guard();

CREATE FUNCTION competition_v2_intake_ledger_fence_guard() RETURNS trigger LANGUAGE plpgsql AS $intake_ledger_fence$
DECLARE epoch_round competition_round%ROWTYPE;
BEGIN
 SELECT * INTO epoch_round FROM competition_round WHERE round_id=NEW.round_id;
 IF COALESCE((epoch_round.policy_json->'automation_policy'->>'enabled')::boolean,false) THEN
  PERFORM competition_finalization_touch_scope(epoch_round.policy_json->'automation_policy'->>'environment',epoch_round.competition_id,epoch_round.policy_json->'automation_policy'->>'season_id');
 END IF;
 RETURN NEW;
END $intake_ledger_fence$;
CREATE TRIGGER competition_01_intake_review_fence BEFORE INSERT ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_v2_intake_ledger_fence_guard();
CREATE TRIGGER competition_01_intake_decision_fence BEFORE INSERT ON competition_reward_decision FOR EACH ROW EXECUTE FUNCTION competition_v2_intake_ledger_fence_guard();
`

// Baseline preparation retains its queued source job and separate singleton
// reservation. It requires real runtime readiness, but not its own not-yet-
// existing baseline or later host-equivalence results.
const competitionIntakeFirstBaselinePrefixSql = `
BEGIN
 IF EXISTS(SELECT 1 FROM competition_source_intake_activation WHERE target_round_id=NEW.round_id) THEN
  IF NOT competition_v2_intake_runtime_ready(NEW.round_id) OR NEW.source_attempt<>1
   OR NOT competition_v2_intake_baseline_valid(NEW.round_id,NEW.baseline_json,NEW.baseline_sha256,NEW.base_sha,NEW.providers_sha256,
    NEW.evaluator_image_digest,NEW.scorer_version,NEW.hardware_id,NEW.host_qualification_sha256)
   OR NEW.source_artifact_manifest_sha256 IS DISTINCT FROM current_setting('urnetwork.competition_executor_baseline_manifest',true)
   OR NOT EXISTS(SELECT 1 FROM competition_job j JOIN competition_job_event e ON e.job_id=j.job_id
    JOIN competition_worker_slot s ON s.slot_id=1 AND s.job_id IS NULL AND s.worker_id=e.actor_id AND clock_timestamp()<s.lease_expires_at
    WHERE j.job_id=NEW.source_job_id AND j.round_id=NEW.round_id AND j.state='queued' AND j.attempt_count=0 AND j.started_at IS NULL
     AND e.event_type='round_baseline_preparation_reserved' AND e.actor_id=current_setting('urnetwork.competition_executor_baseline_owner',true)
     AND e.payload_json->>'context_sha256'=current_setting('urnetwork.competition_executor_baseline_context',true)
     AND e.payload_json->>'round_id'=NEW.round_id::text AND e.payload_json->>'job_id'=NEW.source_job_id::text)
   OR EXISTS(SELECT 1 FROM competition_job WHERE round_id=NEW.round_id AND state='succeeded') THEN
   RAISE EXCEPTION 'intake baseline requires its genuine reserved queued source and exact full R9 control';
  END IF;
  PERFORM competition_v2_require_source_lease((SELECT policy_json->'automation_policy'->>'environment' FROM competition_round WHERE round_id=NEW.round_id),
   (SELECT competition_id FROM competition_round WHERE round_id=NEW.round_id),(SELECT policy_json->'automation_policy'->>'season_id' FROM competition_round WHERE round_id=NEW.round_id));
  RETURN NEW;
 END IF;
`
