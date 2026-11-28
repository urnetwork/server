// These exact shell fragments consume synthetic raw locks only. They do not
// invoke Docker, privileged preparation, a workload, or a readiness endpoint.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Marker selection retains production jq programs instead of copying their
// validation or identity rules into a synthetic shell implementation.
func sourceCoveragePreparerFragment(t *testing.T, begin string, end string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("evaluator", "container", "prepare-evaluation-source.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(content), begin)
	if start < 0 {
		t.Fatal("source preparer fragment start is missing")
	}
	finish := strings.Index(string(content[start:]), end)
	if finish < 0 {
		t.Fatal("source preparer fragment end is missing")
	}
	return string(content[start : start+finish])
}

// Actual raw-lock validation and prepared-identity serialization agree on
// omission versus the complete future policy, without introducing new argv.
func TestSourceCoveragePreparerRetainsExactRawPolicyAndLegacyOmission(t *testing.T) {
	validate := sourceCoveragePreparerFragment(t, "jq -e --argjson repositories \"$repository_names_json\" '", "\n\nfor repository in")
	serialize := sourceCoveragePreparerFragment(t, "jq -nS \\\n", "\nchmod 0400 \"$identity_path\"")
	repositories := sourceTestCommits(strings.Repeat("a", 40))
	repositoryBytes, err := json.Marshal(repositories)
	if err != nil {
		t.Fatal(err)
	}
	names, err := json.Marshal(sourceRepositoryNames())
	if err != nil {
		t.Fatal(err)
	}
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	for _, covered := range []bool{false, true} {
		lock := evaluatorSourceLock{Schema: 1, CandidatePathBase: "mono_root_v1", Repositories: repositories}
		if covered {
			lock.WorkCoveragePolicy = coverage.Clone()
		}
		content, err := json.Marshal(lock)
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		path, identityPath := filepath.Join(root, "source-lock.json"), filepath.Join(root, "identity.json")
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		program := "set -Eeuo pipefail\n" + validate + "\n" + serialize + "\ncat -- \"$identity_path\"\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", program)
		command.Env = []string{"PATH=/usr/bin:/bin", "source_lock_path=" + path, "identity_path=" + identityPath,
			"repository_names_json=" + string(names), "source_repositories=" + string(repositoryBytes),
			"candidate_path_base=mono_root_v1", "base_image_id=sha256:" + strings.Repeat("b", 64),
			"base_sha=" + repositories["server"], "source_epoch=2", "staging=false", "source_branch=sim-latency",
			"source_lock_sha256=" + hex.EncodeToString(digest[:])}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatal("raw source preparation rejected its exact prospective policy", covered, string(output), err)
		}
		var identity map[string]json.RawMessage
		if err := json.Unmarshal(output, &identity); err != nil {
			t.Fatal(err)
		}
		value, present := identity["work_coverage_policy"]
		if present != covered {
			t.Fatal("preparer changed legacy omission or dropped the future leaf", covered)
		}
		if covered {
			var actual model.CompetitionWorkCoveragePolicy
			if json.Unmarshal(value, &actual) != nil || actual != coverage {
				t.Fatal("preparer changed the exact raw coverage policy")
			}
		} else {
			legacy := map[string]any{"schema": 1, "kind": "sim-latency-evaluation-source", "temporary": true,
				"base_image_id": "sha256:" + strings.Repeat("b", 64), "base_sha": repositories["server"], "source_epoch": 2,
				"staging": false, "branch": "sim-latency", "source_lock_sha256": hex.EncodeToString(digest[:]),
				"repositories": repositories, "candidate_patch_sha256": nil, "candidate_path_base": "mono_root_v1"}
			expected, err := json.MarshalIndent(legacy, "", "  ")
			if err != nil || !bytes.Equal(output, append(expected, '\n')) {
				t.Fatal("nil coverage changed complete sorted legacy preparer identity bytes", err)
			}
		}
		var actualDigest string
		if json.Unmarshal(identity["source_lock_sha256"], &actualDigest) != nil || actualDigest != hex.EncodeToString(digest[:]) {
			t.Fatal("prepared identity no longer binds actual raw lock bytes")
		}
	}
}

// jq is not a duplicate-key authenticator; the native CLI tests own that
// boundary. This fragment must still reject null, case aliases and bad values.
func TestSourceCoveragePreparerRejectsMalformedFutureLeaf(t *testing.T) {
	fragment := sourceCoveragePreparerFragment(t, "jq -e --argjson repositories \"$repository_names_json\" '", "\n\nfor repository in")
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	content, err := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: sourceTestCommits(strings.Repeat("a", 40)), WorkCoveragePolicy: &coverage})
	if err != nil {
		t.Fatal(err)
	}
	names, err := json.Marshal(sourceRepositoryNames())
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{
		bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"Work_Coverage_Policy":`), 1),
		bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"unexpected":`), 1),
		bytes.Replace(content, []byte("seeded-crawl-coverage/v1"), []byte("synthetic-unknown/v1"), 1),
		bytes.Replace(content, []byte("0.95"), []byte("0.5"), 1),
	} {
		path := filepath.Join(t.TempDir(), "source-lock.json")
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+fragment+"\n")
		command.Env = []string{"PATH=/usr/bin:/bin", "source_lock_path=" + path, "repository_names_json=" + string(names)}
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatal("preparer serialized malformed prospective metadata", string(output))
		}
	}
}

// The baseline-only producer pins the exact updated coverage script. A stale
// release, a changed file or a private candidate lifecycle cannot replace it.
func TestEvaluatorCoverageBaselinePrefixPinsUpdatedProtectedProducer(t *testing.T) {
	path := filepath.Join("evaluator", "container", "evaluator-work-coverage.sh")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != "5d2d9bef902944eb6db166628dd8bc8adc57ce668f118efd493319af63aa8802" {
		t.Fatal("future baseline producer requires a separately reviewed exact script pin")
	}
	function := g2ScriptFunction(t, filepath.Join("evaluator", "container", "prepare-round-baseline-work-coverage.sh"), "round_baseline_control_prefix")
	run := func(function string, path string) ([]byte, error) {
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+function+"\nround_baseline_control_prefix \"$1\"\n", "synthetic-coverage-prefix", path)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		return command.CombinedOutput()
	}
	prefix, err := run(function, path)
	if err != nil || bytes.Contains(prefix, []byte("\nbuild_candidate\n")) || !bytes.Contains(prefix, []byte("authenticate_work_coverage_patch_policy")) {
		t.Fatal("updated protected baseline prefix lost authentication or gained candidate authority", err)
	}
	stale := strings.Replace(function, hex.EncodeToString(digest[:]), "d2ad9dc8d6fcbb41f74ea5b1fb3746bb45447ffb25d4030c925516f7b46a8c72", 1)
	if stale == function {
		t.Fatal("baseline prefix did not contain its independently checked exact script pin")
	}
	if _, err := run(stale, path); err == nil {
		t.Fatal("stale baseline pin accepted changed producer bytes")
	}
	changed := filepath.Join(t.TempDir(), "changed-producer.sh")
	if err := os.WriteFile(changed, append(content, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := run(function, changed); err == nil {
		t.Fatal("baseline prefix accepted changed unqualified producer bytes")
	}
	if _, err := run(function, filepath.Join("evaluator", "container", "evaluator-subevaluation.sh")); err == nil {
		t.Fatal("private candidate lifecycle replaced baseline-only source")
	}
}
