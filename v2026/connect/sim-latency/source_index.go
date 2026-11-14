// Public source-index previews project authenticated purpose-specific facts.
// Nothing is fetched, published, activated or appended to repository files.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Optional capability keeps the exporter fail-closed when installed control
// code cannot authenticate intake grants and grading independently.
type sourceIndexLifecycleReader interface {
	SourceLifecycleStatus(context.Context, *controller.Settings, int64) (*controller.CompetitionSourceLifecycleStatus, error)
}

// Only public-safe fields cross this boundary. Private manifests, Git remotes,
// storage locations, runtime paths and seed/provider data are never copied.
type sourceIndexCheckpoint struct {
	Schema                          int                     `json:"schema"`
	Status                          string                  `json:"status"`
	Capability                      string                  `json:"capability"`
	Staging                         bool                    `json:"staging"`
	CompetitionId                   string                  `json:"competition_id"`
	SeasonId                        string                  `json:"season_id"`
	RoundId                         server.Id               `json:"round_id"`
	Epoch                           int                     `json:"epoch"`
	SourceGeneration                int                     `json:"source_generation"`
	ActivationGeneration            int64                   `json:"activation_generation"`
	Kind                            string                  `json:"kind"`
	Branch                          string                  `json:"branch"`
	SourceLockSha256                string                  `json:"source_lock_sha256"`
	Repositories                    []sourceIndexRepository `json:"repositories"`
	SelectedProductKind             string                  `json:"selected_product_kind,omitempty"`
	SelectedProductSourceLockSha256 string                  `json:"selected_product_source_lock_sha256,omitempty"`
	SelectedProductRepositories     []sourceIndexRepository `json:"selected_product_repositories,omitempty"`
	EvaluatorImageDigest            string                  `json:"evaluator_image_digest"`
	CompilerRequirement             string                  `json:"compiler_requirement"`
	CompilerVerified                bool                    `json:"compiler_verified"`
	CompilerEvidenceSha256          string                  `json:"compiler_evidence_sha256,omitempty"`
	InitialTakeoverMargin           float64                 `json:"initial_takeover_margin"`
	IntakeOpensAt                   *time.Time              `json:"intake_opens_at,omitempty"`
	IntakeClosesAt                  *time.Time              `json:"intake_closes_at,omitempty"`
	ApiAcknowledged                 bool                    `json:"api_acknowledged"`
	WorkerAcknowledged              bool                    `json:"worker_acknowledged"`
	GradingRuntimeActivated         bool                    `json:"grading_runtime_activated"`
	HostAdmissionReady              bool                    `json:"host_admission_ready"`
	GradingReady                    bool                    `json:"grading_ready"`
	BaselineSha256                  string                  `json:"baseline_sha256,omitempty"`
}

// Repository URLs are an owned public allowlist, never credential-bearing
// origin strings or caller-provided links to operational configuration.
type sourceIndexRepository struct {
	Directory string `json:"directory"`
	Url       string `json:"url"`
	Commit    string `json:"commit"`
}

// The one differently owned source repository retains its actual public URL.
func sourceIndexRepositoryUrl(repository string) (string, error) {
	switch repository {
	case "sn":
		return "https://github.com/urfoundation/sn", nil
	case "server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "warp", "gvisor":
		return "https://github.com/urnetwork/" + repository, nil
	default:
		return "", errors.New("source index repository is outside the complete frozen graph")
	}
}

// A legacy activation timestamp cannot stand in for this purpose capability.
// The reader is trusted control-plane code, never a caller-supplied JSON file.
func loadSourceIndexCheckpoint(ctx context.Context, settings *controller.Settings, store any, generation int64) (*sourceIndexCheckpoint, error) {
	if generation < 1 {
		return nil, errors.New("source-index requires a positive exact activation generation")
	}
	reader, supported := store.(sourceIndexLifecycleReader)
	if !supported {
		return nil, errors.New("source-index requires authenticated source-purpose capability; activation time alone cannot establish intake or grading readiness")
	}
	status, err := reader.SourceLifecycleStatus(ctx, settings, generation)
	if err != nil {
		return nil, err
	}
	if status == nil || status.Intent == nil || status.Intent.Generation != generation {
		return nil, errors.New("source-index lifecycle snapshot names a different generation")
	}
	return newSourceIndexCheckpoint(status)
}

// Native consistency checks supplement the authenticated database read. They
// refuse partial maps and unsupported purpose rather than infer missing facts.
func newSourceIndexCheckpoint(status *controller.CompetitionSourceLifecycleStatus) (*sourceIndexCheckpoint, error) {
	if status == nil || status.Schema != 1 || status.Capability != controller.CompetitionSourceLifecycleCapability || status.Intent == nil {
		return nil, errors.New("source index purpose capability is absent or unsupported")
	}
	intent := status.Intent
	if intent.AbandonedAt != nil || intent.State == "abandoned" || !credentialNamePattern.MatchString(intent.CompetitionId) || !credentialNamePattern.MatchString(intent.SeasonId) ||
		intent.TargetRoundId == (server.Id{}) || intent.Generation < 1 || intent.SourceGeneration < 0 || intent.TargetEpoch < 1 || intent.TargetEpoch > maximumCompetitionEpoch ||
		!validSha256(intent.IntentSha256) || !validSha256(intent.SourceDecisionDigest) || !validSha256(intent.SourceLockSha256) ||
		!strings.HasPrefix(intent.EvaluatorImageDigest, "sha256:") || !validSha256(strings.TrimPrefix(intent.EvaluatorImageDigest, "sha256:")) || !validSourceMargin(intent.Threshold*100, intent.Staging) {
		return nil, errors.New("source index activation identity is incomplete, abandoned or invalid")
	}
	lockDigest, err := model.CompetitionSourceLockDigestForMode(intent.Commits, intent.Staging)
	if err != nil || lockDigest != intent.SourceLockSha256 {
		return nil, errors.New("source index requires the complete exact ten-repository source lock")
	}
	manifestDigest := sha256.Sum256(intent.Manifest)
	if len(intent.Manifest) == 0 || hex.EncodeToString(manifestDigest[:]) != intent.ManifestSha256 {
		return nil, errors.New("source index manifest differs from its authenticated immutable bytes")
	}
	manifest, err := decodeSourceManifest(intent.Manifest)
	if err != nil || manifest.Schema != 2 || manifest.Staging != intent.Staging {
		return nil, errors.New("source index requires the exact explicit v2 source ledger")
	}
	round, err := manifest.sourceRound(intent.TargetEpoch)
	if err != nil || round.ActivationGeneration != intent.Generation || round.SourceGeneration != intent.SourceGeneration || round.SourceLockSha256 != lockDigest ||
		round.SourceDecisionDigest != intent.SourceDecisionDigest || !sourceMarginMatches(round.SignificantImprovementPercent, intent.Threshold) ||
		(intent.Kind != "baseline" && round.Kind != intent.Kind) || (intent.Kind == "baseline" && (intent.TargetEpoch != 1 || intent.SourceGeneration != 0)) {
		return nil, errors.New("source index epoch mapping differs from the selected activation")
	}
	generation, err := manifest.generation(intent.SourceGeneration)
	if err != nil || generation.SourceLockSha256 != lockDigest {
		return nil, errors.New("source index generation differs from its complete source graph")
	}
	result := &sourceIndexCheckpoint{Schema: 1, Capability: status.Capability, Staging: intent.Staging, CompetitionId: intent.CompetitionId,
		SeasonId: intent.SeasonId, RoundId: intent.TargetRoundId, Epoch: intent.TargetEpoch, SourceGeneration: intent.SourceGeneration,
		ActivationGeneration: intent.Generation, Kind: intent.Kind, Branch: sourceBranch(intent.Staging), SourceLockSha256: lockDigest,
		EvaluatorImageDigest: intent.EvaluatorImageDigest, CompilerRequirement: "go1.27.4", InitialTakeoverMargin: intent.Threshold,
		ApiAcknowledged: status.ApiAcknowledged, WorkerAcknowledged: status.WorkerAcknowledged, GradingRuntimeActivated: status.GradingRuntimeActivated,
		HostAdmissionReady: status.HostAdmissionReady, GradingReady: status.GradingReady, BaselineSha256: status.BaselineSha256,
		Repositories: []sourceIndexRepository{}}
	if status.CompilerEvidenceSha256 != "" {
		if !validSha256(status.CompilerEvidenceSha256) {
			return nil, errors.New("source index compiler evidence identity is invalid")
		}
		result.CompilerVerified, result.CompilerEvidenceSha256 = true, status.CompilerEvidenceSha256
	}
	if status.BaselineSha256 != "" && !validSha256(status.BaselineSha256) {
		return nil, errors.New("source index baseline digest is invalid")
	}
	if status.BaselineSha256 != "" && status.Round == nil {
		return nil, errors.New("source index cannot attach a scoring baseline to an absent round")
	}
	if status.Round != nil {
		actual := status.Round
		if actual.RoundId != intent.TargetRoundId || actual.Epoch != intent.TargetEpoch || actual.Canceled || actual.OpensAt.IsZero() || !actual.ClosesAt.After(actual.OpensAt) {
			return nil, errors.New("source index round schedule is absent, canceled or mismatched")
		}
		opensAt, closesAt := actual.OpensAt, actual.ClosesAt
		result.IntakeOpensAt, result.IntakeClosesAt = &opensAt, &closesAt
	}
	if status.Intake != nil {
		grant := status.Intake
		if model.ValidateCompetitionSourceIntakeActivation(grant) != nil || grant.Environment != intent.Environment || grant.CompetitionId != intent.CompetitionId ||
			grant.SeasonId != intent.SeasonId || grant.Generation != intent.Generation || grant.TargetEpoch != intent.TargetEpoch || grant.TargetRoundId != intent.TargetRoundId ||
			grant.ExpectedGeneration != intent.ExpectedGeneration || grant.IntentSha256 != intent.IntentSha256 {
			return nil, errors.New("source index intake purpose does not bind the exact activation")
		}
	}
	switch status.Purpose {
	case controller.CompetitionSourceLifecyclePrepared:
		if status.Round != nil || status.Intake != nil || status.GradingReady {
			return nil, errors.New("prepared source cannot conceal an existing intake or grading authority")
		}
		result.Status = "PREPARED"
	case controller.CompetitionSourceLifecycleIntake:
		if status.Intake == nil || status.Round == nil || !status.ApiAcknowledged || status.GradingReady {
			return nil, errors.New("intake-open source lacks its genuine source-purpose grant and API acknowledgment")
		}
		result.Status = "INTAKE_OPEN-evaluator_pending"
	case controller.CompetitionSourceLifecycleGrading:
		if status.Round == nil || !status.ApiAcknowledged || !status.WorkerAcknowledged || !status.GradingRuntimeActivated || !status.HostAdmissionReady ||
			!status.GradingReady || !validSha256(status.BaselineSha256) {
			return nil, errors.New("grading-ready source lacks actual runtime quorum, host admission or fresh same-round baseline")
		}
		result.Status = "GRADING_READY"
	default:
		return nil, errors.New("source index purpose is unknown; no activation/readiness inference is allowed")
	}
	for _, repository := range sourceRepositoryNamesForMode(intent.Staging) {
		url, err := sourceIndexRepositoryUrl(repository)
		if err != nil {
			return nil, err
		}
		result.Repositories = append(result.Repositories, sourceIndexRepository{Directory: repository, Url: url, Commit: intent.Commits[repository]})
	}
	if proof := generation.TrustedInstrument; proof != nil {
		result.SelectedProductKind, result.SelectedProductSourceLockSha256 = proof.SelectedProductKind, proof.SelectedProductSourceLockSha256
		result.SelectedProductRepositories = []sourceIndexRepository{}
		for _, repository := range sourceRepositoryNamesForMode(intent.Staging) {
			url, err := sourceIndexRepositoryUrl(repository)
			if err != nil {
				return nil, err
			}
			result.SelectedProductRepositories = append(result.SelectedProductRepositories, sourceIndexRepository{Directory: repository, Url: url, Commit: proof.SelectedProductCommits[repository]})
		}
	}
	return result, nil
}

// Markdown is only a preview for the owner's append-only public index update.
// Purpose, compiler evidence and actual baseline absence stay visible.
func renderSourceIndexMarkdown(checkpoint *sourceIndexCheckpoint) string {
	var text strings.Builder
	mode := "Production"
	if checkpoint.Staging {
		mode = "Staging"
	}
	fmt.Fprintf(&text, "## %s epoch %d — %s\n\n", mode, checkpoint.Epoch, checkpoint.Status)
	text.WriteString("Read-only authenticated checkpoint preview; this output does not publish source or activate grading.\n\n")
	fmt.Fprintf(&text, "| Identity | Value |\n|---|---|\n| Status | `%s` |\n| Competition | `%s` |\n| Season | `%s` |\n| Round | `%s` |\n| Source / activation generation | `%d` / `%d` |\n| Source choice | `%s` |\n| Evaluation branch | `%s`; exact commits below, never moving heads |\n| Canonical source-lock SHA-256 | `%s` |\n| Evaluator image identity | `%s` |\n| Compiler requirement | `%s` |\n| Compiler independently verified | `%t` |\n| Initial takeover margin | `%s` |\n",
		checkpoint.Status, checkpoint.CompetitionId, checkpoint.SeasonId, checkpoint.RoundId.String(), checkpoint.SourceGeneration, checkpoint.ActivationGeneration,
		checkpoint.Kind, checkpoint.Branch, checkpoint.SourceLockSha256, checkpoint.EvaluatorImageDigest, checkpoint.CompilerRequirement,
		checkpoint.CompilerVerified, strconv.FormatFloat(checkpoint.InitialTakeoverMargin, 'g', -1, 64))
	if checkpoint.CompilerVerified {
		fmt.Fprintf(&text, "| Compiler evidence SHA-256 | `%s` |\n", checkpoint.CompilerEvidenceSha256)
	}
	if checkpoint.IntakeOpensAt != nil {
		fmt.Fprintf(&text, "| Actual intake opens / closes | `%s` / `%s` |\n", checkpoint.IntakeOpensAt.UTC().Format(time.RFC3339Nano), checkpoint.IntakeClosesAt.UTC().Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&text, "| API / worker acknowledged | `%t` / `%t` |\n| Grading runtime activated | `%t` |\n| Host admission ready | `%t` |\n| Grading ready | `%t` |\n",
		checkpoint.ApiAcknowledged, checkpoint.WorkerAcknowledged, checkpoint.GradingRuntimeActivated, checkpoint.HostAdmissionReady, checkpoint.GradingReady)
	if checkpoint.BaselineSha256 == "" {
		text.WriteString("| Same-round scoring baseline | not established; no baseline is claimed |\n")
	} else {
		fmt.Fprintf(&text, "| Immutable same-round baseline SHA-256 | `%s` |\n", checkpoint.BaselineSha256)
	}
	text.WriteString("\n| Mono-root directory / repository | Exact commit |\n|---|---|\n")
	for _, repository := range checkpoint.Repositories {
		fmt.Fprintf(&text, "| [%s](%s) | [`%s`](%s/commit/%s) |\n", repository.Directory, repository.Url, repository.Commit, repository.Url, repository.Commit)
	}
	if len(checkpoint.SelectedProductRepositories) != 0 {
		fmt.Fprintf(&text, "\n### Selected product before the separate instrument child\n\nKind: `%s`. Canonical source-lock SHA-256: `%s`. These product parents are not the evaluator's instrumented commits above.\n\n| Mono-root directory / repository | Exact selected product commit |\n|---|---|\n",
			checkpoint.SelectedProductKind, checkpoint.SelectedProductSourceLockSha256)
		for _, repository := range checkpoint.SelectedProductRepositories {
			fmt.Fprintf(&text, "| [%s](%s) | [`%s`](%s/commit/%s) |\n", repository.Directory, repository.Url, repository.Commit, repository.Url, repository.Commit)
		}
	}
	return text.String()
}

// The sole output is a public-safe stdout preview. Unsupported installed code
// fails closed without local file updates, network fetches or approval writes.
func runSourceIndex(opts docopt.Opts) {
	generation, err := strconv.ParseInt(optString(opts, "--activation-generation", ""), 10, 64)
	if err != nil || generation < 1 {
		fatalf("source-index requires a positive exact --activation-generation")
	}
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("source-index control scope: %s", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	checkpoint, err := loadSourceIndexCheckpoint(ctx, settings, store, generation)
	if err != nil {
		fatalf("source-index authenticated purpose: %s", err)
	}
	if optBool(opts, "--json") {
		content, err := json.MarshalIndent(checkpoint, "", "  ")
		if err != nil {
			fatalf("source-index public projection: %s", err)
		}
		fmt.Printf("%s\n", content)
		return
	}
	fmt.Print(renderSourceIndexMarkdown(checkpoint))
}
