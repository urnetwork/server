// Source-index previews use synthetic authenticated snapshots. No test fetches
// a moving origin, edits the index, or obtains publication authority.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Full fixed synthetic maps prevent local repository heads from influencing
// a preview. Every referenced configuration value remains private input.
func sourceIndexTestStatus(t *testing.T, purpose string) *controller.CompetitionSourceLifecycleStatus {
	t.Helper()
	manifest := reviewedSourceTestManifest(t)
	manifest.EvaluationSource.Epochs = nil
	round := manifest.EvaluationSource.Rounds[0]
	round.Epoch, round.ActivationGeneration, round.Reason = 2, 2, "synthetic_no_winner_carry"
	round.RewardDecisionDigest = strings.Repeat("e", 64)
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, round)
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(manifestBytes)
	intent := &model.CompetitionSourceActivation{Environment: "synthetic-environment", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season",
		Generation: 2, ExpectedGeneration: 1, SourceGeneration: 0, TargetEpoch: 2, TargetRoundId: server.NewId(), SourceDecisionDigest: round.SourceDecisionDigest,
		IntentSha256: strings.Repeat("f", 64), Kind: "reviewed_source_carry_forward", State: "prepared", Commits: manifest.EvaluationSource.Generations[0].Repositories.commits(),
		SourceLockSha256: round.SourceLockSha256, Manifest: manifestBytes, ManifestSha256: hex.EncodeToString(digest[:]), EvaluatorImageDigest: "sha256:" + strings.Repeat("d", 64),
		Threshold: 0.161, ManifestCommit: strings.Repeat("9", 40), ConfigExpectedCommit: strings.Repeat("8", 40), Reason: "synthetic-private-operational-text"}
	status := &controller.CompetitionSourceLifecycleStatus{Schema: 1, Capability: controller.CompetitionSourceLifecycleCapability, Purpose: purpose, Intent: intent}
	if purpose != controller.CompetitionSourceLifecyclePrepared {
		status.Intake = &model.CompetitionSourceIntakeActivation{Schema: 1, Kind: model.CompetitionIntakeFirstKind,
			Policy:      model.CompetitionIntakeFirstPolicy{Kind: model.CompetitionIntakeFirstKind, FromEpoch: 2, ApprovalSha256: strings.Repeat("a", 64)},
			Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation, ExpectedGeneration: 1,
			TargetEpoch: intent.TargetEpoch, TargetRoundId: intent.TargetRoundId, IntentSha256: intent.IntentSha256, RoundPolicySha256: strings.Repeat("b", 64),
			ApiReadinessSha256: strings.Repeat("c", 64), ApiRuntimeImageDigest: "sha256:" + strings.Repeat("e", 64), CreatedAt: time.Unix(2000000000, 0).UTC()}
		status.Round = &controller.CompetitionSourceLifecycleRound{RoundId: intent.TargetRoundId, Epoch: 2,
			OpensAt: time.Unix(2000000000, 0).UTC(), ClosesAt: time.Unix(2000000000, 0).UTC().Add(7 * 24 * time.Hour)}
		status.ApiAcknowledged = true
	}
	if purpose == controller.CompetitionSourceLifecycleGrading {
		status.WorkerAcknowledged, status.GradingRuntimeActivated, status.HostAdmissionReady, status.GradingReady = true, true, true, true
		status.BaselineSha256 = strings.Repeat("7", 64)
	}
	return status
}

// The exporter reflects purpose grants, not an activated timestamp. It states
// baseline/compiler absence explicitly rather than advertising planned proof.
func TestSourceIndexSeparatesPreparedIntakeAndGradingPurposes(t *testing.T) {
	for _, fixture := range []struct {
		purpose string
		want    string
	}{
		{purpose: controller.CompetitionSourceLifecyclePrepared, want: "PREPARED"},
		{purpose: controller.CompetitionSourceLifecycleIntake, want: "INTAKE_OPEN-evaluator_pending"},
		{purpose: controller.CompetitionSourceLifecycleGrading, want: "GRADING_READY"},
	} {
		status := sourceIndexTestStatus(t, fixture.purpose)
		activatedAt := time.Unix(2000000010, 0).UTC()
		status.Intent.ActivatedAt = &activatedAt
		checkpoint, err := newSourceIndexCheckpoint(status)
		if err != nil || checkpoint.Status != fixture.want || checkpoint.CompilerVerified || checkpoint.CompilerRequirement != "go1.27.3" ||
			checkpoint.GradingReady != (fixture.purpose == controller.CompetitionSourceLifecycleGrading) {
			t.Fatalf("purpose %s was inferred from unrelated activation metadata: %+v, %v", fixture.purpose, checkpoint, err)
		}
		preview := renderSourceIndexMarkdown(checkpoint)
		if !strings.Contains(preview, fixture.want) || strings.Contains(preview, " — ACTIVE") {
			t.Fatal("preview mislabeled an intake-purpose grant as a full activation")
		}
		if fixture.purpose != controller.CompetitionSourceLifecycleGrading && (checkpoint.BaselineSha256 != "" || !strings.Contains(preview, "not established; no baseline is claimed")) {
			t.Fatal("prepared/intake-only preview invented an actual scoring baseline")
		}
	}
}

// Exact full locks, public repository routes and raw manifest hashes are
// checked independently of any product checkout or current remote ref.
func TestSourceIndexExportsExactTenRepositoriesAndSanitizesPrivateInput(t *testing.T) {
	status := sourceIndexTestStatus(t, controller.CompetitionSourceLifecycleGrading)
	status.Intent.ManifestCommit = strings.Repeat("6", 40)
	status.Intent.GitRefs = map[string]model.CompetitionSourceGitRef{"synthetic-private-origin": {Branch: "synthetic-private-path", Commit: strings.Repeat("5", 40)}}
	status.CompilerEvidenceSha256 = strings.Repeat("4", 64)
	checkpoint, err := newSourceIndexCheckpoint(status)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Repositories) != 10 || !checkpoint.CompilerVerified || checkpoint.CompilerEvidenceSha256 != status.CompilerEvidenceSha256 {
		t.Fatal("public source checkpoint omitted complete source or genuine compiler evidence")
	}
	commits := map[string]string{}
	for index, repository := range checkpoint.Repositories {
		name := sourceRepositoryNames()[index]
		parsed, err := url.Parse(repository.Url)
		wantPath := "/urnetwork/" + name
		if name == "sn" {
			wantPath = "/urfoundation/sn"
		}
		if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != wantPath ||
			repository.Directory != name || repository.Commit != status.Intent.Commits[name] {
			t.Fatalf("repository %s lost its public route/exact source identity: %+v, %v", name, repository, err)
		}
		commits[name] = repository.Commit
	}
	digest, err := model.CompetitionSourceLockDigest(commits)
	if err != nil || digest != checkpoint.SourceLockSha256 {
		t.Fatal("exported public repository map does not reproduce the advertised lock", err)
	}
	content, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	preview := renderSourceIndexMarkdown(checkpoint)
	for _, private := range []string{"synthetic-private-operational-text", "synthetic-private-origin", "synthetic-private-path", status.Intent.ManifestSha256, status.Intent.ManifestCommit} {
		if strings.Contains(string(content), private) || strings.Contains(preview, private) {
			t.Fatalf("public checkpoint copied private config/operational input %q", private)
		}
	}
	if _, err := sourceIndexRepositoryUrl("config"); err == nil {
		t.Fatal("public source allowlist exposed operational config as a measured repository")
	}
}

// An independent instrument child changes the evaluated server commit without
// rewriting which exact product parent the operator actually approved.
func TestSourceIndexKeepsSelectedProductSeparateFromInstrumentChild(t *testing.T) {
	status := sourceIndexTestStatus(t, controller.CompetitionSourceLifecyclePrepared)
	intent, _ := sourceTrustedInstrumentModeFixture(t)
	intent.Environment, intent.CompetitionId, intent.SeasonId = status.Intent.Environment, status.Intent.CompetitionId, status.Intent.SeasonId
	intent.TargetRoundId, intent.ExpectedGeneration, intent.IntentSha256 = status.Intent.TargetRoundId, 1, status.Intent.IntentSha256
	status.Intent = intent
	checkpoint, err := newSourceIndexCheckpoint(status)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.SelectedProductRepositories) != 10 || checkpoint.SelectedProductSourceLockSha256 != intent.TrustedInstrument.SelectedProductSourceLockSha256 ||
		checkpoint.SelectedProductSourceLockSha256 == checkpoint.SourceLockSha256 || checkpoint.SelectedProductKind != "reviewed_source_carry_forward" {
		t.Fatal("instrument child was substituted for actual selected product identity")
	}
	productCommits := map[string]string{}
	for _, repository := range checkpoint.SelectedProductRepositories {
		productCommits[repository.Directory] = repository.Commit
	}
	digest, err := model.CompetitionSourceLockDigest(productCommits)
	if err != nil || digest != checkpoint.SelectedProductSourceLockSha256 || productCommits["server"] == checkpoint.Repositories[0].Commit {
		t.Fatal("public product parents did not preserve the separately selected product graph", err)
	}
	if !strings.Contains(renderSourceIndexMarkdown(checkpoint), "Selected product before the separate instrument child") {
		t.Fatal("public preview concealed the product/instrument distinction")
	}
}

// Every missing genuine readiness component refuses the high-authority label.
// A real source-purpose grant is similarly required for intake-only status.
func TestSourceIndexRejectsMissingOrInconsistentAuthority(t *testing.T) {
	for index, mutate := range []func(*controller.CompetitionSourceLifecycleStatus){
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Capability = "" },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Purpose = "synthetic-unsupported" },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.ApiAcknowledged = false },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.WorkerAcknowledged = false },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.GradingRuntimeActivated = false },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.HostAdmissionReady = false },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.GradingReady = false },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.BaselineSha256 = "" },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Round = nil },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Round.RoundId = server.NewId() },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Round.Canceled = true },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Round.ClosesAt = value.Round.OpensAt },
		func(value *controller.CompetitionSourceLifecycleStatus) {
			value.Intent.Commits["connect"] = strings.Repeat("2", 40)
		},
		func(value *controller.CompetitionSourceLifecycleStatus) { delete(value.Intent.Commits, "warp") },
		func(value *controller.CompetitionSourceLifecycleStatus) {
			value.Intent.Commits["config"] = strings.Repeat("2", 40)
		},
		func(value *controller.CompetitionSourceLifecycleStatus) {
			value.Intent.Manifest = append(value.Intent.Manifest, '\n')
		},
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Intent.TargetEpoch = 3 },
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Intent.Generation = 3 },
		func(value *controller.CompetitionSourceLifecycleStatus) {
			value.Intent.SourceDecisionDigest = strings.Repeat("2", 64)
		},
		func(value *controller.CompetitionSourceLifecycleStatus) { value.Intent.State = "abandoned" },
		func(value *controller.CompetitionSourceLifecycleStatus) {
			value.CompilerEvidenceSha256 = "synthetic-unverified"
		},
		func(value *controller.CompetitionSourceLifecycleStatus) {
			value.Intake.IntentSha256 = strings.Repeat("2", 64)
		},
	} {
		status := sourceIndexTestStatus(t, controller.CompetitionSourceLifecycleGrading)
		mutate(status)
		if _, err := newSourceIndexCheckpoint(status); err == nil {
			t.Fatalf("incomplete or inconsistent authority %d produced a public grading-ready label", index)
		}
	}
	status := sourceIndexTestStatus(t, controller.CompetitionSourceLifecycleIntake)
	status.Intake = nil
	if _, err := newSourceIndexCheckpoint(status); err == nil {
		t.Fatal("intake-only source lacked its authenticated purpose grant")
	}
	status = sourceIndexTestStatus(t, controller.CompetitionSourceLifecycleIntake)
	status.Purpose = controller.CompetitionSourceLifecyclePrepared
	if _, err := newSourceIndexCheckpoint(status); err == nil {
		t.Fatal("prepared label concealed an already-open intake round")
	}
	status = sourceIndexTestStatus(t, controller.CompetitionSourceLifecyclePrepared)
	status.BaselineSha256 = strings.Repeat("3", 64)
	if _, err := newSourceIndexCheckpoint(status); err == nil {
		t.Fatal("prepared source invented a baseline for a round that does not exist")
	}
}

// A fixture supplies only the new read capability. No mutation interface or
// fallback-to-activation hook is available to the exporter.
type sourceIndexLifecycleFixture struct {
	status *controller.CompetitionSourceLifecycleStatus
	err    error
	calls  int
}

// The requested exact generation is echoed through the authenticated snapshot.
func (self *sourceIndexLifecycleFixture) SourceLifecycleStatus(context.Context, *controller.Settings, int64) (*controller.CompetitionSourceLifecycleStatus, error) {
	self.calls++
	return self.status, self.err
}

// Old installed control code fails explicitly, without accepting current
// ActivatedAt or guessing whether a newer intake grant has opened a round.
func TestSourceIndexRequiresAuthenticatedPurposeCapabilityAndExactGeneration(t *testing.T) {
	if _, err := loadSourceIndexCheckpoint(context.Background(), nil, struct{}{}, 2); err == nil || !strings.Contains(err.Error(), "source-purpose capability") {
		t.Fatal("unsupported installed code inferred readiness from a legacy activation")
	}
	fixture := &sourceIndexLifecycleFixture{status: sourceIndexTestStatus(t, controller.CompetitionSourceLifecyclePrepared)}
	if _, err := loadSourceIndexCheckpoint(context.Background(), nil, fixture, 2); err != nil || fixture.calls != 1 {
		t.Fatal("authenticated exact-generation source preview failed", err)
	}
	if _, err := loadSourceIndexCheckpoint(context.Background(), nil, fixture, 3); err == nil {
		t.Fatal("another activation generation substituted for the requested source")
	}
	fixture.err = errors.New("synthetic-read-capability-unavailable")
	if _, err := loadSourceIndexCheckpoint(context.Background(), nil, fixture, 2); !errors.Is(err, fixture.err) {
		t.Fatal("unavailable trusted read was converted into an unsupported authority claim")
	}
}

// Command routing never permits a user file to masquerade as a trusted source
// purpose snapshot, and stdout-only preview remains main-environment restricted.
func TestSourceIndexCommandAcceptsOnlyReadOnlyMainScopedInputs(t *testing.T) {
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	for _, args := range [][]string{{"source-index", "--activation-generation=2"}, {"source-index", "--activation-generation=2", "--json"}} {
		opts, err := parser.ParseArgs(simulationUsage(), args, "synthetic")
		if err != nil || !optBool(opts, "source-index") {
			t.Fatal("source-index read-only command was not routed", err)
		}
	}
	if _, err := parser.ParseArgs(simulationUsage(), []string{"source-index", "--activation-generation=2", "--out=synthetic.md"}, "synthetic"); err == nil {
		t.Fatal("read-only source preview accepted a file-write option")
	}
	if err := validateEnvironment("source-index", "main"); err != nil {
		t.Fatal(err)
	}
	for _, environment := range []string{"local", "staging", ""} {
		if validateEnvironment("source-index", environment) == nil {
			t.Fatalf("trusted source-index read accepted %q environment", environment)
		}
	}
}
