package competition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

type rebaselineEvaluator struct {
	check           HostSelfCheck
	omitBaseline    bool
	nonplaceable    bool
	omitGate        bool
	wrongRound      bool
	evaluationCalls int
}

func (e *rebaselineEvaluator) SelfCheck(context.Context, *Settings) (HostSelfCheck, error) {
	return e.check, nil
}

func (e *rebaselineEvaluator) Evaluate(
	_ context.Context,
	settings *Settings,
	job *queuedJob,
) EvaluationOutcome {
	e.evaluationCalls++
	directory := filepath.Join(settings.ArtifactRoot, job.JobId.String(), "attempt-01")
	if err := os.MkdirAll(directory, 0700); err != nil {
		panic(err)
	}
	write := func(name string, value any) {
		bytes, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), append(bytes, '\n'), 0400); err != nil {
			panic(err)
		}
	}
	if !e.omitBaseline {
		roundId := job.RoundId.String()
		if e.wrongRound {
			roundId = server.NewId().String()
		}
		replicates := make([]map[string]any, settings.EvaluationPolicy.Replicates)
		for i := range replicates {
			replicates[i] = map[string]any{"raw_score": 100 + i}
		}
		write("baseline.json", map[string]any{
			"score_schema":       ScoreSchema,
			"kind":               "sim-latency-score-baseline",
			"scorer_version":     ScorerVersion,
			"round_id":           roundId,
			"config_sha256":      job.Round.ProvidersSha256,
			"request_timeout_ms": settings.EvaluationPolicy.RequestTimeoutMs,
			"takeover_margin":    settings.EvaluationPolicy.TakeoverMargin,
			"replicates":         replicates,
		})
	}
	raw, normalized := 100.0, 100.0
	gates := map[string]Gate{
		"G1_success":        {Passed: !e.nonplaceable, Details: map[string]any{}},
		"G2_volume":         {Passed: !e.nonplaceable, Details: map[string]any{}},
		"G3_path_integrity": {Passed: !e.nonplaceable, Details: map[string]any{}},
		"G4_matchmaking":    {Passed: !e.nonplaceable, Details: map[string]any{}},
		"G5_stability":      {Passed: !e.nonplaceable, Details: map[string]any{}},
		"G6_resources":      {Passed: !e.nonplaceable, Details: map[string]any{}},
	}
	if e.omitGate {
		delete(gates, "G6_resources")
	}
	score := &ScoreResult{
		ScoreSchema:     ScoreSchema,
		RawScore:        &raw,
		NormalizedScore: &normalized,
		Placeable:       !e.nonplaceable,
		Gates:           gates,
	}
	security := evaluationSecurity{
		TemplateDatabaseReset: true, RedisReset: true, CgroupContained: true,
		ResourceLimits: true, ManagementCpuReserved: true,
		ManagementMemoryReserved: true, DefaultDenyNetwork: true,
		OfflineBuild: true, OfflineBuildResourceLimits: true,
		NoProductionSecrets: true, StructuralPatchCheck: true,
		AccountingComplete: true, ResourceReportComplete: true,
		CleanupComplete: true, ImmutableReports: true,
		CgroupId: "job.slice", TemplateDatabaseId: strings.Repeat("a", 64),
		RedisGenerationId: strings.Repeat("b", 64),
	}
	write("worker-result.json", evaluatorResult{
		Schema: 1, JobId: job.JobId.String(), Score: score, Security: security,
	})
	artifact := func(name string) evaluationArtifact {
		digest, size, err := hashRegularFile(filepath.Join(directory, name))
		if err != nil {
			panic(err)
		}
		return evaluationArtifact{Path: name, Sha256: digest, Bytes: size}
	}
	workerResult := artifact("worker-result.json")
	evidence := map[string]any{
		"schema":    1,
		"kind":      "sim-latency-evidence-manifest",
		"job_id":    job.JobId.String(),
		"round_id":  job.RoundId.String(),
		"artifacts": []map[string]any{{"path": "evidence/proof.json", "sha256": strings.Repeat("c", 64), "bytes": 1}},
	}
	write("evidence-manifest.json", evidence)
	baselineSha256 := ""
	if !e.omitBaseline {
		baselineSha256 = artifact("baseline.json").Sha256
	}
	evidenceSha256 := artifact("evidence-manifest.json").Sha256
	write("evaluation.complete.json", map[string]any{
		"schema":           1,
		"kind":             "sim-latency-worker-evaluation-complete",
		"job_id":           job.JobId.String(),
		"round_id":         job.RoundId.String(),
		"attempt":          job.AttemptCount,
		"base_image_id":    settings.EvaluatorImageDigest,
		"patch_sha256":     job.PatchSha256,
		"providers_sha256": job.Round.ProvidersSha256,
		"cleanup_complete": true,
		"artifacts":        map[string]string{"baseline": baselineSha256, "evidence_manifest": evidenceSha256},
	})
	artifacts := []evaluationArtifact{
		artifact("evidence-manifest.json"),
		artifact("evaluation.complete.json"),
	}
	if !e.omitBaseline {
		artifacts = append(artifacts, artifact("baseline.json"))
	}
	manifest, err := json.Marshal(artifactManifest{
		Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(),
		Attempt:                job.AttemptCount,
		EvaluatorImageDigest:   settings.EvaluatorImageDigest,
		EvaluatorCommandSha256: settings.EvaluatorCommandSha256,
		RequestSha256:          strings.Repeat("d", 64),
		PatchSha256:            job.PatchSha256,
		StderrSha256:           strings.Repeat("e", 64),
		ResultSha256:           workerResult.Sha256,
		Security:               security,
		Artifacts:              artifacts,
	})
	if err != nil {
		panic(err)
	}
	return EvaluationOutcome{Score: score, ArtifactManifest: manifest}
}

func rebaselineFixture(t *testing.T) (*Settings, *fakeStore, *rebaselineEvaluator) {
	t.Helper()
	settings := validSettings()
	settings.ArtifactRoot = t.TempDir()
	policy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	round := &roundRecord{
		RoundResult:   RoundResult{RoundId: server.NewId(), Status: "open"},
		CompetitionId: settings.CompetitionId,
		PolicyJson:    policy,
	}
	check := passingHostCheck(settings)
	evaluator := &rebaselineEvaluator{check: check}
	return settings, &fakeStore{round: round}, evaluator
}

func TestRunRebaselineAuthenticatesEvidenceWithoutSecrets(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	result, err := RunRebaseline(
		context.Background(),
		settings,
		store,
		evaluator,
		store.round.RoundId,
		testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Schema != RebaselineSchema ||
		result.Kind != "sim-latency-round-rebaseline-evaluation" ||
		result.RoundId != store.round.RoundId ||
		result.JobId == (server.Id{}) ||
		result.BaselineSha256 == "" ||
		result.EvidenceManifestSha256 == "" ||
		result.WorkerResultSha256 == "" ||
		result.EvaluationCompleteSha256 == "" ||
		result.WorkerArtifactManifestSha256 == "" ||
		!result.CandidatePlaceable {
		t.Fatalf("unexpected result: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "seed") ||
		strings.Contains(strings.ToLower(string(encoded)), "token") ||
		strings.Contains(strings.ToLower(string(encoded)), "vault") {
		t.Fatalf("rebaseline evidence disclosed a secret field: %s", encoded)
	}
	if evaluator.evaluationCalls != 1 {
		t.Fatalf("evaluation calls = %d", evaluator.evaluationCalls)
	}
}

func TestRunRebaselineRejectsWrongRoundBeforeEvaluation(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		server.NewId(), testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || evaluator.evaluationCalls != 0 {
		t.Fatalf("wrong round error = %v, calls = %d", err, evaluator.evaluationCalls)
	}
}

func TestRunRebaselineRejectsExistingRoundAttestation(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	evaluator.check.RebaselineRoundId = &store.round.RoundId
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || !strings.Contains(err.Error(), "already re-baselined") ||
		evaluator.evaluationCalls != 0 {
		t.Fatalf("existing rebaseline error = %v, calls = %d", err, evaluator.evaluationCalls)
	}
}

func TestRunRebaselineRejectsClosedRound(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	store.round.Status = "closed"
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || !strings.Contains(err.Error(), "no longer eligible") ||
		evaluator.evaluationCalls != 0 {
		t.Fatalf("closed round error = %v, calls = %d", err, evaluator.evaluationCalls)
	}
}

func TestRunRebaselineFailsClosedOnMissingBaseline(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	evaluator.omitBaseline = true
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || !strings.Contains(err.Error(), "baseline artifact is missing") {
		t.Fatalf("missing baseline error = %v", err)
	}
}

func TestRunRebaselineRejectsNonplaceableTrustedCandidate(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	evaluator.nonplaceable = true
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || !strings.Contains(err.Error(), "did not pass every score gate") {
		t.Fatalf("nonplaceable rebaseline error = %v", err)
	}
}

func TestRunRebaselineRejectsUnpinnedPatchBeforeEvaluation(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"), strings.Repeat("f", 64),
	)
	if err == nil || !strings.Contains(err.Error(), "pinned digest") ||
		evaluator.evaluationCalls != 0 {
		t.Fatalf("unpinned patch error = %v, calls = %d", err, evaluator.evaluationCalls)
	}
}

func TestRunRebaselineRequiresEveryNamedScoreGate(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	evaluator.omitGate = true
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || !strings.Contains(err.Error(), "frozen gate set") {
		t.Fatalf("missing score gate error = %v", err)
	}
}

func TestRunRebaselineRejectsBaselineFromAnotherRound(t *testing.T) {
	settings, store, evaluator := rebaselineFixture(t)
	evaluator.wrongRound = true
	_, err := RunRebaseline(
		context.Background(), settings, store, evaluator,
		store.round.RoundId, testPatch("rebaseline"),
		canonicalPatchSha256(t, settings, testPatch("rebaseline")),
	)
	if err == nil || !strings.Contains(err.Error(), "wrong identity") {
		t.Fatalf("wrong baseline round error = %v", err)
	}
}

func canonicalPatchSha256(t *testing.T, settings *Settings, raw string) string {
	t.Helper()
	patch, err := ValidateAndCanonicalizePatch(raw, settings.PatchPolicy)
	if err != nil {
		t.Fatal(err)
	}
	return patch.Sha256
}
