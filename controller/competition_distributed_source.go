// Exact source evidence is compared with the already authenticated activation
// intent, not the coordinator's mutable main worktrees. These wire shapes are
// emitted by prepare-evaluation-source.sh and evaluator-minimum-work.sh.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/urnetwork/server/model"
)

// A source identity is image-authenticated and temporary; only the candidate's
// repository heads/patch identity change after canonical patch application.
type competitionDistributedSourceIdentity struct {
	Schema               int               `json:"schema"`
	Kind                 string            `json:"kind"`
	Temporary            bool              `json:"temporary"`
	BaseImageId          string            `json:"base_image_id"`
	BaseSha              string            `json:"base_sha"`
	SourceEpoch          int               `json:"source_epoch"`
	Staging              bool              `json:"staging"`
	Branch               string            `json:"branch"`
	SourceLockSha256     string            `json:"source_lock_sha256"`
	Repositories         map[string]string `json:"repositories"`
	CandidatePatchSha256 *string           `json:"candidate_patch_sha256"`
	CandidatePathBase    string            `json:"candidate_path_base,omitempty"`
}

// Matches the trusted retained image-to-temporary-source inventory exactly.
type competitionDistributedSourceEvidence struct {
	Schema               int                                  `json:"schema"`
	Kind                 string                               `json:"kind"`
	Origin               string                               `json:"origin"`
	HostRepositoriesUsed bool                                 `json:"host_repositories_used"`
	RuntimeTarget        string                               `json:"runtime_target"`
	ReadOnlyMount        bool                                 `json:"read_only_mount"`
	Baseline             competitionDistributedSourceIdentity `json:"baseline"`
	Candidate            competitionDistributedSourceIdentity `json:"candidate"`
	CleanupComplete      bool                                 `json:"cleanup_complete"`
}

// Exactly ten measured repositories are permitted; operator control code is
// never copied into an evaluation checkout or accepted as a candidate product.
func validateDistributedMeasuredCommits(commits map[string]string) error {
	if len(commits) != len(model.CompetitionSourceRepositoryNames())-1 {
		return errors.New("measured source must contain exactly ten repositories")
	}
	for _, repository := range model.CompetitionSourceRepositoryNames() {
		if repository == "operator-proxy" {
			continue
		}
		if !gitShaPattern.MatchString(commits[repository]) {
			return errors.New("measured source omitted a frozen repository head")
		}
	}
	return nil
}

// A successful score must prove clean image-derived baseline heads, the exact
// patched candidate graph and completed removal of both temporary checkouts.
func authenticateDistributedCompetitionSources(settings *Settings, job *queuedJob, directory string, artifacts []evaluationArtifact, candidateCompleted bool) error {
	if err := validateDistributedMeasuredCommits(settings.distributedSourceCommits); err != nil {
		return err
	}
	declared := map[string]evaluationArtifact{}
	for _, artifact := range artifacts {
		declared[artifact.Path] = artifact
	}
	read := func(name string, value any) ([]byte, error) {
		artifact, present := declared[name]
		if !present {
			return nil, errors.New("required source evidence is not retained")
		}
		content, err := readRegularFile(filepath.Join(directory, name), settings.distributedCoordinator.Config.Settings.Limits.DocumentBytes)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(content)
		if artifact.Bytes != int64(len(content)) || artifact.Sha256 != hex.EncodeToString(digest[:]) || decodeFailureEvidenceJson(content, value) != nil {
			return nil, errors.New("source evidence identity or digest is invalid")
		}
		return content, nil
	}
	evidence := &competitionDistributedSourceEvidence{}
	if _, err := read("evidence/evaluation-sources.json", evidence); err != nil {
		return err
	}
	if evidence.Schema != 1 || evidence.Kind != "sim-latency-temporary-evaluation-sources" || evidence.Origin != "authenticated_evaluator_image" || evidence.HostRepositoriesUsed || evidence.RuntimeTarget != "/workspace" || !evidence.ReadOnlyMount || !evidence.CleanupComplete {
		return errors.New("source evidence did not prove the private image-derived readonly checkout lifecycle")
	}
	identityMatches := func(identity competitionDistributedSourceIdentity) bool {
		return identity.Schema == 1 && identity.Kind == "sim-latency-evaluation-source" && identity.Temporary && !identity.Staging && identity.Branch == "sim-latency" &&
			identity.BaseImageId == job.EvaluatorImageDigest && identity.BaseSha == settings.BaseSha && identity.SourceEpoch == evaluationSourceEpoch(&job.Round) &&
			sha256Pattern.MatchString(identity.SourceLockSha256) && validateDistributedMeasuredCommits(identity.Repositories) == nil &&
			(identity.CandidatePathBase == "" || identity.CandidatePathBase == competitionMonoRootPathBase) &&
			(settings.PatchPolicy.PathBase == "" || identity.CandidatePathBase == settings.PatchPolicy.PathBase)
	}
	if !identityMatches(evidence.Baseline) || evidence.Baseline.CandidatePatchSha256 != nil {
		return errors.New("baseline source does not match the frozen image and source generation")
	}
	for repository, commit := range settings.distributedSourceCommits {
		if evidence.Baseline.Repositories[repository] != commit {
			return errors.New("baseline source head differs from the exact activated source")
		}
	}
	if !candidateCompleted {
		return nil
	}
	if !identityMatches(evidence.Candidate) || evidence.Candidate.SourceLockSha256 != evidence.Baseline.SourceLockSha256 || evidence.Candidate.CandidatePatchSha256 == nil || *evidence.Candidate.CandidatePatchSha256 != job.PatchSha256 {
		return errors.New("candidate checkout is not bound to its canonical patch")
	}
	original := &competitionDistributedSourceIdentity{}
	originalBytes, err := read("evidence/candidate-original-source.json", original)
	if err != nil || !identityMatches(*original) || original.CandidatePatchSha256 != nil || original.SourceLockSha256 != evidence.Baseline.SourceLockSha256 {
		return errors.New("candidate original checkout is not bound to the pristine source")
	}
	for repository, commit := range settings.distributedSourceCommits {
		if original.Repositories[repository] != commit {
			return errors.New("candidate original source head differs from the activated source")
		}
	}
	build := struct {
		Schema                       int               `json:"schema"`
		BaseImageId                  string            `json:"base_image_id"`
		BaseSha                      string            `json:"base_sha"`
		BuilderSha256                string            `json:"builder_sha256"`
		CandidateCommits             map[string]string `json:"candidate_commits"`
		CandidateCommitsSha256       string            `json:"candidate_commits_sha256"`
		CandidateSha                 string            `json:"candidate_sha"`
		Image                        string            `json:"image"`
		ImageId                      string            `json:"image_id"`
		ImageKey                     string            `json:"image_key"`
		OriginalSourceIdentitySha256 string            `json:"original_source_identity_sha256"`
		PatchSha256                  string            `json:"patch_sha256"`
		PatchValidatorSha256         string            `json:"patch_validator_sha256"`
		PolicySha256                 string            `json:"policy_sha256"`
	}{}
	if _, err := read("evidence/candidate-build.json", &build); err != nil {
		return err
	}
	originalDigest := sha256.Sum256(originalBytes)
	if build.Schema != 1 || build.BaseImageId != job.EvaluatorImageDigest || build.BaseSha != settings.BaseSha || build.PatchSha256 != job.PatchSha256 ||
		build.OriginalSourceIdentitySha256 != hex.EncodeToString(originalDigest[:]) || validateDistributedMeasuredCommits(build.CandidateCommits) != nil || build.CandidateSha != build.CandidateCommits["server"] || !imageDigestPattern.MatchString(build.ImageId) {
		return errors.New("candidate build evidence changed source, patch or original checkout")
	}
	for repository, commit := range evidence.Candidate.Repositories {
		if build.CandidateCommits[repository] != commit {
			return errors.New("candidate image and temporary checkout use different commit graphs")
		}
	}
	// jq -S emits the same two-space sorted JSON plus newline for this ASCII
	// string map; authenticate the exact commit-map format used by the builder.
	commits, err := json.MarshalIndent(build.CandidateCommits, "", "  ")
	if err != nil {
		return err
	}
	commitsDigest := sha256.Sum256(append(commits, '\n'))
	if build.CandidateCommitsSha256 != hex.EncodeToString(commitsDigest[:]) {
		return errors.New("candidate commit-map evidence failed authentication")
	}
	return nil
}
