// File-boundary regressions use only private synthetic directories and data.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Canonicalizes the OS temporary path without weakening path checks on systems
// whose system temporary directory itself has a fixed symlink alias.
func newCalibrationFileRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Testing's per-test child directory need not inherit its parent's private
	// mode. Make the fixture boundary explicit before exercising artifact checks.
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

// Every negative path test needs a valid private root so another rejection
// cannot mask the path/digest/type condition the test intends to exercise.
func TestCalibrationFileRootProvidesPrivateBoundary(t *testing.T) {
	root := newCalibrationFileRoot(t)
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("fixture root mode = %04o, want 0700", info.Mode().Perm())
	}
	binding := writeCalibrationFileFixture(t, root, "synthetic.json", []byte("{}\n"))
	if _, err := readCalibrationArtifact(root, binding.Path, binding.Sha256); err != nil {
		t.Fatalf("valid fixture cannot reach artifact validation: %v", err)
	}
}

// Writes one small synthetic regular file and returns its original binding.
func writeCalibrationFileFixture(t *testing.T, root string, relative string, content []byte) calibrationArtifactBinding {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return calibrationArtifactBinding{Path: relative, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
}

// Pins are checked before an artifact can influence typed reduction.
func TestCalibrationArtifactRejectsChangedBytes(t *testing.T) {
	root := newCalibrationFileRoot(t)
	binding := writeCalibrationFileFixture(t, root, "record.json", []byte("{\"schema\":1}\n"))
	if _, err := readCalibrationArtifact(root, binding.Path, binding.Sha256); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, binding.Path), []byte("{\"schema\":2}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCalibrationArtifact(root, binding.Path, binding.Sha256); err == nil {
		t.Fatal("changed artifact bytes accepted")
	}
}

// Relative path normalization must not turn an outside target into evidence.
func TestCalibrationArtifactRejectsTraversalAndAbsolutePaths(t *testing.T) {
	root := newCalibrationFileRoot(t)
	writeCalibrationFileFixture(t, root, "record.json", []byte("synthetic\n"))
	for _, path := range []string{"../record.json", "nested/../record.json", "/synthetic/record.json", ".", ""} {
		if _, err := calibrationArtifactPath(root, path); err == nil {
			t.Errorf("unsafe path accepted: %q", path)
		}
	}
}

// Neither leaf nor root aliases are accepted as authenticated immutable paths.
func TestCalibrationArtifactRejectsSymlinkAliases(t *testing.T) {
	root := newCalibrationFileRoot(t)
	writeCalibrationFileFixture(t, root, "record.json", []byte("synthetic\n"))
	if err := os.Symlink(filepath.Join(root, "record.json"), filepath.Join(root, "alias.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := calibrationArtifactPath(root, "alias.json"); err == nil {
		t.Fatal("leaf symlink accepted")
	}
	parent := newCalibrationFileRoot(t)
	alias := filepath.Join(parent, "root-alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := calibrationArtifactPath(alias, "record.json"); err == nil {
		t.Fatal("root symlink accepted")
	}
}

// Public/parent directories cannot become an artifact root by path selection.
func TestCalibrationArtifactRequiresPrivateRoot(t *testing.T) {
	root := newCalibrationFileRoot(t)
	writeCalibrationFileFixture(t, root, "record.json", []byte("synthetic\n"))
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := calibrationArtifactPath(root, "record.json"); err == nil {
		t.Fatal("nonprivate artifact root accepted")
	}
}

// Duplicate evidence entries do not count as distinct original files.
func TestCalibrationArtifactBindingsRejectDuplicates(t *testing.T) {
	root := newCalibrationFileRoot(t)
	binding := writeCalibrationFileFixture(t, root, "data.csv", []byte("synthetic,csv\n"))
	settings := defaultCalibrationAnalysisSettings()
	if _, err := verifyCalibrationArtifactBindings(root, []calibrationArtifactBinding{binding}, settings); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyCalibrationArtifactBindings(root, []calibrationArtifactBinding{binding, binding}, settings); err == nil {
		t.Fatal("duplicate binding accepted")
	}
}

// Whole-cohort byte limits apply across individually valid files.
func TestCalibrationArtifactBindingsEnforceTotalByteBudget(t *testing.T) {
	root := newCalibrationFileRoot(t)
	first := writeCalibrationFileFixture(t, root, "first.csv", []byte("synthetic\n"))
	second := writeCalibrationFileFixture(t, root, "second.csv", []byte("synthetic\n"))
	settings := defaultCalibrationAnalysisSettings()
	settings.MaximumCohortBytes = first.Bytes + second.Bytes - 1
	if _, err := verifyCalibrationArtifactBindings(root, []calibrationArtifactBinding{first, second}, settings); err == nil {
		t.Fatal("whole-cohort byte budget bypassed")
	}
}

// Directories are not raw artifact bytes, even with a plausible digest field.
func TestCalibrationArtifactBindingsRejectNonregularFiles(t *testing.T) {
	root := newCalibrationFileRoot(t)
	if err := os.Mkdir(filepath.Join(root, "not-a-file"), 0700); err != nil {
		t.Fatal(err)
	}
	bindings := []calibrationArtifactBinding{{Path: "not-a-file", Sha256: strings.Repeat("a", 64), Bytes: 0}}
	if _, err := verifyCalibrationArtifactBindings(root, bindings, defaultCalibrationAnalysisSettings()); err == nil {
		t.Fatal("directory accepted as artifact bytes")
	}
}

// Only whole-invocation timing and the exact terminal cleanup marker suffice.
func TestCalibrationTerminalRequiresBoundedUniqueCompletion(t *testing.T) {
	valid := "start_utc=2000-01-02T00:00:00Z\nevaluator_exit=0 duration_seconds=6\nend_utc=2000-01-02T00:00:06Z\nterminal_cleanup_verified\n"
	if err := validateCalibrationTerminal([]byte(valid), 10); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{
		strings.ReplaceAll(valid, "evaluator_exit=0", "evaluator_exit=1"),
		strings.ReplaceAll(valid, "duration_seconds=6", "duration_seconds=11"),
		strings.ReplaceAll(valid, "terminal_cleanup_verified", "cleanup_not_verified"),
		valid + "evaluator_exit=0 duration_seconds=6\n",
		valid + "start_utc=2000-01-02T00:00:00Z\n",
		strings.ReplaceAll(valid, "00:00:06Z", "00:00:07Z"),
	} {
		if err := validateCalibrationTerminal([]byte(content), 10); err == nil {
			t.Errorf("invalid terminal accepted: %q", content)
		}
	}
}

// Synthesizes an explicitly pinned request contract, never a live identity.
func newCalibrationRequestFixture(t *testing.T) (map[string]any, calibrationFileManifest, calibrationFileCohort) {
	t.Helper()
	policy := model.CompetitionEvaluationPolicy{Replicates: 9, TakeoverMargin: 0.161, ScoreTimeoutSeconds: 10800}
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(policyBytes)
	manifest := calibrationFileManifest{
		BaseSha: strings.Repeat("a", 40), EvaluatorImageDigest: "sha256:" + strings.Repeat("b", 64),
		ProvidersSha256: strings.Repeat("c", 64), NoopPatchSha256: strings.Repeat("d", 64),
		EvaluationPolicySha256: hex.EncodeToString(digest[:]), RoundSeedHex: strings.Repeat("0", 64),
	}
	cohort := calibrationFileCohort{JobId: "synthetic-job", RoundId: "synthetic-round"}
	request := map[string]any{
		"schema": 1, "source_epoch": 0, "job_id": cohort.JobId, "round_id": cohort.RoundId,
		"base_sha": manifest.BaseSha, "evaluator_image_digest": manifest.EvaluatorImageDigest,
		"providers_sha256": manifest.ProvidersSha256, "patch_sha256": manifest.NoopPatchSha256,
		"round_seed_hex": manifest.RoundSeedHex, "round_baseline_path": "", "round_baseline_sha256": "",
		"scorer_version": officialScorerVersion, "evaluation_policy": policy,
	}
	return request, manifest, cohort
}

// Changing source or reusing another round's control fails before replay.
func TestCalibrationRequestRejectsChangedIdentityAndBaselineReuse(t *testing.T) {
	for _, key := range []string{"base_sha", "evaluator_image_digest", "providers_sha256", "patch_sha256", "round_baseline_path"} {
		request, manifest, cohort := newCalibrationRequestFixture(t)
		original, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCalibrationRequest(original, manifest, cohort, defaultCalibrationAnalysisSettings()); err != nil {
			t.Fatal(err)
		}
		request[key] = "changed-synthetic-value"
		changed, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCalibrationRequest(changed, manifest, cohort, defaultCalibrationAnalysisSettings()); err == nil {
			t.Errorf("changed %s accepted", key)
		}
	}
}

// All shared policy values remain bound, not merely the R9 count.
func TestCalibrationRequestRejectsChangedPolicy(t *testing.T) {
	request, manifest, cohort := newCalibrationRequestFixture(t)
	policy := request["evaluation_policy"].(model.CompetitionEvaluationPolicy)
	policy.DurationMs = 1
	request["evaluation_policy"] = policy
	content, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCalibrationRequest(content, manifest, cohort, defaultCalibrationAnalysisSettings()); err == nil {
		t.Fatal("changed duration accepted under original policy digest")
	}
}

// Duplicate JSON fields cannot be normalized into an apparently valid request.
func TestCalibrationRequestRejectsDuplicateFields(t *testing.T) {
	_, manifest, cohort := newCalibrationRequestFixture(t)
	content := []byte("{\"schema\":1,\"schema\":1}")
	err := validateCalibrationRequest(content, manifest, cohort, defaultCalibrationAnalysisSettings())
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("duplicate fields were not rejected at decoding: %v", err)
	}
}

// JSON null must not become an empty control path or zero source epoch.
func TestCalibrationRequestRejectsNullIdentityValues(t *testing.T) {
	for _, key := range []string{"source_epoch", "round_baseline_path", "round_baseline_sha256"} {
		request, manifest, cohort := newCalibrationRequestFixture(t)
		request[key] = nil
		content, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCalibrationRequest(content, manifest, cohort, defaultCalibrationAnalysisSettings()); err == nil {
			t.Errorf("null %s accepted as its zero value", key)
		}
	}
}

// Null and absent zero-valued policy fields must not acquire the same canonical
// identity as the explicitly declared policy, despite Go zero-value decoding.
func TestCalibrationRequestRejectsNullOrMissingPolicyFields(t *testing.T) {
	for _, missing := range []bool{false, true} {
		request, manifest, cohort := newCalibrationRequestFixture(t)
		content, err := json.Marshal(request["evaluation_policy"])
		if err != nil {
			t.Fatal(err)
		}
		var policy map[string]any
		if err := json.Unmarshal(content, &policy); err != nil {
			t.Fatal(err)
		}
		if missing {
			delete(policy, "queue_limit")
		} else {
			policy["queue_limit"] = nil
		}
		request["evaluation_policy"] = policy
		content, err = json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCalibrationRequest(content, manifest, cohort, defaultCalibrationAnalysisSettings()); err == nil {
			t.Errorf("null/missing zero-valued policy field accepted: missing=%v", missing)
		}
	}
}
