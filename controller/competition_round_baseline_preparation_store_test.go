// The native database guard must see a genuine running job in the same
// transaction that publishes a prepared baseline; no fake job/FK bypass exists.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Exercises the actual existing FK/trigger and append-only event table. The
// producer and retained-object proof remain deterministic synthetic fixtures.
func TestCompetitionRoundBaselinePreparationTransactionStartsRealJobAndSatisfiesFk(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := validSettings()
		settings.CompetitionId += "-baseline-preparation-transaction"
		settings.EvaluationPolicy.Replicates = 9
		settings.ArtifactRoot = t.TempDir()
		settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		now := server.NowUtc().Truncate(time.Second)
		settings.SeasonEndsAt = now.Add(60 * 24 * time.Hour)
		settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
		store := PostgresStore{now: func() time.Time { return now }}
		round, err := store.CreateStagingRound(ctx, settings, GenerateRoundArgs{OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), RevealAt: now.Add(time.Hour)}, false)
		if err != nil {
			t.Fatal(err)
		}
		patch, patchErr := ValidateAndCanonicalizePatch(testPatch("prepared-baseline"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		queued, hit, err := store.Enqueue(ctx, settings, round.RoundId, patch, "synthetic-miner", testApiImageDigest())
		if err != nil || hit {
			t.Fatal("enqueue", err, hit)
		}
		reservation, err := store.ReserveRoundBaseline(ctx, settings, "synthetic-worker", testWorkerImageDigest())
		if err != nil || reservation == nil || reservation.JobId != queued.JobId {
			t.Fatal("reserve", err, reservation)
		}
		if reservation.SourceJob.StartedAt != nil || reservation.SourceJob.State != "queued" || reservation.SourceJob.AttemptCount != 0 {
			t.Fatal("reservation consumed a candidate attempt")
		}
		if job, err := store.Claim(ctx, settings, "different-worker", testWorkerImageDigest()); err != nil || job != nil {
			t.Fatal("ordinary worker stole preparation's singleton", err)
		}
		prepared := preparedRoundBaselineTest(t, settings, reservation)
		// The unmodified native trigger rejects a baseline whose source is queued.
		beforeStartErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round_baseline
 (round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,
 base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at)
 VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,$9,$10,$11,$12)`, round.RoundId, prepared.BaselineJson, prepared.BaselineSha256, queued.JobId, prepared.ArtifactManifestSha256,
					settings.BaseSha, round.ProvidersSha256, settings.EvaluatorImageDigest, ScorerVersion, settings.EvaluationPolicy.HardwareId, settings.EvaluationPolicy.HostQualificationSha256, now))
			}, server.OptReadWrite())
		})
		if beforeStartErr == nil || !strings.Contains(beforeStartErr.Error(), "round attempt") {
			t.Fatalf("queued source escaped native baseline guard: %v", beforeStartErr)
		}
		original := now
		now = now.Add(2 * time.Hour) // Admission has closed, but admitted FIFO still grades.
		job, err := store.ClaimPreparedRoundBaseline(ctx, settings, reservation, prepared)
		if err != nil || job == nil || job.JobId != queued.JobId || job.State != "running" || job.StartedAt == nil || !job.StartedAt.Equal(now) || job.AttemptCount != 1 || job.LeaseOwner != "synthetic-worker" || job.RoundBaseline == nil {
			t.Fatalf("atomic real claim/baseline failed: job=%#v err=%v", job, err)
		}
		if !reservation.StartedAt.Equal(original) || !reservation.Deadline.Equal(original.Add(3*time.Hour)) || job.RoundBaseline.SourceJobId != job.JobId || job.RoundBaseline.SourceAttempt != 1 {
			t.Fatal("preparation and real attempt provenance were conflated")
		}
		if err := store.ReleaseRoundBaselinePreparation(ctx, reservation); err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var owner string
			var slotJob server.Id
			server.Raise(conn.QueryRow(ctx, `SELECT worker_id,job_id FROM competition_worker_slot WHERE slot_id=1`).Scan(&owner, &slotJob))
			if owner != "synthetic-worker" || slotJob != job.JobId {
				t.Fatal("old preparation release erased genuine candidate ownership")
			}
		})
		if _, err := store.ClaimPreparedRoundBaseline(ctx, settings, reservation, prepared); err == nil {
			t.Fatal("publication replay reset the original candidate timer")
		}
		// A transport can report failure after COMMIT. A resulting durable hold
		// must gate warm reclaims too, preserving the actual first clock/attempt.
		if err := store.HoldRoundBaselinePreparation(ctx, settings, reservation, roundBaselinePreparationHold{Stage: "synthetic_ambiguous_commit"}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Duration(settings.WorkerLeaseSeconds+1) * time.Second)
		if again, err := store.ReserveRoundBaseline(ctx, settings, "ambiguity-replacement", testWorkerImageDigest()); !errors.Is(err, errRoundBaselinePreparationHeld) || again != nil {
			t.Fatal("ambiguous committed publication automatically consumed another attempt", again, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var attempts int
			var start time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT attempt_count,started_at FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&attempts, &start))
			if attempts != 1 || !start.Equal(*job.StartedAt) {
				t.Fatal("ambiguous commit reset timer/attempt", attempts, start)
			}
		})
	})
}

// An expired/released singleton is not permission to remeasure a failed cold
// round: its original source event remains the durable no-resampling fence.
func TestCompetitionRoundBaselinePreparationReservationSurvivesSlotExpiry(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := validSettings()
		settings.CompetitionId += "-baseline-preparation-held"
		settings.EvaluationPolicy.Replicates = 9
		settings.ArtifactRoot = t.TempDir()
		settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		now := server.NowUtc().Truncate(time.Second)
		settings.SeasonEndsAt = now.Add(60 * 24 * time.Hour)
		settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
		store := PostgresStore{now: func() time.Time { return now }}
		round, err := store.CreateStagingRound(ctx, settings, GenerateRoundArgs{OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), RevealAt: now.Add(time.Hour)}, false)
		if err != nil {
			t.Fatal(err)
		}
		patch, patchErr := ValidateAndCanonicalizePatch(testPatch("held-baseline"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "synthetic-miner", testApiImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := store.ReserveRoundBaseline(ctx, settings, "first-worker", testWorkerImageDigest())
		if err != nil || reservation == nil {
			t.Fatal(err)
		}
		if err := store.HoldRoundBaselinePreparation(ctx, settings, reservation, roundBaselinePreparationHold{Stage: "synthetic_archive_failure"}); err != nil {
			t.Fatal(err)
		}
		if err := store.HoldRoundBaselinePreparation(ctx, settings, reservation, roundBaselinePreparationHold{Stage: "synthetic_archive_failure"}); err != nil {
			t.Fatal(err)
		}
		// A previously captured earlier admission commits after this reservation.
		lateSettings := *settings
		lateStore := PostgresStore{now: func() time.Time { return reservation.SourceJob.Round.OpensAt }}
		latePatch, patchErr := ValidateAndCanonicalizePatch(testPatch("late-earlier-held-baseline"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		if _, _, err := lateStore.Enqueue(ctx, &lateSettings, round.RoundId, latePatch, "late-synthetic-miner", testApiImageDigest()); err != nil {
			t.Fatal(err)
		}
		if err := store.ReleaseRoundBaselinePreparation(ctx, reservation); err != nil {
			t.Fatal(err)
		}
		now = reservation.Deadline.Add(settings.roundBaselinePreparation.Settings.CleanupTimeout)
		if again, err := store.ReserveRoundBaseline(ctx, settings, "replacement-worker", testWorkerImageDigest()); !errors.Is(err, errRoundBaselinePreparationHeld) || again != nil {
			t.Fatal("expired owner silently resampled", again, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var started *time.Time
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT state,started_at,attempt_count FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&state, &started, &attempts))
			if state != "queued" || started != nil || attempts != 0 {
				t.Fatal("infra hold awarded the first submitter a failed attempt")
			}
			var holds int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='round_baseline_preparation_held'`, job.JobId).Scan(&holds))
			if holds != 1 {
				t.Fatal("replayed hold spammed durable events", holds)
			}
		})
	})
}

// A producer barrier reproduces the real admission clock: Enqueue captures its
// timestamp before private upload, so an older request can commit after B owns
// the round's first reservation. Only archive I/O is mocked.
type roundBaselinePreparationAdmissionArchive struct {
	fakeArtifactArchive
	blockedSha256 string
	entered       chan struct{}
	release       chan struct{}
}

// Neither the admission transaction nor its lock survives this upload wait.
func (self *roundBaselinePreparationAdmissionArchive) ArchiveSubmission(ctx context.Context, settings *Settings, roundId server.Id, patch *CanonicalPatch) (*retainedArtifact, error) {
	if patch.Sha256 == self.blockedSha256 {
		close(self.entered)
		select {
		case <-self.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return self.fakeArtifactArchive.ArchiveSubmission(ctx, settings, roundId, patch)
}

// The reservation is the same linearization point that a warm Claim uses;
// late earlier timestamps cannot invalidate a completed genuine R9 corpus.
func TestCompetitionRoundBaselinePreparationLateAdmissionDoesNotInvalidateOwner(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := validSettings()
		settings.CompetitionId += "-baseline-late-admission"
		settings.EvaluationPolicy.Replicates = 9
		settings.ArtifactRoot = t.TempDir()
		settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		early := server.NowUtc().Truncate(time.Second)
		current := early.Add(time.Second)
		settings.SeasonEndsAt = early.Add(60 * 24 * time.Hour)
		settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
		firstStore := PostgresStore{now: func() time.Time { return early }}
		store := PostgresStore{now: func() time.Time { return current }}
		round, err := store.CreateStagingRound(ctx, settings, GenerateRoundArgs{OpensAt: early.Add(-time.Minute), ClosesAt: early.Add(time.Hour), RevealAt: early.Add(time.Hour)}, false)
		if err != nil {
			t.Fatal(err)
		}
		earlyPatch, patchErr := ValidateAndCanonicalizePatch(testPatch("earlier-slow-upload"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		laterPatch, patchErr := ValidateAndCanonicalizePatch(testPatch("later-fast-upload"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		archive := &roundBaselinePreparationAdmissionArchive{blockedSha256: earlyPatch.Sha256, entered: make(chan struct{}), release: make(chan struct{})}
		settings.artifactArchive = archive
		type admission struct {
			job *queuedJob
			err error
		}
		earlyDone := make(chan admission, 1)
		go func() {
			defer close(earlyDone)
			job, _, err := firstStore.Enqueue(ctx, settings, round.RoundId, earlyPatch, "earlier-miner", testApiImageDigest())
			earlyDone <- admission{job: job, err: err}
		}()
		defer func() { cancel(); awaitRoundBaselinePreparationTest(t, earlyDone) }()
		awaitRoundBaselinePreparationTest(t, archive.entered)
		later, _, err := store.Enqueue(ctx, settings, round.RoundId, laterPatch, "later-miner", testApiImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := store.ReserveRoundBaseline(ctx, settings, "linearized-worker", testWorkerImageDigest())
		if err != nil || reservation == nil || reservation.JobId != later.JobId {
			t.Fatal("reserve committed FIFO", reservation, err)
		}
		close(archive.release)
		admitted := awaitRoundBaselinePreparationTest(t, earlyDone)
		if admitted.err != nil || admitted.job == nil || !admitted.job.SubmittedAt.Before(later.SubmittedAt) {
			t.Fatal("fixture did not reproduce older late admission", admitted.err)
		}
		current = current.Add(2 * time.Hour)
		prepared := preparedRoundBaselineTest(t, settings, reservation)
		claimed, err := store.ClaimPreparedRoundBaseline(ctx, settings, reservation, prepared)
		if err != nil || claimed == nil || claimed.JobId != later.JobId || claimed.StartedAt == nil || !claimed.StartedAt.Equal(current) {
			t.Fatal("late older admission invalidated original owner", claimed, err)
		}
		// Finish the actual claimed candidate with a synthetic terminal outcome;
		// the preparation itself never completed/failed the real job.
		outcome := EvaluationOutcome{Error: submissionError("run_unstable", "synthetic candidate terminal result"), ArtifactManifest: json.RawMessage(`{"schema":1}`), RoundBaselineJson: prepared.BaselineJson, RoundBaselineSha256: prepared.BaselineSha256}
		if retry, err := store.Complete(ctx, settings, reservation.WorkerId, claimed.JobId, outcome); err != nil || retry {
			t.Fatal("finish genuine candidate", retry, err)
		}
		if replacement, err := store.ReserveRoundBaseline(ctx, settings, "next-worker", testWorkerImageDigest()); err != nil || replacement != nil {
			t.Fatal("warm baseline spawned another producer", replacement, err)
		}
		next, err := store.Claim(ctx, settings, "next-worker", testWorkerImageDigest())
		if err != nil || next == nil || next.JobId != admitted.job.JobId || next.RoundBaseline == nil || next.RoundBaseline.SourceJobId != later.JobId {
			t.Fatal("late earlier admission was not next ordinary FIFO claim", next, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var reservations int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_event e JOIN competition_job j USING(job_id) WHERE j.round_id=$1 AND e.event_type='round_baseline_preparation_reserved'`, round.RoundId).Scan(&reservations))
			if reservations != 1 {
				t.Fatal("round resampled", reservations)
			}
		})
	})
}
