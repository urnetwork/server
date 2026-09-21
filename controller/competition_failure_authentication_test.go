// Rejects hostile failure evidence before any retained object can be uploaded.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Canonicalizes fresh fixture roots, including Darwin's /var alias, before any
// intentional links are added. Production readers still reject every link.
func failureEvidenceTestTempDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve failure evidence fixture directory: %v", err)
	}
	return directory
}

// Builds one synthetic sanitizer identity with a known regular-file digest.
func failureEvidenceAuthenticationFixture(t *testing.T) (string, *queuedJob, map[string]any) {
	t.Helper()
	root := failureEvidenceTestTempDir(t)
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{JobId: server.NewId(), RoundId: server.NewId()},
		AttemptCount:   1,
	}
	artifact := writeArchiveTestFile(t, root, "failed-evidence/failure.json", []byte("synthetic sanitized evidence\n"))
	if err := authenticateFailureArtifact(root, artifact); err != nil {
		t.Fatalf("authenticate pristine failure evidence fixture: %v", err)
	}
	return root, job, map[string]any{
		"schema": 1, "kind": "sim-latency-failed-evidence-manifest",
		"job_id": job.JobId.String(), "round_id": job.RoundId.String(),
		"attempt": job.AttemptCount, "sanitized": true, "artifacts": []evaluationArtifact{artifact},
	}
}

// Publishes exactly the immutable wire bytes a sanitizer would hand to the controller.
func writeFailureAuthenticationManifest(t *testing.T, root string, manifest map[string]any) []byte {
	t.Helper()
	value, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "failed-evidence-manifest.json"), value, 0o400); err != nil {
		t.Fatal(err)
	}
	return value
}

// Positive control authenticates the manifest itself as well as its declared evidence.
func TestFailureEvidenceManifestAuthenticatesExactSanitizedBytes(t *testing.T) {
	root, job, manifest := failureEvidenceAuthenticationFixture(t)
	value := writeFailureAuthenticationManifest(t, root, manifest)
	artifacts, present, err := authenticateFailedEvidence(root, job)
	if err != nil || !present || len(artifacts) != 2 {
		t.Fatalf("authenticated failure evidence = %+v, present = %v, error = %v", artifacts, present, err)
	}
	for _, artifact := range artifacts {
		if err := authenticateFailureArtifact(root, artifact); err != nil {
			t.Fatal(err)
		}
		if artifact.Path == "failed-evidence-manifest.json" && artifact.Bytes != int64(len(value)) {
			t.Fatal("retained manifest size does not describe the decoded bytes")
		}
	}
}

// A synthetic temp-directory alias exercises Darwin's /var boundary on every Unix host.
func TestFailureEvidenceFixturesResolveTempDirectoryAliases(t *testing.T) {
	parent, err := os.MkdirTemp("", "synthetic-failure-fixtures-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(parent); err != nil {
			t.Error(err)
		}
	})
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	realDirectory := filepath.Join(canonicalParent, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(canonicalParent, "alias")
	if err := os.Symlink(realDirectory, alias); err != nil {
		t.Fatal(err)
	}
	// Set this before the first t.TempDir call so its parent includes the alias.
	t.Setenv("TMPDIR", alias)
	root, job, manifest := failureEvidenceAuthenticationFixture(t)
	writeFailureAuthenticationManifest(t, root, manifest)
	if _, present, err := authenticateFailedEvidence(root, job); err != nil || !present {
		t.Errorf("authentication fixture below a temp alias: present = %v, error = %v", present, err)
	}
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, nil) + "exit 17\n"
	})
	for _, root := range []string{root, settings.ArtifactRoot} {
		canonicalRoot, err := filepath.EvalSymlinks(root)
		if err != nil || canonicalRoot != root {
			t.Errorf("failure fixture root is not canonical: %q, resolved = %q, error = %v", root, canonicalRoot, err)
		}
	}
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "evaluator_exit" || archive.calls != 1 {
		t.Fatalf("evaluator fixture below a temp alias: outcome = %+v, archive calls = %d", outcome, archive.calls)
	}
}

// The normal completion branch may win select even when cancellation is already ready.
func TestCandidateStageFailureEligibilityRejectsCanceledSuccessfulWait(t *testing.T) {
	cases := []struct {
		name     string
		canceled bool
		exitCode int
		runErr   error
		closeErr error
		eligible bool
	}{
		{name: "candidate exit", exitCode: 17, eligible: true},
		{name: "candidate limit exit", exitCode: 137, eligible: true},
		{name: "canceled with successful wait", canceled: true, exitCode: 17},
		{name: "canceled process", canceled: true, exitCode: 17, runErr: context.Canceled},
		{name: "zero exit", exitCode: 0},
		{name: "signal termination", exitCode: -1},
		{name: "oversized exit", exitCode: 256},
		{name: "cleanup failure", exitCode: 17, runErr: errors.New("synthetic cleanup failure")},
		{name: "stderr close failure", exitCode: 17, closeErr: errors.New("synthetic close failure")},
	}
	for _, testCase := range cases {
		if actual := candidateStageFailureEligible(testCase.canceled, testCase.exitCode, testCase.runErr, testCase.closeErr); actual != testCase.eligible {
			t.Errorf("%s eligibility = %v, want %v", testCase.name, actual, testCase.eligible)
		}
	}
}

// Schema, identity, duplicate-field, and trailing-data failures are all terminal authentication errors.
func TestFailureEvidenceManifestRejectsInvalidIdentityAndEncoding(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
		bytes  func([]byte) []byte
	}{
		{name: "schema", mutate: func(value map[string]any) { value["schema"] = 2 }},
		{name: "kind", mutate: func(value map[string]any) { value["kind"] = "synthetic-other-kind" }},
		{name: "job", mutate: func(value map[string]any) { value["job_id"] = server.NewId().String() }},
		{name: "round", mutate: func(value map[string]any) { value["round_id"] = server.NewId().String() }},
		{name: "attempt", mutate: func(value map[string]any) { value["attempt"] = 2 }},
		{name: "unsanitized", mutate: func(value map[string]any) { value["sanitized"] = false }},
		{name: "missing sanitizer", mutate: func(value map[string]any) { delete(value, "sanitized") }},
		{name: "unknown field", mutate: func(value map[string]any) { value["synthetic_unknown"] = true }},
		{name: "empty artifacts", mutate: func(value map[string]any) { value["artifacts"] = []evaluationArtifact{} }},
		{name: "missing byte count", mutate: func(value map[string]any) {
			artifact := value["artifacts"].([]evaluationArtifact)[0]
			value["artifacts"] = []map[string]any{{"path": artifact.Path, "sha256": artifact.Sha256}}
		}},
		{name: "unknown artifact field", mutate: func(value map[string]any) {
			artifact := value["artifacts"].([]evaluationArtifact)[0]
			value["artifacts"] = []map[string]any{{"path": artifact.Path, "sha256": artifact.Sha256, "bytes": artifact.Bytes, "unknown": true}}
		}},
		{name: "duplicate field", bytes: func(value []byte) []byte { return append([]byte(`{"schema":1,`), value[1:]...) }},
		{name: "case alias", bytes: func(value []byte) []byte { return bytes.Replace(value, []byte(`"schema"`), []byte(`"Schema"`), 1) }},
		{name: "trailing document", bytes: func(value []byte) []byte { return append(value, []byte(` {"schema":1}`)...) }},
		{name: "trailing token", bytes: func(value []byte) []byte { return append(value, []byte(` true`)...) }},
		{name: "trailing garbage", bytes: func(value []byte) []byte { return append(value, []byte(` synthetic`)...) }},
	}
	for _, testCase := range cases {
		root, job, manifest := failureEvidenceAuthenticationFixture(t)
		if testCase.mutate != nil {
			testCase.mutate(manifest)
		}
		value := writeFailureAuthenticationManifest(t, root, manifest)
		if testCase.bytes != nil {
			path := filepath.Join(root, "failed-evidence-manifest.json")
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, testCase.bytes(value), 0o400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
		}
		if artifacts, present, err := authenticateFailedEvidence(root, job); err == nil || !present || artifacts != nil {
			t.Errorf("%s: invalid manifest accepted: %+v, present = %v, error = %v", testCase.name, artifacts, present, err)
		}
	}
}

// Even a matching digest cannot authorize hidden inputs, credentials, or an escaping path.
func TestFailureEvidenceManifestRejectsUnsafeArtifacts(t *testing.T) {
	for _, path := range []string{
		"worker-request.json", "providers.yml", "/synthetic/absolute.json", "../worker-request.json",
		"failed-evidence/../worker-request.json", "failed-evidence//failure.json", "failed-evidence/./failure.json",
		"failed-evidence/input/providers.yml", "failed-evidence/scorer-input/request.json",
		"failed-evidence/score-runtime/value", "failed-evidence/runs/candidate/runtime/throwaway.env",
		"failed-evidence/evaluation-sources.synthetic/source.go", "failed-evidence/.hidden/value",
		"failed-evidence/run.env", "failed-evidence/run.env.new", "failed-evidence/run.ENV.NEW",
		"failed-evidence/config/local.json", "failed-evidence/vault/local.json", "failed-evidence/secrets/value",
		"failed-evidence/worker-request.json", "failed-evidence/providers.yml", "failed-evidence/round_seed.hex",
		"failed-evidence/round_seed_hex", "failed-evidence/log\nvalue", "failed-evidence/path\\value",
	} {
		root, job, manifest := failureEvidenceAuthenticationFixture(t)
		artifact := manifest["artifacts"].([]evaluationArtifact)[0]
		artifact.Path = path
		manifest["artifacts"] = []evaluationArtifact{artifact}
		writeFailureAuthenticationManifest(t, root, manifest)
		if _, _, err := authenticateFailedEvidence(root, job); err == nil {
			t.Errorf("unsafe evidence path was accepted: %q", path)
		}
	}
}

// Leaf links, linked parents, hard links, and special files must not cross retention.
func TestFailureEvidenceManifestRejectsLinksAndSpecialFiles(t *testing.T) {
	for _, mode := range []string{"symlink", "parent symlink", "root symlink", "ancestor symlink", "hardlink", "directory", "fifo"} {
		root, job, manifest := failureEvidenceAuthenticationFixture(t)
		artifact := manifest["artifacts"].([]evaluationArtifact)[0]
		targetRoot := failureEvidenceTestTempDir(t)
		target := writeArchiveTestFile(t, targetRoot, "synthetic-secret.json", []byte("synthetic sanitized evidence\n"))
		fullPath := filepath.Join(root, filepath.FromSlash(artifact.Path))
		if err := os.Remove(fullPath); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "symlink":
			if err := os.Symlink(filepath.Join(targetRoot, target.Path), fullPath); err != nil {
				t.Fatal(err)
			}
		case "parent symlink":
			if err := os.Remove(filepath.Dir(fullPath)); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(filepath.Join(targetRoot, target.Path), filepath.Join(targetRoot, "failure.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(targetRoot, filepath.Dir(fullPath)); err != nil {
				t.Fatal(err)
			}
		case "root symlink", "ancestor symlink":
			writeArchiveTestFile(t, root, artifact.Path, []byte("synthetic sanitized evidence\n"))
			writeFailureAuthenticationManifest(t, root, manifest)
			linkedRoot := filepath.Join(failureEvidenceTestTempDir(t), "linked-attempt")
			target := root
			if mode == "ancestor symlink" {
				target = filepath.Dir(root)
			}
			if err := os.Symlink(target, linkedRoot); err != nil {
				t.Fatal(err)
			}
			if mode == "ancestor symlink" {
				linkedRoot = filepath.Join(linkedRoot, filepath.Base(root))
			}
			root = linkedRoot
		case "hardlink":
			if err := os.Link(filepath.Join(targetRoot, target.Path), fullPath); err != nil {
				t.Fatal(err)
			}
		case "directory":
			if err := os.Mkdir(fullPath, 0o700); err != nil {
				t.Fatal(err)
			}
		case "fifo":
			if err := syscall.Mkfifo(fullPath, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if mode != "root symlink" && mode != "ancestor symlink" {
			writeFailureAuthenticationManifest(t, root, manifest)
		}
		if _, _, err := authenticateFailedEvidence(root, job); err == nil {
			t.Errorf("unsafe %s was accepted", mode)
		}
	}
}

// Manifest authentication checks size, digest, duplicates, and cumulative evidence bounds.
func TestFailureEvidenceManifestRejectsHashSizeAndQuotaViolations(t *testing.T) {
	for _, mode := range []string{"hash", "size", "negative size", "duplicate path", "oversized entry", "cumulative bytes", "entry count", "manifest bytes"} {
		root, job, manifest := failureEvidenceAuthenticationFixture(t)
		artifacts := manifest["artifacts"].([]evaluationArtifact)
		switch mode {
		case "hash":
			artifacts[0].Sha256 = strings.Repeat("a", 64)
		case "size":
			artifacts[0].Bytes++
		case "negative size":
			artifacts[0].Bytes = -1
		case "duplicate path":
			artifacts = append(artifacts, artifacts[0])
		case "oversized entry":
			artifacts[0].Bytes = maxFailureEvidenceBytes + 1
		case "cumulative bytes":
			artifacts[0].Bytes = maxFailureEvidenceBytes
			artifacts = append(artifacts, evaluationArtifact{Path: "failed-evidence/stderr.log", Sha256: artifacts[0].Sha256, Bytes: 1})
		case "entry count":
			artifacts = make([]evaluationArtifact, maxFailureArtifacts+1)
		}
		manifest["artifacts"] = artifacts
		writeFailureAuthenticationManifest(t, root, manifest)
		if mode == "manifest bytes" {
			path := filepath.Join(root, "failed-evidence-manifest.json")
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(path, maxFailureManifestBytes+1); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o400); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := authenticateFailedEvidence(root, job); err == nil {
			t.Errorf("invalid %s was accepted", mode)
		}
	}
}

// Counts uploads while preserving the real retained-store implementation beneath it.
type failureArchiveCountingStore struct {
	server.RetainedBlobStore
	puts int
}

// Records every durable write, including writes that precede a later authentication error.
func (self *failureArchiveCountingStore) PutRetained(ctx context.Context, key, path, contentType string, retainUntil time.Time) (*server.BlobRetention, error) {
	self.puts++
	return self.RetainedBlobStore.PutRetained(ctx, key, path, contentType, retainUntil)
}

// Full preflight prevents a valid first object from uploading before a later unsafe one.
func TestBlobArtifactArchiveRejectsUnsafeFailureBeforeUploading(t *testing.T) {
	for _, unsafePath := range []string{"worker-request.json", "providers.yml", "failed-evidence/input/hidden.json", "failed-evidence/invalid-hash.json"} {
		settings := validSettings()
		store := &failureArchiveCountingStore{RetainedBlobStore: server.NewLocalBlobStore(t.TempDir(), "synthetic-evidence").(server.RetainedBlobStore)}
		archive := &blobArtifactArchive{store: store}
		job := &queuedJob{
			ScoreJobResult: ScoreJobResult{
				JobId: server.NewId(), RoundId: server.NewId(),
				EvaluatorImageDigest: settings.EvaluatorImageDigest,
				ApiImageDigest:       testApiImageDigest(), WorkerImageDigest: testWorkerImageDigest(),
			},
			AttemptCount: 1,
		}
		root := failureEvidenceTestTempDir(t)
		patch := writeArchiveTestFile(t, root, "canonical.patch", []byte("synthetic patch\n"))
		stderr := writeArchiveTestFile(t, root, "worker.stderr.log", nil)
		valid := writeArchiveTestFile(t, root, "controller-failure.json", []byte("synthetic diagnostic\n"))
		if err := authenticateFailureArchiveArtifacts(root, []evaluationArtifact{patch, stderr, valid}); err != nil {
			t.Fatalf("authenticate pristine failure archive fixture: %v", err)
		}
		invalid := writeArchiveTestFile(t, root, unsafePath, []byte("synthetic must-not-upload\n"))
		if strings.HasSuffix(unsafePath, "invalid-hash.json") {
			invalid.Sha256 = strings.Repeat("a", 64)
		}
		job.PatchSha256 = patch.Sha256
		_, err := archive.ArchiveAttempt(context.Background(), settings, job, root, artifactManifest{
			Schema: 1, JobId: job.JobId.String(), RoundId: job.RoundId.String(), Attempt: job.AttemptCount,
			EvaluatorImageDigest: job.EvaluatorImageDigest, ApiImageDigest: job.ApiImageDigest, WorkerImageDigest: job.WorkerImageDigest,
			PatchSha256: patch.Sha256, StderrSha256: stderr.Sha256,
			Failure: infrastructureError("evaluator_exit", "synthetic failure"), Artifacts: []evaluationArtifact{valid, invalid},
		})
		if err == nil || store.puts != 0 {
			t.Errorf("unsafe artifact %q: archive error = %v, writes before rejection = %d", unsafePath, err, store.puts)
		}
	}
}
