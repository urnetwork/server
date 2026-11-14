// The evaluator takes sidecar as its eleventh repository through new producer
// releases (QD-769). Each new producer is its historical sibling with the
// longer repository list and the frozen selector that names it, and nothing
// else; the three historical producers and their preparation programs stay at
// the bytes version-one controls were made with.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

type sourceGraphRelease struct {
	name                 string
	producer             string
	producerSha256       string
	preparer             string
	preparerSha256       string
	sidecarProducer      string
	sidecarPreparer      string
	g2VolumePolicy       model.CompetitionG2VolumePolicy
	workCoverage         bool
	scorerVersion        string
	acceptsLegacyScoring bool
}

func sourceGraphReleases() []sourceGraphRelease {
	return []sourceGraphRelease{
		{name: "legacy", producer: "evaluator.sh", producerSha256: "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b",
			preparer: "prepare-round-baseline.sh", preparerSha256: "c3a7474685eb6e5aba141fa460c5ada2f5cfc8d8cbc543f2fe309720cb9e5428",
			sidecarProducer: "evaluator-sidecar.sh", sidecarPreparer: "prepare-round-baseline-sidecar.sh", scorerVersion: model.CompetitionScorerVersion},
		{name: "minimum-work", producer: "evaluator-minimum-work.sh", producerSha256: "7780930d44ebc1df5d8f775a75c37aadb2048fc1b9386027615f3ce6b5fe56d4",
			preparer: "prepare-round-baseline-minimum-work.sh", preparerSha256: "ef49fbae0d821e5a72620d18c00a1ce422891e2a3962a0a13415c3c21573df9f",
			sidecarProducer: "evaluator-minimum-work-sidecar.sh", sidecarPreparer: "prepare-round-baseline-minimum-work-sidecar.sh",
			g2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, scorerVersion: model.CompetitionMinimumWorkScorerVersion, acceptsLegacyScoring: true},
		{name: "work-coverage", producer: "evaluator-work-coverage.sh", producerSha256: "d2ad9dc8d6fcbb41f74ea5b1fb3746bb45447ffb25d4030c925516f7b46a8c72",
			preparer: "prepare-round-baseline-work-coverage.sh", preparerSha256: "9bf0bba8308f718ba225b0c50c13822113be7956bfd2d90694d075a5aa66048a",
			sidecarProducer: "evaluator-work-coverage-sidecar.sh", sidecarPreparer: "prepare-round-baseline-work-coverage-sidecar.sh",
			g2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, workCoverage: true, scorerVersion: model.CompetitionWorkCoverageScorerVersion},
	}
}

func sourceGraphContainerFile(t *testing.T, name string) (string, string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("evaluator", "container", name))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return string(content), hex.EncodeToString(digest[:])
}

// Replaces one exact occurrence, so a release that drifted from its sibling in
// any other way cannot be folded back onto the historical bytes.
func sourceGraphReplaceOnce(t *testing.T, name, content, old, replacement string) string {
	t.Helper()
	if strings.Count(content, old) != 1 {
		t.Fatalf("%s holds %q %d times", name, old, strings.Count(content, old))
	}
	return strings.Replace(content, old, replacement, 1)
}

func sourceGraphSha256(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

const (
	sourceGraphTenList    = "REPOSITORIES=(server connect sdk proxy glog goidenticons userwireguard sn warp gvisor)\n"
	sourceGraphElevenList = "REPOSITORIES=(server connect sdk proxy glog goidenticons userwireguard sn warp gvisor sidecar)\n"
	sourceGraphNote       = "# This release measures the source graph of eleven repositories, the ten with\n" +
		"# sidecar, and accepts only a request whose frozen policy names sidecar/1.\n"
	sourceGraphRequestCheck = "         (.evaluation_policy | type == \"object\" and .source_graph_policy == \"sidecar/1\") and\n"
	sourceGraphPreparerNote = "# This release prepares the control of a round whose source graph is the ten\n" +
		"# repositories with sidecar. It accepts only a request naming sidecar/1 and\n" +
		"# runs only the producer of that graph, pinned below.\n"
	sourceGraphPreparerCheck = ".evaluation_policy.source_graph_policy == \"sidecar/1\" and "
)

// Version-one controls must still authenticate, so not one byte of the three
// historical producers or of their preparation programs may move.
func TestSourceGraphHistoricalProducersStayByteForByte(t *testing.T) {
	for _, release := range sourceGraphReleases() {
		producer, producerSha256 := sourceGraphContainerFile(t, release.producer)
		if producerSha256 != release.producerSha256 {
			t.Errorf("%s changed: %s", release.producer, producerSha256)
		}
		preparer, preparerSha256 := sourceGraphContainerFile(t, release.preparer)
		if preparerSha256 != release.preparerSha256 {
			t.Errorf("%s changed: %s", release.preparer, preparerSha256)
		}
		if strings.Count(producer, "\n"+sourceGraphTenList+"readonly REPOSITORIES\n") != 1 || strings.Contains(producer, "source_graph_policy") {
			t.Errorf("%s no longer holds exactly the ten repositories", release.producer)
		}
		if strings.Count(preparer, release.producerSha256) != 1 || strings.Contains(preparer, "sidecar") {
			t.Errorf("%s no longer pins exactly its historical producer", release.preparer)
		}
	}
}

// A new producer folded back by its three additions is its historical sibling
// to the byte: the repository list, the selector's key and value, and a note.
func TestSourceGraphSidecarProducersDifferOnlyByTheGraph(t *testing.T) {
	seen := map[string]bool{}
	for _, release := range sourceGraphReleases() {
		content, digest := sourceGraphContainerFile(t, release.sidecarProducer)
		if seen[digest] || digest == release.producerSha256 {
			t.Fatalf("%s is not a release of its own", release.sidecarProducer)
		}
		seen[digest] = true
		folded := sourceGraphReplaceOnce(t, release.sidecarProducer, content, sourceGraphElevenList, sourceGraphTenList)
		folded = sourceGraphReplaceOnce(t, release.sidecarProducer, folded, sourceGraphNote, "")
		folded = sourceGraphReplaceOnce(t, release.sidecarProducer, folded, sourceGraphRequestCheck, "")
		folded = sourceGraphReplaceOnce(t, release.sidecarProducer, folded, `"site_listen","source_graph_policy","takeover_margin"`, `"site_listen","takeover_margin"`)
		if sourceGraphSha256(folded) != release.producerSha256 {
			t.Errorf("%s differs from %s by more than the source graph", release.sidecarProducer, release.producer)
		}
		info, err := os.Stat(filepath.Join("evaluator", "container", release.sidecarProducer))
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable: %v", release.sidecarProducer, err)
		}
		if output, err := exec.CommandContext(t.Context(), "bash", "-n", filepath.Join("evaluator", "container", release.sidecarProducer)).CombinedOutput(); err != nil {
			t.Errorf("%s has invalid shell syntax: %v: %s", release.sidecarProducer, err, output)
		}
	}
}

// A new preparation program is its historical sibling with the new producer's
// name and digest, the selector in its request check and in its receipt.
func TestSourceGraphSidecarPreparersPinOnlyTheirOwnProducer(t *testing.T) {
	for _, release := range sourceGraphReleases() {
		_, sidecarProducerSha256 := sourceGraphContainerFile(t, release.sidecarProducer)
		content, digest := sourceGraphContainerFile(t, release.sidecarPreparer)
		if digest == release.preparerSha256 || strings.Contains(content, release.producerSha256) || strings.Count(content, sidecarProducerSha256) != 1 {
			t.Fatalf("%s does not pin exactly %s", release.sidecarPreparer, release.sidecarProducer)
		}
		folded := sourceGraphReplaceOnce(t, release.sidecarPreparer, content, sidecarProducerSha256, release.producerSha256)
		folded = sourceGraphReplaceOnce(t, release.sidecarPreparer, folded, `"$baseline_release/`+release.sidecarProducer+`"`, `"$baseline_release/`+release.producer+`"`)
		folded = sourceGraphReplaceOnce(t, release.sidecarPreparer, folded, sourceGraphPreparerNote, "")
		folded = sourceGraphReplaceOnce(t, release.sidecarPreparer, folded, sourceGraphPreparerCheck, "")
		folded = sourceGraphReplaceOnce(t, release.sidecarPreparer, folded, `source_graph_policy:"sidecar/1",`, "")
		if release.name == "legacy" {
			folded = sourceGraphReplaceOnce(t, release.sidecarPreparer, folded,
				"# A separate worker role reuses only the exact sealed control producer of its\n# own release. Install this file beside that release's dependencies in a new\n# protected directory.\n",
				"# A separate v4 worker role reuses only the exact sealed v3 control producer.\n# Install this file with unchanged v3 dependencies in a NEW protected directory.\n")
		}
		if sourceGraphSha256(folded) != release.preparerSha256 {
			t.Errorf("%s differs from %s by more than its producer and the source graph", release.sidecarPreparer, release.preparer)
		}
		if output, err := exec.CommandContext(t.Context(), "bash", "-n", filepath.Join("evaluator", "container", release.sidecarPreparer)).CombinedOutput(); err != nil {
			t.Errorf("%s has invalid shell syntax: %v: %s", release.sidecarPreparer, err, output)
		}
		info, err := os.Stat(filepath.Join("evaluator", "container", release.sidecarPreparer))
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable: %v", release.sidecarPreparer, err)
		}
	}
}

func sourceGraphRequest(t *testing.T, release sourceGraphRelease) map[string]any {
	t.Helper()
	request := evaluatorRequestSchemaFixture()
	policy := request["evaluation_policy"].(map[string]any)
	request["scorer_version"] = release.scorerVersion
	if release.g2VolumePolicy != model.CompetitionG2VolumePolicyLegacy {
		policy["g2_volume_policy"] = string(release.g2VolumePolicy)
	}
	if release.workCoverage {
		content, err := json.Marshal(model.DefaultCompetitionWorkCoveragePolicy())
		if err != nil {
			t.Fatal(err)
		}
		var coverage map[string]any
		if err := json.Unmarshal(content, &coverage); err != nil {
			t.Fatal(err)
		}
		policy["work_coverage_policy"] = coverage
	}
	return request
}

func sourceGraphRequestAccepted(t *testing.T, script string, request map[string]any) bool {
	t.Helper()
	function := g2ScriptFunction(t, filepath.Join("evaluator", "container", script), "validate_worker_request_schema")
	content, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+function+"\nvalidate_worker_request_schema \"$1\"", "synthetic-source-graph-request", path)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	return command.Run() == nil
}

// The selector cannot be omitted, emptied or renamed toward a new producer,
// and a request that carries it never reaches a producer of the ten.
func TestSourceGraphWorkerSchemaBindsEachProducerToItsGraph(t *testing.T) {
	for _, release := range sourceGraphReleases() {
		ten := sourceGraphRequest(t, release)
		if !sourceGraphRequestAccepted(t, release.producer, ten) {
			t.Fatalf("%s refused its own historical request", release.producer)
		}
		if sourceGraphRequestAccepted(t, release.sidecarProducer, ten) {
			t.Errorf("%s accepted a request of the ten repositories", release.sidecarProducer)
		}
		eleven := sourceGraphRequest(t, release)
		eleven["evaluation_policy"].(map[string]any)["source_graph_policy"] = string(model.CompetitionSourceGraphPolicySidecar)
		if !sourceGraphRequestAccepted(t, release.sidecarProducer, eleven) {
			t.Fatalf("%s refused its own request", release.sidecarProducer)
		}
		if sourceGraphRequestAccepted(t, release.producer, eleven) {
			t.Errorf("%s accepted a request of the eleven repositories", release.producer)
		}
		for _, value := range []any{"", nil, 1, true, "sidecar", "sidecar/2", "legacy", []string{"sidecar/1"}} {
			changed := sourceGraphRequest(t, release)
			changed["evaluation_policy"].(map[string]any)["source_graph_policy"] = value
			if sourceGraphRequestAccepted(t, release.sidecarProducer, changed) {
				t.Errorf("%s accepted source graph %v", release.sidecarProducer, value)
			}
		}
		unknown := sourceGraphRequest(t, release)
		unknown["evaluation_policy"].(map[string]any)["source_graph_policy"] = string(model.CompetitionSourceGraphPolicySidecar)
		unknown["evaluation_policy"].(map[string]any)["unknown"] = 1
		if sourceGraphRequestAccepted(t, release.sidecarProducer, unknown) {
			t.Errorf("%s accepted an unknown policy field beside the selector", release.sidecarProducer)
		}
		if release.acceptsLegacyScoring {
			legacyScoring := sourceGraphRequest(t, sourceGraphReleases()[0])
			legacyScoring["evaluation_policy"].(map[string]any)["source_graph_policy"] = string(model.CompetitionSourceGraphPolicySidecar)
			if !sourceGraphRequestAccepted(t, release.sidecarProducer, legacyScoring) {
				t.Errorf("%s lost its sibling's replay of the legacy scoring contract", release.sidecarProducer)
			}
		}
		// The legacy schema leaves the scorer version to a later check of the
		// producer; the two newer contracts bind it in the schema itself.
		if release.g2VolumePolicy != model.CompetitionG2VolumePolicyLegacy {
			wrongVersion := sourceGraphRequest(t, release)
			wrongVersion["evaluation_policy"].(map[string]any)["source_graph_policy"] = string(model.CompetitionSourceGraphPolicySidecar)
			wrongVersion["scorer_version"] = "sim-latency-score/9"
			if sourceGraphRequestAccepted(t, release.sidecarProducer, wrongVersion) {
				t.Errorf("%s let the selector excuse a scorer version mismatch", release.sidecarProducer)
			}
		}
	}
}

// The private executor's helper moves with the graph: this tree's copy serves
// only rounds of the eleven, as the helpers it builds and copies with do.
func TestSourceGraphPrivateHelperRequiresTheSelector(t *testing.T) {
	release := sourceGraphReleases()[2]
	request := sourceGraphRequest(t, release)
	if sourceGraphRequestAccepted(t, "evaluator-subevaluation.sh", request) {
		t.Fatal("the private helper accepted a request of the ten repositories")
	}
	request["evaluation_policy"].(map[string]any)["source_graph_policy"] = string(model.CompetitionSourceGraphPolicySidecar)
	if !sourceGraphRequestAccepted(t, "evaluator-subevaluation.sh", request) {
		t.Fatal("the private helper refused a request of the eleven repositories")
	}
	request["evaluation_policy"].(map[string]any)["source_graph_policy"] = "sidecar/2"
	if sourceGraphRequestAccepted(t, "evaluator-subevaluation.sh", request) {
		t.Fatal("the private helper accepted an unknown source graph")
	}
}

// One set at eleven: every program and list that names the evaluated
// repositories names the Go list, which is the control plane's graph.
func TestSourceGraphEverySurfaceHoldsTheElevenRepositories(t *testing.T) {
	names := sourceRepositoryNames()
	if !slices.Equal(names, model.CompetitionSourceRepositoryNames()) || !slices.Equal(names, sourceRepositoryNamesForMode(true)) || len(names) != 11 || names[10] != "sidecar" {
		t.Fatalf("the evaluated repositories are %v", names)
	}
	graph, err := model.CompetitionSourceRepositoryNamesForGraphPolicy(model.CompetitionSourceGraphPolicySidecar)
	if err != nil || !slices.Equal(names, graph) {
		t.Fatalf("sidecar/1 names %v: %v", graph, err)
	}
	list := "\nREPOSITORIES=(" + strings.Join(names, " ") + ")\nreadonly REPOSITORIES\n"
	for _, name := range []string{"build-base.sh", "prepare-evaluation-source.sh", "build-submission.sh", "evaluator-subevaluation.sh",
		"evaluator-sidecar.sh", "evaluator-minimum-work-sidecar.sh", "evaluator-work-coverage-sidecar.sh"} {
		content, _ := sourceGraphContainerFile(t, name)
		if strings.Count(content, list) != 1 || strings.Count(content, "REPOSITORIES=(") != 1 {
			t.Errorf("%s does not hold exactly the eleven repositories", name)
		}
	}
	dockerfile, _ := sourceGraphContainerFile(t, "Dockerfile.base")
	for _, repository := range names {
		if strings.Count(dockerfile, "    && git config --system --add safe.directory /workspace/"+repository+" \\\n") != 1 {
			t.Errorf("the base image does not trust the checkout of %s", repository)
		}
	}
	if strings.Count(dockerfile, "--add safe.directory /workspace/") != len(names) {
		t.Error("the base image trusts a checkout outside the source graph")
	}
	runMain, err := os.ReadFile("run-main.sh")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(runMain, []byte("--argjson repositories '"+string(encoded)+"' '")) != 1 {
		t.Error("run-main.sh does not verify a staging source of exactly the eleven repositories")
	}
	commits := sourceRepositoriesFromCommits(map[string]string{"sidecar": strings.Repeat("b", 40)}).commits()
	if len(commits) != len(names) || commits["sidecar"] != strings.Repeat("b", 40) {
		t.Errorf("the ledger shape does not carry sidecar: %v", commits)
	}
	for _, repository := range names {
		if _, present := commits[repository]; !present {
			t.Errorf("the ledger shape omits %s", repository)
		}
		if _, err := sourceIndexRepositoryUrl(repository); err != nil {
			t.Errorf("the public source index cannot name %s: %v", repository, err)
		}
	}
}
