// Additive coverage custody binds prospective measurements to the complete
// frozen policy. Published eligibility and historical instrument guards remain
// unchanged; the trusted scorer, not SQL, computes the approved numeric floor.
package server

const competitionWorkCoverageMigration = `
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION competition_work_coverage_policy_valid(policy jsonb)
RETURNS boolean LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $coverage_policy$
 SELECT COALESCE(jsonb_typeof(policy)='object' AND policy->>'kind'='seeded-crawl-coverage/v1',false)
$coverage_policy$;

CREATE FUNCTION competition_work_coverage_score_valid(score jsonb, evaluation_policy jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $coverage_score$
DECLARE policy jsonb; gate jsonb;
BEGIN
 policy:=evaluation_policy->'work_coverage_policy';
 IF policy IS NULL THEN
  RETURN NOT COALESCE(score->'diagnostics' ? 'work_coverage_policy',false)
   AND NOT COALESCE(score->'gates' ? 'G7_work_coverage',false)
   AND COALESCE(score->'diagnostics'->>'scorer_version','')<>'sim-latency-score/3';
 END IF;
 gate:=score->'gates'->'G7_work_coverage';
 RETURN COALESCE(competition_work_coverage_policy_valid(policy)
  AND evaluation_policy->>'g2_volume_policy'='minimum-work/1'
  AND jsonb_typeof(score)='object'
  AND score->'diagnostics'->'work_coverage_policy'=policy
  AND score->'diagnostics'->>'g2_volume_policy'='minimum-work/1'
  AND score->'diagnostics'->>'scorer_version'='sim-latency-score/3'
  AND jsonb_typeof(gate)='object' AND jsonb_typeof(gate->'passed')='boolean'
  AND jsonb_typeof(gate->'details')='object' AND gate->'details'->'policy'=policy
  AND (gate->'passed'='true'::jsonb OR (score->'placeable'='false'::jsonb AND COALESCE(score->'takeover_eligible','false'::jsonb)='false'::jsonb)),false);
END $coverage_score$;

CREATE FUNCTION competition_v2_score_eligible_policy(score jsonb, baseline_raw double precision, margin double precision, staging_mode boolean, evaluation_policy jsonb)
RETURNS boolean LANGUAGE plpgsql IMMUTABLE AS $coverage_eligible$
BEGIN
 IF NOT competition_work_coverage_score_valid(score,evaluation_policy) THEN RETURN false; END IF;
 RETURN competition_v2_score_eligible_mode(score,baseline_raw,margin,staging_mode);
END $coverage_eligible$;

CREATE FUNCTION competition_work_coverage_job_guard() RETURNS trigger LANGUAGE plpgsql AS $coverage_job$
DECLARE evaluation_policy jsonb;
BEGIN
 SELECT policy_json->'evaluation_policy' INTO evaluation_policy FROM competition_round WHERE round_id=NEW.round_id;
 IF (NEW.score_json IS NULL OR NEW.score_json='null'::jsonb) AND
  (NEW.state IS DISTINCT FROM 'succeeded' OR NOT COALESCE(evaluation_policy ? 'work_coverage_policy',false)) THEN RETURN NEW; END IF;
 IF NOT FOUND OR NOT competition_work_coverage_score_valid(NEW.score_json,evaluation_policy) THEN
  RAISE EXCEPTION 'score lacks the exact frozen coverage policy, scorer version or mandatory G7';
 END IF;
 RETURN NEW;
END $coverage_job$;
CREATE TRIGGER competition_work_coverage_job BEFORE INSERT OR UPDATE ON competition_job
 FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_job_guard();

CREATE FUNCTION competition_work_coverage_baseline_guard() RETURNS trigger LANGUAGE plpgsql AS $coverage_baseline$
DECLARE policy jsonb; evaluation_policy jsonb; baseline jsonb;
BEGIN
 SELECT policy_json->'evaluation_policy' INTO evaluation_policy FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'coverage baseline has no frozen round'; END IF;
 policy:=evaluation_policy->'work_coverage_policy';
 baseline:=convert_from(NEW.baseline_json,'UTF8')::jsonb;
 IF policy IS NULL THEN
  IF baseline ? 'work_coverage_policy' OR baseline->>'scorer_version'='sim-latency-score/3' THEN
   RAISE EXCEPTION 'historical round cannot acquire an unapproved coverage baseline';
  END IF;
  RETURN NEW;
 END IF;
 IF NOT competition_work_coverage_policy_valid(policy)
  OR evaluation_policy->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
  OR baseline->'work_coverage_policy' IS DISTINCT FROM policy
  OR baseline->>'scorer_version' IS DISTINCT FROM 'sim-latency-score/3'
  OR baseline->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
  OR jsonb_typeof(baseline->'replicates') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'baseline differs from the frozen coverage instrument';
 END IF;
 IF jsonb_array_length(baseline->'replicates')=0 OR EXISTS(
  SELECT 1 FROM jsonb_array_elements(baseline->'replicates') r WHERE jsonb_typeof(r->'work_coverage') IS DISTINCT FROM 'object') THEN
  RAISE EXCEPTION 'coverage baseline omitted its independent measured work summaries';
 END IF;
 RETURN NEW;
END $coverage_baseline$;
CREATE TRIGGER competition_work_coverage_baseline BEFORE INSERT ON competition_round_baseline
 FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_baseline_guard();

CREATE FUNCTION competition_work_coverage_seal_guard() RETURNS trigger LANGUAGE plpgsql AS $coverage_seal$
DECLARE evaluation_policy jsonb; staging_mode boolean; entry jsonb; baseline_raw double precision; margin double precision;
BEGIN
 SELECT policy_json->'evaluation_policy',staging INTO evaluation_policy,staging_mode FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'coverage seal has no frozen round'; END IF;
 SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
  FROM competition_round_baseline b,jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=NEW.round_id;
 margin:=(evaluation_policy->>'takeover_margin')::double precision;
 FOR entry IN SELECT value FROM jsonb_array_elements(NEW.seal_json->'entries') LOOP
  IF entry->>'state'='succeeded' OR (entry->'score' IS NOT NULL AND entry->'score'<>'null'::jsonb) THEN
   IF NOT competition_work_coverage_score_valid(entry->'score',evaluation_policy) THEN
    RAISE EXCEPTION 'seal contains an unbound or downgraded coverage score';
   END IF;
   IF entry->'eligible'='true'::jsonb AND NOT competition_v2_score_eligible_policy(entry->'score',baseline_raw,margin,staging_mode,evaluation_policy) THEN
    RAISE EXCEPTION 'seal cannot make an incomplete coverage score eligible';
   END IF;
  END IF;
 END LOOP;
 RETURN NEW;
END $coverage_seal$;
CREATE TRIGGER competition_work_coverage_seal BEFORE INSERT ON competition_evaluation_seal
 FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_seal_guard();

CREATE FUNCTION competition_work_coverage_source_guard() RETURNS trigger LANGUAGE plpgsql AS $coverage_source$
DECLARE intent jsonb; previous jsonb; policy jsonb; prior_policy jsonb; configured jsonb; proof jsonb;
 evaluation_policy jsonb; selected_score jsonb; selected_round uuid; staging_mode boolean; baseline_raw double precision;
BEGIN
 -- Historical active receipts remain immutable replayable evidence. New
 -- preparation and the pending-to-active boundary both receive this check.
 IF TG_OP='UPDATE' AND OLD.activated_at IS NOT NULL THEN RETURN NEW; END IF;
 intent:=NEW.intent_json;
 policy:=intent->'work_coverage_policy';
 SELECT a.intent_json INTO previous FROM competition_source_pointer p
  JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
  WHERE p.environment=NEW.environment AND p.competition_id=NEW.competition_id AND p.season_id=NEW.season_id AND a.activated_at IS NOT NULL;
 prior_policy:=previous->'work_coverage_policy';
 configured:=NULLIF(current_setting('urnetwork.competition_v2_instrument_approval_json',true),'')::jsonb;
 proof:=NULLIF(intent->'trusted_instrument','null'::jsonb);
 IF configured ? 'work_coverage_policy' THEN
  IF NOT competition_work_coverage_policy_valid(configured->'work_coverage_policy') THEN
   RAISE EXCEPTION 'configured instrument coverage policy is malformed';
  END IF;
  IF NOT COALESCE((intent->>'staging')::boolean,false) AND NEW.target_epoch >= (configured->>'target_epoch')::integer
   AND policy IS DISTINCT FROM configured->'work_coverage_policy' THEN
   RAISE EXCEPTION 'new source cannot omit or change the configured coverage instrument';
  END IF;
 END IF;
 IF policy IS NULL THEN
  IF prior_policy IS NOT NULL OR proof->'approval' ? 'work_coverage_policy' OR proof->'build_receipt' ? 'work_coverage_policy' THEN
   RAISE EXCEPTION 'source cannot strip an approved coverage policy';
  END IF;
 ELSE
  IF NOT competition_work_coverage_policy_valid(policy) OR intent->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1' THEN
   RAISE EXCEPTION 'source coverage requires the approved minimum-work instrument';
  END IF;
  IF proof IS NULL THEN
   IF policy IS DISTINCT FROM prior_policy THEN RAISE EXCEPTION 'new coverage policy requires independent instrument proof'; END IF;
  ELSIF proof->'approval'->'work_coverage_policy' IS DISTINCT FROM policy
   OR proof->'build_receipt'->'work_coverage_policy' IS DISTINCT FROM policy
   OR configured->'work_coverage_policy' IS DISTINCT FROM policy
   OR proof->'approval' IS DISTINCT FROM configured THEN
   RAISE EXCEPTION 'coverage source, approval and actual build receipt must bind the same complete policy';
  END IF;
 END IF;
 -- Genuine product promotion keeps the evaluated round's policy, which can
 -- differ from the independently approved next-epoch instrument policy.
 IF intent->>'review_id' IS NOT NULL THEN
  SELECT r.round_id,r.policy_json->'evaluation_policy',r.staging,j.score_json INTO selected_round,evaluation_policy,staging_mode,selected_score
   FROM competition_integrity_review v JOIN competition_round r ON r.round_id=v.round_id
   JOIN competition_job j ON j.job_id=v.job_id AND j.round_id=r.round_id WHERE v.review_id=(intent->>'review_id')::uuid;
  IF FOUND AND evaluation_policy ? 'work_coverage_policy' THEN
   SELECT percentile_disc(0.5) WITHIN GROUP (ORDER BY (r->>'raw_score')::double precision) INTO baseline_raw
    FROM competition_round_baseline b,jsonb_array_elements(convert_from(b.baseline_json,'UTF8')::jsonb->'replicates') r WHERE b.round_id=selected_round;
   IF NOT competition_v2_score_eligible_policy(selected_score,baseline_raw,(evaluation_policy->>'takeover_margin')::double precision,staging_mode,evaluation_policy) THEN
    RAISE EXCEPTION 'source promotion lacks eligible exact-policy coverage evidence';
   END IF;
  END IF;
 END IF;
 RETURN NEW;
END $coverage_source$;
CREATE TRIGGER competition_work_coverage_source BEFORE INSERT OR UPDATE ON competition_source_activation
 FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_source_guard();

CREATE FUNCTION competition_work_coverage_round_guard() RETURNS trigger LANGUAGE plpgsql AS $coverage_round$
DECLARE automation jsonb; evaluation jsonb; intent jsonb; policy jsonb;
BEGIN
 evaluation:=NEW.policy_json->'evaluation_policy'; policy:=evaluation->'work_coverage_policy';
 IF (policy IS NOT NULL AND NEW.policy_json->>'scorer_version' IS DISTINCT FROM 'sim-latency-score/3')
  OR (policy IS NULL AND NEW.policy_json->>'scorer_version'='sim-latency-score/3') THEN
  RAISE EXCEPTION 'frozen coverage policy requires its exact scorer version';
 END IF;
 IF TG_OP='UPDATE' THEN
  IF policy IS DISTINCT FROM OLD.policy_json->'evaluation_policy'->'work_coverage_policy'
   OR (policy IS NOT NULL AND evaluation->>'g2_volume_policy' IS DISTINCT FROM OLD.policy_json->'evaluation_policy'->>'g2_volume_policy') THEN
   RAISE EXCEPTION 'frozen coverage policy is immutable';
  END IF;
  RETURN NEW;
 END IF;
 automation:=NEW.policy_json->'automation_policy';
 IF COALESCE((automation->>'enabled')::boolean,false) THEN
  SELECT a.intent_json INTO intent FROM competition_source_pointer p
   JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
   WHERE p.environment=automation->>'environment' AND p.competition_id=NEW.competition_id AND p.season_id=automation->>'season_id'
    AND a.activated_at IS NOT NULL AND a.generation=(automation->>'source_activation_generation')::bigint;
 END IF;
 IF policy IS NOT NULL OR intent ? 'work_coverage_policy' THEN
  IF NOT competition_work_coverage_policy_valid(policy) OR policy IS DISTINCT FROM intent->'work_coverage_policy'
   OR evaluation->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1'
   OR intent->>'g2_volume_policy' IS DISTINCT FROM 'minimum-work/1' THEN
   RAISE EXCEPTION 'round coverage policy differs from its exact activated source';
  END IF;
 END IF;
 RETURN NEW;
END $coverage_round$;
CREATE TRIGGER competition_work_coverage_round BEFORE INSERT OR UPDATE ON competition_round
 FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_round_guard();

CREATE FUNCTION competition_work_coverage_equivalence_guard() RETURNS trigger LANGUAGE plpgsql AS $coverage_equivalence$
DECLARE policy jsonb; metric jsonb;
BEGIN
 SELECT policy_json->'evaluation_policy'->'work_coverage_policy' INTO policy FROM competition_round WHERE round_id=NEW.round_id;
 IF NOT FOUND THEN RAISE EXCEPTION 'coverage host receipt has no frozen round'; END IF;
 IF policy IS NULL THEN
  IF NEW.receipt_json ? 'work_coverage_policy' THEN RAISE EXCEPTION 'historical host receipt cannot select a new coverage policy'; END IF;
  RETURN NEW;
 END IF;
 IF NOT competition_work_coverage_policy_valid(policy) OR NEW.receipt_json->'work_coverage_policy' IS DISTINCT FROM policy
  OR jsonb_typeof(NEW.receipt_json->'metrics') IS DISTINCT FROM 'array' THEN
  RAISE EXCEPTION 'host receipt lacks the complete frozen coverage policy and run-level metrics';
 END IF;
 IF jsonb_array_length(NEW.receipt_json->'metrics')<>9 OR
  (SELECT count(DISTINCT m->>'metric') FROM jsonb_array_elements(NEW.receipt_json->'metrics') m
   WHERE m->>'metric'=ANY(ARRAY['raw_score','request_count','received_bytes','ttfb_p50_ms','ttfb_p95_ms','throughput_p50_bytes_per_s','throughput_p95_bytes_per_s','page_coverage','byte_coverage']))<>9 THEN
  RAISE EXCEPTION 'coverage host receipt requires the exact nine-metric census';
 END IF;
 FOR metric IN SELECT value FROM jsonb_array_elements(NEW.receipt_json->'metrics') LOOP
  IF jsonb_typeof(metric->'baseline_values') IS DISTINCT FROM 'array' OR jsonb_typeof(metric->'candidate_values') IS DISTINCT FROM 'array'
   OR jsonb_typeof(metric->'baseline_sample_variance') IS DISTINCT FROM 'number' OR jsonb_typeof(metric->'candidate_sample_variance') IS DISTINCT FROM 'number'
   OR jsonb_typeof(metric->'difference_ci_low') IS DISTINCT FROM 'number' OR jsonb_typeof(metric->'difference_ci_high') IS DISTINCT FROM 'number' THEN
   RAISE EXCEPTION 'coverage host metric omitted its run-level samples or uncertainty';
  END IF;
  IF jsonb_array_length(metric->'baseline_values')<>9 OR jsonb_array_length(metric->'candidate_values')<>9
   OR EXISTS(SELECT 1 FROM jsonb_array_elements((metric->'baseline_values')||(metric->'candidate_values')) v WHERE jsonb_typeof(v) IS DISTINCT FROM 'number') THEN
   RAISE EXCEPTION 'coverage host metric requires nine independent numeric values per side';
  END IF;
 END LOOP;
 RETURN NEW;
END $coverage_equivalence$;
CREATE TRIGGER competition_work_coverage_equivalence BEFORE INSERT ON competition_executor_equivalence
 FOR EACH ROW EXECUTE FUNCTION competition_work_coverage_equivalence_guard();
`
