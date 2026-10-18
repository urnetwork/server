// Explicitly registers an approved, already published baseline for the same
// runtime-readiness and preparation gates used by every later v2 source.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"gopkg.in/yaml.v3"
)

// Checks against prospectively approved source/image policy, not a fabricated
// review. A bootstrap may not add a promotion or target a later round.
func validateBootstrapSource(manifest *sourceManifest, settings *controller.Settings) error {
	if manifest == nil || settings == nil || manifest.Schema != 2 || manifest.Staging != settings.AutomationPolicy.Staging ||
		!settings.AutomationPolicy.Enabled || !validSha256(settings.AutomationPolicy.PolicyApprovalSha256) ||
		len(manifest.EvaluationSource.Generations) != 1 || len(manifest.EvaluationSource.Rounds) != 1 {
		return errors.New("bootstrap requires an approved v2 baseline-only manifest")
	}
	if err := validateSourceManifest(manifest); err != nil {
		return err
	}
	generation, round := manifest.EvaluationSource.Generations[0], manifest.EvaluationSource.Rounds[0]
	if generation.Generation != 0 || generation.Kind != "baseline" || generation.SourceLockSha256 != settings.AutomationPolicy.SourceLockSha256 ||
		generation.Repositories.Server.Commit != settings.BaseSha || generation.SignificantImprovementPercent != settings.EvaluationPolicy.TakeoverMargin*100 ||
		round.Epoch != 1 || round.SourceGeneration != 0 || round.ActivationGeneration != 1 || settings.AutomationPolicy.SourceActivationGeneration != 1 {
		return errors.New("baseline manifest differs from the approved epoch-one policy")
	}
	return nil
}

// Attaches only the persisted epoch-one baseline to an untouched source-choice
// checkpoint. Publication, runtime readiness and preparation remain pending.
func bootstrapSourceTransition(intent *model.CompetitionSourceActivation, previous *model.CompetitionSupervisorTransition) (*model.CompetitionSupervisorTransition, bool, error) {
	if intent == nil || intent.Kind != "baseline" || intent.Generation != 1 || intent.ExpectedGeneration != 0 || intent.SourceGeneration != 0 ||
		intent.TargetEpoch != 1 || intent.TargetRoundId == (server.Id{}) || intent.PreparedAt.IsZero() || intent.AbandonedAt != nil || !validSha256(intent.IntentSha256) {
		return nil, false, errors.New("bootstrap checkpoint requires the exact persisted epoch-one baseline")
	}
	if previous == nil {
		return &model.CompetitionSupervisorTransition{TargetEpoch: 1, ExpectedRevision: 0, State: "source_publication_pending", SourceActivationGeneration: intent.Generation,
			SourceChoiceAt: intent.PreparedAt, PreparationStartedAt: intent.PreparedAt}, true, nil
	}
	if previous.TargetEpoch != intent.TargetEpoch || previous.Revision < 1 ||
		(previous.RoundId != nil && *previous.RoundId != intent.TargetRoundId) {
		return nil, false, errors.New("bootstrap checkpoint belongs to a conflicting epoch or round")
	}
	if previous.SourceActivationGeneration == intent.Generation {
		return previous, false, nil
	}
	if previous.SourceActivationGeneration != 0 || previous.State != "source_choice_pending" || previous.RoundId != nil ||
		!previous.PreparationStartedAt.IsZero() || !previous.OpensAt.IsZero() || previous.SourceChoiceAt.IsZero() {
		return nil, false, errors.New("bootstrap cannot replace an already selected or prepared source checkpoint")
	}
	next := *previous
	next.ExpectedRevision, next.State, next.Reason = previous.Revision, "source_publication_pending", ""
	next.SourceActivationGeneration, next.PreparationStartedAt = intent.Generation, intent.PreparedAt
	return &next, true, nil
}

// Stages recovery material and a durable checkpoint, never an active pointer.
// Repeating this command resumes the exact explicitly requested config commit.
func runBootstrapSource(opts docopt.Opts) {
	if !optBool(opts, "--enable-automation") {
		fatalf("bootstrap requires explicit automation enablement")
	}
	settings, store, err := loadCompetitionControl()
	if err != nil {
		fatalf("baseline settings: %s", err)
	}
	if err := model.ValidateCompetitionAutomationPolicy(settings.AutomationPolicy); err != nil || !settings.AutomationPolicy.Enabled {
		fatalf("approved v2 baseline policy is required: %v", err)
	}
	manifestCommit := optString(opts, "--manifest-commit", "")
	if !sourceGitShaPattern.MatchString(manifestCommit) {
		fatalf("bootstrap requires an exact --manifest-commit")
	}
	ctx, cancel := signalContext()
	defer cancel()
	lease, err := store.AcquireAutomationLease(ctx, settings, optString(opts, "--owner-id", ""), 5*time.Minute)
	if err != nil {
		fatalf("bootstrap lease: %s", err)
	}
	if current, err := store.CurrentSourceActivation(ctx, settings); err == nil && current != nil {
		fatalf("bootstrap cannot replace an active source pointer")
	} else if err != nil && !errors.Is(err, controller.ErrNotFound) {
		fatalf("baseline pointer: %s", err)
	}
	stateDirectory, err := filepath.Abs(optString(opts, "--state-dir", ""))
	if err != nil || optString(opts, "--state-dir", "") == "" {
		fatalf("bootstrap requires --state-dir")
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		fatalf("baseline state: %s", err)
	}
	info, err := os.Lstat(stateDirectory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || info.Mode()&os.ModeSymlink != 0 {
		fatalf("baseline state directory must be private")
	}
	finalRoot := filepath.Join(stateDirectory, "source-1")
	intent, err := store.SourceActivation(ctx, settings, 1)
	if err != nil && !errors.Is(err, controller.ErrNotFound) {
		fatalf("baseline intent: %s", err)
	}
	if intent == nil {
		if content, err := readSourceFile(filepath.Join(finalRoot, "activation.json")); err == nil {
			intent = &model.CompetitionSourceActivation{}
			if err := decodeStrictJSONBytes(content, intent, "baseline recovery"); err != nil {
				fatalf("baseline recovery: %s", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			fatalf("baseline recovery: %s", err)
		}
	}
	if intent == nil {
		sourceConfig, repositoriesRoot, lock, err := configuredSourcePathsForMode(opts, settings.AutomationPolicy.Staging)
		if err != nil || lock != "" {
			fatalf("bootstrap requires host source paths: %v", err)
		}
		configRoot, err := sourceGitText(ctx, filepath.Dir(sourceConfig), "rev-parse", "--show-toplevel")
		if err != nil {
			fatalf("baseline config: %s", err)
		}
		manifestBytes, err := sourceGitBlobAtCommit(ctx, configRoot, manifestCommit, sourceManifestRepositoryPath(settings.AutomationPolicy.Staging), maximumSourceConfigBytes)
		if err != nil {
			fatalf("baseline exact config: %s", err)
		}
		manifest := &sourceManifest{}
		decoder := yaml.NewDecoder(bytes.NewReader(manifestBytes))
		decoder.KnownFields(true)
		if err := decoder.Decode(manifest); err != nil {
			fatalf("baseline manifest: %s", err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			fatalf("baseline manifest has trailing yaml")
		}
		if err := validateBootstrapSource(manifest, settings); err != nil {
			fatalf("approved baseline: %s", err)
		}
		root, err := os.MkdirTemp(stateDirectory, ".baseline-source-")
		if err != nil {
			fatalf("baseline staging: %s", err)
		}
		generation, round := manifest.EvaluationSource.Generations[0], manifest.EvaluationSource.Rounds[0]
		digest := sha256.Sum256(manifestBytes)
		intent = &model.CompetitionSourceActivation{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
			Staging: settings.AutomationPolicy.Staging, StagingRoundWindowSeconds: settings.AutomationPolicy.StagingRoundWindowSeconds, StagingPreparationSeconds: settings.AutomationPolicy.StagingPreparationSeconds,
			Generation: 1, ExpectedGeneration: 0, SourceGeneration: 0, TargetEpoch: 1, Kind: "baseline", Reason: "policy_approved_baseline", SourceDecisionDigest: round.SourceDecisionDigest,
			TargetRoundId:    server.NewId(),
			SourceLockSha256: generation.SourceLockSha256, Commits: generation.Repositories.commits(), GitRefs: map[string]model.CompetitionSourceGitRef{}, Manifest: manifestBytes,
			ManifestCommit: manifestCommit, ManifestSha256: hex.EncodeToString(digest[:]), ConfigExpectedCommit: manifestCommit,
			Threshold: settings.EvaluationPolicy.TakeoverMargin, ThresholdEvidenceSha256: generation.ThresholdEvidenceSha256,
			EvaluatorImageDigest: settings.EvaluatorImageDigest, HostQualificationSha256: settings.EvaluationPolicy.HostQualificationSha256, PreparedAt: time.Now().UTC()}
		publication := &sourcePublicationIntent{Schema: 2, Staging: intent.Staging, ActivationGeneration: 1, SourceDecisionDigest: intent.SourceDecisionDigest, ManifestSha256: intent.ManifestSha256, Bootstrap: true}
		for _, repository := range append(sourceRepositoryNames(), "config") {
			localRoot := filepath.Join(repositoriesRoot, repository)
			commit, branch := intent.Commits[repository], sourceBranch(intent.Staging)
			if repository == "config" {
				localRoot, commit, branch = configRoot, manifestCommit, "main"
			}
			origin, err := sourceGitText(ctx, localRoot, "remote", "get-url", "origin")
			if err != nil {
				fatalf("baseline origin: %s", err)
			}
			stagedRoot := filepath.Join(root, repository)
			if _, err := sourceGitText(ctx, root, "-c", "core.fsync=all", "clone", "--quiet", "--no-checkout", origin, stagedRoot); err != nil {
				fatalf("baseline clone: %s", err)
			}
			if err := os.Chmod(stagedRoot, 0o700); err != nil {
				fatalf("baseline permissions: %s", err)
			}
			if _, err := sourceGitText(ctx, stagedRoot, "checkout", "--quiet", "--detach", commit); err != nil {
				fatalf("baseline exact checkout: %s", err)
			}
			if repository == "server" {
				if err := verifyFrozenSourceGraph(ctx, stagedRoot, commit); err != nil {
					fatalf("baseline measured dependency graph: %s", err)
				}
			}
			intent.GitRefs[repository] = model.CompetitionSourceGitRef{Branch: branch, ExpectedCommit: commit, Commit: commit}
			publication.Refs = append(publication.Refs, sourcePublicationRef{Repository: repository, Branch: branch, ExpectedCommit: commit, Commit: commit, StagedRoot: filepath.Join(finalRoot, repository)})
			if _, err := store.RenewAutomationLease(ctx, settings, *lease, 5*time.Minute); err != nil {
				fatalf("baseline staging lease: %s", err)
			}
		}
		journalPath := filepath.Join(root, "publication.json")
		if err := saveSourcePublicationIntent(journalPath, publication); err != nil {
			fatalf("baseline journal: %s", err)
		}
		journalBytes, err := readSourceFile(journalPath)
		if err != nil {
			fatalf("baseline journal: %s", err)
		}
		journalDigest := sha256.Sum256(journalBytes)
		intent.PublicationJournalSha256 = hex.EncodeToString(journalDigest[:])
		content, err := json.Marshal(intent)
		if err != nil {
			fatalf("baseline intent: %s", err)
		}
		if err := writePrivateReviewFile(filepath.Join(root, "activation.json"), content); err != nil {
			fatalf("baseline intent: %s", err)
		}
		directory, err := os.Open(root)
		if err != nil {
			fatalf("baseline durability: %s", err)
		}
		err = directory.Sync()
		directory.Close()
		if err != nil {
			fatalf("baseline durability: %s", err)
		}
		if err := os.Rename(root, finalRoot); err != nil {
			fatalf("baseline staging installation: %s", err)
		}
		directory, err = os.Open(stateDirectory)
		if err != nil {
			fatalf("baseline state: %s", err)
		}
		err = directory.Sync()
		directory.Close()
		if err != nil {
			fatalf("baseline state: %s", err)
		}
	}
	if intent.Kind != "baseline" || intent.Generation != 1 || intent.ManifestCommit != manifestCommit {
		fatalf("baseline recovery conflicts with the explicitly requested config commit")
	}
	if err := validateSourceActivationMode(intent, settings); err != nil {
		fatalf("baseline recovery mode: %s", err)
	}
	intent, err = store.RecordSourceIntent(ctx, settings, *lease, *intent)
	if err != nil {
		fatalf("baseline authorization: %s", err)
	}
	transition, err := store.SupervisorTransition(ctx, settings, 1)
	if errors.Is(err, controller.ErrNotFound) {
		transition, err = nil, nil
	}
	if err != nil {
		fatalf("baseline checkpoint: %s", err)
	}
	transition, record, err := bootstrapSourceTransition(intent, transition)
	if err != nil {
		fatalf("baseline checkpoint: %s", err)
	}
	if record {
		transition, err = store.RecordSupervisorTransition(ctx, settings, *lease, *transition)
	}
	if err != nil {
		fatalf("baseline checkpoint: %s", err)
	}
	encoded, _ := json.Marshal(transition)
	fmt.Printf("%s\n", encoded)
}
