// The source graph selector routes a round to the evaluator release cut for its
// repositories. The three historical producers, their preparation programs and
// their pins stay byte for byte, and no release authenticates another's control.
package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func sourceGraphContainerFileSha256(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", name))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func sourceGraphPolicyCases() []struct {
	name     string
	policy   model.CompetitionG2VolumePolicy
	coverage *model.CompetitionWorkCoveragePolicy
} {
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	return []struct {
		name     string
		policy   model.CompetitionG2VolumePolicy
		coverage *model.CompetitionWorkCoveragePolicy
	}{
		{name: "legacy", policy: model.CompetitionG2VolumePolicyLegacy},
		{name: "minimum-work", policy: model.CompetitionG2VolumePolicyMinimumWork},
		{name: "work-coverage", policy: model.CompetitionG2VolumePolicyMinimumWork, coverage: &coverage},
	}
}

// Version-one controls must still authenticate: the programs of the ten
// repositories and the pins that name them are the bytes they were sealed at.
func TestSourceGraphHistoricalProducersAndPinsAreUnchanged(t *testing.T) {
	for name, digest := range map[string]string{
		"evaluator.sh":                            "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b",
		"evaluator-minimum-work.sh":               "7780930d44ebc1df5d8f775a75c37aadb2048fc1b9386027615f3ce6b5fe56d4",
		"evaluator-work-coverage.sh":              "d2ad9dc8d6fcbb41f74ea5b1fb3746bb45447ffb25d4030c925516f7b46a8c72",
		"prepare-round-baseline.sh":               "c3a7474685eb6e5aba141fa460c5ada2f5cfc8d8cbc543f2fe309720cb9e5428",
		"prepare-round-baseline-minimum-work.sh":  "ef49fbae0d821e5a72620d18c00a1ce422891e2a3962a0a13415c3c21573df9f",
		"prepare-round-baseline-work-coverage.sh": "9bf0bba8308f718ba225b0c50c13822113be7956bfd2d90694d075a5aa66048a",
	} {
		if actual := sourceGraphContainerFileSha256(t, name); actual != digest {
			t.Errorf("historical program %s changed: %s", name, actual)
		}
	}
	wantPrograms := map[string][3]string{
		"legacy":        {"prepare-round-baseline.sh", "evaluator.sh", "0cccda1d1dcc227f30fda800ddb9bbb28e29dd48cc62885da637fcd4eb4eb00b"},
		"minimum-work":  {"prepare-round-baseline-minimum-work.sh", "evaluator-minimum-work.sh", "7780930d44ebc1df5d8f775a75c37aadb2048fc1b9386027615f3ce6b5fe56d4"},
		"work-coverage": {"prepare-round-baseline-work-coverage.sh", "evaluator-work-coverage.sh", "484a9e2eb107b0d5a812db4f0c2a9cd30a30e3615604def4eeea1d000a0b8b4e"},
	}
	for _, c := range sourceGraphPolicyCases() {
		historical, historicalProgram, err := roundBaselinePreparationPolicyDependencies(c.policy, c.coverage)
		if err != nil {
			t.Fatal(c.name, err)
		}
		selected, program, err := roundBaselinePreparationReleaseDependencies(model.CompetitionSourceGraphPolicyLegacy, c.policy, c.coverage)
		if err != nil || program != historicalProgram || !reflect.DeepEqual(selected, historical) {
			t.Fatalf("%s: omission did not select the historical release: %v", c.name, err)
		}
		want := wantPrograms[c.name]
		if program != want[0] || selected[want[1]] != want[2] || len(selected) != 16 {
			t.Errorf("%s: historical release is %s with %s=%s over %d files", c.name, program, want[1], selected[want[1]], len(selected))
		}
		for name := range selected {
			if strings.Contains(name, "sidecar") {
				t.Errorf("%s: historical release names %s", c.name, name)
			}
		}
	}
	base := roundBaselinePreparationDependencyDigests()
	for name, digest := range map[string]string{
		"build-submission.sh":          "4a998275ffe94d322462ec9d7876af1ff288c3e8ba85f0c3278b4b512baa0172",
		"prepare-evaluation-source.sh": "a2445db1aa6e8655da12ca8ec810eda86c5e93df84801887c76ed490f232d4b6",
		"competitionpatch":             "3d161d76bace48bff6536164a84f330b1618c82f319775b8551c19872b937e54",
		"competitionpatch.sha256":      "560358c0192972f71e715144289b8871fc4c3566d382cd30a9b7d3deb74c6df7",
	} {
		if base[name] != digest {
			t.Errorf("sealed v3 dependency %s was re-pinned to %s", name, base[name])
		}
	}
}

// Each policy has its own producer of the eleven repositories, and the
// preparation program in this tree pins exactly that producer's bytes.
func TestSourceGraphSidecarReleasePinsItsOwnProducers(t *testing.T) {
	pinPattern := regexp.MustCompile(`(?m)^    \[ "\$\(sha256sum -- "\$source" \| awk '\{print \$1\}'\)" = ([0-9a-f]{64}) \] \|\| return 1$`)
	seenProducers, seenPrograms := map[string]bool{}, map[string]bool{}
	for _, c := range sourceGraphPolicyCases() {
		historical, historicalProgram, err := roundBaselinePreparationPolicyDependencies(c.policy, c.coverage)
		if err != nil {
			t.Fatal(c.name, err)
		}
		producer, digest, program, err := roundBaselineSidecarProducer(c.policy, c.coverage)
		if err != nil {
			t.Fatal(c.name, err)
		}
		if seenProducers[producer] || seenPrograms[program] || !strings.HasSuffix(producer, "-sidecar.sh") || !strings.HasSuffix(program, "-sidecar.sh") {
			t.Fatalf("%s: release %s / %s is not its own", c.name, producer, program)
		}
		seenProducers[producer], seenPrograms[program] = true, true
		if program == historicalProgram || historical[producer] != "" {
			t.Fatalf("%s: the sidecar release reuses a historical program", c.name)
		}
		for name, pinned := range historical {
			if pinned == digest {
				t.Fatalf("%s: the sidecar producer has the bytes of historical %s", c.name, name)
			}
		}
		if actual := sourceGraphContainerFileSha256(t, producer); actual != digest {
			t.Errorf("%s: %s is %s, pinned %s", c.name, producer, actual, digest)
		}
		content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", program))
		if err != nil {
			t.Fatal(err)
		}
		matches := pinPattern.FindAllSubmatch(content, -1)
		if len(matches) != 1 || string(matches[0][1]) != digest {
			t.Errorf("%s: %s does not pin exactly its producer", c.name, program)
		}
		source := string(content)
		if strings.Count(source, `"$baseline_release/`+producer+`"`) != 1 || strings.Count(source, `.evaluation_policy.source_graph_policy == "sidecar/1"`) != 1 ||
			strings.Count(source, `source_graph_policy:"sidecar/1"`) != 1 {
			t.Errorf("%s: %s does not run its producer for sidecar/1 alone", c.name, program)
		}
		producerBytes, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", producer))
		if err != nil {
			t.Fatal(err)
		}
		producerSource := string(producerBytes)
		if strings.Count(producerSource, "\nREPOSITORIES=("+strings.Join(model.CompetitionSourceRepositoryNames(), " ")+")\nreadonly REPOSITORIES\n") != 1 ||
			strings.Count(producerSource, `(.evaluation_policy | type == "object" and .source_graph_policy == "sidecar/1") and`) != 1 ||
			strings.Count(producerSource, `"site_listen","source_graph_policy","takeover_margin"`) != 1 {
			t.Errorf("%s: %s does not hold the eleven repositories under sidecar/1", c.name, producer)
		}
	}
	if _, _, _, err := roundBaselineSidecarProducer("unknown/1", nil); err == nil {
		t.Fatal("an unknown scoring policy named a sidecar producer")
	}
	coverage := model.DefaultCompetitionWorkCoveragePolicy()
	if _, _, _, err := roundBaselineSidecarProducer(model.CompetitionG2VolumePolicyLegacy, &coverage); err == nil {
		t.Fatal("coverage without the minimum-work contract named a sidecar producer")
	}
}

// Until the release is qualified this tree is where it is cut from, so every
// script it pins is the tree's bytes. The patch validator is compiled at the
// qualification: its two digests are unrecorded and the release is refused.
func TestSourceGraphSidecarReleaseIsCutFromThisTreeAndUnqualified(t *testing.T) {
	dependencies := roundBaselineSidecarDependencyDigests()
	unrecorded := []string{}
	for name, digest := range dependencies {
		if digest == "" {
			unrecorded = append(unrecorded, name)
			continue
		}
		if actual := sourceGraphContainerFileSha256(t, name); actual != digest {
			t.Errorf("%s is %s in this tree, pinned %s", name, actual, digest)
		}
	}
	if len(dependencies) != 16 || len(unrecorded) != 2 || dependencies["competitionpatch"] != "" || dependencies["competitionpatch.sha256"] != "" {
		t.Fatalf("the sidecar release has %d files, unrecorded %v", len(dependencies), unrecorded)
	}
	historical := roundBaselinePreparationDependencyDigests()
	if dependencies["build-submission.sh"] == historical["build-submission.sh"] || dependencies["prepare-evaluation-source.sh"] == historical["prepare-evaluation-source.sh"] {
		t.Fatal("the sidecar release pins a helper that copies the ten repositories")
	}
	for _, name := range []string{"build-submission.sh", "prepare-evaluation-source.sh"} {
		content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(content, []byte("\nREPOSITORIES=("+strings.Join(model.CompetitionSourceRepositoryNames(), " ")+")\n")) {
			t.Errorf("%s does not hold the eleven repositories", name)
		}
	}
	for _, c := range sourceGraphPolicyCases() {
		if selected, program, err := roundBaselinePreparationReleaseDependencies(model.CompetitionSourceGraphPolicySidecar, c.policy, c.coverage); err == nil || selected != nil || program != "" {
			t.Fatalf("%s: an unqualified release was selected", c.name)
		}
	}
	for _, graph := range []model.CompetitionSourceGraphPolicy{"sidecar", "sidecar/2", "legacy"} {
		if _, _, err := roundBaselinePreparationReleaseDependencies(graph, model.CompetitionG2VolumePolicyLegacy, nil); err == nil {
			t.Fatalf("unknown source graph %q selected a release", graph)
		}
	}
}

// A policy of the ten keeps its frozen bytes, an explicit graph is frozen with
// the round, and a frozen round never takes the graph of the process settings.
func TestSourceGraphPolicyIsFrozenWithTheRound(t *testing.T) {
	settings := validSettings()
	legacyPolicy, err := policySnapshot(settings)
	if err != nil || bytes.Contains(legacyPolicy, []byte("source_graph_policy")) {
		t.Fatal("a policy of the ten acquired a source graph field", err)
	}
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	eleven := *settings
	eleven.EvaluationPolicy.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	if err := eleven.Validate(); err != nil {
		t.Fatal("sidecar/1 refused", err)
	}
	elevenPolicy, err := policySnapshot(&eleven)
	if err != nil || !bytes.Contains(elevenPolicy, []byte(`"source_graph_policy":"sidecar/1"`)) {
		t.Fatal("the explicit source graph was not frozen", err)
	}
	unknown := *settings
	unknown.EvaluationPolicy.SourceGraphPolicy = "sidecar/2"
	if unknown.Validate() == nil {
		t.Fatal("an unknown source graph validated")
	}
	if _, err := decodeRoundPolicySnapshot(bytes.Replace(elevenPolicy, []byte(`"sidecar/1"`), []byte(`"sidecar/2"`), 1)); err == nil {
		t.Fatal("a frozen round named an unknown source graph")
	}
	elevenRound := &roundRecord{RoundResult: RoundResult{RoundId: server.NewId(), Epoch: 1}, PolicyJson: elevenPolicy}
	frozen, err := settingsForFrozenRound(settings, elevenRound)
	if err != nil || frozen.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicySidecar || settings.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicyLegacy {
		t.Fatal("a round frozen under sidecar/1 took the process graph", err)
	}
	legacyRound := &roundRecord{RoundResult: RoundResult{RoundId: server.NewId(), Epoch: 1}, PolicyJson: legacyPolicy}
	frozen, err = settingsForFrozenRound(&eleven, legacyRound)
	if err != nil || frozen.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicyLegacy {
		t.Fatal("a round frozen under the ten took the process graph", err)
	}
}

// A staged future policy moves only its own epochs to the eleven, needs its own
// evaluator image for that, and never moves the current round.
func TestSourceGraphFuturePolicySelectsItsOwnGraphAndImage(t *testing.T) {
	settings := validSettings()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(t)
	inherited, err := settings.ForCompetitionEpoch(2)
	if err != nil || inherited.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicyLegacy {
		t.Fatal("a future policy naming no graph changed it", err)
	}
	settings.FutureEpochPolicy.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	current, err := settings.ForCompetitionEpoch(1)
	if err != nil || current.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicyLegacy {
		t.Fatal("the staged graph changed the current epoch", err)
	}
	future, err := settings.ForCompetitionEpoch(2)
	if err != nil || future.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicySidecar || settings.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicyLegacy {
		t.Fatal("the future epoch did not take its staged graph", err)
	}
	policy, err := policySnapshot(future)
	if err != nil || !bytes.Contains(policy, []byte(`"source_graph_policy":"sidecar/1"`)) {
		t.Fatal("the future round did not freeze its graph", err)
	}
	for _, image := range []string{"", settings.EvaluatorImageDigest} {
		relabeled := *settings
		staged := *settings.FutureEpochPolicy
		staged.EvaluatorImageDigest = image
		relabeled.FutureEpochPolicy = &staged
		if _, err := relabeled.ForCompetitionEpoch(2); err == nil {
			t.Fatalf("the image %q of the ten was relabeled as the eleven's", image)
		}
	}
	unknown := *settings
	staged := *settings.FutureEpochPolicy
	staged.SourceGraphPolicy = "sidecar/2"
	unknown.FutureEpochPolicy = &staged
	if _, err := unknown.ForCompetitionEpoch(2); err == nil {
		t.Fatal("an unknown future source graph was staged")
	}
	legacyProfile := &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{}}
	settings.roundBaselinePreparation = legacyProfile
	settings.futureBaselinePreparation = &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{G2VolumePolicy: model.CompetitionG2VolumePolicyMinimumWork,
		WorkCoveragePolicy: settings.FutureEpochPolicy.WorkCoveragePolicy, SourceGraphPolicy: model.CompetitionSourceGraphPolicySidecar}}
	future, err = settings.ForCompetitionEpoch(2)
	if err != nil || future.roundBaselinePreparation != settings.futureBaselinePreparation || settings.roundBaselinePreparation != legacyProfile {
		t.Fatal("the future epoch did not select the preparation role of its graph", err)
	}
}

// The lock of a source activation must hold exactly the graph its target
// epoch's evaluator release copies, and the preparation role must be that
// graph's own.
func TestSourceGraphSourceSettingsRefuseALockOfAnotherGraph(t *testing.T) {
	settings := automationApiSettings()
	ten := map[string]string{}
	for _, repository := range model.CompetitionSourceRepositoryNamesBeforeSidecar() {
		ten[repository] = strings.Repeat("a", 40)
	}
	eleven := map[string]string{"sidecar": strings.Repeat("b", 40)}
	for repository, commit := range ten {
		eleven[repository] = commit
	}
	intentFor := func(commits map[string]string) *model.CompetitionSourceActivation {
		lock, err := model.CompetitionSourceLockDigest(commits)
		if err != nil {
			t.Fatal(err)
		}
		return &model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
			TargetRoundId: server.NewId(), TargetEpoch: 1, Commits: commits, SourceLockSha256: lock, EvaluatorImageDigest: settings.EvaluatorImageDigest,
			HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256, Threshold: settings.EvaluationPolicy.TakeoverMargin}
	}
	if derived, err := sourceSettingsFromIntent(settings, intentFor(ten)); err != nil || derived.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicyLegacy {
		t.Fatal("the ten were refused under the graph of the ten", err)
	}
	if _, err := sourceSettingsFromIntent(settings, intentFor(eleven)); err == nil {
		t.Fatal("a lock naming sidecar was prepared for an evaluator release of the ten")
	}
	sidecar := *settings
	sidecar.EvaluationPolicy.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	derived, err := sourceSettingsFromIntent(&sidecar, intentFor(eleven))
	if err != nil || derived.EvaluationPolicy.SourceGraphPolicy != model.CompetitionSourceGraphPolicySidecar {
		t.Fatal("the eleven were refused under sidecar/1", err)
	}
	if _, err := sourceSettingsFromIntent(&sidecar, intentFor(ten)); err == nil {
		t.Fatal("a lock of the ten was prepared for an evaluator release of the eleven")
	}
	sidecar.roundBaselinePreparation = &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{}}
	if _, err := sourceSettingsFromIntent(&sidecar, intentFor(eleven)); err == nil {
		t.Fatal("the eleven inherited the preparation role of the ten")
	}
	sidecar.roundBaselinePreparation = &roundBaselinePreparationProfile{Config: roundBaselinePreparationConfig{SourceGraphPolicy: model.CompetitionSourceGraphPolicySidecar}}
	if _, err := sourceSettingsFromIntent(&sidecar, intentFor(eleven)); err != nil {
		t.Fatal("the eleven were refused their own preparation role", err)
	}
}

// A receipt is accepted only from the release of the frozen round's graph: the
// programs of the eleven write sidecar/1, those of the ten write nothing.
func TestSourceGraphPreparationReceiptBindsTheFrozenGraph(t *testing.T) {
	fixture := newRoundBaselinePreparationEvidenceFixture(t)
	if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err != nil {
		t.Fatal(err)
	}
	fixture.receipt.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	fixture.publish(t)
	if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err == nil {
		t.Fatal("a receipt of the eleven authenticated a control of the ten")
	}

	fixture = newRoundBaselinePreparationEvidenceFixture(t)
	fixture.settings.EvaluationPolicy.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	fixture.settings.roundBaselinePreparation.Config.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
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
	fixture.publish(t)
	if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err == nil {
		t.Fatal("a receipt of the ten authenticated a control of the eleven")
	}
	fixture.receipt.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	fixture.publish(t)
	if _, _, err := authenticateRoundBaselinePreparation(fixture.directory, fixture.settings, fixture.preparation); err != nil {
		t.Fatal("a receipt of the eleven did not authenticate its own control", err)
	}
}

// The producer capability refuses to run a release cut for another graph than
// the frozen round's before it reads any pinned file.
func TestSourceGraphPreparationRefusesAnotherGraphsRelease(t *testing.T) {
	fixture := newRoundBaselinePreparationEvidenceFixture(t)
	fixture.settings.roundBaselinePreparation.Config.SourceGraphPolicy = model.CompetitionSourceGraphPolicySidecar
	if _, err := (CommandEvaluator{}).PrepareRoundBaseline(t.Context(), fixture.settings, fixture.preparation); err == nil || !strings.Contains(err.Error(), "another source graph") {
		t.Fatal("a release of the eleven prepared a round of the ten", err)
	}
}
