// Wires the durable v2 reconciler to private Git staging and an explicitly
// pinned deployment/preparation adapter. Nothing activates by default.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Keeps trusted deployment programs outside candidate workspaces. Their hashes
// are prospectively configured, never read from a submission or review bundle.
type sourceSupervisorRuntime struct {
	settings         *controller.Settings
	store            controller.PostgresStore
	stateDirectory   string
	repositoriesRoot string
	sourceConfig     string
	workerCommand    string
	workerSha256     string
	prepareCommand   string
	prepareSha256    string
}

// Runs a bounded, cancellation-aware trusted integration without a shell.
func runSourceSupervisorCommand(ctx context.Context, path, expectedDigest string, request any) error {
	_, err := runSourceSupervisorProgram(ctx, path, expectedDigest, request, nil)
	return err
}

// The optional environment is an allowlist for isolated candidate-build
// brokers; candidate code must never inherit the main control-plane environment.
func runSourceSupervisorProgram(ctx context.Context, path, expectedDigest string, request any, environment []string) ([]byte, error) {
	return runSourceSupervisorProgramArguments(ctx, path, expectedDigest, request, environment, nil)
}

// Arguments are fixed trusted protocol selectors, never strings from a review
// or candidate. SIGTERM gives the worker time to join containment cleanup.
func runSourceSupervisorProgramArguments(ctx context.Context, path, expectedDigest string, request any, environment, arguments []string) ([]byte, error) {
	if !filepath.IsAbs(path) || !validSha256(expectedDigest) {
		return nil, errors.New("pinned automation integration is not configured")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return nil, errors.New("automation integration must be a non-writable regular executable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	_, hashErr := io.Copy(hash, io.LimitReader(file, 64*1024*1024+1))
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil || info.Size() > 64*1024*1024 || hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return nil, errors.New("automation integration executable digest does not match")
	}
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, arguments...)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 30 * time.Second
	command.Env = environment
	command.Stdin = bytes.NewReader(requestBytes)
	output := &sourceBoundedWriter{limit: 1024 * 1024}
	command.Stdout = output
	command.Stderr = &sourceBoundedWriter{limit: 64 * 1024}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("trusted automation integration failed: %w", err)
	}
	return output.content, nil
}

// Loads only the already reviewed pointer and stages an explicit source-only
// carry-forward at the deadline. No review completion is needed or fabricated.
func (self *sourceSupervisorRuntime) chooseSource(ctx context.Context, lease model.CompetitionSupervisorLease, snapshot *controller.CompetitionSupervisorSnapshot, transition model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	current := snapshot.CurrentSource
	if current == nil {
		return nil, errors.New("initial reviewed source activation must be explicitly provisioned before automation")
	}
	if err := validateSourceActivationMode(current, self.settings); err != nil {
		return nil, err
	}
	if transition.TargetEpoch == 1 {
		if current.TargetEpoch != 1 || current.State != "active" {
			return nil, errors.New("initial source pointer is not active for epoch one")
		}
		return current, nil
	}
	// A crash after the database intent but before the checkpoint resumes that
	// same intent. It does not select current remote heads or restage commits.
	existing, err := self.store.SourceActivationForTarget(ctx, self.settings, transition.TargetEpoch)
	if err == nil && existing != nil {
		if err := validateSourceActivationMode(existing, self.settings); err != nil {
			return nil, err
		}
		if existing.TargetEpoch != transition.TargetEpoch || existing.ExpectedGeneration != current.Generation {
			return nil, errors.New("pending source intent conflicts with the current successor")
		}
		return existing, nil
	}
	if err != nil && !errors.Is(err, controller.ErrNotFound) {
		return nil, err
	}
	generation, err := self.store.NextSourceActivationGeneration(ctx, self.settings)
	if err != nil {
		return nil, err
	}
	finalRoot := filepath.Join(self.stateDirectory, "source-"+strconv.FormatInt(generation, 10))
	intentPath := filepath.Join(finalRoot, "activation.json")
	if content, readErr := readSourceFile(intentPath); readErr == nil {
		var intent model.CompetitionSourceActivation
		if err := decodeStrictJSONBytes(content, &intent, "source activation recovery"); err != nil {
			return nil, err
		}
		if intent.Generation != generation || intent.TargetEpoch != transition.TargetEpoch || intent.ExpectedGeneration != current.Generation {
			return nil, errors.New("staged activation has a conflicting identity")
		}
		if err := validateSourceActivationMode(&intent, self.settings); err != nil {
			return nil, err
		}
		return &intent, nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	if _, err := os.Lstat(finalRoot); err == nil {
		return nil, errors.New("incomplete private staging exists without a sealed intent; retain evidence and repair before retry")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Unpublished incomplete attempts remain private audit material. Only a
	// complete, fsynced directory is atomically installed at the recovery path.
	root, err := os.MkdirTemp(self.stateDirectory, ".source-"+strconv.FormatInt(generation, 10)+"-")
	if err != nil {
		return nil, err
	}
	manifest := &sourceManifest{}
	decoder := yaml.NewDecoder(bytes.NewReader(current.Manifest))
	decoder.KnownFields(true)
	if err := decoder.Decode(manifest); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("active source manifest has trailing yaml content")
	}
	if err := validateSourceManifest(manifest); err != nil {
		return nil, err
	}
	if manifest.Schema != 2 || manifest.Staging != current.Staging {
		return nil, errors.New("v2 supervisor requires an explicitly provisioned v2 baseline manifest")
	}
	lockedGeneration, err := manifest.generation(current.SourceGeneration)
	if err != nil || lockedGeneration.SourceLockSha256 != current.SourceLockSha256 || lockedGeneration.SignificantImprovementPercent != current.Threshold*100 {
		return nil, errors.New("active source pointer disagrees with its immutable manifest")
	}
	reason, rewardDigest := "review_pending", ""
	if snapshot.Round != nil {
		result, err := self.store.SealAndElect(ctx, self.settings, snapshot.Round.RoundId)
		if err != nil {
			return nil, err
		}
		if len(result.Decisions) == 0 {
			return nil, errors.New("sealed round has no authoritative reward revision")
		}
		decision := result.Decisions[len(result.Decisions)-1]
		rewardDigest = decision.DecisionSha256
		if decision.SelectedJobId == nil {
			reason = "no_reward_winner"
		}
	}
	preparedAt := time.Now().UTC()
	intent := &model.CompetitionSourceActivation{
		Environment: current.Environment, CompetitionId: current.CompetitionId, SeasonId: current.SeasonId,
		Staging: current.Staging, StagingRoundWindowSeconds: current.StagingRoundWindowSeconds, StagingPreparationSeconds: current.StagingPreparationSeconds,
		Generation: generation, ExpectedGeneration: current.Generation, SourceGeneration: current.SourceGeneration, TargetEpoch: transition.TargetEpoch,
		TargetRoundId: server.NewId(),
		Kind:          "reviewed_source_carry_forward", Reason: reason, BaseSourceLockSha256: current.SourceLockSha256, SourceLockSha256: current.SourceLockSha256,
		Commits: current.Commits, RewardDecisionDigest: rewardDigest, Threshold: current.Threshold, ThresholdEvidenceSha256: current.ThresholdEvidenceSha256,
		EvaluatorImageDigest: current.EvaluatorImageDigest, HostQualificationSha256: current.HostQualificationSha256, PreparedAt: preparedAt,
		GitRefs: map[string]model.CompetitionSourceGitRef{},
	}
	var proposal *reviewedSourceProposal
	if transition.Reason == "safe_carry_forward_after_unpublished_promotion" {
		intent.Reason = "readiness_fallback"
	} else {
		proposal, err = self.readySourceProposal(ctx, current, transition.SourceChoiceAt)
		if err != nil {
			return nil, err
		}
	}
	selectedMargin := lockedGeneration.SignificantImprovementPercent
	if proposal != nil {
		intent.Kind, intent.Reason = "reviewed_source_promotion", "ready_reviewed_source"
		intent.SourceGeneration = current.SourceGeneration + 1
		intent.SourceLockSha256, intent.Commits = proposal.SourceLockSha256, proposal.Commits
		intent.ReviewId, intent.ReviewDecisionDigest = &proposal.ReviewId, proposal.ReviewDecisionDigest
		intent.ApprovedPatchSha256 = proposal.PatchSha256
		intent.EvaluatedImageDigest = proposal.EvaluatedImageDigest
		intent.EvaluatorImageDigest, intent.HostQualificationSha256 = proposal.BuildReceipt.EvaluatorImageDigest, proposal.BuildReceipt.HostQualificationSha256
		intent.BuildReadyAt = proposal.ReadyAt
		if !intent.Staging {
			selectedMargin = proposal.Significance.RecommendedNextEpochTakeoverMarginPercent
		}
		intent.Threshold, intent.ThresholdEvidenceSha256 = selectedMargin/100, proposal.Significance.ScoreSha256
		manifest.EvaluationSource.Generations = append(manifest.EvaluationSource.Generations, reviewedSourceGeneration{
			Generation: intent.SourceGeneration, Kind: intent.Kind, SourceLockSha256: intent.SourceLockSha256,
			SignificantImprovementPercent: selectedMargin, ThresholdEvidenceSha256: intent.ThresholdEvidenceSha256,
			SourceApprovalId: proposal.ReviewId.String(), SourceApprovalDigest: proposal.ReviewDecisionDigest, EvaluatedBaseSourceLockSha256: current.SourceLockSha256,
			PatchSha256: proposal.PatchSha256, ScoreSha256: proposal.Significance.ScoreSha256, Significance: proposal.Significance, Repositories: sourceRepositoriesFromCommits(proposal.Commits),
		})
		if err := copyReviewedSourcePatch(proposal, root); err != nil {
			return nil, err
		}
	}
	intent.SourceDecisionDigest, err = model.CompetitionAutomationDigest(struct {
		Staging              bool      `json:"staging,omitempty"`
		Generation           int64     `json:"generation"`
		TargetEpoch          int       `json:"target_epoch"`
		SourceLockSha256     string    `json:"source_lock_sha256"`
		RewardDecisionDigest string    `json:"reward_decision_digest"`
		Kind                 string    `json:"kind"`
		TargetRoundId        server.Id `json:"target_round_id"`
	}{Staging: intent.Staging, Generation: generation, TargetEpoch: intent.TargetEpoch, SourceLockSha256: intent.SourceLockSha256, RewardDecisionDigest: rewardDigest, Kind: intent.Kind, TargetRoundId: intent.TargetRoundId})
	if err != nil {
		return nil, err
	}
	manifest.EvaluationSource.Rounds = append(manifest.EvaluationSource.Rounds, reviewedSourceRound{
		Epoch: intent.TargetEpoch, SourceGeneration: intent.SourceGeneration, ActivationGeneration: intent.Generation,
		SourceDecisionDigest: intent.SourceDecisionDigest, Kind: intent.Kind, Reason: intent.Reason, RewardDecisionDigest: rewardDigest,
		SourceLockSha256: intent.SourceLockSha256, SignificantImprovementPercent: selectedMargin,
	})
	if err := validateSourceManifest(manifest); err != nil {
		return nil, err
	}
	manifestBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	intent.Manifest, intent.ManifestSha256 = manifestBytes, hex.EncodeToString(manifestDigest[:])
	publication := &sourcePublicationIntent{Schema: 2, Staging: intent.Staging, ActivationGeneration: generation, SourceDecisionDigest: intent.SourceDecisionDigest, ManifestSha256: intent.ManifestSha256}
	if proposal != nil {
		publication.ApprovedPatchSha256, publication.ApprovedPatchPath = proposal.PatchSha256, filepath.Join(finalRoot, "canonical.patch")
	}
	for _, repository := range sourceRepositoryNames() {
		stagedRoot := filepath.Join(root, repository)
		origin, err := sourceGitText(ctx, filepath.Join(self.repositoriesRoot, repository), "remote", "get-url", "origin")
		if err != nil {
			return nil, err
		}
		cloneSource := origin
		if proposal != nil {
			cloneSource = proposal.StagedRoots[repository]
			if !filepath.IsAbs(cloneSource) {
				return nil, errors.New("reviewed proposal lost its private source clone")
			}
		}
		if _, err := sourceGitText(ctx, root, "-c", "core.fsync=all", "clone", "--quiet", "--no-checkout", "--no-hardlinks", cloneSource, stagedRoot); err != nil {
			return nil, err
		}
		if _, err := sourceGitText(ctx, stagedRoot, "remote", "set-url", "origin", origin); err != nil {
			return nil, err
		}
		if err := os.Chmod(stagedRoot, 0o700); err != nil {
			return nil, err
		}
		if _, err := sourceGitText(ctx, stagedRoot, "checkout", "--quiet", "--detach", intent.Commits[repository]); err != nil {
			return nil, err
		}
		ref := model.CompetitionSourceGitRef{Branch: sourceBranch(intent.Staging), ExpectedCommit: current.Commits[repository], Commit: intent.Commits[repository]}
		intent.GitRefs[repository] = ref
		publication.Refs = append(publication.Refs, sourcePublicationRef{Repository: repository, Branch: ref.Branch, ExpectedCommit: ref.ExpectedCommit, Commit: ref.Commit, StagedRoot: filepath.Join(finalRoot, repository)})
	}
	configRoot, err := sourceGitText(ctx, filepath.Dir(self.sourceConfig), "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	configOrigin, err := sourceGitText(ctx, configRoot, "remote", "get-url", "origin")
	if err != nil {
		return nil, err
	}
	stagedConfig := filepath.Join(root, "config")
	intent.ConfigExpectedCommit, intent.ManifestCommit, err = stageFreshSourceConfig(ctx, configOrigin, stagedConfig, current, manifestBytes, fmt.Sprintf("competition: %s for epoch %d", intent.Kind, intent.TargetEpoch))
	if err != nil {
		return nil, err
	}
	intent.GitRefs["config"] = model.CompetitionSourceGitRef{Branch: "main", ExpectedCommit: intent.ConfigExpectedCommit, Commit: intent.ManifestCommit}
	publication.Refs = append(publication.Refs, sourcePublicationRef{Repository: "config", Branch: "main", ExpectedCommit: intent.ConfigExpectedCommit, Commit: intent.ManifestCommit, StagedRoot: filepath.Join(finalRoot, "config")})
	journalPath := filepath.Join(root, "publication.json")
	if err := saveSourcePublicationIntent(journalPath, publication); err != nil {
		return nil, err
	}
	journalBytes, err := readSourceFile(journalPath)
	if err != nil {
		return nil, err
	}
	journalDigest := sha256.Sum256(journalBytes)
	intent.PublicationJournalSha256 = hex.EncodeToString(journalDigest[:])
	intentBytes, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writePrivateReviewFile(filepath.Join(root, "activation.json"), append(intentBytes, '\n')); err != nil {
		return nil, err
	}
	rootFile, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	syncErr := rootFile.Sync()
	closeErr := rootFile.Close()
	if syncErr != nil {
		return nil, syncErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if _, err := self.store.RenewAutomationLease(ctx, self.settings, lease, time.Minute); err != nil {
		return nil, err
	}
	if err := os.Rename(root, finalRoot); err != nil {
		return nil, err
	}
	stateFile, err := os.Open(self.stateDirectory)
	if err != nil {
		return nil, err
	}
	defer stateFile.Close()
	if err := stateFile.Sync(); err != nil {
		return nil, err
	}
	return intent, nil
}

// Replays exact refs and records durable local receipts. The database rechecks
// source approval each time, including after remote pushes but before activation.
func (self *sourceSupervisorRuntime) publishSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	if err := validateSourceActivationMode(intent, self.settings); err != nil {
		return err
	}
	if intent.State == "abandoned" {
		return errors.New("source publication intent was durably abandoned")
	}
	root := filepath.Join(self.stateDirectory, "source-"+strconv.FormatInt(intent.Generation, 10))
	publication, err := loadSourcePublicationIntent(filepath.Join(root, "publication.json"), intent.PublicationJournalSha256)
	if err != nil {
		return err
	}
	if publication.Staging != intent.Staging || publication.ActivationGeneration != intent.Generation || publication.SourceDecisionDigest != intent.SourceDecisionDigest || publication.ManifestSha256 != intent.ManifestSha256 || publication.ApprovedPatchSha256 != intent.ApprovedPatchSha256 {
		return errors.New("publication journal is not the authenticated source intent")
	}
	if publication.Bootstrap != (intent.Kind == "baseline") {
		return errors.New("publication bootstrap flag differs from authorized source kind")
	}
	for _, ref := range publication.Refs {
		expected, ok := intent.GitRefs[ref.Repository]
		if !ok || expected.Branch != ref.Branch || expected.ExpectedCommit != ref.ExpectedCommit || expected.Commit != ref.Commit {
			return errors.New("publication ref differs from durable expected-ref intent")
		}
	}
	backend := &gitSourcePublicationBackend{
		patchPolicy: self.settings.PatchPolicy,
		authorize: func(ctx context.Context, _ *sourcePublicationIntent) error {
			if _, err := self.store.RenewAutomationLease(ctx, self.settings, lease, time.Minute); err != nil {
				return err
			}
			_, err := self.store.RecordSourceIntent(ctx, self.settings, lease, *intent)
			return err
		},
		receipt: func(_ context.Context, ref sourcePublicationRef, _ *sourcePublicationIntent) error {
			path := filepath.Join(root, "published-"+ref.Repository+".json")
			content, err := json.Marshal(ref)
			if err != nil {
				return err
			}
			if existing, err := os.ReadFile(path); err == nil {
				if !bytes.Equal(existing, content) {
					return errors.New("publication receipt conflicts")
				}
				return nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return writePrivateReviewFile(path, content)
		},
	}
	return reconcileSourcePublication(ctx, publication, backend)
}

// Only an entirely unpublished promotion may be abandoned. The proof is
// retained before the fenced database operation; any advanced ref leaves the
// original intent blocked/retryable and can never trigger a remote rewind.
func (self *sourceSupervisorRuntime) fallbackSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, reason string) (bool, error) {
	if err := validateSourceActivationMode(intent, self.settings); err != nil {
		return false, err
	}
	if intent.Kind != "reviewed_source_promotion" {
		return false, nil
	}
	if intent.State == "abandoned" {
		return true, nil
	}
	root := filepath.Join(self.stateDirectory, "source-"+strconv.FormatInt(intent.Generation, 10))
	publication, err := loadSourcePublicationIntent(filepath.Join(root, "publication.json"), intent.PublicationJournalSha256)
	if err != nil {
		return false, err
	}
	if publication.Staging != intent.Staging || publication.ActivationGeneration != intent.Generation || publication.SourceDecisionDigest != intent.SourceDecisionDigest || publication.ManifestSha256 != intent.ManifestSha256 || publication.Bootstrap {
		return false, errors.New("fallback journal differs from authenticated source intent")
	}
	for _, ref := range publication.Refs {
		expected, ok := intent.GitRefs[ref.Repository]
		if !ok || ref.ExpectedCommit != expected.ExpectedCommit || ref.Commit != expected.Commit || ref.Branch != expected.Branch {
			return false, errors.New("fallback ref differs from durable expected-ref intent")
		}
	}
	proof, err := probeUnpublishedSource(ctx, publication, &gitSourcePublicationBackend{})
	if err != nil {
		return false, err
	}
	proofPath := filepath.Join(root, "unpublished-proof.json")
	if existing, err := readSourceFile(proofPath); err == nil {
		if !bytes.Equal(existing, proof) {
			return false, errors.New("fallback proof conflicts with retained evidence")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	} else if err := writePrivateReviewFile(proofPath, proof); err != nil {
		return false, err
	}
	directory, err := os.Open(root)
	if err != nil {
		return false, err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256(proof)
	if err := self.store.AbandonSourceIntent(ctx, self.settings, lease, intent.Generation, reason, hex.EncodeToString(digest[:])); err != nil {
		return false, err
	}
	return true, nil
}

// Deployment/preparation must install exact runtime envelopes, obtain each API
// acknowledgment, and independently qualify the worker. Missing integration
// leaves a durable preparation_blocked checkpoint instead of opening a round.
func (self *sourceSupervisorRuntime) prepareSource(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation) error {
	if err := validateSourceActivationMode(intent, self.settings); err != nil {
		return err
	}
	if _, err := self.store.RenewAutomationLease(ctx, self.settings, lease, time.Minute); err != nil {
		return err
	}
	return runSourceSupervisorCommand(ctx, self.prepareCommand, self.prepareSha256, struct {
		Operation string                             `json:"operation"`
		Intent    *model.CompetitionSourceActivation `json:"intent"`
		Lease     model.CompetitionSupervisorLease   `json:"lease"`
	}{Operation: "propagate_and_qualify", Intent: intent, Lease: lease})
}

// Starts only after explicit enablement and frozen v2 policy approval. Command
// digests are deployment settings, never content provided by a candidate.
func runSourceSupervisor(opts docopt.Opts) {
	if !optBool(opts, "--enable-automation") {
		fatalf("supervisor is disabled; explicit --enable-automation and approved v2 configuration are required")
	}
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("supervisor settings: %s", err)
	}
	if !settings.AutomationPolicy.Enabled {
		fatalf("competition automation policy is disabled")
	}
	stateDirectory, err := filepath.Abs(optString(opts, "--state-dir", ""))
	if err != nil || optString(opts, "--state-dir", "") == "" {
		fatalf("supervisor requires --state-dir")
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		fatalf("supervisor state: %s", err)
	}
	info, err := os.Lstat(stateDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		fatalf("supervisor state directory must be private")
	}
	sourceConfig, repositoriesRoot, sourceLock, err := configuredSourcePathsForMode(opts, settings.AutomationPolicy.Staging)
	if err != nil || sourceLock != "" {
		fatalf("supervisor requires host-only source paths: %v", err)
	}
	runtime := &sourceSupervisorRuntime{
		settings: settings, store: store, stateDirectory: stateDirectory, repositoriesRoot: repositoriesRoot, sourceConfig: sourceConfig,
		workerCommand: optString(opts, "--worker-command", ""), workerSha256: optString(opts, "--worker-sha256", ""),
		prepareCommand: optString(opts, "--prepare-command", ""), prepareSha256: optString(opts, "--prepare-sha256", ""),
	}
	driver := &controller.PostgresCompetitionSupervisorDriver{Settings: settings, Store: store, Hooks: controller.CompetitionSupervisorHooks{
		ChooseSource: runtime.chooseSource, PublishSource: runtime.publishSource, PrepareSource: runtime.prepareSource, FallbackSource: runtime.fallbackSource,
		RunWorker: func(ctx context.Context, lease model.CompetitionSupervisorLease, round model.CompetitionRoundResult) error {
			_, err := runSourceSupervisorProgramArguments(ctx, runtime.workerCommand, runtime.workerSha256, controller.CompetitionSupervisorWorkerRequest{Operation: "run_one_epoch", Round: round, Lease: lease}, nil, []string{"--supervisor-request"})
			return err
		},
	}}
	supervisor := &controller.CompetitionSupervisor{Enabled: true, OwnerId: optString(opts, "--owner-id", ""), Driver: driver}
	ctx, cancel := signalContext()
	defer cancel()
	report := func(state string, err error) {
		result := struct {
			State string `json:"state"`
			Error string `json:"error,omitempty"`
		}{State: state}
		if err != nil {
			result.Error = err.Error()
		}
		encoded, _ := json.Marshal(result)
		fmt.Printf("%s\n", encoded)
	}
	if optBool(opts, "--once") {
		state, err := supervisor.Reconcile(ctx)
		report(state, err)
		if err != nil {
			fatalf("supervisor: %s", err)
		}
		return
	}
	if err := supervisor.Run(ctx, report); err != nil && !errors.Is(err, context.Canceled) {
		fatalf("supervisor: %s", err)
	}
}
