package main

// Actual shell predicates are exercised with private synthetic metadata and a
// fixed fake capability responder. No Docker, services or workload is started.

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

	"github.com/urnetwork/server/model"
)

// New metadata cannot downgrade the scorer or silently drop its work policy.
func TestEvaluatorWorkCoverageWorkerSchemaBindsExactVersionedShape(t *testing.T) {
	for _, script := range []string{"evaluator-work-coverage.sh", "evaluator-subevaluation.sh"} {
		function := g2ScriptFunction(t, filepath.Join("evaluator", "container", script), "validate_worker_request_schema")
		for _, test := range []struct {
			mutate func(map[string]any)
			valid  bool
		}{
			{valid: true},
			{mutate: func(request map[string]any) { request["scorer_version"] = model.CompetitionMinimumWorkScorerVersion }},
			{mutate: func(request map[string]any) {
				delete(request["evaluation_policy"].(map[string]any), "work_coverage_policy")
			}},
			{mutate: func(request map[string]any) {
				delete(request["evaluation_policy"].(map[string]any), "g2_volume_policy")
			}},
			{mutate: func(request map[string]any) {
				request["evaluation_policy"].(map[string]any)["work_coverage_policy"] = nil
			}},
			{mutate: func(request map[string]any) {
				request["evaluation_policy"].(map[string]any)["work_coverage_policy"].(map[string]any)["unknown"] = true
			}},
			{mutate: func(request map[string]any) {
				request["evaluation_policy"].(map[string]any)["work_coverage_policy"].(map[string]any)["kind"] = "unknown/1"
			}},
			{mutate: func(request map[string]any) {
				request["staging"] = true
				request["evaluation_policy"].(map[string]any)["takeover_margin"] = 0
			}},
		} {
			request := evaluatorRequestSchemaFixture()
			policyBytes, err := json.Marshal(model.DefaultCompetitionWorkCoveragePolicy())
			if err != nil {
				t.Fatal(err)
			}
			var policy map[string]any
			if err := json.Unmarshal(policyBytes, &policy); err != nil {
				t.Fatal(err)
			}
			request["scorer_version"] = model.CompetitionWorkCoverageScorerVersion
			request["evaluation_policy"].(map[string]any)["g2_volume_policy"] = string(model.CompetitionG2VolumePolicyMinimumWork)
			request["evaluation_policy"].(map[string]any)["work_coverage_policy"] = policy
			if test.mutate != nil {
				test.mutate(request)
			}
			content, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "request.json")
			if err := os.WriteFile(path, content, 0o600); err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), "/bin/bash", "-c", "set -Eeuo pipefail\n"+function+"\nvalidate_worker_request_schema \"$1\"", "synthetic-coverage-request", path)
			command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
			if output, err := command.CombinedOutput(); (err == nil) != test.valid {
				t.Fatal("versioned worker schema disagreed with its exact policy envelope", script, test.valid, string(output), err)
			}
		}
	}
}

// Full policy equality, not a kind-only capability, authenticates tolerance.
func TestOfficialRunWorkCoverageRequiresFullCompiledCapability(t *testing.T) {
	policy := model.DefaultCompetitionWorkCoveragePolicy()
	capability, err := json.Marshal(map[string]any{"score_schema": 1, "scorer_version": model.CompetitionWorkCoverageScorerVersion, "work_coverage_policy": policy})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fake := filepath.Join(root, "synthetic-scorer")
	script := "#!/bin/bash\nset -Eeuo pipefail\n[ \"$*\" = 'score-policy --g2-volume-policy minimum-work/1 --work-coverage-policy seeded-crawl-coverage/v1' ]\nprintf '%s\\n' '" + string(capability) + "'\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	function := g2ScriptFunction(t, "official-run.sh", "coverage_policy_arguments")
	for _, test := range []struct {
		policy *model.CompetitionWorkCoveragePolicy
		valid  bool
	}{
		{policy: &policy, valid: true},
		{valid: true},
		{policy: &model.CompetitionWorkCoveragePolicy{Kind: model.CompetitionWorkCoverageKind, MinimumPageCoverageRatio: .5, MinimumByteCoverageRatio: .5}},
		{policy: &model.CompetitionWorkCoveragePolicy{Kind: "unknown/1"}},
	} {
		content, err := json.Marshal(test.policy)
		if err != nil {
			t.Fatal(err)
		}
		body := "set -Eeuo pipefail\ndie(){ exit 71; }\n" + function + "\ncoverage_policy_arguments\nif [ \"$APEX_WORK_COVERAGE_POLICY_JSON\" = null ]; then [ \"${#coverage_args[@]}\" = 0 ]; else [ \"${coverage_args[*]}\" = '--work-coverage-policy seeded-crawl-coverage/v1' ]; fi\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", body)
		command.Env = []string{"PATH=/usr/bin:/bin", "APEX_SCORER_BIN=" + fake, "APEX_G2_VOLUME_POLICY=minimum-work/1", "APEX_WORK_COVERAGE_POLICY_JSON=" + string(content)}
		if output, err := command.CombinedOutput(); (err == nil) != test.valid {
			t.Fatal("compiled coverage capability was omitted or substituted", test.valid, string(output), err)
		}
	}
}

// Separate producer identities protect the byte-exact historical release.
func TestEvaluatorWorkCoverageUsesNewProducerAndLeavesHistoricalBytes(t *testing.T) {
	legacy, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-minimum-work.sh"))
	if err != nil {
		t.Fatal(err)
	}
	legacyHash := sha256.Sum256(legacy)
	if hex.EncodeToString(legacyHash[:]) != "7780930d44ebc1df5d8f775a75c37aadb2048fc1b9386027615f3ce6b5fe56d4" {
		t.Fatal("historical minimum-work producer bytes changed")
	}
	future, err := os.ReadFile(filepath.Join("evaluator", "container", "evaluator-work-coverage.sh"))
	if err != nil {
		t.Fatal(err)
	}
	preparer, err := os.ReadFile(filepath.Join("evaluator", "container", "prepare-round-baseline-work-coverage.sh"))
	if err != nil {
		t.Fatal(err)
	}
	futureHash := sha256.Sum256(future)
	if !bytes.Contains(preparer, []byte(hex.EncodeToString(futureHash[:]))) || bytes.Contains(preparer, []byte(hex.EncodeToString(legacyHash[:]))) ||
		!strings.Contains(string(preparer), "evaluator-work-coverage.sh") || !bytes.Contains(future, []byte("APEX_WORK_COVERAGE_POLICY_JSON=")) {
		t.Fatal("coverage producer lost its separately pinned protocol or policy")
	}
}
