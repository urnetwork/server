// Instrument qualification must use the exact configured local fixture leaves.
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Compose the real request builder with the host-configuration admission gate.
func TestTrustedInstrumentBuildPreservesRelocatedLocalFixtures(t *testing.T) {
	request := sourceCoverageCorrectnessRequest(t, false)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &sourceSupervisorRuntime{repositoriesRoot: "/synthetic/operator-checkout", settings: &controller.Settings{
		ConfigLocalDirectory: "/synthetic/isolated-fixtures/config/local", VaultLocalDirectory: "/synthetic/isolated-fixtures/vault/local",
		PatchPolicy: controller.PatchPolicy{PathBase: request.Proposal.CandidatePathBase},
	}}
	current := &model.CompetitionSourceActivation{EvaluatorImageDigest: request.Proposal.EvaluatedImageDigest}
	intent := &model.CompetitionSourceActivation{TrustedInstrument: request.TrustedInstrument, SourceGeneration: request.SourceGeneration,
		SourceLockSha256: request.Proposal.SourceLockSha256, Commits: request.Proposal.Commits, WinnerReviewPolicy: request.Proposal.WinnerReviewPolicy}
	built, err := runtime.trustedInstrumentBuildRequest(current, intent, manifest, request.Proposal.StagedRoots)
	if err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{"schema": 1, "hardware_id": "synthetic-hardware", "image_digest": current.EvaluatorImageDigest,
		"qualification_sha256": strings.Repeat("b", 64), "config_local_directory": runtime.settings.ConfigLocalDirectory,
		"vault_local_directory": runtime.settings.VaultLocalDirectory, "config_local_sha256": strings.Repeat("c", 64), "vault_local_sha256": strings.Repeat("d", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prospectiveSourceHostConfig(config, built, sourceBuildArtifacts{ImageId: "sha256:" + strings.Repeat("7", 64)}); err != nil {
		t.Fatal("instrument request changed configured fixtures before host qualification", err)
	}
	if built.ConfigLocalDirectory != runtime.settings.ConfigLocalDirectory || built.VaultLocalDirectory != runtime.settings.VaultLocalDirectory {
		t.Fatal("instrument request derived fixture leaves from the operator checkout")
	}
}

// Missing configured leaves cannot silently fall back to checkout-local secrets.
func TestTrustedInstrumentBuildRefusesUnconfiguredLocalFixtures(t *testing.T) {
	request := sourceCoverageCorrectnessRequest(t, false)
	manifest, err := decodeSourceManifest(request.SourceManifest)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &sourceSupervisorRuntime{repositoriesRoot: "/synthetic/operator-checkout", settings: &controller.Settings{}}
	current := &model.CompetitionSourceActivation{EvaluatorImageDigest: request.Proposal.EvaluatedImageDigest}
	intent := &model.CompetitionSourceActivation{TrustedInstrument: request.TrustedInstrument, SourceGeneration: request.SourceGeneration,
		SourceLockSha256: request.Proposal.SourceLockSha256, Commits: request.Proposal.Commits, WinnerReviewPolicy: request.Proposal.WinnerReviewPolicy}
	if _, err := runtime.trustedInstrumentBuildRequest(current, intent, manifest, request.Proposal.StagedRoots); err == nil {
		t.Fatal("unconfigured local fixture leaves fell back to the checkout")
	}
}
