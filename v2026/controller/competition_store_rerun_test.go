// Competition store transactions that server.Tx reruns report and count only
// what the committed run did. A deferred trigger fails the first commit that
// writes a chosen row with a serialization failure, so server.Tx reruns that
// callback. Where a competing writer matters, a hook attached with
// server.Testing_WithTxRerunHook commits the competing change exactly between
// the two attempts, so the rerun meets a world the first attempt did not see.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
)

// Fails the first commit after arming whose transaction wrote a row of the
// table, by the operation, that matches the condition.
func armCompetitionCommitFault(ctx context.Context, name string, table string, operation string, condition string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE SEQUENCE synthetic_%[1]s_commit;
			CREATE FUNCTION synthetic_%[1]s_fault() RETURNS trigger AS $fault$
			BEGIN
				IF nextval('synthetic_%[1]s_commit') = 1 THEN
					RAISE EXCEPTION 'synthetic serialization failure at commit' USING ERRCODE = '40001';
				END IF;
				RETURN NULL;
			END
			$fault$ LANGUAGE plpgsql;
			CREATE CONSTRAINT TRIGGER synthetic_%[1]s_fault
				AFTER %[3]s ON %[2]s
				DEFERRABLE INITIALLY DEFERRED
				FOR EACH ROW WHEN (%[4]s)
				EXECUTE FUNCTION synthetic_%[1]s_fault();
		`, name, table, operation, condition)))
	})
}

// Counts the rows the armed fault saw, which includes the commit it failed.
func competitionCommitFaultCount(ctx context.Context, name string) (count int64) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT last_value FROM synthetic_%s_commit`, name)).Scan(&count))
	})
	return
}

// The value the round events counter holds for the label.
func roundEventCount(label string) float64 {
	return testutil.ToFloat64(competitionRoundEvents.WithLabelValues(label))
}

// A competing change committed once, at the first rerun of a transaction made
// with its context.
type rerunCompetitor struct {
	compete func() error
	ran     bool
	err     error
}

// Returns a context whose transactions run the competing change between their
// first two attempts.
func (self *rerunCompetitor) context(ctx context.Context) context.Context {
	return server.Testing_WithTxRerunHook(ctx, func() {
		if self.ran {
			return
		}
		self.ran = true
		self.err = self.compete()
	})
}

// Fails the test unless the competing change ran between two attempts and
// committed.
func (self *rerunCompetitor) require(t testing.TB) {
	t.Helper()
	if !self.ran {
		t.Fatal("the competing change never ran: no rerun of the transaction under test called the hook")
	}
	if self.err != nil {
		t.Fatalf("competing change: %v", self.err)
	}
}

// A discard whose transaction reruns records one event per discarded job:
// the claim's outside-window discard, the staging discard when epoch one
// commits, and the staging supersede when the operator replaces the round.
func TestCompetitionDiscardRecordsOneEventWhenItsTransactionReruns(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
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
			faultName := "rerun_" + c.name
			armCompetitionCommitFault(
				ctx, faultName, "competition_job_event", "INSERT",
				fmt.Sprintf("NEW.event_type = '%s'", c.eventType),
			)

			if err := c.discard(fixture); err != nil {
				t.Fatalf("%s: discard: %v", c.name, err)
			}
			eventCount := 0
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `
					SELECT count(*) FROM competition_job_event
					WHERE job_id = $1 AND event_type = $2
				`, discardedJobId, c.eventType).Scan(&eventCount))
			})
			if faultCount := competitionCommitFaultCount(ctx, faultName); faultCount < 2 {
				t.Errorf("%s: the discard wrote %d %s events in all, want a failed commit and its rerun", c.name, faultCount, c.eventType)
			}
			if eventCount != 1 {
				t.Errorf("%s: job %s has %d %s events, want 1", c.name, discardedJobId, eventCount, c.eventType)
			}
		}
	})
}

// A claim whose first attempt rolls back, and whose rerun finds that another
// worker claimed the job in between, returns no job: the job its rolled-back
// attempt picked was never its own.
func TestCompetitionClaimRerunReturnsOnlyAJobItClaimed(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "claim-rerun")
		round := fixture.openStagingRound(t, ctx)
		queued, _, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, fixture.patch(t, "claim-rerun"), "miner-a", testApiImageDigest(),
		)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		armCompetitionCommitFault(
			ctx, "claim_rerun", "competition_job_event", "INSERT",
			"NEW.event_type = 'claimed' AND NEW.actor_id = 'worker-a'",
		)
		competitor := &rerunCompetitor{compete: func() error {
			job, err := fixture.store.Claim(ctx, fixture.settings, "worker-b", testWorkerImageDigest())
			if err == nil && (job == nil || job.JobId != queued.JobId) {
				err = fmt.Errorf("the other worker claimed %#v, want %s", job, queued.JobId)
			}
			return err
		}}

		claimed, err := fixture.store.Claim(competitor.context(ctx), fixture.settings, "worker-a", testWorkerImageDigest())
		competitor.require(t)
		if err != nil {
			t.Fatalf("claim whose rerun found the slot taken: %v", err)
		}
		if claimed != nil {
			t.Fatalf("claim whose rerun found the slot taken returned job %s, want no job", claimed.JobId)
		}
		var slotWorker *string
		var leaseOwner *string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT worker_id FROM competition_worker_slot WHERE slot_id = 1`).Scan(&slotWorker))
			server.Raise(conn.QueryRow(ctx, `SELECT lease_owner FROM competition_job WHERE job_id = $1`, queued.JobId).Scan(&leaseOwner))
		})
		if slotWorker == nil || *slotWorker != "worker-b" || leaseOwner == nil || *leaseOwner != "worker-b" {
			t.Fatalf("slot worker = %v, job lease owner = %v, want the other worker for both", slotWorker, leaseOwner)
		}
	})
}

// A staging replacement whose first attempt rolls back, and whose rerun finds
// the current round finalized in between, creates the next round without
// replacing anything, so it counts no replacement.
func TestCompetitionStagingReplacementRerunCountsOnlyItsOwnReplacement(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "replace-rerun")
		current := fixture.openStagingRound(t, ctx)
		armCompetitionCommitFault(ctx, "replace_rerun", "competition_round", "INSERT", "true")
		labels := []string{"staging_created", "staging_replaced", "staging_closed", "staging_finalized"}
		countsBefore := map[string]float64{}
		for _, label := range labels {
			countsBefore[label] = roundEventCount(label)
		}
		competitor := &rerunCompetitor{compete: func() error {
			if _, err := fixture.store.CloseStagingRound(ctx, fixture.settings); err != nil {
				return err
			}
			finalized, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, current.Epoch)
			if err == nil && finalized.FinalizedAt == nil {
				err = errors.New("the current staging round did not finalize")
			}
			return err
		}}

		next, err := fixture.store.CreateStagingRound(competitor.context(ctx), fixture.settings, GenerateRoundArgs{
			OpensAt:  fixture.clock,
			ClosesAt: fixture.clock.Add(time.Hour),
			RevealAt: fixture.clock.Add(time.Hour),
		}, true)
		competitor.require(t)
		if err != nil || next == nil || next.Epoch != current.Epoch+1 || !next.Staging {
			t.Fatalf("staging round after the rerun = %#v, %v, want epoch %d", next, err, current.Epoch+1)
		}
		previous, err := fixture.store.GetRound(ctx, fixture.settings, current.RoundId)
		if err != nil || previous.Canceled || previous.FinalizedAt == nil {
			t.Fatalf("previous staging round = %#v, %v, want it finalized and not replaced", previous, err)
		}
		for label, want := range map[string]float64{
			"staging_created":   1,
			"staging_replaced":  0,
			"staging_closed":    1,
			"staging_finalized": 1,
		} {
			if counted := roundEventCount(label) - countsBefore[label]; counted != want {
				t.Errorf("%s counted %v times, want %v", label, counted, want)
			}
		}
	})
}

// A round step whose first attempt rolls back, and whose rerun finds the same
// step already taken by another caller in between, is counted once, by the
// caller whose change committed: closing staging admission, finalizing a
// staging round, and finalizing a production round with no candidate.
func TestCompetitionRoundStepRerunCountsTheStepOnce(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			name       string
			label      string
			production bool
			prepare    func(fixture *submissionArchiveFixture, round *roundRecord) error
			step       func(ctx context.Context, fixture *submissionArchiveFixture, round *roundRecord) error
		}{
			{
				name:  "staging_close",
				label: "staging_closed",
				step: func(ctx context.Context, fixture *submissionArchiveFixture, round *roundRecord) error {
					closed, err := fixture.store.CloseStagingRound(ctx, fixture.settings)
					if err == nil && closed.AdmissionClosedAt == nil {
						err = errors.New("staging admission did not close")
					}
					return err
				},
			},
			{
				name:  "staging_finalize",
				label: "staging_finalized",
				prepare: func(fixture *submissionArchiveFixture, round *roundRecord) error {
					_, err := fixture.store.CloseStagingRound(ctx, fixture.settings)
					return err
				},
				step: func(ctx context.Context, fixture *submissionArchiveFixture, round *roundRecord) error {
					finalized, err := fixture.store.FinalizeStagingRound(ctx, fixture.settings, round.Epoch)
					if err == nil && finalized.FinalizedAt == nil {
						err = errors.New("the staging round did not finalize")
					}
					return err
				},
			},
			{
				name:       "production_finalize",
				label:      "finalized",
				production: true,
				prepare: func(fixture *submissionArchiveFixture, round *roundRecord) error {
					fixture.clock = round.ClosesAt
					return nil
				},
				step: func(ctx context.Context, fixture *submissionArchiveFixture, round *roundRecord) error {
					state, err := fixture.store.PrepareCandidateReview(ctx, fixture.settings, round.Epoch)
					if err == nil && state.Status != "finalized" {
						err = fmt.Errorf("review state = %q, want finalized", state.Status)
					}
					return err
				},
			},
		} {
			fixture := newSubmissionArchiveFixture(t, ctx, "step-"+c.name)
			var round *roundRecord
			if c.production {
				round = fixture.openProductionRound(t, ctx)
			} else {
				round = fixture.openStagingRound(t, ctx)
			}
			if c.prepare != nil {
				if err := c.prepare(fixture, round); err != nil {
					t.Fatalf("%s: prepare: %v", c.name, err)
				}
			}
			armCompetitionCommitFault(ctx, "step_"+c.name, "competition_round", "UPDATE", "true")
			countBefore := roundEventCount(c.label)
			competitor := &rerunCompetitor{compete: func() error {
				return c.step(ctx, fixture, round)
			}}

			if err := c.step(competitor.context(ctx), fixture, round); err != nil {
				t.Fatalf("%s: step whose rerun found it taken: %v", c.name, err)
			}
			competitor.require(t)
			if counted := roundEventCount(c.label) - countBefore; counted != 1 {
				t.Errorf("%s: %s counted %v times, want 1", c.name, c.label, counted)
			}
		}
	})
}

// An evaluation result whose first attempt rolls back, and whose rerun finds
// that another worker took over the expired lease in between, reports the lost
// lease and no retry: the retry the rolled-back attempt scheduled never
// happened.
func TestCompetitionCompleteRerunReportsNoRetryAfterTheLeaseMoved(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "complete-rerun")
		round := fixture.openStagingRound(t, ctx)
		if _, _, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, fixture.patch(t, "complete-rerun"), "miner-a", testApiImageDigest(),
		); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		claimed, err := fixture.store.Claim(ctx, fixture.settings, "worker-a", testWorkerImageDigest())
		if err != nil || claimed == nil {
			t.Fatalf("claim = %#v, %v", claimed, err)
		}
		armCompetitionCommitFault(
			ctx, "complete_rerun", "competition_job_event", "INSERT",
			"NEW.event_type = 'infrastructure_retry'",
		)
		// The other worker's clock is past worker-a's lease.
		leaseExpired := fixture.clock.Add(time.Duration(fixture.settings.WorkerLeaseSeconds+1) * time.Second)
		competitorStore := fixture.store
		competitorStore.now = func() time.Time { return leaseExpired }
		competitor := &rerunCompetitor{compete: func() error {
			job, err := competitorStore.Claim(ctx, fixture.settings, "worker-b", testWorkerImageDigest())
			if err == nil && (job == nil || job.JobId != claimed.JobId) {
				err = fmt.Errorf("the other worker claimed %#v, want %s", job, claimed.JobId)
			}
			return err
		}}

		retry, err := fixture.store.Complete(competitor.context(ctx), fixture.settings, "worker-a", claimed.JobId, EvaluationOutcome{
			Error: infrastructureError("synthetic_infrastructure_failure", "synthetic infrastructure failure"),
		})
		competitor.require(t)
		if !errors.Is(err, ErrLeaseLost) || retry {
			t.Fatalf("result after the lease moved = retry %t, %v, want no retry and ErrLeaseLost", retry, err)
		}
		var leaseOwner *string
		retryEventCount := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT lease_owner FROM competition_job WHERE job_id = $1`, claimed.JobId).Scan(&leaseOwner))
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM competition_job_event
				WHERE job_id = $1 AND event_type = 'infrastructure_retry'
			`, claimed.JobId).Scan(&retryEventCount))
		})
		if leaseOwner == nil || *leaseOwner != "worker-b" || retryEventCount != 0 {
			t.Fatalf("job lease owner = %v with %d retry events, want the other worker and none", leaseOwner, retryEventCount)
		}
	})
}

// A heartbeat whose first attempt rolls back after finding the job no longer
// leased to its worker, and whose rerun finds the lease back, reports the
// lease its committed attempt extended. Production keeps the slot and the
// job's lease together; this test parts them by hand to reach the branch.
func TestCompetitionHeartbeatRerunReportsTheCommittedLease(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "heartbeat-rerun")
		round := fixture.openStagingRound(t, ctx)
		if _, _, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, fixture.patch(t, "heartbeat-rerun"), "miner-a", testApiImageDigest(),
		); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		claimed, err := fixture.store.Claim(ctx, fixture.settings, "worker-a", testWorkerImageDigest())
		if err != nil || claimed == nil {
			t.Fatalf("claim = %#v, %v", claimed, err)
		}
		setLeaseOwner := func(leaseOwner string) error {
			return captureDatabaseError(func() {
				server.Db(ctx, func(conn server.PgConn) {
					server.RaisePgResult(conn.Exec(
						ctx,
						`UPDATE competition_job SET lease_owner = $2 WHERE job_id = $1`,
						claimed.JobId,
						leaseOwner,
					))
				}, server.OptReadWrite())
			})
		}
		if err := setLeaseOwner("worker-z"); err != nil {
			t.Fatalf("part the job's lease from the slot: %v", err)
		}
		armCompetitionCommitFault(ctx, "heartbeat_rerun", "competition_worker_slot", "UPDATE", "true")
		competitor := &rerunCompetitor{compete: func() error {
			return setLeaseOwner("worker-a")
		}}

		fixture.clock = fixture.clock.Add(time.Minute)
		err = fixture.store.Heartbeat(competitor.context(ctx), fixture.settings, "worker-a", claimed.JobId)
		competitor.require(t)
		if err != nil {
			t.Fatalf("heartbeat whose rerun found the lease = %v, want the lease extended", err)
		}
		var leaseExpiresAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT lease_expires_at FROM competition_job WHERE job_id = $1`,
				claimed.JobId,
			).Scan(&leaseExpiresAt))
		})
		wantLease := fixture.clock.Add(time.Duration(fixture.settings.WorkerLeaseSeconds) * time.Second)
		if !leaseExpiresAt.Equal(wantLease) {
			t.Fatalf("job lease expires at %s, want %s from the committed heartbeat", leaseExpiresAt, wantLease)
		}
	})
}
