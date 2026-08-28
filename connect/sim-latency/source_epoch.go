package main

// Epoch source control binds every measured run to the four repositories that
// can affect the product result. Host runs require clean sim-latency branches
// at the configured heads. Evaluator images use their authenticated source-lock
// record so a one-commit candidate can run against its configured base epoch.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"
	"gopkg.in/yaml.v3"
)

const (
	maximumCompetitionEpoch  = 6
	maximumSourceConfigBytes = 1024 * 1024
)

var sourceGitShaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type sourceRepository struct {
	Commit string `yaml:"commit"`
}

type sourceRepositories struct {
	Connect sourceRepository `yaml:"connect"`
	Sdk     sourceRepository `yaml:"sdk"`
	Server  sourceRepository `yaml:"server"`
	Proxy   sourceRepository `yaml:"proxy"`
}

// commits returns the complete measured-source identity for one epoch.
func (self sourceRepositories) commits() map[string]string {
	return map[string]string{
		"connect": self.Connect.Commit,
		"sdk":     self.Sdk.Commit,
		"server":  self.Server.Commit,
		"proxy":   self.Proxy.Commit,
	}
}

type sourceEpoch struct {
	Epoch                         int                 `yaml:"epoch"`
	Kind                          string              `yaml:"kind"`
	SignificantImprovementPercent float64             `yaml:"significant_improvement_percent"`
	WinnerJobId                   string              `yaml:"winner_job_id,omitempty"`
	WinnerSignificance            *sourceSignificance `yaml:"winner_significance,omitempty"`
	PromotedFromEpoch             *int                `yaml:"promoted_from_epoch,omitempty"`
	PromotedAt                    string              `yaml:"promoted_at,omitempty"`
	Repositories                  sourceRepositories  `yaml:"repositories"`
}

// sourceSignificance preserves the winning evaluation statistics that define
// the next epoch's incumbent and recommended threshold.
type sourceSignificance struct {
	ScoreSha256                               string  `yaml:"score_sha256"`
	Method                                    string  `yaml:"method"`
	Alpha                                     float64 `yaml:"alpha"`
	ReplicateCount                            int     `yaml:"replicate_count"`
	BaselineMeanRawScore                      float64 `yaml:"baseline_mean_raw_score"`
	CandidateMeanRawScore                     float64 `yaml:"candidate_mean_raw_score"`
	BaselineSampleVariance                    float64 `yaml:"baseline_sample_variance"`
	CandidateSampleVariance                   float64 `yaml:"candidate_sample_variance"`
	ObservedImprovementPercent                float64 `yaml:"observed_improvement_percent"`
	TakeoverMarginPercent                     float64 `yaml:"takeover_margin_percent"`
	MinimumSignificantImprovementPercent      float64 `yaml:"minimum_significant_improvement_percent"`
	RequiredImprovementPercent                float64 `yaml:"required_improvement_percent"`
	OneSidedPValue                            float64 `yaml:"one_sided_p_value"`
	NextEpochMinimumImprovementPercent        float64 `yaml:"next_epoch_minimum_improvement_percent"`
	RecommendedNextEpochTakeoverMarginPercent float64 `yaml:"recommended_next_epoch_takeover_margin_percent"`
}

type evaluationSource struct {
	Branch string        `yaml:"branch"`
	Epochs []sourceEpoch `yaml:"epochs"`
}

type controlPlaneIdentity struct {
	ApiBranch                     string `yaml:"api_branch"`
	WorkerBranch                  string `yaml:"worker_branch"`
	RuntimeImageDigestEnvironment string `yaml:"runtime_image_digest_environment"`
	PersistPerEvaluation          bool   `yaml:"persist_per_evaluation"`
	FreezeMainCommits             bool   `yaml:"freeze_main_commits"`
}

type sourceManifest struct {
	Schema               int                  `yaml:"schema"`
	FrozenAt             string               `yaml:"frozen_at"`
	EvaluationSource     evaluationSource     `yaml:"evaluation_source"`
	ControlPlaneIdentity controlPlaneIdentity `yaml:"control_plane_identity"`
}

type evaluatorSourceLock struct {
	Schema              int               `json:"schema"`
	DevelopmentSnapshot bool              `json:"development_snapshot"`
	Repositories        map[string]string `json:"repositories"`
}

// readSourceFile bounds operator configuration before parsing it.
func readSourceFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fileBytes, err := io.ReadAll(io.LimitReader(file, maximumSourceConfigBytes+1))
	if err != nil {
		return nil, err
	}
	if maximumSourceConfigBytes < len(fileBytes) {
		return nil, errors.New("file is oversized")
	}
	return fileBytes, nil
}

// loadSourceManifest decodes a strict single-document epoch ledger.
func loadSourceManifest(path string) (*sourceManifest, error) {
	manifestBytes, err := readSourceFile(path)
	if err != nil {
		return nil, fmt.Errorf("read source config: %w", err)
	}
	manifest := &sourceManifest{}
	decoder := yaml.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(manifest); err != nil {
		return nil, fmt.Errorf("decode source config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode source config: multiple yaml documents are not allowed")
		}
		return nil, fmt.Errorf("decode source config trailing data: %w", err)
	}
	if err := validateSourceManifest(manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

// validateSourceManifest enforces the six-round monotonic ledger and control policy.
func validateSourceManifest(manifest *sourceManifest) error {
	if manifest == nil || manifest.Schema != 1 {
		return errors.New("source config schema must be 1")
	}
	if manifest.EvaluationSource.Branch != "sim-latency" {
		return errors.New("evaluation source branch must be sim-latency")
	}
	if _, err := time.Parse(time.RFC3339, manifest.FrozenAt); err != nil {
		return errors.New("source config frozen_at must be an RFC3339 timestamp")
	}
	if len(manifest.EvaluationSource.Epochs) == 0 || maximumCompetitionEpoch+1 < len(manifest.EvaluationSource.Epochs) {
		return errors.New("source config must contain baseline epoch 0 and at most six promoted epochs")
	}
	for epochIndex, epoch := range manifest.EvaluationSource.Epochs {
		if epoch.Epoch != epochIndex {
			return fmt.Errorf("source epochs must be contiguous: entry %d has epoch %d", epochIndex, epoch.Epoch)
		}
		if !finitePositive(epoch.SignificantImprovementPercent) ||
			50 < epoch.SignificantImprovementPercent {
			return fmt.Errorf("source epoch %d has an invalid significant improvement percentage", epochIndex)
		}
		if epochIndex == 0 && epoch.Kind != "baseline" {
			return errors.New("source epoch 0 must be the baseline")
		}
		if epochIndex == 0 {
			if epoch.WinnerJobId != "" || epoch.WinnerSignificance != nil ||
				epoch.PromotedFromEpoch != nil || epoch.PromotedAt != "" {
				return errors.New("source epoch 0 cannot contain winner promotion metadata")
			}
		} else {
			switch epoch.Kind {
			case "winner_promotion":
				if epoch.WinnerJobId == "" || epoch.WinnerSignificance == nil {
					return fmt.Errorf("source epoch %d must identify its winning job and significance", epochIndex)
				}
				if err := validateSourceSignificance(epoch.WinnerSignificance); err != nil {
					return fmt.Errorf("source epoch %d winner significance: %w", epochIndex, err)
				}
				if epoch.SignificantImprovementPercent !=
					epoch.WinnerSignificance.RecommendedNextEpochTakeoverMarginPercent {
					return fmt.Errorf("source epoch %d threshold does not match its winner recommendation", epochIndex)
				}
			case "no_winner_carry_forward":
				if epoch.WinnerJobId != "" || epoch.WinnerSignificance != nil {
					return fmt.Errorf("source epoch %d cannot identify a winner or significance when carrying forward", epochIndex)
				}
				previous := manifest.EvaluationSource.Epochs[epochIndex-1]
				if epoch.Repositories != previous.Repositories {
					return fmt.Errorf("source epoch %d no-winner transition must carry repository commits forward unchanged", epochIndex)
				}
				if epoch.SignificantImprovementPercent != previous.SignificantImprovementPercent {
					return fmt.Errorf("source epoch %d no-winner transition must carry the significance percentage forward unchanged", epochIndex)
				}
			default:
				return fmt.Errorf("source epoch %d has unsupported transition kind %q", epochIndex, epoch.Kind)
			}
			if epoch.PromotedFromEpoch == nil || *epoch.PromotedFromEpoch != epochIndex-1 {
				return fmt.Errorf("source epoch %d must promote epoch %d", epochIndex, epochIndex-1)
			}
			if _, err := time.Parse(time.RFC3339, epoch.PromotedAt); err != nil {
				return fmt.Errorf("source epoch %d promoted_at must be an RFC3339 timestamp", epochIndex)
			}
		}
		for repository, commit := range epoch.Repositories.commits() {
			if !sourceGitShaPattern.MatchString(commit) {
				return fmt.Errorf("source epoch %d repository %s has an invalid commit", epochIndex, repository)
			}
		}
	}
	if manifest.ControlPlaneIdentity.ApiBranch != "main" ||
		manifest.ControlPlaneIdentity.WorkerBranch != "main" ||
		manifest.ControlPlaneIdentity.FreezeMainCommits ||
		!manifest.ControlPlaneIdentity.PersistPerEvaluation ||
		manifest.ControlPlaneIdentity.RuntimeImageDigestEnvironment != "WARP_IMAGE_DIGEST" {
		return errors.New("control-plane identity policy is invalid")
	}
	return nil
}

func validateSourceSignificance(significance *sourceSignificance) error {
	if significance == nil || !validSha256(significance.ScoreSha256) ||
		significance.Method != scoreSignificanceMethod ||
		significance.Alpha != scoreSignificanceAlpha ||
		significance.ReplicateCount != 9 ||
		!finitePositive(significance.BaselineMeanRawScore) ||
		!finitePositive(significance.CandidateMeanRawScore) ||
		!finiteNonnegative(significance.BaselineSampleVariance) ||
		!finiteNonnegative(significance.CandidateSampleVariance) ||
		!finite(significance.ObservedImprovementPercent) ||
		significance.ObservedImprovementPercent <= 0 ||
		!finitePositive(significance.TakeoverMarginPercent) ||
		50 < significance.TakeoverMarginPercent ||
		!finiteNonnegative(significance.MinimumSignificantImprovementPercent) ||
		!finiteNonnegative(significance.RequiredImprovementPercent) ||
		!finite(significance.OneSidedPValue) || significance.OneSidedPValue < 0 ||
		significance.Alpha < significance.OneSidedPValue ||
		!finiteNonnegative(significance.NextEpochMinimumImprovementPercent) ||
		!finitePositive(significance.RecommendedNextEpochTakeoverMarginPercent) ||
		50 < significance.RecommendedNextEpochTakeoverMarginPercent {
		return errors.New("record is incomplete or not statistically significant")
	}
	if significance.RequiredImprovementPercent < significance.TakeoverMarginPercent ||
		significance.RequiredImprovementPercent < significance.MinimumSignificantImprovementPercent ||
		significance.RecommendedNextEpochTakeoverMarginPercent < significance.TakeoverMarginPercent ||
		significance.RecommendedNextEpochTakeoverMarginPercent < significance.NextEpochMinimumImprovementPercent {
		return errors.New("record contains an inconsistent significance margin")
	}
	return nil
}

// epoch returns one explicitly configured source generation.
func (self *sourceManifest) epoch(epochNumber int) (*sourceEpoch, error) {
	if epochNumber < 0 || len(self.EvaluationSource.Epochs) <= epochNumber {
		return nil, fmt.Errorf("source config does not contain epoch %d", epochNumber)
	}
	epoch := &self.EvaluationSource.Epochs[epochNumber]
	if epoch.Epoch != epochNumber {
		return nil, fmt.Errorf("source config epoch %d has inconsistent identity", epochNumber)
	}
	return epoch, nil
}

// gitOutput runs a read-only or explicitly requested git operation in one repository.
func gitOutput(repositoryRoot string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", repositoryRoot}, args...)
	command := exec.Command("git", commandArgs...)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// loadEvaluatorSourceLock authenticates the source copied into an evaluator image.
func loadEvaluatorSourceLock(path string) (*evaluatorSourceLock, error) {
	lockBytes, err := readSourceFile(path)
	if err != nil {
		return nil, err
	}
	lock := &evaluatorSourceLock{}
	decoder := json.NewDecoder(bytes.NewReader(lockBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(lock); err != nil {
		return nil, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("evaluator source lock contains trailing data")
	}
	if lock.Schema != 1 {
		return nil, errors.New("evaluator source lock schema must be 1")
	}
	if lock.DevelopmentSnapshot {
		return nil, errors.New("development snapshot cannot be used for an epoch-bound evaluation")
	}
	return lock, nil
}

// verifySourceEpoch fails closed unless every measured repository matches the ledger.
func verifySourceEpoch(manifest *sourceManifest, epochNumber int, repositoriesRoot string, sourceLockPath string) error {
	epoch, err := manifest.epoch(epochNumber)
	if err != nil {
		return err
	}
	commits := epoch.Repositories.commits()
	var sourceLock *evaluatorSourceLock
	if sourceLockPath != "" {
		sourceLock, err = loadEvaluatorSourceLock(sourceLockPath)
		if err != nil {
			return fmt.Errorf("load evaluator source lock: %w", err)
		}
	}
	for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
		expectedCommit := commits[repository]
		repositoryRoot := filepath.Join(repositoriesRoot, repository)
		status, err := gitOutput(repositoryRoot, "status", "--porcelain=v1", "--untracked-files=all")
		if err != nil {
			return fmt.Errorf("repository %s unavailable: %w", repository, err)
		}
		if status != "" {
			return fmt.Errorf("repository %s worktree is not clean", repository)
		}
		head, err := gitOutput(repositoryRoot, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("repository %s head unavailable: %w", repository, err)
		}
		if sourceLock == nil {
			branch, branchErr := gitOutput(repositoryRoot, "symbolic-ref", "--quiet", "--short", "HEAD")
			if branchErr != nil || branch != manifest.EvaluationSource.Branch {
				return fmt.Errorf("repository %s is not on branch %s", repository, manifest.EvaluationSource.Branch)
			}
			if head != expectedCommit {
				return fmt.Errorf("repository %s head %s does not match epoch %d commit %s", repository, head, epochNumber, expectedCommit)
			}
			continue
		}
		if sourceLock.Repositories[repository] != expectedCommit {
			return fmt.Errorf("evaluator source lock repository %s does not match epoch %d", repository, epochNumber)
		}
		if head == expectedCommit {
			continue
		}
		if repository != "server" {
			return fmt.Errorf("evaluator repository %s head does not match its authenticated source lock", repository)
		}
		parents, parentErr := gitOutput(repositoryRoot, "rev-list", "--parents", "-n", "1", "HEAD")
		if parentErr != nil {
			return parentErr
		}
		parentFields := strings.Fields(parents)
		if len(parentFields) != 2 || parentFields[1] != expectedCommit {
			return errors.New("candidate server commit is not exactly one commit above its configured epoch base")
		}
	}
	return nil
}

// findRepositoriesRoot discovers the common parent of the four measured repositories.
func findRepositoriesRoot() (string, error) {
	if root := strings.TrimSpace(os.Getenv("SIM_LATENCY_REPOSITORIES_ROOT")); root != "" {
		return filepath.Abs(root)
	}
	candidates := []string{}
	if warpHome := strings.TrimSpace(os.Getenv("WARP_HOME")); warpHome != "" {
		candidates = append(candidates, warpHome)
	}
	if cwd, err := os.Getwd(); err == nil {
		for current := cwd; ; current = filepath.Dir(current) {
			candidates = append(candidates, current)
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
		}
	}
	candidates = append(candidates, "/workspace")
	for _, candidate := range candidates {
		complete := true
		for _, repository := range []string{"connect", "sdk", "server", "proxy"} {
			if _, err := os.Stat(filepath.Join(candidate, repository)); err != nil {
				complete = false
				break
			}
		}
		if complete {
			return filepath.Clean(candidate), nil
		}
	}
	return "", errors.New("could not locate connect, sdk, server, and proxy repositories")
}

// findSourceConfig locates the non-secret source epoch ledger.
func findSourceConfig(repositoriesRoot string) (string, error) {
	if path := strings.TrimSpace(os.Getenv("SIM_LATENCY_SOURCE_CONFIG")); path != "" {
		return filepath.Abs(path)
	}
	candidates := []string{}
	if configHome := strings.TrimSpace(os.Getenv("WARP_CONFIG_HOME")); configHome != "" {
		candidates = append(candidates, filepath.Join(configHome, "main", "sim-latency.yml"))
	}
	candidates = append(candidates,
		filepath.Join(repositoriesRoot, "config", "main", "sim-latency.yml"),
		"/opt/urnetwork/sim-latency.yml",
	)
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return filepath.Clean(candidate), nil
		}
	}
	return "", errors.New("could not locate config/main/sim-latency.yml")
}

// configuredSourcePaths resolves explicit paths before falling back to the host/image layout.
func configuredSourcePaths(opts docopt.Opts) (string, string, string, error) {
	repositoriesRoot := optString(opts, "--repos-root", "")
	if repositoriesRoot == "" {
		var err error
		repositoriesRoot, err = findRepositoriesRoot()
		if err != nil {
			return "", "", "", err
		}
	} else {
		var err error
		repositoriesRoot, err = filepath.Abs(repositoriesRoot)
		if err != nil {
			return "", "", "", err
		}
	}
	sourceConfig := optString(opts, "--source-config", "")
	if sourceConfig == "" {
		var err error
		sourceConfig, err = findSourceConfig(repositoriesRoot)
		if err != nil {
			return "", "", "", err
		}
	} else {
		var err error
		sourceConfig, err = filepath.Abs(sourceConfig)
		if err != nil {
			return "", "", "", err
		}
	}
	sourceLock := strings.TrimSpace(os.Getenv("SIM_LATENCY_SOURCE_LOCK"))
	if sourceLock == "" {
		candidate := "/opt/urnetwork/source-lock.json"
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			sourceLock = candidate
		}
	}
	return sourceConfig, repositoriesRoot, sourceLock, nil
}

// configuredEpoch parses the mandatory bounded epoch argument.
func configuredEpoch(opts docopt.Opts) (int, error) {
	value := optString(opts, "--epoch", "")
	epochNumber, err := strconv.Atoi(value)
	if err != nil || epochNumber < 0 || maximumCompetitionEpoch < epochNumber {
		return 0, fmt.Errorf("--epoch must be an integer in 0..%d", maximumCompetitionEpoch)
	}
	return epochNumber, nil
}

// checkConfiguredSource is the common measurement-command fail-closed gate.
func checkConfiguredSource(opts docopt.Opts) (int, string, string, error) {
	epochNumber, err := configuredEpoch(opts)
	if err != nil {
		return 0, "", "", err
	}
	sourceConfig, repositoriesRoot, sourceLock, err := configuredSourcePaths(opts)
	if err != nil {
		return 0, "", "", err
	}
	manifest, err := loadSourceManifest(sourceConfig)
	if err != nil {
		return 0, "", "", err
	}
	if err := verifySourceEpoch(manifest, epochNumber, repositoriesRoot, sourceLock); err != nil {
		return 0, "", "", err
	}
	return epochNumber, sourceConfig, repositoriesRoot, nil
}

// runSourceCheck exposes the same preflight without starting a measurement.
func runSourceCheck(opts docopt.Opts) {
	epochNumber, sourceConfig, repositoriesRoot, err := checkConfiguredSource(opts)
	if err != nil {
		fatalf("source epoch preflight: %s", err)
	}
	if optBool(opts, "--json") {
		manifest, err := loadSourceManifest(sourceConfig)
		if err != nil {
			fatalf("source epoch config: %s", err)
		}
		epoch, err := manifest.epoch(epochNumber)
		if err != nil {
			fatalf("source epoch config: %s", err)
		}
		result := struct {
			Schema                        int     `json:"schema"`
			Epoch                         int     `json:"epoch"`
			SignificantImprovementPercent float64 `json:"significant_improvement_percent"`
		}{
			Schema:                        1,
			Epoch:                         epochNumber,
			SignificantImprovementPercent: epoch.SignificantImprovementPercent,
		}
		resultBytes, err := json.Marshal(result)
		if err != nil {
			fatalf("encode source epoch: %s", err)
		}
		fmt.Printf("%s\n", resultBytes)
		return
	}
	fmt.Printf("source epoch %d verified: config=%s repositories=%s\n", epochNumber, sourceConfig, repositoriesRoot)
}
