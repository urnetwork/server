// A private PostgreSQL fixture executes the real old and appended functions.
// Synthetic evidence timestamps prove prereview adoption without clock waiver.
package server

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Only the tested review functions and their real append/scope primitives are
// installed. No host database, provider or deployment settings are consulted.
func TestCompetitionEpochTwoOfficialReviewPostgresPrereviewAndGates(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		extract := func(sql, start, end string) string {
			i := strings.Index(sql, start)
			if i < 0 || strings.Count(sql, start) != 1 {
				tb.Fatal("test primitive source absent or ambiguous", start)
			}
			j := strings.Index(sql[i:], end)
			if j < 0 {
				tb.Fatal("test primitive source unterminated", start)
			}
			return sql[i : i+j+len(end)]
		}
		appendOnly := ""
		for _, migration := range migrations {
			if value, ok := migration.(*SqlMigration); ok && strings.Contains(value.sql, "CREATE FUNCTION competition_append_only_guard()") {
				if appendOnly != "" {
					tb.Fatal("published append guard duplicated")
				}
				appendOnly = extract(value.sql, "CREATE FUNCTION competition_append_only_guard()", "$competition_append_only_guard$;")
			}
		}
		if appendOnly == "" {
			tb.Fatal("published append guard missing")
		}
		scope := extract(competitionAutomationGuardMigration, "CREATE FUNCTION competition_v2_scope_lock", "$lock$;")
		prepare := competitionOfficialEpochTwoSqlFixture + appendOnly + scope + competitionProductionBenefitReviewMigration() + competitionOfficialReviewSql
		before := strings.ReplaceAll(competitionOfficialEpochTwoBeforeSql, "@addendum_sha256@", CompetitionProductionBenefitAddendumSha256())
		oldCondition := "(finding->>'disposition'='fail' AND request->>'source_disposition'='approved')"
		newCondition := "(finding->>'disposition'='fail' AND (request->>'reward_disposition'='approved' OR request->>'source_disposition'='approved'))"
		oldBody := CompetitionOfficialReviewEpochTwoBodiesSql().FindingsValid
		if strings.Count(oldBody, newCondition) != 1 {
			tb.Fatal("deferral root-cause counterfactual anchor changed")
		}
		oldBody = strings.Replace(oldBody, newCondition, oldCondition, 1)
		oldControl := "CREATE FUNCTION synthetic_official_old_deferral_findings(request jsonb) RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $old_deferral$" + oldBody + "$old_deferral$;"
		Db(tb.Context(), func(conn PgConn) {
			RaisePgResult(conn.Exec(tb.Context(), prepare+before+competitionOfficialReviewEpochTwoMigration()+oldControl+competitionOfficialEpochTwoAfterSql+
				competitionOfficialEpochTwoAstra62BeforeSql+competitionOfficialReviewEpochTwoAstra62Migration()+competitionOfficialEpochTwoAstra62AfterSql, pgx.QueryExecModeSimpleProtocol))
		}, OptReadWrite(), OptNoRetry())
	})
}

const competitionOfficialEpochTwoSqlFixture = `
CREATE TABLE competition_round(round_id uuid PRIMARY KEY,competition_id text,epoch_number integer,staging boolean,policy_json jsonb);
CREATE TABLE competition_evaluation_seal(round_id uuid,seal_json jsonb);
CREATE TABLE competition_reward_decision(round_id uuid,revision integer,decision_json jsonb);
CREATE TABLE competition_supervisor_transition(environment text,competition_id text,season_id text,target_epoch integer,revision integer,transition_json jsonb);
CREATE TABLE competition_round_baseline(round_id uuid,baseline_sha256 text,created_at timestamp);
CREATE TABLE competition_automation_incident(round_id uuid,invalidates_evaluation boolean);
CREATE TABLE competition_integrity_review(review_id uuid,round_id uuid,job_id uuid,actor_id text,idempotency_key text,request_sha256 text,review_json jsonb,created_at timestamp,review_cursor bigserial);
CREATE TABLE synthetic_official_request(epoch integer PRIMARY KEY,request jsonb);
CREATE FUNCTION synthetic_official_expect_reject(statement text) RETURNS void LANGUAGE plpgsql AS $reject$
BEGIN
 BEGIN EXECUTE statement; EXCEPTION WHEN raise_exception THEN RETURN; END;
 RAISE EXCEPTION 'synthetic official statement unexpectedly accepted';
END $reject$;
`

const competitionOfficialEpochTwoBeforeSql = `
DO $before_official_two$
DECLARE epoch integer; round_uuid uuid; request jsonb; findings jsonb; proof jsonb; entry jsonb; checkpoint jsonb;
BEGIN
 SELECT jsonb_agg(jsonb_build_object('criterion',criterion,'disposition','pass','reason_code','APPROVED','explanation','Synthetic complete finding.','evidence_sha256',repeat('7',64))) INTO findings
  FROM unnest(ARRAY['measurement_integrity','workload_generalization','containment','patch_scope','reproducibility','source_safety']) criterion;
 FOR epoch IN 1..2 LOOP
  round_uuid:=CASE WHEN epoch=1 THEN '00000000-0000-0000-0000-000000000101'::uuid ELSE '00000000-0000-0000-0000-000000000102'::uuid END;
  INSERT INTO competition_round VALUES(round_uuid,'synthetic-official',epoch,false,jsonb_build_object('automation_policy',jsonb_build_object('environment','synthetic','season_id','synthetic-season','review_criteria_version','synthetic-review/v1','repeat_reward_policy','exclude_exact_identity','winner_review_policy','elected_winner_snow_union_v2')));
  proof:=jsonb_build_object('authorization_sha256',repeat(epoch::text,64),'completed_at',CASE WHEN epoch=1 THEN timestamp '2000-01-01 01:01:00' ELSE timestamp '2000-01-01 00:20:00' END,
   'native_qualification_waived',epoch=1,'unaccepted_calibration_acknowledged',epoch=1);
  request:=jsonb_build_object('idempotency_key','synthetic-review','job_id','00000000-0000-0000-0000-000000000103','expected_revision',1,'expected_decision_sha256',repeat('a',64),
   'patch_sha256',repeat('b',64),'base_source_lock_sha256',repeat('c',64),'score_sha256',repeat('d',64),'baseline_sha256',repeat('e',64),'artifact_manifest_sha256',repeat('f',64),
   'criteria_version','synthetic-review/v1','reward_disposition','approved','source_disposition','approved','reason_codes',jsonb_build_array('APPROVED'),'findings',findings,
   'reviewer_model','gpt-6-astra','reviewer_reasoning','max','official_review_override',proof,'production_benefit_addendum',jsonb_build_object('version','production-benefit/1','sha256','@addendum_sha256@'),
   'private_evidence_sha256',repeat('6',64),'public_explanation','Synthetic review.','withheld_evidence_explanation','Synthetic retained private evidence.');
  INSERT INTO synthetic_official_request VALUES(epoch,request);
  entry:=jsonb_build_object('job_id',request->>'job_id','state','succeeded','completed_at',timestamp '2000-01-01 00:10:00','patch_sha256',request->>'patch_sha256',
   'base_source_lock_sha256',request->>'base_source_lock_sha256','score_sha256',request->>'score_sha256','artifact_manifest_sha256',request->>'artifact_manifest_sha256');
  INSERT INTO competition_evaluation_seal VALUES(round_uuid,jsonb_build_object('publication_at',timestamp '2000-01-01 01:00:00','baseline_sha256',request->>'baseline_sha256','entries',jsonb_build_array(entry)));
  INSERT INTO competition_reward_decision VALUES(round_uuid,1,jsonb_build_object('selected_job_id',request->>'job_id','revision',1,'decision_sha256',request->>'expected_decision_sha256'));
  INSERT INTO competition_round_baseline VALUES(round_uuid,request->>'baseline_sha256',timestamp '2000-01-01 00:00:00');
  checkpoint:=jsonb_build_object('round_id',round_uuid,'job_id',request->>'job_id','reward_revision',1,'reward_decision_sha256',request->>'expected_decision_sha256',
   'started_at',timestamp '2000-01-01 01:01:00','deadline',CASE WHEN epoch=1 THEN timestamp '2000-01-01 01:01:30' ELSE timestamp '2000-01-01 01:11:00' END);
  INSERT INTO competition_supervisor_transition VALUES('synthetic','synthetic-official','synthetic-season',epoch+1,1,jsonb_build_object('winner_review',checkpoint));
 END LOOP;
 PERFORM set_config('urnetwork.competition_official_review_actor','synthetic-operator',true);
 PERFORM set_config('urnetwork.competition_official_review_import_sha256',repeat('8',64),true);
 PERFORM set_config('urnetwork.competition_review_handoff_clock','2000-01-01 01:02:00',true);
 SELECT r.request INTO request FROM synthetic_official_request r WHERE r.epoch=1;
 INSERT INTO competition_official_review_authorization VALUES(repeat('1',64),'00000000-0000-0000-0000-000000000101','synthetic-operator',repeat('8',64),jsonb_build_object('round_id','00000000-0000-0000-0000-000000000101','review',request),timestamp '2000-01-01 01:02:00');
 IF NOT competition_official_review_request_authorized('00000000-0000-0000-0000-000000000101',request) THEN RAISE EXCEPTION 'legacy exact waiver control failed'; END IF;
 SELECT r.request INTO request FROM synthetic_official_request r WHERE r.epoch=2;
 IF competition_official_review_findings_valid(request) OR competition_official_review_request_authorized('00000000-0000-0000-0000-000000000102',request) THEN RAISE EXCEPTION 'old boundary unexpectedly supports epoch two'; END IF;
 PERFORM synthetic_official_expect_reject(format('INSERT INTO competition_official_review_authorization VALUES(%L,%L,%L,%L,%L::jsonb,%L::timestamp)',repeat('2',64),'00000000-0000-0000-0000-000000000102','synthetic-operator',repeat('8',64),jsonb_build_object('round_id','00000000-0000-0000-0000-000000000102','review',request),'2000-01-01 01:02:00'));
END $before_official_two$;
`

const competitionOfficialEpochTwoAfterSql = `
DO $after_official_two$
DECLARE request jsonb; altered jsonb; original_transition jsonb; original_seal jsonb; round_uuid uuid:='00000000-0000-0000-0000-000000000102';
BEGIN
 SELECT r.request INTO request FROM synthetic_official_request r WHERE r.epoch=1;
 IF NOT competition_official_review_request_authorized('00000000-0000-0000-0000-000000000101',request) THEN RAISE EXCEPTION 'append changed the legacy waiver'; END IF;
 SELECT r.request INTO request FROM synthetic_official_request r WHERE r.epoch=2;
 IF NOT competition_official_review_epoch_two_findings_valid(request) OR NOT competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:02:00') THEN RAISE EXCEPTION 'genuine prereview was not adopted'; END IF;
 INSERT INTO competition_official_review_authorization VALUES(repeat('2',64),round_uuid,'synthetic-operator',repeat('8',64),jsonb_build_object('round_id',round_uuid,'review',request),timestamp '2000-01-01 01:02:00');
 IF NOT competition_official_review_request_authorized(round_uuid,request) THEN RAISE EXCEPTION 'exact epoch-two import was not authorized'; END IF;
 FOR altered IN SELECT jsonb_set(request,'{official_review_override,native_qualification_waived}','true')
  UNION ALL SELECT jsonb_set(request,'{reviewer_model}','"synthetic-other-model"')
  UNION ALL SELECT jsonb_set(request,'{reviewer_reasoning}','"high"')
  UNION ALL SELECT jsonb_set(request,'{findings,0,model}','"synthetic-native-member"')
  UNION ALL SELECT jsonb_set(request,'{findings,0,disposition}','"uncertain"')
  UNION ALL SELECT jsonb_set(request,'{findings}',(request->'findings')-0)
  UNION ALL SELECT request-'production_benefit_addendum'
  UNION ALL SELECT jsonb_set(request,'{production_benefit_addendum,sha256}',to_jsonb(repeat('0',64)))
  UNION ALL SELECT jsonb_set(jsonb_set(request||jsonb_build_object('source_disposition','deferred','reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR')),'{findings,5,disposition}','"fail"'),'{findings,5,reason_code}','"UNSAFE_BEHAVIOR"')
  UNION ALL SELECT request||jsonb_build_object('reward_disposition','disqualified','source_disposition','rejected','reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR'))
  UNION ALL SELECT request||jsonb_build_object('reward_disposition','disqualified','source_disposition','deferred','reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR')) LOOP
  IF competition_official_review_epoch_two_findings_valid(altered) THEN RAISE EXCEPTION 'epoch-two content guard accepted invalid evidence'; END IF;
 END LOOP;
 altered:=request||jsonb_build_object('source_disposition','deferred','reason_codes',jsonb_build_array('SOURCE_NOT_READY'));
 IF NOT competition_official_review_epoch_two_findings_valid(altered) THEN RAISE EXCEPTION 'source deferral was forced approved'; END IF;
 altered:=jsonb_set(jsonb_set(request||jsonb_build_object('source_disposition','deferred','reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR')),'{findings,5,disposition}','"fail"'),'{findings,5,reason_code}','"UNSAFE_BEHAVIOR"');
 IF NOT synthetic_official_old_deferral_findings(altered) OR competition_official_review_epoch_two_findings_valid(altered) THEN RAISE EXCEPTION 'failed-honesty deferral counterfactual did not distinguish old/new predicates'; END IF;
 altered:=jsonb_set(request,'{findings,0}',(request->'findings'->0)||jsonb_build_object('disposition','fail','reason_code','UNSAFE_BEHAVIOR'));
 altered:=altered||jsonb_build_object('reward_disposition','disqualified','source_disposition','rejected','reason_codes',jsonb_build_array('UNSAFE_BEHAVIOR'));
 IF NOT competition_official_review_epoch_two_findings_valid(altered) THEN RAISE EXCEPTION 'substantiated rejection was forced approved'; END IF;
 SELECT jsonb_agg(CASE WHEN f->>'criterion'='workload_generalization' THEN f||jsonb_build_object('disposition','fail','reason_code','NO_PRODUCTION_BENEFIT') ELSE f END) INTO altered FROM jsonb_array_elements(request->'findings') f;
 altered:=request||jsonb_build_object('findings',altered,'reward_disposition','disqualified','source_disposition','rejected','reason_codes',jsonb_build_array('NO_PRODUCTION_BENEFIT'));
 IF NOT competition_official_review_epoch_two_findings_valid(altered) THEN RAISE EXCEPTION 'non-misconduct production-benefit ineligibility was erased'; END IF;
 FOR altered IN SELECT jsonb_set(request,'{official_review_override,completed_at}','"2000-01-01T00:09:59Z"')
  UNION ALL SELECT jsonb_set(request,'{official_review_override,completed_at}','"2000-01-01T01:02:01Z"')
  UNION ALL SELECT jsonb_set(request,'{job_id}','"00000000-0000-0000-0000-000000000104"')
  UNION ALL SELECT jsonb_set(request,'{score_sha256}',to_jsonb(repeat('0',64)))
  UNION ALL SELECT jsonb_set(request,'{patch_sha256}',to_jsonb(repeat('0',64)))
  UNION ALL SELECT jsonb_set(request,'{base_source_lock_sha256}',to_jsonb(repeat('0',64)))
  UNION ALL SELECT jsonb_set(request,'{artifact_manifest_sha256}',to_jsonb(repeat('0',64)))
  UNION ALL SELECT jsonb_set(request,'{baseline_sha256}',to_jsonb(repeat('0',64)))
  UNION ALL SELECT jsonb_set(request,'{expected_revision}','2')
  UNION ALL SELECT request||jsonb_build_object('handoff_id','00000000-0000-0000-0000-000000000104') LOOP
  IF competition_official_review_epoch_two_binding(round_uuid,altered,timestamp '2000-01-01 01:02:00') THEN RAISE EXCEPTION 'prereview crossed evidence, election or actual completion'; END IF;
 END LOOP;
 IF competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:00:59')
  OR competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:11:00') THEN RAISE EXCEPTION 'prereview renewed the import window'; END IF;
 SELECT transition_json INTO original_transition FROM competition_supervisor_transition WHERE target_epoch=3;
 FOR altered IN SELECT '{}'::jsonb UNION ALL SELECT jsonb_set(original_transition,'{winner_review,handoff_id}','"00000000-0000-0000-0000-000000000104"')
  UNION ALL SELECT jsonb_set(original_transition,'{winner_review,job_id}','"00000000-0000-0000-0000-000000000104"') LOOP
  UPDATE competition_supervisor_transition SET transition_json=altered WHERE target_epoch=3;
  IF competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:02:00') THEN RAISE EXCEPTION 'missing or foreign checkpoint accepted'; END IF;
 END LOOP;
 UPDATE competition_supervisor_transition SET transition_json=original_transition WHERE target_epoch=3;
 UPDATE competition_round_baseline SET created_at=timestamp '2000-01-01 00:20:01' WHERE round_id=round_uuid;
 IF competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:02:00') THEN RAISE EXCEPTION 'future baseline was reviewed before existence'; END IF;
 UPDATE competition_round_baseline SET created_at=timestamp '2000-01-01 00:00:00' WHERE round_id=round_uuid;
 UPDATE competition_round SET staging=true WHERE round_id=round_uuid;
 IF competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:02:00') THEN RAISE EXCEPTION 'production reviewer substitution entered staging'; END IF;
 UPDATE competition_round SET staging=false,epoch_number=3 WHERE round_id=round_uuid;
 IF competition_official_review_epoch_two_binding(round_uuid,request,timestamp '2000-01-01 01:02:00') THEN RAISE EXCEPTION 'reviewer substitution crossed epoch'; END IF;
 UPDATE competition_round SET epoch_number=2 WHERE round_id=round_uuid;
 INSERT INTO competition_automation_incident VALUES(round_uuid,true);
 IF competition_official_review_request_authorized(round_uuid,request) THEN RAISE EXCEPTION 'review overrode invalid evaluation'; END IF;
 DELETE FROM competition_automation_incident WHERE round_id=round_uuid;
 PERFORM set_config('urnetwork.competition_official_review_actor','synthetic-wrong-operator',true);
 altered:=jsonb_set(request,'{official_review_override,authorization_sha256}',to_jsonb(repeat('3',64)));
 PERFORM synthetic_official_expect_reject(format('INSERT INTO competition_official_review_authorization VALUES(%L,%L,%L,%L,%L::jsonb,%L::timestamp)',repeat('3',64),round_uuid,'synthetic-operator',repeat('8',64),jsonb_build_object('round_id',round_uuid,'review',altered),'2000-01-01 01:02:00'));
 PERFORM synthetic_official_expect_reject('UPDATE competition_official_review_authorization SET actor_id=''synthetic-changed''');
 IF (SELECT count(*) FROM competition_official_review_authorization)<>2 THEN RAISE EXCEPTION 'invalid import added authority'; END IF;
END $after_official_two$;
`

const competitionOfficialEpochTwoAstra62BeforeSql = `
DO $before_official_astra62$
DECLARE request jsonb;
BEGIN
 SELECT r.request||jsonb_build_object('reviewer_model','gpt-6.2-astra') INTO request FROM synthetic_official_request r WHERE r.epoch=2;
 IF competition_official_review_epoch_two_findings_valid(request) THEN
  RAISE EXCEPTION 'published reviewer predicate no longer distinguishes exact Astra 6.2 provenance';
 END IF;
END $before_official_astra62$;
`

const competitionOfficialEpochTwoAstra62AfterSql = `
DO $after_official_astra62$
DECLARE legacy_one jsonb; legacy_two jsonb; request jsonb; altered jsonb;
 round_one uuid:='00000000-0000-0000-0000-000000000101'; round_two uuid:='00000000-0000-0000-0000-000000000102';
 imported_at timestamp:='2000-01-01 01:02:00';
BEGIN
 SELECT r.request INTO legacy_one FROM synthetic_official_request r WHERE r.epoch=1;
 SELECT r.request INTO legacy_two FROM synthetic_official_request r WHERE r.epoch=2;
 IF NOT competition_official_review_request_authorized(round_one,legacy_one)
  OR NOT competition_official_review_request_authorized(round_two,legacy_two)
  OR competition_official_review_findings_valid(legacy_one||jsonb_build_object('reviewer_model','gpt-6.2-astra')) THEN
  RAISE EXCEPTION 'reviewer ID append altered historical authorization or the legacy epoch-one provider';
 END IF;
 request:=legacy_two||jsonb_build_object('reviewer_model','gpt-6.2-astra');
 IF NOT competition_official_review_epoch_two_findings_valid(request)
  OR NOT competition_official_review_epoch_two_binding(round_two,request,imported_at)
  OR competition_official_review_request_authorized(round_two,request)
  OR competition_official_review_epoch_two_binding(round_one,request,imported_at)
  OR competition_official_review_epoch_two_binding(round_two,request,timestamp '2000-01-01 01:11:00') THEN
  RAISE EXCEPTION 'reviewer ID did not extend shape only under the exact existing bindings';
 END IF;

 FOR altered IN SELECT request||jsonb_build_object('reviewer_model','gpt-6.2-sol')
  UNION ALL SELECT request||jsonb_build_object('reviewer_model','gpt-6.2-astra ')
  UNION ALL SELECT request||jsonb_build_object('reviewer_model','Astra Max')
  UNION ALL SELECT request||jsonb_build_object('reviewer_model',NULL)
  UNION ALL SELECT request-'reviewer_model'
  UNION ALL SELECT request||jsonb_build_object('reviewer_reasoning','high')
  UNION ALL SELECT request-'reviewer_reasoning'
  UNION ALL SELECT jsonb_set(request,'{official_review_override,native_qualification_waived}','true') LOOP
  IF competition_official_review_epoch_two_findings_valid(altered) OR competition_official_review_epoch_two_binding(round_two,altered,imported_at) THEN
   RAISE EXCEPTION 'changed or missing reviewer identity, effort or custody passed the successor';
  END IF;
 END LOOP;

 request:=jsonb_set(request,'{official_review_override,authorization_sha256}',to_jsonb(repeat('4',64)));
 PERFORM set_config('urnetwork.competition_official_review_actor','synthetic-operator',true);
 PERFORM set_config('urnetwork.competition_official_review_import_sha256',repeat('8',64),true);
 PERFORM synthetic_official_expect_reject(format('INSERT INTO competition_official_review_authorization VALUES(%L,%L,%L,%L,%L::jsonb,%L::timestamp)',
  repeat('4',64),round_two,'synthetic-operator',repeat('9',64),jsonb_build_object('round_id',round_two,'review',request),imported_at));
 PERFORM set_config('urnetwork.competition_official_review_import_sha256',repeat('9',64),true);
 INSERT INTO competition_official_review_authorization VALUES(repeat('4',64),round_two,'synthetic-operator',repeat('9',64),jsonb_build_object('round_id',round_two,'review',request),imported_at);
 IF NOT competition_official_review_request_authorized(round_two,request)
  OR competition_official_review_request_authorized(round_one,request)
  OR competition_official_review_request_authorized(round_two,request||jsonb_build_object('reviewer_model','gpt-6-astra'))
  OR competition_official_review_request_authorized(round_two,request||jsonb_build_object('public_explanation','Changed after authorization.'))
  OR (SELECT import_json->'review' FROM competition_official_review_authorization WHERE authorization_sha256=repeat('1',64)) IS DISTINCT FROM legacy_one
  OR (SELECT import_json->'review' FROM competition_official_review_authorization WHERE authorization_sha256=repeat('2',64)) IS DISTINCT FROM legacy_two
  OR (SELECT import_json->'review' FROM competition_official_review_authorization WHERE authorization_sha256=repeat('4',64)) IS DISTINCT FROM request
  OR (SELECT count(*) FROM competition_official_review_authorization)<>3 THEN
  RAISE EXCEPTION 'exact new authorization was absent or rerecorded legacy evidence';
 END IF;
 PERFORM synthetic_official_expect_reject('UPDATE competition_official_review_authorization SET actor_id=''synthetic-changed''');
END $after_official_astra62$;
`
