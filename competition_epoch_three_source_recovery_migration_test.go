package server

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestCompetitionEpochThreeSourceRecoveryPostgres(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		Db(tb.Context(), func(conn PgConn) {
			RaisePgResult(conn.Exec(tb.Context(), competitionOfficialEpochTwoSqlFixture+
				`CREATE TABLE competition_official_review_authorization(round_id uuid, import_json jsonb);`+
				competitionEpochThreeSourceRecoverySql+competitionEpochThreeSourceRecoveryFixtureSql, pgx.QueryExecModeSimpleProtocol))
		}, OptReadWrite(), OptNoRetry())
	})
}

const competitionEpochThreeSourceRecoveryFixtureSql = `
DO $test$
DECLARE
 r uuid:='01a1c110-585b-ce38-4f29-ca4860a1c058';
 j uuid:='01a1e7b9-0b4e-cec4-3fad-526342ab01ac';
 d text:='232be10e3627c421e92c1bc0c3197f3966ed6c8dfa7fbfda905d740d44e5146d';
 e text:='ce3876303e142c95f3af55fe1c97dc11326f65f201baccf090fa8fac0e92e808';
 request jsonb;
 checkpoint jsonb;
BEGIN
 request:=jsonb_build_object('job_id',j,'expected_revision',2,'expected_decision_sha256',d,
  'reward_disposition','approved','source_disposition','approved','reviewer_model','gpt-6.2-astra','reviewer_reasoning','max',
  'private_evidence_sha256',e,'official_review_override',jsonb_build_object('completed_at','2026-11-18T21:29:01Z'));
 checkpoint:=jsonb_build_object('round_id',r,'job_id',j,'reward_revision',2,'reward_decision_sha256',d,
  'started_at','2026-11-18T20:00:00Z','deadline','2026-11-18T20:48:56Z');
 INSERT INTO competition_integrity_review(review_id,round_id,job_id,review_json,created_at)
 VALUES('01a1f050-3e1b-05ea-1f68-47e072828d9e',r,j,jsonb_build_object('request',
  jsonb_set(request,'{source_disposition}','"deferred"')),timestamp '2026-11-18 20:01:00');
 IF NOT competition_epoch_three_source_recovery_matches(r,request,checkpoint,timestamp '2026-11-18 21:29:01') THEN
  RAISE EXCEPTION 'exact recovery refused';
 END IF;
 IF competition_epoch_three_source_recovery_matches(r,request,checkpoint,timestamp '2026-11-18 21:29:00')
  OR competition_epoch_three_source_recovery_matches(r,request,checkpoint,timestamp '2026-11-20 21:29:01')
  OR competition_epoch_three_source_recovery_matches(r,request - 'private_evidence_sha256',checkpoint,timestamp '2026-11-18 21:29:01')
  OR competition_epoch_three_source_recovery_matches(r,request,checkpoint - 'reward_revision',timestamp '2026-11-18 21:29:01')
  OR competition_epoch_three_source_recovery_matches(r,jsonb_set(request,'{expected_revision}','3'),checkpoint,timestamp '2026-11-18 21:29:01') THEN
  RAISE EXCEPTION 'altered recovery accepted';
 END IF;
 INSERT INTO competition_official_review_authorization VALUES(r,jsonb_build_object('review',request));
 BEGIN
  INSERT INTO competition_official_review_authorization VALUES(r,jsonb_build_object('review',request));
  RAISE EXCEPTION 'second recovery grant accepted';
 EXCEPTION WHEN unique_violation THEN NULL;
 END;
END $test$;
`

func TestCompetitionEpochThreeSourceRecoveryMigrationRetainsClock(t *testing.T) {
	sql := competitionEpochThreeSourceRecoveryMigration()
	for _, required := range []string{
		"CREATE UNIQUE INDEX competition_epoch_three_source_recovery_once",
		"imported_at<(checkpoint->>'deadline')::timestamp)",
		"imported_at>=(checkpoint->>'deadline')::timestamp",
		"imported_at<'2026-11-20 21:29:01'::timestamp",
		"prior.created_at<(checkpoint->>'deadline')::timestamp",
		"checkpoint IS DISTINCT FROM actual_checkpoint",
		"competition_official_review_request_authorized(predecessor_id,review.review_json->'request')",
		"build_ready_at",
		"AND NOT EXISTS(SELECT 1 FROM competition_automation_incident",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration lost recovery boundary %q", required)
		}
	}
	for _, forbidden := range []string{"UPDATE competition_", "DELETE FROM competition_", "DROP ", "competition_v2_winner_checkpoint_guard"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("migration modifies historical state or its guard: %q", forbidden)
		}
	}
}
