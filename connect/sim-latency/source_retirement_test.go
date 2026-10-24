// Both production and staging must omit the retired repository from every
// active source operation. Historical evidence is not rewritten.
package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// The independently enumerated set prevents accidental reintroduction in either mode.
func TestSourceRepositorySetRetiresOperatorProxyInBothModes(t *testing.T) {
	want := []string{"server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "warp", "gvisor"}
	if !slices.Equal(sourceRepositoryNamesForMode(true), want) || !slices.Equal(sourceRepositoryNames(), want) {
		t.Fatal("retirement changed another required pin or left a retired production requirement")
	}
	manifest := stagingSourceTestManifest(t)
	if err := validateSourceManifest(manifest); err != nil {
		t.Fatal(err)
	}
	content, err := yaml.Marshal(manifest)
	if err != nil || strings.Contains(string(content), "operator-proxy:") {
		t.Fatalf("retired repository leaked into the new staging manifest: %v", err)
	}
	if _, err := decodeSourceManifest(content); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(content, &raw); err != nil {
		t.Fatal(err)
	}
	repositories := raw["evaluation_source"].(map[string]any)["generations"].([]any)[0].(map[string]any)["repositories"].(map[string]any)
	repositories["operator-proxy"] = map[string]any{}
	emptyRetiredPin, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSourceManifest(emptyRetiredPin); err == nil || !strings.Contains(err.Error(), "retired operator-proxy") {
		t.Fatalf("new staging accepted an empty retired repository entry: %v", err)
	}
	manifest.EvaluationSource.Generations[0].Repositories.OperatorProxy.Commit = strings.Repeat("a", 40)
	if err := validateSourceManifest(manifest); err == nil || !strings.Contains(err.Error(), "retired operator-proxy") {
		t.Fatalf("new staging accepted a retired repository pin: %v", err)
	}
	production := reviewedSourceTestManifest(t)
	if err := validateSourceManifest(production); err != nil {
		t.Fatalf("retirement broke historical production source: %v", err)
	}
}

// An empty obsolete key must not evade strict census validation in either
// supported production manifest shape, even when the typed value is zero.
func TestProductionSourceManifestRejectsRetiredPins(t *testing.T) {
	for _, schema := range []int{1, 2} {
		manifest := sourceTestManifest(sourceTestCommits(strings.Repeat("a", 40)))
		section := "epochs"
		if schema == 2 {
			manifest = reviewedSourceTestManifest(t)
			section = "generations"
		}
		content, err := yaml.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeSourceManifest(content); err != nil {
			t.Fatalf("production schema %d rejected ten complete pins: %v", schema, err)
		}
		var raw map[string]any
		if err := yaml.Unmarshal(content, &raw); err != nil {
			t.Fatal(err)
		}
		repositories := raw["evaluation_source"].(map[string]any)[section].([]any)[0].(map[string]any)["repositories"].(map[string]any)
		repositories["operator-proxy"] = map[string]any{}
		content, err = yaml.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeSourceManifest(content); err == nil || !strings.Contains(err.Error(), "retired operator-proxy") {
			t.Fatalf("production schema %d accepted a retired pin: %v", schema, err)
		}
	}
}

// The old alias command cannot interpret a new independent stage ledger as
// production history or append an empty eleventh pin to its output.
func TestLegacyStagingAliasRejectsExplicitV2Manifest(t *testing.T) {
	err := verifyRemoteSourceEpochBranchHead(stagingSourceTestManifest(t), 0, t.TempDir(), stagingEvaluationSourceBranch)
	if err == nil || !strings.Contains(err.Error(), "legacy staging source aliases") {
		t.Fatalf("legacy alias accepted an explicit v2 staging manifest: %v", err)
	}
}

// Exact lock keysets are validated against explicit mode; unchanged canonical
// fields do not permit a stage lock to masquerade as a production one.
func TestStagingEvaluatorSourceLockRequiresTenExactPins(t *testing.T) {
	commits := sourceTestCommits(strings.Repeat("a", 40))
	delete(commits, "operator-proxy")
	path := filepath.Join(t.TempDir(), "source-lock.json")
	for _, extra := range []string{"", "operator-proxy", "synthetic-extra"} {
		locked := maps.Clone(commits)
		if extra != "" {
			locked[extra] = strings.Repeat("b", 40)
		}
		content, err := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: locked})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = loadEvaluatorSourceLockForMode(path, true)
		if (err == nil) != (extra == "") {
			t.Fatalf("staging source lock extra=%q error=%v", extra, err)
		}
	}
	content, _ := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: commits})
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEvaluatorSourceLock(path); err != nil {
		t.Fatalf("production rejected the complete live graph: %v", err)
	}
}

// A genuinely required retired module still blocks the new staging release;
// repository removal is not a license to ignore measured dependencies.
func TestStagingFrozenGraphRejectsRetiredRequiredModule(t *testing.T) {
	root := t.TempDir()
	frozen := sourceTestRepository(t, root, "server")
	serverRoot := filepath.Join(root, "server")
	content := []byte("module synthetic.example/server\n\ngo 1.23.0\nrequire synthetic.example/operator-proxy v0.0.0\nreplace synthetic.example/operator-proxy => ../operator-proxy\n")
	if err := os.WriteFile(filepath.Join(serverRoot, "go.mod"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceTestGit(t, serverRoot, "add", "go.mod")
	sourceTestGit(t, serverRoot, "commit", "--quiet", "--no-gpg-sign", "-m", "synthetic retired required dependency")
	changed := sourceTestGit(t, serverRoot, "rev-parse", "HEAD")
	if err := verifyFrozenSourceGraphForMode(context.Background(), serverRoot, frozen, true); err != nil {
		t.Fatal(err)
	}
	if err := verifyFrozenSourceGraphForMode(context.Background(), serverRoot, changed, true); err == nil || !strings.Contains(err.Error(), "operator-proxy is absent") {
		t.Fatalf("staging ignored a genuinely required retired dependency: %v", err)
	}
	if err := verifyFrozenSourceGraph(context.Background(), serverRoot, changed); err == nil || !strings.Contains(err.Error(), "operator-proxy is absent") {
		t.Fatalf("production ignored a genuinely required retired dependency: %v", err)
	}
}

// Ten local-only remotes are sufficient. The Git tripwire rejects any attempt
// to discover or contact operator-proxy, without ever permitting real network.
func TestStagingRemoteVerificationNeverContactsRetiredRepository(t *testing.T) {
	verifyRemoteSourceWithoutRetiredRepository(t, true)
}

// Production must work from the same ten remotes without a retired placeholder.
func TestProductionRemoteVerificationNeverContactsRetiredRepository(t *testing.T) {
	verifyRemoteSourceWithoutRetiredRepository(t, false)
}

// The tripwire runs through actual Git source and remote verification in both modes.
func verifyRemoteSourceWithoutRetiredRepository(t *testing.T, staging bool) {
	t.Helper()
	root := t.TempDir()
	commits := map[string]string{}
	for _, repository := range sourceRepositoryNamesForMode(staging) {
		commits[repository] = sourceTestRepository(t, root, repository)
		repositoryRoot := filepath.Join(root, repository)
		sourceTestGit(t, repositoryRoot, "branch", "-m", sourceBranch(staging))
		sourceTestGit(t, repositoryRoot, "remote", "add", "origin", repositoryRoot)
	}
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	if staging {
		manifest = stagingSourceTestManifest(t)
	}
	manifest.EvaluationSource.Generations[0].Repositories = sourceRepositoriesFromCommits(commits)
	lockDigest, err := model.CompetitionSourceLockDigestForMode(commits, staging)
	if err != nil {
		t.Fatal(err)
	}
	manifest.EvaluationSource.Generations[0].SourceLockSha256 = lockDigest
	manifest.EvaluationSource.Rounds[0].SourceLockSha256 = lockDigest
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	commandRoot := filepath.Join(root, "bin")
	if err := os.Mkdir(commandRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const tripwire = `#!/usr/bin/env bash
set -Eeuo pipefail
for argument in "$@"; do
    case "$argument" in
        *operator-proxy*) printf 'retired repository contacted\n' > "$SOURCE_RETIRED_TRIPWIRE"; exit 97 ;;
    esac
done
exec "$SOURCE_RETIRED_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(commandRoot, "git"), []byte(tripwire), 0o700); err != nil {
		t.Fatal(err)
	}
	tripwirePath := filepath.Join(root, "retired-contacted")
	t.Setenv("PATH", commandRoot+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOURCE_RETIRED_REAL_GIT", gitPath)
	t.Setenv("SOURCE_RETIRED_TRIPWIRE", tripwirePath)
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	if err := verifyRemoteSourceEpoch(manifest, 0, root); err != nil {
		t.Fatal(err)
	}
	if err := verifySourceEpoch(manifest, 0, root, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tripwirePath); !os.IsNotExist(err) {
		t.Fatal("staging attempted to contact the retired repository")
	}
}
