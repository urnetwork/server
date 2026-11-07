// Synthetic native Git fixtures exercise authenticated coverage context and
// dedicated-file fencing. No candidate code, image or service is executed.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The fixture owns every mutable byte, raw lock and independently pinned head.
type repositoryCoverageFixture struct {
	root           string
	identityBytes  []byte
	identitySha256 string
	identity       repositorySourceIdentity
	commits        map[string]string
	policy         controller.PatchPolicy
	coverage       model.CompetitionWorkCoveragePolicy
}

// These regular placeholder files prove native custody, not a compiled G7
// instrument. The source identity is rederived after the synthetic base commit.
func repositoryWorkCoverageFixture(t *testing.T) repositoryCoverageFixture {
	t.Helper()
	root, content, _, commits := repositoryApplyFixture(t)
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	paths, err := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		absolute := filepath.Join(root, "server", path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte("synthetic original\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	serverRoot := filepath.Join(root, "server")
	repositoryApplyTestGit(t, serverRoot, "add", "--all")
	repositoryApplyTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic protected helper graph")
	commits["server"] = repositoryApplyTestGit(t, serverRoot, "rev-parse", "HEAD")
	var identity repositorySourceIdentity
	if err := json.Unmarshal(content, &identity); err != nil {
		t.Fatal(err)
	}
	identity.BaseSha, identity.Repositories, identity.WorkCoveragePolicy = commits["server"], commits, &coverage
	content, digest := repositoryApplyTestRetainIdentity(t, root, identity)
	if err := json.Unmarshal(content, &identity); err != nil {
		t.Fatal(err)
	}
	policy := controller.PatchPolicy{PathBase: "mono_root_v1", MaxPatchBytes: 4096,
		AllowedPaths: []string{"server/**", "connect/**"}, ForbiddenPaths: []string{"server/connect/sim-latency/**"}}
	for _, path := range paths {
		policy.ForbiddenPaths = append(policy.ForbiddenPaths, "server/"+path)
	}
	return repositoryCoverageFixture{root: root, identityBytes: content, identitySha256: digest, identity: identity,
		commits: commits, policy: policy, coverage: coverage}
}

// The public CLI receives the original authenticated bytes and native policy.
func repositoryWorkCoverageApply(t *testing.T, fixture repositoryCoverageFixture, policy controller.PatchPolicy, patch []byte) ([]byte, error) {
	t.Helper()
	policyBytes, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	inputs := t.TempDir()
	patchPath := writeTestFile(t, inputs, "candidate.patch", patch)
	policyPath := writeTestFile(t, inputs, "policy.json", policyBytes)
	identityPath := writeTestFile(t, inputs, "source.json", fixture.identityBytes)
	patchDigest, policyDigest := sha256.Sum256(patch), sha256.Sum256(policyBytes)
	var output bytes.Buffer
	err = runContext(t.Context(), []string{"--base-sha", fixture.commits["server"], "--patch", patchPath,
		"--expected-patch-sha256", hex.EncodeToString(patchDigest[:]), "--policy", policyPath,
		"--expected-policy-sha256", hex.EncodeToString(policyDigest[:]), "--apply-source-root", fixture.root,
		"--source-identity", identityPath, "--expected-source-identity-sha256", fixture.identitySha256}, &output)
	return append([]byte{}, output.Bytes()...), err
}

// Mixed product model code remains editable; only the fixed dedicated leaves
// and preexisting simulator tree are excluded from this source application.
func TestCompetitionPatchCliCoverageRetainsMixedProductSurface(t *testing.T) {
	fixture := repositoryWorkCoverageFixture(t)
	patch := []byte("diff --git a/server/model/competition_model.go b/server/model/competition_model.go\nnew file mode 100644\n--- /dev/null\n+++ b/server/model/competition_model.go\n@@ -0,0 +1 @@\n+synthetic product model\n")
	output, err := repositoryWorkCoverageApply(t, fixture, fixture.policy, patch)
	if err != nil {
		t.Fatal("fixed helper protection froze mixed product code", err)
	}
	var receipt imageIdentity
	if err := json.Unmarshal(output, &receipt); err != nil || receipt.CandidateCommits["server"] == fixture.commits["server"] {
		t.Fatal("complete coverage source application lost its receipt", err)
	}
	paths, _ := model.CompetitionWorkCoverageProtectedHelperPaths(&fixture.coverage)
	for _, path := range paths {
		if repositoryApplyTestGit(t, filepath.Join(fixture.root, "server"), "rev-parse", "HEAD:"+path) !=
			repositoryApplyTestGit(t, filepath.Join(fixture.root, "server"), "rev-parse", fixture.commits["server"]+":"+path) {
			t.Fatal("product change rewrote a protected helper", path)
		}
	}
}

// Raw context rejects every missing prohibition before an unrelated product
// patch can mutate source. Removing all restrictions cannot choose legacy.
func TestCompetitionPatchCliCoverageRequiresEveryFrozenHelperProhibition(t *testing.T) {
	for missing := 1; missing < 7; missing++ {
		fixture := repositoryWorkCoverageFixture(t)
		policy := fixture.policy
		policy.ForbiddenPaths = append([]string{}, fixture.policy.ForbiddenPaths[:missing]...)
		policy.ForbiddenPaths = append(policy.ForbiddenPaths, fixture.policy.ForbiddenPaths[missing+1:]...)
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		output, err := repositoryWorkCoverageApply(t, fixture, policy, patch)
		if err == nil || competitionPatchExitCode(err) != 2 || !strings.Contains(err.Error(), "coverage") || len(output) != 0 {
			t.Fatal("coverage raw lock accepted a missing helper prohibition", missing, err)
		}
		if repositoryApplyTestGit(t, filepath.Join(fixture.root, "connect"), "rev-parse", "HEAD") != fixture.commits["connect"] {
			t.Fatal("policy refusal happened after source mutation")
		}
	}
}

// Missing or redirected raw lock bytes never fall back to the flat identity.
func TestCompetitionPatchCliCoverageRequiresActualRawLock(t *testing.T) {
	for _, redirected := range []bool{false, true} {
		fixture := repositoryWorkCoverageFixture(t)
		path := filepath.Join(fixture.root, ".evaluation-source-lock.json")
		if redirected {
			retained := filepath.Join(t.TempDir(), "synthetic-lock.json")
			if err := os.Rename(path, retained); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(retained, path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		output, err := repositoryWorkCoverageApply(t, fixture, fixture.policy, patch)
		if err == nil || competitionPatchExitCode(err) != 2 || len(output) != 0 || !strings.Contains(err.Error(), "raw image source lock") {
			t.Fatal("missing or redirected raw context gained source authority", redirected, err)
		}
	}
}

// Exact CLI decoding, not a standalone DTO test, rejects explicit erasure,
// key ambiguity and mismatched copies while the raw anchor remains genuine.
func TestCompetitionPatchCliCoverageRejectsAmbiguousIdentityCopies(t *testing.T) {
	for _, change := range []string{"missing", "null", "alias", "duplicate", "unknown-kind", "wrong-ratio"} {
		fixture := repositoryWorkCoverageFixture(t)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(fixture.identityBytes, &fields); err != nil {
			t.Fatal(err)
		}
		value := append([]byte{}, fields["work_coverage_policy"]...)
		switch change {
		case "missing":
			delete(fields, "work_coverage_policy")
		case "null":
			fields["work_coverage_policy"] = json.RawMessage("null")
		case "alias":
			delete(fields, "work_coverage_policy")
			fields["Work_Coverage_Policy"] = value
		case "unknown-kind":
			fields["work_coverage_policy"] = bytes.Replace(value, []byte("seeded-crawl-coverage/v1"), []byte("synthetic-unknown/v1"), 1)
		case "wrong-ratio":
			fields["work_coverage_policy"] = bytes.Replace(value, []byte("0.95"), []byte("0.5"), 1)
		}
		content, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if change == "duplicate" {
			content = bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"work_coverage_policy":`), 1)
		}
		digest := sha256.Sum256(content)
		fixture.identityBytes, fixture.identitySha256 = content, hex.EncodeToString(digest[:])
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		output, err := repositoryWorkCoverageApply(t, fixture, fixture.policy, patch)
		if err == nil || competitionPatchExitCode(err) != 2 || len(output) != 0 {
			t.Fatal("ambiguous copied identity produced a source receipt", change, err)
		}
		if repositoryApplyTestGit(t, filepath.Join(fixture.root, "connect"), "rev-parse", "HEAD") != fixture.commits["connect"] {
			t.Fatal("identity refusal followed source mutation", change)
		}
	}
}

// The immutable raw anchor is decoded at the actual CLI boundary, even when a
// synthetic caller recomputes its duplicate raw-file and identity digests.
func TestCompetitionPatchCliCoverageRejectsAmbiguousRawAnchor(t *testing.T) {
	for _, change := range []string{"missing", "null", "alias", "duplicate", "unknown-kind", "wrong-ratio"} {
		fixture := repositoryWorkCoverageFixture(t)
		path := filepath.Join(fixture.root, ".evaluation-source-lock.json")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(content, &fields); err != nil {
			t.Fatal(err)
		}
		value := append([]byte{}, fields["work_coverage_policy"]...)
		switch change {
		case "missing":
			delete(fields, "work_coverage_policy")
		case "null":
			fields["work_coverage_policy"] = json.RawMessage("null")
		case "alias":
			delete(fields, "work_coverage_policy")
			fields["Work_Coverage_Policy"] = value
		case "unknown-kind":
			fields["work_coverage_policy"] = bytes.Replace(value, []byte("seeded-crawl-coverage/v1"), []byte("synthetic-unknown/v1"), 1)
		case "wrong-ratio":
			fields["work_coverage_policy"] = bytes.Replace(value, []byte("0.95"), []byte("0.5"), 1)
		}
		content, err = json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if change == "duplicate" {
			content = bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"work_coverage_policy":`), 1)
		}
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		rawDigest := sha256.Sum256(content)
		fixture.identity.SourceLockSha256 = hex.EncodeToString(rawDigest[:])
		fixture.identityBytes, err = json.Marshal(fixture.identity)
		if err != nil {
			t.Fatal(err)
		}
		identityDigest := sha256.Sum256(fixture.identityBytes)
		fixture.identitySha256 = hex.EncodeToString(identityDigest[:])
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		output, err := repositoryWorkCoverageApply(t, fixture, fixture.policy, patch)
		if err == nil || competitionPatchExitCode(err) != 2 || len(output) != 0 {
			t.Fatal("ambiguous actual raw anchor produced a source receipt", change, err)
		}
		if repositoryApplyTestGit(t, filepath.Join(fixture.root, "connect"), "rev-parse", "HEAD") != fixture.commits["connect"] {
			t.Fatal("raw anchor refusal followed source mutation", change)
		}
	}
}

// The existing canonical source-root and independent-clone checks remain in
// front of helper custody. A parent alias cannot redirect the checked trees.
func TestCompetitionPatchCliCoverageRejectsRootAndRepositoryRedirection(t *testing.T) {
	for _, repositoryOnly := range []bool{false, true} {
		fixture := repositoryWorkCoverageFixture(t)
		if repositoryOnly {
			path := filepath.Join(fixture.root, "server")
			retained := filepath.Join(t.TempDir(), "server")
			if err := os.Rename(path, retained); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(retained, path); err != nil {
				t.Fatal(err)
			}
		} else {
			alias := filepath.Join(t.TempDir(), "source-root")
			if err := os.Symlink(fixture.root, alias); err != nil {
				t.Fatal(err)
			}
			fixture.root = alias
		}
		patch := []byte("diff --git a/connect/source.txt b/connect/source.txt\n--- a/connect/source.txt\n+++ b/connect/source.txt\n@@ -1 +1 @@\n-original\n+changed\n")
		output, err := repositoryWorkCoverageApply(t, fixture, fixture.policy, patch)
		if err == nil || competitionPatchExitCode(err) != 2 || len(output) != 0 {
			t.Fatal("redirected source context produced a candidate receipt", repositoryOnly, err)
		}
	}
}

// Appending an omitted field cannot change the old prepared-identity byte
// order or shape. This is distinct from raw-lock serialization parity.
func TestCompetitionPatchCoverageLegacyIdentityBytesRemainUnchanged(t *testing.T) {
	legacy := struct {
		Schema               int               `json:"schema"`
		Kind                 string            `json:"kind"`
		Temporary            bool              `json:"temporary"`
		BaseImageId          string            `json:"base_image_id"`
		BaseSha              string            `json:"base_sha"`
		SourceEpoch          int               `json:"source_epoch"`
		Staging              bool              `json:"staging"`
		Branch               string            `json:"branch"`
		SourceLockSha256     string            `json:"source_lock_sha256"`
		CandidatePathBase    string            `json:"candidate_path_base,omitempty"`
		Repositories         map[string]string `json:"repositories"`
		CandidatePatchSha256 *string           `json:"candidate_patch_sha256"`
	}{Schema: 1, Kind: "sim-latency-evaluation-source", Temporary: true, BaseImageId: "sha256:" + strings.Repeat("b", 64),
		BaseSha: strings.Repeat("a", 40), SourceEpoch: 1, Branch: "sim-latency", SourceLockSha256: strings.Repeat("c", 64),
		Repositories: map[string]string{"server": strings.Repeat("a", 40)}}
	for _, pathBase := range []string{"", "mono_root_v1"} {
		legacy.CandidatePathBase = pathBase
		expected, err := json.Marshal(legacy)
		if err != nil {
			t.Fatal(err)
		}
		var identity repositorySourceIdentity
		if err := json.Unmarshal(expected, &identity); err != nil {
			t.Fatal(err)
		}
		actual, err := json.Marshal(identity)
		if err != nil || !bytes.Equal(expected, actual) {
			t.Fatal("omitted coverage changed historical prepared identity bytes", err)
		}
	}
}

// The real metadata bytes pass through the production authenticator, while a
// private reader records the exact built-in path without touching host /opt.
func TestCompetitionPatchCoverageBuiltinContextHasNoFallback(t *testing.T) {
	fixture := repositoryWorkCoverageFixture(t)
	for _, root := range []string{"/workspace", "/synthetic/private-source"} {
		calls := 0
		read := func(path string, maximum int64) ([]byte, error) {
			calls++
			expected := filepath.Join(root, ".evaluation-source-lock.json")
			if root == "/workspace" {
				expected = "/opt/urnetwork/source-lock.json"
			}
			if path != expected || maximum != maxPolicyBytes {
				t.Fatal("source authenticator selected an unchecked raw-lock path", path)
			}
			return readRegularFile(filepath.Join(fixture.root, ".evaluation-source-lock.json"), maximum)
		}
		coverage, err := authenticateRepositoryWorkCoverageWithReader(t.Context(), root, fixture.identityBytes, fixture.identity, fixture.policy, read)
		if err != nil || coverage == nil || *coverage != fixture.coverage || calls != 1 {
			t.Fatal("fixed source context did not authenticate the actual bytes", root, err)
		}
		refuse := func(path string, maximum int64) ([]byte, error) { calls++; return nil, os.ErrNotExist }
		if _, err := authenticateRepositoryWorkCoverageWithReader(t.Context(), root, fixture.identityBytes, fixture.identity, fixture.policy, refuse); err == nil || calls != 2 {
			t.Fatal("missing built-in raw lock tried an alternate path", err)
		}
	}
}

// Finite read completion cannot erase cancellation or return an authenticated
// policy after the parent retired, including concurrent read/shape failures.
func TestCompetitionPatchCoverageReadCancellationPreservesCause(t *testing.T) {
	fixture := repositoryWorkCoverageFixture(t)
	for _, failure := range []string{"", "unavailable", "malformed", "mismatch"} {
		identity := fixture.identity
		identityBytes := fixture.identityBytes
		if failure == "malformed" {
			digest := sha256.Sum256([]byte("not JSON"))
			identity.SourceLockSha256 = hex.EncodeToString(digest[:])
			var err error
			identityBytes, err = json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(t.Context())
		read := func(path string, maximum int64) ([]byte, error) {
			content, err := readRegularFile(filepath.Join(fixture.root, ".evaluation-source-lock.json"), maximum)
			cancel()
			if failure == "unavailable" {
				return nil, os.ErrNotExist
			}
			if failure == "malformed" {
				return []byte("not JSON"), nil
			}
			if failure == "mismatch" {
				return append(content, ' '), nil
			}
			return content, err
		}
		policy, err := authenticateRepositoryWorkCoverageWithReader(ctx, fixture.root, identityBytes, identity, fixture.policy, read)
		cancel()
		if !errors.Is(err, context.Canceled) || policy != nil {
			t.Fatal("raw-lock read outcome erased parent cancellation", failure, err)
		}
		if failure == "malformed" && !strings.Contains(err.Error(), "is malformed") ||
			failure == "mismatch" && !strings.Contains(err.Error(), "digest differs") {
			t.Fatal("cancellation control did not reach its distinct raw-lock failure", failure, err)
		}
	}
}

// Virtual time forces the metadata owner to expire during a finite read;
// no real sleep, watchdog or source mutation participates in the proof.
func TestCompetitionPatchCoverageReadDeadlinePreservesCause(t *testing.T) {
	fixture := repositoryWorkCoverageFixture(t)
	content, err := readRegularFile(filepath.Join(fixture.root, ".evaluation-source-lock.json"), maxPolicyBytes)
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		for _, failRead := range []bool{false, true} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			read := func(path string, maximum int64) ([]byte, error) {
				time.Sleep(2 * time.Second)
				if failRead {
					return nil, os.ErrNotExist
				}
				return content, nil
			}
			policy, err := authenticateRepositoryWorkCoverageWithReader(ctx, fixture.root, fixture.identityBytes, fixture.identity, fixture.policy, read)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) || policy != nil {
				t.Fatal("raw-lock read lost the metadata owner's deadline", failRead, err)
			}
		}
	})
}

// Coverage protection does not create a new staging authorization rule; the
// original image, branch, and raw-lock provenance continue to own that choice.
func TestCompetitionPatchCoveragePreservesAuthenticatedStagingContext(t *testing.T) {
	fixture := repositoryWorkCoverageFixture(t)
	identity := fixture.identity
	identity.Staging, identity.Branch = true, "sim-latency-staging"
	content, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := authenticateRepositoryWorkCoverage(t.Context(), fixture.root, content, identity, fixture.policy)
	if err != nil || coverage == nil || *coverage != fixture.coverage {
		t.Fatal("helper fencing added an unrelated staging prohibition", err)
	}
	identity.WorkCoveragePolicy = nil
	content, err = json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateRepositoryWorkCoverage(t.Context(), fixture.root, content, identity, fixture.policy); err == nil {
		t.Fatal("staging context erased the independently authenticated raw coverage leaf")
	}
}

// Real Git snapshots use no candidate implementation or metadata parser stub.
func repositoryWorkCoverageTestGit(t *testing.T, root string) func(string, ...string) (string, error) {
	t.Helper()
	return func(repository string, args ...string) (string, error) {
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", filepath.Join(root, repository)}, args...)...)
		output, err := command.CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}
}

// Neither skip-worktree nor assume-unchanged can hide edited physical bytes.
// Each observation owns its map, and changing a returned snapshot is isolated.
func TestCompetitionPatchCoverageSnapshotsRejectHiddenWorkingEdits(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		fixture := repositoryWorkCoverageFixture(t)
		runGit := repositoryWorkCoverageTestGit(t, fixture.root)
		first, err := repositoryWorkCoverageSnapshot(fixture.root, &fixture.coverage, runGit)
		if err != nil {
			t.Fatal(err)
		}
		first["synthetic"] = repositoryWorkCoverageFile{mode: "100644", blob: strings.Repeat("a", 40)}
		second, err := repositoryWorkCoverageSnapshot(fixture.root, &fixture.coverage, runGit)
		if err != nil || maps.Equal(first, second) {
			t.Fatal("coverage snapshots share caller-owned state", err)
		}
		path := "model/competition_work_coverage_policy.go"
		repositoryApplyTestGit(t, filepath.Join(fixture.root, "server"), "update-index", flag, "--", path)
		if err := os.WriteFile(filepath.Join(fixture.root, "server", path), []byte("hidden changed bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if snapshot, err := repositoryWorkCoverageSnapshot(fixture.root, &fixture.coverage, runGit); err == nil || snapshot != nil {
			t.Fatal("Git index visibility flag hid changed protected bytes", flag)
		}
	}
}

// Parent/leaf symlinks, executable drift and staged blobs are refused even
// when callers would otherwise regard the repository as their own source.
func TestCompetitionPatchCoverageSnapshotsRejectModesIndexAndRedirection(t *testing.T) {
	for _, change := range []string{"mode", "index", "leaf-symlink", "parent-symlink"} {
		fixture := repositoryWorkCoverageFixture(t)
		serverRoot := filepath.Join(fixture.root, "server")
		path := "model/competition_work_coverage_policy.go"
		absolute := filepath.Join(serverRoot, path)
		switch change {
		case "mode":
			if err := os.Chmod(absolute, 0755); err != nil {
				t.Fatal(err)
			}
		case "index":
			alternate := writeTestFile(t, t.TempDir(), "synthetic-blob", []byte("different index blob\n"))
			blob := repositoryApplyTestGit(t, serverRoot, "hash-object", "-w", "--no-filters", "--", alternate)
			repositoryApplyTestGit(t, serverRoot, "update-index", "--cacheinfo", "100644,"+blob+","+path)
		case "leaf-symlink":
			alternate := filepath.Join(t.TempDir(), "synthetic-leaf")
			if err := os.Rename(absolute, alternate); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(alternate, absolute); err != nil {
				t.Fatal(err)
			}
		case "parent-symlink":
			alternate := filepath.Join(t.TempDir(), "synthetic-parent")
			if err := os.Rename(filepath.Dir(absolute), alternate); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(alternate, filepath.Dir(absolute)); err != nil {
				t.Fatal(err)
			}
		}
		if snapshot, err := repositoryWorkCoverageSnapshot(fixture.root, &fixture.coverage, repositoryWorkCoverageTestGit(t, fixture.root)); err == nil || snapshot != nil {
			t.Fatal("unsafe physical/index helper state was accepted", change)
		}
	}
}

// Deliberately inconsistent internal routing bypasses the ordinary canonical
// parser to prove that the final native helper fence is independently active.
func TestCompetitionPatchCoveragePostCommitFenceRejectsHelperDrift(t *testing.T) {
	fixture := repositoryWorkCoverageFixture(t)
	patch := []byte("diff --git a/model/competition_g2_policy.go b/model/competition_g2_policy.go\n--- a/model/competition_g2_policy.go\n+++ b/model/competition_g2_policy.go\n@@ -1 +1 @@\n-synthetic original\n+synthetic changed\n")
	digest := sha256.Sum256(patch)
	canonical := &controller.CanonicalPatch{Sha256: hex.EncodeToString(digest[:]), Paths: []string{"server/source.txt"},
		FileOperations: map[string]controller.CompetitionPatchFileOperation{"server/source.txt": {Operation: "modify"}}}
	commits, err := applyRepositoryPatchesContext(t.Context(), fixture.root, fixture.identityBytes, fixture.identitySha256,
		fixture.commits["server"], canonical.Sha256, fixture.policy, canonical, map[string][]byte{"server": patch}, defaultRepositoryApplySettings())
	if err == nil || commits != nil || !strings.Contains(err.Error(), "protected coverage helper") {
		t.Fatal("post-commit helper edit produced successful source authority", err)
	}
}

// The one metadata command is fixed, cancellation-aware and contains no
// caller-selected paths. Extra arguments do not turn it into a general reader.
func TestCompetitionPatchCoverageCensusCommandIsFixedAndBounded(t *testing.T) {
	var output bytes.Buffer
	if err := runContext(t.Context(), []string{"--work-coverage-protected-paths"}, &output); err != nil || output.Len() > 2048 {
		t.Fatal("fixed helper census command failed or exceeded its format", err)
	}
	var census struct {
		Schema int      `json:"schema"`
		Kind   string   `json:"kind"`
		Paths  []string `json:"paths"`
	}
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	paths, _ := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err := json.Unmarshal(output.Bytes(), &census); err != nil || census.Schema != 1 || census.Kind != model.CompetitionWorkCoverageProtectedHelpersVersion || !reflect.DeepEqual(census.Paths, paths) {
		t.Fatal("fixed census metadata differs from its owned policy", err)
	}
	output.Reset()
	if err := runContext(t.Context(), []string{"--work-coverage-protected-paths", "--arbitrary"}, &output); err == nil || output.Len() != 0 {
		t.Fatal("census command accepted additional instructions")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runContext(ctx, []string{"--work-coverage-protected-paths"}, &output); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatal("canceled census request emitted metadata", err)
	}
}
