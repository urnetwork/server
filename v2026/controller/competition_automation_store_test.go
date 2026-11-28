// Disposable-database fixtures exercise v2 serialization, immutable evidence and exact replay contracts.
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

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Direct fixture insertion creates an isolated shadow round; it never converts an existing round.
func newCompetitionAutomationFixture(t testing.TB) *competitionRankingFixture {
	return newCompetitionAutomationFixtureWithMode(t, false)
}

// Each mode creates its own prospective source and immutable round; no policy is rewritten.
func newCompetitionAutomationFixtureWithMode(t testing.TB, staging bool) *competitionRankingFixture {
	return newCompetitionAutomationFixtureWithCapability(t, staging, model.CompetitionAutomationCapability)
}

// Runtime transport is frozen prospectively, before source and round creation.
func newCompetitionAutomationFixtureWithCapability(t testing.TB, staging bool, capability string) *competitionRankingFixture {
	return newCompetitionAutomationFixtureWithWinnerPolicy(t, staging, capability, "", time.Time{})
}

// A new synthetic round freezes its own policy and initial clock before any
// source or round insertion; historical fixtures retain their original values.
func newCompetitionAutomationFixtureWithWinnerPolicy(t testing.TB, staging bool, capability, winnerPolicy string, initialNow time.Time) *competitionRankingFixture {
	t.Helper()
	fixture := &competitionRankingFixture{settings: validSettings(), now: server.NowUtc().Truncate(time.Second)}
	if !initialNow.IsZero() {
		fixture.now = initialNow.UTC().Truncate(time.Second)
	}
	fixture.settings.CompetitionId = "synthetic-automation-" + server.NewId().String()
	fixture.settings.ArtifactRoot = t.TempDir()
	fixture.settings.EvaluationPolicy.Replicates = 9
	fixture.settings.EvaluationPolicy.HardwareId = "synthetic-evaluator"
	fixture.settings.SeasonEndsAt = fixture.now.Add(90 * 24 * time.Hour)
	fixture.settings.RetainUntil = fixture.settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
	fixture.settings.AutomationPolicy = model.CompetitionAutomationPolicy{Enabled: true, Version: 2, Environment: "synthetic-stage", SeasonId: "synthetic-season", ElectionRule: model.CompetitionElectionRule,
		ReviewCriteriaVersion: "synthetic-criteria-v1", SettlementPolicyVersion: "synthetic-settlement-v1", BurnPolicyVersion: "synthetic-burn-v1", RepeatRewardPolicy: "exclude_exact_identity", SettlementMode: "shadow", RequiredApiCapability: capability,
		PolicyApprovalSha256: strings.Repeat("a", 64), SourceLockSha256: strings.Repeat("b", 64), SourceGeneration: 0, SourceActivationGeneration: 1}
	if winnerPolicy != "" {
		fixture.settings.AutomationPolicy.WinnerReviewPolicy, fixture.settings.AutomationPolicy.WinnerReviewTimeoutSeconds = winnerPolicy, 600
	}
	window, preparation := 7*24*time.Hour, 16*time.Hour
	if staging {
		window, preparation = 10*time.Minute, time.Minute
		fixture.settings.AutomationPolicy.Staging = true
		fixture.settings.AutomationPolicy.StagingRoundWindowSeconds = int(window / time.Second)
		fixture.settings.AutomationPolicy.StagingPreparationSeconds = int(preparation / time.Second)
		fixture.settings.SeasonPolicy.SubmissionWindowSeconds = int(window / time.Second)
		fixture.settings.SeasonPolicy.PreparationWindowSeconds = int(preparation / time.Second)
		fixture.settings.SeasonPolicy.SubmissionFeeUsd = 0
		fixture.settings.EvaluationPolicy.TakeoverMargin = 0
	}
	if winnerPolicy != "" {
		preparation = 0
		fixture.settings.AutomationPolicy.StagingPreparationSeconds = 0
		fixture.settings.SeasonPolicy.PreparationWindowSeconds = 0
		if err := model.ValidateCompetitionAutomationPolicy(fixture.settings.AutomationPolicy); err != nil {
			t.Fatal("synthetic frozen winner policy", err)
		}
		t.Log("handoff fixture: frozen winner policy valid; bootstrap source")
	}
	fixture.store = PostgresStore{now: func() time.Time { return fixture.now }}
	bootstrapStore := fixture.store
	if !initialNow.IsZero() {
		// Synthetic round history is independent of current source authority.
		// Never backdate the lease that the real SQL source guard validates.
		var databaseNow time.Time
		server.Db(context.Background(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(context.Background(), `SELECT timezone('UTC',clock_timestamp())`).Scan(&databaseNow))
		})
		bootstrapStore.now = func() time.Time { return databaseNow }
	}
	commits := map[string]string{}
	for _, repository := range []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"} {
		commits[repository] = fixture.settings.BaseSha
	}
	sourceLock, sourceErr := model.CompetitionSourceLockDigest(commits)
	if sourceErr != nil {
		t.Fatal("synthetic source lock", sourceErr)
	}
	fixture.settings.AutomationPolicy.SourceLockSha256 = sourceLock
	lease, sourceErr := bootstrapStore.AcquireAutomationLease(context.Background(), fixture.settings, "synthetic-supervisor", 5*time.Minute)
	if sourceErr != nil {
		t.Fatal("synthetic bootstrap lease", sourceErr)
	}
	manifest := []byte("synthetic immutable source manifest\n")
	manifestDigest := sha256.Sum256(manifest)
	intent := model.CompetitionSourceActivation{Environment: fixture.settings.AutomationPolicy.Environment, CompetitionId: fixture.settings.CompetitionId, SeasonId: fixture.settings.AutomationPolicy.SeasonId,
		WinnerReviewPolicy: winnerPolicy,
		Staging:            staging, StagingRoundWindowSeconds: fixture.settings.AutomationPolicy.StagingRoundWindowSeconds, StagingPreparationSeconds: fixture.settings.AutomationPolicy.StagingPreparationSeconds,
		Generation: 1, ExpectedGeneration: 0, SourceGeneration: 0, TargetEpoch: 1, TargetRoundId: server.NewId(), Kind: "baseline", Reason: "prospectively_approved_baseline", SourceDecisionDigest: strings.Repeat("c", 64), SourceLockSha256: sourceLock, Commits: commits,
		Manifest: manifest, ManifestSha256: hex.EncodeToString(manifestDigest[:]), ManifestCommit: strings.Repeat("c", 40), ConfigExpectedCommit: strings.Repeat("d", 40), PublicationJournalSha256: strings.Repeat("e", 64),
		EvaluatorImageDigest: fixture.settings.EvaluatorImageDigest, HostQualificationSha256: fixture.settings.EvaluationPolicy.HostQualificationSha256, Threshold: fixture.settings.EvaluationPolicy.TakeoverMargin, ThresholdEvidenceSha256: strings.Repeat("f", 64), PreparedAt: fixture.now.Add(-preparation), OpensAt: fixture.now, GitRefs: map[string]model.CompetitionSourceGitRef{}}
	for repository, commit := range commits {
		intent.GitRefs[repository] = model.CompetitionSourceGitRef{Branch: "synthetic-source", ExpectedCommit: commit, Commit: commit}
	}
	intent.GitRefs["config"] = model.CompetitionSourceGitRef{Branch: "synthetic-config", ExpectedCommit: intent.ConfigExpectedCommit, Commit: intent.ManifestCommit}
	if staging {
		for repository, ref := range intent.GitRefs {
			ref.Branch = "sim-latency-staging"
			if repository == "config" {
				ref.Branch = "main"
			}
			intent.GitRefs[repository] = ref
		}
	}
	if winnerPolicy != "" {
		if err := model.ValidateCompetitionAutomationPolicy(fixture.settings.AutomationPolicy); err != nil {
			t.Fatal("frozen winner policy immediately before source intent", err)
		}
	}
	if _, sourceErr = fixture.store.RecordSourceIntent(context.Background(), fixture.settings, *lease, intent); sourceErr != nil {
		t.Fatal("synthetic baseline source intent", sourceErr)
	}
	for _, role := range []string{"api", "worker"} {
		sourceErr = fixture.store.RecordSourceReadiness(context.Background(), fixture.settings, model.CompetitionSourceReadiness{
			Staging:     staging,
			Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: 1, TargetRoundId: intent.TargetRoundId, Role: role, ActorId: "synthetic-" + role, ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256,
			SourceLockSha256: sourceLock, RuntimeImageDigest: testWorkerImageDigest(), QualificationSha256: intent.HostQualificationSha256, Ready: true})
		if sourceErr != nil {
			t.Fatal("synthetic baseline readiness", role, sourceErr)
		}
	}
	activatedSource, sourceErr := fixture.store.ActivateSource(context.Background(), fixture.settings, *lease, 1, 0)
	if sourceErr != nil {
		t.Fatal("synthetic baseline activation", sourceErr)
	}
	var err error
	fixture.settings.automationTargetRoundId = intent.TargetRoundId
	fixture.round, err = fixture.store.prepareRound(context.Background(), fixture.settings, GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(window), RevealAt: fixture.now.Add(window + time.Hour)}, staging)
	if err != nil {
		t.Fatal("synthetic immutable round preparation", err)
	}
	fixture.round.Epoch = 1
	if activatedSource == nil || activatedSource.ActivatedAt == nil || !activatedSource.ActivatedAt.Equal(fixture.now) ||
		fixture.round.OpensAt.Before(*activatedSource.ActivatedAt) || fixture.round.OpensAt.Before(intent.PreparedAt.Add(preparation)) ||
		fixture.round.ClosesAt.Sub(fixture.round.OpensAt) != window || fixture.round.RevealAt.Before(fixture.round.ClosesAt) {
		t.Fatal("synthetic source/round historical schedule differs before guarded insertion")
	}
	if winnerPolicy != "" {
		t.Log("handoff fixture: historical source/round schedule exact; current bootstrap lease separate")
	}
	server.Tx(context.Background(), func(conn server.PgTx) {
		server.Raise(bootstrapStore.requireAutomationLeaseInTx(context.Background(), conn, fixture.settings, *lease))
		round := fixture.round
		var readyBeforeOpen int
		server.Raise(conn.QueryRow(context.Background(), `SELECT count(*) FROM competition_source_readiness
		 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4 AND created_at<=$5
		 AND (receipt_json->>'recorded_at')::timestamp=created_at`, lease.Environment, lease.CompetitionId, lease.SeasonId, intent.Generation, round.OpensAt).Scan(&readyBeforeOpen))
		if readyBeforeOpen != 2 {
			panic(errors.New("synthetic source readiness does not precede its round opening"))
		}
		server.RaisePgResult(conn.Exec(context.Background(), `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
	 VALUES($1,$2,1,$13,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false)`, round.RoundId, round.CompetitionId, round.WorkloadCommitment, round.SeedNonce, round.SeedCiphertext, round.ProvidersSha256, round.ProvidersPath, string(round.PolicyJson), round.OpensAt, round.ClosesAt, round.RevealAt, round.CreatedAt, staging))
	})
	if initialNow.IsZero() {
		// Historical fixtures keep their original synthetic lease boundary.
		fixture.now = lease.LeaseExpiresAt
	} else {
		// Release only this test-owned bootstrap fence after setup. Preserve
		// historical submission times; later review takes a real fresh lease.
		server.Tx(context.Background(), func(tx server.PgTx) {
			automationScopeLockInTx(context.Background(), tx, fixture.settings)
			updated, err := tx.Exec(context.Background(), `UPDATE competition_supervisor_lease SET lease_expires_at=timezone('UTC',clock_timestamp())
			 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5`, lease.Environment, lease.CompetitionId, lease.SeasonId, lease.OwnerId, lease.FencingToken)
			server.Raise(err)
			if updated.RowsAffected() != 1 {
				panic(errors.New("synthetic bootstrap lease was not exactly released"))
			}
		})
		fixture.now = fixture.now.Add(5 * time.Minute)
	}
	return fixture
}

// Uses a real terminal completion transaction and exact archived score-byte digest.
func addCompetitionAutomationJob(t testing.TB, fixture *competitionRankingFixture, raw float64) *queuedJob {
	return addCompetitionAutomationJobWithManifest(t, fixture, raw, nil)
}

// Optional archive construction happens before Complete, just as a real worker
// uploads authenticated evidence before atomically making a job terminal.
func addCompetitionAutomationJobWithManifest(t testing.TB, fixture *competitionRankingFixture, raw float64, archive func(*queuedJob, *ScoreResult) (json.RawMessage, error)) *queuedJob {
	t.Helper()
	fixture.now = fixture.now.Add(time.Second)
	leaseUntil := fixture.now.Add(time.Duration(fixture.settings.WorkerLeaseSeconds) * time.Second)
	jobId := server.NewId()
	patch := []byte("synthetic-automation-patch-" + jobId.String())
	digest := sha256.Sum256(patch)
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: fixture.round.RoundId, PatchSha256: hex.EncodeToString(digest[:]), SubmittedAt: fixture.now}, Patch: patch, AttemptCount: 1, Round: *fixture.round}
	job.LeaseExpiresAt = &leaseUntil
	server.Tx(context.Background(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(context.Background(), `SET LOCAL urnetwork.competition_v2_worker='on'`))
		server.RaisePgResult(tx.Exec(context.Background(), `INSERT INTO competition_job(job_id,round_id,patch_bytes,patch_sha256,cache_key,state,submitted_at,available_at,started_at,lease_owner,lease_expires_at,attempt_count,artifact_retain_until,api_image_digest,worker_image_digest)
	 VALUES($1,$2,$3,$4,$4,'running',$5,$5,$5,'synthetic-worker',$9,1,$6,$7,$8)`, jobId, job.RoundId, patch, job.PatchSha256, fixture.now, fixture.settings.RetainUntil, testApiImageDigest(), testWorkerImageDigest(), leaseUntil))
		server.RaisePgResult(tx.Exec(context.Background(), `UPDATE competition_worker_slot
		 SET worker_id='synthetic-worker',job_id=$1,lease_expires_at=$2,heartbeat_at=$3 WHERE slot_id=1`, jobId, leaseUntil, fixture.now))
	})
	score := competitionRankingScore(raw, 120)
	if fixture.settings.AutomationPolicy.Staging {
		score.TakeoverEligible = false
		score.Significance = testScoreSignificance(false)
		score.Significance.TakeoverMarginPercent = 0
	}
	score.Significance.ReplicateCount = 9
	score.Significance.CandidateMeanRawScore = raw + 1
	score.Gates = map[string]Gate{}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		score.Gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: score})
	content, err := json.MarshalIndent(score, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	scoreDigest := sha256.Sum256(content)
	manifest, err := json.Marshal(artifactManifest{Schema: 1, JobId: jobId.String(), RoundId: job.RoundId.String(), Attempt: 1, PatchSha256: job.PatchSha256, Artifacts: []evaluationArtifact{{Path: "score.json", Sha256: hex.EncodeToString(scoreDigest[:]), Bytes: int64(len(content))}}})
	if err != nil {
		t.Fatal(err)
	}
	outcome.ArtifactManifest = manifest
	if archive != nil {
		outcome.ArtifactManifest, err = archive(job, score)
		if err != nil {
			t.Fatal(err)
		}
	}
	fixture.now = fixture.now.Add(time.Second)
	if retry, err := fixture.store.Complete(context.Background(), fixture.settings, "synthetic-worker", jobId, outcome); err != nil || retry {
		t.Fatalf("complete shadow job retry=%t error=%v", retry, err)
	}
	fixture.jobs = append(fixture.jobs, job)
	return job
}

// Every criterion has structured evidence; this helper never fabricates a legacy approval row.
func competitionAutomationReviewRequest(result *model.CompetitionV2RoundResult, entry model.CompetitionSealedEntry, disqualify bool) model.CompetitionIntegrityReviewRequest {
	request := model.CompetitionIntegrityReviewRequest{IdempotencyKey: "synthetic-review-" + server.NewId().String(), ExpectedRevision: result.RewardRevision, JobId: entry.JobId,
		ProductionBenefitAddendum: model.CompetitionProductionBenefitReviewAddendum(),
		PatchSha256:               entry.PatchSha256, BaseSourceLockSha256: entry.BaseSourceLockSha256, ScoreSha256: entry.ScoreSha256, BaselineSha256: result.Seal.BaselineSha256, ArtifactManifestSha256: entry.ArtifactManifestSha256,
		CriteriaVersion: "synthetic-criteria-v1", RewardDisposition: "approved", SourceDisposition: "deferred", ReasonCodes: []string{"APPROVED"}, PublicExplanation: "Synthetic deterministic review evidence.", WithheldEvidenceExplanation: "Synthetic private evidence retained for the test.", PrivateEvidenceSha256: strings.Repeat("d", 64)}
	if disqualify {
		request.RewardDisposition = "disqualified"
		request.SourceDisposition = "rejected"
		request.ReasonCodes = []string{"SCORE_TAMPERING"}
	}
	for _, criterion := range []string{"measurement_integrity", "workload_generalization", "containment", "patch_scope", "reproducibility", "source_safety"} {
		request.Findings = append(request.Findings, model.CompetitionReviewFinding{Criterion: criterion, Disposition: "pass", ReasonCode: request.ReasonCodes[0], Explanation: "Synthetic criterion inspected.", EvidenceSha256: strings.Repeat("e", 64)})
	}
	return request
}

// Close, drain and reveal are independent, and repeated sealing cannot create another event.
func TestCompetitionAutomationSealPublicationAndReplay(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 80)
		if _, err := fixture.store.SealAndElect(context.Background(), fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrReviewNotReady) {
			t.Fatalf("open seal error=%v", err)
		}
		fixture.now = fixture.round.ClosesAt.Add(time.Second)
		result, err := fixture.store.SealAndElect(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Decisions) != 1 || result.ProvisionalRewardWinner == nil {
			t.Fatalf("seal=%+v", result)
		}
		repeated, err := fixture.store.SealAndElect(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil || repeated.Seal.SealSha256 != result.Seal.SealSha256 || len(repeated.Decisions) != 1 {
			t.Fatalf("seal replay=%+v %v", repeated, err)
		}
		if _, err := fixture.store.AutomationResult(context.Background(), fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrAutomationNotPublished) {
			t.Fatalf("pre-reveal public result error=%v", err)
		}
		page, err := fixture.store.RewardEvents(context.Background(), fixture.settings, 0, 200)
		if err != nil || len(page.Events) != 0 || page.NextCursor != 0 {
			t.Fatalf("embargo feed=%+v %v", page, err)
		}
		fixture.now = fixture.round.RevealAt
		public, err := fixture.store.AutomationResult(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil || public.RewardRevision != 1 {
			t.Fatalf("public=%+v %v", public, err)
		}
		round, err := fixture.store.GetRound(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil || round.FinalizedAt != nil || round.WinnerJobId != nil || !roundPublished(round, fixture.now) {
			t.Fatalf("legacy state changed: %+v %v", round, err)
		}
		if _, err := fixture.store.PrepareCandidateReview(context.Background(), fixture.settings, 1); !errors.Is(err, ErrUnsupportedDecisionMode) {
			t.Fatalf("legacy review accepted v2: %v", err)
		}
	})
}

// Retrospective rejection advances once, approval does not issue another entitlement, and receipts never regress.
func TestCompetitionAutomationReviewRevisionsAndOutOfOrderReceipts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt
		result, err := fixture.store.SealAndElect(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		request := competitionAutomationReviewRequest(result, result.Seal.Entries[0], true)
		review, err := fixture.store.RecordIntegrityReview(context.Background(), fixture.settings, fixture.round.RoundId, "synthetic-reviewer", request)
		if err != nil || review.RewardDecision.Revision != 2 || review.RewardDecision.SelectedJobId == nil || *review.RewardDecision.SelectedJobId != result.Seal.Entries[1].JobId {
			t.Fatalf("replacement=%+v %v", review, err)
		}
		replay, err := fixture.store.RecordIntegrityReview(context.Background(), fixture.settings, fixture.round.RoundId, "synthetic-reviewer", request)
		if err != nil || replay.ReviewId != review.ReviewId {
			t.Fatalf("review replay=%+v %v", replay, err)
		}
		changed := request
		changed.PublicExplanation = "Conflicting synthetic payload."
		if _, err := fixture.store.RecordIntegrityReview(context.Background(), fixture.settings, fixture.round.RoundId, "synthetic-reviewer", changed); !errors.Is(err, ErrConflict) {
			t.Fatalf("conflicting replay error=%v", err)
		}
		result, err = fixture.store.AutomationResult(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		approval := competitionAutomationReviewRequest(result, result.Seal.Entries[1], false)
		approved, err := fixture.store.RecordIntegrityReview(context.Background(), fixture.settings, fixture.round.RoundId, "synthetic-reviewer", approval)
		if err != nil || approved.RewardDecision.Revision != 2 {
			t.Fatalf("approval elected again: %+v %v", approved, err)
		}
		page, err := fixture.store.RewardEvents(context.Background(), fixture.settings, 0, 200)
		if err != nil || len(page.Events) != 2 {
			t.Fatalf("events=%+v %v", page, err)
		}
		event := page.Events[1]
		receipt := model.CompetitionSettlementReceipt{EventId: event.EventId, Environment: event.Decision.Environment, CompetitionId: event.Decision.CompetitionId, SeasonId: event.Decision.SeasonId, RoundId: event.Decision.RoundId, Revision: 2, PayloadSha256: event.PayloadSha256, DecisionSha256: event.Decision.DecisionSha256, Status: "confirmed", AccountingReference: "synthetic-confirmation", AppliedAt: &fixture.now}
		saved, err := fixture.store.RecordSettlementReceipt(context.Background(), fixture.settings, "synthetic-apex", receipt)
		if err != nil {
			t.Fatal(err)
		}
		replayReceipt, err := fixture.store.RecordSettlementReceipt(context.Background(), fixture.settings, "synthetic-apex", receipt)
		if err != nil || !saved.ReceivedAt.Equal(replayReceipt.ReceivedAt) {
			t.Fatalf("receipt replay=%+v %v", replayReceipt, err)
		}
		receipt.Status = "received"
		receipt.AppliedAt = nil
		receipt.AccountingReference = ""
		if _, err := fixture.store.RecordSettlementReceipt(context.Background(), fixture.settings, "synthetic-apex", receipt); err != nil {
			t.Fatal(err)
		}
		result, err = fixture.store.AutomationResult(context.Background(), fixture.settings, fixture.round.RoundId)
		if err != nil || result.SettlementStatus != "confirmed" || len(result.Decisions) != 2 {
			t.Fatalf("late receipt regressed result=%+v %v", result, err)
		}
	})
}

// Durable fences reject a stale owner without depending on sleeps or scheduler timing.
func TestCompetitionAutomationSupervisorLeaseTakeoverFencesOldOwner(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		fixture := newCompetitionAutomationFixture(t)
		first, err := fixture.store.AcquireAutomationLease(context.Background(), fixture.settings, "synthetic-owner-a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.AcquireAutomationLease(context.Background(), fixture.settings, "synthetic-owner-b", time.Minute); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("concurrent owner acquired lease: %v", err)
		}
		fixture.now = first.LeaseExpiresAt
		second, err := fixture.store.AcquireAutomationLease(context.Background(), fixture.settings, "synthetic-owner-b", time.Minute)
		if err != nil || second.FencingToken <= first.FencingToken {
			t.Fatalf("takeover=%+v %v", second, err)
		}
		if _, err := fixture.store.RenewAutomationLease(context.Background(), fixture.settings, *first, time.Minute); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale renewal=%v", err)
		}
		transition := model.CompetitionSupervisorTransition{TargetEpoch: 1, ExpectedRevision: 0, State: "source_choice_pending"}
		if _, err := fixture.store.RecordSupervisorTransition(context.Background(), fixture.settings, *first, transition); !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("stale transition=%v", err)
		}
		if _, err := fixture.store.RecordSupervisorTransition(context.Background(), fixture.settings, *second, transition); err != nil {
			t.Fatal(err)
		}
	})
}
