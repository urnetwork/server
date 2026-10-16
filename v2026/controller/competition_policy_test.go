// Pins the host-side evaluator for new rounds while preserving legacy round
// provenance and seed reveal during a rolling control-plane upgrade.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Updating either the host script bytes or the release directory must not
// change an already-created round, including after jsonb normalization.
func TestRoundPolicyPinsEvaluatorCommandIdentity(t *testing.T) {
	settings := validSettings()
	stored, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := decodeRoundPolicySnapshot(stored)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Schema != 2 || policy.EvaluatorCommand != settings.EvaluatorCommand ||
		policy.EvaluatorCommandSha256 != settings.EvaluatorCommandSha256 {
		t.Fatalf("new round omitted the host evaluator pin: %#v", policy)
	}
	var normalizedKVs map[string]any
	if err := json.Unmarshal(stored, &normalizedKVs); err != nil {
		t.Fatal(err)
	}
	stored, err = json.Marshal(normalizedKVs)
	if err != nil {
		t.Fatal(err)
	}
	if !storedPolicyMatches(settings, stored) {
		t.Fatal("unchanged normalized round policy rejected")
	}
	for _, change := range []string{"sha256", "path", "both"} {
		changed := *settings
		if change == "sha256" || change == "both" {
			changed.EvaluatorCommandSha256 = strings.Repeat("f", 64)
		}
		if change == "path" || change == "both" {
			changed.EvaluatorCommand = "/synthetic/new-release/evaluator.sh"
		}
		if storedPolicyMatches(&changed, stored) {
			t.Errorf("configured evaluator %s change accepted for frozen schema-two round", change)
		}
	}
}

func TestStagingRoundZeroMarginDoesNotChangeProductionOrHistoricalRound(t *testing.T) {
	settings := validSettings()
	stagingSettings := *settings
	stagingSettings.EvaluationPolicy.TakeoverMargin = 0
	zeroPolicy, err := policySnapshot(&stagingSettings)
	if err != nil {
		t.Fatal(err)
	}
	stagingRound := &roundRecord{RoundResult: RoundResult{Staging: true}, PolicyJson: zeroPolicy}
	frozen, err := settingsForFrozenRound(settings, stagingRound)
	if err != nil || frozen.EvaluationPolicy.TakeoverMargin != 0 || settings.EvaluationPolicy.TakeoverMargin == 0 {
		t.Fatalf("zero-margin staging policy = %+v, %v", frozen, err)
	}
	if _, err := settingsForFrozenRound(settings, &roundRecord{PolicyJson: zeroPolicy}); err == nil {
		t.Fatal("production accepted zero staging margin")
	}
	historicalPolicy, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	historical, err := settingsForFrozenRound(settings, &roundRecord{
		RoundResult: RoundResult{Staging: true}, PolicyJson: historicalPolicy,
	})
	if err != nil || historical.EvaluationPolicy.TakeoverMargin != settings.EvaluationPolicy.TakeoverMargin {
		t.Fatalf("historical staging policy = %+v, %v", historical, err)
	}
	info := settings.PublicInfo()
	if err := applyFrozenRoundPublicPolicy(&info, stagingRound); err != nil || info.EvaluationPolicy.TakeoverMargin != 0 {
		t.Fatalf("public staging policy = %+v, %v", info.EvaluationPolicy, err)
	}
	info = settings.PublicInfo()
	if err := applyFrozenRoundPublicPolicy(&info, &roundRecord{PolicyJson: historicalPolicy}); err != nil ||
		info.EvaluationPolicy.TakeoverMargin != settings.EvaluationPolicy.TakeoverMargin {
		t.Fatalf("public historical policy = %+v, %v", info.EvaluationPolicy, err)
	}
}

func TestScoreMarginMustMatchFrozenStagingOrProductionPolicy(t *testing.T) {
	score := &ScoreResult{Significance: &ScoreSignificance{TakeoverMarginPercent: 0}}
	if !scoreMatchesFrozenMargin(score, 0) || scoreMatchesFrozenMargin(score, 0.161) {
		t.Fatal("zero staging margin crossed production policy")
	}
	score.Significance.TakeoverMarginPercent = 16.1
	if !scoreMatchesFrozenMargin(score, 0.161) || scoreMatchesFrozenMargin(score, 0) {
		t.Fatal("positive production margin crossed staging policy")
	}
}

// A valid newly configured executable must still be refused before an attempt
// starts if its path or bytes differ from the round's frozen host command.
func TestCommandEvaluatorRejectsRoundScriptReleaseChange(t *testing.T) {
	for _, change := range []string{"sha256", "path"} {
		root := t.TempDir()
		settings := validSettings()
		settings.EvaluatorCommand = filepath.Join(root, "evaluator.sh")
		content := []byte("#!/bin/sh\nexit 99\n")
		if err := os.WriteFile(settings.EvaluatorCommand, content, 0o700); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		settings.EvaluatorCommandSha256 = hex.EncodeToString(digest[:])
		policy, err := policySnapshot(settings)
		if err != nil {
			t.Fatal(err)
		}
		if change == "sha256" {
			content = []byte("#!/bin/sh\nexit 98\n")
			digest = sha256.Sum256(content)
			settings.EvaluatorCommandSha256 = hex.EncodeToString(digest[:])
		} else {
			settings.EvaluatorCommand = filepath.Join(root, "another-evaluator.sh")
		}
		if err := os.WriteFile(settings.EvaluatorCommand, content, 0o700); err != nil {
			t.Fatal(err)
		}
		outcome := (CommandEvaluator{}).Evaluate(t.Context(), settings, &queuedJob{
			Round: roundRecord{PolicyJson: policy},
		})
		if outcome.Error == nil || outcome.Error.Kind != "infrastructure" || outcome.Error.Code != "round_policy_mismatch" {
			t.Errorf("%s change passed evaluator policy gate: %#v", change, outcome.Error)
		}
	}
}

// Legacy snapshots never included a host-script pin. Keep their evaluation
// behavior explicit, while still enforcing every identity they did freeze.
func TestRoundPolicyPreservesLegacyEvaluationCompatibility(t *testing.T) {
	settings := validSettings()
	stored, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	var legacyKVs map[string]any
	if err := json.Unmarshal(stored, &legacyKVs); err != nil {
		t.Fatal(err)
	}
	legacyKVs["schema"] = 1
	delete(legacyKVs, "evaluator_command")
	delete(legacyKVs, "evaluator_command_sha256")
	stored, err = json.Marshal(legacyKVs)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := decodeRoundPolicySnapshot(stored)
	if err != nil || policy.EvaluatorCommand != "" || policy.EvaluatorCommandSha256 != "" {
		t.Fatalf("legacy snapshot acquired an invented evaluator pin: %#v, %v", policy, err)
	}
	changed := *settings
	changed.EvaluatorCommand = "/synthetic/new-release/evaluator.sh"
	changed.EvaluatorCommandSha256 = strings.Repeat("f", 64)
	if !storedPolicyMatches(&changed, stored) {
		t.Fatal("legacy snapshot cannot finish evaluation after control-plane upgrade")
	}
	changed.EvaluatorImageDigest = "sha256:" + strings.Repeat("c", 64)
	if storedPolicyMatches(&changed, stored) {
		t.Fatal("legacy compatibility relaxed the existing frozen image gate")
	}
}

// Schema downgrade and incomplete host pins are malformed, not a request to
// silently skip the new gate. Unknown fields must not bypass semantic matching.
func TestRoundPolicyRejectsInvalidEvaluatorPins(t *testing.T) {
	settings := validSettings()
	stored, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name  string
		field string
		value any
	}{
		{name: "downgrade with pin", field: "schema", value: 1},
		{name: "unknown schema", field: "schema", value: 3},
		{name: "missing command", field: "evaluator_command", value: nil},
		{name: "relative command", field: "evaluator_command", value: "evaluator.sh"},
		{name: "root command", field: "evaluator_command", value: "/"},
		{name: "missing digest", field: "evaluator_command_sha256", value: nil},
		{name: "invalid digest", field: "evaluator_command_sha256", value: "not-a-digest"},
		{name: "unknown field", field: "unrecognized_policy_field", value: true},
	} {
		var mutatedKVs map[string]any
		if err := json.Unmarshal(stored, &mutatedKVs); err != nil {
			t.Fatal(err)
		}
		if fixture.value == nil {
			delete(mutatedKVs, fixture.field)
		} else {
			mutatedKVs[fixture.field] = fixture.value
		}
		encoded, err := json.Marshal(mutatedKVs)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeRoundPolicySnapshot(encoded); err == nil {
			t.Errorf("%s: malformed policy decoded", fixture.name)
		}
		if storedPolicyMatches(settings, encoded) {
			t.Errorf("%s: malformed policy matched evaluator", fixture.name)
		}
	}
}

// Reading an old round must not depend on whether current main has advanced
// either the image or host-script release, or rewrite its retained evidence.
func TestLegacyRoundCommitmentSurvivesEvaluatorReleaseChange(t *testing.T) {
	settings := validSettings()
	round, _ := sealedTestRound(t, settings)
	var legacyKVs map[string]any
	if err := json.Unmarshal(round.PolicyJson, &legacyKVs); err != nil {
		t.Fatal(err)
	}
	legacyKVs["schema"] = 1
	delete(legacyKVs, "evaluator_command")
	delete(legacyKVs, "evaluator_command_sha256")
	stored, err := json.Marshal(legacyKVs)
	if err != nil {
		t.Fatal(err)
	}
	round.PolicyJson = stored
	round.SeedNonce, round.SeedCiphertext, round.WorkloadCommitment, err = createRoundSecret(settings, round)
	if err != nil {
		t.Fatal(err)
	}
	wantSeed, err := revealRoundSecret(settings, round)
	if err != nil {
		t.Fatal(err)
	}
	settings.EvaluatorCommand = "/synthetic/new-release/evaluator.sh"
	settings.EvaluatorCommandSha256 = strings.Repeat("f", 64)
	settings.EvaluatorImageDigest = "sha256:" + strings.Repeat("c", 64)
	settings.BaseSha = strings.Repeat("b", 40)
	seed, err := revealRoundSecret(settings, round)
	if err != nil || seed != wantSeed || string(round.PolicyJson) != string(stored) {
		t.Fatalf("legacy reveal after evaluator release changed = %q, %v", seed, err)
	}
}
