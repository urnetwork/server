// Disposable PostgreSQL failures must not expose a scanned claim or commit one
// authority without the other. Public admission/claim paths retain all history.
package controller

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Genuine admission leaves a pristine queued job with no charged attempt/start.
func newCompetitionClaimRollbackPgFixture(t testing.TB, ctx context.Context, name string) (*submissionArchiveFixture, *queuedJob) {
	t.Helper()
	fixture := newSubmissionArchiveFixture(t, ctx, "synthetic-claim-rollback-"+name)
	round := fixture.openStagingRound(t, ctx)
	job, cacheHit, err := fixture.store.Enqueue(ctx, fixture.settings, round.RoundId, fixture.patch(t, "synthetic-claim-rollback"),
		"synthetic-claim-submitter", testApiImageDigest())
	if err != nil || job == nil || cacheHit || job.State != "queued" || job.AttemptCount != 0 || job.StartedAt != nil {
		t.Fatalf("admit pristine synthetic claim: job=%+v cache_hit=%t error=%v", job, cacheHit, err)
	}
	return fixture, job
}

// An error after the job UPDATE must erase the candidate return value as well
// as roll back stored authority. A later clean call starts only its first run.
func TestCompetitionClaimPgReturnsNoJobAfterRollback(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture, job := newCompetitionClaimRollbackPgFixture(t, ctx, "sql-error")
		before := readCompetitionCompletionPgSnapshot(ctx, job)
		beforeEvidence := readCompetitionReevaluationPgSnapshot(ctx, job)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_refuse_claim_slot() RETURNS trigger AS $body$
			 BEGIN
			  IF NOT EXISTS(SELECT 1 FROM competition_job WHERE job_id=NEW.job_id
			   AND state='running' AND attempt_count=1 AND started_at IS NOT NULL) THEN
			   RAISE EXCEPTION USING ERRCODE='P0002',MESSAGE='synthetic claim job was not staged';
			  END IF;
			  RAISE EXCEPTION USING ERRCODE='P0001',MESSAGE='synthetic claim slot refusal';
			 END $body$ LANGUAGE plpgsql;
			 CREATE TRIGGER synthetic_refuse_claim_slot BEFORE UPDATE ON competition_worker_slot
			 FOR EACH ROW EXECUTE FUNCTION synthetic_refuse_claim_slot();`))
		})
		claimed, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-claim-worker", testWorkerImageDigest())
		var pgError *pgconn.PgError
		if !errors.As(err, &pgError) || pgError.Code != "P0001" {
			t.Fatalf("claim did not reach the post-job-update SQL refusal: %v", err)
		}
		if claimed != nil {
			t.Fatal("rolled-back claim returned an uncommitted scanned job")
		}
		if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
			t.Fatal("SQL refusal partially changed job/slot authority, start, attempt or events")
		}
		if afterEvidence := readCompetitionReevaluationPgSnapshot(ctx, job); !reflect.DeepEqual(afterEvidence, beforeEvidence) {
			t.Fatal("SQL refusal rewrote original submission, history, principals, baseline or audit")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_refuse_claim_slot ON competition_worker_slot;
			 DROP FUNCTION synthetic_refuse_claim_slot();`))
		})
		fixture.clock = fixture.clock.Add(time.Second)
		claimed, err = fixture.store.Claim(ctx, fixture.settings, "synthetic-claim-worker", testWorkerImageDigest())
		if err != nil || claimed == nil || claimed.JobId != job.JobId || claimed.State != "running" || claimed.AttemptCount != 1 ||
			claimed.StartedAt == nil || !claimed.StartedAt.Equal(fixture.clock) || claimed.EvaluationCycle != 0 || claimed.CycleStartedAt != nil {
			t.Fatalf("clean claim consumed a rolled-back attempt or execution clock: job=%+v error=%v", claimed, err)
		}
		if after := readCompetitionCompletionPgSnapshot(ctx, job); after.events != before.events+1 {
			t.Fatal("clean retry did not append exactly one committed claim event")
		}
	})
}

// A suppressed job or slot UPDATE must roll back the entire acquisition,
// return no candidate and leave attempt/start, immutable history and audit intact.
func TestCompetitionClaimPgRollsBackSuppressedAuthorityUpdates(t *testing.T) {
	for _, check := range []struct {
		name, table string
	}{
		{name: "job", table: "competition_job"},
		{name: "slot", table: "competition_worker_slot"},
	} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx := context.Background()
			fixture, job := newCompetitionClaimRollbackPgFixture(t, ctx, check.name)
			before := readCompetitionCompletionPgSnapshot(ctx, job)
			beforeEvidence := readCompetitionReevaluationPgSnapshot(ctx, job)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_suppress_claim() RETURNS trigger AS $body$
				 BEGIN RETURN NULL; END $body$ LANGUAGE plpgsql;`))
				// Both relation names are fixed synthetic test cases, never input.
				server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER synthetic_suppress_claim BEFORE UPDATE ON `+check.table+`
				 FOR EACH ROW EXECUTE FUNCTION synthetic_suppress_claim();`))
			})
			claimed, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-claim-worker", testWorkerImageDigest())
			if !errors.Is(err, ErrLeaseLost) || claimed != nil {
				t.Fatalf("suppressed %s update exposed a partial claim: job=%+v error=%v", check.name, claimed, err)
			}
			if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatalf("suppressed %s update changed job/slot authority, start, attempt or events", check.name)
			}
			if afterEvidence := readCompetitionReevaluationPgSnapshot(ctx, job); !reflect.DeepEqual(afterEvidence, beforeEvidence) {
				t.Fatalf("suppressed %s update rewrote original evidence or audit", check.name)
			}
		})
	}
}
