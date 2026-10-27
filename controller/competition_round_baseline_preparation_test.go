// Preparation ownership is independent of a submission's unchanged clock.
// Explicit barriers exercise cancellation/fencing without wall-clock sleeps.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Only the optional cold-round capability is mocked; unexpected ordinary
// operations panic through the embedded nil interface.
type roundBaselinePreparationTestStore struct {
	Store
	stateLock       sync.Mutex
	preparation     *roundBaselinePreparation
	job             *queuedJob
	now             time.Time
	held            bool
	released        bool
	claimed         bool
	ordinaryClaims  int
	hostHeartbeats  int
	heartbeatNotify chan struct{}
	checkErr        error
	check           func(context.Context) error
	publicationErr  error
	publishCheck    func(context.Context) error
}

// The fake preserves the same durable single-producer admission rule.
func (self *roundBaselinePreparationTestStore) ReserveRoundBaseline(context.Context, *Settings, string, string) (*roundBaselinePreparation, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.held || self.released {
		return nil, errRoundBaselinePreparationHeld
	}
	return self.preparation, nil
}

// A driven fence edge never renews the preparation's original deadline.
func (self *roundBaselinePreparationTestStore) CheckRoundBaselinePreparation(ctx context.Context, _ *Settings, _ *roundBaselinePreparation) error {
	self.stateLock.Lock()
	check, err := self.check, self.checkErr
	self.stateLock.Unlock()
	if check != nil {
		return check(ctx)
	}
	return err
}

// Failed preparation does not complete, retry, or start the real queued job.
func (self *roundBaselinePreparationTestStore) HoldRoundBaselinePreparation(context.Context, *Settings, *roundBaselinePreparation, roundBaselinePreparationHold) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.held = true
	return nil
}

// The commit boundary is the only place the real submission gets a clock.
func (self *roundBaselinePreparationTestStore) ClaimPreparedRoundBaseline(ctx context.Context, _ *Settings, preparation *roundBaselinePreparation, prepared *preparedRoundBaseline) (*queuedJob, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.publicationErr != nil {
		return nil, self.publicationErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if self.publishCheck != nil {
		if err := self.publishCheck(ctx); err != nil {
			return nil, err
		}
	}
	job := *self.job
	started := self.now
	job.State, job.StartedAt, job.AttemptCount, job.LeaseOwner = "running", &started, 1, preparation.WorkerId
	job.RoundBaseline = &roundBaselineRecord{Json: prepared.BaselineJson, Sha256: prepared.BaselineSha256}
	self.claimed = true
	return &job, nil
}

// An owner release cannot erase a successfully transferred candidate slot.
func (self *roundBaselinePreparationTestStore) ReleaseRoundBaselinePreparation(context.Context, *roundBaselinePreparation) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.released = true
	return nil
}

// The warm/disabled path remains the historical ordinary claim.
func (self *roundBaselinePreparationTestStore) Claim(context.Context, *Settings, string, string) (*queuedJob, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ordinaryClaims++
	return self.job, nil
}

// Preparation refreshes ordinary host liveness without starting a job lease.
func (self *roundBaselinePreparationTestStore) RegisterHost(ctx context.Context, _ *Settings, _ HostSelfCheck) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	self.hostHeartbeats++
	if self.heartbeatNotify != nil {
		close(self.heartbeatNotify)
		self.heartbeatNotify = nil
	}
	return nil
}

// A producer callback is joined before its lease can be released.
type roundBaselinePreparationTestProducer struct {
	Evaluator
	run func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error)
}

// No candidate evaluator method is implemented or reached by this fixture.
func (self roundBaselinePreparationTestProducer) PrepareRoundBaseline(ctx context.Context, _ *Settings, preparation *roundBaselinePreparation) (*preparedRoundBaseline, error) {
	return self.run(ctx, preparation)
}

// Uses synthetic identities and native R9 policy, without hidden/live workload.
func newRoundBaselinePreparationTest(t testing.TB) (*Settings, *roundBaselinePreparation, *roundBaselinePreparationTestStore, HostSelfCheck) {
	t.Helper()
	settings := validSettings()
	settings.EvaluationPolicy.Replicates = 9
	settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	now := server.NowUtc().Truncate(time.Second)
	roundId, jobId := server.NewId(), server.NewId()
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: roundId, State: "queued", PatchSha256: strings.Repeat("c", 64), SubmittedAt: now, WorkerImageDigest: testWorkerImageDigest()},
		Round: roundRecord{RoundResult: RoundResult{RoundId: roundId, ProvidersSha256: strings.Repeat("d", 64), WorkloadCommitment: strings.Repeat("e", 64), OpensAt: now, ClosesAt: now.Add(time.Hour)}, PolicyJson: policy}}
	digest, err := roundBaselinePreparationContext(settings, job)
	if err != nil {
		t.Fatal(err)
	}
	preparation := &roundBaselinePreparation{Schema: 1, Kind: "sim-latency-round-baseline-preparation-reservation", Owner: "baseline.synthetic", WorkerId: "worker.synthetic", JobId: jobId, RoundId: roundId,
		PatchSha256: job.PatchSha256, ContextSha256: digest, WorkerImage: testWorkerImageDigest(), StartedAt: now, Deadline: now.Add(3 * time.Hour), SourceJob: job}
	store := &roundBaselinePreparationTestStore{preparation: preparation, job: job, now: now}
	return settings, preparation, store, HostSelfCheck{RebaselinePassed: true, RebaselineRoundId: &roundId}
}

// A complete retained preparation fixture is not an ordinary attempt manifest.
func preparedRoundBaselineTest(t testing.TB, settings *Settings, preparation *roundBaselinePreparation) *preparedRoundBaseline {
	t.Helper()
	baseline := testRoundBaseline(t, settings, &preparation.SourceJob.Round, []float64{100, 101, 99, 100, 101, 99, 100, 101, 99})
	digest := sha256.Sum256(baseline)
	baselineSha256 := hex.EncodeToString(digest[:])
	prefix := "private/competition/v1/" + settings.CompetitionId + "/rounds/" + preparation.RoundId.String() + "/baseline-preparation/" + preparation.Owner
	artifact := evaluationArtifact{Path: "baseline.json", Sha256: baselineSha256, Bytes: int64(len(baseline))}
	document := roundBaselinePreparationArchiveDocument{Schema: 1, Kind: "sim-latency-round-baseline-preparation-artifacts", Outcome: "prepared", Owner: preparation.Owner, ContextSha256: preparation.ContextSha256,
		JobId: preparation.JobId.String(), RoundId: preparation.RoundId.String(), BaseSha: settings.BaseSha, ImageId: settings.EvaluatorImageDigest, ProvidersSha256: preparation.SourceJob.Round.ProvidersSha256,
		WorkerImage: preparation.WorkerImage, BaselineSha256: baselineSha256, Artifacts: []evaluationArtifact{artifact},
		Retention: &artifactRetention{Backend: "server/blob", Bucket: settings.ArtifactBucket, Prefix: "private", ManifestKey: prefix + "/baseline-preparation-manifest.json", RetainUntil: settings.RetainUntil,
			AuthenticatedAfterUpload: true, ComplianceObjectLock: true, ObjectCount: 1, Bytes: artifact.Bytes,
			Objects: []retainedArtifact{{Path: artifact.Path, Key: prefix + "/objects/" + artifact.Sha256 + "/" + artifact.Path, Sha256: artifact.Sha256, Bytes: artifact.Bytes, VersionId: "synthetic-version", Mode: "COMPLIANCE", RetainUntil: settings.RetainUntil}}}}
	for index := 1; index <= 9; index++ {
		document.ControlEvaluationIds = append(document.ControlEvaluationIds, fmt.Sprintf("baseline-%02d-%s", index, preparation.JobId.String()[:8]))
	}
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest := sha256.Sum256(content)
	return &preparedRoundBaseline{BaselineJson: baseline, BaselineSha256: baselineSha256, ArtifactManifest: content, ArtifactManifestSha256: hex.EncodeToString(manifestDigest[:])}
}

// Cold controls consume no real submission time or failed-attempt allowance.
func TestCompetitionRoundBaselinePreparationStartsClockOnlyAtPublication(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	prepared := preparedRoundBaselineTest(t, settings, preparation)
	started, finish := make(chan struct{}), make(chan struct{})
	producer := roundBaselinePreparationTestProducer{run: func(ctx context.Context, reservation *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(preparation.Deadline) {
			return nil, errors.New("preparation deadline changed")
		}
		close(started)
		<-finish
		return prepared, nil
	}}
	worker := &Worker{settings: settings, store: store, evaluator: producer, workerId: preparation.WorkerId, workerImageDigest: preparation.WorkerImage}
	type result struct {
		job *queuedJob
		err error
	}
	done := make(chan result, 1)
	go func() {
		job, err := worker.claimWithPreparedRoundBaseline(context.Background(), host)
		done <- result{job: job, err: err}
	}()
	<-started
	if store.job.State != "queued" || store.job.StartedAt != nil || store.job.AttemptCount != 0 {
		t.Fatal("control producer started the real job")
	}
	store.stateLock.Lock()
	store.now = preparation.StartedAt.Add(2 * time.Hour)
	store.stateLock.Unlock()
	close(finish)
	actual := <-done
	if actual.err != nil || actual.job == nil || actual.job.StartedAt == nil || !actual.job.StartedAt.Equal(preparation.StartedAt.Add(2*time.Hour)) || actual.job.AttemptCount != 1 || actual.job.RoundBaseline == nil {
		t.Fatalf("publication did not start one genuine claim: job=%#v err=%v", actual.job, actual.err)
	}
	if actual.job.StartedAt.Add(time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds)*time.Second).Sub(*actual.job.StartedAt) != 3*time.Hour || store.ordinaryClaims != 0 || !store.released || !store.claimed {
		t.Fatal("candidate lost its unchanged full budget or singleton transfer")
	}
}

// A canceled producer is joined before release and its durable owner prevents
// the next worker from silently sampling a replacement control corpus.
func TestCompetitionRoundBaselinePreparationCancelJoinsAndHolds(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	started, joined := make(chan struct{}), make(chan struct{})
	producerCalls := 0
	producer := roundBaselinePreparationTestProducer{run: func(ctx context.Context, _ *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		producerCalls++
		close(started)
		<-ctx.Done()
		close(joined)
		return nil, ctx.Err()
	}}
	worker := &Worker{settings: settings, store: store, evaluator: producer, workerId: preparation.WorkerId, workerImageDigest: preparation.WorkerImage}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := worker.claimWithPreparedRoundBaseline(ctx, host); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, errRoundBaselinePreparationHeld) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("lease released before producer joined")
	}
	if !store.released || !store.held || store.claimed || store.job.StartedAt != nil || store.job.AttemptCount != 0 {
		t.Fatal("cancellation consumed a real attempt or lost its hold")
	}
	if _, err := worker.claimWithPreparedRoundBaseline(context.Background(), host); !errors.Is(err, errRoundBaselinePreparationHeld) || producerCalls != 1 {
		t.Fatal("replacement worker resampled after cancellation", err, producerCalls)
	}
}

// The periodic owner fence remains active through producer-side archive waits.
func TestCompetitionRoundBaselinePreparationFenceLossCancelsBeforePublication(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	edge := make(chan time.Time)
	settings.roundBaselinePreparation.Settings.FenceNotify = edge
	started := make(chan struct{})
	producer := roundBaselinePreparationTestProducer{run: func(ctx context.Context, _ *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		close(started)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}}
	worker := &Worker{settings: settings, store: store, evaluator: producer, workerId: preparation.WorkerId, workerImageDigest: preparation.WorkerImage}
	done := make(chan error, 1)
	go func() { _, err := worker.claimWithPreparedRoundBaseline(context.Background(), host); done <- err }()
	<-started
	store.stateLock.Lock()
	store.checkErr = ErrLeaseLost
	store.stateLock.Unlock()
	edge <- preparation.StartedAt
	if err := <-done; !errors.Is(err, ErrLeaseLost) || !errors.Is(err, errRoundBaselinePreparationHeld) {
		t.Fatal("lost fence did not hold owned preparation", err)
	}
	if store.claimed || !store.released || store.job.StartedAt != nil {
		t.Fatal("fence loss published a candidate claim")
	}
}

// Frozen context includes source/workload/policy as well as the real FIFO job;
// neither borrowed bridge controls nor ordinary attempt manifests qualify.
func TestCompetitionRoundBaselinePreparationRejectsForeignRetainedContext(t *testing.T) {
	settings, preparation, _, _ := newRoundBaselinePreparationTest(t)
	prepared := preparedRoundBaselineTest(t, settings, preparation)
	if err := validatePreparedRoundBaseline(prepared, preparation, settings); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*roundBaselinePreparationArchiveDocument){
		func(document *roundBaselinePreparationArchiveDocument) { document.RoundId = server.NewId().String() },
		func(document *roundBaselinePreparationArchiveDocument) {
			document.ProvidersSha256 = strings.Repeat("a", 64)
		},
		func(document *roundBaselinePreparationArchiveDocument) { document.BaseSha = strings.Repeat("c", 40) },
		func(document *roundBaselinePreparationArchiveDocument) {
			document.Retention.ManifestKey = "private/jobs/foreign/attempt-01/artifact-manifest.json"
		},
		func(document *roundBaselinePreparationArchiveDocument) {
			document.Retention.Objects[0].Mode = "GOVERNANCE"
		},
		func(document *roundBaselinePreparationArchiveDocument) {
			document.ControlEvaluationIds[8] = document.ControlEvaluationIds[0]
		},
	} {
		copy := *prepared
		var document roundBaselinePreparationArchiveDocument
		if err := json.Unmarshal(copy.ArtifactManifest, &document); err != nil {
			t.Fatal(err)
		}
		change(&document)
		copy.ArtifactManifest, _ = json.Marshal(document)
		digest := sha256.Sum256(copy.ArtifactManifest)
		copy.ArtifactManifestSha256 = hex.EncodeToString(digest[:])
		if err := validatePreparedRoundBaseline(&copy, preparation, settings); err == nil {
			t.Fatal("foreign or unretained control authority was accepted")
		}
	}
}

// Disabled/warm paths retain the old claim behavior; a failed final fence does
// not consume the source job even after the expensive controls have completed.
func TestCompetitionRoundBaselinePreparationWarmAndPublicationFence(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	producerCalls := 0
	producer := roundBaselinePreparationTestProducer{run: func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		producerCalls++
		return preparedRoundBaselineTest(t, settings, preparation), nil
	}}
	worker := &Worker{settings: settings, store: store, evaluator: producer, workerId: preparation.WorkerId, workerImageDigest: preparation.WorkerImage}
	store.preparation = nil
	if job, err := worker.claimWithPreparedRoundBaseline(context.Background(), host); err != nil || job != store.job || producerCalls != 0 || store.ordinaryClaims != 1 {
		t.Fatal("warm baseline ran the producer", err)
	}
	store.preparation = preparation
	store.publicationErr = ErrConflict
	if _, err := worker.claimWithPreparedRoundBaseline(context.Background(), host); !errors.Is(err, ErrConflict) || !errors.Is(err, errRoundBaselinePreparationHeld) {
		t.Fatal("FIFO/publication fence was ignored", err)
	}
	if store.job.StartedAt != nil || store.job.AttemptCount != 0 || store.claimed || !store.held || producerCalls != 1 {
		t.Fatal("failed publication started or resampled a real job")
	}
}

// A preparation heartbeat cannot observe the successful slot transfer and
// then cancel its own publication. Intentional observer shutdown is joined.
func TestCompetitionRoundBaselinePreparationJoinsFenceBeforePublication(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	prepared := preparedRoundBaselineTest(t, settings, preparation)
	edge := make(chan time.Time)
	settings.roundBaselinePreparation.Settings.FenceNotify = edge
	producerStarted, producerFinish := make(chan struct{}), make(chan struct{})
	checkStarted, checkJoined := make(chan struct{}), make(chan struct{})
	producer := roundBaselinePreparationTestProducer{run: func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		close(producerStarted)
		<-producerFinish
		return prepared, nil
	}}
	worker := &Worker{settings: settings, store: store, evaluator: producer, workerId: preparation.WorkerId, workerImageDigest: preparation.WorkerImage}
	done := make(chan error, 1)
	go func() { _, err := worker.claimWithPreparedRoundBaseline(context.Background(), host); done <- err }()
	awaitRoundBaselinePreparationTest(t, producerStarted)
	store.stateLock.Lock()
	store.check = func(ctx context.Context) error {
		close(checkStarted)
		<-ctx.Done()
		close(checkJoined)
		return ctx.Err()
	}
	store.publishCheck = func(context.Context) error {
		select {
		case <-checkJoined:
			return nil
		default:
			return errors.New("publication raced its preparation heartbeat")
		}
	}
	store.stateLock.Unlock()
	select {
	case edge <- preparation.StartedAt:
	case <-time.After(5 * time.Second):
		t.Fatal("fence observer did not subscribe")
	}
	awaitRoundBaselinePreparationTest(t, checkStarted)
	close(producerFinish)
	if err := awaitRoundBaselinePreparationTest(t, done); err != nil {
		t.Fatal("intentional heartbeat stop canceled genuine publication", err)
	}
	if !store.claimed || store.held || !store.released {
		t.Fatal("successful singleton transfer entered infrastructure hold")
	}
}

// A timeout is only a deadlock diagnostic, never an ordering mechanism.
func awaitRoundBaselinePreparationTest[T any](t testing.TB, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("baseline preparation barrier did not complete")
	}
	var zero T
	return zero
}

// A long genuine baseline cannot make admission report no eligible host after
// its normal sixty-second liveness window. No repeated self-check is executed.
func TestCompetitionRoundBaselinePreparationKeepsHostLiveWithoutStartingJob(t *testing.T) {
	settings, preparation, store, host := newRoundBaselinePreparationTest(t)
	prepared := preparedRoundBaselineTest(t, settings, preparation)
	edge := make(chan time.Time)
	settings.roundBaselinePreparation.Settings.FenceNotify = edge
	settings.roundBaselinePreparation.Settings.Now = func() time.Time { store.stateLock.Lock(); defer store.stateLock.Unlock(); return store.now }
	started, finish, heartbeat := make(chan struct{}), make(chan struct{}), make(chan struct{})
	store.heartbeatNotify = heartbeat
	producer := roundBaselinePreparationTestProducer{run: func(context.Context, *roundBaselinePreparation) (*preparedRoundBaseline, error) {
		close(started)
		<-finish
		return prepared, nil
	}}
	worker := &Worker{settings: settings, store: store, evaluator: producer, workerId: preparation.WorkerId, workerImageDigest: preparation.WorkerImage}
	done := make(chan error, 1)
	go func() { _, err := worker.claimWithPreparedRoundBaseline(context.Background(), host); done <- err }()
	awaitRoundBaselinePreparationTest(t, started)
	store.stateLock.Lock()
	store.now = preparation.StartedAt.Add(time.Duration(settings.WorkerHeartbeatSeconds+1) * time.Second)
	store.stateLock.Unlock()
	select {
	case edge <- preparation.StartedAt:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation liveness observer did not subscribe")
	}
	awaitRoundBaselinePreparationTest(t, heartbeat)
	if store.job.StartedAt != nil || store.job.AttemptCount != 0 || !preparation.Deadline.Equal(preparation.StartedAt.Add(3*time.Hour)) {
		t.Fatal("liveness extended the owner or started the submission")
	}
	close(finish)
	if err := awaitRoundBaselinePreparationTest(t, done); err != nil {
		t.Fatal(err)
	}
	if store.hostHeartbeats != 1 || !store.claimed {
		t.Fatal("host liveness was not maintained independently", store.hostHeartbeats)
	}
}
