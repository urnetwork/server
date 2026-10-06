// Competition store transactions that server.Tx reruns keep only the
// committed run's results. A deferred trigger on the job event log fails the
// first commit that writes a chosen event type with a serialization failure,
// so server.Tx reruns that callback; a rolled-back run's discarded jobs must
// not reach the append-only log a second time.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Fails the first commit after arming that writes an event of the given type.
func armCompetitionEventFault(ctx context.Context, name string, eventType string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE SEQUENCE synthetic_%[1]s_commit;
			CREATE CONSTRAINT TRIGGER synthetic_%[1]s_fault
				AFTER INSERT ON competition_job_event
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW EXECUTE FUNCTION synthetic_competition_event_fault('%[2]s', 'synthetic_%[1]s_commit');
		`, name, eventType)))
	})
}

// A discard whose transaction reruns records one event per discarded job:
// the claim's outside-window discard, the staging discard when epoch one
// commits, and the staging supersede when the operator replaces the round.
func TestCompetitionDiscardRecordsOneEventWhenItsTransactionReruns(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION synthetic_competition_event_fault() RETURNS trigger AS $fault$
				BEGIN
					IF NEW.event_type = TG_ARGV[0] THEN
						IF nextval(TG_ARGV[1]::regclass) = 1 THEN
							RAISE EXCEPTION 'synthetic serialization failure at commit' USING ERRCODE = '40001';
						END IF;
					END IF;
					RETURN NULL;
				END
				$fault$ LANGUAGE plpgsql;
			`))
		})
		for _, c := range []struct {
			name      string
			eventType string
			discard   func(fixture *submissionArchiveFixture) error
		}{
			{
				name:      "claim",
				eventType: "discarded_outside_epoch",
				discard: func(fixture *submissionArchiveFixture) error {
					_, err := fixture.store.Claim(ctx, fixture.settings, "worker-a", testWorkerImageDigest())
					return err
				},
			},
			{
				name:      "epoch_one",
				eventType: "staging_discarded",
				discard: func(fixture *submissionArchiveFixture) error {
					_ = fixture.openProductionRound(t, ctx)
					return nil
				},
			},
			{
				name:      "replacement",
				eventType: "staging_superseded",
				discard: func(fixture *submissionArchiveFixture) error {
					_, err := fixture.store.CreateStagingRound(ctx, fixture.settings, GenerateRoundArgs{
						OpensAt:  fixture.clock,
						ClosesAt: fixture.clock.Add(time.Hour),
						RevealAt: fixture.clock.Add(time.Hour),
					}, true)
					return err
				},
			},
		} {
			fixture := newSubmissionArchiveFixture(t, ctx, "rerun-"+c.name)
			round := fixture.openStagingRound(t, ctx)
			job, _, err := fixture.store.Enqueue(
				ctx, fixture.settings, round.RoundId, fixture.patch(t, "rerun-"+c.name), "miner-a", testApiImageDigest(),
			)
			if err != nil {
				t.Fatalf("%s: enqueue: %v", c.name, err)
			}
			discardedJobId := job.JobId
			if c.name == "claim" {
				// A queued job submitted at the round's end is outside its window;
				// the claim discards it and claims the job submitted in time.
				discardedJobId = server.NewId()
				cacheKeyDigest := sha256.Sum256([]byte("synthetic-outside-window-" + discardedJobId.String()))
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(ctx, `
						INSERT INTO competition_job (
							job_id, round_id, patch_bytes, patch_sha256, cache_key, state,
							submitted_at, available_at, artifact_retain_until, api_image_digest
						)
						SELECT $1, round_id, patch_bytes, patch_sha256, $2, 'queued',
						       $3, $3, artifact_retain_until, api_image_digest
						FROM competition_job WHERE job_id = $4
					`, discardedJobId, hex.EncodeToString(cacheKeyDigest[:]), round.ClosesAt, job.JobId))
				}, server.OptReadWrite())
			}
			armCompetitionEventFault(ctx, "rerun_"+c.name, c.eventType)

			if err := c.discard(fixture); err != nil {
				t.Fatalf("%s: discard: %v", c.name, err)
			}
			var faultCount int64
			eventCount := 0
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					fmt.Sprintf(`SELECT last_value FROM synthetic_rerun_%s_commit`, c.name),
				).Scan(&faultCount))
				server.Raise(conn.QueryRow(ctx, `
					SELECT count(*) FROM competition_job_event
					WHERE job_id = $1 AND event_type = $2
				`, discardedJobId, c.eventType).Scan(&eventCount))
			})
			if faultCount < 2 {
				t.Errorf("%s: the discard wrote %d %s events in all, want a failed commit and its rerun", c.name, faultCount, c.eventType)
			}
			if eventCount != 1 {
				t.Errorf("%s: job %s has %d %s events, want 1", c.name, discardedJobId, eventCount, c.eventType)
			}
		}
	})
}
