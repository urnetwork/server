// Prospective-amendment tests use synthetic original publications. Cardinality,
// identity and historical defaults never depend on favorable result discovery.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

// Retains all five synthetic original positions, including the unused last two.
func newCalibrationProtocolFixture(t *testing.T) (calibrationAnalysisSettings, calibrationFileManifest, string, string, string, string) {
	t.Helper()
	root := newCalibrationFileRoot(t)
	write := func(path string, content []byte) string {
		t.Helper()
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		return hex.EncodeToString(digest[:])
	}
	approval := filepath.Join(root, "approval.md")
	approvalDigest := write(approval, []byte("Synthetic explicit three-A / one-bridge / three-B approval.\n"))
	positions := []calibrationOriginalPosition{}
	for index := 1; index <= 5; index++ {
		positionRoot := filepath.Join(root, fmt.Sprintf("cohort-%02d", index))
		if err := os.Mkdir(positionRoot, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(positionRoot, "artifacts"), 0700); err != nil {
			t.Fatal(err)
		}
		positions = append(positions, calibrationOriginalPosition{Position: index, JobId: fmt.Sprintf("synthetic-job-%d", index), RoundId: fmt.Sprintf("synthetic-round-%d", index), Root: positionRoot,
			RequestSha256: write(filepath.Join(positionRoot, "artifacts", "worker-request.json"), []byte(fmt.Sprintf("synthetic original request %d", index))), ProvenanceSha256: write(filepath.Join(positionRoot, "fixture-provenance.json"), []byte(fmt.Sprintf("synthetic provenance %d", index)))})
	}
	plan := map[string]any{"schema": 1, "kind": "prepared-corrected-release-five-cohort-aa-plan", "ordered_cohorts": positions, "base_sha": strings.Repeat("a", 40), "evaluator_image_digest": "sha256:" + strings.Repeat("b", 64), "evaluation_policy_sha256": strings.Repeat("c", 64)}
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(root, "original-plan.json")
	planDigest := write(planPath, planBytes)
	settings, err := calibrationSettingsForProtocol(calibrationProtocolThreeABridgeThreeB)
	if err != nil {
		t.Fatal(err)
	}
	settings, err = bindCalibrationProtocolPublications(settings, approval, approvalDigest, planPath, planDigest)
	if err != nil {
		t.Fatal(err)
	}
	manifest := calibrationFileManifest{Protocol: settings.Protocol, ProtocolApprovalSha256: approvalDigest, OriginalPlanSha256: planDigest, OriginalPositions: []int{1, 2, 3}, BaseSha: settings.OriginalBaseSha, EvaluatorImageDigest: settings.OriginalImageDigest, EvaluationPolicySha256: settings.OriginalPolicySha256}
	for _, position := range settings.OriginalPositions {
		manifest.Cohorts = append(manifest.Cohorts, calibrationFileCohort{JobId: position.JobId, RoundId: position.RoundId, ArtifactDirectory: filepath.Join(position.Root, "artifacts"), RequestSha256: position.RequestSha256})
	}
	return settings, manifest, approval, approvalDigest, planPath, planDigest
}

// Published cohort roots own provenance; requests belong to their artifact
// leaf. A root-level duplicate must never mask a missing authenticated leaf.
func TestCalibrationProtocolAuthenticatesArtifactRequestInventory(t *testing.T) {
	settings, _, approval, approvalDigest, plan, planDigest := newCalibrationProtocolFixture(t)
	if _, err := bindCalibrationProtocolPublications(settings, approval, approvalDigest, plan, planDigest); err != nil {
		t.Fatalf("original-shaped artifact request inventory refused: %v", err)
	}
	for _, position := range settings.OriginalPositions {
		artifactPath := filepath.Join(position.Root, "artifacts", "worker-request.json")
		rootPath := filepath.Join(position.Root, "worker-request.json")
		if err := os.Rename(artifactPath, rootPath); err != nil {
			t.Fatal(err)
		}
		if _, err := bindCalibrationProtocolPublications(settings, approval, approvalDigest, plan, planDigest); err == nil {
			t.Error("root-level duplicate replaced missing artifact request")
		}
		if err := os.Rename(rootPath, artifactPath); err != nil {
			t.Fatal(err)
		}
	}
}

// The sole statistical change is explicitly named three-cohort cardinality.
func TestCalibrationProtocolPreservesHistoricalSettings(t *testing.T) {
	original := defaultCalibrationAnalysisSettings()
	legacy, err := calibrationSettingsForProtocol("")
	if err != nil || !reflect.DeepEqual(legacy, original) {
		t.Fatalf("historical default changed: %v", err)
	}
	amended, err := calibrationSettingsForProtocol(calibrationProtocolThreeABridgeThreeB)
	if err != nil || amended.Cohorts != 3 {
		t.Fatalf("named amendment unavailable: %v", err)
	}
	amended.Protocol = ""
	amended.Cohorts = 5
	if !reflect.DeepEqual(amended, original) {
		t.Fatal("amendment changed a non-cardinality analysis setting")
	}
	if _, err := calibrationSettingsForProtocol("synthetic-unapproved-count"); err == nil {
		t.Fatal("unapproved protocol accepted")
	}
}

// Three observed pairs remain 54 observations, not 90 or bootstrap measurements.
func TestCalibrationProtocolThreePairsKeepsDeterministicReduction(t *testing.T) {
	settings, err := calibrationSettingsForProtocol(calibrationProtocolThreeABridgeThreeB)
	if err != nil {
		t.Fatal(err)
	}
	settings.BootstrapDraws = 128
	cohorts := newCalibrationAnalysisFixture(settings)
	first, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil {
		t.Fatal(err)
	}
	second, err := analyzeCalibrationCohorts(cohorts, settings)
	if err != nil || !reflect.DeepEqual(first, second) || !first.Accepted || first.Protocol != settings.Protocol || first.Cohorts != 3 || first.ObservedReplicates != 54 || first.BootstrapDraws != 128 || first.Placeability != 1 || first.RecommendedMargin != 0.161 || len(first.MarginTerms) != 10 {
		t.Fatalf("amended reduction drift: %+v, %v", first, err)
	}
	if _, err := analyzeCalibrationCohorts(cohorts, defaultCalibrationAnalysisSettings()); err == nil {
		t.Fatal("three pairs silently selected the amendment")
	}
	if _, err := analyzeCalibrationCohorts(newCalibrationAnalysisFixture(defaultCalibrationAnalysisSettings()), settings); err == nil {
		t.Fatal("five pairs accepted as amended three-pair sample")
	}
}

// First positions and the original old release are immutable even if another
// three-result subset, order or release happens to have favorable statistics.
func TestCalibrationProtocolRejectsOriginalPositionSubstitution(t *testing.T) {
	settings, original, _, _, _, _ := newCalibrationProtocolFixture(t)
	if err := validateCalibrationProtocolManifest(original, settings); err != nil {
		t.Fatal(err)
	}
	mutations := []func(*calibrationFileManifest){func(m *calibrationFileManifest) { m.Cohorts[0], m.Cohorts[1] = m.Cohorts[1], m.Cohorts[0] }, func(m *calibrationFileManifest) { m.Cohorts[2].JobId = "synthetic-later-job" }, func(m *calibrationFileManifest) { m.Cohorts[2].RoundId = "synthetic-later-round" }, func(m *calibrationFileManifest) { m.Cohorts[2].RequestSha256 = strings.Repeat("d", 64) }, func(m *calibrationFileManifest) {
		m.Cohorts[2].ArtifactDirectory = filepath.Join(filepath.Dir(m.Cohorts[2].ArtifactDirectory), "substitute")
	}, func(m *calibrationFileManifest) { m.OriginalPositions = []int{1, 2, 4} }, func(m *calibrationFileManifest) { m.BaseSha = strings.Repeat("e", 40) }, func(m *calibrationFileManifest) { m.EvaluatorImageDigest = "sha256:" + strings.Repeat("f", 64) }}
	for index, mutate := range mutations {
		manifest := original
		manifest.Cohorts = append([]calibrationFileCohort{}, original.Cohorts...)
		mutate(&manifest)
		if err := validateCalibrationProtocolManifest(manifest, settings); err == nil {
			t.Errorf("position/release mutation %d accepted", index)
		}
	}
}

// A named label without independent approval/plan pins is not authority.
func TestCalibrationProtocolRequiresIndependentPublicationPins(t *testing.T) {
	settings, manifest, approval, approvalDigest, plan, planDigest := newCalibrationProtocolFixture(t)
	for _, pins := range [][4]string{{"", approvalDigest, plan, planDigest}, {approval, "", plan, planDigest}, {approval, approvalDigest, "", planDigest}, {approval, approvalDigest, plan, strings.Repeat("0", 64)}} {
		if _, err := bindCalibrationProtocolPublications(settings, pins[0], pins[1], pins[2], pins[3]); err == nil {
			t.Error("missing/changed publication accepted")
		}
	}
	settings.ProtocolApprovalSha256 = ""
	if err := validateCalibrationProtocolManifest(manifest, settings); err == nil {
		t.Fatal("self-declared manifest approval accepted")
	}
	if _, err := bindCalibrationProtocolPublications(defaultCalibrationAnalysisSettings(), approval, approvalDigest, plan, planDigest); err == nil {
		t.Fatal("amendment pins accepted under historical protocol")
	}
}

// Original fourth/fifth positions must remain unused rather than silently
// running more samples and selecting the initial successful three afterward.
func TestCalibrationProtocolRejectsExecutionInUnusedOriginalPositions(t *testing.T) {
	settings, _, approval, approvalDigest, plan, planDigest := newCalibrationProtocolFixture(t)
	content, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	var publication struct {
		Positions []calibrationOriginalPosition `json:"ordered_cohorts"`
	}
	if err := json.Unmarshal(content, &publication); err != nil {
		t.Fatal(err)
	}
	for _, position := range publication.Positions[3:] {
		for _, relative := range []string{"started", "started.json", "manager-terminal.log", "artifacts/worker-result.json"} {
			path := filepath.Join(position.Root, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("synthetic execution marker"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := bindCalibrationProtocolPublications(settings, approval, approvalDigest, plan, planDigest); err == nil {
				t.Errorf("unused position %d %s executed", position.Position, relative)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Retaining the five-position publication alone is insufficient if the unused
// original request/provenance inventory itself was removed or overwritten.
func TestCalibrationProtocolRequiresRetainedUnusedOriginalInventory(t *testing.T) {
	settings, _, approval, approvalDigest, plan, planDigest := newCalibrationProtocolFixture(t)
	unused := filepath.Join(filepath.Dir(settings.OriginalPositions[0].Root), "cohort-05", "fixture-provenance.json")
	if err := os.WriteFile(unused, []byte("changed synthetic provenance"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := bindCalibrationProtocolPublications(settings, approval, approvalDigest, plan, planDigest); err == nil {
		t.Fatal("changed unused original inventory accepted")
	}
}

// A historical manifest can never carry amended analysis metadata unnoticed.
func TestCalibrationProtocolDefaultRejectsAmendedManifest(t *testing.T) {
	_, manifest, _, _, _, _ := newCalibrationProtocolFixture(t)
	if err := validateCalibrationProtocolManifest(manifest, defaultCalibrationAnalysisSettings()); err == nil {
		t.Fatal("amended report relabeled as historical default")
	}
}

// All amendment flags parse together; the ordinary calibrated route keeps its
// required source, lock and cpu options unchanged.
func TestCalibrationProtocolCommandParsesExplicitPublicationPins(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	args := []string{"calibrate", "--calibration-inputs=/synthetic/input.json", "--calibration-inputs-sha256=" + strings.Repeat("a", 64), "--calibration-lock=/synthetic/lock", "--calibration-cpuset=0,1", "--calibration-protocol=" + calibrationProtocolThreeABridgeThreeB, "--calibration-approval=/synthetic/approval.md", "--calibration-approval-sha256=" + strings.Repeat("b", 64), "--calibration-original-plan=/synthetic/original-plan.json", "--calibration-original-plan-sha256=" + strings.Repeat("c", 64)}
	opts, err := parser.ParseArgs(simulationUsage(), args, "synthetic-version")
	if err != nil || optString(opts, "--calibration-protocol", "") != calibrationProtocolThreeABridgeThreeB {
		t.Fatalf("named amended CLI rejected: %v", err)
	}
}
