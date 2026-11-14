// Complete synthetic native controls exercise new-policy preparation without
// running a simulator, Docker, a build or a timed workload.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

// Rebinds fixture-owned native hashes only, retaining the original nine tuples
// and every resource/accounting/segment predicate of the legacy fixture.
func minimumWorkPreparationEvidenceFixture(t *testing.T) *roundBaselinePreparationEvidenceFixture {
	t.Helper()
	fixture := newRoundBaselinePreparationEvidenceFixture(t)
	fixture.settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	fixture.settings.roundBaselinePreparation.Config.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	policy, err := policySnapshot(fixture.settings)
	if err != nil {
		t.Fatal(err)
	}
	fixture.preparation.SourceJob.Round.PolicyJson = policy
	fixture.preparation.ContextSha256, err = roundBaselinePreparationContext(fixture.settings, fixture.preparation.SourceJob)
	if err != nil {
		t.Fatal(err)
	}
	fixture.receipt.ContextSha256 = fixture.preparation.ContextSha256
	fixture.receipt.G2VolumePolicy, fixture.receipt.ScorerVersion = model.CompetitionG2VolumePolicyMinimumWork, model.CompetitionMinimumWorkScorerVersion
	baseline := preparedRoundBaselineTest(t, fixture.settings, fixture.preparation)
	roundBaselinePreparationTestFile(t, fixture.directory, "baseline.json", baseline.BaselineJson)
	fixture.receipt.BaselineSha256 = baseline.BaselineSha256
	for index := range fixture.artifacts {
		artifact := fixture.artifacts[index]
		if !strings.HasSuffix(artifact.Path, "/run.json") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(fixture.directory, artifact.Path))
		if err != nil {
			t.Fatal(err)
		}
		var run map[string]any
		if err := json.Unmarshal(content, &run); err != nil {
			t.Fatal(err)
		}
		run["g2_volume_policy"], run["scorer_version"] = string(model.CompetitionG2VolumePolicyMinimumWork), model.CompetitionMinimumWorkScorerVersion
		updatedRun := roundBaselinePreparationTestJson(t, fixture.directory, artifact.Path, run)
		fixture.artifacts[index] = updatedRun
		markerPath := strings.TrimSuffix(artifact.Path, "run.json") + "run.complete.json"
		markerBytes, err := os.ReadFile(filepath.Join(fixture.directory, markerPath))
		if err != nil {
			t.Fatal(err)
		}
		var marker map[string]any
		if err := json.Unmarshal(markerBytes, &marker); err != nil {
			t.Fatal(err)
		}
		marker["g2_volume_policy"], marker["scorer_version"] = string(model.CompetitionG2VolumePolicyMinimumWork), model.CompetitionMinimumWorkScorerVersion
		marker["run_manifest_sha256"], marker["run_manifest_bytes"] = updatedRun.Sha256, updatedRun.Bytes
		updatedMarker := roundBaselinePreparationTestJson(t, fixture.directory, markerPath, marker)
		for markerIndex := range fixture.artifacts {
			if fixture.artifacts[markerIndex].Path == markerPath {
				fixture.artifacts[markerIndex] = updatedMarker
			}
		}
	}
	fixture.publish(t)
	return fixture
}

// Nine controls and their separate no-submission/no-candidate purpose survive
// authentication under scorer two; the three-hour candidate budget is untouched.
func TestG2PreparationAuthenticatesMinimumWorkNineTupleChain(t *testing.T) {
	fixture := minimumWorkPreparationEvidenceFixture(t)
	baseline, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(baseline)
	if hex.EncodeToString(digest[:]) != fixture.receipt.BaselineSha256 || fixture.settings.EvaluationPolicy.Replicates != 9 || fixture.settings.EvaluationPolicy.ScoreTimeoutSeconds != 10800 || fixture.preparation.SourceJob.StartedAt != nil {
		t.Fatal("new preparation changed its control identity or submission clock")
	}
}

// Rehashing an otherwise valid chain cannot make a policy omission acceptable.
func TestG2PreparationRejectsPolicyOmissionAtEveryNativeBoundary(t *testing.T) {
	for _, boundary := range []string{"receipt", "run", "marker"} {
		fixture := minimumWorkPreparationEvidenceFixture(t)
		if boundary == "receipt" {
			fixture.receipt.G2VolumePolicy = ""
		} else {
			root := "evidence/runs/baseline-01/output/" + fixture.receipt.ControlEvaluationIds[0] + "/"
			name := "run.json"
			if boundary == "marker" {
				name = "run.complete.json"
			}
			path := root + name
			content, err := os.ReadFile(filepath.Join(fixture.directory, path))
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(content, &document); err != nil {
				t.Fatal(err)
			}
			delete(document, "g2_volume_policy")
			changed := roundBaselinePreparationTestJson(t, fixture.directory, path, document)
			for index := range fixture.artifacts {
				if fixture.artifacts[index].Path == path {
					fixture.artifacts[index] = changed
				}
			}
			if boundary == "run" {
				markerPath := root + "run.complete.json"
				content, err := os.ReadFile(filepath.Join(fixture.directory, markerPath))
				if err != nil {
					t.Fatal(err)
				}
				var marker map[string]any
				if err := json.Unmarshal(content, &marker); err != nil {
					t.Fatal(err)
				}
				marker["run_manifest_sha256"], marker["run_manifest_bytes"] = changed.Sha256, changed.Bytes
				changedMarker := roundBaselinePreparationTestJson(t, fixture.directory, markerPath, marker)
				for index := range fixture.artifacts {
					if fixture.artifacts[index].Path == markerPath {
						fixture.artifacts[index] = changedMarker
					}
				}
			}
		}
		fixture.publish(t)
		if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err == nil {
			t.Errorf("%s policy omission accepted after valid native rehash", boundary)
		}
	}
}
