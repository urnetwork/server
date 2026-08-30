package controller

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"io"
	"math"
	"net/http"
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
)

func writeArchiveTestFile(t *testing.T, root string, path string, content []byte) evaluationArtifact {
	t.Helper()
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return evaluationArtifact{
		Path: path, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content)),
	}
}

func TestBlobArtifactArchiveRetainsSubmissionBeforeEvaluation(t *testing.T) {
	settings := validSettings()
	settings.RetainUntil = server.NowUtc().Add(24 * time.Hour).Truncate(time.Second)
	store := server.NewLocalBlobStore(t.TempDir(), "evidence").(server.RetainedBlobStore)
	archive := &blobArtifactArchive{store: store}
	settings.artifactArchive = archive

	patchBytes := []byte("diff --git a/connect/example.go b/connect/example.go\n")
	digest := sha256.Sum256(patchBytes)
	patch := &CanonicalPatch{Bytes: patchBytes, Sha256: hex.EncodeToString(digest[:])}
	retained, err := archive.ArchiveSubmission(
		context.Background(),
		settings,
		server.NewId(),
		patch,
	)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Path != "canonical.patch" || retained.Sha256 != patch.Sha256 ||
		retained.Bytes != int64(len(patch.Bytes)) || retained.Mode != "LOCAL" ||
		!strings.Contains(retained.Key, "/submissions/sha256/"+patch.Sha256+"/") {
		t.Fatalf("submission retention = %+v", retained)
	}
	reader, err := store.GetVersion(context.Background(), retained.Key, retained.VersionId)
	if err != nil {
		t.Fatal(err)
	}
	retainedBytes, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(retainedBytes, patchBytes) {
		t.Fatalf("retained submission = %q, read=%v close=%v", retainedBytes, err, closeErr)
	}
}

func TestBlobArtifactArchiveRetainsAuthenticatedAttemptWithoutSeedRequest(t *testing.T) {
	settings := validSettings()
	settings.RetainUntil = server.NowUtc().Add(24 * time.Hour).Truncate(time.Second)
	store := server.NewLocalBlobStore(t.TempDir(), "evidence").(server.RetainedBlobStore)
	archive := &blobArtifactArchive{store: store}
	settings.artifactArchive = archive

	roundId, jobId := server.NewId(), server.NewId()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: jobId, RoundId: roundId, PatchSha256: strings.Repeat("a", 64),
			EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest:       testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
		AttemptCount: 1,
	}
	attemptDirectory := t.TempDir()
	declared := writeArchiveTestFile(t, attemptDirectory, "evidence/accounting.json", []byte("{\"cpu\":1}\n"))
	patch := writeArchiveTestFile(t, attemptDirectory, "canonical.patch", []byte("patch\n"))
	result := writeArchiveTestFile(t, attemptDirectory, "worker-result.json", []byte("{\"schema\":1}\n"))
	stderr := writeArchiveTestFile(t, attemptDirectory, "worker.stderr.log", nil)
	writeArchiveTestFile(t, attemptDirectory, "worker-request.json", []byte("{\"round_seed_hex\":\"secret\"}\n"))
	job.PatchSha256 = patch.Sha256

	manifestBytes, err := archive.ArchiveAttempt(context.Background(), settings, job, attemptDirectory, artifactManifest{
		Schema: 1, JobId: jobId.String(), RoundId: roundId.String(), Attempt: 1,
		EvaluatorImageDigest: job.EvaluatorImageDigest,
		ApiImageDigest:       job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest,
		PatchSha256: patch.Sha256, ResultSha256: result.Sha256,
		StderrSha256: stderr.Sha256, Artifacts: []evaluationArtifact{declared},
	})
	if err != nil {
		t.Fatal(err)
	}
	var manifest artifactManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Retention == nil || manifest.Retention.HiddenSeedRequestRetained ||
		!manifest.Retention.AuthenticatedAfterUpload || manifest.Retention.ComplianceObjectLock ||
		manifest.Retention.ObjectCount != 4 {
		t.Fatalf("retention manifest = %+v", manifest.Retention)
	}
	if manifest.EvaluatorImageDigest != job.EvaluatorImageDigest ||
		manifest.ApiImageDigest != job.ApiImageDigest || manifest.WorkerImageDigest != job.WorkerImageDigest {
		t.Fatal("retention manifest lost evaluation image provenance")
	}
	objects, err := store.List(context.Background(), "evidence/competition/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 5 {
		t.Fatalf("retained object count = %d, want four artifacts plus manifest", len(objects))
	}
	for _, object := range objects {
		if strings.Contains(object.Key, "worker-request") || strings.Contains(object.Key, "round_seed") {
			t.Fatalf("hidden-seed request escaped into retained storage: %s", object.Key)
		}
	}
}

func TestBlobArtifactArchiveRestoresRoundWorkloadByCommittedHash(t *testing.T) {
	settings := validSettings()
	settings.RetainUntil = server.NowUtc().Add(24 * time.Hour).Truncate(time.Second)
	store := server.NewLocalBlobStore(t.TempDir(), "evidence").(server.RetainedBlobStore)
	archive := &blobArtifactArchive{store: store}
	settings.artifactArchive = archive
	settings.ArtifactRoot = t.TempDir()

	roundId := server.NewId()
	directory := filepath.Join(settings.ArtifactRoot, "rounds", roundId.String())
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	providers := []byte("seed: 7\nfleet: []\n")
	artifact := writeArchiveTestFile(t, directory, "providers.yml", providers)
	round := &roundRecord{
		RoundResult:   RoundResult{RoundId: roundId, ProvidersSha256: artifact.Sha256},
		ProvidersPath: filepath.Join(directory, "providers.yml"),
	}
	if err := archive.ArchiveRound(context.Background(), settings, round, workloadArtifact{
		Path: round.ProvidersPath, Sha256: artifact.Sha256, Bytes: artifact.Bytes,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(round.ProvidersPath); err != nil {
		t.Fatal(err)
	}
	restored, err := readRoundWorkload(context.Background(), settings, round)
	if err != nil || string(restored) != string(providers) {
		t.Fatalf("restored workload = %q, %v", restored, err)
	}
}

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
	stageOut, err := exec.Command("../connect/sim-latency/evaluator/container/timeout-budget.sh", append([]string{"stage"}, args...)...).Output()
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

func TestThreeHourEvaluationDeadlineBoundsInfrastructureRetries(t *testing.T) {
	settings := validSettings()
	if settings.EvaluationPolicy.ScoreTimeoutSeconds != 3*60*60 {
		t.Fatalf("evaluation timeout = %ds, want 3h", settings.EvaluationPolicy.ScoreTimeoutSeconds)
	}
	startedAt := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		completedAt time.Time
		attempts    int
		wantRetry   bool
	}{
		{completedAt: startedAt.Add(2 * time.Hour), attempts: 1, wantRetry: true},
		{completedAt: startedAt.Add(3*time.Hour - 10*time.Second), attempts: 1, wantRetry: false},
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

type dockerInstruction struct {
	stage string
	user  string
	text  string
}

// Candidate package initialization runs when the compile-only test binary is
// started. It must be unprivileged and its entire filesystem must be discarded
// before the final submission image is assembled.
func TestSubmissionDockerfileIsolatesCandidateExecution(t *testing.T) {
	instructions := readDockerInstructions(t, "../connect/sim-latency/evaluator/container/Dockerfile.submission")
	stages := map[string]string{}
	var finalCopies []string
	checkDependency := false
	for _, instruction := range instructions {
		fields := strings.Fields(instruction.text)
		if len(fields) >= 4 && fields[0] == "FROM" && strings.EqualFold(fields[len(fields)-2], "AS") {
			stages[fields[len(fields)-1]] = fields[1]
		}
		if strings.HasPrefix(instruction.text, "RUN ") &&
			(strings.Contains(instruction.text, "go vet ") ||
				strings.Contains(instruction.text, "go test ") ||
				strings.Contains(instruction.text, "go build ")) && instruction.user != "65532:65532" {
			t.Fatalf("candidate command runs as %q in stage %q: %s", instruction.user, instruction.stage, instruction.text)
		}
		if instruction.stage == "final" && strings.HasPrefix(instruction.text, "COPY ") {
			finalCopies = append(finalCopies, instruction.text)
		}
		if instruction.stage == "candidate-binary" &&
			strings.HasPrefix(instruction.text, "COPY ") &&
			strings.Contains(instruction.text, "--from=candidate-check") &&
			strings.Contains(instruction.text, "/opt/urnetwork/candidate-check/passed") {
			checkDependency = true
		}
		if instruction.stage == "final" && strings.Contains(instruction.text, "--from=candidate-check") {
			t.Fatal("final image imports the candidate-check filesystem")
		}
	}

	if stages["candidate-check"] != "source-prep" ||
		stages["candidate-binary"] != "source-prep" ||
		stages["final"] != "source-prep" {
		t.Fatalf("untrusted stages do not branch independently from source-prep: %#v", stages)
	}
	if !checkDependency {
		t.Fatal("candidate binary stage does not require the discarded check stage to pass")
	}
	if len(finalCopies) != 1 ||
		!strings.Contains(finalCopies[0], "--from=candidate-binary") ||
		!strings.Contains(finalCopies[0], "/opt/urnetwork/candidate-build/output/sim-latency") ||
		!strings.HasSuffix(finalCopies[0], "/opt/urnetwork/bin/sim-latency") {
		t.Fatalf("final image imports more than the isolated candidate binary: %#v", finalCopies)
	}
}

func TestSubmissionBuilderProtectsSimulatorTree(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/build-submission.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`protected_simulator_tree="$(git -C "$source_root/server" rev-parse HEAD:connect/sim-latency)"`,
		`git -C "$source_root/server" diff --quiet -- connect/sim-latency`,
		`status --porcelain=v1 --untracked-files=all -- connect/sim-latency`,
		`[ "$(git -C "$source_root/server" rev-parse HEAD:connect/sim-latency)" = "$protected_simulator_tree" ]`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("submission builder is missing protected-tree check %q", required)
		}
	}
}

// The development smoke validates containment and scorer plumbing, not the
// production load frontier. Keep its small fleet away from artificial churn,
// scheduler-scale timeouts, and client/lane reuse that manufactures contract
// contention under the unchanged 97% scorer floor.
func TestContainerSmokeUsesStablePlumbingProfile(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/smoke-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	requiredCounts := map[string]int{
		"--count=32":                     1,
		"--clients=8":                    1,
		"--rate=16":                      1,
		"--quality-window=8":             1,
		"'APEX_TEST_TIMEOUT=3s'":         2,
		"'APEX_ANNOUNCE_TIMEOUT=2s'":     2,
		"'APEX_PIPELINE_INTERVAL=100ms'": 2,
	}
	for value, want := range requiredCounts {
		if got := strings.Count(script, value); got != want {
			t.Errorf("smoke profile %q count = %d, want %d", value, got, want)
		}
	}
	for _, forbidden := range []string{
		"--count=8",
		"--clients=2",
		"--quality-window=2",
		"--count=128",
		"--clients=16",
		"--rate=30",
		"--rate=120",
		"APEX_TEST_TIMEOUT=10ms",
		"APEX_ANNOUNCE_TIMEOUT=10ms",
		"s/^      uptime_s:",
		"s/^      downtime_s:",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("smoke profile reintroduced unstable setting %q", forbidden)
		}
	}
}

// Every trusted build input participates in both the build record check and
// the runtime identity check before an untrusted candidate can execute.
func TestEvaluatorAuthenticatesEveryCandidateBuildInput(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)

	requiredChecks := []string{
		`[ "$(jq -er '.base_image_id' "$candidate_build_json")" = "$base_image_id" ]`,
		`[ "$(jq -er '.base_sha' "$candidate_build_json")" = "$base_sha" ]`,
		`[ "$(jq -er '.patch_sha256' "$candidate_build_json")" = "$patch_sha256" ]`,
		`[ "$(jq -er '.policy_sha256' "$candidate_build_json")" = "$policy_sha256" ]`,
		`[ "$(jq -er '.builder_sha256' "$candidate_build_json")" = "$builder_sha256" ]`,
		`[ "$(jq -er '.image_key' "$candidate_build_json")" = "$image_key" ]`,
		`.policy_sha256 == $policy_sha`,
		`.builder_sha256 == $builder_sha`,
		`.image_key == $image_key`,
	}
	for _, requiredCheck := range requiredChecks {
		if !strings.Contains(script, requiredCheck) {
			t.Errorf("evaluator is missing candidate identity check %q", requiredCheck)
		}
	}
}

func TestEvaluatorPublishesContainedInternalLiveProgress(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`progress_path="$artifact_dir/evaluation-progress.json"`,
		`kind:"sim-latency-evaluation-progress"`,
		`write_evaluation_progress preparing 0 0`,
		`record_stage_progress "$role" "$index" "$run_dir/run.json"`,
		`--network none --read-only --user 65532:65532`,
		`--memory "$SCORER_MEMORY_BYTES" --memory-swap "$SCORER_MEMORY_BYTES"`,
		`--entrypoint /opt/urnetwork/bin/sim-latency`,
		`candidate_manifests`,
		`baseline_manifests`,
		`evaluation-progress.json evaluation.complete.json`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("live evaluation progress is missing %q", required)
		}
	}
	for _, metric := range []string{
		"ttfb_p50_ms",
		"ttfb_p95_ms",
		"throughput_p50_bytes_per_s",
		"throughput_p95_bytes_per_s",
	} {
		if !strings.Contains(script, metric) {
			t.Errorf("live evaluation progress omits %s", metric)
		}
	}
}

// Runner and API/worker source must have disjoint lifecycles. Each attempt
// materializes both sides of the A/B pair from the immutable evaluator image,
// checks out local sim-latency branches in that tmpfs, mounts the selected tree
// read-only, and removes the source before durable evidence is copied.
func TestEvaluatorUsesAttemptLocalSourceCheckouts(t *testing.T) {
	prepareBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/prepare-evaluation-source.sh")
	if err != nil {
		t.Fatal(err)
	}
	prepare := string(prepareBytes)
	for _, required := range []string{
		`readonly REPOSITORIES=(server connect sdk proxy)`,
		`docker cp "$source_container:/workspace/$repository" "$destination/"`,
		`checkout --quiet -B sim-latency "$expected_commit"`,
		`source_lock_sha256`,
		`candidate_patch_sha256:null`,
	} {
		if !strings.Contains(prepare, required) {
			t.Errorf("temporary source preparer is missing %q", required)
		}
	}
	for _, forbidden := range []string{"WORKSPACE_ROOT", "SERVER_ROOT", "../server", "../connect"} {
		if strings.Contains(prepare, forbidden) {
			t.Errorf("temporary source preparer depends on a host checkout: %q", forbidden)
		}
	}

	evaluatorBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`active_source_root="$(mktemp -d "$work_dir/evaluation-sources.XXXXXXXX")"`,
		`--destination "$baseline_source_root"`,
		`--destination "$candidate_source_root"`,
		`--source-root "$candidate_source_root"`,
		`rev-parse HEAD:connect/sim-latency`,
		`candidate changed the protected sim-latency source tree`,
		`EVALUATION_SOURCE_DIR=$source`,
		`.Destination == "/workspace" and .RW == false`,
		`origin:"authenticated_evaluator_image",host_repositories_used:false`,
		`remove_evaluation_sources`,
		`sudo -n rm -rf -- "$active_source_root"`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator source isolation is missing %q", required)
		}
	}

	composeBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(composeBytes)
	for _, required := range []string{
		`source: ${EVALUATION_SOURCE_DIR:?set the per-evaluation temporary source checkout}`,
		`target: /workspace`,
		`read_only: true`,
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("runner source mount is missing %q", required)
		}
	}
	if strings.Count(compose, `source: ${EVALUATION_SOURCE_DIR:`) != 1 {
		t.Fatal("temporary source must be mounted into the runner only")
	}
}

// Only the two frozen host local directories cross the container boundary.
// Mounting either parent would expose main/all material even though the leaf
// bind mounts are read-only.
func TestEvaluatorMountsOnlyLocalConfigAndVault(t *testing.T) {
	composeBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	compose := string(composeBytes)
	for value, want := range map[string]int{
		"target: /runtime/config/local": 2,
		"target: /runtime/vault/local":  2,
	} {
		if got := strings.Count(compose, value); got != want {
			t.Errorf("Compose %q count = %d, want %d", value, got, want)
		}
	}
	for _, forbidden := range []string{
		"EVALUATION_RUNTIME_DIR",
		"target: /runtime\n",
		"target: /runtime/config\n",
		"target: /runtime/vault\n",
		"target: /runtime/config/all",
		"target: /runtime/config/main",
		"target: /runtime/vault/all",
		"target: /runtime/vault/main",
	} {
		if strings.Contains(compose, forbidden) {
			t.Errorf("Compose exposes forbidden runtime mount %q", forbidden)
		}
	}

	evaluatorBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`config_local_directory="$(jq -er '.config_local_directory' "$request_path")"`,
		`vault_local_directory="$(jq -er '.vault_local_directory' "$request_path")"`,
		`EVALUATION_CONFIG_LOCAL_DIR=$config_local_directory`,
		`EVALUATION_VAULT_LOCAL_DIR=$vault_local_directory`,
		`authenticate_local_mounts`,
		`kind:"sim-latency-local-mounts",direct_bind:true`,
		`.Destination == "/runtime/config/local" and .RW == false`,
		`.Destination == "/runtime/vault/local" and .RW == false`,
		`[.Mounts[] | select(.Destination | startswith("/runtime"))] | length == 2`,
		`mounts:[.Mounts[] | {type:.Type,destination:.Destination,rw:.RW}]`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator local-only mount attestation is missing %q", required)
		}
	}
	if strings.Contains(evaluator, "EVALUATION_RUNTIME_DIR=") {
		t.Fatal("evaluator still emits the parent runtime mount")
	}
	if strings.Contains(evaluator, "PREPARE_RUNTIME") || strings.Contains(evaluator, `$runtime/config/local`) {
		t.Fatal("production evaluator still creates or mounts a copied local tree")
	}

	prepareRuntimeBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/prepare-runtime.sh")
	if err != nil {
		t.Fatal(err)
	}
	prepareRuntime := string(prepareRuntimeBytes)
	for _, required := range []string{
		`"$runtime_root/vault/local"`,
		`"$runtime_root/config/local"`,
		"runtime tree does not match the local-only allowlist",
	} {
		if !strings.Contains(prepareRuntime, required) {
			t.Errorf("runtime local-only allowlist is missing %q", required)
		}
	}
	if strings.Contains(prepareRuntime, `"$runtime_root/site/local"`) {
		t.Fatal("runtime preparer still creates a site overlay")
	}

	hashBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/hash-local-mount.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`! -type d ! -type f`,
		`sort -z`,
		`command -v sha256sum`,
		`command -v shasum`,
		`hash_file "$root/$relative"`,
		`hash_stream`,
	} {
		if !strings.Contains(string(hashBytes), required) {
			t.Errorf("direct local digest helper is missing %q", required)
		}
	}

	buildBaseBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/build-base.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(buildBaseBytes),
		"readonly REPOSITORIES=(server connect proxy sdk glog goidenticons userwireguard sn)",
	) {
		t.Fatal("evaluator base repository allowlist changed; re-audit config/vault exclusion")
	}
	dockerfileBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/Dockerfile.base")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"COPY source/config", "COPY source/vault"} {
		if strings.Contains(string(dockerfileBytes), forbidden) {
			t.Errorf("evaluator base image includes forbidden repository content %q", forbidden)
		}
	}

	smokeBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/smoke-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	smoke := string(smokeBytes)
	for _, required := range []string{
		"local-source/config/local",
		"local-source/vault/local",
		"config/all/forbidden",
		"config/main/forbidden",
		"vault/all/forbidden",
		"vault/main/forbidden",
		"test ! -e /runtime/config/all",
		"test ! -e /runtime/config/main",
		"test ! -e /runtime/vault/all",
		"test ! -e /runtime/vault/main",
		"! touch /runtime/config/local/write-test",
		"! touch /runtime/vault/local/write-test",
		`"exact read-only config/local and vault/local mounts"`,
	} {
		if !strings.Contains(smoke, required) {
			t.Errorf("live local-only mount gate is missing %q", required)
		}
	}
	if !strings.Contains(compose, `APEX_CONTAINER_EVALUATION: "true"`) {
		t.Fatal("direct local mounts do not enable per-stage throwaway credential overrides")
	}

	hostCheckBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`.local_only_read_only_mounts_verified == true`,
		`.no_production_secrets_verified == true`,
		`containment_no_production_secrets=true`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification does not bind local-only evidence %q", required)
		}
	}
}

func TestDockerIDMapTranslation(t *testing.T) {
	script := "../connect/sim-latency/evaluator/container/docker-id-map.sh"
	tests := []struct {
		name    string
		mapping string
		id      string
		want    string
		ok      bool
	}{
		{name: "identity", mapping: "0 0 4294967295\n", id: "65532", want: "65532", ok: true},
		{name: "daemon remap", mapping: "0 100000 65536\n", id: "65532", want: "165532", ok: true},
		{name: "rootless split root", mapping: "0 1000 1\n1 100000 65536\n", id: "65532", want: "165531", ok: true},
		{name: "unmapped gap", mapping: "0 100000 100\n200 200000 100\n", id: "150", ok: false},
		{name: "overlap", mapping: "0 100000 1000\n500 200000 1000\n", id: "750", ok: false},
		{name: "malformed", mapping: "zero 100000 65536\n", id: "1", ok: false},
	}
	for _, test := range tests {
		mappingPath := t.TempDir() + "/id_map"
		if err := os.WriteFile(mappingPath, []byte(test.mapping), 0o600); err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(script, "--translate", mappingPath, test.id).CombinedOutput()
		if test.ok && err != nil {
			t.Fatalf("%s translation failed: %v: %s", test.name, err, output)
		}
		if !test.ok && err == nil {
			t.Fatalf("%s invalid mapping translated to %q", test.name, output)
		}
		if test.ok && strings.TrimSpace(string(output)) != test.want {
			t.Fatalf("%s translation = %q, want %q", test.name, strings.TrimSpace(string(output)), test.want)
		}
	}

	evaluatorBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`$DOCKER_ID_MAP --image "$base_image_id" --uid 65532 --gid 65532`,
		`container_host_uid="$(jq -er '.host_uid'`,
		`container_host_gid="$(jq -er '.host_gid'`,
		`"$work_dir/docker-id-map.json"`,
		`chown -R "$container_host_uid:$container_host_gid"`,
		`install -o "$container_host_uid" -g "$container_host_gid"`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator is not user-namespace ownership aware: missing %q", required)
		}
	}
	for _, forbidden := range []string{`chown -R 65532:65532`, `install -o 65532 -g 65532`} {
		if strings.Contains(evaluator, forbidden) {
			t.Errorf("evaluator assumes identity user mapping: %q", forbidden)
		}
	}

	hostCheckBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`$DOCKER_ID_MAP --image "$image_digest" --uid 65532 --gid 65532`,
		`[ "$docker_id_map_remapped" = true ]`,
		`.docker_user_namespace_verified == true`,
		`.docker_uid_map_sha256 == $docker_uid_map_sha256`,
		`.docker_gid_map_sha256 == $docker_gid_map_sha256`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification is missing live Docker id-map check %q", required)
		}
	}
}

// The daemon example is part of the frozen host identity, not deployment
// advice. Keep the exact namespace and hardening policy machine-checked, and
// require the live host check to authenticate both its bytes and semantics.
func TestDockerDaemonConfigurationIsFailClosed(t *testing.T) {
	configBytes, err := os.ReadFile("../connect/sim-latency/evaluator/docker-daemon.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatalf("decode Docker daemon config: %v", err)
	}
	if len(config) != 7 {
		t.Fatalf("Docker daemon config has %d top-level fields, want 7", len(config))
	}
	if config["userns-remap"] != "default" || config["no-new-privileges"] != true ||
		config["userland-proxy"] != false || config["log-driver"] != "local" ||
		config["shutdown-timeout"] != float64(45) || config["ipv6"] != false {
		t.Fatalf("Docker daemon hardening changed: %#v", config)
	}
	logOptions, ok := config["log-opts"].(map[string]any)
	if !ok || len(logOptions) != 3 || logOptions["max-size"] != "16m" ||
		logOptions["max-file"] != "2" || logOptions["compress"] != "true" {
		t.Fatalf("Docker log bounds changed: %#v", config["log-opts"])
	}

	hostCheckBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`readonly DOCKER_DAEMON_CONFIG=/etc/docker/daemon.json`,
		`[ ! -L "$DOCKER_DAEMON_CONFIG" ]`,
		`[ "$(stat -c %u "$DOCKER_DAEMON_CONFIG"`,
		`& 0022)) -eq 0`,
		`."userns-remap" | type == "string" and length > 0`,
		`."no-new-privileges" == true`,
		`."userland-proxy" == false`,
		`."log-driver" == "local"`,
		`."log-opts"."max-size" == "16m"`,
		`."shutdown-timeout" == 45`,
		`[ "$docker_daemon_config_sha256" = "$expected_docker_daemon_config_sha256" ]`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification is missing Docker daemon check %q", required)
		}
	}

	hostConfigBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var hostConfig map[string]any
	if err := json.Unmarshal(hostConfigBytes, &hostConfig); err != nil {
		t.Fatalf("decode host config: %v", err)
	}
	if hostConfig["docker_daemon_config_sha256"] != "REPLACE_WITH_64_HEX" {
		t.Fatalf("host config does not freeze Docker daemon bytes: %#v", hostConfig["docker_daemon_config_sha256"])
	}
}

// Daemon-wide user-namespace remapping cannot create BuildKit's host-network
// executor mounts on the authoritative Ubuntu host. The trusted base needs
// outbound package downloads, but it does not need the host namespace; keep it
// on Docker's ordinary bridge while submission builds remain networkless.
func TestEvaluatorBaseBuildAvoidsHostNetwork(t *testing.T) {
	buildBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/build-base.sh")
	if err != nil {
		t.Fatal(err)
	}
	build := string(buildBytes)
	if !strings.Contains(build, "--network default") {
		t.Fatal("evaluator base build does not select Docker's default bridge")
	}
	if strings.Contains(build, "--network host") {
		t.Fatal("evaluator base build joins the host network namespace")
	}
}

// The season-one editable surface is a literal file list, not a directory
// pattern. Keep this pre-freeze policy narrow while the final source identity
// is pending, and make any later expansion require an explicit test change.
func TestExamplePatchPolicyMatchesReviewedSurface(t *testing.T) {
	policyBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/policy.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var policy PatchPolicy
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatalf("decode patch policy: %v", err)
	}
	if policy.MaxPatchBytes != 262144 {
		t.Fatalf("max patch bytes = %d, want 262144", policy.MaxPatchBytes)
	}
	if len(policy.AllowedPaths) != 1 || policy.AllowedPaths[0] != "connect/resident_contract_manager.go" {
		t.Fatalf("editable surface is not the reviewed literal file: %#v", policy.AllowedPaths)
	}
	if strings.ContainsAny(policy.AllowedPaths[0], `*?[\\`) {
		t.Fatalf("editable surface contains a glob: %q", policy.AllowedPaths[0])
	}
	if !pathAllowed(policy.AllowedPaths[0], policy) {
		t.Fatal("reviewed editable file is not accepted by the server validator")
	}
	for _, protected := range []string{
		"competition/worker.go",
		"connect/sim-latency/main.go",
		"stats/writer.go",
		"db_migrations.go",
		"config/local/settings.yml",
		"config/all/settings.yml",
		"vault/local/jwt.yml",
		"vault/all/jwt.yml",
		"site/local/settings.yml",
		"go.mod",
	} {
		if pathAllowed(protected, policy) {
			t.Errorf("protected path %q is editable", protected)
		}
	}
}

// Infrastructure failures must leave enough immutable evidence to diagnose a
// rejected replicate without retaining hidden inputs or throwaway credentials.
func TestEvaluatorRetainsOnlySanitizedFailureEvidence(t *testing.T) {
	evaluatorBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	retainCall := strings.Index(evaluator, `"$RETAIN_FAILURE_EVIDENCE" \`)
	unmountCall := strings.Index(evaluator, `sudo -n umount "$active_work_mount"`)
	if retainCall < 0 || unmountCall < 0 || unmountCall < retainCall {
		t.Fatal("evaluator does not retain failure evidence before unmounting its tmpfs")
	}
	for _, required := range []string{
		`FAILURE_EVALUATOR_LINE="$failure_line"`,
		`retained sanitized failure evidence`,
		`baseline_scorer_log="$work_dir/baseline-scorer.log"`,
		`candidate_scorer_log="$work_dir/candidate-scorer.log"`,
		`inspect_json="$(sudo -n docker inspect`,
		`<<<"$inspect_json" > "$inspect_path"`,
		`[ "$scorer_exit" -eq 0 ] || die "baseline scorer exited $scorer_exit"`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator failure diagnostics are missing %q", required)
		}
	}

	retainerBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/retain-failure-evidence.sh")
	if err != nil {
		t.Fatal(err)
	}
	retainer := string(retainerBytes)
	for _, required := range []string{
		`"$source_dir/input"`,
		`"$source_dir/scorer-input"`,
		`"$source_dir/score-runtime"`,
		`-name 'evaluation-sources.*'`,
		`-type d -name runtime -exec rm -rf`,
		`-name '*.env' -o -name '*.env.new'`,
		`-name containers.json -print0`,
		`(keys | sort) == ["config","host_config","id","image_id","mounts","name","state"]`,
		`! -type f ! -type d -delete`,
		`kind:"sim-latency-evaluator-failure"`,
		`kind:"sim-latency-failed-evidence-manifest"`,
		`find "$destination_dir" -type d -exec chmod 0500`,
		`find "$destination_dir" -type f -exec chmod 0400`,
	} {
		if !strings.Contains(retainer, required) {
			t.Errorf("failure evidence sanitizer is missing %q", required)
		}
	}
	if strings.Contains(evaluator, `docker inspect "$runner_id" "$postgres_id" "$redis_id" > "$inspect_path"`) {
		t.Fatal("evaluator persists raw Docker inspection data before sanitization")
	}
}

// Fresh images receive the same complete authentication as cache hits; a
// successful Docker build alone is not evidence that its labels are trusted.
func TestSubmissionBuilderAuthenticatesEveryCandidateBuildInput(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/build-submission.sh")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(scriptBytes), `actual_labels=`, 2)
	if len(parts) != 2 {
		t.Fatal("submission builder does not independently inspect final image labels")
	}
	verification := parts[1]

	for _, requiredCheck := range []string{
		`."com.urnetwork.competition.base-sha" == $base_sha`,
		`."org.opencontainers.image.revision" == $candidate_sha`,
		`."com.urnetwork.competition.patch-sha256" == $patch_sha256`,
		`."com.urnetwork.competition.policy-sha256" == $policy_sha256`,
		`."com.urnetwork.competition.builder-sha256" == $builder_sha256`,
		`."com.urnetwork.competition.image-key" == $image_key`,
		`.policy_sha256 == $policy_sha256`,
		`.builder_sha256 == $builder_sha256`,
		`.image_key == $image_key`,
	} {
		if !strings.Contains(verification, requiredCheck) {
			t.Errorf("submission builder is missing final identity check %q", requiredCheck)
		}
	}
}

// Host management remains schedulable even when candidate code saturates its
// CPU set or reaches every runtime/build memory ceiling.
func TestEvaluatorReservesManagementResources(t *testing.T) {
	boundaryBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/resource-boundary.sh")
	if err != nil {
		t.Fatal(err)
	}
	boundary := string(boundaryBytes)
	for _, required := range []string{
		"EVALUATION_PHYSICAL_CORE_COUNT=10",
		"MANAGEMENT_PHYSICAL_CORE_COUNT=2",
		"RUNNER_MEMORY_LIMIT=72g",
		"MINIMUM_MANAGEMENT_MEMORY_RESERVE_BYTES=25769803776",
		"disjoint_cpu_sets:true,memory_capacity_passed:true",
	} {
		if !strings.Contains(boundary, required) {
			t.Errorf("resource boundary is missing %q", required)
		}
	}

	evaluatorBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	for _, required := range []string{
		`taskset -c "$management_cpuset"`,
		`EVALUATION_CPUSET=$cpuset`,
		`APEX_CPU_COUNT=$evaluation_cpu_count`,
		`resource-boundary.json`,
		`management_cpu_reserved:true`,
		`management_memory_reserved:true`,
		`offline_build_resource_limits:true`,
	} {
		if !strings.Contains(evaluator, required) {
			t.Errorf("evaluator resource boundary is missing %q", required)
		}
	}

	builderBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/build-submission.sh")
	if err != nil {
		t.Fatal(err)
	}
	builder := string(builderBytes)
	for _, required := range []string{
		`--cgroup-parent "$build_cgroup_parent"`,
		`--resource "cpuset-cpus=$evaluation_cpuset"`,
		`--resource "memory=$build_memory_limit"`,
		`--resource "memory-swap=$build_memory_limit"`,
		`timeout --signal=TERM --kill-after=30s`,
	} {
		if !strings.Contains(builder, required) {
			t.Errorf("submission build boundary is missing %q", required)
		}
	}

}

// The worker must be restricted to the management set without making the host
// topology appear to contain only two CPUs. Host-online CPUs and inherited
// worker affinity are distinct qualification facts.
func TestHostSelfCheckSeparatesHostTopologyFromWorkerAffinity(t *testing.T) {
	hostCheckBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`host_cpu_list="$(lscpu -p=CPU`,
		`worker_cpu_list="$(awk '/^Cpus_allowed_list:/`,
		`[ "$logical_cpu_count" = 12 ] && [ "$host_cpu_list" = "$expected_cpu_list" ]`,
		`[ "$worker_cpu_list" = "$expected_management_cpu_list" ]`,
		`worker_affinity_pinned:$worker_affinity_pinned`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host/worker CPU qualification is missing %q", required)
		}
	}
	if strings.Contains(hostCheck, `logical_cpu_count="$(nproc`) {
		t.Fatal("host CPU count still inherits the worker's management-only affinity")
	}
}

func TestAuthoritativeHostControlsAreFailClosed(t *testing.T) {
	controlBytes, err := os.ReadFile("../connect/sim-latency/evaluator/authoritative-host-controls.sh")
	if err != nil {
		t.Fatal(err)
	}
	control := string(controlBytes)
	for _, required := range []string{
		`readonly SMT_CONTROL=/sys/devices/system/cpu/smt/control`,
		`urnetwork_disable_smt "$SMT_CONTROL" 10 1`,
		`write_root_file "$governor_path" performance`,
		`write_root_file /sys/devices/system/cpu/intel_pstate/no_turbo 1`,
		`sysctl -q -w vm.overcommit_memory=1`,
		`[ "$logical_cpu_count" -eq 12 ]`,
		`[ "$threads_per_core" -eq 1 ]`,
		`[ "$governors" = performance ]`,
		`[ "$turbo_state" = disabled ]`,
		`.management_logical_cpu_count == 2`,
		`[ "$passed" = true ]`,
	} {
		if !strings.Contains(control, required) {
			t.Errorf("authoritative host controls are missing %q", required)
		}
	}

	controlUnitBytes, err := os.ReadFile("../connect/sim-latency/evaluator/authoritative-host-controls.service.example")
	if err != nil {
		t.Fatal(err)
	}
	controlUnit := string(controlUnitBytes)
	for _, required := range []string{
		`Before=containerd.service docker.service competitionworker.service`,
		`ConditionFileIsExecutable=/usr/local/libexec/urnetwork/authoritative-host-controls`,
		`ExecStart=/usr/local/libexec/urnetwork/authoritative-host-controls --apply`,
		`RemainAfterExit=yes`,
		`Restart=on-failure`,
		`RestartSec=2`,
		`TimeoutStartSec=60`,
		`RequiredBy=containerd.service docker.service`,
	} {
		if !strings.Contains(controlUnit, required) {
			t.Errorf("host-control unit is missing %q", required)
		}
	}
	if strings.Contains(controlUnit, "ConditionPathIsExecutable") {
		t.Fatal("host-control unit uses the unsupported ConditionPathIsExecutable directive")
	}

	irqBytes, err := os.ReadFile("../connect/sim-latency/evaluator/authoritative-host-irqs.sh")
	if err != nil {
		t.Fatal(err)
	}
	irq := string(irqBytes)
	for _, required := range []string{
		`readonly MIN_DEVICE_IRQ=16`,
		`management_cpuset="$(jq -er '.management_cpuset'`,
		`evaluation_cpuset="$(jq -er '.evaluation_cpuset'`,
		`/proc/interrupts | sort -n -u`,
		`printf '%s\n' "$management_cpuset" | sudo -n tee "$affinity_path"`,
		`[ "$configured" != "$management_cpuset" ]`,
		`[ "${#failed_irqs[@]}" -eq 0 ]`,
		`kind:"sim-latency-irq-placement-policy"`,
		`irq_policy_sha256`,
		`[ "$passed" = true ]`,
	} {
		if !strings.Contains(irq, required) {
			t.Errorf("authoritative IRQ control is missing %q", required)
		}
	}

	hostCheckBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`irq_report="$($IRQ_CONTROL --check`,
		`[ "$irq_live_passed" = true ]`,
		`[ "$irq_policy_sha256" = "$expected_irq_policy_sha" ]`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host IRQ qualification is missing %q", required)
		}
	}
	factsStart := strings.Index(hostCheck, `facts="$(jq -cnS`)
	factsEnd := strings.Index(hostCheck, `qualification_sha256="$(printf`)
	if factsStart == -1 || factsEnd <= factsStart {
		t.Fatal("host qualification facts block is unavailable")
	}
	facts := hostCheck[factsStart:factsEnd]
	if !strings.Contains(facts, `irq_policy_sha256`) {
		t.Fatal("host qualification does not bind the stable IRQ policy")
	}
	if strings.Contains(facts, `irq_affinity_sha256`) {
		t.Fatal("host qualification still binds reboot-unstable IRQ numbers")
	}

	irqUnitBytes, err := os.ReadFile("../connect/sim-latency/evaluator/authoritative-host-irqs.service.example")
	if err != nil {
		t.Fatal(err)
	}
	irqUnit := string(irqUnitBytes)
	for _, required := range []string{
		`After=urnetwork-authoritative-host-controls.service local-fs.target`,
		`Before=containerd.service docker.service competitionworker.service`,
		`Requires=urnetwork-authoritative-host-controls.service`,
		`ConditionFileIsExecutable=/usr/local/libexec/urnetwork/authoritative-host-irqs`,
		`ExecStart=/usr/local/libexec/urnetwork/authoritative-host-irqs --apply`,
		`Restart=on-failure`,
		`RequiredBy=containerd.service docker.service`,
	} {
		if !strings.Contains(irqUnit, required) {
			t.Errorf("IRQ unit is missing %q", required)
		}
	}

	installerBytes, err := os.ReadFile("../connect/sim-latency/evaluator/install-authoritative-host-controls.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)
	for _, required := range []string{
		`install -D -o root -g root -m 0555 "$CONTROL_SOURCE" "$CONTROL_TARGET"`,
		`install -D -o root -g root -m 0444 "$CONTROL_LIBRARY_SOURCE" "$CONTROL_LIBRARY_TARGET"`,
		`install -D -o root -g root -m 0555 "$BOUNDARY_SOURCE" "$BOUNDARY_TARGET"`,
		`install -D -o root -g root -m 0555 "$IRQ_SOURCE" "$IRQ_TARGET"`,
		`install -D -o root -g root -m 0444 "$UNIT_SOURCE" "$UNIT_TARGET"`,
		`install -D -o root -g root -m 0444 "$IRQ_UNIT_SOURCE" "$IRQ_UNIT_TARGET"`,
		`systemctl reenable "$UNIT_NAME" "$IRQ_UNIT_NAME"`,
		`sudo -n "$CONTROL_TARGET" --check`,
		`sudo -n "$IRQ_TARGET" --check`,
	} {
		if !strings.Contains(installer, required) {
			t.Errorf("host-control installer is missing %q", required)
		}
	}
}

func TestAuthoritativeHostSMTNormalization(t *testing.T) {
	command := exec.Command("bash", "../connect/sim-latency/evaluator/test-authoritative-host-controls-lib.sh")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("SMT normalization regression test failed: %v\n%s", err, output)
	}
}

func TestContainerSmokeHashesRemappedLocalSourcesAsRoot(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/smoke-test.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`sudo -n chown -R "$container_host_uid:$container_host_gid" "$smoke_root/local-source"`,
		`config_local_sha256="$(sudo -n "$HASH_LOCAL_MOUNT" "$config_local_directory")"`,
		`vault_local_sha256="$(sudo -n "$HASH_LOCAL_MOUNT" "$vault_local_directory")"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("remapped local-source smoke boundary is missing %q", required)
		}
	}
	if strings.Contains(script, `config_local_sha256="$($HASH_LOCAL_MOUNT`) ||
		strings.Contains(script, `vault_local_sha256="$($HASH_LOCAL_MOUNT`) {
		t.Fatal("smoke hashes a remapped local source as the unprivileged caller")
	}
}

// Build output is attacker-controlled and must be drained without allowing a
// noisy package initializer to consume the host-memory reserve.
func TestEvaluatorBoundsCandidateBuildLogWhileDrainingIt(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`mkfifo -m 0600 "$candidate_build_pipe"`,
		`head -c "$MAX_BUILD_LOG_BYTES"`,
		`cat >/dev/null`,
		`2> "$candidate_build_pipe"`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("bounded build log is missing %q", required)
		}
	}
}

// The live gate must prove both kernel OOM containment and cleanup from the
// disjoint management CPU set, then reject any residual labeled object.
func TestResourceBombGateCoversOOMAndCleanup(t *testing.T) {
	scriptBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/test-resource-bomb-cleanup.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(scriptBytes)
	for _, required := range []string{
		`memory_exit_code" -eq 137`,
		`'{{.State.OOMKilled}}'`,
		`[ "$observed_cpuset" = "$evaluation_cpuset" ]`,
		`"$IMAGE" cpu "$evaluation_cpuset"`,
		`--production-memory-limit`,
		`'.runner_memory_limit_bytes'`,
		`production_memory_limit:$production_memory_limit`,
		`taskset -c "$management_cpuset" sudo -n docker rm -f`,
		`cleanup_elapsed_ms`,
		`residual_containers:0,residual_networks:0`,
	} {
		if !strings.Contains(script, required) {
			t.Errorf("resource bomb gate is missing %q", required)
		}
	}
	var fixtureBuilder strings.Builder
	for _, path := range []string{
		"../connect/sim-latency/evaluator/container/testdata/resource-bomb/main.go",
		"../connect/sim-latency/evaluator/container/testdata/resource-bomb/cpu_linux.go",
	} {
		fixtureBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fixtureBuilder.Write(fixtureBytes)
	}
	fixture := fixtureBuilder.String()
	for _, required := range []string{
		"runtime.LockOSThread()",
		"//go:build linux",
		"unix.SchedSetaffinity(0, &affinity)",
		"unix.SYS_GETCPU",
		`fmt.Println("cpu-bomb-ready")`,
	} {
		if !strings.Contains(fixture, required) {
			t.Errorf("resource bomb fixture is missing %q", required)
		}
	}

	hostCheckBytes, err := os.ReadFile("../connect/sim-latency/evaluator/host-self-check.sh")
	if err != nil {
		t.Fatal(err)
	}
	hostCheck := string(hostCheckBytes)
	for _, required := range []string{
		`.production_memory_limit_verified == true`,
		`.memory_bomb_limit_bytes == $runner_memory_limit_bytes`,
		`.memory_bomb_exit_code == 137`,
		`.default_deny_network_verified == true`,
		`.no_published_ports_verified == true`,
		`.scorer_network_none_verified == true`,
		`.evidence_manifest_sha256 | type == "string"`,
		`.evaluation_complete_sha256 | type == "string"`,
		`.kind == "sim-latency-template-database-reset"`,
		`.kind == "sim-latency-redis-reset"`,
		`.kind == "sim-latency-immutable-reports"`,
		`.job_id == $containment[0].qualified_job_id`,
		`.worker_result_sha256 == $containment[0].worker_result_sha256`,
		`.cleanup_elapsed_ms <= .cleanup_limit_ms`,
		`.residual_containers == 0 and .residual_networks == 0`,
	} {
		if !strings.Contains(hostCheck, required) {
			t.Errorf("host qualification is missing production bomb check %q", required)
		}
	}
	if strings.Contains(hostCheck, `/proc/net/route`) {
		t.Fatal("host qualification still substitutes the host route table for evaluator network evidence")
	}
}

// Host readiness consumes a short root-owned marker, so marker promotion must
// authenticate the full evaluator chain and reject semantically unsafe mount
// evidence even when all attacker-controlled hashes are internally coherent.
func TestHostContainmentPromotionAuthenticatesEvidence(t *testing.T) {
	promoterBytes, err := os.ReadFile("../connect/sim-latency/evaluator/promote-host-containment.sh")
	if err != nil {
		t.Fatal(err)
	}
	promoter := string(promoterBytes)
	for _, required := range []string{
		`promotion must run as root`,
		`host config parent is not root-owned`,
		`worker result did not pass every score and containment gate`,
		`authenticate_declared_artifact evaluation.complete.json`,
		`authenticate_declared_artifact evidence-manifest.json`,
		`evidence hash mismatch`,
		`evidence/local-mounts.json`,
		`evidence/docker-id-map.json`,
		`direct local mount evidence is invalid`,
		`Docker user-namespace evidence is invalid`,
		`.destination == "/runtime/config/local" and .rw == false`,
		`.destination == "/runtime/vault/local" and .rw == false`,
		`container evidence violates the local-only boundary`,
		`sim-latency-template-database-reset`,
		`sim-latency-redis-reset`,
		`sim-latency-immutable-reports`,
		`.template_database_marker_sha256 = $template_sha`,
		`.redis_reset_marker_sha256 = $redis_sha`,
		`.cleanup_marker_sha256 = $marker_sha`,
		`.immutable_reports_marker_sha256 = $immutable_sha`,
	} {
		if !strings.Contains(promoter, required) {
			t.Errorf("host containment promoter is missing %q", required)
		}
	}

	testBytes, err := os.ReadFile("../connect/sim-latency/evaluator/test-promote-host-containment.sh")
	if err != nil {
		t.Fatal(err)
	}
	testScript := string(testBytes)
	for _, required := range []string{
		`host containment promotion test passed`,
		`.mounts[0].destination) = "/runtime/config"`,
		`unsafe parent config mount was promoted`,
		`identity Docker user namespace was promoted`,
		`failed promotion left a marker`,
		`failed user-namespace promotion left a marker`,
		`simulated reboot did not recreate runtime markers`,
	} {
		if !strings.Contains(testScript, required) {
			t.Errorf("host containment promotion regression is missing %q", required)
		}
	}
}

func readDockerInstructions(t *testing.T, path string) []dockerInstruction {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var instructions []dockerInstruction
	var pending string
	stage := ""
	user := ""
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pending += strings.TrimSuffix(line, "\\")
		if strings.HasSuffix(line, "\\") {
			pending += " "
			continue
		}
		instruction := strings.Join(strings.Fields(pending), " ")
		pending = ""
		fields := strings.Fields(instruction)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "FROM":
			stage = ""
			user = ""
			if len(fields) >= 4 && strings.EqualFold(fields[len(fields)-2], "AS") {
				stage = fields[len(fields)-1]
			}
		case "USER":
			if len(fields) != 2 {
				t.Fatalf("malformed USER instruction: %s", instruction)
			}
			user = fields[1]
		}
		instructions = append(instructions, dockerInstruction{stage: stage, user: user, text: instruction})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if pending != "" {
		t.Fatal("Dockerfile ends with an incomplete instruction")
	}
	return instructions
}

func TestEvaluatorRequestBindsCanonicalPatchDigest(t *testing.T) {
	settings := validSettings()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId: server.NewId(), RoundId: server.NewId(), PatchSha256: strings.Repeat("c", 64),
			EvaluatorImageDigest: settings.EvaluatorImageDigest,
			ApiImageDigest:       testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
		},
		AttemptCount: 2,
		Round: roundRecord{
			RoundResult:   RoundResult{Epoch: 1, ProvidersSha256: strings.Repeat("d", 64)},
			ProvidersPath: "/trusted/round/providers.yml",
		},
	}
	request := evaluatorRequestForJob(settings, job, strings.Repeat("e", 64), "/artifacts/attempt-02", "/artifacts/attempt-02/canonical.patch")
	if request.PatchSha256 != job.PatchSha256 {
		t.Fatalf("patch SHA-256 = %q, want %q", request.PatchSha256, job.PatchSha256)
	}
	if request.SourceEpoch != 0 {
		t.Fatalf("source epoch = %d, want baseline epoch 0", request.SourceEpoch)
	}
	if request.EvaluatorImageDigest != job.EvaluatorImageDigest ||
		request.ApiImageDigest != job.ApiImageDigest || request.WorkerImageDigest != job.WorkerImageDigest {
		t.Fatal("evaluator request did not bind the exact control-plane image identities")
	}
	if request.ConfigLocalDirectory != settings.ConfigLocalDirectory ||
		request.VaultLocalDirectory != settings.VaultLocalDirectory {
		t.Fatal("evaluator request did not bind the exact direct local directories")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["patch_sha256"] != job.PatchSha256 {
		t.Fatalf("wire patch_sha256 = %#v, want %q", wire["patch_sha256"], job.PatchSha256)
	}
}

func TestHashLocalMountDirectoryIsDeterministicAndRejectsLinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "z.yml"), []byte("z: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.yml"), []byte("a: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "st.yml"), []byte("st: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stripe.yml"), []byte("stripe: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := hashLocalMountDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	shellDigest, err := exec.Command(
		"../connect/sim-latency/evaluator/container/hash-local-mount.sh",
		root,
	).CombinedOutput()
	if err != nil || strings.TrimSpace(string(shellDigest)) != first {
		t.Fatalf("host and Go local-mount digests differ: %q %q %v", first, shellDigest, err)
	}
	hostileLocaleCommand := exec.Command("../connect/sim-latency/evaluator/container/hash-local-mount.sh", root)
	hostileLocaleCommand.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_ALL=en_US.UTF-8")
	hostileLocaleDigest, err := hostileLocaleCommand.CombinedOutput()
	if err != nil || strings.TrimSpace(string(hostileLocaleDigest)) != first {
		t.Fatalf(
			"caller locale changed local-mount digest: want %q, got %q (%v)",
			first,
			hostileLocaleDigest,
			err,
		)
	}
	second, err := hashLocalMountDirectory(root)
	if err != nil || second != first {
		t.Fatalf("local mount digest changed without a content change: %q %q %v", first, second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.yml"), []byte("a: 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := hashLocalMountDirectory(root)
	if err != nil || changed == first {
		t.Fatal("local mount content change did not change its digest")
	}
	if err := os.Symlink("z.yml", filepath.Join(root, "link.yml")); err != nil {
		t.Fatal(err)
	}
	if _, err := hashLocalMountDirectory(root); err == nil {
		t.Fatal("local mount containing a symbolic link was accepted")
	}
}

func TestEvaluatorPinsLocaleAndUsesCanonicalLocalMountDigest(t *testing.T) {
	evaluatorBytes, err := os.ReadFile("../connect/sim-latency/evaluator/container/evaluator.sh")
	if err != nil {
		t.Fatal(err)
	}
	evaluator := string(evaluatorBytes)
	localePin := strings.Index(evaluator, "export LANG=C LC_ALL=C")
	digestBinding := strings.Index(evaluator, `readonly HASH_LOCAL_MOUNT="$SCRIPT_DIR/hash-local-mount.sh"`)
	digestCall := strings.Index(evaluator, `"$HASH_LOCAL_MOUNT" "$1"`)
	firstAuthentication := strings.Index(evaluator, "authenticate_local_mounts()")
	if localePin < 0 || digestBinding < 0 || digestCall < 0 || firstAuthentication < 0 {
		t.Fatal("evaluator does not bind the locale-pinned local-mount digest helper")
	}
	if firstAuthentication < localePin || firstAuthentication < digestBinding || firstAuthentication < digestCall {
		t.Fatal("evaluator authenticates local mounts before pinning digest semantics")
	}
}

func TestAuthenticateArtifactsRequiresMandatoryExactFiles(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		"accounting.json",
		"baseline.json",
		"resources.json",
		"score.json",
		"evaluation.complete.json",
		"evidence/container.json",
	}
	declared := make([]evaluationArtifact, 0, len(paths))
	for _, path := range paths {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(path+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		digest, size, err := hashRegularFile(full)
		if err != nil {
			t.Fatal(err)
		}
		declared = append(declared, evaluationArtifact{Path: path, Sha256: digest, Bytes: size})
	}
	for i, j := 0, len(declared)-1; i < j; i, j = i+1, j-1 {
		declared[i], declared[j] = declared[j], declared[i]
	}
	authenticated, err := authenticateArtifacts(root, declared)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(authenticated); i++ {
		if authenticated[i].Path <= authenticated[i-1].Path {
			t.Fatalf("artifacts are not sorted: %#v", authenticated)
		}
	}

	withoutCompletion := make([]evaluationArtifact, 0, len(declared)-1)
	for _, artifact := range declared {
		if artifact.Path != "evaluation.complete.json" {
			withoutCompletion = append(withoutCompletion, artifact)
		}
	}
	if _, err := authenticateArtifacts(root, withoutCompletion); err == nil {
		t.Fatal("artifact set without completion marker was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "score.json"), []byte("changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateArtifacts(root, declared); err == nil {
		t.Fatal("artifact with changed bytes was accepted")
	}
}

func TestSubmissionBuildFailureRequiresOnlyBuildBoundaryEvidence(t *testing.T) {
	security := evaluationSecurity{
		DefaultDenyNetwork:         true,
		OfflineBuild:               true,
		OfflineBuildResourceLimits: true,
		ManagementCpuReserved:      true,
		ManagementMemoryReserved:   true,
		NoProductionSecrets:        true,
		StructuralPatchCheck:       true,
		CleanupComplete:            true,
		ImmutableReports:           true,
	}
	evalError := &CompetitionError{
		Kind: "submission", Code: "candidate_build_failed",
		Message: "candidate did not pass the frozen offline build", Retriable: false,
	}
	if !security.passedFor(evalError) {
		t.Fatal("contained terminal build failure was rejected")
	}
	if security.passedFor(nil) {
		t.Fatal("build-only evidence was accepted as a completed measurement")
	}
	security.OfflineBuild = false
	if security.passedFor(evalError) {
		t.Fatal("submission failure without offline-build evidence was accepted")
	}
}

func TestAuthenticateSubmissionFailureArtifacts(t *testing.T) {
	root := t.TempDir()
	var declared []evaluationArtifact
	for _, path := range []string{"submission-error.json", "evaluation.complete.json"} {
		full := filepath.Join(root, path)
		if err := os.WriteFile(full, []byte(path+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		digest, size, err := hashRegularFile(full)
		if err != nil {
			t.Fatal(err)
		}
		declared = append(declared, evaluationArtifact{Path: path, Sha256: digest, Bytes: size})
	}
	evalError := &CompetitionError{Kind: "submission", Code: "candidate_build_failed"}
	if _, err := authenticateResultArtifacts(root, declared, evalError); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateResultArtifacts(root, declared[:1], evalError); err == nil {
		t.Fatal("submission failure without completion marker was accepted")
	}
}

func TestSealArtifactDirectoryMakesTreeReadOnly(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "evidence")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(nested, "container.json")
	if err := os.WriteFile(artifact, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sealArtifactDirectory(root); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{root: 0500, nested: 0500, artifact: 0400} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o, want %04o", path, got, want)
		}
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(nested, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestSealArtifactDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("missing", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := sealArtifactDirectory(root); err == nil {
		t.Fatal("artifact tree containing a symbolic link was accepted")
	}
}

func testEvaluationProgress() *evaluationProgress {
	progress := &evaluationProgress{
		Schema: 1, Kind: "sim-latency-evaluation-progress",
		JobId: "job-1", RoundId: "round-1", Phase: "candidate",
		ReplicateCount: 1, BaselineCompleted: 1, CandidateCompleted: 1,
		UpdatedUnixMs: 1,
	}
	pImprovement := 0.01
	pRegression := 0.99
	for metric, quantile := range evaluationProgressMetrics {
		progress.Metrics = append(progress.Metrics, evaluationProgressMetric{
			Role: "baseline", Replicate: 1, Metric: metric,
			Quantile: quantile, Value: 100, Significance: "baseline",
		})
		progress.Metrics = append(progress.Metrics, evaluationProgressMetric{
			Role: "candidate", Replicate: 1, Metric: metric,
			Quantile: quantile, Value: 90, PImprovement: &pImprovement,
			PRegression: &pRegression, Significance: "improved",
		})
	}
	return progress
}

func TestEvaluationProgressValidationAndMetricExport(t *testing.T) {
	progress := testEvaluationProgress()
	if err := validateEvaluationProgress(progress, "job-1", "round-1", 1); err != nil {
		t.Fatal(err)
	}
	applyEvaluationProgress(progress)
	defer competitionLiveEvaluationMetric.Reset()
	metrics := make(chan prometheus.Metric, 16)
	competitionLiveEvaluationMetric.Collect(metrics)
	if got := len(metrics); got != 8 {
		t.Fatalf("exported live metrics = %d, want 8", got)
	}

	progress.Metrics = append(progress.Metrics, progress.Metrics[0])
	if err := validateEvaluationProgress(progress, "job-1", "round-1", 1); err == nil {
		t.Fatal("duplicate progress metric passed validation")
	}
}

func TestEvaluationProgressDecoderRejectsUnknownFields(t *testing.T) {
	progress := testEvaluationProgress()
	encoded, err := json.Marshal(progress)
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded[:len(encoded)-1], []byte(`,"unexpected":true}`)...)
	path := filepath.Join(t.TempDir(), evaluationProgressFileName)
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationProgress(path, "job-1", "round-1", 1); err == nil {
		t.Fatal("unknown progress field passed strict decoding")
	}
}

func TestRunnerHeartbeatContract(t *testing.T) {
	if runnerHeartbeatInterval != 15*time.Second {
		t.Fatalf("runner heartbeat interval = %s, want 15s", runnerHeartbeatInterval)
	}

	want := time.Date(2026, time.August, 28, 12, 34, 56, 789000000, time.UTC)
	recordRunnerHeartbeat(want)
	defer recordRunnerHeartbeat(server.NowUtc())

	metric := &dto.Metric{}
	if err := competitionRunnerHeartbeatTimestamp.Write(metric); err != nil {
		t.Fatal(err)
	}
	got := metric.GetGauge().GetValue()
	wantSeconds := float64(want.UnixNano()) / float64(time.Second)
	if math.Abs(got-wantSeconds) > 0.000001 {
		t.Fatalf("runner heartbeat timestamp = %.9f, want %.9f", got, wantSeconds)
	}
}

type referenceManifest struct {
	Schema                   int                    `json:"schema"`
	Status                   string                 `json:"status"`
	BaseGitSha               string                 `json:"base_git_sha"`
	BaseImageId              string                 `json:"base_image_id"`
	TargetPath               string                 `json:"target_path"`
	TargetBlobSha1           string                 `json:"target_blob_sha1"`
	PolicySha256             string                 `json:"policy_sha256"`
	BuilderSha256            string                 `json:"builder_sha256"`
	LocalBuildVerification   localBuildVerification `json:"local_build_verification"`
	References               []referenceRecord      `json:"references"`
	LowerRawScoreIsBetter    bool                   `json:"lower_raw_score_is_better"`
	OfficialSeparability     string                 `json:"official_separability"`
	RequiredCorrectSeedOrder string                 `json:"required_correct_seed_order"`
	RequiredSeedPassCount    int                    `json:"required_seed_pass_count"`
	RequiredSeedCount        int                    `json:"required_seed_count"`
}

type localBuildVerification struct {
	Status                      string `json:"status"`
	CacheReuse                  string `json:"cache_reuse"`
	CandidateExecutionIsolation string `json:"candidate_execution_isolation"`
	ProtectedPathCount          int    `json:"protected_path_count"`
	Scope                       string `json:"scope"`
	OfficialHost                bool   `json:"official_host"`
}

type referenceRecord struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Sha256          string `json:"sha256"`
	CandidateGitSha string `json:"candidate_git_sha"`
	Image           string `json:"image"`
	ImageId         string `json:"image_id"`
	ImageKey        string `json:"image_key"`
	SimulatorSha256 string `json:"simulator_sha256"`
	ExpectedOrder   int    `json:"expected_order"`
}

// The provisional references stay bound to one exact development base while
// making the unrun official separability gate impossible to mistake for a pass.
func TestReferenceSubmissionsAuthenticate(t *testing.T) {
	referenceRoot := "../connect/sim-latency/evaluator/references"
	manifestBytes := readReferenceFile(t, filepath.Join(referenceRoot, "manifest.json"))
	var manifest referenceManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("decode reference manifest: %v", err)
	}
	if manifest.Schema != 1 || manifest.Status != "provisional" ||
		manifest.OfficialSeparability != "not_run" || !manifest.LowerRawScoreIsBetter ||
		manifest.RequiredCorrectSeedOrder != "better < noop < worse" ||
		manifest.RequiredSeedPassCount != 19 || manifest.RequiredSeedCount != 20 {
		t.Fatalf("invalid provisional reference contract: %#v", manifest)
	}
	if manifest.LocalBuildVerification.Status != "passed" ||
		manifest.LocalBuildVerification.CacheReuse != "passed" ||
		manifest.LocalBuildVerification.CandidateExecutionIsolation != "passed" ||
		manifest.LocalBuildVerification.ProtectedPathCount != 6 ||
		manifest.LocalBuildVerification.Scope != "development_host" ||
		manifest.LocalBuildVerification.OfficialHost {
		t.Fatalf("invalid local build verification boundary: %#v", manifest.LocalBuildVerification)
	}
	if !gitShaPattern.MatchString(manifest.BaseGitSha) ||
		!imageDigestPattern.MatchString(manifest.BaseImageId) ||
		!sha256Pattern.MatchString(manifest.PolicySha256) ||
		!sha256Pattern.MatchString(manifest.BuilderSha256) {
		t.Fatal("reference manifest contains a malformed pinned identity")
	}

	policyBytes := readReferenceFile(t, filepath.Join("../connect/sim-latency/evaluator/container", "policy.example.json"))
	if digestHex(policyBytes) != manifest.PolicySha256 {
		t.Fatal("reference policy hash does not match the manifest")
	}
	var policy PatchPolicy
	if err := json.Unmarshal(policyBytes, &policy); err != nil {
		t.Fatalf("decode reference patch policy: %v", err)
	}
	builderBytes := readReferenceFile(t, filepath.Join("../connect/sim-latency/evaluator/container", "Dockerfile.submission"))
	if digestHex(builderBytes) != manifest.BuilderSha256 {
		t.Fatal("reference builder hash does not match the manifest")
	}

	targetBytes := readReferenceFile(t, filepath.Join("..", manifest.TargetPath))
	gitBlob := append([]byte(fmt.Sprintf("blob %d\x00", len(targetBytes))), targetBytes...)
	targetDigest := sha1.Sum(gitBlob)
	if hex.EncodeToString(targetDigest[:]) != manifest.TargetBlobSha1 {
		t.Fatal("reference target blob does not match the manifest")
	}

	wantOrders := map[string]int{"better": 0, "noop": 1, "worse": 2}
	wantSnippets := map[string]string{
		"better": "model.GetOpenContractIdsForSourceOrDestination(",
		"noop":   "// All other controller activity moved",
		"worse":  "time.Sleep(25 * time.Millisecond)",
	}
	seen := map[string]bool{}
	seenCandidateShas := map[string]bool{}
	seenImageIds := map[string]bool{}
	seenImageKeys := map[string]bool{}
	for _, reference := range manifest.References {
		wantOrder, ok := wantOrders[reference.Name]
		if !ok || seen[reference.Name] || reference.ExpectedOrder != wantOrder ||
			reference.Path != reference.Name+".patch" || !sha256Pattern.MatchString(reference.Sha256) ||
			!gitShaPattern.MatchString(reference.CandidateGitSha) ||
			!imageDigestPattern.MatchString(reference.ImageId) ||
			!sha256Pattern.MatchString(reference.ImageKey) ||
			!sha256Pattern.MatchString(reference.SimulatorSha256) ||
			reference.Image != "urnetwork/sim-latency-submission:"+reference.ImageKey[:32] ||
			seenCandidateShas[reference.CandidateGitSha] || seenImageIds[reference.ImageId] ||
			seenImageKeys[reference.ImageKey] {
			t.Fatalf("invalid reference record: %#v", reference)
		}
		seen[reference.Name] = true
		seenCandidateShas[reference.CandidateGitSha] = true
		seenImageIds[reference.ImageId] = true
		seenImageKeys[reference.ImageKey] = true
		patchBytes := readReferenceFile(t, filepath.Join(referenceRoot, reference.Path))
		if digestHex(patchBytes) != reference.Sha256 {
			t.Fatalf("%s patch hash does not match the manifest", reference.Name)
		}
		canonical, validationErr := ValidateAndCanonicalizePatch(string(patchBytes), policy)
		if validationErr != nil {
			t.Fatalf("%s patch rejected: %s", reference.Name, validationErr.Code)
		}
		if len(canonical.Paths) != 1 || canonical.Paths[0] != manifest.TargetPath ||
			!strings.Contains(string(patchBytes), wantSnippets[reference.Name]) {
			t.Fatalf("%s patch does not match its declared target/behavior", reference.Name)
		}
	}
	if len(seen) != len(wantOrders) {
		t.Fatalf("reference set incomplete: %#v", seen)
	}
}

// The bulk lookup used by the better reference keys results by unordered
// transfer pair. A directional key silently misses the reverse ID ordering and
// can turn a real contract into a false inactive result.
func TestBetterReferenceUsesLookupCompatibleUnorderedPair(t *testing.T) {
	sourceId := server.RequireParseId("00000000-0000-0000-0000-000000000001")
	destinationId := server.RequireParseId("00000000-0000-0000-0000-000000000002")
	lookupKey := model.NewUnorderedTransferPair(sourceId, destinationId)
	lookup := map[model.TransferPair]bool{lookupKey: true}

	if lookup[model.NewTransferPair(destinationId, sourceId)] {
		t.Fatal("directional reverse key unexpectedly matched an unordered lookup key")
	}
	if !lookup[model.NewUnorderedTransferPair(destinationId, sourceId)] {
		t.Fatal("unordered reverse key did not match the bulk lookup key")
	}

	patch := string(readReferenceFile(t, filepath.Join("../connect/sim-latency/evaluator/references", "better.patch")))
	if !strings.Contains(patch, "-\ttransferPair := model.NewTransferPair(sourceId, destinationId)") ||
		!strings.Contains(patch, "+\ttransferPair := model.NewUnorderedTransferPair(sourceId, destinationId)") {
		t.Fatal("better reference does not replace the directional cache/lookup key")
	}
}

func readReferenceFile(t *testing.T, path string) []byte {
	t.Helper()
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return bytes
}

func digestHex(bytes []byte) string {
	digest := sha256.Sum256(bytes)
	return hex.EncodeToString(digest[:])
}

type apexConformanceFeeCollector struct {
	stateLock sync.Mutex
	receipts  map[string]string
	calls     int
}

func (self *apexConformanceFeeCollector) CollectOnce(
	ctx context.Context,
	submissionId string,
	feeUsd int,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if feeUsd != apexSubmissionFeeUsd {
		return "", fmt.Errorf("fee = %d, want %d", feeUsd, apexSubmissionFeeUsd)
	}
	if receipt, ok := self.receipts[submissionId]; ok {
		return receipt, nil
	}
	self.calls++
	receipt := "receipt-" + submissionId
	self.receipts[submissionId] = receipt
	return receipt, nil
}

// The emulator forces every adapter transition: durable fee intent, typed
// backpressure, immutable admission, FIFO polling, embargo, finalized
// leaderboard publication, and authenticated workload reveal.
func TestApexAdapterConformance(t *testing.T) {
	roundId := server.RequireParseId("00000000-0000-0000-0000-000000000101")
	jobIds := []server.Id{
		server.RequireParseId("00000000-0000-0000-0000-000000000201"),
		server.RequireParseId("00000000-0000-0000-0000-000000000202"),
	}
	patches := [][]byte{
		[]byte("diff --git a/connect/a.go b/connect/a.go\n"),
		[]byte("diff --git a/connect/b.go b/connect/b.go\n"),
	}
	patchDigests := make([]string, len(patches))
	for i, patch := range patches {
		digest := sha256.Sum256(patch)
		patchDigests[i] = hex.EncodeToString(digest[:])
	}
	providers := []byte("schema: 1\nproviders: []\n")
	providersDigest := sha256.Sum256(providers)
	providersSha256 := hex.EncodeToString(providersDigest[:])

	var serverStateLock sync.Mutex
	submitCalls := 0
	pollJobIds := []server.Id{}
	leakEmbargoedScore := true
	apiServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		serverStateLock.Lock()
		defer serverStateLock.Unlock()
		response.Header().Set("Content-Type", "application/json")
		writeJson := func(status int, value any) {
			response.WriteHeader(status)
			if err := json.NewEncoder(response).Encode(value); err != nil {
				t.Errorf("encode conformance response: %v", err)
			}
		}

		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/competition/info":
			if request.Header.Get("Authorization") != "" {
				t.Error("public competition info received a bearer token")
			}
			writeJson(http.StatusOK, InfoResult{
				CompetitionId: "sim-latency",
				ActiveRound: &RoundResult{
					RoundId: roundId,
					Status:  "open",
				},
			})
		case request.Method == http.MethodPost && request.URL.Path == "/competition/score":
			if request.Header.Get("Authorization") != "Bearer submitter-secret" {
				t.Error("competition admission did not authenticate with the submitter token")
			}
			var args ScoreArgs
			if err := json.NewDecoder(request.Body).Decode(&args); err != nil {
				t.Errorf("decode conformance admission: %v", err)
				writeJson(http.StatusBadRequest, CompetitionError{Code: "bad_request"})
				return
			}
			if args.RoundId != roundId {
				t.Errorf("admission round = %s, want %s", args.RoundId, roundId)
			}
			patchIndex := -1
			for i, patch := range patches {
				if args.Patch == string(patch) {
					patchIndex = i
					break
				}
			}
			if patchIndex < 0 {
				t.Errorf("unexpected submitted patch %q", args.Patch)
				writeJson(http.StatusUnprocessableEntity, CompetitionError{Code: "invalid_patch"})
				return
			}
			submitCalls++
			if patchIndex == 0 && submitCalls == 1 {
				response.Header().Set("Retry-After", "0")
				writeJson(http.StatusTooManyRequests, CompetitionError{Code: "backpressure", Retriable: true})
				return
			}
			if patchIndex == 0 && submitCalls == 2 {
				writeJson(http.StatusServiceUnavailable, CompetitionError{Code: "temporarily_unavailable"})
				return
			}
			writeJson(http.StatusAccepted, ScoreAcceptedResult{
				JobId:       jobIds[patchIndex],
				RoundId:     roundId,
				PatchSha256: patchDigests[patchIndex],
				State:       "queued",
				StatusUrl:   "/competition/score/" + jobIds[patchIndex].String(),
			})
		case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/competition/score/"):
			if request.Header.Get("Authorization") != "Bearer submitter-secret" {
				t.Error("competition poll did not authenticate with the submitter token")
			}
			jobId, err := server.ParseId(strings.TrimPrefix(request.URL.Path, "/competition/score/"))
			if err != nil {
				writeJson(http.StatusBadRequest, CompetitionError{Code: "bad_job_id"})
				return
			}
			patchIndex := 0
			if jobId == jobIds[1] {
				patchIndex = 1
			} else if jobId != jobIds[0] {
				writeJson(http.StatusNotFound, CompetitionError{Code: "not_found"})
				return
			}
			pollJobIds = append(pollJobIds, jobId)
			job := ScoreJobResult{
				JobId:       jobId,
				RoundId:     roundId,
				PatchSha256: patchDigests[patchIndex],
				State:       "completed",
			}
			if patchIndex == 0 && leakEmbargoedScore {
				leakEmbargoedScore = false
				job.State = "running"
				job.Score = &ScoreResult{ScoreSchema: ScoreSchema}
			}
			writeJson(http.StatusOK, job)
		case request.Method == http.MethodGet && request.URL.Path == "/competition/leaderboard":
			if request.Header.Get("Authorization") != "" {
				t.Error("public leaderboard received a bearer token")
			}
			rawScores := []float64{90, 95}
			normalizedScores := []float64{0.90, 0.85}
			entries := make([]LeaderboardEntry, len(jobIds))
			for i, jobId := range jobIds {
				entries[i] = LeaderboardEntry{
					Rank:        i + 1,
					JobId:       jobId,
					PatchSha256: patchDigests[i],
					Winner:      i == 0,
					HonestyReview: func() string {
						if i == 0 {
							return "approved"
						}
						return "not_selected"
					}(),
					Score: ScoreResult{
						ScoreSchema:      ScoreSchema,
						RawScore:         &rawScores[i],
						NormalizedScore:  &normalizedScores[i],
						Placeable:        true,
						TakeoverEligible: true,
						Significance: &ScoreSignificance{
							Method:                   "welch_t",
							StatisticallySignificant: true,
						},
					},
				}
			}
			finalizedAt := time.Date(2026, time.August, 29, 2, 0, 0, 0, time.UTC)
			writeJson(http.StatusOK, SeasonLeaderboardResult{
				CompetitionId: "sim-latency",
				Epochs: []LeaderboardResult{{
					CompetitionId: "sim-latency",
					RoundId:       roundId,
					Epoch:         1,
					Status:        "finalized",
					FinalizedAt:   finalizedAt,
					WinnerJobId:   &jobIds[0],
					Entries:       entries,
				}},
			})
		case request.Method == http.MethodGet && request.URL.Path == "/competition/round/"+roundId.String()+"/providers.yml":
			if request.Header.Get("Authorization") != "" {
				t.Error("public workload reveal received a bearer token")
			}
			response.Header().Set("Content-Type", "application/yaml")
			response.Header().Set("X-Content-SHA256", providersSha256)
			response.WriteHeader(http.StatusOK)
			if _, err := response.Write(providers); err != nil {
				t.Errorf("write conformance workload: %v", err)
			}
		default:
			writeJson(http.StatusNotFound, CompetitionError{Code: "not_found"})
		}
	}))
	defer apiServer.Close()

	storeDirectory := t.TempDir()
	if err := os.Chmod(storeDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewApexAdapterFileStore(storeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	feeCollector := &apexConformanceFeeCollector{receipts: map[string]string{}}
	delays := []time.Duration{}
	now := time.Date(2026, time.August, 29, 1, 0, 0, 0, time.UTC)
	adapter, err := NewApexAdapter(apiServer.URL, "submitter-secret", store, feeCollector, ApexAdapterOptions{
		HttpClient:  apiServer.Client(),
		MaxAttempts: 5,
		Wait: func(ctx context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return ctx.Err()
		},
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	records := make([]*ApexAdapterRecord, len(patches))
	for i, patch := range patches {
		records[i], err = adapter.Submit(context.Background(), fmt.Sprintf("apex-%d", i+1), patch)
		if err != nil {
			t.Fatalf("submit %d: %v", i+1, err)
		}
		if records[i].JobId != jobIds[i] || records[i].RoundId != roundId || records[i].FeeReceipt == "" {
			t.Fatalf("admission %d = %+v", i+1, records[i])
		}
	}
	if feeCollector.calls != 2 || submitCalls != 4 || !reflect.DeepEqual(delays, []time.Duration{0, 2 * time.Second}) {
		t.Fatalf("retry contract: fees=%d submits=%d delays=%v", feeCollector.calls, submitCalls, delays)
	}

	reopenedStore, err := NewApexAdapterFileStore(storeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	reopenedAdapter, err := NewApexAdapter(apiServer.URL, "submitter-secret", reopenedStore, feeCollector, ApexAdapterOptions{
		HttpClient: apiServer.Client(),
		Wait:       func(context.Context, time.Duration) error { return nil },
		Now:        func() time.Time { return now.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopenedAdapter.Submit(context.Background(), "apex-1", patches[0]); err != nil {
		t.Fatalf("durable duplicate admission: %v", err)
	}
	if feeCollector.calls != 2 || submitCalls != 4 {
		t.Fatal("durable duplicate charged a fee or resubmitted the job")
	}
	if _, err := reopenedAdapter.Submit(context.Background(), "apex-1", patches[1]); err == nil {
		t.Fatal("Apex submission id accepted different patch bytes")
	}
	changedAdmission := ScoreAcceptedResult{
		JobId:       server.RequireParseId("00000000-0000-0000-0000-000000000299"),
		RoundId:     roundId,
		PatchSha256: patchDigests[0],
		State:       "queued",
		StatusUrl:   "/competition/score/00000000-0000-0000-0000-000000000299",
	}
	if _, err := reopenedStore.RecordAdmission("apex-1", changedAdmission, now); err == nil {
		t.Fatal("durable store accepted a changed competition job identity")
	}

	if _, err := reopenedAdapter.PollNext(context.Background()); err == nil || !strings.Contains(err.Error(), "embargoed") {
		t.Fatalf("embargoed score disclosure = %v", err)
	}
	for i := range jobIds {
		record, err := reopenedAdapter.PollNext(context.Background())
		if err != nil {
			t.Fatalf("FIFO poll %d: %v", i+1, err)
		}
		if record.JobId != jobIds[i] || record.State != "completed" || record.Score != nil {
			t.Fatalf("FIFO poll %d = %+v", i+1, record)
		}
	}
	if record, err := reopenedAdapter.PollNext(context.Background()); err != nil || record != nil {
		t.Fatalf("drained FIFO = %+v, %v", record, err)
	}
	if !reflect.DeepEqual(pollJobIds, []server.Id{jobIds[0], jobIds[0], jobIds[1]}) {
		t.Fatalf("poll order = %v", pollJobIds)
	}

	leaderboards, err := reopenedAdapter.Reconcile(context.Background())
	if err != nil || len(leaderboards.Epochs) != 1 {
		t.Fatalf("leaderboard reconciliation = %+v, %v", leaderboards, err)
	}
	for i := range jobIds {
		record, err := reopenedStore.Get(fmt.Sprintf("apex-%d", i+1))
		if err != nil || !record.Published || record.Score == nil || record.Winner != (i == 0) {
			t.Fatalf("published record %d = %+v, %v", i+1, record, err)
		}
	}

	seed := strings.Repeat("a", 64)
	revealed, err := reopenedAdapter.Reveal(context.Background(), RoundResult{
		RoundId:         roundId,
		Status:          "finalized",
		ProvidersSha256: providersSha256,
		RevealedSeed:    &seed,
		ProvidersUrl:    "/competition/round/" + roundId.String() + "/providers.yml",
	})
	if err != nil || !bytes.Equal(revealed, providers) {
		t.Fatalf("workload reveal = %q, %v", revealed, err)
	}

	stateBytes, err := os.ReadFile(filepath.Join(storeDirectory, "adapter-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range patches {
		if bytes.Contains(stateBytes, patch) {
			t.Fatal("durable adapter state retained raw submission code")
		}
	}
}

func TestSubmissionWindowIsStartInclusiveAndEndExclusive(t *testing.T) {
	start := time.Date(2026, time.August, 31, 12, 0, 0, 0, time.UTC)
	round := &roundRecord{RoundResult: RoundResult{OpensAt: start, ClosesAt: start.Add(7 * 24 * time.Hour)}}
	for _, test := range []struct {
		name        string
		submittedAt time.Time
		accepted    bool
	}{
		{name: "before start", submittedAt: round.OpensAt.Add(-time.Nanosecond), accepted: false},
		{name: "exact start", submittedAt: round.OpensAt, accepted: true},
		{name: "before end", submittedAt: round.ClosesAt.Add(-time.Nanosecond), accepted: true},
		{name: "exact end", submittedAt: round.ClosesAt, accepted: false},
		{name: "after end", submittedAt: round.ClosesAt.Add(time.Nanosecond), accepted: false},
	} {
		if accepted := submissionWithinEpoch(round, test.submittedAt); accepted != test.accepted {
			t.Errorf("%s: submissionWithinEpoch(%s) = %v, want %v", test.name, test.submittedAt, accepted, test.accepted)
		}
	}
}

func TestApexAdapterRejectsTrailingApiJson(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte("{}\n{}\n"))
	}))
	defer apiServer.Close()
	storeDirectory := t.TempDir()
	if err := os.Chmod(storeDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewApexAdapterFileStore(storeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewApexAdapter(
		apiServer.URL,
		"submitter-secret",
		store,
		&apexConformanceFeeCollector{receipts: map[string]string{}},
		ApexAdapterOptions{HttpClient: apiServer.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	var result InfoResult
	err = adapter.requestWithRetry(context.Background(), http.MethodGet, "/competition/info", nil, false, &result)
	if err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing API JSON error = %v", err)
	}
}

// TestApexAdapterFileStoreRejectsTrailingJson proves that recovery never
// accepts an appended state document after the authenticated state object.
func TestApexAdapterFileStoreRejectsTrailingJson(t *testing.T) {
	storeDirectory := t.TempDir()
	if err := os.Chmod(storeDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	state := []byte("{\"schema\":1,\"next_sequence\":0,\"records\":[]}\n{}\n")
	if err := os.WriteFile(filepath.Join(storeDirectory, "adapter-state.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewApexAdapterFileStore(storeDirectory); err == nil || !strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing state JSON error = %v", err)
	}
}

func TestApexAdapterRejectsHttpRedirects(t *testing.T) {
	redirectFollowed := false
	target := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		redirectFollowed = true
		if request.Header.Get("Authorization") != "" {
			t.Error("redirect target received the Apex bearer token")
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, target.URL+"/credential-capture", http.StatusFound)
	}))
	defer origin.Close()
	storeDirectory := t.TempDir()
	if err := os.Chmod(storeDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := NewApexAdapterFileStore(storeDirectory)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewApexAdapter(
		origin.URL,
		"submitter-secret",
		store,
		&apexConformanceFeeCollector{receipts: map[string]string{}},
		ApexAdapterOptions{HttpClient: origin.Client(), MaxAttempts: 1},
	)
	if err != nil {
		t.Fatal(err)
	}
	var result ScoreAcceptedResult
	err = adapter.requestWithRetry(
		context.Background(),
		http.MethodPost,
		"/competition/score",
		ScoreArgs{RoundId: server.NewId(), Patch: "patch"},
		true,
		&result,
	)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("redirecting Apex endpoint error = %v", err)
	}
	if redirectFollowed {
		t.Fatal("Apex adapter followed a credential-bearing redirect")
	}
}

func TestCompetitionFullLifecycleQueueCacheHonestyPromotionAndNextEpoch(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := validSettings()
		currentTime := server.NowUtc()
		store := PostgresStore{now: func() time.Time { return currentTime }}
		fifoListKey, fifoMemberKey := competitionFifoKeys(settings)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Del(ctx, fifoListKey, fifoMemberKey).Err())
		})
		t.Cleanup(func() {
			server.Redis(context.Background(), func(client server.RedisClient) {
				_ = client.Del(context.Background(), fifoListKey, fifoMemberKey).Err()
			})
		})
		apiImageDigest := testApiImageDigest()
		workerAImageDigest := testWorkerImageDigest()
		workerBImageDigest := "sha256:" + strings.Repeat("9", 64)
		now := currentTime
		round, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt: now.Add(-time.Minute), ClosesAt: now.Add(time.Hour), RevealAt: now.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("CreateRound: %s", err)
		}
		if round.Status != "open" || round.WorkloadCommitment == "" || len(round.SeedCiphertext) <= 32 {
			t.Fatalf("unexpected round: %#v", round)
		}
		if _, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt: now, ClosesAt: now.Add(30 * time.Minute), RevealAt: now.Add(time.Hour),
		}); !errors.Is(err, ErrPreviousEpochOpen) {
			t.Fatalf("overlapping round error = %v", err)
		}

		patch1, patchErr := ValidateAndCanonicalizePatch(testPatch("first"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job1, hit, err := store.Enqueue(ctx, settings, round.RoundId, patch1, "miner-a", apiImageDigest)
		if err != nil || hit {
			t.Fatalf("first enqueue = hit %v, err %v", hit, err)
		}
		if job1.EvaluatorImageDigest != settings.EvaluatorImageDigest || job1.ApiImageDigest != apiImageDigest {
			t.Fatalf(
				"enqueued image identities = evaluator %q, API %q",
				job1.EvaluatorImageDigest,
				job1.ApiImageDigest,
			)
		}
		cached, hit, err := store.Enqueue(ctx, settings, round.RoundId, patch1, "miner-b", apiImageDigest)
		if err != nil || !hit || cached.JobId != job1.JobId {
			t.Fatalf("cached enqueue = %#v, hit %v, err %v", cached, hit, err)
		}
		if _, err := store.GetJob(ctx, settings, job1.JobId, &Principal{Id: "miner-b", Role: "submitter"}); err != nil {
			t.Fatalf("cache-hit principal cannot poll: %s", err)
		}
		if _, err := store.GetJob(ctx, settings, job1.JobId, &Principal{Id: "miner-c", Role: "submitter"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unlisted principal poll error = %v", err)
		}

		patch2, patchErr := ValidateAndCanonicalizePatch(testPatch("second"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job2, _, err := store.Enqueue(ctx, settings, round.RoundId, patch2, "miner-a", apiImageDigest)
		if err != nil {
			t.Fatalf("second enqueue: %s", err)
		}
		patch3, patchErr := ValidateAndCanonicalizePatch(testPatch("third"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job3, _, err := store.Enqueue(ctx, settings, round.RoundId, patch3, "miner-a", apiImageDigest)
		if err != nil {
			t.Fatalf("third enqueue: %s", err)
		}
		var queuedSignals int64
		server.Redis(ctx, func(client server.RedisClient) {
			value, redisErr := client.LLen(ctx, fifoListKey).Result()
			server.Raise(redisErr)
			queuedSignals = value
		})
		if queuedSignals != 3 {
			t.Fatalf("Redis FIFO contains %d signals, want 3 unique queued jobs", queuedSignals)
		}
		beforeWindowJobId := server.NewId()
		afterWindowJobId := server.NewId()
		server.Db(ctx, func(conn server.PgConn) {
			for index, injected := range []struct {
				jobId       server.Id
				submittedAt time.Time
			}{
				{jobId: beforeWindowJobId, submittedAt: round.OpensAt.Add(-time.Microsecond)},
				{jobId: afterWindowJobId, submittedAt: round.ClosesAt},
			} {
				cacheKeyDigest := sha256.Sum256([]byte(fmt.Sprintf("outside-window-%d-%s", index, injected.jobId)))
				server.RaisePgResult(conn.Exec(ctx, `
					INSERT INTO competition_job (
						job_id, round_id, patch_bytes, patch_sha256, cache_key, state,
						submitted_at, available_at, artifact_retain_until, api_image_digest
					)
					SELECT $1, round_id, patch_bytes, patch_sha256, $2, 'queued',
					       $3, $3, artifact_retain_until, api_image_digest
					FROM competition_job WHERE job_id = $4
				`, injected.jobId, hex.EncodeToString(cacheKeyDigest[:]), injected.submittedAt, job1.JobId))
			}
		}, server.OptReadWrite())
		claimed1, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest)
		if err != nil || claimed1 == nil || claimed1.JobId != job1.JobId || claimed1.AttemptCount != 1 {
			t.Fatalf("first immediate claim = %#v, %v", claimed1, err)
		}
		for _, discardedJobId := range []server.Id{beforeWindowJobId, afterWindowJobId} {
			var state string
			var errorCode string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `
					SELECT state, eval_error_json->>'code'
					FROM competition_job WHERE job_id = $1
				`, discardedJobId).Scan(&state, &errorCode))
			})
			if state != "failed" || errorCode != "outside_epoch_window" {
				t.Fatalf("out-of-window job %s = state %q error %q", discardedJobId, state, errorCode)
			}
		}
		server.Redis(ctx, func(client server.RedisClient) {
			value, redisErr := client.LLen(ctx, fifoListKey).Result()
			server.Raise(redisErr)
			queuedSignals = value
		})
		if queuedSignals != 2 {
			t.Fatalf("Redis FIFO contains %d signals after one claim, want 2", queuedSignals)
		}
		if blocked, err := store.Claim(ctx, settings, "worker-b", workerBImageDigest); err != nil || blocked != nil {
			t.Fatalf("singleton slot allowed concurrent claim: %#v, %v", blocked, err)
		}
		if blocked, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest); err != nil || blocked != nil {
			t.Fatalf("duplicate worker id bypassed singleton slot: %#v, %v", blocked, err)
		}
		if err := store.Heartbeat(ctx, settings, "worker-a", claimed1.JobId); err != nil {
			t.Fatalf("heartbeat: %s", err)
		}
		raw, normalized := 100.0, 100.0
		_, err = store.Complete(ctx, settings, "worker-a", claimed1.JobId, EvaluationOutcome{
			Score:            &ScoreResult{ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, TakeoverEligible: true, Gates: map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}}, Significance: testScoreSignificance(true)},
			ArtifactManifest: []byte(`{"schema":1,"test":true}`),
		})
		if err != nil {
			t.Fatalf("complete first: %s", err)
		}
		currentTime = round.ClosesAt.Add(time.Second)
		claimed2, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest)
		if err != nil || claimed2 == nil || claimed2.JobId != job2.JobId {
			t.Fatalf("post-close backlog claim = %#v, %v", claimed2, err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_job SET lease_expires_at = $1 WHERE job_id = $2
			`, now.Add(-time.Minute), job2.JobId))
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE competition_worker_slot SET lease_expires_at = $1 WHERE slot_id = 1
			`, now.Add(-time.Minute)))
		})
		failedOver, err := store.Claim(ctx, settings, "worker-b", workerBImageDigest)
		if err != nil || failedOver == nil || failedOver.JobId != job2.JobId || failedOver.AttemptCount != 2 {
			t.Fatalf("failover claim = %#v, %v", failedOver, err)
		}
		_, err = store.Complete(ctx, settings, "worker-b", failedOver.JobId, EvaluationOutcome{
			Error:            &CompetitionError{Kind: "submission", Code: "build_failed", Message: "candidate did not build", Retriable: false},
			ArtifactManifest: []byte(`{"schema":1,"test":true}`),
		})
		if err != nil {
			t.Fatalf("complete failed submission: %s", err)
		}

		claimed3, err := store.Claim(ctx, settings, "worker-a", workerAImageDigest)
		if err != nil || claimed3 == nil || claimed3.JobId != job3.JobId {
			t.Fatalf("third claim = %#v, %v", claimed3, err)
		}
		retry, err := store.Complete(ctx, settings, "worker-a", claimed3.JobId, EvaluationOutcome{
			Error:            infrastructureError("host_transient", "transient host fault"),
			ArtifactManifest: []byte(`{"schema":1,"attempt":1}`),
			Infrastructure:   true,
		})
		if err != nil || !retry {
			t.Fatalf("infrastructure retry = %v, %v", retry, err)
		}
		var retryError []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT eval_error_json FROM competition_job WHERE job_id = $1`, job3.JobId).Scan(&retryError))
		})
		if len(retryError) != 0 {
			t.Fatalf("transient retry error was stored in mutable terminal field: %s", retryError)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE competition_job SET available_at = $1 WHERE job_id = $2`, now.Add(-time.Minute), job3.JobId))
		}, server.OptReadWrite())
		retried3, err := store.Claim(ctx, settings, "worker-b", workerBImageDigest)
		if err != nil || retried3 == nil || retried3.JobId != job3.JobId || retried3.AttemptCount != 2 {
			t.Fatalf("third retry claim = %#v, %v", retried3, err)
		}
		raw3, normalized3 := 101.0, 99.0
		_, err = store.Complete(ctx, settings, "worker-b", retried3.JobId, EvaluationOutcome{
			Score: &ScoreResult{
				ScoreSchema: 1, RawScore: &raw3, NormalizedScore: &normalized3,
				Placeable: true, TakeoverEligible: true,
				Gates:        map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}},
				Significance: testScoreSignificance(true),
			},
			ArtifactManifest: []byte(`{"schema":1,"attempt":2}`),
		})
		if err != nil {
			t.Fatalf("complete retried submission: %s", err)
		}
		review, err := store.PrepareCandidateReview(ctx, settings, round.Epoch)
		if err != nil || review == nil || review.Status != "pending_review" ||
			review.Candidate == nil || review.Candidate.JobId != job1.JobId ||
			review.Candidate.Rank != 1 || review.FinalizedAt != nil {
			t.Fatalf("initial honesty review = %#v, %v", review, err)
		}
		var skippedRankErr error
		server.Db(ctx, func(conn server.PgConn) {
			_, skippedRankErr = conn.Exec(ctx, `
				INSERT INTO competition_candidate_review (
					round_id, job_id, candidate_rank, decision, reviewer_id,
					reason, evidence_json, evidence_sha256, reviewed_at
				) VALUES ($1, $2, 2, 'approved', 'bypass-test', 'skip rank one',
				          '{"schema":1}'::json, $3, $4)
			`, round.RoundId, job3.JobId, strings.Repeat("a", 64), currentTime)
		}, server.OptReadWrite())
		if skippedRankErr == nil || !strings.Contains(skippedRankErr.Error(), "higher-ranked") {
			t.Fatalf("direct rank skip error = %v", skippedRankErr)
		}
		var gateErr error
		server.Db(ctx, func(conn server.PgConn) {
			_, gateErr = conn.Exec(ctx, `
				UPDATE competition_round
				SET finalized_at = $2, winner_job_id = $3
				WHERE round_id = $1
			`, round.RoundId, server.NowUtc(), job1.JobId)
		}, server.OptReadWrite())
		if gateErr == nil || !strings.Contains(gateErr.Error(), "honesty review") {
			t.Fatalf("unreviewed winner publication error = %v", gateErr)
		}
		review, err = store.RecordCandidateReview(
			ctx,
			settings,
			round.Epoch,
			testCandidateReviewDecision(job1.JobId, "rejected"),
		)
		if err != nil || review.Status != "pending_review" || review.RejectedCount != 1 ||
			review.Candidate == nil || review.Candidate.JobId != job3.JobId ||
			review.Candidate.Rank != 2 {
			t.Fatalf("rejected candidate advance = %#v, %v", review, err)
		}
		if _, err := store.RecordCandidateReview(
			ctx,
			settings,
			round.Epoch,
			testCandidateReviewDecision(job1.JobId, "approved"),
		); !errors.Is(err, ErrReviewOutOfOrder) {
			t.Fatalf("already rejected candidate approval error = %v", err)
		}
		review, err = store.RecordCandidateReview(
			ctx,
			settings,
			round.Epoch,
			testCandidateReviewDecision(job3.JobId, "approved"),
		)
		if err != nil || review.Status != "finalized" || review.FinalizedAt == nil ||
			review.WinnerJobId == nil || *review.WinnerJobId != job3.JobId {
			t.Fatalf("approved candidate finalization = %#v, %v", review, err)
		}
		promotionCandidate, err := store.RequirePromotionDecision(ctx, settings, round.Epoch, &job3.JobId)
		if err != nil || promotionCandidate == nil || promotionCandidate.JobId != job3.JobId ||
			promotionCandidate.PatchSha256 != job3.PatchSha256 {
			t.Fatalf("approved promotion decision: %v", err)
		}
		if _, err := store.RequirePromotionDecision(ctx, settings, round.Epoch, &job1.JobId); !errors.Is(err, ErrConflict) {
			t.Fatalf("rejected promotion decision error = %v", err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			value, redisErr := client.LLen(ctx, fifoListKey).Result()
			server.Raise(redisErr)
			queuedSignals = value
		})
		if queuedSignals != 0 {
			t.Fatalf("Redis FIFO retained %d stale signals after finalization", queuedSignals)
		}
		leaderboards, err := store.Leaderboards(ctx, settings)
		if err != nil || len(leaderboards.Epochs) != 1 ||
			len(leaderboards.Epochs[0].Entries) != 2 ||
			leaderboards.Epochs[0].Entries[0].Winner ||
			leaderboards.Epochs[0].Entries[0].HonestyReview != "rejected" ||
			!leaderboards.Epochs[0].Entries[1].Winner ||
			leaderboards.Epochs[0].Entries[1].HonestyReview != "approved" {
			t.Fatalf("leaderboard = %#v, %v", leaderboards, err)
		}
		var immutableErr error
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `
				UPDATE competition_candidate_review
				SET reason = 'tampered'
				WHERE round_id = $1 AND job_id = $2
			`, round.RoundId, job1.JobId)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "append-only") {
			t.Fatalf("candidate review append-only update error = %v", immutableErr)
		}
		nextRound, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt: now.Add(time.Hour), ClosesAt: now.Add(2 * time.Hour), RevealAt: now.Add(2 * time.Hour),
		})
		if err != nil || nextRound.Epoch != 2 {
			t.Fatalf("next epoch = %#v, %v", nextRound, err)
		}

		checkA := passingHostCheck(settings)
		checkA.RebaselineRoundId = &nextRound.RoundId
		if err := store.RegisterHost(ctx, settings, checkA); err != nil {
			t.Fatalf("register host: %s", err)
		}
		if err := refreshOperationalMetrics(ctx, settings); err != nil {
			t.Fatalf("refresh competition metrics: %s", err)
		}
		checks, err := store.Readiness(ctx, settings)
		if err != nil || !allChecks(checks) {
			t.Fatalf("readiness = %#v, %v", checks, err)
		}

		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job SET patch_bytes = 'tampered'::bytea WHERE job_id = $1`, job1.JobId)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "immutable") {
			t.Fatalf("patch immutability update error = %v", immutableErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job SET api_image_digest = $2 WHERE job_id = $1`, job1.JobId, workerBImageDigest)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "immutable") {
			t.Fatalf("API image identity immutability update error = %v", immutableErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job SET eval_error_json = '{"tampered":true}'::jsonb WHERE job_id = $1`, job2.JobId)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "immutable") {
			t.Fatalf("terminal result immutability update error = %v", immutableErr)
		}
		var events int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job_event`).Scan(&events))
		})
		if events < 8 {
			t.Fatalf("only %d durable queue events recorded", events)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, immutableErr = conn.Exec(ctx, `UPDATE competition_job_event SET actor_id = 'tampered' WHERE event_id = (SELECT min(event_id) FROM competition_job_event)`)
		}, server.OptReadWrite())
		if immutableErr == nil || !strings.Contains(immutableErr.Error(), "append-only") {
			t.Fatalf("event append-only update error = %v", immutableErr)
		}
	})
}

func TestCompetitionFullLifecycleNoWinnerCarryForward(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := validSettings()
		settings.CompetitionId += "-no-winner"
		currentTime := server.NowUtc()
		store := PostgresStore{now: func() time.Time { return currentTime }}
		fifoListKey, fifoMemberKey := competitionFifoKeys(settings)
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Del(ctx, fifoListKey, fifoMemberKey).Err())
		})
		t.Cleanup(func() {
			server.Redis(context.Background(), func(client server.RedisClient) {
				_ = client.Del(context.Background(), fifoListKey, fifoMemberKey).Err()
			})
		})

		now := currentTime
		round, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt:  now.Add(-time.Minute),
			ClosesAt: now.Add(time.Hour),
			RevealAt: now.Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		patch, patchErr := ValidateAndCanonicalizePatch(testPatch("below-threshold"), settings.PatchPolicy)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		job, _, err := store.Enqueue(ctx, settings, round.RoundId, patch, "miner-a", testApiImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.Claim(ctx, settings, "worker-a", testWorkerImageDigest())
		if err != nil || claimed == nil || claimed.JobId != job.JobId {
			t.Fatalf("immediate claim = %#v, %v", claimed, err)
		}
		raw, normalized := 95.0, 105.0
		_, err = store.Complete(ctx, settings, "worker-a", job.JobId, EvaluationOutcome{
			Score: &ScoreResult{
				ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized,
				Placeable: true, TakeoverEligible: false,
				Gates:        map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}},
				Significance: testScoreSignificance(false),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		currentTime = round.ClosesAt.Add(time.Second)
		review, err := store.PrepareCandidateReview(ctx, settings, round.Epoch)
		if err != nil || review == nil || review.Status != "finalized" ||
			review.FinalizedAt == nil || review.WinnerJobId != nil {
			t.Fatalf("no-winner finalization = %#v, %v", review, err)
		}
		if candidate, err := store.RequirePromotionDecision(ctx, settings, round.Epoch, nil); err != nil || candidate != nil {
			t.Fatalf("no-winner promotion decision: %v", err)
		}
		leaderboards, err := store.Leaderboards(ctx, settings)
		if err != nil || len(leaderboards.Epochs) != 1 ||
			len(leaderboards.Epochs[0].Entries) != 1 || leaderboards.Epochs[0].Entries[0].Winner {
			t.Fatalf("no-winner leaderboard = %#v, %v", leaderboards, err)
		}

		now = currentTime
		significantRound, err := store.CreateRound(ctx, settings, GenerateRoundArgs{
			OpensAt:  now,
			ClosesAt: now.Add(time.Hour),
			RevealAt: now.Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("create second round after no-winner finalization: %v", err)
		}
		significantPatch, patchErr := ValidateAndCanonicalizePatch(
			testPatch("significant-but-dishonest"),
			settings.PatchPolicy,
		)
		if patchErr != nil {
			t.Fatal(patchErr)
		}
		significantJob, _, err := store.Enqueue(
			ctx,
			settings,
			significantRound.RoundId,
			significantPatch,
			"miner-b",
			testApiImageDigest(),
		)
		if err != nil {
			t.Fatal(err)
		}
		claimed, err = store.Claim(ctx, settings, "worker-a", testWorkerImageDigest())
		if err != nil || claimed == nil || claimed.JobId != significantJob.JobId {
			t.Fatalf("significant claim = %#v, %v", claimed, err)
		}
		raw, normalized = 70, 142.857
		_, err = store.Complete(ctx, settings, "worker-a", significantJob.JobId, EvaluationOutcome{
			Score: &ScoreResult{
				ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized,
				Placeable: true, TakeoverEligible: true,
				Gates:        map[string]Gate{"G1": {Passed: true, Details: map[string]any{}}},
				Significance: testScoreSignificance(true),
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		currentTime = significantRound.ClosesAt.Add(time.Second)
		review, err = store.PrepareCandidateReview(ctx, settings, significantRound.Epoch)
		if err != nil || review == nil || review.Status != "pending_review" ||
			review.Candidate == nil || review.Candidate.JobId != significantJob.JobId {
			t.Fatalf("significant review = %#v, %v", review, err)
		}
		var unresolvedErr error
		server.Db(ctx, func(conn server.PgConn) {
			_, unresolvedErr = conn.Exec(ctx, `
				UPDATE competition_round SET finalized_at = $2, winner_job_id = NULL
				WHERE round_id = $1
			`, significantRound.RoundId, currentTime)
		}, server.OptReadWrite())
		if unresolvedErr == nil || !strings.Contains(unresolvedErr.Error(), "unresolved significant candidate") {
			t.Fatalf("unreviewed no-winner publication error = %v", unresolvedErr)
		}
		review, err = store.RecordCandidateReview(
			ctx,
			settings,
			significantRound.Epoch,
			testCandidateReviewDecision(significantJob.JobId, "rejected"),
		)
		if err != nil || review.Status != "finalized" || review.FinalizedAt == nil ||
			review.WinnerJobId != nil || review.RejectedCount != 1 {
			t.Fatalf("exhausted honesty review = %#v, %v", review, err)
		}
		if candidate, err := store.RequirePromotionDecision(
			ctx,
			settings,
			significantRound.Epoch,
			nil,
		); err != nil || candidate != nil {
			t.Fatalf("all-rejected no-winner promotion decision: %#v, %v", candidate, err)
		}
	})
}

func testCandidateReviewDecision(jobId server.Id, decision string) CandidateReviewDecision {
	evidence := []byte(`{"schema":1,"tampering_checks":["score-path","runner-boundary"],"conclusion":"` + decision + `"}`)
	digest := sha256.Sum256(evidence)
	return CandidateReviewDecision{
		JobId:          jobId,
		Decision:       decision,
		ReviewerId:     "honesty-agent-test",
		Reason:         "deterministic test review " + decision,
		Evidence:       evidence,
		EvidenceSha256: hex.EncodeToString(digest[:]),
	}
}

func testPatch(value string) string {
	return "diff --git a/connect/example.go b/connect/example.go\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/connect/example.go\n" +
		"+++ b/connect/example.go\n" +
		"@@ -1 +1 @@\n-old\n+" + value + "\n"
}
