// The private database fixture traverses real source/round/hand-off/claim
// guards. Its source images, reviews, receipt producers and blob store are
// explicitly synthetic; this is not native host or MinIO qualification.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

type competitionIntakeFirstDatabaseFixture struct {
	settings *Settings
	store    PostgresStore
	lease    model.CompetitionSupervisorLease
	intent   *model.CompetitionSourceActivation
	archive  *blobArtifactArchive
	now      time.Time
}

// A real, presently live SQL lease is acquired first. Only the application
// fixture clock is then moved backward to materialize its historical epoch;
// all production triggers remain installed and active throughout bootstrap.
func newCompetitionIntakeFirstDatabaseFixture(t *testing.T, ctx context.Context, configureFuture ...func(*Settings)) *competitionIntakeFirstDatabaseFixture {
	t.Helper()
	if server.CompetitionIntakeFirstMigrationVersion() == 0 {
		t.Fatal("intake lifecycle fixture requires its actually registered composed migration")
	}
	var actualHead int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actualHead))
	})
	if actualHead != server.MigrationCount() {
		t.Fatalf("intake lifecycle fixture schema=%d want=%d", actualHead, server.MigrationCount())
	}
	t.Logf("intake_lifecycle_fixture_schema_head=%d", actualHead)
	fixture := &competitionIntakeFirstDatabaseFixture{settings: automationApiSettings()}
	server.Db(ctx, func(conn server.PgConn) {
		var err error
		fixture.now, err = competitionIntakeFirstDatabaseTime(ctx, conn)
		server.Raise(err)
	})
	currentTime := fixture.now
	settings := fixture.settings
	settings.CompetitionId = "synthetic-intake-" + server.NewId().String()
	settings.AutomationPolicy.RequiredApiCapability = model.CompetitionRuntimeBlobCapability
	settings.AutomationPolicy.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	settings.AutomationPolicy.WinnerReviewTimeoutSeconds = 3600
	settings.SeasonPolicy.PreparationWindowSeconds = 0
	settings.EvaluationPolicy.Replicates = 9
	settings.EvaluationPolicy.HardwareId = "synthetic-intake-evaluator"
	settings.ArtifactRoot = t.TempDir()
	settings.SeasonEndsAt, settings.RetainUntil = currentTime.Add(60*24*time.Hour), currentTime.Add(400*24*time.Hour)
	commits := map[string]string{}
	for _, name := range model.CompetitionSourceRepositoryNames() {
		commits[name] = settings.BaseSha
	}
	var err error
	settings.AutomationPolicy.SourceLockSha256, err = model.CompetitionSourceLockDigest(commits)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = PostgresStore{now: func() time.Time { return fixture.now }}
	lease, err := fixture.store.AcquireAutomationLease(ctx, settings, "synthetic-intake-supervisor", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	fixture.lease, settings.automationLease = *lease, lease
	fixture.now = currentTime.Add(-8 * 24 * time.Hour)
	manifest := []byte("synthetic historical immutable source manifest\n")
	manifestHash := sha256.Sum256(manifest)
	initial := model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		Generation: 1, TargetEpoch: 1, TargetRoundId: server.NewId(), Kind: "baseline", Reason: "prospectively_approved_baseline", WinnerReviewPolicy: model.CompetitionSnowWinnerReviewPolicy,
		Commits: commits, SourceLockSha256: settings.AutomationPolicy.SourceLockSha256, SourceDecisionDigest: strings.Repeat("c", 64),
		Manifest: manifest, ManifestSha256: hex.EncodeToString(manifestHash[:]), ManifestCommit: strings.Repeat("c", 40), ConfigExpectedCommit: strings.Repeat("d", 40), PublicationJournalSha256: strings.Repeat("e", 64),
		EvaluatorImageDigest: settings.EvaluatorImageDigest, HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256, Threshold: settings.EvaluationPolicy.TakeoverMargin,
		ThresholdEvidenceSha256: strings.Repeat("f", 64), PreparedAt: fixture.now.Add(-time.Hour), OpensAt: fixture.now, GitRefs: map[string]model.CompetitionSourceGitRef{}}
	for repository, commit := range commits {
		initial.GitRefs[repository] = model.CompetitionSourceGitRef{Branch: "synthetic-source", ExpectedCommit: commit, Commit: commit}
	}
	initial.GitRefs["config"] = model.CompetitionSourceGitRef{Branch: "synthetic-config", ExpectedCommit: initial.ConfigExpectedCommit, Commit: initial.ManifestCommit}
	stored, err := fixture.store.RecordSourceIntent(ctx, settings, *lease, initial)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"api", "worker"} {
		// These guarded synthetic historical rows are not next-epoch ACKs.
		receipt := competitionPreparationTestReceipt(stored, role, "synthetic-historical-"+role, testWorkerImageDigest())
		if err := fixture.store.RecordSourceReadiness(ctx, settings, receipt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.store.ActivateSource(ctx, settings, *lease, 1, 0); err != nil {
		t.Fatal(err)
	}
	settings.automationTargetRoundId = initial.TargetRoundId
	if _, err := fixture.store.CreateRound(ctx, settings, GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(7 * 24 * time.Hour), RevealAt: fixture.now.Add(7 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	fixture.now = currentTime
	seal, err := fixture.store.SealAndElect(ctx, settings, initial.TargetRoundId)
	if err != nil || seal == nil || len(seal.Decisions) == 0 || seal.Decisions[len(seal.Decisions)-1].SelectedJobId != nil {
		t.Fatal("historical empty-ranking fixture failed genuine seal", err)
	}
	current, err := fixture.store.CurrentSourceActivation(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	future, _, overlay := trustedInstrumentAuthorizationFixture(t)
	settings.FutureEpochPolicy = future.FutureEpochPolicy
	for _, configure := range configureFuture {
		configure(settings)
	}
	// This is a new synthetic receipt of the fixture's frozen provider class,
	// not a relabeling of a retained historical source-build receipt.
	providerModel, providerReasoning, supported := model.CompetitionWinnerReviewProvider(model.CompetitionSnowWinnerReviewPolicy)
	if !supported {
		t.Fatal("synthetic frozen provider class is unavailable")
	}
	overlay.TrustedInstrument.BuildReceipt.WinnerReviewPolicy = model.CompetitionSnowWinnerReviewPolicy
	overlay.TrustedInstrument.BuildReceipt.TestModel, overlay.TrustedInstrument.BuildReceipt.TestReasoning = providerModel, providerReasoning
	settings.IntakeFirstPolicy = &model.CompetitionIntakeFirstPolicy{Kind: model.CompetitionIntakeFirstKind, FromEpoch: 2, ApprovalSha256: strings.Repeat("9", 64)}
	intent := *overlay
	intent.Environment, intent.CompetitionId, intent.SeasonId = initial.Environment, initial.CompetitionId, initial.SeasonId
	intent.Generation, intent.ExpectedGeneration, intent.TargetRoundId = 2, 1, server.NewId()
	intent.BaseSourceLockSha256, intent.Threshold, intent.ThresholdEvidenceSha256 = current.SourceLockSha256, current.Threshold, current.ThresholdEvidenceSha256
	intent.RewardDecisionDigest, intent.SourceDecisionDigest = seal.Decisions[len(seal.Decisions)-1].DecisionSha256, strings.Repeat("8", 64)
	intent.PreparedAt, intent.WinnerReviewPolicy = currentTime, model.CompetitionSnowWinnerReviewPolicy
	intent.Manifest, intent.ManifestCommit, intent.ConfigExpectedCommit, intent.PublicationJournalSha256 = []byte("synthetic prospective immutable instrument manifest\n"), strings.Repeat("e", 40), current.ManifestCommit, strings.Repeat("7", 64)
	manifestHash = sha256.Sum256(intent.Manifest)
	intent.ManifestSha256 = hex.EncodeToString(manifestHash[:])
	intent.GitRefs = map[string]model.CompetitionSourceGitRef{}
	for name, commit := range intent.Commits {
		intent.GitRefs[name] = model.CompetitionSourceGitRef{Branch: "synthetic-source", ExpectedCommit: current.Commits[name], Commit: commit}
	}
	intent.GitRefs["config"] = model.CompetitionSourceGitRef{Branch: "synthetic-config", ExpectedCommit: current.ManifestCommit, Commit: intent.ManifestCommit}
	fixture.intent, err = fixture.store.RecordSourceIntent(ctx, settings, *lease, intent)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// Recording is a real guarded store call with synthetic role identity. The
// separate HTTPS tests exercise the concrete API producer/envelope path.
func (self *competitionIntakeFirstDatabaseFixture) acknowledgeApi(t testing.TB, ctx context.Context) {
	t.Helper()
	self.archive = newCompetitionIntakeFirstRetainedArchive(self.settings)
	if _, err := self.archive.publishRuntimeManifest(ctx, self.settings, self.intent); err != nil {
		t.Fatal(err)
	}
	api := competitionPreparationTestReceipt(self.intent, "api", "synthetic-prospective-api", testApiImageDigest())
	if err := self.store.RecordSourceReadiness(ctx, self.settings, api); err != nil {
		t.Fatal(err)
	}
}

// The retained archive consumes real local bytes, unlike the metadata-only
// generator paired with validSettings' ordinary no-op archive. Both boundaries
// move together when this fixture starts testing actual retained publication.
func newCompetitionIntakeFirstRetainedArchive(settings *Settings) *blobArtifactArchive {
	settings.workloadGenerator = ControlPlaneWorkloadGenerator{}
	archive := &blobArtifactArchive{store: &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}}
	settings.artifactArchive = archive
	return archive
}

// Publication is a pinned external boundary in production. This controlled
// witness proves that the actual driver checks human authority before it.
func (self *competitionIntakeFirstDatabaseFixture) handoffAndPublish(t testing.TB, ctx context.Context) {
	t.Helper()
	publications := 0
	driver := &PostgresCompetitionSupervisorDriver{Settings: self.settings, Store: self.store, Hooks: CompetitionSupervisorHooks{PublishSource: func(context.Context, model.CompetitionSupervisorLease, *model.CompetitionSourceActivation) error {
		publications++
		return nil
	}}}
	if err := driver.PublishSource(ctx, self.lease, self.intent); !errors.Is(err, errCompetitionSourceOperatorHandoff) || publications != 0 {
		t.Fatal("source publication did not stop at the actual human barrier", err, publications)
	}
	request := model.CompetitionSourceOperatorHandoffArgs{TargetEpoch: 2, RewardDecisionSha256: self.intent.RewardDecisionDigest,
		ProductSourceLockSha256: competitionIntakeFirstProductLock(self.intent), Disposition: "approve_no_winner_carry", IdempotencyKey: "synthetic-no-winner-handoff", EvidenceSha256: strings.Repeat("6", 64)}
	first, err := self.store.RecordSourceOperatorHandoff(ctx, self.settings, "synthetic-human", request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := self.store.RecordSourceOperatorHandoff(ctx, self.settings, "synthetic-human", request)
	if err != nil || first.RequestSha256 != replay.RequestSha256 || !first.CreatedAt.Equal(replay.CreatedAt) || first.Revision != replay.Revision {
		t.Fatal("human handoff replay changed its durable identity", err)
	}
	if err := driver.PublishSource(ctx, self.lease, self.intent); err != nil || publications != 1 {
		t.Fatal("exact handoff did not release the actual source hook", err)
	}
	_, err = self.store.RecordSupervisorTransition(ctx, self.settings, self.lease, model.CompetitionSupervisorTransition{TargetEpoch: 2, State: "preparation_pending",
		SourceActivationGeneration: 2, SourceChoiceAt: self.intent.PreparedAt, PreparationStartedAt: self.intent.PreparedAt})
	if err != nil {
		t.Fatal(err)
	}
}

type competitionIntakeFirstArchiveBarrier struct {
	artifactArchive
	archived chan struct{}
	release  chan struct{}
}

func (self *competitionIntakeFirstArchiveBarrier) ArchiveRound(ctx context.Context, settings *Settings, round *roundRecord, workload workloadArtifact) error {
	if err := self.artifactArchive.ArchiveRound(ctx, settings, round, workload); err != nil {
		return err
	}
	close(self.archived)
	select {
	case <-self.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Real guarded store opening follows an external archive join and a database
// clock even when the application clock is deliberately behind. No production
// clock, native host, real deployment credential or frozen row is modified.
func TestCompetitionIntakeFirstDatabaseOpensFullWeekAfterArchiveAndRetainsClocklessQueue(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(tb.Context(), 60*time.Second)
		defer cancel()
		fixture := newCompetitionIntakeFirstDatabaseFixture(t, ctx)
		fixture.handoffAndPublish(tb, ctx)
		fixture.acknowledgeApi(tb, ctx)
		// This application clock cannot authorize an opening against the SQL
		// clock. Only the database-owned authority timestamps may do so.
		fixture.now = fixture.now.Add(-2 * time.Minute)
		grant, err := fixture.store.ActivateSourceIntake(ctx, fixture.settings, fixture.lease, 2)
		if err != nil {
			tb.Fatal("API-only purpose could not mint its genuine scoped grant", err)
		}
		if _, err := fixture.store.ActivateSource(ctx, fixture.settings, fixture.lease, 2, 1); !errors.Is(err, ErrReviewNotReady) {
			tb.Fatal("API-only intake fabricated a worker readiness acknowledgment", err)
		}
		barrier := &competitionIntakeFirstArchiveBarrier{artifactArchive: fixture.archive, archived: make(chan struct{}), release: make(chan struct{})}
		openingSettings := *fixture.settings
		openingSettings.artifactArchive = barrier
		type result struct {
			round *RoundResult
			err   error
		}
		done := make(chan result, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			round, err := fixture.store.CreateIntakeFirstRound(ctx, &openingSettings, fixture.lease, 2)
			done <- result{round: round, err: err}
		}()
		defer func() { cancel(); <-joined }()
		select {
		case <-barrier.archived:
		case early := <-done:
			tb.Fatal("application clock rejected opening before the archive boundary", early.err)
		case <-ctx.Done():
			tb.Fatal("owned workload archive did not reach its join boundary")
		}
		var afterArchive time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var err error
			afterArchive, err = competitionIntakeFirstDatabaseTime(ctx, conn)
			server.Raise(err)
		})
		close(barrier.release)
		opened := <-done
		if opened.err != nil || opened.round == nil || opened.round.RoundId != grant.TargetRoundId || opened.round.OpensAt.Before(afterArchive) || opened.round.ClosesAt.Sub(opened.round.OpensAt) != 7*24*time.Hour {
			tb.Fatal("actual full week did not begin after archive join under the DB clock", opened.round, opened.err)
		}
		first := *opened.round
		replay, err := fixture.store.CreateIntakeFirstRound(ctx, fixture.settings, fixture.lease, 2)
		if err != nil || replay == nil || replay.RoundId != first.RoundId || !replay.OpensAt.Equal(first.OpensAt) || !replay.ClosesAt.Equal(first.ClosesAt) || replay.WorkloadCommitment != first.WorkloadCommitment || replay.ProvidersSha256 != first.ProvidersSha256 {
			tb.Fatal("replay moved the date or regenerated the committed workload", replay, err)
		}
		// Only the opening boundary is tested under a skewed application clock;
		// ordinary admission retains its existing clock semantics unchanged.
		server.Db(ctx, func(conn server.PgConn) {
			var err error
			fixture.now, err = competitionIntakeFirstDatabaseTime(ctx, conn)
			server.Raise(err)
		})
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		info, evalError := service.Info(ctx)
		if evalError != nil || info == nil || info.RuntimeStatus == nil || !info.RuntimeStatus.IntakeReady || info.RuntimeStatus.GradingReady || !info.RuntimeStatus.PublicationHeld {
			tb.Fatal("real API discovery conflated intake with grading readiness", info, evalError)
		}
		principal := &Principal{Id: "synthetic-submitter", Role: "submitter"}
		accepted, status, evalError := service.SubmitV2(ctx, ScoreArgs{RoundId: first.RoundId, Patch: testPatch("synthetic-intake-change")}, principal)
		if evalError != nil || status != 202 || accepted == nil || accepted.State != "queued" {
			tb.Fatal("source-pinned intake did not accept its protected queued submission", accepted, status, evalError)
		}
		job, err := fixture.store.GetJob(ctx, fixture.settings, accepted.JobId, principal)
		if err != nil || job == nil || job.StartedAt != nil || job.CycleStartedAt != nil || job.AttemptCount != 0 || job.LeaseOwner != "" {
			tb.Fatal("intake consumed a candidate execution clock", job, err)
		}
		frozen, err := settingsForCompetitionApiRound(ctx, fixture.settings, fixture.store, &job.Round)
		if err != nil {
			tb.Fatal(err)
		}
		frozen.automationLease, frozen.automationRoundId = &fixture.lease, first.RoundId
		if candidate, err := fixture.store.Claim(ctx, frozen, "synthetic-unready-worker", testWorkerImageDigest()); err != nil || candidate != nil {
			tb.Fatal("normal claim consumed a clock before actual grading quorum/baseline", candidate, err)
		}
		poll, code, evalError := service.GetScoreV2(ctx, accepted.JobId, principal)
		if evalError != nil || code != 200 || poll == nil || poll.State != "queued" {
			tb.Fatal("owner could not poll an admitted but ungraded job", poll, code, evalError)
		}
		projection, err := json.Marshal([]any{info, accepted, poll})
		if err != nil || strings.Contains(string(projection), `"revealed_seed"`) || strings.Contains(string(projection), `"providers_url"`) || strings.Contains(string(projection), string(fixture.settings.SeedKey)) {
			tb.Fatal("source-only intake published private workload or key material", err)
		}
		bypass := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_job SET state='running',started_at=timezone('UTC',clock_timestamp()),attempt_count=1 WHERE job_id=$1`, accepted.JobId))
			})
		})
		if bypass == nil || !strings.Contains(bypass.Error(), "intake admission cannot start candidate clocks") {
			tb.Fatal("stale direct SQL writer bypassed the independent grading-purpose barrier", bypass)
		}
	})
}
