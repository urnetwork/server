// Prospective helper protection is derived and frozen without changing the
// current round, product implementation surface or historical policy bytes.
package controller

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// Future derivation owns its slice and is idempotent; merely configuring the
// future instrument cannot change epoch one's serialized policy or paths.
func TestCompetitionWorkCoverageHelperDerivationPreservesCurrentPolicy(t *testing.T) {
	settings := validSettings()
	before, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	original := append([]string{}, settings.PatchPolicy.ForbiddenPaths...)
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	current, err := settings.ForCompetitionEpoch(1)
	if err != nil {
		t.Fatal(err)
	}
	after, err := policySnapshot(current)
	if err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(current.PatchPolicy.ForbiddenPaths, original) {
		t.Fatal("staged helper protection changed current policy bytes or paths", err)
	}
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil || model.ValidateCompetitionWorkCoveragePatchPolicy(future.PatchPolicy, future.EvaluationPolicy.WorkCoveragePolicy) != nil {
		t.Fatal("future policy lost fixed helper protection", err)
	}
	second, err := future.ForCompetitionEpoch(2)
	if err != nil || !reflect.DeepEqual(second.PatchPolicy, future.PatchPolicy) {
		t.Fatal("repeated future derivation changed the frozen list", err)
	}
	future.PatchPolicy.ForbiddenPaths[0] = "synthetic/caller-mutation"
	if !reflect.DeepEqual(settings.PatchPolicy.ForbiddenPaths, original) {
		t.Fatal("future policy list shared current mutable settings")
	}
}

// Decoding and freezing enforce the complete policy independently, so a later
// mutable settings change cannot substitute a weaker public/submission view.
func TestCompetitionWorkCoverageHelperPolicyIsFrozenAndCannotDowngrade(t *testing.T) {
	settings := validSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	content, err := policySnapshot(future)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := decodeRoundPolicySnapshot(content)
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := settingsForFrozenRound(future, &roundRecord{RoundResult: RoundResult{Epoch: 2}, PolicyJson: content})
	if err != nil || !reflect.DeepEqual(frozen.PatchPolicy, stored.PatchPolicy) {
		t.Fatal("matching settings lost the frozen helper policy", err)
	}
	future.PatchPolicy.ForbiddenPaths = []string{protectedSimulatorTreePattern}
	if _, err := settingsForFrozenRound(future, &roundRecord{RoundResult: RoundResult{Epoch: 2}, PolicyJson: content}); err == nil {
		t.Fatal("moving weaker settings were accepted for a frozen coverage round")
	}
	view := &InfoResult{CompetitionId: stored.CompetitionId}
	if err := applyFrozenRoundPublicPolicy(view, &roundRecord{RoundResult: RoundResult{Epoch: 2}, PolicyJson: content}); err != nil || !reflect.DeepEqual(view.PatchPolicy, stored.PatchPolicy) {
		t.Fatal("public view substituted moving restrictions for the frozen helper policy", err)
	}
	helpers, err := model.CompetitionWorkCoverageProtectedHelperPaths(stored.EvaluationPolicy.WorkCoveragePolicy)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range helpers {
		if stored.PatchPolicy.PathBase == competitionMonoRootPathBase {
			path = "server/" + path
		}
		index := slices.Index(stored.PatchPolicy.ForbiddenPaths, path)
		if index < 0 {
			t.Fatal("frozen fixture omitted a fixed helper", path)
		}
		changed := *frozen
		changed.PatchPolicy.ForbiddenPaths = append([]string{}, stored.PatchPolicy.ForbiddenPaths[:index]...)
		changed.PatchPolicy.ForbiddenPaths = append(changed.PatchPolicy.ForbiddenPaths, stored.PatchPolicy.ForbiddenPaths[index+1:]...)
		if _, err := policySnapshot(&changed); err == nil {
			t.Fatal("freezer accepted a missing helper prohibition", path)
		}
		changedStored := *stored
		changedStored.PatchPolicy = changed.PatchPolicy
		changedBytes, err := json.Marshal(changedStored)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeRoundPolicySnapshot(changedBytes); err == nil {
			t.Fatal("frozen decoder accepted a missing helper prohibition", path)
		}
	}
}

// Parsing is restrictive only: a complete explicit v1 list denies aliases,
// while mixed product model/controller code remains a legitimate submission.
func TestCompetitionWorkCoverageHelperParserRejectsAliasesNotProductCode(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	for _, pathBase := range []string{"", competitionMonoRootPathBase} {
		prefix := ""
		allowed := []string{"model/competition_model.go", "controller/competition_controller.go"}
		if pathBase != "" {
			prefix, allowed = "server/", []string{"server/**"}
		}
		policy, err := competitionWorkCoveragePatchPolicy(PatchPolicy{PathBase: pathBase, MaxPatchBytes: 4096,
			AllowedPaths: allowed, ForbiddenPaths: []string{prefix + protectedSimulatorTreePattern}}, &coverage)
		if err != nil {
			t.Fatal(err)
		}
		patch := func(path string) string {
			return "diff --git a/" + path + " b/" + path + "\n--- a/" + path + "\n+++ b/" + path + "\n@@ -1 +1 @@\n-before\n+after\n"
		}
		paths, _ := model.CompetitionWorkCoverageProtectedHelperPaths(&coverage)
		for _, path := range paths {
			for _, variant := range []string{prefix + path, prefix + strings.Replace(path, "competition_", "Competition_", 1)} {
				if _, err := ValidateAndCanonicalizePatch(patch(variant), policy); err == nil || err.Code != "path_not_allowed" {
					t.Fatal("protected helper alias was admitted", pathBase, variant, err)
				}
			}
		}
		for _, path := range []string{"model/competition_model.go", "controller/competition_controller.go"} {
			if _, err := ValidateAndCanonicalizePatch(patch(prefix+path), policy); err != nil {
				t.Fatal("dedicated helper protection froze mixed product code", pathBase, err)
			}
		}
	}
}
