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

	"github.com/urnetwork/server"
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
			ForbiddenPaths: []string{"connect/payment/**", "model/payment*.go"},
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
			TakeoverMargin:    .12, QueueLimit: 16, ScoreTimeoutSeconds: 28800,
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

	scoreArgs := append([]string{"score", strconv.Itoa(p.Replicates)}, args...)
	scoreOut, err := exec.Command("container/timeout-budget.sh", scoreArgs...).Output()
	if err != nil {
		t.Fatalf("score timeout calculator: %s", err)
	}
	scoreSeconds, err := strconv.ParseInt(strings.TrimSpace(string(scoreOut)), 10, 64)
	if err != nil {
		t.Fatalf("parse score timeout: %s", err)
	}
	if want := minimumScoreTimeoutSeconds(p); scoreSeconds != want {
		t.Fatalf("shell score timeout = %d, Go = %d", scoreSeconds, want)
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
	p := settings.EvaluationPolicy
	oldPerRunMs := p.RampMs + p.SettleMs + p.DurationMs + p.RequestTimeoutMs
	settings.EvaluationPolicy.ScoreTimeoutSeconds = int((2*int64(p.Replicates)*oldPerRunMs + 999) / 1000)
	if err := settings.Validate(); err == nil {
		t.Fatal("score timeout that omits client warm-up accepted")
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

func TestSecureEvaluatorChecksKeepQueueAdmissionSeparate(t *testing.T) {
	checks := map[string]bool{
		"configuration": true, "database": true, "queue_admission": false,
		"authoritative_evaluator_host": true, "host_rebaseline": true,
	}
	if !secureEvaluatorChecksPass(checks) {
		t.Fatal("a full queue should retain the endpoint's explicit 429 path")
	}
	checks["host_rebaseline"] = false
	if secureEvaluatorChecksPass(checks) {
		t.Fatal("failed evaluator qualification was treated as ready")
	}
}

func TestRoundGenerationReadinessPrecedesRoundRebaseline(t *testing.T) {
	checks := map[string]bool{
		"configuration": true, "database": true, "queue_admission": false,
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

func jsonUnmarshal(value []byte, out any) error {
	// Kept as a tiny seam so this test never depends on textual field order.
	return json.Unmarshal(value, out)
}

type fakeStore struct {
	mu         sync.Mutex
	claimJobs  []*queuedJob
	claims     int
	heartbeats int
	completed  []EvaluationOutcome
	handbacks  int
	readiness  map[string]bool
	readyErr   error
	round      *roundRecord
}

func (f *fakeStore) CreateRound(context.Context, *Settings, GenerateRoundArgs) (*roundRecord, error) {
	return nil, errors.New("unused")
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
func (f *fakeStore) Enqueue(context.Context, *Settings, server.Id, *CanonicalPatch, string) (*queuedJob, bool, error) {
	return nil, false, errors.New("unused")
}
func (f *fakeStore) GetJob(context.Context, *Settings, server.Id, *Principal) (*queuedJob, error) {
	return nil, errors.New("unused")
}
func (f *fakeStore) Readiness(context.Context, *Settings) (map[string]bool, error) {
	return f.readiness, f.readyErr
}
func (f *fakeStore) RegisterHost(context.Context, *Settings, HostSelfCheck) error { return nil }
func (f *fakeStore) Claim(context.Context, *Settings, string) (*queuedJob, error) {
	if f.claims < len(f.claimJobs) {
		job := f.claimJobs[f.claims]
		f.claims++
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
		ImageDigest:         settings.EvaluatorImageDigest, KernelRelease: "6.8.0", MicrocodeRevision: "0x42",
		IrqAffinitySha256: strings.Repeat("5", 64), IrqPolicySha256: strings.Repeat("6", 64), LogicalCpuCount: 12,
		SMTDisabled: true, GovernorPinned: true, TurboPinned: true, NumaPinned: true, IrqPinned: true,
		CgroupV2: true, ServicesInJobCgroup: true, DefaultDenyNetwork: true, OfflineBuildCache: true,
		TemplateDatabase: true, RedisReset: true, ArtifactStorage: true, ImmutableReports: true,
		NoProductionSecrets: true, CleanupVerified: true, ResourceLimitsVerified: true,
		ManagementCpuReserved: true, ManagementMemoryReserved: true, ResourceBombCleanupVerified: true,
		RebaselinePassed: true, RebaselineRoundId: &rebaselineRoundId, Checks: map[string]bool{"all": true},
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

func TestHostEligibilityRejectsMissingOrMalformedIrqEvidence(t *testing.T) {
	settings := validSettings()
	for _, test := range []struct {
		name              string
		irqAffinitySha256 string
		irqPolicySha256   string
	}{
		{name: "missing affinity", irqAffinitySha256: "", irqPolicySha256: strings.Repeat("6", 64)},
		{name: "malformed affinity", irqAffinitySha256: strings.Repeat("g", 64), irqPolicySha256: strings.Repeat("6", 64)},
		{name: "missing policy", irqAffinitySha256: strings.Repeat("5", 64), irqPolicySha256: ""},
		{name: "malformed policy", irqAffinitySha256: strings.Repeat("5", 64), irqPolicySha256: strings.Repeat("A", 64)},
	} {
		check := passingHostCheck(settings)
		check.IrqAffinitySha256 = test.irqAffinitySha256
		check.IrqPolicySha256 = test.irqPolicySha256
		if check.Eligible(settings) {
			t.Errorf("%s IRQ evidence was eligible", test.name)
		}
	}
}

func passingScoreGates() map[string]Gate {
	gates := make(map[string]Gate, len(requiredScoreGateNames))
	for _, name := range requiredScoreGateNames {
		gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	return gates
}

func TestValidateScoreRequiresFrozenGateSet(t *testing.T) {
	raw, normalized := 10.0, 100.0
	score := &ScoreResult{
		ScoreSchema: ScoreSchema, RawScore: &raw, NormalizedScore: &normalized,
		Placeable: true, Gates: passingScoreGates(),
	}
	if err := validateScore(score); err != nil {
		t.Fatal(err)
	}
	delete(score.Gates, "G6_resources")
	if err := validateScore(score); err == nil {
		t.Fatal("score without every frozen gate was accepted")
	}
	score.Gates["replacement"] = Gate{Passed: true, Details: map[string]any{}}
	if err := validateScore(score); err == nil {
		t.Fatal("score with a renamed frozen gate was accepted")
	}
}

func TestWorkerRetriesInfrastructureUnderSameJob(t *testing.T) {
	settings := validSettings()
	raw, normalized := 10.0, 100.0
	jobId := server.NewId()
	hostCheck := passingHostCheck(settings)
	roundId := *hostCheck.RebaselineRoundId
	jobs := []*queuedJob{
		{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: roundId}, AttemptCount: 1},
		{ScoreJobResult: ScoreJobResult{JobId: jobId, RoundId: roundId}, AttemptCount: 2},
	}
	store := &fakeStore{claimJobs: jobs}
	evaluator := &fakeEvaluator{
		check: hostCheck,
		outcomes: []EvaluationOutcome{
			{Error: infrastructureError("host_transient", "host transient"), Infrastructure: true},
			{Score: &ScoreResult{ScoreSchema: 1, RawScore: &raw, NormalizedScore: &normalized, Placeable: true, Gates: passingScoreGates()}, ArtifactManifest: []byte(`{"schema":1}`)},
		},
	}
	worker, err := NewWorker(settings, store, evaluator, "box-a-worker")
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
	round := &roundRecord{
		RoundResult: RoundResult{
			RoundId: roundId, ProvidersSha256: hex.EncodeToString(digest[:]),
			RevealAt: server.NowUtc().Add(-time.Minute),
		},
		ProvidersPath: path,
	}
	service := NewService(settings, &fakeStore{round: round})
	got, gotDigest, status, evalError := service.GetRoundWorkload(context.Background(), roundId)
	if evalError != nil || status != 200 || gotDigest != round.ProvidersSha256 || !reflect.DeepEqual(got, providers) {
		t.Fatalf("revealed workload = %q, %q, %d, %#v", got, gotDigest, status, evalError)
	}
	round.RevealAt = server.NowUtc().Add(time.Minute)
	if _, _, status, evalError := service.GetRoundWorkload(context.Background(), roundId); status != 409 || evalError == nil || evalError.Code != "round_not_revealed" {
		t.Fatalf("active workload response = %d, %#v", status, evalError)
	}
}

func TestSeedKeyExampleEncoding(t *testing.T) {
	if got := base64.StdEncoding.EncodeToString(validSettings().SeedKey); got == "" {
		t.Fatal("empty seed-key example")
	}
}
