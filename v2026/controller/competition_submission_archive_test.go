// Admission of competition submissions around their retained upload. The
// upload runs with no transaction or lock held, a submission is admitted only
// if its round still admits once the upload is done, an admission that reruns
// uploads once, and a failed or timed-out upload writes nothing; a round's
// workload upload is bounded too. The probe archive retains submissions
// through a local blob store and runs hooks inside the uploads, so a test acts
// while an upload is in progress instead of sleeping.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Retains submissions through a real local archive and counts their uploads.
// Each hook runs inside its upload, before anything is retained; an error from
// it fails the upload. Round workloads are not retained.
type submissionArchiveProbe struct {
	fakeArtifactArchive
	retained          *blobArtifactArchive
	uploadCount       atomic.Int32
	duringUpload      func(ctx context.Context, patch *CanonicalPatch) error
	duringRoundUpload func(ctx context.Context) error
}

// Runs the round hook in place of an upload.
func (self *submissionArchiveProbe) ArchiveRound(
	ctx context.Context,
	settings *Settings,
	round *roundRecord,
	workload workloadArtifact,
) error {
	if self.duringRoundUpload != nil {
		return self.duringRoundUpload(ctx)
	}
	return nil
}

// Runs the hook, then retains the patch.
func (self *submissionArchiveProbe) ArchiveSubmission(
	ctx context.Context,
	settings *Settings,
	roundId server.Id,
	patch *CanonicalPatch,
) (*retainedArtifact, error) {
	self.uploadCount.Add(1)
	if self.duringUpload != nil {
		if err := self.duringUpload(ctx, patch); err != nil {
			return nil, err
		}
	}
	return self.retained.ArchiveSubmission(ctx, settings, roundId, patch)
}

// A competition of its own with an empty FIFO, the probe as its archive and a
// clock the test sets. Every store call reads the clock, so a test moves
// time between a submission's receipt and its admission without sleeping.
// Hooks run database work inside an upload, so the store bounds uploads
// generously enough that the work never races the deadline on a loaded
// machine; the timeout test sets its own bound.
type submissionArchiveFixture struct {
	settings *Settings
	store    PostgresStore
	probe    *submissionArchiveProbe
	clock    time.Time
}

// Sets up the competition named by the suffix in the current test database.
func newSubmissionArchiveFixture(t testing.TB, ctx context.Context, name string) *submissionArchiveFixture {
	t.Helper()
	fixture := &submissionArchiveFixture{
		probe: &submissionArchiveProbe{
			retained: &blobArtifactArchive{
				store: server.NewLocalBlobStore(t.TempDir(), "competition").(server.RetainedBlobStore),
			},
		},
		clock: server.NowUtc().Truncate(time.Second),
	}
	fixture.settings = validSettings()
	fixture.settings.CompetitionId += "-" + name
	fixture.settings.ArtifactRoot = t.TempDir()
	fixture.settings.SeasonEndsAt = fixture.clock.Add(60 * 24 * time.Hour)
	fixture.settings.RetainUntil = fixture.settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
	fixture.settings.artifactArchive = fixture.probe
	fixture.store = PostgresStore{
		now:                      func() time.Time { return fixture.clock },
		submissionArchiveTimeout: time.Minute,
	}
	listKey, memberKey := competitionFifoKeys(fixture.settings)
	server.Redis(ctx, func(client server.RedisClient) {
		server.Raise(client.Del(ctx, listKey, memberKey).Err())
	})
	t.Cleanup(func() {
		server.Redis(context.Background(), func(client server.RedisClient) {
			_ = client.Del(context.Background(), listKey, memberKey).Err()
		})
	})
	return fixture
}

// Opens a staging round that admits from the fixture's clock for an hour.
func (self *submissionArchiveFixture) openStagingRound(t testing.TB, ctx context.Context) *roundRecord {
	t.Helper()
	round, err := self.store.CreateStagingRound(ctx, self.settings, GenerateRoundArgs{
		OpensAt: self.clock, ClosesAt: self.clock.Add(time.Hour), RevealAt: self.clock.Add(time.Hour),
	}, false)
	if err != nil {
		t.Fatalf("open staging round: %v", err)
	}
	return round
}

// Opens the first production epoch, admitting from the fixture's clock for an
// hour.
func (self *submissionArchiveFixture) openProductionRound(t testing.TB, ctx context.Context) *roundRecord {
	t.Helper()
	round, err := self.store.CreateRound(ctx, self.settings, GenerateRoundArgs{
		OpensAt: self.clock, ClosesAt: self.clock.Add(time.Hour), RevealAt: self.clock.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("open production round: %v", err)
	}
	return round
}

// Canonicalizes a synthetic one-line patch.
func (self *submissionArchiveFixture) patch(t testing.TB, value string) *CanonicalPatch {
	t.Helper()
	patch, patchErr := ValidateAndCanonicalizePatch(testPatch(value), self.settings.PatchPolicy)
	if patchErr != nil {
		t.Fatalf("canonicalize patch %q: %v", value, patchErr)
	}
	return patch
}

// Lists the evaluator FIFO signals in dispatch order.
func (self *submissionArchiveFixture) queuedSignals(t testing.TB, ctx context.Context) []string {
	t.Helper()
	listKey, _ := competitionFifoKeys(self.settings)
	var signals []string
	server.Redis(ctx, func(client server.RedisClient) {
		var err error
		signals, err = client.LRange(ctx, listKey, 0, -1).Result()
		server.Raise(err)
	})
	return signals
}

// Checks that the job's one submitted event names a retained object holding
// exactly the patch bytes, so the artifact was written before the job
// committed and the job records which one it is.
func (self *submissionArchiveFixture) requireRetainedSubmission(
	t testing.TB,
	ctx context.Context,
	jobId server.Id,
	patch *CanonicalPatch,
) {
	t.Helper()
	payloads := [][]byte{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT payload_json FROM competition_job_event
			WHERE job_id = $1 AND event_type = 'submitted'
		`, jobId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var payload []byte
				server.Raise(result.Scan(&payload))
				payloads = append(payloads, payload)
			}
		})
	})
	if len(payloads) != 1 {
		t.Fatalf("job %s has %d submitted events, want 1", jobId, len(payloads))
	}
	var submitted struct {
		SubmissionArtifact *retainedArtifact `json:"submission_artifact"`
	}
	if err := json.Unmarshal(payloads[0], &submitted); err != nil {
		t.Fatalf("decode submitted event: %v", err)
	}
	artifact := submitted.SubmissionArtifact
	if artifact == nil || artifact.Sha256 != patch.Sha256 || artifact.Bytes != int64(len(patch.Bytes)) {
		t.Fatalf("submitted event artifact = %+v, want the retained patch %s", artifact, patch.Sha256)
	}
	reader, err := self.probe.retained.store.GetVersion(ctx, artifact.Key, artifact.VersionId)
	if err != nil {
		t.Fatalf("read retained submission: %v", err)
	}
	retainedBytes, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(retainedBytes, patch.Bytes) {
		t.Fatalf("retained submission = %q, read=%v close=%v", retainedBytes, readErr, closeErr)
	}
}

// Lists the committed job ids for one patch in one round.
func submissionJobIds(ctx context.Context, roundId server.Id, patch *CanonicalPatch) []server.Id {
	jobIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT job_id FROM competition_job WHERE cache_key = $1 ORDER BY job_id`,
			cacheKey(roundId, patch.Bytes),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var jobId server.Id
				server.Raise(result.Scan(&jobId))
				jobIds = append(jobIds, jobId)
			}
		})
	})
	return jobIds
}

// Returns an error when the submission whose upload is running holds the
// global submit lock or the round row: another transaction must take both
// without waiting. A hook that admits, closes or finalizes checks this first,
// because it would otherwise wait on a submission that is waiting on the hook.
func checkUploadHoldsNoLock(ctx context.Context, roundId server.Id) error {
	submitLockFree := false
	roundRowFree := false
	err := captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			submitLockFree, roundRowFree = false, false
			server.Raise(tx.QueryRow(
				ctx,
				`SELECT pg_try_advisory_xact_lock(hashtextextended('competition-submit-v1', 0))`,
			).Scan(&submitLockFree))
			var lockedRoundId server.Id
			scanErr := tx.QueryRow(
				ctx,
				`SELECT round_id FROM competition_round WHERE round_id = $1 FOR UPDATE SKIP LOCKED`,
				roundId,
			).Scan(&lockedRoundId)
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return
			}
			server.Raise(scanErr)
			roundRowFree = true
		})
	})
	if err != nil {
		return err
	}
	if !submitLockFree || !roundRowFree {
		return fmt.Errorf(
			"the upload ran holding the submit lock (free %t) or the round row (free %t)",
			submitLockFree,
			roundRowFree,
		)
	}
	return nil
}

// A submission whose upload is still running holds neither the submit lock nor
// its round, so another submission is admitted start to finish meanwhile. The
// queue keeps receipt order, and both jobs record their retained artifacts.
func TestCompetitionSlowSubmissionUploadDoesNotBlockAnotherSubmission(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "slow-upload")
		round := fixture.openStagingRound(t, ctx)
		slowPatch := fixture.patch(t, "slow-upload")
		otherPatch := fixture.patch(t, "other-upload")
		receivedAt := fixture.clock

		var otherJob *queuedJob
		otherHit := false
		var otherErr error
		var otherCommittedJobIds []server.Id
		var slowCommittedJobIds []server.Id
		fixture.probe.duringUpload = func(uploadCtx context.Context, patch *CanonicalPatch) error {
			if patch.Sha256 != slowPatch.Sha256 {
				return nil
			}
			if err := checkUploadHoldsNoLock(uploadCtx, round.RoundId); err != nil {
				return err
			}
			// Another submission arrives a second later and is admitted start to
			// finish while this upload is still running.
			fixture.clock = receivedAt.Add(time.Second)
			otherJob, otherHit, otherErr = fixture.store.Enqueue(
				ctx, fixture.settings, round.RoundId, otherPatch, "miner-b", testApiImageDigest(),
			)
			otherCommittedJobIds = submissionJobIds(ctx, round.RoundId, otherPatch)
			slowCommittedJobIds = submissionJobIds(ctx, round.RoundId, slowPatch)
			return nil
		}
		slowJob, slowHit, slowErr := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, slowPatch, "miner-a", testApiImageDigest(),
		)
		if slowErr != nil || slowHit || slowJob == nil || slowJob.State != "queued" {
			t.Fatalf("slow submission = %#v, hit=%t, err=%v", slowJob, slowHit, slowErr)
		}
		if otherErr != nil || otherHit || otherJob == nil || otherJob.State != "queued" {
			t.Fatalf("submission during the slow upload = %#v, hit=%t, err=%v", otherJob, otherHit, otherErr)
		}
		if !slices.Equal(otherCommittedJobIds, []server.Id{otherJob.JobId}) || len(slowCommittedJobIds) != 0 {
			t.Fatalf(
				"during the slow upload committed jobs were other=%v slow=%v, want other=[%s] and no slow job yet",
				otherCommittedJobIds,
				slowCommittedJobIds,
				otherJob.JobId,
			)
		}
		if uploads := fixture.probe.uploadCount.Load(); uploads != 2 {
			t.Fatalf("uploaded %d times, want once per submission", uploads)
		}
		fixture.requireRetainedSubmission(t, ctx, slowJob.JobId, slowPatch)
		fixture.requireRetainedSubmission(t, ctx, otherJob.JobId, otherPatch)

		// The queue keeps receipt order: the slow submission was received first,
		// so it is evaluated first although the other one committed first.
		if !slowJob.SubmittedAt.Equal(receivedAt) || !otherJob.SubmittedAt.Equal(receivedAt.Add(time.Second)) {
			t.Fatalf(
				"submitted at slow=%s other=%s, want the receipt times %s and %s",
				slowJob.SubmittedAt,
				otherJob.SubmittedAt,
				receivedAt,
				receivedAt.Add(time.Second),
			)
		}
		wantSignals := []string{otherJob.JobId.String(), slowJob.JobId.String()}
		if signals := fixture.queuedSignals(t, ctx); !slices.Equal(signals, wantSignals) {
			t.Fatalf("FIFO signals = %v, want %v in commit order", signals, wantSignals)
		}
		claimed, err := fixture.store.Claim(ctx, fixture.settings, "worker-a", testWorkerImageDigest())
		if err != nil || claimed == nil || claimed.JobId != slowJob.JobId {
			t.Fatalf("first claim = %#v, %v, want the slow submission %s", claimed, err, slowJob.JobId)
		}
	})
}

// An identical submission that commits while the first one uploads leaves one
// job: the first submission ends as its cache hit instead of a second job or a
// unique violation.
func TestCompetitionIdenticalSubmissionDuringUploadEndsAsCacheHit(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "identical-upload")
		round := fixture.openStagingRound(t, ctx)
		patch := fixture.patch(t, "identical-upload")

		var identicalJob *queuedJob
		identicalHit := false
		var identicalErr error
		uploadHooked := false
		fixture.probe.duringUpload = func(uploadCtx context.Context, _ *CanonicalPatch) error {
			if uploadHooked {
				return nil
			}
			uploadHooked = true
			if err := checkUploadHoldsNoLock(uploadCtx, round.RoundId); err != nil {
				return err
			}
			identicalJob, identicalHit, identicalErr = fixture.store.Enqueue(
				ctx, fixture.settings, round.RoundId, patch, "miner-b", testApiImageDigest(),
			)
			return nil
		}
		job, hit, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
		)
		if err != nil {
			t.Fatalf("submission whose upload admitted an identical one: %v", err)
		}
		if identicalErr != nil || identicalHit || identicalJob == nil {
			t.Fatalf("identical submission during the upload = %#v, hit=%t, err=%v", identicalJob, identicalHit, identicalErr)
		}
		// The identical submission committed first, so this one joins its job.
		if !hit || job == nil || job.JobId != identicalJob.JobId {
			t.Fatalf("submission after its upload = %#v, hit=%t, want a cache hit on %s", job, hit, identicalJob.JobId)
		}
		if jobIds := submissionJobIds(ctx, round.RoundId, patch); !slices.Equal(jobIds, []server.Id{job.JobId}) {
			t.Fatalf("committed jobs = %v, want only %s", jobIds, job.JobId)
		}
		// Both uploaded the same content-addressed object; only the committed
		// job's proof is recorded.
		if uploads := fixture.probe.uploadCount.Load(); uploads != 2 {
			t.Fatalf("uploaded %d times, want 2", uploads)
		}
		fixture.requireRetainedSubmission(t, ctx, job.JobId, patch)
		for _, principalId := range []string{"miner-a", "miner-b"} {
			if _, err := fixture.store.GetJob(ctx, fixture.settings, job.JobId, &Principal{Id: principalId, Role: "submitter"}); err != nil {
				t.Fatalf("principal %s cannot poll the shared job: %v", principalId, err)
			}
		}
		if signals := fixture.queuedSignals(t, ctx); !slices.Equal(signals, []string{job.JobId.String()}) {
			t.Fatalf("FIFO signals = %v, want one for %s", signals, job.JobId)
		}
	})
}

// A submission received while its round admitted is refused when the round
// stops admitting during its upload: staging admission closed, the staging
// round replaced, or the production round finalized. Nothing is queued; the
// upload leaves only its retained object.
func TestCompetitionSubmissionRefusedWhenRoundStopsAdmittingDuringUpload(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			name       string
			production bool
			stop       func(fixture *submissionArchiveFixture, round *roundRecord) error
		}{
			{
				name: "staging-closed",
				stop: func(fixture *submissionArchiveFixture, round *roundRecord) error {
					closed, err := fixture.store.CloseStagingRound(ctx, fixture.settings)
					if err == nil && closed.AdmissionClosedAt == nil {
						err = errors.New("staging admission did not close")
					}
					return err
				},
			},
			{
				name: "staging-replaced",
				stop: func(fixture *submissionArchiveFixture, round *roundRecord) error {
					_, err := fixture.store.CreateStagingRound(ctx, fixture.settings, GenerateRoundArgs{
						OpensAt:  fixture.clock,
						ClosesAt: fixture.clock.Add(time.Hour),
						RevealAt: fixture.clock.Add(time.Hour),
					}, true)
					return err
				},
			},
			{
				// Production finalization does not close admission; only the
				// finalized round itself refuses the late admission.
				name:       "production-finalized",
				production: true,
				stop: func(fixture *submissionArchiveFixture, round *roundRecord) error {
					fixture.clock = round.ClosesAt
					state, err := fixture.store.PrepareCandidateReview(ctx, fixture.settings, round.Epoch)
					if err == nil && state.Status != "finalized" {
						err = fmt.Errorf("review state = %q, want finalized", state.Status)
					}
					return err
				},
			},
		} {
			fixture := newSubmissionArchiveFixture(t, ctx, "stops-"+c.name)
			var round *roundRecord
			if c.production {
				round = fixture.openProductionRound(t, ctx)
			} else {
				round = fixture.openStagingRound(t, ctx)
			}
			patch := fixture.patch(t, "stops-"+c.name)
			var stopErr error
			fixture.probe.duringUpload = func(uploadCtx context.Context, _ *CanonicalPatch) error {
				if err := checkUploadHoldsNoLock(uploadCtx, round.RoundId); err != nil {
					return err
				}
				stopErr = c.stop(fixture, round)
				return nil
			}
			job, hit, err := fixture.store.Enqueue(
				ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
			)
			if stopErr != nil {
				t.Fatalf("%s: stop admission during the upload: %v", c.name, stopErr)
			}
			if !errors.Is(err, ErrRoundClosed) || job != nil || hit {
				t.Fatalf("%s: submission = %#v, hit=%t, err=%v, want ErrRoundClosed", c.name, job, hit, err)
			}
			if jobIds := submissionJobIds(ctx, round.RoundId, patch); len(jobIds) != 0 {
				t.Fatalf("%s: admitted jobs %v into a round that stopped admitting", c.name, jobIds)
			}
			if signals := fixture.queuedSignals(t, ctx); len(signals) != 0 {
				t.Fatalf("%s: FIFO signals = %v, want none", c.name, signals)
			}
			// The upload happened; what it leaves is a retention-bounded object
			// under the content-addressed key, which no job names.
			if uploads := fixture.probe.uploadCount.Load(); uploads != 1 {
				t.Fatalf("%s: uploaded %d times, want 1", c.name, uploads)
			}
			orphanKey := fixture.probe.retained.submissionPatchKey(fixture.settings, round.RoundId, patch.Sha256)
			orphan, err := fixture.probe.retained.store.Get(ctx, orphanKey)
			if err != nil {
				t.Fatalf("%s: retained upload of the refused submission: %v", c.name, err)
			}
			orphan.Close()
		}
	})
}

// An admission whose commit fails with a serialization failure reruns without
// uploading again, and commits exactly one job.
func TestCompetitionSubmissionUploadRunsOnceWhenAdmissionReruns(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "admission-rerun")
		round := fixture.openStagingRound(t, ctx)
		patch := fixture.patch(t, "admission-rerun")
		// The first commit that inserts a job fails with a serialization
		// failure, so server.Tx reruns that admission.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE SEQUENCE synthetic_competition_admission_commit;
				CREATE FUNCTION synthetic_competition_admission_fault() RETURNS trigger AS $fault$
				BEGIN
					IF nextval('synthetic_competition_admission_commit') = 1 THEN
						RAISE EXCEPTION 'synthetic serialization failure at commit' USING ERRCODE = '40001';
					END IF;
					RETURN NULL;
				END
				$fault$ LANGUAGE plpgsql;
				CREATE CONSTRAINT TRIGGER synthetic_competition_admission_fault
					AFTER INSERT ON competition_job
					DEFERRABLE INITIALLY DEFERRED
					FOR EACH ROW EXECUTE FUNCTION synthetic_competition_admission_fault();
			`))
		})

		job, hit, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
		)
		if err != nil || hit || job == nil || job.State != "queued" {
			t.Fatalf("submission = %#v, hit=%t, err=%v", job, hit, err)
		}
		var commitCount int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT last_value FROM synthetic_competition_admission_commit`,
			).Scan(&commitCount))
		})
		if commitCount != 2 {
			t.Fatalf("admission tried %d commits, want the failed one and its rerun", commitCount)
		}
		if uploads := fixture.probe.uploadCount.Load(); uploads != 1 {
			t.Fatalf("uploaded %d times across the rerun, want 1", uploads)
		}
		if jobIds := submissionJobIds(ctx, round.RoundId, patch); !slices.Equal(jobIds, []server.Id{job.JobId}) {
			t.Fatalf("committed jobs = %v, want only the returned %s", jobIds, job.JobId)
		}
		fixture.requireRetainedSubmission(t, ctx, job.JobId, patch)
	})
}

// The upload carries its own deadline, the production bound unless the store
// sets one, even when the caller has none. A hung upload ends at that deadline
// with nothing written, and a resubmission is admitted.
func TestCompetitionSubmissionUploadIsBoundedByTimeout(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "hung-upload")
		round := fixture.openStagingRound(t, ctx)
		patch := fixture.patch(t, "hung-upload")

		fixture.store.submissionArchiveTimeout = 0
		var defaultBound time.Duration
		fixture.probe.duringUpload = func(uploadCtx context.Context, _ *CanonicalPatch) error {
			enteredAt := time.Now()
			deadline, bounded := uploadCtx.Deadline()
			if !bounded {
				return errors.New("upload deadline is unbounded")
			}
			defaultBound = deadline.Sub(enteredAt)
			return errors.New("synthetic object store failure")
		}
		if _, _, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
		); err == nil || defaultBound <= 0 || defaultSubmissionArchiveTimeout < defaultBound {
			t.Fatalf("upload bound = %s (err %v), want at most the default %s", defaultBound, err, defaultSubmissionArchiveTimeout)
		}

		archiveTimeout := 200 * time.Millisecond
		fixture.store.submissionArchiveTimeout = archiveTimeout
		fixture.probe.duringUpload = func(uploadCtx context.Context, _ *CanonicalPatch) error {
			enteredAt := time.Now()
			deadline, bounded := uploadCtx.Deadline()
			if !bounded || enteredAt.Add(archiveTimeout).Before(deadline) {
				return fmt.Errorf("upload deadline = %s (bounded %t), want at most %s from its start", deadline, bounded, archiveTimeout)
			}
			// A hung object store: only the deadline ends the upload.
			<-uploadCtx.Done()
			return uploadCtx.Err()
		}
		job, hit, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
		)
		if !errors.Is(err, context.DeadlineExceeded) || job != nil || hit {
			t.Fatalf("submission with a hung upload = %#v, hit=%t, err=%v, want the upload deadline", job, hit, err)
		}
		if jobIds := submissionJobIds(ctx, round.RoundId, patch); len(jobIds) != 0 {
			t.Fatalf("a timed-out upload committed jobs %v", jobIds)
		}
		if signals := fixture.queuedSignals(t, ctx); len(signals) != 0 {
			t.Fatalf("FIFO signals = %v after a timed-out upload, want none", signals)
		}

		// Once the store answers again, the resubmission uploads again and is
		// admitted as a new job.
		fixture.probe.duringUpload = nil
		job, hit, err = fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
		)
		if err != nil || hit || job == nil || job.State != "queued" {
			t.Fatalf("resubmission = %#v, hit=%t, err=%v", job, hit, err)
		}
		if uploads := fixture.probe.uploadCount.Load(); uploads != 3 {
			t.Fatalf("uploaded %d times, want the failed, the timed-out and the resubmitted upload", uploads)
		}
		fixture.requireRetainedSubmission(t, ctx, job.JobId, patch)
	})
}

// A failed upload refuses the submission as a retriable 503 enqueue_failed with
// nothing written or queued, and a resubmission uploads again as a new job.
func TestCompetitionFailedSubmissionUploadRefusesWithNothingWritten(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "failed-upload")
		round := fixture.openStagingRound(t, ctx)
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		principal := &Principal{Id: "miner-a", Role: "submitter"}
		args := ScoreArgs{RoundId: round.RoundId, Patch: testPatch("failed-upload")}
		patch := fixture.patch(t, "failed-upload")
		fixture.probe.duringUpload = func(context.Context, *CanonicalPatch) error {
			return errors.New("synthetic object store failure")
		}

		accepted, status, evalError := service.Submit(ctx, args, principal)
		if accepted != nil || status != http.StatusServiceUnavailable || evalError == nil ||
			evalError.Code != "enqueue_failed" || !evalError.Retriable {
			t.Fatalf("submission with a failed upload = %#v, %d, %#v, want a retriable 503 enqueue_failed", accepted, status, evalError)
		}
		if jobIds := submissionJobIds(ctx, round.RoundId, patch); len(jobIds) != 0 {
			t.Fatalf("a failed upload committed jobs %v", jobIds)
		}
		if signals := fixture.queuedSignals(t, ctx); len(signals) != 0 {
			t.Fatalf("FIFO signals = %v after a failed upload, want none", signals)
		}

		fixture.probe.duringUpload = nil
		accepted, status, evalError = service.Submit(ctx, args, principal)
		if evalError != nil || status != http.StatusAccepted || accepted == nil || accepted.CacheHit {
			t.Fatalf("resubmission = %#v, %d, %#v, want a new job", accepted, status, evalError)
		}
		if uploads := fixture.probe.uploadCount.Load(); uploads != 2 {
			t.Fatalf("uploaded %d times, want the failed upload and the resubmission", uploads)
		}
		fixture.requireRetainedSubmission(t, ctx, accepted.JobId, patch)
	})
}

// Only a submission that needs a new job uploads: cache hits, unknown rounds and
// closed rounds are decided before any upload.
func TestCompetitionSubmissionUploadsOnlyForNewJobs(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "upload-new-jobs")
		round := fixture.openStagingRound(t, ctx)
		patch := fixture.patch(t, "upload-new-jobs")
		requireUploads := func(step string, want int32) {
			t.Helper()
			if uploads := fixture.probe.uploadCount.Load(); uploads != want {
				t.Fatalf("after %s uploaded %d times, want %d", step, uploads, want)
			}
		}

		job, hit, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-a", testApiImageDigest(),
		)
		if err != nil || hit || job == nil {
			t.Fatalf("first submission = %#v, hit=%t, err=%v", job, hit, err)
		}
		requireUploads("a new job", 1)

		cached, hit, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, patch, "miner-b", testApiImageDigest(),
		)
		if err != nil || !hit || cached == nil || cached.JobId != job.JobId {
			t.Fatalf("identical submission = %#v, hit=%t, err=%v, want a cache hit on %s", cached, hit, err, job.JobId)
		}
		requireUploads("a cache hit", 1)

		if _, _, err := fixture.store.Enqueue(
			ctx, fixture.settings, server.NewId(), patch, "miner-a", testApiImageDigest(),
		); !errors.Is(err, ErrNotFound) {
			t.Fatalf("submission to an unknown round error = %v, want ErrNotFound", err)
		}
		requireUploads("an unknown round", 1)

		if _, err := fixture.store.CloseStagingRound(ctx, fixture.settings); err != nil {
			t.Fatalf("close staging admission: %v", err)
		}
		if _, _, err := fixture.store.Enqueue(
			ctx, fixture.settings, round.RoundId, fixture.patch(t, "after-close"), "miner-a", testApiImageDigest(),
		); !errors.Is(err, ErrRoundClosed) {
			t.Fatalf("submission to a closed round error = %v, want ErrRoundClosed", err)
		}
		requireUploads("a closed round", 1)
	})
}

// A round's workload upload carries its own deadline, the same kind of bound
// as a submission upload, even when the operator's request has none; a failed
// upload refuses the round with nothing written.
func TestCompetitionRoundUploadIsBoundedByTimeout(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "round-upload")
		var uploadBound time.Duration
		fixture.probe.duringRoundUpload = func(uploadCtx context.Context) error {
			enteredAt := time.Now()
			deadline, bounded := uploadCtx.Deadline()
			if !bounded {
				return errors.New("round upload deadline is unbounded")
			}
			uploadBound = deadline.Sub(enteredAt)
			return nil
		}
		round := fixture.openStagingRound(t, ctx)
		if uploadBound <= 0 || defaultRoundArchiveTimeout < uploadBound {
			t.Fatalf("round upload bound = %s, want at most %s", uploadBound, defaultRoundArchiveTimeout)
		}

		fixture.probe.duringRoundUpload = func(context.Context) error {
			return errors.New("synthetic object store failure")
		}
		if replacement, err := fixture.store.CreateStagingRound(ctx, fixture.settings, GenerateRoundArgs{
			OpensAt:  fixture.clock,
			ClosesAt: fixture.clock.Add(time.Hour),
			RevealAt: fixture.clock.Add(time.Hour),
		}, true); err == nil {
			t.Fatalf("round created with a failed workload upload: %#v", replacement)
		}
		current, err := fixture.store.CurrentStagingRound(ctx, fixture.settings)
		if err != nil || current == nil || current.RoundId != round.RoundId || current.Canceled {
			t.Fatalf("current staging round after a failed upload = %#v, %v, want %s unchanged", current, err, round.RoundId)
		}
	})
}
