package competition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
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
