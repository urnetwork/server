// A new ledger is additive: historical review bytes and frozen policies stay
// unchanged. A timely pending observation is not an approval or clock renewal.
package server

import "strings"

func competitionReviewHandoffMigration() string {
	extract := func(sql, start, end string) string {
		begin := strings.Index(sql, start)
		if begin < 0 {
			panic("review handoff authority anchor absent")
		}
		length := strings.Index(sql[begin:], end)
		if length < 0 {
			panic("review handoff authority end absent")
		}
		return strings.Replace(sql[begin:begin+length+len(end)], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
	}
	replace := func(sql, old, next string) string {
		if strings.Count(sql, old) != 1 {
			panic("review handoff clock anchor ambiguous")
		}
		return strings.Replace(sql, old, next, 1)
	}
	current := competitionSnowTwoMemberWinnerReviewMigration()
	source := extract(current, "CREATE OR REPLACE FUNCTION competition_v2_winner_source_authorized", "END $winner_source$;")
	source = replace(source, "review.created_at>=(checkpoint->>'started_at')::timestamp AND review.created_at<(checkpoint->>'deadline')::timestamp",
		"competition_review_handoff_observed(review.review_json,review.created_at)>=(checkpoint->>'started_at')::timestamp AND competition_review_handoff_observed(review.review_json,review.created_at)<(checkpoint->>'deadline')::timestamp")
	source = replace(source, "checkpoint IS NULL OR checkpoint IS DISTINCT FROM actual_checkpoint", "checkpoint IS NULL OR COALESCE(checkpoint->>'handoff_id','') IS DISTINCT FROM COALESCE(review.review_json->'request'->>'handoff_id','') OR checkpoint IS DISTINCT FROM actual_checkpoint")
	review := extract(current, "CREATE OR REPLACE FUNCTION competition_v2_winner_review_guard", "END $winner_review$;")
	review = replace(review, "NEW.created_at>=(checkpoint->>'started_at')::timestamp AND NEW.created_at<(checkpoint->>'deadline')::timestamp",
		"competition_review_handoff_observed(NEW.review_json,NEW.created_at)>=(checkpoint->>'started_at')::timestamp AND competition_review_handoff_observed(NEW.review_json,NEW.created_at)<(checkpoint->>'deadline')::timestamp")
	review = replace(review, "OR checkpoint IS NULL OR NOT COALESCE", "OR COALESCE(checkpoint->>'handoff_id','') IS DISTINCT FROM COALESCE(request->>'handoff_id','') OR checkpoint IS NULL OR NOT COALESCE")
	checkpoint := extract(competitionWinnerReviewGuards, "CREATE FUNCTION competition_v2_winner_checkpoint_guard", "END $winner_checkpoint$;")
	checkpoint = replace(checkpoint, "(checkpoint->>'deadline')::timestamp=(checkpoint->>'started_at')::timestamp+make_interval(secs=>(policy->>'winner_review_timeout_seconds')::integer)",
		`(CASE WHEN COALESCE(checkpoint->>'handoff_id','')='' THEN
    (checkpoint->>'deadline')::timestamp=(checkpoint->>'started_at')::timestamp+make_interval(secs=>(policy->>'winner_review_timeout_seconds')::integer)
   ELSE EXISTS(SELECT 1 FROM competition_review_handoff h WHERE h.handoff_id::text=checkpoint->>'handoff_id'
    AND policy->>'winner_review_policy'='elected_winner_snow_union_v2' AND h.round_id=predecessor_id
    AND h.job_id::text=checkpoint->>'job_id' AND h.decision_sha256=checkpoint->>'reward_decision_sha256'
    AND h.started_at=(checkpoint->>'started_at')::timestamp AND h.deadline=(checkpoint->>'deadline')::timestamp) END)`)
	return competitionReviewHandoffLedgerSql + source + "\n" + review + "\n" + checkpoint
}

const competitionReviewHandoffLedgerSql = `
SET LOCAL lock_timeout = '5s';
CREATE FUNCTION competition_review_handoff_clock() RETURNS timestamp LANGUAGE sql VOLATILE AS $handoff_clock$
 SELECT timezone('UTC',clock_timestamp());
$handoff_clock$;
CREATE TABLE competition_review_handoff (
 handoff_id uuid PRIMARY KEY,
 round_id uuid NOT NULL REFERENCES competition_round(round_id),
 job_id uuid NOT NULL,
 decision_sha256 text NOT NULL CHECK(decision_sha256 ~ '^[0-9a-f]{64}$'),
 owner_json jsonb NOT NULL CHECK(octet_length(owner_json::text)<=1048576),
 started_at timestamp NOT NULL,
 deadline timestamp NOT NULL CHECK(deadline>started_at AND deadline<=started_at+interval '24 hours'),
 UNIQUE(round_id,decision_sha256)
);
CREATE TABLE competition_review_handoff_call (
 event_cursor bigserial PRIMARY KEY,
 handoff_id uuid NOT NULL REFERENCES competition_review_handoff(handoff_id),
 call_id text NOT NULL CHECK(call_id ~ '^[a-z0-9][a-z0-9._-]{0,127}$'),
 kind text NOT NULL CHECK(kind IN ('claim','receipt')),
 call_json jsonb NOT NULL CHECK(octet_length(call_json::text)<=8192),
 recorded_at timestamp NOT NULL,
 UNIQUE(handoff_id,call_id,kind)
);
CREATE TABLE competition_review_handoff_completion (
 handoff_id uuid PRIMARY KEY REFERENCES competition_review_handoff(handoff_id),
 completion_json jsonb NOT NULL CHECK(octet_length(completion_json::text)<=131072),
 completed_at timestamp NOT NULL
);
CREATE TRIGGER competition_review_handoff_immutable BEFORE UPDATE OR DELETE ON competition_review_handoff FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_review_handoff_call_immutable BEFORE UPDATE OR DELETE ON competition_review_handoff_call FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();
CREATE TRIGGER competition_review_handoff_completion_immutable BEFORE UPDATE OR DELETE ON competition_review_handoff_completion FOR EACH ROW EXECUTE FUNCTION competition_append_only_guard();

CREATE FUNCTION competition_review_handoff_owner_guard() RETURNS trigger LANGUAGE plpgsql AS $handoff_owner$
DECLARE args jsonb; policy jsonb; seal jsonb; entry jsonb; decision jsonb; epoch integer; call_id text;
BEGIN
 args:=NEW.owner_json->'args';
 SELECT policy_json->'automation_policy',epoch_number INTO policy,epoch FROM competition_round WHERE round_id=NEW.round_id;
 PERFORM competition_v2_scope_lock(policy->>'environment',(SELECT competition_id FROM competition_round WHERE round_id=NEW.round_id),policy->>'season_id');
 PERFORM 1 FROM competition_round WHERE round_id=NEW.round_id FOR UPDATE;
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=NEW.round_id ORDER BY revision DESC LIMIT 1;
 SELECT seal_json INTO seal FROM competition_evaluation_seal WHERE round_id=NEW.round_id;
 SELECT value INTO entry FROM jsonb_array_elements(seal->'entries') WHERE value->>'job_id'=NEW.job_id::text;
 IF policy->>'winner_review_policy' IS DISTINCT FROM 'elected_winner_snow_union_v2'
  OR NEW.owner_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_review_handoff_actor',true)
  OR COALESCE(NEW.owner_json->>'actor_id','')='' OR NEW.owner_json->>'handoff_id' IS DISTINCT FROM NEW.handoff_id::text
  OR args->>'protocol' IS DISTINCT FROM 'elected-winner-qg-multipass/1'
  OR NOT COALESCE((args->>'timeout_seconds')::integer BETWEEN 60 AND 86400,false)
  OR NEW.started_at IS DISTINCT FROM NULLIF(current_setting('urnetwork.competition_review_handoff_clock',true),'')::timestamp
  OR competition_review_handoff_clock()>=NEW.deadline
  OR NEW.deadline IS DISTINCT FROM NEW.started_at+make_interval(secs=>(args->>'timeout_seconds')::integer)
  OR (NEW.owner_json->>'started_at')::timestamp IS DISTINCT FROM NEW.started_at OR (NEW.owner_json->>'deadline')::timestamp IS DISTINCT FROM NEW.deadline
  OR args->>'round_id' IS DISTINCT FROM NEW.round_id::text OR args->>'job_id' IS DISTINCT FROM NEW.job_id::text
  OR decision->>'selected_job_id' IS DISTINCT FROM NEW.job_id::text OR args->>'reward_revision' IS DISTINCT FROM decision->>'revision'
  OR args->>'decision_sha256' IS DISTINCT FROM NEW.decision_sha256 OR decision->>'decision_sha256' IS DISTINCT FROM NEW.decision_sha256
  OR entry->>'state' IS DISTINCT FROM 'succeeded' OR entry->>'eligible' IS DISTINCT FROM 'true'
  OR args->>'seal_sha256' IS DISTINCT FROM seal->>'seal_sha256' OR args->>'baseline_sha256' IS DISTINCT FROM seal->>'baseline_sha256'
  OR args->>'patch_sha256' IS DISTINCT FROM entry->>'patch_sha256' OR args->>'base_source_lock_sha256' IS DISTINCT FROM entry->>'base_source_lock_sha256'
  OR args->>'score_sha256' IS DISTINCT FROM entry->>'score_sha256' OR args->>'artifact_manifest_sha256' IS DISTINCT FROM entry->>'artifact_manifest_sha256'
  OR NOT COALESCE((seal->>'publication_at')::timestamp<=NEW.started_at,false)
  OR COALESCE(args->>'authorization_sha256','') !~ '^[0-9a-f]{64}$' OR COALESCE(args->>'provider_sha256','') !~ '^[0-9a-f]{64}$'
  OR COALESCE(NEW.owner_json->'plan'->>'plan_sha256','') !~ '^[0-9a-f]{64}$' OR COALESCE(NEW.owner_json->'plan'->>'bundle_sha256','') !~ '^[0-9a-f]{64}$'
  OR jsonb_typeof(NEW.owner_json->'plan'->'call_ids') IS DISTINCT FROM 'array'
  OR jsonb_array_length(NEW.owner_json->'plan'->'call_ids') NOT BETWEEN 2 AND 384
  OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=NEW.round_id AND invalidates_evaluation)
  OR EXISTS(SELECT 1 FROM competition_integrity_review WHERE round_id=NEW.round_id AND job_id=NEW.job_id)
  OR EXISTS(SELECT 1 FROM competition_supervisor_transition WHERE environment=policy->>'environment' AND competition_id=(SELECT competition_id FROM competition_round WHERE round_id=NEW.round_id) AND season_id=policy->>'season_id' AND target_epoch=epoch+1 AND COALESCE(transition_json->'winner_review','null'::jsonb)<>'null'::jsonb) THEN
  RAISE EXCEPTION 'handoff must own a fresh published elected decision with an exact prospective database clock';
 END IF;
 IF (SELECT count(DISTINCT value) FROM jsonb_array_elements_text(NEW.owner_json->'plan'->'call_ids'))<>jsonb_array_length(NEW.owner_json->'plan'->'call_ids') THEN RAISE EXCEPTION 'duplicate planned review calls'; END IF;
 FOR call_id IN SELECT value FROM jsonb_array_elements_text(NEW.owner_json->'plan'->'call_ids') LOOP
  IF call_id !~ '^[a-z0-9][a-z0-9._-]{0,127}$' THEN RAISE EXCEPTION 'invalid planned review call'; END IF;
 END LOOP;
 RETURN NEW;
END $handoff_owner$;
CREATE TRIGGER competition_review_handoff_before_insert BEFORE INSERT ON competition_review_handoff FOR EACH ROW EXECUTE FUNCTION competition_review_handoff_owner_guard();

CREATE FUNCTION competition_review_handoff_call_guard() RETURNS trigger LANGUAGE plpgsql AS $handoff_call$
DECLARE owner competition_review_handoff%ROWTYPE; claim jsonb; decision jsonb; policy jsonb; claims integer; receipts integer;
BEGIN
 SELECT * INTO owner FROM competition_review_handoff WHERE handoff_id=NEW.handoff_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'review call owner is absent'; END IF;
 SELECT policy_json INTO policy FROM competition_round WHERE round_id=owner.round_id;
 PERFORM competition_v2_scope_lock(policy->'automation_policy'->>'environment',policy->>'competition_id',policy->'automation_policy'->>'season_id');
 PERFORM 1 FROM competition_round WHERE round_id=owner.round_id FOR UPDATE;
 SELECT * INTO owner FROM competition_review_handoff WHERE handoff_id=NEW.handoff_id FOR UPDATE;
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=owner.round_id ORDER BY revision DESC LIMIT 1;
 IF NOT FOUND OR owner.owner_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_review_handoff_actor',true)
  OR NEW.recorded_at IS DISTINCT FROM NULLIF(current_setting('urnetwork.competition_review_handoff_clock',true),'')::timestamp
  OR NEW.call_json->>'handoff_id' IS DISTINCT FROM NEW.handoff_id::text OR NEW.call_json->>'call_id' IS DISTINCT FROM NEW.call_id
  OR NEW.call_json->>'kind' IS DISTINCT FROM NEW.kind OR (NEW.call_json->>'recorded_at')::timestamp IS DISTINCT FROM NEW.recorded_at
  OR NOT COALESCE((owner.owner_json->'plan'->'call_ids') ? NEW.call_id,false)
  OR COALESCE(NEW.call_json->>'request_sha256','') !~ '^[0-9a-f]{64}$' THEN RAISE EXCEPTION 'review call lacks immutable native ownership'; END IF;
 IF NEW.kind='claim' THEN
  SELECT count(*) FILTER (WHERE kind='claim'),count(*) FILTER (WHERE kind='receipt') INTO claims,receipts FROM competition_review_handoff_call WHERE handoff_id=NEW.handoff_id;
  IF NEW.recorded_at>=owner.deadline OR competition_review_handoff_clock()>=owner.deadline
   OR claims<>receipts OR owner.owner_json->'plan'->'call_ids'->>claims IS DISTINCT FROM NEW.call_id
   OR EXISTS(SELECT 1 FROM competition_review_handoff_call WHERE handoff_id=NEW.handoff_id AND kind='receipt' AND COALESCE(call_json->>'failure','')<>'')
   OR decision->>'decision_sha256' IS DISTINCT FROM owner.decision_sha256
   OR decision->>'selected_job_id' IS DISTINCT FROM owner.job_id::text
   OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=owner.round_id AND invalidates_evaluation)
   OR EXISTS(SELECT 1 FROM competition_review_handoff_completion WHERE handoff_id=NEW.handoff_id)
   OR COALESCE(NEW.call_json->>'evidence_sha256','')<>'' OR COALESCE(NEW.call_json->>'failure','')<>'' THEN RAISE EXCEPTION 'expired, completed or changed review cannot claim a call'; END IF;
 ELSE
  SELECT call_json INTO claim FROM competition_review_handoff_call WHERE handoff_id=NEW.handoff_id AND call_id=NEW.call_id AND kind='claim';
  IF claim IS NULL OR claim->>'request_sha256' IS DISTINCT FROM NEW.call_json->>'request_sha256'
   OR COALESCE(NEW.call_json->>'evidence_sha256','') !~ '^[0-9a-f]{64}$' THEN RAISE EXCEPTION 'review receipt lacks its original claim'; END IF;
 END IF;
 RETURN NEW;
END $handoff_call$;
CREATE TRIGGER competition_review_handoff_call_before_insert BEFORE INSERT ON competition_review_handoff_call FOR EACH ROW EXECUTE FUNCTION competition_review_handoff_call_guard();

CREATE FUNCTION competition_review_handoff_completion_guard() RETURNS trigger LANGUAGE plpgsql AS $handoff_complete$
DECLARE owner competition_review_handoff%ROWTYPE; proposal jsonb; args jsonb; evidence jsonb; decision jsonb; policy jsonb;
BEGIN
 SELECT * INTO owner FROM competition_review_handoff WHERE handoff_id=NEW.handoff_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'review completion owner is absent'; END IF;
 SELECT policy_json INTO policy FROM competition_round WHERE round_id=owner.round_id;
 PERFORM competition_v2_scope_lock(policy->'automation_policy'->>'environment',policy->>'competition_id',policy->'automation_policy'->>'season_id');
 PERFORM 1 FROM competition_round WHERE round_id=owner.round_id FOR UPDATE;
 SELECT * INTO owner FROM competition_review_handoff WHERE handoff_id=NEW.handoff_id FOR UPDATE;
 args:=owner.owner_json->'args'; proposal:=NULLIF(NEW.completion_json->'proposal','null'::jsonb); evidence:=NEW.completion_json->'evidence';
 IF NOT FOUND OR owner.owner_json->>'actor_id' IS DISTINCT FROM current_setting('urnetwork.competition_review_handoff_actor',true)
  OR NEW.completed_at IS DISTINCT FROM NULLIF(current_setting('urnetwork.competition_review_handoff_clock',true),'')::timestamp
  OR NEW.completion_json->>'handoff_id' IS DISTINCT FROM NEW.handoff_id::text
  OR (NEW.completion_json->>'completed_at')::timestamp IS DISTINCT FROM NEW.completed_at
  OR NEW.completion_json->>'plan_sha256' IS DISTINCT FROM owner.owner_json->'plan'->>'plan_sha256'
  OR COALESCE(evidence->>'key','')='' OR COALESCE(evidence->>'version_id','')=''
  OR COALESCE(evidence->>'sha256','') !~ '^[0-9a-f]{64}$' OR NOT COALESCE((evidence->>'retain_until')::timestamp>NEW.completed_at,false) THEN RAISE EXCEPTION 'review completion lacks native retained identity'; END IF;
 IF proposal IS NULL THEN
  IF COALESCE(NEW.completion_json->>'failure','')='' OR COALESCE(NEW.completion_json->>'proposal_sha256','')<>'' THEN RAISE EXCEPTION 'incomplete review must retain failure without authority'; END IF;
  RETURN NEW;
 END IF;
 SELECT decision_json INTO decision FROM competition_reward_decision WHERE round_id=owner.round_id ORDER BY revision DESC LIMIT 1;
 IF NEW.completed_at>=owner.deadline OR competition_review_handoff_clock()>=owner.deadline OR COALESCE(NEW.completion_json->>'failure','')<>''
  OR decision->>'decision_sha256' IS DISTINCT FROM owner.decision_sha256 OR decision->>'selected_job_id' IS DISTINCT FROM owner.job_id::text
  OR EXISTS(SELECT 1 FROM competition_automation_incident WHERE round_id=owner.round_id AND invalidates_evaluation)
  OR proposal->>'handoff_id' IS DISTINCT FROM NEW.handoff_id::text OR proposal->>'job_id' IS DISTINCT FROM owner.job_id::text
  OR proposal->>'expected_revision' IS DISTINCT FROM args->>'reward_revision' OR proposal->>'expected_decision_sha256' IS DISTINCT FROM owner.decision_sha256
  OR proposal->>'patch_sha256' IS DISTINCT FROM args->>'patch_sha256' OR proposal->>'base_source_lock_sha256' IS DISTINCT FROM args->>'base_source_lock_sha256'
  OR proposal->>'score_sha256' IS DISTINCT FROM args->>'score_sha256' OR proposal->>'artifact_manifest_sha256' IS DISTINCT FROM args->>'artifact_manifest_sha256'
  OR proposal->>'baseline_sha256' IS DISTINCT FROM args->>'baseline_sha256' OR proposal->>'private_evidence_sha256' IS DISTINCT FROM evidence->>'sha256'
  OR NOT COALESCE(proposal->>'reward_disposition' IN ('approved','disqualified'),false)
  OR NOT COALESCE(competition_v2_winner_review_provider('elected_winner_snow_union_v2',proposal->>'reviewer_model',proposal->>'reviewer_reasoning'),false)
  OR NOT COALESCE(competition_v2_snow_review_findings_valid_v2(proposal),false)
  OR COALESCE(NEW.completion_json->>'proposal_sha256','') !~ '^[0-9a-f]{64}$'
  OR (SELECT count(*) FROM competition_review_handoff_call WHERE handoff_id=NEW.handoff_id AND kind='receipt')<>jsonb_array_length(owner.owner_json->'plan'->'call_ids')
  OR EXISTS(SELECT 1 FROM competition_review_handoff_call WHERE handoff_id=NEW.handoff_id AND kind='receipt' AND (recorded_at>=owner.deadline OR COALESCE(call_json->>'failure','')<>'')) THEN RAISE EXCEPTION 'only complete timely exact Qwen/Gemma evidence can form a pending proposal'; END IF;
 RETURN NEW;
END $handoff_complete$;
CREATE TRIGGER competition_review_handoff_completion_before_insert BEFORE INSERT ON competition_review_handoff_completion FOR EACH ROW EXECUTE FUNCTION competition_review_handoff_completion_guard();

CREATE FUNCTION competition_review_handoff_observed(review_json jsonb, legacy_created_at timestamp)
RETURNS timestamp LANGUAGE plpgsql STABLE AS $handoff_observed$
DECLARE owner competition_review_handoff%ROWTYPE; completion competition_review_handoff_completion%ROWTYPE; request jsonb;
BEGIN
 request:=review_json->'request';
 IF COALESCE(request->>'handoff_id','')='' THEN
  IF review_json ? 'model_completed_at' THEN RETURN NULL; END IF;
  RETURN legacy_created_at;
 END IF;
 SELECT * INTO owner FROM competition_review_handoff WHERE handoff_id::text=request->>'handoff_id';
 IF NOT FOUND THEN RETURN NULL; END IF;
 SELECT * INTO completion FROM competition_review_handoff_completion WHERE handoff_id=owner.handoff_id;
 IF NOT FOUND OR completion.completion_json->'proposal' IS DISTINCT FROM request
  OR completion.completion_json->>'proposal_sha256' IS DISTINCT FROM review_json->>'request_sha256'
  OR COALESCE(completion.completion_json->>'failure','')<>''
  OR review_json->>'round_id' IS DISTINCT FROM owner.round_id::text
  OR (review_json->>'model_completed_at')::timestamp IS DISTINCT FROM completion.completed_at
  OR completion.completed_at<owner.started_at OR completion.completed_at>=owner.deadline THEN RETURN NULL; END IF;
 RETURN completion.completed_at;
END $handoff_observed$;

CREATE FUNCTION competition_review_handoff_import_guard() RETURNS trigger LANGUAGE plpgsql AS $handoff_import$
BEGIN
 IF COALESCE(NEW.review_json->'request'->>'handoff_id','')='' THEN
  IF NEW.review_json ? 'model_completed_at' THEN RAISE EXCEPTION 'legacy review cannot impersonate a pending handoff'; END IF;
  RETURN NEW;
 END IF;
 IF NEW.actor_id IS DISTINCT FROM current_setting('urnetwork.competition_review_operator_actor',true)
  OR NEW.created_at IS DISTINCT FROM NULLIF(current_setting('urnetwork.competition_review_handoff_clock',true),'')::timestamp
  OR competition_review_handoff_observed(NEW.review_json,NEW.created_at) IS NULL THEN
  RAISE EXCEPTION 'exact pending model proposal requires separate explicit operator import';
 END IF;
 RETURN NEW;
END $handoff_import$;
CREATE TRIGGER competition_review_handoff_import_before_insert BEFORE INSERT ON competition_integrity_review FOR EACH ROW EXECUTE FUNCTION competition_review_handoff_import_guard();
`
