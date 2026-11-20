package server

import "strings"

func CompetitionEpochThreeSourceRecoveryMigrationVersion() int {
	sql := competitionEpochThreeSourceRecoveryMigration()
	for index, migration := range migrations {
		if value, ok := migration.(*SqlMigration); ok && value.sql == sql {
			return index + 1
		}
	}
	panic("epoch-three source recovery migration is not registered")
}

func CompetitionEpochThreeSourceRecoveryBodiesSql() (binding, source, recovery string) {
	sql := competitionEpochThreeSourceRecoveryMigration()
	body := func(tag string) string {
		parts := strings.Split(sql, "$"+tag+"$")
		if len(parts) != 3 {
			panic("epoch-three source recovery body absent or ambiguous: " + tag)
		}
		return parts[1]
	}
	return body("official_two_binding"), body("winner_source"), body("epoch_three_recovery")
}

// Preserve the published clocks. Only the exact separately reviewed
// competition-only handoff can use this appended, single-use authorization.
func competitionEpochThreeSourceRecoveryMigration() string {
	extract := func(sql, start, end string) string {
		i := strings.Index(sql, start)
		if i < 0 {
			panic("epoch-three source recovery predecessor absent")
		}
		j := strings.Index(sql[i:], end)
		if j < 0 {
			panic("epoch-three source recovery predecessor unterminated")
		}
		return sql[i : i+j+len(end)]
	}
	replace := func(sql, old, next string) string {
		if strings.Count(sql, old) != 1 {
			panic("epoch-three source recovery anchor absent or ambiguous")
		}
		return strings.Replace(sql, old, next, 1)
	}
	binding := extract(competitionOfficialReviewEpochTwoSql, "CREATE FUNCTION competition_official_review_epoch_two_binding", "$official_two_binding$;")
	binding = replace(binding, "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION")
	window := "imported_at>=(checkpoint->>'started_at')::timestamp AND imported_at<(checkpoint->>'deadline')::timestamp"
	binding = replace(binding, window, "(("+window+") OR competition_epoch_three_source_recovery_matches(round_uuid,request,checkpoint,imported_at))")
	source := extract(competitionOfficialReviewEpochTwoMigration(), "CREATE OR REPLACE FUNCTION competition_v2_winner_source_authorized", "END $winner_source$;")
	clock := "(competition_review_handoff_observed(review.review_json,review.created_at)>="
	source = replace(source, clock, "(competition_epoch_three_source_recovery_matches(predecessor_id,review.review_json->'request',checkpoint,review.created_at) AND competition_official_review_request_authorized(predecessor_id,review.review_json->'request')) OR "+clock)
	return competitionEpochThreeSourceRecoverySql + binding + "\n" + source
}

const competitionEpochThreeSourceRecoverySql = `
SET LOCAL lock_timeout = '5s';
CREATE UNIQUE INDEX competition_epoch_three_source_recovery_once
 ON competition_official_review_authorization(round_id)
 WHERE round_id='01a1c110-585b-ce38-4f29-ca4860a1c058'::uuid
 AND import_json->'review'->>'private_evidence_sha256'='ce3876303e142c95f3af55fe1c97dc11326f65f201baccf090fa8fac0e92e808';

CREATE FUNCTION competition_epoch_three_source_recovery_matches(round_uuid uuid,request jsonb,checkpoint jsonb,imported_at timestamp)
RETURNS boolean LANGUAGE sql STABLE AS $epoch_three_recovery$
 SELECT COALESCE(EXISTS(
  SELECT 1 FROM competition_integrity_review prior
  WHERE round_uuid='01a1c110-585b-ce38-4f29-ca4860a1c058'::uuid
   AND prior.review_id='01a1f050-3e1b-05ea-1f68-47e072828d9e'::uuid AND prior.round_id=round_uuid
   AND prior.job_id='01a1e7b9-0b4e-cec4-3fad-526342ab01ac'::uuid
   AND request->>'job_id'=prior.job_id::text AND request->>'expected_revision'='2'
   AND request->>'expected_decision_sha256'='232be10e3627c421e92c1bc0c3197f3966ed6c8dfa7fbfda905d740d44e5146d'
   AND checkpoint->>'round_id'=round_uuid::text AND checkpoint->>'job_id'=prior.job_id::text
   AND checkpoint->>'reward_revision'=request->>'expected_revision'
   AND checkpoint->>'reward_decision_sha256'=request->>'expected_decision_sha256'
   AND prior.review_json->'request'->>'expected_revision'=request->>'expected_revision'
   AND prior.review_json->'request'->>'expected_decision_sha256'=request->>'expected_decision_sha256'
   AND prior.review_json->'request'->>'reward_disposition'='approved'
   AND prior.review_json->'request'->>'source_disposition'='deferred'
   AND prior.created_at>=(checkpoint->>'started_at')::timestamp AND prior.created_at<(checkpoint->>'deadline')::timestamp
   AND request->>'reward_disposition'='approved' AND request->>'source_disposition'='approved'
   AND request->>'reviewer_model'='gpt-6.2-astra' AND request->>'reviewer_reasoning'='max'
   AND request->>'private_evidence_sha256'='ce3876303e142c95f3af55fe1c97dc11326f65f201baccf090fa8fac0e92e808'
   AND (request->'official_review_override'->>'completed_at')::timestamptz='2026-11-18T21:29:01Z'::timestamptz
   AND imported_at>=(checkpoint->>'deadline')::timestamp
   AND imported_at>='2026-11-18 21:29:01'::timestamp AND imported_at<'2026-11-20 21:29:01'::timestamp
 ),false);
$epoch_three_recovery$;
`
