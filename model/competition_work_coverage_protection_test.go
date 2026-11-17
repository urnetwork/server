// Coverage protection has a fixed future-only census and strict source leaf.
// These scalar fixtures do not select a product or authorize an instrument.
package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A caller cannot mutate the next caller's paths or extend the mixed product
// model merely because the standalone coverage scorer uses its wire types.
func TestCompetitionWorkCoverageProtectedHelperCensusIsFixedAndOwned(t *testing.T) {
	coverage := DefaultCompetitionWorkCoveragePolicy()
	expected := []string{
		"controller/competition_trusted_instrument_approval.go",
		"model/competition_g2_codec_test.go",
		"model/competition_g2_policy.go",
		"model/competition_trusted_instrument.go",
		"model/competition_work_coverage_policy.go",
		"model/competition_work_coverage_policy_test.go",
	}
	paths, err := CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err != nil || !reflect.DeepEqual(paths, expected) || CompetitionWorkCoverageProtectedHelpersVersion != "seeded-crawl-coverage-protected-helpers/v1" {
		t.Fatal("coverage protection changed its versioned literal census", err)
	}
	paths[0] = "model/synthetic_product.go"
	second, err := CompetitionWorkCoverageProtectedHelperPaths(&coverage)
	if err != nil || !reflect.DeepEqual(second, expected) {
		t.Fatal("returned helper list shared mutable policy state", err)
	}
	if legacy, err := CompetitionWorkCoverageProtectedHelperPaths(nil); err != nil || legacy != nil {
		t.Fatal("historical omission acquired protected helpers", err)
	}
}

// Both path conventions require every canonical leaf, not a glob, partial list
// or the single leaf protected before the complete census was introduced.
func TestCompetitionWorkCoveragePatchPolicyRequiresEveryFixedHelper(t *testing.T) {
	coverage := DefaultCompetitionWorkCoveragePolicy()
	for _, pathBase := range []string{"", "mono_root_v1"} {
		prefix := ""
		if pathBase != "" {
			prefix = "server/"
		}
		paths, err := CompetitionWorkCoverageProtectedHelperPaths(&coverage)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, "connect/sim-latency/**")
		policy := CompetitionPatchPolicy{PathBase: pathBase, MaxPatchBytes: 4096, AllowedPaths: []string{prefix + "model/product.go"}}
		for _, path := range paths {
			policy.ForbiddenPaths = append(policy.ForbiddenPaths, prefix+path)
		}
		if err := ValidateCompetitionWorkCoveragePatchPolicy(policy, &coverage); err != nil {
			t.Fatal("complete coverage prohibition was rejected", pathBase, err)
		}
		for index := range policy.ForbiddenPaths {
			changed := policy
			changed.ForbiddenPaths = append([]string{}, policy.ForbiddenPaths[:index]...)
			changed.ForbiddenPaths = append(changed.ForbiddenPaths, policy.ForbiddenPaths[index+1:]...)
			if err := ValidateCompetitionWorkCoveragePatchPolicy(changed, &coverage); err == nil {
				t.Fatal("coverage accepted a missing fixed prohibition", pathBase, paths[index])
			}
		}
	}
}

// No field is added to the historical policy, and nil coverage does not
// reinterpret even an arbitrary legacy caller's explicit prohibition list.
func TestCompetitionWorkCoverageProtectionPreservesLegacyPolicyBytes(t *testing.T) {
	policy := CompetitionPatchPolicy{MaxPatchBytes: 4096, AllowedPaths: []string{"model/product.go"}, ForbiddenPaths: []string{"connect/sim-latency/**"}}
	before, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCompetitionWorkCoveragePatchPolicy(policy, nil); err != nil {
		t.Fatal("historical policy acquired a new protection requirement", err)
	}
	after, err := json.Marshal(policy)
	if err != nil || string(before) != string(after) || string(after) != `{"max_patch_bytes":4096,"allowed_paths":["model/product.go"],"forbidden_paths":["connect/sim-latency/**"]}` {
		t.Fatal("legacy policy serialization changed", err)
	}
}

// Missing source metadata stays missing; an explicit prospective object can
// never become historical nil through null, aliases or duplicate JSON keys.
func TestCompetitionWorkCoverageSourceLeafRejectsAmbiguousShapes(t *testing.T) {
	coverage := DefaultCompetitionWorkCoveragePolicy()
	value, err := json.Marshal(coverage)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"schema":1,"work_coverage_policy":` + string(value) + `}`
	decoded, err := CompetitionWorkCoveragePolicyFromSourceJson([]byte(valid))
	if err != nil || decoded == nil || *decoded != coverage {
		t.Fatal("exact source coverage policy was rejected", err)
	}
	if legacy, err := CompetitionWorkCoveragePolicyFromSourceJson([]byte(`{"schema":1}`)); err != nil || legacy != nil {
		t.Fatal("source omission acquired coverage authority", err)
	}
	for _, content := range []string{
		`{"work_coverage_policy":null}`,
		strings.Replace(valid, "work_coverage_policy", "Work_Coverage_Policy", 1),
		`{"work_coverage_policy":` + string(value) + `,"work_coverage_policy":null}`,
		strings.Replace(valid, "seeded-crawl-coverage/v1", "synthetic-unknown/v1", 1),
		strings.Replace(valid, `"kind":`, `"unexpected":true,"kind":`, 1),
		valid + `{}`,
	} {
		if _, err := CompetitionWorkCoveragePolicyFromSourceJson([]byte(content)); err == nil {
			t.Fatal("source policy ambiguity was accepted", content)
		}
	}
}
