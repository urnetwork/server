// Explicit producer selection must not silently rewrite historical authority.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/urnetwork/server/model"
)

// The actual tracked fixture-corrected producer differs from the historical
// release. Selection must authenticate those bytes without changing defaults.
func TestRoundBaselineExplicitPrivateProbeReleaseMatchesTrackedProducer(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	config := roundBaselinePreparationConfig{ReleaseKind: roundBaselinePrivateProbeRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, WorkCoveragePolicy: &coverage}
	dependencies, program, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || program != "prepare-round-baseline-work-coverage.sh" {
		t.Fatal("explicit producer refused", err)
	}
	content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", "evaluator-work-coverage.sh"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	actual := hex.EncodeToString(digest[:])
	if dependencies["evaluator-work-coverage.sh"] != actual {
		t.Fatal("new release does not authenticate the actual tracked control producer")
	}
	for name, expected := range dependencies {
		// The two compiled patch-helper files have separately sealed build
		// identities; every textual dependency is checked against its source.
		if name == "competitionpatch" || name == "competitionpatch.sha256" {
			continue
		}
		asset, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", name))
		if err != nil {
			t.Fatal(name, err)
		}
		hash := sha256.Sum256(asset)
		if hex.EncodeToString(hash[:]) != expected {
			t.Fatal("new producer release dependency differs from tracked source", name)
		}
	}
	config.ReleaseKind = ""
	historical, _, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || !reflect.DeepEqual(historical, roundBaselineWorkCoverageDependencyDigests()) || historical["evaluator-work-coverage.sh"] == actual {
		t.Fatal("historical release authority changed", err)
	}
	dependencies["evaluator-work-coverage.sh"] = "changed"
	config.ReleaseKind = roundBaselinePrivateProbeRelease
	fresh, _, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || fresh["evaluator-work-coverage.sh"] != actual {
		t.Fatal("one caller mutated another release inventory", err)
	}
}

// A private probe correction cannot reinterpret already sealed release bytes.
func TestRoundBaselinePrivateProbeReleasePreservesImageBranchInventory(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	config := roundBaselinePreparationConfig{ReleaseKind: roundBaselineImageBranchesRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, WorkCoveragePolicy: &coverage}
	previous, _, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || previous["docker-id-map.sh"] != "ecf319fccdd00a2ab76871ec0a29693abca83e3a99ee1ee5c508bc1b076d96a0" {
		t.Fatal("previous probe authority changed", err)
	}
	config.ReleaseKind = roundBaselinePrivateProbeRelease
	current, _, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || current["docker-id-map.sh"] == previous["docker-id-map.sh"] {
		t.Fatal("private probe correction not explicitly selected", err)
	}
	delete(previous, "docker-id-map.sh")
	delete(current, "docker-id-map.sh")
	if !reflect.DeepEqual(previous, current) {
		t.Fatal("private probe correction changed an unrelated dependency")
	}
}

// The previous explicit release retains its original Dockerfile pin. A new
// metadata preparation step cannot silently change a sealed old inventory.
func TestRoundBaselineImageBranchReleasePreservesFixtureInventory(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	config := roundBaselinePreparationConfig{ReleaseKind: roundBaselineLocalFixturesRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, WorkCoveragePolicy: &coverage}
	previous, _, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || previous["Dockerfile.submission"] != "255d1827494c22482bb851323ee13d8ee03fb407fbf40348531b7d2ec32fc034" {
		t.Fatal("previous explicit producer was reinterpreted", err)
	}
	config.ReleaseKind = roundBaselineImageBranchesRelease
	current, _, err := roundBaselinePreparationConfiguredDependencies(config)
	if err != nil || current["Dockerfile.submission"] == previous["Dockerfile.submission"] {
		t.Fatal("new release lost its explicit changed asset", err)
	}
	delete(current, "Dockerfile.submission")
	delete(previous, "Dockerfile.submission")
	if !reflect.DeepEqual(previous, current) {
		t.Fatal("image branch preparation changed another sealed dependency")
	}
}

// Unknown releases and using new coverage assets under old scoring refuse.
func TestRoundBaselineExplicitReleaseRejectsUnknownOrLegacyPolicy(t *testing.T) {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	for _, config := range []roundBaselinePreparationConfig{
		{ReleaseKind: "synthetic-unknown/v1", G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork, WorkCoveragePolicy: &coverage},
		{ReleaseKind: roundBaselineLocalFixturesRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork},
		{ReleaseKind: roundBaselineLocalFixturesRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyLegacy},
		{ReleaseKind: roundBaselineImageBranchesRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork},
		{ReleaseKind: roundBaselineImageBranchesRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyLegacy},
		{ReleaseKind: roundBaselinePrivateProbeRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork},
		{ReleaseKind: roundBaselinePrivateProbeRelease, G2VolumePolicy: model.CompetitionG2VolumePolicyLegacy},
	} {
		if _, _, err := roundBaselinePreparationConfiguredDependencies(config); err == nil {
			t.Fatal("unsupported producer/policy combination accepted")
		}
	}
}
