// Prospective guards preserve historical definitions and bind source to the
// elected winner. The optional SQL test owns a completely isolated database.
package server

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A changed migration template must never silently omit independent authority.
func TestCompetitionWinnerReviewMigrationPreservesLegacyAndBindsWinner(t *testing.T) {
	sql := competitionWinnerReviewMigration()
	for _, required := range []string{"competition_v2_winner_source_authorized", "competition_v2_winner_review_guard", "review.created_at>choice_at", "competition_v2_source_graph_valid", "current elected reward decision", "checkpoint IS DISTINCT FROM actual_checkpoint", "round and activated source closing policy disagree"} {
		if !strings.Contains(sql, required) {
			t.Fatalf("prospective migration omitted %q", required)
		}
	}
	if !strings.Contains(competitionAutomationStagingSourceGraphMigration, "review.created_at>choice_at") || strings.Contains(competitionAutomationStagingSourceGraphMigration, "winner_review_policy") {
		t.Fatal("historical migration was rewritten")
	}
}

// No ports, host paths, main configuration, or existing database are used.
// SQL exceptions force wrong-winner/deadline/fallback cases without timing luck.
func TestCompetitionWinnerReviewDatabaseGuards(t *testing.T) {
	testCompetitionWinnerReviewDatabaseGuards(t, competitionWinnerReviewMigration(), winnerReviewSqlAssertions)
}

// Each caller installs only into its own fresh, networkless fixture database.
func testCompetitionWinnerReviewDatabaseGuards(t *testing.T, migration, assertions string) {
	t.Helper()
	if os.Getenv("COMPETITION_WINNER_REVIEW_SQL_TEST") != "true" {
		t.Skip("set COMPETITION_WINNER_REVIEW_SQL_TEST=true for isolated PostgreSQL guards")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	name := "competition-winner-guard-" + strings.ReplaceAll(NewId().String(), "-", "")
	run := func(input string, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, "sudo", append([]string{"-n", "docker"}, args...)...)
		command.Stdin = strings.NewReader(input)
		return command.CombinedOutput()
	}
	output, err := run("", "run", "--pull=never", "--detach", "--rm", "--name", name, "--network", "none", "--memory", "512m", "--cpus", "1", "--pids-limit", "128", "-e", "POSTGRES_HOST_AUTH_METHOD=trust", "-e", "POSTGRES_DB=competition_test_winner", "postgres:18")
	if err != nil {
		t.Fatalf("isolated PostgreSQL start: %s %v", output, err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_ = exec.CommandContext(cleanup, "sudo", "-n", "docker", "rm", "--force", "--volumes", name).Run()
	})
	for {
		// The image's initialization server accepts Unix sockets before the
		// requested database exists. TCP plus a real query proves final startup.
		if _, err = run("", "exec", name, "psql", "--host=localhost", "-U", "postgres", "-d", "competition_test_winner", "--no-psqlrc", "--command=SELECT 1"); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("isolated PostgreSQL startup timeout")
		case <-time.After(50 * time.Millisecond):
		}
	}
	output, err = run(winnerReviewSqlFixture+migration+assertions, "exec", "--interactive", name, "psql", "--host=localhost", "-U", "postgres", "-d", "competition_test_winner", "--no-psqlrc", "--set=ON_ERROR_STOP=1")
	if err != nil {
		t.Fatalf("SQL winner authority regression: %s %v", output, err)
	}
}

const winnerReviewSqlFixture = `
CREATE TABLE competition_round(round_id uuid,competition_id text,staging boolean,epoch_number integer,policy_json jsonb);
CREATE TABLE competition_integrity_review(review_id uuid,round_id uuid,job_id uuid,request_sha256 text,review_json jsonb,review_cursor bigserial,created_at timestamp);
CREATE TABLE competition_source_pointer(environment text,competition_id text,season_id text,generation bigint);
CREATE TABLE competition_source_activation(environment text,competition_id text,season_id text,generation bigint,intent_json jsonb,activated_at timestamp,abandoned_at timestamp);
CREATE TABLE competition_reward_decision(round_id uuid,revision integer,decision_json jsonb);
CREATE TABLE competition_supervisor_transition(environment text,competition_id text,season_id text,target_epoch integer,revision integer,transition_json jsonb,updated_at timestamp DEFAULT timezone('UTC',now()));
CREATE TABLE competition_automation_incident(round_id uuid,invalidates_evaluation boolean);
CREATE TABLE competition_evaluation_seal(round_id uuid,seal_json jsonb);
CREATE TABLE competition_automation_hold(environment text,competition_id text,season_id text,kind text,revision integer,active boolean);
INSERT INTO competition_round VALUES('00000000-0000-0000-0000-000000000001','competition-test',false,1,
 '{"automation_policy":{"environment":"test","season_id":"season-test","winner_review_policy":"elected_winner_sol61max_v1","winner_review_timeout_seconds":600}}');
INSERT INTO competition_reward_decision VALUES('00000000-0000-0000-0000-000000000001',1,
 jsonb_build_object('revision',1,'selected_job_id','00000000-0000-0000-0000-000000000002','decision_sha256',repeat('a',64)));
INSERT INTO competition_evaluation_seal VALUES('00000000-0000-0000-0000-000000000001',jsonb_build_object('publication_at',timezone('UTC',now())-interval '1 hour'));
INSERT INTO competition_supervisor_transition(environment,competition_id,season_id,target_epoch,revision,transition_json) VALUES('test','competition-test','season-test',2,1,
 jsonb_build_object('winner_review',jsonb_build_object('round_id','00000000-0000-0000-0000-000000000001','job_id','00000000-0000-0000-0000-000000000002',
 'reward_revision',1,'reward_decision_sha256',repeat('a',64),'started_at',timezone('UTC',now())-interval '5 seconds','deadline',timezone('UTC',now())+interval '595 seconds')));
INSERT INTO competition_integrity_review VALUES('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',repeat('b',64),
 jsonb_build_object('request',jsonb_build_object('job_id','00000000-0000-0000-0000-000000000002','source_disposition','approved','reward_disposition','approved',
 'expected_revision',1,'expected_decision_sha256',repeat('a',64),'reviewer_model','gpt-6.1-sol','reviewer_reasoning','max')),DEFAULT,timezone('UTC',now()));
`

const winnerReviewSqlAssertions = `
DO $test$
DECLARE intent jsonb; checkpoint jsonb; altered jsonb; rejected boolean;
BEGIN
 SELECT transition_json->'winner_review' INTO checkpoint FROM competition_supervisor_transition;
 intent:=jsonb_build_object('environment','test','competition_id','competition-test','season_id','season-test','target_epoch',2,
  'winner_review_policy','elected_winner_sol61max_v1','kind','reviewed_source_promotion','review_id','00000000-0000-0000-0000-000000000003',
  'reward_decision_digest',repeat('a',64),'winner_review',checkpoint,'build_ready_at',timezone('UTC',now()),'prepared_at',timezone('UTC',now()));
 IF NOT competition_v2_winner_source_authorized(intent,'00000000-0000-0000-0000-000000000001') THEN RAISE EXCEPTION 'valid winner refused'; END IF;
 rejected:=false;
 BEGIN INSERT INTO competition_supervisor_transition(environment,competition_id,season_id,target_epoch,revision,transition_json)
  VALUES('test','competition-test','season-test',2,2,jsonb_build_object('winner_review',jsonb_set(checkpoint,'{deadline}',to_jsonb('2099-01-01T00:00:00Z'::text))));
 EXCEPTION WHEN OTHERS THEN rejected:=SQLERRM LIKE '%deadline cannot reset%'; END;
 IF NOT rejected THEN RAISE EXCEPTION 'database allowed a restarted review deadline'; END IF;
 rejected:=false;
 BEGIN
  PERFORM competition_v2_winner_source_authorized(intent||'{"kind":"reviewed_source_carry_forward","reason":"review_pending"}'::jsonb,'00000000-0000-0000-0000-000000000001');
 EXCEPTION WHEN OTHERS THEN rejected:=SQLERRM LIKE '%genuinely reviewed elected winner%'; END;
 IF NOT rejected THEN RAISE EXCEPTION 'pending winner allowed carry-forward'; END IF;
 altered:=jsonb_set(intent,'{winner_review,deadline}',to_jsonb('2099-01-01T00:00:00Z'::text));rejected:=false;
 BEGIN PERFORM competition_v2_winner_source_authorized(altered,'00000000-0000-0000-0000-000000000001');
 EXCEPTION WHEN OTHERS THEN rejected:=SQLERRM LIKE '%durable in-deadline%'; END;
 IF NOT rejected THEN RAISE EXCEPTION 'deadline was caller extendable'; END IF;
 UPDATE competition_integrity_review SET job_id='00000000-0000-0000-0000-000000000004';rejected:=false;
 BEGIN PERFORM competition_v2_winner_source_authorized(intent,'00000000-0000-0000-0000-000000000001');
 EXCEPTION WHEN OTHERS THEN rejected:=SQLERRM LIKE '%genuinely reviewed elected winner%'; END;
 IF NOT rejected THEN RAISE EXCEPTION 'different candidate was allowed'; END IF;
 UPDATE competition_integrity_review SET job_id='00000000-0000-0000-0000-000000000002';
 UPDATE competition_reward_decision SET decision_json=jsonb_build_object('selected_job_id',NULL,'decision_sha256',repeat('a',64));
 altered:=(intent-'review_id'-'winner_review')||'{"kind":"reviewed_source_carry_forward","reason":"no_reward_winner"}'::jsonb;
 IF NOT competition_v2_winner_source_authorized(altered,'00000000-0000-0000-0000-000000000001') THEN RAISE EXCEPTION 'no-winner carry-forward refused'; END IF;
 UPDATE competition_round SET policy_json='{"automation_policy":{}}';
 IF competition_v2_winner_source_authorized(altered-'winner_review_policy','00000000-0000-0000-0000-000000000001') THEN RAISE EXCEPTION 'historical policy changed'; END IF;
END $test$;
`
