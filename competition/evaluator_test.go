package competition

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

func TestEvaluatorRequestBindsCanonicalPatchDigest(t *testing.T) {
	settings := validSettings()
	job := &queuedJob{
		ScoreJobResult: ScoreJobResult{
			JobId:       server.NewId(),
			RoundId:     server.NewId(),
			PatchSha256: strings.Repeat("c", 64),
		},
		AttemptCount: 2,
		Round: roundRecord{
			RoundResult:   RoundResult{ProvidersSha256: strings.Repeat("d", 64)},
			ProvidersPath: "/trusted/round/providers.yml",
		},
	}
	request := evaluatorRequestForJob(settings, job, strings.Repeat("e", 64), "/artifacts/attempt-02", "/artifacts/attempt-02/canonical.patch")
	if request.PatchSha256 != job.PatchSha256 {
		t.Fatalf("patch SHA-256 = %q, want %q", request.PatchSha256, job.PatchSha256)
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
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "z.yml"), []byte("z: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "a.yml"), []byte("a: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := hashLocalMountDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	shellDigest, err := exec.Command("./container/hash-local-mount.sh", root).Output()
	if err != nil || strings.TrimSpace(string(shellDigest)) != first {
		t.Fatalf("host and Go local-mount digests differ: %q %q %v", first, shellDigest, err)
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
