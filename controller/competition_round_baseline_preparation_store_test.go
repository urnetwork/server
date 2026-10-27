// The native database guard must see a genuine running job in the same
// transaction that publishes a prepared baseline; no fake job/FK bypass exists.
// Scanner failures cannot manufacture a nonnil baseline for a cold claim.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// Models the exact pgx row boundary without a database or scheduler dependency.
type roundBaselinePreparationScanRow func(...any) error

// A row may populate a prefix of its destinations before reporting failure.
func (self roundBaselinePreparationScanRow) Scan(destinations ...any) error {
	return self(destinations...)
}

// A missing SQL row must stay absent when the final Claim gate checks cold work.
func TestCompetitionRoundBaselinePreparationScanMissingReturnsNil(t *testing.T) {
	calls := 0
	row := roundBaselinePreparationScanRow(func(...any) error {
		calls++
		return pgx.ErrNoRows
	})
	baseline, err := scanRoundBaseline(row)
	if baseline != nil || !errors.Is(err, pgx.ErrNoRows) || calls != 1 {
		t.Fatal("absent baseline acquired an allocated identity", baseline, err, calls)
	}
}

// The no-row fix must not expose a partially decoded identity on other errors.
func TestCompetitionRoundBaselinePreparationScanPartialFailureReturnsNil(t *testing.T) {
	scanErr := errors.New("synthetic partial baseline scan")
	row := roundBaselinePreparationScanRow(func(destinations ...any) error {
		if len(destinations) != 13 {
			t.Fatalf("unexpected baseline scan field count: %d", len(destinations))
		}
		*destinations[0].(*server.Id) = server.Id{1}
		*destinations[1].(*[]byte) = []byte("synthetic incomplete control")
		return scanErr
	})
	baseline, err := scanRoundBaseline(row)
	if baseline != nil || !errors.Is(err, scanErr) {
		t.Fatal("partially scanned baseline escaped its error", baseline, err)
	}
}

// Successful decoding preserves every stored field and leaves validation to
// the existing caller; no zero-row or error path gains baseline authority.
func TestCompetitionRoundBaselinePreparationScanSuccessPreservesRecord(t *testing.T) {
	want := &roundBaselineRecord{RoundId: server.Id{1}, Json: []byte(`{"synthetic":"control"}`),
		Sha256: strings.Repeat("a", 64), SourceJobId: server.Id{2}, SourceAttempt: 3,
		SourceArtifactManifestSha256: strings.Repeat("b", 64), BaseSha: strings.Repeat("c", 40),
		ProvidersSha256: strings.Repeat("d", 64), EvaluatorImageDigest: "sha256:" + strings.Repeat("e", 64),
		ScorerVersion: "synthetic-scorer", HardwareId: "synthetic-hardware", HostQualificationSha256: strings.Repeat("f", 64),
		CreatedAt: time.Date(2040, time.January, 2, 3, 4, 5, 0, time.UTC)}
	row := roundBaselinePreparationScanRow(func(destinations ...any) error {
		if len(destinations) != 13 {
			t.Fatalf("unexpected baseline scan field count: %d", len(destinations))
		}
		*destinations[0].(*server.Id) = want.RoundId
		*destinations[1].(*[]byte) = append([]byte(nil), want.Json...)
		*destinations[2].(*string) = want.Sha256
		*destinations[3].(*server.Id) = want.SourceJobId
		*destinations[4].(*int) = want.SourceAttempt
		*destinations[5].(*string) = want.SourceArtifactManifestSha256
		*destinations[6].(*string) = want.BaseSha
		*destinations[7].(*string) = want.ProvidersSha256
		*destinations[8].(*string) = want.EvaluatorImageDigest
		*destinations[9].(*string) = want.ScorerVersion
		*destinations[10].(*string) = want.HardwareId
		*destinations[11].(*string) = want.HostQualificationSha256
		*destinations[12].(*time.Time) = want.CreatedAt
		return nil
	})
	baseline, err := scanRoundBaseline(row)
	if err != nil || !reflect.DeepEqual(baseline, want) {
		t.Fatalf("successful baseline scan changed fields: actual=%#v expected=%#v err=%v", baseline, want, err)
	}
}

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

// Holds the real worker at the gap between its completed Reserve transaction
// and ordinary Claim. All database methods remain the actual PostgresStore.
type roundBaselinePreparationClaimBarrierStore struct {
	PostgresStore
	reserved chan struct{}
	resume   chan struct{}
}

// A nil result may mean a busy owner, a warm round, or no queued work; none
// grants authority to skip Claim's final locked preparation/HOLD gate.
func (self *roundBaselinePreparationClaimBarrierStore) ReserveRoundBaseline(ctx context.Context, settings *Settings, workerId, workerImage string) (*roundBaselinePreparation, error) {
	preparation, err := self.PostgresStore.ReserveRoundBaseline(ctx, settings, workerId, workerImage)
	if err == nil && preparation == nil {
		close(self.reserved)
		select {
		case <-self.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return preparation, err
}

// Source controls are synthetic SQL/retention fixtures; no provider, host
// command, candidate evaluator or native control producer is invoked.
func roundBaselinePreparationClaimRaceFixture(t testing.TB, suffix string) (*Settings, PostgresStore, *roundRecord) {
	t.Helper()
	settings := validSettings()
	settings.CompetitionId += suffix
	settings.EvaluationPolicy.Replicates = 9
	settings.ArtifactRoot = t.TempDir()
	settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
	now := server.NowUtc().Truncate(time.Second)
	settings.SeasonEndsAt = now.Add(60 * 24 * time.Hour)
	settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
	store := PostgresStore{now: func() time.Time { return now }}
	round, err := store.CreateStagingRound(context.Background(), settings, GenerateRoundArgs{OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), RevealAt: now.Add(time.Hour)}, false)
	if err != nil {
		t.Fatal(err)
	}
	return settings, store, round
}

// The old two-transaction fallback could start a held cold job immediately
// after its original owner released the slot. Drive that exact interleaving.
func TestCompetitionRoundBaselinePreparationColdFallbackCannotBypassHold(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings, store, round := roundBaselinePreparationClaimRaceFixture(t, "-baseline-cold-claim-race")
		patch, patchErr := ValidateAndCanonicalizePatch(testPatch("cold-fallback-held"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		source, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "synthetic-miner", testApiImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		owner, err := store.ReserveRoundBaseline(ctx, settings, "owner-worker", testWorkerImageDigest())
		if err != nil || owner == nil {
			t.Fatal("source reservation", owner, err)
		}
		barrier := &roundBaselinePreparationClaimBarrierStore{PostgresStore: store, reserved: make(chan struct{}), resume: make(chan struct{})}
		producerCalls := 0
		producer := roundBaselinePreparationTestProducer{run: func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
			producerCalls++
			return nil, errors.New("unexpected producer rerun")
		}}
		worker := &Worker{settings: settings, store: barrier, evaluator: producer, workerId: "waiting-worker", workerImageDigest: testWorkerImageDigest()}
		type result struct {
			job *queuedJob
			err error
		}
		done := make(chan result, 1)
		go func() {
			defer close(done)
			job, err := worker.claimWithPreparedRoundBaseline(ctx, HostSelfCheck{})
			done <- result{job: job, err: err}
		}()
		defer func() { cancel(); awaitRoundBaselinePreparationTest(t, done) }()
		awaitRoundBaselinePreparationTest(t, barrier.reserved)
		if err := store.HoldRoundBaselinePreparation(ctx, settings, owner, roundBaselinePreparationHold{Stage: "synthetic_original_owner_failure"}); err != nil {
			t.Fatal(err)
		}
		if err := store.ReleaseRoundBaselinePreparation(ctx, owner); err != nil {
			t.Fatal(err)
		}
		close(barrier.resume)
		actual := awaitRoundBaselinePreparationTest(t, done)
		if actual.job != nil || !errors.Is(actual.err, errRoundBaselinePreparationHeld) || producerCalls != 0 {
			t.Fatal("busy-owner fallback bypassed durable cold hold", actual.job, actual.err, producerCalls)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var start *time.Time
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT state,started_at,attempt_count FROM competition_job WHERE job_id=$1`, source.JobId).Scan(&state, &start, &attempts))
			if state != "queued" || start != nil || attempts != 0 {
				t.Fatal("cold fallback consumed a real attempt", state, start, attempts)
			}
		})
	})
}

// A completed warm peek is not a permit that survives a new HOLD. The hold's
// source job differs from the FIFO job, exercising the shared singleton lock.
func TestCompetitionRoundBaselinePreparationWarmFallbackRechecksNewHold(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings, store, round := roundBaselinePreparationClaimRaceFixture(t, "-baseline-warm-claim-race")
		jobs := []*queuedJob{}
		for _, name := range []string{"warm-source", "warm-next"} {
			patch, patchErr := ValidateAndCanonicalizePatch(testPatch(name), settings.PatchPolicy)
			if patchErr != nil {
				t.Fatal(patchErr)
			}
			job, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "synthetic-"+name, testApiImageDigest())
			if err != nil {
				t.Fatal(err)
			}
			jobs = append(jobs, job)
		}
		owner, err := store.ReserveRoundBaseline(ctx, settings, "source-worker", testWorkerImageDigest())
		if err != nil || owner == nil {
			t.Fatal(owner, err)
		}
		prepared := preparedRoundBaselineTest(t, settings, owner)
		claimed, err := store.ClaimPreparedRoundBaseline(ctx, settings, owner, prepared)
		if err != nil || claimed == nil {
			t.Fatal(claimed, err)
		}
		outcome := EvaluationOutcome{Error: submissionError("run_unstable", "synthetic genuine candidate terminal result"), ArtifactManifest: json.RawMessage(`{"schema":1}`), RoundBaselineJson: prepared.BaselineJson, RoundBaselineSha256: prepared.BaselineSha256}
		if retry, err := store.Complete(ctx, settings, owner.WorkerId, claimed.JobId, outcome); err != nil || retry {
			t.Fatal(retry, err)
		}
		barrier := &roundBaselinePreparationClaimBarrierStore{PostgresStore: store, reserved: make(chan struct{}), resume: make(chan struct{})}
		producerCalls := 0
		producer := roundBaselinePreparationTestProducer{run: func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
			producerCalls++
			return nil, errors.New("warm controls were resampled")
		}}
		worker := &Worker{settings: settings, store: barrier, evaluator: producer, workerId: "warm-waiting-worker", workerImageDigest: testWorkerImageDigest()}
		type result struct {
			job *queuedJob
			err error
		}
		done := make(chan result, 1)
		go func() {
			defer close(done)
			job, err := worker.claimWithPreparedRoundBaseline(ctx, HostSelfCheck{})
			done <- result{job: job, err: err}
		}()
		defer func() { cancel(); awaitRoundBaselinePreparationTest(t, done) }()
		awaitRoundBaselinePreparationTest(t, barrier.reserved)
		if err := store.HoldRoundBaselinePreparation(ctx, settings, owner, roundBaselinePreparationHold{Stage: "synthetic_late_publication_hold"}); err != nil {
			t.Fatal(err)
		}
		close(barrier.resume)
		actual := awaitRoundBaselinePreparationTest(t, done)
		if actual.job != nil || !errors.Is(actual.err, errRoundBaselinePreparationHeld) || producerCalls != 0 {
			t.Fatal("warm fallback ignored new hold", actual.job, actual.err, producerCalls)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var start *time.Time
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT state,started_at,attempt_count FROM competition_job WHERE job_id=$1`, jobs[1].JobId).Scan(&state, &start, &attempts))
			if state != "queued" || start != nil || attempts != 0 {
				t.Fatal("warm fallback started after a durable hold", state, start, attempts)
			}
		})
	})
}

// New work can also appear after an empty Reserve transaction. It must wait
// for its own first reservation, not fall through to a timed cold Claim.
func TestCompetitionRoundBaselinePreparationEmptyFallbackWaitsForNewColdAdmission(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings, store, round := roundBaselinePreparationClaimRaceFixture(t, "-baseline-empty-claim-race")
		if preparation, err := store.ReserveRoundBaseline(ctx, settings, "empty-worker", testWorkerImageDigest()); err != nil || preparation != nil {
			t.Fatal(preparation, err)
		}
		patch, patchErr := ValidateAndCanonicalizePatch(testPatch("arrived-after-empty-peek"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		queued, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "new-synthetic-miner", testApiImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		if job, err := store.Claim(ctx, settings, "empty-worker", testWorkerImageDigest()); err != nil || job != nil {
			t.Fatal("new cold admission bypassed its own preparation", job, err)
		}
		preparation, err := store.ReserveRoundBaseline(ctx, settings, "empty-worker", testWorkerImageDigest())
		if err != nil || preparation == nil || preparation.JobId != queued.JobId || preparation.SourceJob.StartedAt != nil || preparation.SourceJob.AttemptCount != 0 {
			t.Fatal("new cold admission was not prepared once while still queued", preparation, err)
		}
	})
}
