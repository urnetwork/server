// Future coverage metadata is authenticated independently of candidate code.
// These fixtures do not build, deploy, qualify or activate an evaluator image.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// Only the authenticated generation selects the new raw-lock leaf, including
// inherited product generations that have no new instrument proof themselves.
func TestSourceCoverageImageRawLockBindsImmutableGeneration(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		request := sourceCoverageCorrectnessRequest(t, inherited)
		plan, err := sourceBuildImagePlanForDigest(request, strings.Repeat("a", 64))
		if err != nil {
			t.Fatal(err)
		}
		var lock evaluatorSourceLock
		if err := json.Unmarshal(plan.SourceLock, &lock); err != nil || lock.WorkCoveragePolicy == nil || *lock.WorkCoveragePolicy != model.DefaultCompetitionWorkCoveragePolicy() {
			t.Fatal("image raw lock omitted its immutable full coverage contract", inherited, err)
		}
		var identity map[string]any
		if err := json.Unmarshal(plan.Identity, &identity); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(plan.SourceLock)
		if identity["source_lock_sha256"] != hex.EncodeToString(digest[:]) || !bytes.Contains(plan.Dockerfile, []byte(hex.EncodeToString(digest[:]))) {
			t.Fatal("source image lost the raw-lock label/identity binding")
		}
	}
}

// Appending an omitted field does not alter historical image source-lock
// bytes, pretty formatting, map order or the inherited dispatcher arguments.
func TestSourceCoverageRawLockOmissionPreservesLegacyBytes(t *testing.T) {
	commits := sourceTestCommits(strings.Repeat("a", 40))
	for _, pathBase := range []string{"", "mono_root_v1"} {
		legacy := struct {
			Schema              int               `json:"schema"`
			DevelopmentSnapshot bool              `json:"development_snapshot"`
			CandidatePathBase   string            `json:"candidate_path_base,omitempty"`
			Repositories        map[string]string `json:"repositories"`
		}{Schema: 1, CandidatePathBase: pathBase, Repositories: commits}
		expected, err := json.MarshalIndent(legacy, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := json.MarshalIndent(evaluatorSourceLock{Schema: 1, CandidatePathBase: pathBase, Repositories: commits}, "", "  ")
		if err != nil || !bytes.Equal(expected, actual) {
			t.Fatal("nil coverage changed legacy raw-lock serialization", err)
		}
	}
}

// The raw-lock codec rejects explicit null/case/duplicate leaves before a
// source check can reinterpret prospective metadata as historical omission.
func TestSourceCoverageRawLockRejectsAmbiguousLeaves(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	content, err := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: sourceTestCommits(strings.Repeat("a", 40)), WorkCoveragePolicy: &coverage})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range [][]byte{
		content,
		bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"Work_Coverage_Policy":`), 1),
		bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"work_coverage_policy":`), 1),
		bytes.Replace(content, []byte(`"work_coverage_policy":`), []byte(`"work_coverage_policy":null,"unexpected":`), 1),
	} {
		path := filepath.Join(t.TempDir(), "source-lock.json")
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadEvaluatorSourceLock(path)
		if bytes.Equal(value, content) {
			if err != nil || loaded.WorkCoveragePolicy == nil || *loaded.WorkCoveragePolicy != coverage {
				t.Fatal("exact coverage raw lock was rejected", err)
			}
		} else if err == nil {
			t.Fatal("ambiguous source coverage leaf was accepted")
		}
	}
}

// Contract mismatch fails before Git access, not through an unrelated absent
// repository. Source-check cannot bless a raw-lock coverage downgrade.
func TestSourceCoverageSourceCheckRejectsRawLockContractMismatch(t *testing.T) {
	request := sourceCoverageCorrectnessRequest(t, false)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := manifest.generation(request.SourceGeneration)
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(evaluatorSourceLock{Schema: 1, Repositories: generation.Repositories.commits()})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "source-lock.json")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifySourceEpoch(manifest, 2, t.TempDir(), path); err == nil || !strings.Contains(err.Error(), "coverage differs") {
		t.Fatal("raw-lock downgrade reached Git or passed source checking", err)
	}
}

// The real shell boundary obtains the census from a fixed trusted-tool ABI.
// The responder below is synthetic unit data, not a qualified host validator.
func TestEvaluatorCoverageProtectedPolicyRequiresEveryTrustedHelper(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	paths, _ := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	census, err := json.Marshal(map[string]any{"schema": 1, "kind": model.CompetitionWorkCoverageProtectedHelpersVersion, "paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"evaluator-work-coverage.sh", "evaluator-subevaluation.sh"} {
		function := g2ScriptFunction(t, filepath.Join("evaluator", "container", name), "authenticate_work_coverage_patch_policy")
		for missing := -1; missing < len(paths); missing++ {
			root := t.TempDir()
			tool := "#!/bin/bash\nset -Eeuo pipefail\n[ \"$*\" = '--work-coverage-protected-paths' ]\nprintf '%s\\n' '" + string(census) + "'\n"
			if err := os.WriteFile(filepath.Join(root, "competitionpatch"), []byte(tool), 0700); err != nil {
				t.Fatal(err)
			}
			forbidden := []string{"server/connect/sim-latency/**"}
			for index, path := range paths {
				if index != missing {
					forbidden = append(forbidden, "server/"+path)
				}
			}
			request, err := json.Marshal(map[string]any{
				"evaluation_policy": map[string]any{"g2_volume_policy": "minimum-work/1", "work_coverage_policy": coverage},
				"patch_policy":      map[string]any{"path_base": "mono_root_v1", "forbidden_paths": forbidden},
			})
			if err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(root, "request.json")
			if err := os.WriteFile(requestPath, request, 0600); err != nil {
				t.Fatal(err)
			}
			program := "set -Eeuo pipefail\ndie(){ exit 71; }\nsubmission_builder_identity(){ :; }\n" + function + "\nauthenticate_work_coverage_patch_policy \"$1\"\n"
			command := exec.CommandContext(t.Context(), "/bin/bash", "-c", program, "synthetic-protected-policy", requestPath)
			command.Env = []string{"PATH=/usr/bin:/bin", "SCRIPT_DIR=" + root}
			if output, err := command.CombinedOutput(); (err == nil) != (missing == -1) {
				t.Fatal("trusted shell policy disagreed with complete fixed census", name, missing, string(output), err)
			}
		}
	}
}

// Nil legacy requests never invoke the new validator command or synthesize
// its arguments. No external identity helper is needed for that old path.
func TestEvaluatorCoverageProtectedPolicyPreservesLegacyArguments(t *testing.T) {
	for _, name := range []string{"evaluator-work-coverage.sh", "evaluator-subevaluation.sh"} {
		function := g2ScriptFunction(t, filepath.Join("evaluator", "container", name), "authenticate_work_coverage_patch_policy")
		requestPath := filepath.Join(t.TempDir(), "request.json")
		if err := os.WriteFile(requestPath, []byte(`{"evaluation_policy":{}}`), 0600); err != nil {
			t.Fatal(err)
		}
		program := "set -Eeuo pipefail\ndie(){ exit 71; }\nsubmission_builder_identity(){ exit 72; }\n" + function + "\nauthenticate_work_coverage_patch_policy \"$1\"\n[ -z \"$coverage_protected_paths_json\" ]\n"
		command := exec.CommandContext(t.Context(), "/bin/bash", "-c", program, "synthetic-legacy-policy", requestPath)
		command.Env = []string{"PATH=/usr/bin:/bin"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal("legacy request acquired new tool arguments or source authority", name, string(output), err)
		}
	}
}

// Qualification-only images carry the same raw leaf without becoming a
// production image or a selected source generation.
func TestSourceCoverageTechnicalRawLockRetainsNonproductionIdentity(t *testing.T) {
	request := sourceCoverageCorrectnessRequest(t, false)
	plan, err := planG2InstrumentBaseImage(request.TrustedInstrument.Approval, strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("f", 64), strings.Repeat("1", 64))
	if err != nil {
		t.Fatal(err)
	}
	var lock evaluatorSourceLock
	var identity map[string]any
	if json.Unmarshal(plan.SourceLock, &lock) != nil || json.Unmarshal(plan.Identity, &identity) != nil ||
		!reflect.DeepEqual(lock.WorkCoveragePolicy, request.TrustedInstrument.Approval.WorkCoveragePolicy) || identity["production_eligible"] != false || identity["runtime_qualified"] != false {
		t.Fatal("technical raw policy was lost or gained production authority")
	}
}
