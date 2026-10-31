// Isolated PostgreSQL proves that an instrument approval cannot replace
// actual winner authorization, change product dependencies or leak into epoch one.
package server

import (
	"strings"
	"testing"
)

// The registered successor retains the full latest product authorization and
// its function oid, rather than renaming a function still bound to old triggers.
func TestCompetitionTrustedInstrumentMigrationRetainsLegacyAuthority(t *testing.T) {
	sql := competitionTrustedInstrumentMigration()
	previous := competitionRepositoryPatchMigration()
	anchor := "BEGIN\n PERFORM competition_v2_source_mode(intent);"
	modified := strings.Replace(previous, anchor,
		"BEGIN\n intent:=competition_v2_instrument_product_intent(intent,env,competition,season);\n PERFORM competition_v2_source_mode(intent);", 1)
	modified = strings.Replace(modified, "  IF evaluated_round.staging IS DISTINCT FROM",
		competitionTrustedInstrumentPathBaseSql+"  IF evaluated_round.staging IS DISTINCT FROM", 1)
	if sql != competitionTrustedInstrumentSql+modified || sql != competitionTrustedInstrumentMigration() {
		t.Fatal("instrument migration changed unrelated authorization or is nondeterministic")
	}
	for _, required := range []string{
		"approval IS DISTINCT FROM configured", "urnetwork.competition_v2_instrument_approval_json",
		"competition_v2_winner_source_authorized(intent,predecessor_id)",
		"intent->>'threshold_evidence_sha256' IS DISTINCT FROM review.review_json->'request'->>'score_sha256'",
		"intent->>'candidate_path_base' IS DISTINCT FROM COALESCE(evaluated_round.policy_json->'patch_policy'->>'path_base','')",
		"receipt->>'review_id' IS DISTINCT FROM '00000000-0000-0000-0000-000000000000'",
	} {
		if !strings.Contains(sql, required) {
			t.Errorf("instrument migration omitted %q", required)
		}
	}
	for _, forbidden := range []string{"ALTER FUNCTION", "CREATE TRIGGER", "UPDATE competition_", "DELETE FROM competition_"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("instrument migration changed unrelated historical authority: %s", forbidden)
		}
	}
	registered := 0
	for _, migration := range migrations {
		if migration, ok := migration.(*SqlMigration); ok && migration.sql == sql {
			registered++
		}
	}
	if registered != 1 {
		t.Fatal("trusted instrument migration must be registered exactly once")
	}
}

// Both unknown approved winner code and unchanged incumbent code traverse the
// actual source authorizer; no manufactured winner review is used for a carry.
func TestCompetitionTrustedInstrumentDatabasePreservesSelectedProductAuthority(t *testing.T) {
	testCompetitionWinnerReviewDatabaseGuards(t, competitionTrustedInstrumentTestMigrations(),
		competitionTrustedInstrumentSqlFixture+`
DO $selected$
DECLARE intent jsonb; product jsonb; rejected boolean;
BEGIN
 intent:=competition_test_instrument_intent('reviewed_source_promotion');
 product:=competition_v2_instrument_product_intent(intent,'test','competition-test','season-test');
 IF product->'commits' IS DISTINCT FROM intent->'trusted_instrument'->'selected_product_commits'
  OR product->>'review_id' IS DISTINCT FROM intent->>'review_id'
  OR product->>'kind' IS DISTINCT FROM 'reviewed_source_promotion'
  OR product ? 'trusted_instrument' THEN RAISE EXCEPTION 'winner product was not preserved'; END IF;
 PERFORM competition_v2_source_authorized(intent,'test','competition-test','season-test');
 UPDATE competition_integrity_review SET job_id='00000000-0000-0000-0000-000000000004';
 rejected:=false;
 BEGIN PERFORM competition_v2_source_authorized(intent,'test','competition-test','season-test');
 EXCEPTION WHEN OTHERS THEN rejected:=SQLERRM LIKE '%genuinely reviewed elected winner%'; END;
 IF NOT rejected THEN RAISE EXCEPTION 'instrument proof bypassed the actual elected winner'; END IF;
 UPDATE competition_integrity_review SET job_id='00000000-0000-0000-0000-000000000002';
 UPDATE competition_reward_decision SET decision_json=jsonb_build_object('selected_job_id',NULL,'decision_sha256',repeat('a',64));
 intent:=competition_test_instrument_intent('reviewed_source_carry_forward');
 product:=competition_v2_instrument_product_intent(intent,'test','competition-test','season-test');
 IF product->>'kind' IS DISTINCT FROM 'reviewed_source_carry_forward' OR product->>'source_generation' IS DISTINCT FROM '0'
  OR product->>'review_id' IS NOT NULL THEN RAISE EXCEPTION 'instrument carry invented product review or generation'; END IF;
 PERFORM competition_v2_source_authorized(intent,'test','competition-test','season-test');
END $selected$;`)
}

// Homogeneous mutations force every boundary without scheduler timing or any
// production host, token, database, source identity or configuration fixture.
func TestCompetitionTrustedInstrumentDatabaseRejectsApprovalAndLineageTampering(t *testing.T) {
	testCompetitionWinnerReviewDatabaseGuards(t, competitionTrustedInstrumentTestMigrations(),
		competitionTrustedInstrumentSqlFixture+`
DO $tamper$
DECLARE intent jsonb; altered jsonb; product jsonb; mutation integer; rejected boolean;
BEGIN
 intent:=competition_test_instrument_intent('reviewed_source_promotion');
 PERFORM competition_v2_source_authorized(intent,'test','competition-test','season-test');
 FOR mutation IN 1..23 LOOP
  altered:=CASE mutation
   WHEN 1 THEN intent-'trusted_instrument'
   WHEN 2 THEN jsonb_set(intent,'{target_epoch}','1')
   WHEN 3 THEN jsonb_set(intent,'{staging}','true')
   WHEN 4 THEN jsonb_set(intent,'{trusted_instrument,approval,protected_paths}','["product.go"]')
   WHEN 5 THEN jsonb_set(intent,'{trusted_instrument,approval,patch}',to_jsonb(encode(convert_to('altered','UTF8'),'base64')))
   WHEN 6 THEN jsonb_set(intent,'{trusted_instrument,selected_product_source_lock_sha256}',to_jsonb(repeat('f',64)))
   WHEN 7 THEN jsonb_set(intent,'{commits,connect}',to_jsonb(repeat('f',40)))
   WHEN 8 THEN jsonb_set(intent,'{trusted_instrument,instrumented_server_commit}',to_jsonb(repeat('f',40)))
   WHEN 9 THEN jsonb_set(intent,'{trusted_instrument,build_receipt,review_id}','"00000000-0000-0000-0000-000000000003"')
   WHEN 10 THEN jsonb_set(intent,'{trusted_instrument,build_receipt,no_main_credentials}','false')
   WHEN 11 THEN jsonb_set(intent,'{trusted_instrument,selected_product_review_digest}',to_jsonb(repeat('f',64)))
   WHEN 12 THEN jsonb_set(intent,'{scorer_sha256}',to_jsonb(repeat('9',64)))
   WHEN 13 THEN jsonb_set(intent,'{source_generation}','2')
   WHEN 14 THEN jsonb_set(intent,'{git_refs,server,commit}',to_jsonb(repeat('f',40)))
   WHEN 15 THEN jsonb_set(intent,'{trusted_instrument,build_receipt,source_lock_sha256}',to_jsonb(repeat('f',64)))
   WHEN 16 THEN jsonb_set(intent,'{g2_volume_policy}','"unknown-policy"')
   WHEN 17 THEN intent-'g2_volume_policy'
   WHEN 18 THEN jsonb_set(intent,'{trusted_instrument,build_receipt,g2_volume_policy}','""')
   WHEN 19 THEN jsonb_set(intent,'{environment}','"other-test-environment"')
   WHEN 20 THEN jsonb_set(intent,'{trusted_instrument,approval,schema}','2')
   WHEN 21 THEN jsonb_set(intent,'{kind}','"reviewed_source_carry_forward"')
   WHEN 22 THEN intent-'trusted_instrument'-'g2_volume_policy'
   WHEN 23 THEN jsonb_set(intent,'{candidate_path_base}','"unapproved-path-base"')
  END;
  IF altered IS NOT DISTINCT FROM intent THEN
   RAISE EXCEPTION 'instrument mutation % did not change its authorized fixture',mutation;
  END IF;
  rejected:=false;
  BEGIN PERFORM competition_v2_source_authorized(altered,'test','competition-test','season-test');
  EXCEPTION WHEN OTHERS THEN rejected:=true; END;
  IF NOT rejected THEN RAISE EXCEPTION 'instrument mutation % was accepted',mutation; END IF;
 END LOOP;
 PERFORM set_config('urnetwork.competition_v2_instrument_approval_json','',true);
 rejected:=false;
 BEGIN PERFORM competition_v2_source_authorized(intent,'test','competition-test','season-test');
 EXCEPTION WHEN OTHERS THEN rejected:=true; END;
 IF NOT rejected THEN RAISE EXCEPTION 'public approval digest alone authorized an arbitrary approval'; END IF;
 PERFORM set_config('urnetwork.competition_v2_instrument_approval','',true);
 -- A legacy request remains byte-identical and a later minimum-work epoch
 -- can inherit its instrument without replaying the epoch-two authorization.
 product:=jsonb_build_object('kind','reviewed_source_carry_forward','target_epoch',1);
 IF competition_v2_instrument_product_intent(product,'test','competition-test','season-test') IS DISTINCT FROM product THEN
  RAISE EXCEPTION 'legacy source was rewritten'; END IF;
 UPDATE competition_source_activation SET intent_json=intent_json||'{"g2_volume_policy":"minimum-work/1","target_epoch":2}';
 product:=product||'{"g2_volume_policy":"minimum-work/1","target_epoch":3}';
 IF competition_v2_instrument_product_intent(product,'test','competition-test','season-test') IS DISTINCT FROM product THEN
  RAISE EXCEPTION 'future source could not inherit minimum-work instrument'; END IF;
 rejected:=false;
 BEGIN PERFORM competition_v2_instrument_product_intent(product-'g2_volume_policy','test','competition-test','season-test');
 EXCEPTION WHEN OTHERS THEN rejected:=true; END;
 IF NOT rejected THEN RAISE EXCEPTION 'minimum-work instrument silently downgraded'; END IF;
END $tamper$;`)
}

// Replays only the relevant real predecessor definitions in a fresh database.
func competitionTrustedInstrumentTestMigrations() string {
	return competitionAutomationStagingSourceGraphMigration + competitionWinnerReviewMigration() +
		competitionSourceRetirementGraph + `
CREATE TEMP TABLE competition_instrument_original_function AS
 SELECT 'competition_v2_source_authorized(jsonb,text,text,text)'::regprocedure::oid AS original_oid;
` + competitionTrustedInstrumentMigration() + `
DO $same_function$
BEGIN
 IF (SELECT original_oid FROM competition_instrument_original_function) IS DISTINCT FROM
  'competition_v2_source_authorized(jsonb,text,text,text)'::regprocedure::oid THEN
  RAISE EXCEPTION 'instrument migration disconnected existing source triggers';
 END IF;
END $same_function$;
`
}

const competitionTrustedInstrumentSqlFixture = `
CREATE TABLE competition_job(job_id uuid,round_id uuid,state text,score_json jsonb);
INSERT INTO competition_job VALUES('00000000-0000-0000-0000-000000000002','00000000-0000-0000-0000-000000000001','succeeded',
 '{"significance":{"recommended_next_epoch_takeover_margin_supported":true,"recommended_next_epoch_takeover_margin_percent":10}}');
UPDATE competition_evaluation_seal SET seal_json=seal_json||jsonb_build_object('source_choice_at',timezone('UTC',now())-interval '1 minute');

CREATE FUNCTION competition_test_instrument_intent(selected_kind text) RETURNS jsonb LANGUAGE plpgsql AS $fixture$
DECLARE commits jsonb; selected jsonb; actual jsonb; previous jsonb; approval jsonb; proof jsonb; receipt jsonb; intent jsonb;
 previous_lock text; selected_lock text; actual_lock text; refs jsonb; patch text;
BEGIN
 commits:=jsonb_build_object('server',repeat('1',40),'connect',repeat('2',40),'sdk',repeat('2',40),'proxy',repeat('2',40),
  'glog',repeat('2',40),'goidenticons',repeat('2',40),'userwireguard',repeat('2',40),'sn',repeat('2',40),'warp',repeat('2',40),'gvisor',repeat('2',40));
 previous_lock:=competition_v2_instrument_lock_digest(commits);
 previous:=jsonb_build_object('commits',commits,'source_lock_sha256',previous_lock,'target_epoch',1,'source_generation',0,
  'threshold',0.2,'threshold_evidence_sha256',repeat('a',64),'evaluator_image_digest','sha256:'||repeat('b',64));
 IF NOT EXISTS(SELECT 1 FROM competition_source_pointer) THEN
  INSERT INTO competition_source_pointer VALUES('test','competition-test','season-test',1);
  INSERT INTO competition_source_activation VALUES('test','competition-test','season-test',1,previous,timezone('UTC',now()),NULL);
 END IF;
 selected:=CASE WHEN selected_kind='reviewed_source_promotion' THEN jsonb_set(commits,'{server}',to_jsonb(repeat('3',40))) ELSE commits END;
 selected_lock:=competition_v2_instrument_lock_digest(selected);
 actual:=jsonb_set(selected,'{server}',to_jsonb(repeat('4',40)));
 actual_lock:=competition_v2_instrument_lock_digest(actual);
 patch:=encode(convert_to('synthetic protected instrument patch','UTF8'),'base64');
 approval:=jsonb_build_object('schema',1,'approval_id','synthetic-instrument','approval_sha256',repeat('c',64),'target_epoch',2,
  'g2_volume_policy','minimum-work/1','instrument_base_commits',commits,'instrument_base_source_lock_sha256',previous_lock,
  'instrument_base_image_digest','sha256:'||repeat('b',64),'patch_sha256',encode(sha256(decode(patch,'base64')),'hex'),
  'patch',patch,'protected_paths',jsonb_build_array('connect/sim-latency/score.go'));
 PERFORM set_config('urnetwork.competition_v2_instrument_approval',approval->>'approval_sha256',true);
 PERFORM set_config('urnetwork.competition_v2_instrument_approval_json',approval::text,true);
 receipt:=jsonb_build_object('schema',2,'operation','trusted_instrument_build','g2_volume_policy','minimum-work/1','review_id','00000000-0000-0000-0000-000000000000',
  'instrument_approval_sha256',approval->>'approval_sha256','patch_sha256',approval->>'patch_sha256','source_lock_sha256',actual_lock,
  'evaluator_image_digest','sha256:'||repeat('d',64),'host_qualification_sha256',repeat('e',64),'simulator_sha256',repeat('f',64),
  'offline',true,'no_main_credentials',true,'read_only_source',true,'passed',true,'containment_sha256',repeat('e',64),'test_evidence_sha256',repeat('e',64));
 proof:=jsonb_build_object('schema',1,'approval',approval,'selected_product_kind',selected_kind,'selected_product_commits',selected,
  'selected_product_source_lock_sha256',selected_lock,'instrumented_server_commit',actual->>'server','build_receipt',receipt);
 SELECT jsonb_object_agg(c.key,jsonb_build_object('branch','sim-latency','commit',c.value,'expected_commit',commits->>c.key)) INTO refs FROM jsonb_each_text(actual) c;
 refs:=refs||jsonb_build_object('config',jsonb_build_object('branch','main','commit',repeat('c',40),'expected_commit',repeat('d',40)));
 intent:=jsonb_build_object('environment','test','competition_id','competition-test','season_id','season-test','generation',2,'expected_generation',1,
  'target_epoch',2,'source_generation',1,'kind',CASE WHEN selected_kind='reviewed_source_promotion' THEN selected_kind ELSE 'trusted_instrument_transition' END,
  'reason',CASE WHEN selected_kind='reviewed_source_promotion' THEN 'approved' ELSE 'no_reward_winner' END,'commits',actual,'source_lock_sha256',actual_lock,
  'base_source_lock_sha256',previous_lock,'manifest',encode(convert_to('{}','UTF8'),'base64'),'manifest_sha256',encode(sha256(convert_to('{}','UTF8')),'hex'),
  'manifest_commit',repeat('c',40),'config_expected_commit',repeat('d',40),'git_refs',refs,'threshold',0.2,'threshold_evidence_sha256',repeat('a',64),
  'source_decision_digest',repeat('a',64),'publication_journal_sha256',repeat('a',64),'evaluator_image_digest','sha256:'||repeat('d',64),
  'host_qualification_sha256',repeat('e',64),'simulator_sha256',repeat('f',64),'scorer_sha256',repeat('f',64),'g2_volume_policy','minimum-work/1',
  'prepared_at',timezone('UTC',now()),'build_ready_at',timezone('UTC',now()),'winner_review_policy','elected_winner_sol61max_v1','reward_decision_digest',repeat('a',64));
 IF selected_kind='reviewed_source_promotion' THEN
  proof:=proof||jsonb_build_object('selected_product_review_id','00000000-0000-0000-0000-000000000003',
   'selected_product_review_digest',repeat('b',64),'selected_product_patch_sha256',repeat('f',64));
  intent:=intent||jsonb_build_object('review_id','00000000-0000-0000-0000-000000000003','review_decision_digest',repeat('b',64),'approved_patch_sha256',repeat('f',64),
   'evaluated_image_digest','sha256:'||repeat('b',64),'candidate_path_base','mono_root_v1',
   'winner_review',(SELECT transition_json->'winner_review' FROM competition_supervisor_transition LIMIT 1));
 END IF;
 UPDATE competition_integrity_review SET review_json=jsonb_set(review_json,'{request}',review_json->'request'||jsonb_build_object(
  'base_source_lock_sha256',previous_lock,'patch_sha256',repeat('f',64),'score_sha256',repeat('a',64)));
 UPDATE competition_round SET policy_json=policy_json||jsonb_build_object('evaluator_image_digest','sha256:'||repeat('b',64),'patch_policy',jsonb_build_object('path_base','mono_root_v1'));
 RETURN intent||jsonb_build_object('trusted_instrument',proof);
END $fixture$;
`
