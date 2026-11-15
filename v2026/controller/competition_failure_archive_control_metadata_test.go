// Keeps frozen evaluator source inventories out of retained failure objects without relaxing the scrub boundary.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The uploaded archive manifest selects objects; the unchanged sanitizer manifest records declarations only.
func TestCommandEvaluatorArchivesFailureWithoutSourceInventoryMetadata(t *testing.T) {
	inventory := `{"schema":1,"kind":"sim-latency-temporary-evaluation-sources","origin":"authenticated_evaluator_image","host_repositories_used":false,"runtime_target":"/workspace","read_only_mount":true,"baseline":{"synthetic":"baseline-inventory-content"},"candidate":{"synthetic":"candidate-inventory-content"},"cleanup_complete":true}`
	pending := `{"schema":1,"synthetic":"pending-inventory-content"`
	var originalManifest []byte
	settings, job, _ := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, func(manifest map[string]any) {
			artifacts := manifest["artifacts"].([]evaluationArtifact)
			// Valid declarations deliberately do not match the omitted contents, so hashing them would fail.
			manifest["artifacts"] = append(artifacts,
				evaluationArtifact{Path: "failed-evidence/evaluation-sources.json", Sha256: strings.Repeat("0", 64), Bytes: int64(len(inventory))},
				evaluationArtifact{Path: "failed-evidence/evaluation-sources.json.new", Sha256: strings.Repeat("1", 64), Bytes: int64(len(pending))},
			)
			var err error
			originalManifest, err = json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
		}) + "printf '%s' " + failureArchiveShellQuote(inventory) + " > failed-evidence/evaluation-sources.json\n" +
			"printf '%s' " + failureArchiveShellQuote(pending) + " > failed-evidence/evaluation-sources.json.new\nexit 17\n"
	})
	settings.RetainUntil = server.NowUtc().Add(time.Hour).Truncate(time.Second)
	store := server.NewLocalBlobStore(t.TempDir(), "synthetic-failure-inventory").(server.RetainedBlobStore)
	settings.artifactArchive = &blobArtifactArchive{store: store}
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "evaluator_exit" || outcome.Score != nil {
		t.Fatalf("inventory omission outcome = %+v", outcome)
	}
	var manifest artifactManifest
	if err := json.Unmarshal(outcome.ArtifactManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Failure == nil || manifest.Failure.Code != "evaluator_exit" || manifest.Retention == nil ||
		!manifest.Retention.AuthenticatedAfterUpload || manifest.Retention.ObjectCount != 5 {
		t.Fatalf("retained failure metadata = %+v, retention = %+v", manifest.Failure, manifest.Retention)
	}
	for _, artifact := range manifest.Artifacts {
		if strings.HasPrefix(artifact.Path, "failed-evidence/evaluation-sources.") {
			t.Fatalf("archive manifest selected omitted inventory: %q", artifact.Path)
		}
	}
	sealedManifestFound := false
	for _, artifact := range manifest.Retention.Objects {
		if strings.HasPrefix(artifact.Path, "failed-evidence/evaluation-sources.") {
			t.Fatalf("retention selected omitted inventory: %q", artifact.Path)
		}
		reader, err := store.GetVersion(context.Background(), artifact.Key, artifact.VersionId)
		if err != nil {
			t.Fatal(err)
		}
		value, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read retained object: %v, close: %v", readErr, closeErr)
		}
		if bytes.Contains(value, []byte("baseline-inventory-content")) || bytes.Contains(value, []byte("pending-inventory-content")) {
			t.Fatalf("omitted source inventory content reached retained object %q", artifact.Path)
		}
		if artifact.Path == "failed-evidence-manifest.json" {
			sealedManifestFound = true
			if !bytes.Equal(value, originalManifest) {
				t.Fatal("original sealed sanitizer manifest changed")
			}
			var original failedEvidenceManifest
			if err := json.Unmarshal(value, &original); err != nil || len(original.Artifacts) != 3 {
				t.Fatalf("sealed declaration inventory = %+v, error = %v", original, err)
			}
		}
	}
	if !sealedManifestFound {
		t.Fatal("sealed original sanitizer manifest was not retained")
	}
	objects, err := store.List(context.Background(), "")
	if err != nil || len(objects) != manifest.Retention.ObjectCount+1 {
		t.Fatalf("stored objects = %d, selected objects plus archive manifest = %d, error = %v", len(objects), manifest.Retention.ObjectCount+1, err)
	}
}

// A symlink and an absent pending file prove omission happens before opening either source inventory.
func TestCommandEvaluatorOmitsSourceInventoryMetadataWithoutOpening(t *testing.T) {
	outsidePath := filepath.Join(t.TempDir(), "synthetic-inventory.json")
	if err := os.WriteFile(outsidePath, []byte("synthetic inventory outside the attempt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
		return failureArchiveEvidenceScript(t, job, func(manifest map[string]any) {
			artifacts := manifest["artifacts"].([]evaluationArtifact)
			manifest["artifacts"] = append(artifacts,
				evaluationArtifact{Path: "failed-evidence/evaluation-sources.json", Sha256: strings.Repeat("0", 64), Bytes: 17},
				evaluationArtifact{Path: "failed-evidence/evaluation-sources.json.new", Sha256: strings.Repeat("1", 64), Bytes: 23},
			)
		}) + "ln -s " + failureArchiveShellQuote(outsidePath) + " failed-evidence/evaluation-sources.json\nexit 17\n"
	})
	outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
	if outcome.Error == nil || outcome.Error.Code != "evaluator_exit" || archive.calls != 1 {
		t.Fatalf("unopened inventory outcome = %+v, retention calls = %d", outcome, archive.calls)
	}
	for _, artifact := range archive.manifest.Artifacts {
		if strings.HasPrefix(artifact.Path, "failed-evidence/evaluation-sources.") {
			t.Fatalf("unopened inventory selected for retention: %q", artifact.Path)
		}
	}
}

// Omitted declarations still have exact digest, length, duplicate and aggregate budget checks.
func TestCommandEvaluatorRejectsInvalidOmittedSourceInventoryDeclarations(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		path      string
		digest    string
		byteCount any
		duplicate bool
	}{
		{name: "malformed digest", path: "failed-evidence/evaluation-sources.json", digest: strings.Repeat("g", 64), byteCount: int64(1)},
		{name: "negative bytes", path: "failed-evidence/evaluation-sources.json", digest: strings.Repeat("0", 64), byteCount: int64(-1)},
		{name: "null bytes", path: "failed-evidence/evaluation-sources.json", digest: strings.Repeat("0", 64), byteCount: nil},
		{name: "duplicate inventory", path: "failed-evidence/evaluation-sources.json", digest: strings.Repeat("0", 64), byteCount: int64(1), duplicate: true},
		{name: "duplicate pending inventory", path: "failed-evidence/evaluation-sources.json.new", digest: strings.Repeat("1", 64), byteCount: int64(1), duplicate: true},
		{name: "total byte budget", path: "failed-evidence/evaluation-sources.json.new", digest: strings.Repeat("1", 64), byteCount: int64(maxFailureEvidenceBytes)},
	} {
		settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
			return failureArchiveEvidenceScript(t, job, func(manifest map[string]any) {
				declaration := map[string]any{"path": testCase.path, "sha256": testCase.digest, "bytes": testCase.byteCount}
				artifacts := []any{manifest["artifacts"].([]evaluationArtifact)[0], declaration}
				if testCase.duplicate {
					artifacts = append(artifacts, declaration)
				}
				manifest["artifacts"] = artifacts
			}) + "exit 17\n"
		})
		outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
		if outcome.Error == nil || outcome.Error.Code != "artifact_archive_failed" || archive.calls != 0 || len(outcome.ArtifactManifest) != 0 {
			t.Errorf("%s: invalid omitted declaration outcome = %+v, retention calls = %d", testCase.name, outcome, archive.calls)
		}
	}
}

// Nested inventories, lookalikes and scrubbed secrets are not part of the exact root omission.
func TestCommandEvaluatorRejectsSourceInventoryLookalikesAndSecrets(t *testing.T) {
	for _, path := range []string{
		"failed-evidence/runs/evaluation-sources.json",
		"failed-evidence/Evaluation-sources.json",
		"failed-evidence/evaluation-sources.json.backup",
		"failed-evidence/evaluation-sources.json.new.backup",
		"failed-evidence/evaluation-sources.json/nested.json",
		"failed-evidence/vault/local/synthetic-key",
		"failed-evidence/config/main/synthetic.yml",
		"failed-evidence/.env",
	} {
		settings, job, archive := failureArchiveEvaluatorFixture(t, func(_ *Settings, job *queuedJob) string {
			return failureArchiveEvidenceScript(t, job, func(manifest map[string]any) {
				manifest["artifacts"] = append(manifest["artifacts"].([]evaluationArtifact), evaluationArtifact{
					Path: path, Sha256: strings.Repeat("0", 64), Bytes: 1,
				})
			}) + "exit 17\n"
		})
		outcome := (CommandEvaluator{}).Evaluate(context.Background(), settings, job)
		if outcome.Error == nil || outcome.Error.Code != "artifact_archive_failed" || archive.calls != 0 || len(outcome.ArtifactManifest) != 0 {
			t.Errorf("%s: unsafe evidence outcome = %+v, retention calls = %d", path, outcome, archive.calls)
		}
	}
}

// The final archive gate never authorizes the omitted paths, even when their files authenticate.
func TestAuthenticateFailureArchiveArtifactsRejectsSourceInventoryMetadata(t *testing.T) {
	for _, path := range []string{"failed-evidence/evaluation-sources.json", "failed-evidence/evaluation-sources.json.new"} {
		if safeFailureEvidencePath(path) {
			t.Errorf("omitted inventory path became a permitted failure artifact: %q", path)
		}
		root := failureEvidenceTestTempDir(t)
		artifact := writeArchiveTestFile(t, root, path, []byte("synthetic source inventory\n"))
		if err := authenticateFailureArchiveArtifacts(root, []evaluationArtifact{artifact}); err == nil {
			t.Errorf("final archive gate accepted source inventory %q", path)
		}
	}
}
