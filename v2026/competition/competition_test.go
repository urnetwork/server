package competition

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

type fakeWorkloadGenerator struct{}

func (fakeWorkloadGenerator) Generate(_ context.Context, settings *Settings, roundId server.Id, seedHex string) (workloadArtifact, error) {
	if len(seedHex) != 64 {
		return workloadArtifact{}, errors.New("invalid test seed")
	}
	return workloadArtifact{
		Path:   filepath.Join(settings.ArtifactRoot, "rounds", roundId.String(), "providers.yml"),
		Sha256: strings.Repeat("c", 64),
		Bytes:  1,
	}, nil
}

type fakeArtifactArchive struct{}

func (fakeArtifactArchive) Check(context.Context) error {
	return nil
}

func (fakeArtifactArchive) ArchiveRound(context.Context, *Settings, *roundRecord, workloadArtifact) error {
	return nil
}

func (fakeArtifactArchive) ArchiveSubmission(
	context.Context,
	*Settings,
	server.Id,
	*CanonicalPatch,
) (*retainedArtifact, error) {
	return &retainedArtifact{
		Path: "canonical.patch", Key: "test/submission.patch", Sha256: strings.Repeat("a", 64),
		Bytes: 1, Mode: "LOCAL", RetainUntil: server.NowUtc().Add(time.Hour),
	}, nil
}

func (fakeArtifactArchive) ArchiveAttempt(
	context.Context,
	*Settings,
	*queuedJob,
	string,
	artifactManifest,
) (json.RawMessage, error) {
	return json.RawMessage(`{"schema":1}`), nil
}

func (fakeArtifactArchive) ReadRoundWorkload(context.Context, *Settings, *roundRecord) ([]byte, error) {
	return nil, errors.New("unused")
}

func validSettings() *Settings {
	submitHash := sha256.Sum256([]byte("submit-secret"))
	operatorHash := sha256.Sum256([]byte("operator-secret"))
	return &Settings{
		Enabled:              true,
		CompetitionId:        "sim-latency-season-1",
		BaseSha:              strings.Repeat("a", 40),
		EvaluatorImageDigest: "sha256:" + strings.Repeat("b", 64),
		PatchPolicy: PatchPolicy{
			MaxPatchBytes:  262144,
			AllowedPaths:   []string{"connect/example.go", "model/example.go"},
			ForbiddenPaths: []string{protectedSimulatorTreePattern, "connect/payment/**", "model/payment*.go"},
		},
		EvaluationPolicy: EvaluationPolicy{
			HardwareId:              "xeon-e5-2697v2-12c-v1",
			HostQualificationSha256: strings.Repeat("f", 64),
			ConfigLocalSha256:       strings.Repeat("3", 64),
			VaultLocalSha256:        strings.Repeat("4", 64),
			SimulatorSha256:         strings.Repeat("1", 64), ScorerSha256: strings.Repeat("2", 64),
			ProviderCount: 1800, ClientPoolSize: 200, ArrivalsPerMinute: 80,
			QualityWindowSize: 0, ExchangeHosts: 4, FleetShards: 4,
			SiteListen: "127.0.0.1:0", ApiPort: 7640,
			RampMs: 60000, PrewarmMs: 46800000, SettleMs: 60000, ClientWarmupTimeoutMs: 1200000,
			DurationMs: 1800000, RequestTimeoutMs: 120000, Replicates: 3,
			PipelineIntervalMs: 10000, TestTimeoutMs: 3000, AnnounceTimeoutMs: 2000,
			ImpairmentEnabled: true,
			TakeoverMargin:    .12, QueueLimit: 0, ScoreTimeoutSeconds: submissionEvaluationTimeoutSeconds,
		},
		SeasonPolicy: SeasonPolicy{
			EpochCount: 6, SubmissionWindowSeconds: 7 * 24 * 60 * 60,
			PreparationWindowSeconds: 16 * 60 * 60, SubmissionFeeUsd: 20,
		},
		ArtifactRoot:         "/var/lib/urnetwork/competition",
		ConfigLocalDirectory: "/srv/warp/config/local",
		VaultLocalDirectory:  "/srv/warp/vault/local",
		SeasonEndsAt:         time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		RetainUntil:          time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		WorkerLeaseSeconds:   90, WorkerHeartbeatSeconds: 20,
		HostHeartbeatMaxAgeSeconds: 60, MaxInfrastructureAttempts: 3,
		SimulatorCommand:       "/usr/local/libexec/urnetwork/sim-latency",
		EvaluatorCommand:       "/usr/local/libexec/urnetwork/competition-evaluator",
		EvaluatorCommandSha256: strings.Repeat("d", 64),
		SelfCheckCommand:       "/usr/local/libexec/urnetwork/competition-self-check",
		SelfCheckCommandSha256: strings.Repeat("e", 64),
		Tokens: []Token{
			{Name: "apex", Role: "submitter", Sha256: hex.EncodeToString(submitHash[:])},
			{Name: "ops", Role: "operator", Sha256: hex.EncodeToString(operatorHash[:])},
		},
		SeedKey:           []byte("0123456789abcdef0123456789abcdef"),
		workloadGenerator: fakeWorkloadGenerator{},
		artifactArchive:   fakeArtifactArchive{},
	}
}

func testApiImageDigest() string {
	return "sha256:" + strings.Repeat("6", 64)
}

func testWorkerImageDigest() string {
	return "sha256:" + strings.Repeat("7", 64)
}

func testScoreSignificance(significant bool) *ScoreSignificance {
	baselineVariance := 4.0
	candidateVariance := 4.0
	minimumPercent := 2.0
	requiredPercent := 12.0
	pValue := 0.5
	observedPercent := 5.0
	if significant {
		pValue = 0.01
		observedPercent = 20
	}
	welchT := 3.0
	degreesOfFreedom := 4.0
	nextEpochMinimumPercent := 2.0
	recommendedPercent := 12.0
	return &ScoreSignificance{
		Method:                                      "one-sided-welch-t",
		Alpha:                                       0.05,
		ReplicateCount:                              3,
		BaselineMeanRawScore:                        100,
		CandidateMeanRawScore:                       100 - observedPercent,
		BaselineSampleVariance:                      &baselineVariance,
		CandidateSampleVariance:                     &candidateVariance,
		ObservedImprovementPercent:                  observedPercent,
		TakeoverMarginPercent:                       12,
		MinimumSignificantImprovementPercent:        &minimumPercent,
		RequiredImprovementPercent:                  &requiredPercent,
		OneSidedPValue:                              &pValue,
		WelchT:                                      &welchT,
		WelchDegreesOfFreedom:                       &degreesOfFreedom,
		StatisticallySignificant:                    significant,
		NextEpochMinimumImprovementPercent:          &nextEpochMinimumPercent,
		RecommendedNextEpochTakeoverMarginPercent:   &recommendedPercent,
		RecommendedNextEpochTakeoverMarginSupported: true,
	}
}

func TestRuntimeImageDigestRequiresExactSha256Identity(t *testing.T) {
	want := testApiImageDigest()
	t.Setenv(runtimeImageDigestEnvironment, "  "+want+"\n")
	got, err := runtimeImageDigest()
	if err != nil || got != want {
		t.Fatalf("runtime image digest = %q, %v", got, err)
	}
	t.Setenv(runtimeImageDigestEnvironment, "main-api:latest")
	if _, err := runtimeImageDigest(); err == nil {
		t.Fatal("mutable image tag accepted as runtime identity")
	}
}

func TestEvaluatorRequestBindsControlPlaneImageDigests(t *testing.T) {
	settings := validSettings()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: server.NewId(), RoundId: server.NewId(), PatchSha256: strings.Repeat("8", 64),
			EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest:       testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
		AttemptCount: 1,
		Round:        roundRecord{RoundResult: RoundResult{Epoch: 1}},
	}
	request := evaluatorRequestForJob(settings, job, strings.Repeat("9", 64), "/tmp/attempt", "/tmp/attempt/canonical.patch")
	if request.EvaluatorImageDigest != job.EvaluatorImageDigest ||
		request.ApiImageDigest != job.ApiImageDigest || request.WorkerImageDigest != job.WorkerImageDigest {
		t.Fatalf(
			"request image identity = %q, %q, %q",
			request.EvaluatorImageDigest,
			request.ApiImageDigest,
			request.WorkerImageDigest,
		)
	}
	if request.SourceEpoch != 0 {
		t.Fatalf("request source epoch = %d, want 0", request.SourceEpoch)
	}
}

func TestTimeoutBudgetMatchesEvaluator(t *testing.T) {
	p := validSettings().EvaluationPolicy
	args := []string{
		strconv.FormatInt(p.RampMs, 10),
		strconv.FormatInt(p.SettleMs, 10),
		strconv.FormatInt(p.ClientWarmupTimeoutMs, 10),
		strconv.FormatInt(p.DurationMs, 10),
		strconv.FormatInt(p.RequestTimeoutMs, 10),
	}
	stageOut, err := exec.Command("container/timeout-budget.sh", append([]string{"stage"}, args...)...).Output()
	if err != nil {
		t.Fatalf("stage timeout calculator: %s", err)
	}
	stageSeconds, err := strconv.ParseInt(strings.TrimSpace(string(stageOut)), 10, 64)
	if err != nil {
		t.Fatalf("parse stage timeout: %s", err)
	}
	if want := evaluationStageTimeoutSeconds(p); stageSeconds != want {
		t.Fatalf("shell stage timeout = %d, Go = %d", stageSeconds, want)
	}

}

func TestSettingsValidateFrozenPolicy(t *testing.T) {
	settings := validSettings()
	if err := settings.Validate(); err != nil {
		t.Fatalf("valid settings rejected: %s", err)
	}
	settings.EvaluationPolicy.Replicates = 2
	if err := settings.Validate(); err == nil {
		t.Fatal("even replicate count accepted")
	}
	settings = validSettings()
	settings.RetainUntil = settings.SeasonEndsAt.Add(-time.Second)
	if err := settings.Validate(); err == nil {
		t.Fatal("artifact retention shorter than season accepted")
	}
	settings = validSettings()
	settings.PatchPolicy.ForbiddenPaths = nil
	if err := settings.Validate(); err == nil {
		t.Fatal("empty hard-forbidden path list accepted")
	}
	settings = validSettings()
	settings.PatchPolicy.ForbiddenPaths = []string{"connect/payment/**"}
	if err := settings.Validate(); err == nil {
		t.Fatal("policy without an explicit protected simulator tree accepted")
	}
	settings = validSettings()
	settings.PatchPolicy.AllowedPaths = []string{"connect/**"}
	if err := settings.Validate(); err == nil {
		t.Fatal("glob-expanded editable surface accepted")
	}
	settings = validSettings()
	settings.PatchPolicy.AllowedPaths = []string{"config/local/settings.yml"}
	if err := settings.Validate(); err == nil {
		t.Fatal("non-Go local configuration accepted as editable")
	}
	settings = validSettings()
	settings.PatchPolicy.AllowedPaths = []string{"competition/worker.go"}
	if err := settings.Validate(); err == nil {
		t.Fatal("competition control plane accepted as editable")
	}
	settings = validSettings()
	settings.ConfigLocalDirectory = "/srv/warp/config"
	if err := settings.Validate(); err == nil {
		t.Fatal("parent config directory accepted as the direct local mount")
	}
	settings = validSettings()
	settings.VaultLocalDirectory = "/srv/warp/vault/main"
	if err := settings.Validate(); err == nil {
		t.Fatal("vault/main accepted as the direct local mount")
	}
	settings = validSettings()
	settings.EvaluationPolicy.ConfigLocalSha256 = ""
	if err := settings.Validate(); err == nil {
		t.Fatal("unfrozen direct config/local digest accepted")
	}
	settings = validSettings()
	settings.SeasonPolicy.SubmissionFeeUsd = 19
	if err := settings.Validate(); err == nil {
		t.Fatal("submission fee other than $20 accepted")
	}
	settings = validSettings()
	settings.EvaluationPolicy.QueueLimit = 1
	if err := settings.Validate(); err == nil {
		t.Fatal("bounded epoch queue accepted")
	}
	settings = validSettings()
	settings.EvaluationPolicy.ScoreTimeoutSeconds = submissionEvaluationTimeoutSeconds - 1
	if err := settings.Validate(); err == nil {
		t.Fatal("submission timeout below three hours accepted")
	}
	settings = validSettings()
	settings.EvaluationPolicy.ScoreTimeoutSeconds = submissionEvaluationTimeoutSeconds + 1
	if err := settings.Validate(); err == nil {
		t.Fatal("submission timeout above three hours accepted")
	}
	settings = validSettings()
	settings.EvaluationPolicy.ClientWarmupTimeoutMs = 0
	if err := settings.Validate(); err == nil {
		t.Fatal("unfrozen client warm-up timeout accepted")
	}
	settings = validSettings()
	settings.Tokens[0].Name = "bad token"
	if err := settings.Validate(); err == nil {
		t.Fatal("unsafe token principal name accepted")
	}
}

func TestAuthenticateOpaqueTokens(t *testing.T) {
	settings := validSettings()
	tests := []struct {
		header   string
		wantId   string
		wantRole string
	}{
		{"Bearer submit-secret", "apex", "submitter"},
		{"Bearer operator-secret", "ops", "operator"},
		{"Bearer wrong", "", ""},
		{"bearer submit-secret", "", ""},
		{"Bearer ", "", ""},
		{"Bearer submit-secret trailing", "", ""},
	}
	for _, test := range tests {
		request := httptest.NewRequest("GET", "/competition/readyz", nil)
		request.Header.Set("Authorization", test.header)
		principal, ok := Authenticate(request, settings)
		if test.wantId == "" {
			if ok || principal != nil {
				t.Errorf("Authenticate(%q) unexpectedly succeeded", test.header)
			}
			continue
		}
		if !ok || principal.Id != test.wantId || principal.Role != test.wantRole {
			t.Errorf("Authenticate(%q) = %#v, %v", test.header, principal, ok)
		}
	}
}

func TestSecureEvaluatorChecksRequireUnboundedAdmissionReadiness(t *testing.T) {
	checks := map[string]bool{
		"configuration": true, "database": true, "queue_admission": false,
		"authoritative_evaluator_host": true, "host_rebaseline": true,
	}
	if secureEvaluatorChecksPass(checks) {
		t.Fatal("unavailable admission path was treated as ready")
	}
	checks["queue_admission"] = true
	checks["host_rebaseline"] = false
	if secureEvaluatorChecksPass(checks) {
		t.Fatal("failed evaluator qualification was treated as ready")
	}
}

func TestRoundGenerationReadinessPrecedesRoundRebaseline(t *testing.T) {
	checks := map[string]bool{
		"configuration": true, "database": true, "queue_admission": true,
		"authoritative_evaluator_host": true, "host_rebaseline": false,
	}
	if !roundGenerationChecksPass(checks) {
		t.Fatal("round generation required a same-round baseline before the round existed")
	}
	checks["authoritative_evaluator_host"] = false
	if roundGenerationChecksPass(checks) {
		t.Fatal("round generation accepted an unqualified evaluator boundary")
	}
}

func TestPatchValidationAndCanonicalIdentity(t *testing.T) {
	policy := validSettings().PatchPolicy
	valid := "diff --git a/connect/example.go b/connect/example.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/connect/example.go\n" +
		"+++ b/connect/example.go\n" +
		"@@ -1 +1 @@\n-old\n+new\n"
	patch, evalError := ValidateAndCanonicalizePatch(valid, policy)
	if evalError != nil {
		t.Fatalf("valid patch rejected: %s", evalError)
	}
	digest := sha256.Sum256([]byte(valid))
	if patch.Sha256 != hex.EncodeToString(digest[:]) || !reflect.DeepEqual(patch.Paths, []string{"connect/example.go"}) {
		t.Fatalf("unexpected canonical patch: %#v", patch)
	}

	tests := []struct {
		name  string
		patch string
		code  string
	}{
		{"traversal", strings.Replace(valid, "connect/example.go", "../example.go", 2), "invalid_patch_structure"},
		{"simulator", strings.ReplaceAll(valid, "connect/example.go", "connect/sim-latency/main.go"), "path_not_allowed"},
		{"go-mod", strings.ReplaceAll(valid, "connect/example.go", "go.mod"), "path_not_allowed"},
		{"binary", "diff --git a/connect/example.go b/connect/example.go\nGIT binary patch\n", "binary_patch"},
		{"mode", "diff --git a/connect/example.go b/connect/example.go\nold mode 100644\nnew mode 120000\n", "unsupported_patch_operation"},
		{"rename", "diff --git a/connect/a.go b/connect/b.go\n", "invalid_patch_structure"},
		{"crlf", strings.ReplaceAll(valid, "\n", "\r\n"), "noncanonical_patch"},
		{"no-final-lf", strings.TrimSuffix(valid, "\n"), "noncanonical_patch"},
		{"forbidden-config", strings.ReplaceAll(valid, "connect/example.go", "connect/payment/card.go"), "path_not_allowed"},
		{"hunk-count", strings.Replace(valid, "@@ -1 +1 @@", "@@ -1,2 +1 @@", 1), "invalid_patch_structure"},
		{"trailing-metadata", valid + "new file mode 100644\n", "invalid_patch_structure"},
		{"bad-index", strings.Replace(valid, "index 1111111..2222222 100644", "index nope..2222222 100644", 1), "unsupported_patch_operation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := ValidateAndCanonicalizePatch(test.patch, policy)
			if got == nil || got.Code != test.code {
				t.Fatalf("got %#v, want code %s", got, test.code)
			}
		})
	}
}

func TestPatchValidationAlwaysProtectsSimulatorTree(t *testing.T) {
	paths := []string{
		"connect/sim-latency/main.go",
		"connect/sim-latency/score.go",
		"connect/sim-latency/internal/probe.go",
	}
	for _, filePath := range paths {
		t.Run(filePath, func(t *testing.T) {
			// Deliberately allowlist the protected file and omit it from the
			// caller-supplied denylist. The hard boundary must still win if a
			// malformed policy reaches the standalone image validator.
			policy := PatchPolicy{
				MaxPatchBytes:  4096,
				AllowedPaths:   []string{filePath},
				ForbiddenPaths: []string{"unrelated/**"},
			}
			patch := "diff --git a/" + filePath + " b/" + filePath + "\n" +
				"index 1111111..2222222 100644\n" +
				"--- a/" + filePath + "\n" +
				"+++ b/" + filePath + "\n" +
				"@@ -1 +1 @@\n-old\n+new\n"
			_, evalError := ValidateAndCanonicalizePatch(patch, policy)
			if evalError == nil || evalError.Code != "path_not_allowed" || evalError.Retriable {
				t.Fatalf("protected simulator patch result = %#v", evalError)
			}
		})
	}
}

func TestRoundCommitmentEncryptsAndReveals(t *testing.T) {
	settings := validSettings()
	roundId := server.NewId()
	nonce, ciphertext, commitment, err := createRoundSecret(settings, roundId)
	if err != nil {
		t.Fatal(err)
	}
	if len(ciphertext) <= 32 || strings.Contains(hex.EncodeToString(ciphertext), commitment) {
		t.Fatal("round seed was not authenticated-encrypted")
	}
	round := &roundRecord{RoundResult: RoundResult{RoundId: roundId, WorkloadCommitment: commitment}, SeedNonce: nonce, SeedCiphertext: ciphertext}
	seed, err := revealRoundSecret(settings, round)
	if err != nil || len(seed) != 64 {
		t.Fatalf("reveal = %q, %v", seed, err)
	}
	round.WorkloadCommitment = strings.Repeat("0", 64)
	if _, err := revealRoundSecret(settings, round); err == nil {
		t.Fatal("tampered commitment accepted")
	}
	other := validSettings()
	other.SeedKey = []byte("abcdef0123456789abcdef0123456789")
	if _, err := revealRoundSecret(other, round); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestPolicySnapshotSurvivesJsonbNormalization(t *testing.T) {
	settings := validSettings()
	encoded, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	// jsonb changes key order and whitespace on output.
	var generic any
	if err := jsonUnmarshal(encoded, &generic); err != nil {
		t.Fatal(err)
	}
	normalized, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	if !storedPolicyMatches(settings, normalized) {
		t.Fatal("semantically identical jsonb policy rejected")
	}
	root := generic.(map[string]any)
	root["evaluation_policy"].(map[string]any)["replicates"] = float64(5)
	normalized, err = json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if storedPolicyMatches(settings, normalized) {
		t.Fatal("changed policy accepted")
	}
}

func TestEvaluatorImageDigestFromPolicyRetainsHistoricalIdentity(t *testing.T) {
	settings := validSettings()
	want := settings.EvaluatorImageDigest
	stored, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.EvaluatorImageDigest = "sha256:" + strings.Repeat("c", 64)
	got, err := evaluatorImageDigestFromPolicy(stored)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("historical evaluator image digest = %q, want %q", got, want)
	}
}

func TestEvaluatorImageDigestFromPolicyRejectsUnpinnedIdentity(t *testing.T) {
	stored := json.RawMessage(`{"evaluator_image_digest":"evaluator:latest"}`)
	if _, err := evaluatorImageDigestFromPolicy(stored); err == nil {
		t.Fatal("mutable evaluator image identity accepted from round policy")
	}
}

func jsonUnmarshal(value []byte, out any) error {
	// Kept as a tiny seam so this test never depends on textual field order.
	return json.Unmarshal(value, out)
}

type fakeStore struct {
	mu              sync.Mutex
	claimJobs       []*queuedJob
	claims          int
	heartbeats      int
	completed       []EvaluationOutcome
	handbacks       int
	readiness       map[string]bool
	readyErr        error
	round           *roundRecord
	reviewState     *CandidateReviewState
	reviewErr       error
	reviewCalls     int
	createRound     *roundRecord
	createErr       error
	createRoundArgs []GenerateRoundArgs
	getJob          *queuedJob
	getJobErr       error
}

func (f *fakeStore) CreateRound(_ context.Context, _ *Settings, args GenerateRoundArgs) (*roundRecord, error) {
	f.createRoundArgs = append(f.createRoundArgs, args)
	return f.createRound, f.createErr
}
func (f *fakeStore) CurrentRound(context.Context, *Settings) (*roundRecord, error) {
	return f.round, nil
}
func (f *fakeStore) GetRound(_ context.Context, _ *Settings, roundId server.Id) (*roundRecord, error) {
	if f.round == nil || f.round.RoundId != roundId {
		return nil, ErrNotFound
	}
	return f.round, nil
}
func (f *fakeStore) PrepareCandidateReview(context.Context, *Settings, int) (*CandidateReviewState, error) {
	f.reviewCalls++
	return f.reviewState, f.reviewErr
}
func (f *fakeStore) RecordCandidateReview(context.Context, *Settings, int, CandidateReviewDecision) (*CandidateReviewState, error) {
	return nil, errors.New("unused")
}
func (f *fakeStore) Leaderboards(context.Context, *Settings) (*SeasonLeaderboardResult, error) {
	return &SeasonLeaderboardResult{CompetitionId: "test", Epochs: []LeaderboardResult{}}, nil
}
func (f *fakeStore) Enqueue(context.Context, *Settings, server.Id, *CanonicalPatch, string, string) (*queuedJob, bool, error) {
	return nil, false, errors.New("unused")
}
func (f *fakeStore) GetJob(context.Context, *Settings, server.Id, *Principal) (*queuedJob, error) {
	return f.getJob, f.getJobErr
}
func (f *fakeStore) Readiness(context.Context, *Settings) (map[string]bool, error) {
	return f.readiness, f.readyErr
}
func (f *fakeStore) RegisterHost(context.Context, *Settings, HostSelfCheck) error { return nil }
func (f *fakeStore) Claim(_ context.Context, _ *Settings, _ string, workerImageDigest string) (*queuedJob, error) {
	if f.claims < len(f.claimJobs) {
		job := f.claimJobs[f.claims]
		f.claims++
		job.WorkerImageDigest = workerImageDigest
		return job, nil
	}
	return nil, nil
}
func (f *fakeStore) Heartbeat(context.Context, *Settings, string, server.Id) error {
	f.heartbeats++
	return nil
}
func (f *fakeStore) Complete(_ context.Context, settings *Settings, _ string, _ server.Id, outcome EvaluationOutcome) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, outcome)
	return outcome.Infrastructure && len(f.completed) < settings.MaxInfrastructureAttempts, nil
}
func (f *fakeStore) completedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completed)
}
func (f *fakeStore) outcomes() []EvaluationOutcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EvaluationOutcome(nil), f.completed...)
}
func (f *fakeStore) HandBack(context.Context, string, server.Id, string) error {
	f.handbacks++
	return nil
}

type fakeEvaluator struct {
	check    HostSelfCheck
	outcomes []EvaluationOutcome
	calls    int
}

func (f *fakeEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	return f.check, nil
}
func (f *fakeEvaluator) Evaluate(context.Context, *Settings, *queuedJob) EvaluationOutcome {
	result := f.outcomes[f.calls]
	f.calls++
	return result
}

func passingHostCheck(settings *Settings) HostSelfCheck {
	rebaselineRoundId := server.NewId()
	return HostSelfCheck{
		Schema: 1, HostId: "box-a", HardwareId: settings.EvaluationPolicy.HardwareId,
		QualificationSha256: settings.EvaluationPolicy.HostQualificationSha256,
		ImageDigest:         settings.EvaluatorImageDigest, KernelRelease: "6.8.0", MicrocodeRevision: "0x42", LogicalCpuCount: 12,
		SMTDisabled: true, GovernorPinned: true, TurboPinned: true, NumaPinned: true, IrqPinned: true,
		CgroupV2: true, ServicesInJobCgroup: true, DefaultDenyNetwork: true, OfflineBuildCache: true,
		TemplateDatabase: true, RedisReset: true, ArtifactStorage: true, ImmutableReports: true,
		NoProductionSecrets: true, CleanupVerified: true, ResourceLimitsVerified: true,
		ManagementCpuReserved: true, ManagementMemoryReserved: true, ResourceBombCleanupVerified: true,
		RebaselinePassed: true, RebaselineRoundId: &rebaselineRoundId, Checks: map[string]bool{"all": true},
	}
}

func TestWorkerExitsAfterSealingEpochForHonestyReview(t *testing.T) {
	settings := validSettings()
	current := &roundRecord{RoundResult: RoundResult{
		RoundId: server.NewId(), Epoch: 1, Status: "grading",
	}}
	candidateId := server.NewId()
	store := &fakeStore{
		round: current,
		reviewState: &CandidateReviewState{
			CompetitionId: settings.CompetitionId,
			RoundId:       current.RoundId,
			Epoch:         current.Epoch,
			Status:        "pending_review",
			Candidate:     &CandidateReviewCandidate{Rank: 1, JobId: candidateId},
		},
	}
	evaluator := &fakeEvaluator{check: passingHostCheck(settings)}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "box-a-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claims != 0 || store.reviewCalls != 1 || len(store.createRoundArgs) != 0 {
		t.Fatalf("post-seal work: claims=%d reviews=%d created_rounds=%d", store.claims, store.reviewCalls, len(store.createRoundArgs))
	}
}

func TestWorkerExitsWhenEpochWasAlreadyFinalized(t *testing.T) {
	settings := validSettings()
	finalizedAt := server.NowUtc()
	current := &roundRecord{RoundResult: RoundResult{
		RoundId: server.NewId(), Epoch: 2, Status: "finalized", FinalizedAt: &finalizedAt,
	}}
	store := &fakeStore{round: current}
	worker, err := newWorkerWithImageDigest(settings, store, &fakeEvaluator{}, "box-a-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	finished, err := worker.finishEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !finished || len(store.createRoundArgs) != 0 {
		t.Fatalf("finished=%t created_rounds=%d", finished, len(store.createRoundArgs))
	}
}

func TestScoreResultsRemainEmbargoedUntilWinnerFinalization(t *testing.T) {
	settings := validSettings()
	raw, normalized := 10.0, 112.0
	completedAt := server.NowUtc()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: server.NewId(), RoundId: server.NewId(), State: "succeeded",
			CompletedAt: &completedAt,
			Score: &ScoreResult{
				ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized,
				Placeable: true, TakeoverEligible: true,
				Gates:        map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}},
				Significance: testScoreSignificance(true),
			},
		},
		Round: roundRecord{RoundResult: RoundResult{RevealAt: completedAt.Add(-time.Minute)}},
	}
	service := newServiceWithImageDigest(settings, &fakeStore{getJob: job}, testApiImageDigest(), nil)
	submitter := &Principal{Id: "apex", Role: "submitter"}
	result, status, evalError := service.GetScore(context.Background(), job.JobId, submitter)
	if evalError != nil || status != 200 || result.State != "completed" ||
		result.Score != nil || result.EvalError != nil {
		t.Fatalf("embargoed result = %#v, %d, %#v", result, status, evalError)
	}
	operator := &Principal{Id: "ops", Role: "operator"}
	result, status, evalError = service.GetScore(context.Background(), job.JobId, operator)
	if evalError != nil || status != 200 || result.State != "succeeded" || result.Score == nil {
		t.Fatalf("operator result = %#v, %d, %#v", result, status, evalError)
	}
	job.State = "failed"
	job.Score = nil
	job.EvalError = submissionError("candidate_build_failed", "candidate did not build")
	result, _, _ = service.GetScore(context.Background(), job.JobId, submitter)
	if result.State != "completed" || result.EvalError != nil {
		t.Fatalf("embargoed failure = %#v", result)
	}
	job.Round.FinalizedAt = &completedAt
	result, status, evalError = service.GetScore(context.Background(), job.JobId, submitter)
	if evalError != nil || status != 200 || result.State != "failed" || result.EvalError == nil {
		t.Fatalf("published failure = %#v, %d, %#v", result, status, evalError)
	}
}

func TestInfrastructureRetryHonorsSubmissionExecutionDeadline(t *testing.T) {
	settings := validSettings()
	settings.EvaluationPolicy.ScoreTimeoutSeconds = 120
	startedAt := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		completedAt time.Time
		attempts    int
		wantRetry   bool
	}{
		{completedAt: startedAt.Add(80 * time.Second), attempts: 1, wantRetry: true},
		{completedAt: startedAt.Add(110 * time.Second), attempts: 1, wantRetry: false},
		{completedAt: startedAt.Add(10 * time.Second), attempts: 3, wantRetry: false},
	}
	for _, c := range cases {
		_, retry := infrastructureRetrySchedule(settings, startedAt, c.completedAt, c.attempts)
		if retry != c.wantRetry {
			t.Errorf("retry at completed=%s attempts=%d: got %t, want %t", c.completedAt, c.attempts, retry, c.wantRetry)
		}
	}
}

func TestWorkerDoesNotLaunchExpiredSubmission(t *testing.T) {
	settings := validSettings()
	hostCheck := passingHostCheck(settings)
	startedAt := server.NowUtc().Add(
		-time.Duration(settings.EvaluationPolicy.ScoreTimeoutSeconds+1) * time.Second,
	)
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: server.NewId(), RoundId: *hostCheck.RebaselineRoundId,
			StartedAt: &startedAt, EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest: testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
	}
	store := &fakeStore{}
	evaluator := &fakeEvaluator{}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "box-a-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.evaluateOne(context.Background(), job, hostCheck); err != nil {
		t.Fatal(err)
	}
	outcomes := store.outcomes()
	if evaluator.calls != 0 || len(outcomes) != 1 || outcomes[0].Error == nil ||
		outcomes[0].Error.Code != "evaluation_time_budget_exhausted" {
		t.Fatalf("expired execution: evaluator_calls=%d outcomes=%#v", evaluator.calls, outcomes)
	}
}

func TestHostEligibilityAllowsPreRoundHeartbeatButRejectsMalformedRebaseline(t *testing.T) {
	settings := validSettings()
	check := passingHostCheck(settings)
	check.RebaselinePassed = false
	check.RebaselineRoundId = nil
	if !check.Eligible(settings) {
		t.Fatal("qualified host could not heartbeat before round generation")
	}
	check.RebaselinePassed = true
	if check.Eligible(settings) {
		t.Fatal("host claimed a re-baseline without binding a round")
	}
}

func TestWorkerRetriesInfrastructureUnderSameJob(t *testing.T) {
	settings := validSettings()
	raw, normalized := 10.0, 100.0
	jobId := server.NewId()
	hostCheck := passingHostCheck(settings)
	roundId := *hostCheck.RebaselineRoundId
	startedAt := server.NowUtc()
	jobs := []*queuedJob{
		{ScoreJobResult: ScoreJobResult{
			JobId: jobId, RoundId: roundId, StartedAt: &startedAt,
			EvaluatorImageDigest: settings.EvaluatorImageDigest, ApiImageDigest: testApiImageDigest(),
		}, AttemptCount: 1},
		{ScoreJobResult: ScoreJobResult{
			JobId: jobId, RoundId: roundId, StartedAt: &startedAt,
			EvaluatorImageDigest: settings.EvaluatorImageDigest, ApiImageDigest: testApiImageDigest(),
		}, AttemptCount: 2},
	}
	store := &fakeStore{claimJobs: jobs}
	evaluator := &fakeEvaluator{
		check: hostCheck,
		outcomes: []EvaluationOutcome{
			{Error: infrastructureError("host_transient", "host transient"), Infrastructure: true},
			{Score: &ScoreResult{ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, Gates: map[string]Gate{"g1": {Passed: true, Details: map[string]any{}}}, Significance: testScoreSignificance(false)}, ArtifactManifest: []byte(`{"schema":1}`)},
		},
	}
	worker, err := newWorkerWithImageDigest(settings, store, evaluator, "box-a-worker", testWorkerImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.After(2 * time.Second)
	for store.completedCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("worker did not evaluate both attempts")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	outcomes := store.outcomes()
	if evaluator.calls != 2 || store.claims != 2 || !outcomes[0].Infrastructure || outcomes[1].Score == nil {
		t.Fatalf("unexpected worker sequence: calls=%d claims=%d outcomes=%#v", evaluator.calls, store.claims, outcomes)
	}
}

func TestRevealedRoundWorkloadAuthenticatesCommittedBytes(t *testing.T) {
	settings := validSettings()
	settings.ArtifactRoot = t.TempDir()
	roundId := server.NewId()
	directory := filepath.Join(settings.ArtifactRoot, "rounds", roundId.String())
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	providers := []byte("seed: 48\nfleet: []\n")
	path := filepath.Join(directory, "providers.yml")
	if err := os.WriteFile(path, providers, 0400); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(providers)
	finalizedAt := server.NowUtc()
	round := &roundRecord{
		RoundResult: RoundResult{
			RoundId: roundId, ProvidersSha256: hex.EncodeToString(digest[:]),
			RevealAt: server.NowUtc().Add(-time.Minute), FinalizedAt: &finalizedAt,
		},
		ProvidersPath: path,
	}
	service := newServiceWithImageDigest(settings, &fakeStore{round: round}, testApiImageDigest(), nil)
	got, gotDigest, status, evalError := service.GetRoundWorkload(context.Background(), roundId)
	if evalError != nil || status != 200 || gotDigest != round.ProvidersSha256 || !reflect.DeepEqual(got, providers) {
		t.Fatalf("revealed workload = %q, %q, %d, %#v", got, gotDigest, status, evalError)
	}
	round.FinalizedAt = nil
	if _, _, status, evalError := service.GetRoundWorkload(context.Background(), roundId); status != 409 || evalError == nil || evalError.Code != "round_not_revealed" {
		t.Fatalf("unfinalized workload response = %d, %#v", status, evalError)
	}
}

func TestSeedKeyExampleEncoding(t *testing.T) {
	if got := base64.StdEncoding.EncodeToString(validSettings().SeedKey); got == "" {
		t.Fatal("empty seed-key example")
	}
}
