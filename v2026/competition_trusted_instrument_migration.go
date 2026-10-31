// Prospective instrument authority is independent of product winner authority.
// Historical source records, migrations, review guards and trigger oids remain unchanged.
package server

import "strings"

// The existing function keeps its oid and complete legacy authorization body.
// Only a configured, authenticated overlay can project a different instrument
// back to its genuinely selected product for the existing review checks.
func competitionTrustedInstrumentMigration() string {
	previous := competitionRepositoryPatchMigration()
	anchor := "BEGIN\n PERFORM competition_v2_source_mode(intent);"
	if strings.Count(previous, anchor) != 1 {
		panic("trusted instrument source authorization anchor is ambiguous")
	}
	source := strings.Replace(previous, anchor,
		"BEGIN\n intent:=competition_v2_instrument_product_intent(intent,env,competition,season);\n PERFORM competition_v2_source_mode(intent);", 1)
	reviewAnchor := "  IF evaluated_round.staging IS DISTINCT FROM"
	if strings.Count(source, reviewAnchor) != 1 {
		panic("trusted instrument evaluated path-base anchor is ambiguous")
	}
	return competitionTrustedInstrumentSql + strings.Replace(source, reviewAnchor,
		competitionTrustedInstrumentPathBaseSql+reviewAnchor, 1)
}

const competitionTrustedInstrumentPathBaseSql = `  IF COALESCE(intent->>'candidate_path_base','')<>''
   AND intent->>'candidate_path_base' IS DISTINCT FROM COALESCE(evaluated_round.policy_json->'patch_policy'->>'path_base','') THEN
   RAISE EXCEPTION 'source publication path base differs from the genuinely evaluated patch policy';
  END IF;
`

const competitionTrustedInstrumentSql = `
SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION competition_v2_instrument_lock_digest(commits jsonb)
RETURNS text LANGUAGE sql IMMUTABLE AS $instrument_lock$
 SELECT encode(sha256(convert_to('{"schema":1,"development_snapshot":false,"repositories":{'||
  string_agg(to_json(c.key)::text||':'||to_json(c.value)::text,',' ORDER BY c.key)||'}}','UTF8')),'hex')
 FROM jsonb_each_text(commits) c;
$instrument_lock$;

CREATE OR REPLACE FUNCTION competition_v2_instrument_product_intent(intent jsonb, env text, competition text, season text)
RETURNS jsonb LANGUAGE plpgsql AS $instrument$
DECLARE previous jsonb; previous_generation bigint; proof jsonb; approval jsonb; configured jsonb;
 selected jsonb; receipt jsonb; projected jsonb; policy text; prior_policy text; selected_kind text;
BEGIN
 SELECT p.generation,a.intent_json INTO previous_generation,previous FROM competition_source_pointer p
  JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
  WHERE p.environment=env AND p.competition_id=competition AND p.season_id=season AND a.activated_at IS NOT NULL;
 policy:=COALESCE(intent->>'g2_volume_policy','');
 prior_policy:=COALESCE(previous->>'g2_volume_policy','');
 configured:=NULLIF(current_setting('urnetwork.competition_v2_instrument_approval_json',true),'')::jsonb;
 IF (configured IS NULL) IS DISTINCT FROM (COALESCE(current_setting('urnetwork.competition_v2_instrument_approval',true),'')='')
  OR (configured IS NOT NULL AND (jsonb_typeof(configured) IS DISTINCT FROM 'object'
   OR configured->'schema' IS DISTINCT FROM '1'::jsonb OR configured->'target_epoch' IS DISTINCT FROM '2'::jsonb
   OR configured->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
   OR COALESCE(configured->>'approval_sha256','') !~ '^[0-9a-f]{64}$'
   OR configured->>'approval_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_v2_instrument_approval',true))) THEN
  RAISE EXCEPTION 'trusted instrument configuration must bind its complete validated approval';
 END IF;
 IF policy NOT IN ('','minimum-work/1') OR prior_policy NOT IN ('','minimum-work/1') THEN
  RAISE EXCEPTION 'unsupported trusted instrument volume policy';
 END IF;
 proof:=NULLIF(intent->'trusted_instrument','null'::jsonb);
 IF proof IS NULL THEN
  IF intent->>'kind'='trusted_instrument_transition' OR policy IS DISTINCT FROM prior_policy
   OR (configured IS NOT NULL AND (intent->>'target_epoch')::integer>=(configured->>'target_epoch')::integer
    AND policy IS DISTINCT FROM configured->>'g2_volume_policy') THEN
   RAISE EXCEPTION 'changed instrument policy requires independent operator proof';
  END IF;
  RETURN intent;
 END IF;

 approval:=proof->'approval';
 IF previous IS NULL OR COALESCE((intent->>'staging')::boolean,false)
  OR (intent->>'target_epoch')::integer IS DISTINCT FROM 2 OR (previous->>'target_epoch')::integer IS DISTINCT FROM 1
  OR prior_policy<>'' OR policy<>'minimum-work/1'
  OR proof->'schema' IS DISTINCT FROM '1'::jsonb OR approval->'schema' IS DISTINCT FROM '1'::jsonb
  OR jsonb_typeof(approval) IS DISTINCT FROM 'object' OR approval IS DISTINCT FROM configured
  OR COALESCE(approval->>'approval_sha256','') !~ '^[0-9a-f]{64}$'
  OR approval->>'approval_sha256' IS DISTINCT FROM current_setting('urnetwork.competition_v2_instrument_approval',true)
  OR approval->'target_epoch' IS DISTINCT FROM intent->'target_epoch'
  OR approval->>'g2_volume_policy' IS DISTINCT FROM policy
  OR approval->'instrument_base_commits' IS DISTINCT FROM previous->'commits'
  OR approval->>'instrument_base_source_lock_sha256' IS DISTINCT FROM previous->>'source_lock_sha256'
  OR approval->>'instrument_base_image_digest' IS DISTINCT FROM previous->>'evaluator_image_digest'
  OR jsonb_typeof(approval->'protected_paths') IS DISTINCT FROM 'array'
  OR jsonb_array_length(approval->'protected_paths')=0
  OR COALESCE(approval->>'patch','')='' OR COALESCE(approval->>'patch_sha256','') !~ '^[0-9a-f]{64}$'
  OR approval->>'patch_sha256' IS DISTINCT FROM encode(sha256(decode(approval->>'patch','base64')),'hex') THEN
  RAISE EXCEPTION 'trusted instrument is not the configured prospective operator approval';
 END IF;

 selected:=proof->'selected_product_commits';
 selected_kind:=proof->>'selected_product_kind';
 IF NOT competition_v2_source_graph_valid(selected,false) OR NOT competition_v2_source_graph_valid(intent->'commits',false)
  OR proof->>'selected_product_source_lock_sha256' IS DISTINCT FROM competition_v2_instrument_lock_digest(selected)
  OR intent->>'source_lock_sha256' IS DISTINCT FROM competition_v2_instrument_lock_digest(intent->'commits')
  OR ((intent->'commits')-'server') IS DISTINCT FROM (selected-'server')
  OR COALESCE(proof->>'instrumented_server_commit','') !~ '^[0-9a-f]{40}$'
  OR proof->>'instrumented_server_commit' IS DISTINCT FROM intent->'commits'->>'server'
  OR intent->'commits'->>'server' IS NOT DISTINCT FROM selected->>'server'
  OR intent->'git_refs'->'server'->>'commit' IS DISTINCT FROM intent->'commits'->>'server'
  OR (intent->>'source_generation')::integer IS DISTINCT FROM (previous->>'source_generation')::integer+1
  OR COALESCE(intent->>'simulator_sha256','') !~ '^[0-9a-f]{64}$'
  OR intent->>'scorer_sha256' IS DISTINCT FROM intent->>'simulator_sha256' THEN
  RAISE EXCEPTION 'trusted instrument lost its exact selected graph or compiled child';
 END IF;

 receipt:=proof->'build_receipt';
 IF receipt->'schema' IS DISTINCT FROM '2'::jsonb OR receipt->>'operation' IS DISTINCT FROM 'trusted_instrument_build'
  OR receipt->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
  OR receipt->>'review_id' IS DISTINCT FROM '00000000-0000-0000-0000-000000000000'
  OR receipt->>'instrument_approval_sha256' IS DISTINCT FROM approval->>'approval_sha256'
  OR receipt->>'patch_sha256' IS DISTINCT FROM approval->>'patch_sha256'
  OR receipt->>'source_lock_sha256' IS DISTINCT FROM intent->>'source_lock_sha256'
  OR receipt->>'evaluator_image_digest' IS DISTINCT FROM intent->>'evaluator_image_digest'
  OR receipt->>'host_qualification_sha256' IS DISTINCT FROM intent->>'host_qualification_sha256'
  OR receipt->>'simulator_sha256' IS DISTINCT FROM intent->>'simulator_sha256'
  OR receipt->'offline' IS DISTINCT FROM 'true'::jsonb OR receipt->'no_main_credentials' IS DISTINCT FROM 'true'::jsonb
  OR receipt->'read_only_source' IS DISTINCT FROM 'true'::jsonb OR receipt->'passed' IS DISTINCT FROM 'true'::jsonb
  OR COALESCE(receipt->>'containment_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(receipt->>'test_evidence_sha256','') !~ '^[0-9a-f]{64}$' THEN
  RAISE EXCEPTION 'trusted instrument lacks its actual qualified isolated build';
 END IF;

 IF selected_kind='reviewed_source_carry_forward' THEN
  IF intent->>'kind' IS DISTINCT FROM 'trusted_instrument_transition' OR selected IS DISTINCT FROM previous->'commits'
   OR intent->>'review_id' IS NOT NULL OR proof->>'selected_product_review_id' IS NOT NULL
   OR COALESCE(intent->>'review_decision_digest','')<>'' OR COALESCE(intent->>'approved_patch_sha256','')<>''
   OR COALESCE(proof->>'selected_product_review_digest','')<>'' OR COALESCE(proof->>'selected_product_patch_sha256','')<>'' THEN
   RAISE EXCEPTION 'trusted instrument carry cannot invent product review or change the incumbent';
  END IF;
 ELSIF selected_kind='reviewed_source_promotion' THEN
  IF intent->>'kind' IS DISTINCT FROM 'reviewed_source_promotion' OR intent->>'review_id' IS NULL
   OR proof->>'selected_product_review_id' IS DISTINCT FROM intent->>'review_id'
   OR proof->>'selected_product_review_digest' IS DISTINCT FROM intent->>'review_decision_digest'
   OR proof->>'selected_product_patch_sha256' IS DISTINCT FROM intent->>'approved_patch_sha256' THEN
   RAISE EXCEPTION 'trusted instrument cannot substitute genuine product winner approval';
  END IF;
 ELSE RAISE EXCEPTION 'trusted instrument has no authenticated product selection'; END IF;

 projected:=jsonb_set(jsonb_set(jsonb_set(jsonb_set(intent-'trusted_instrument','{kind}',to_jsonb(selected_kind)),
  '{commits}',selected),'{source_lock_sha256}',proof->'selected_product_source_lock_sha256'),
  '{git_refs,server,commit}',selected->'server');
 IF selected_kind='reviewed_source_carry_forward' THEN
  projected:=jsonb_set(projected,'{source_generation}',previous->'source_generation');
 END IF;
 RETURN projected;
END $instrument$;
`
