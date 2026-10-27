package main

// Launch tooling produces machine-readable, fail-closed evidence without
// modifying the developer's frozen working trees. Remote source verification
// uses private temporary bare clones; live checks never serialize credentials.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	servergrafana "github.com/urnetwork/server/grafana"
	"github.com/urnetwork/server/model"
)

const (
	launchEvidenceSchema = 1
)

// Operational proof gates are invocation-owned, not global tunables.
type launchGateSettings struct{ ArtifactCapacityStopPercent, RunnerHeartbeatMaximumSeconds float64 }

// Frozen launch thresholds remain unchanged; no new CLI override is exposed.
func defaultLaunchGateSettings() launchGateSettings {
	return launchGateSettings{ArtifactCapacityStopPercent: 90, RunnerHeartbeatMaximumSeconds: 30}
}

var mandatoryLaunchChecks = []string{
	"frozen_source",
	"evaluator_image",
	"openapi",
	"competition_api",
	"minio_retention_capacity",
	"grafana_dashboard_alerts_routing",
	"grafana_live_metrics",
}

type launchCheck struct {
	Passed   bool           `json:"passed"`
	Error    string         `json:"error,omitempty"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

type launchPreflightResult struct {
	Schema               int                    `json:"schema"`
	Passed               bool                   `json:"passed"`
	CheckedAt            time.Time              `json:"checked_at"`
	Epoch                int                    `json:"epoch"`
	SourceConfigSha256   string                 `json:"source_config_sha256"`
	EvaluatorImageDigest string                 `json:"evaluator_image_digest"`
	OpenApiSha256        string                 `json:"openapi_sha256"`
	Checks               map[string]launchCheck `json:"checks"`
}

type launchPreflightConfig struct {
	// Bounds the entire proof, including source remotes and blob operations.
	Timeout time.Duration
	// Bounds draining a canceled source subprocess that left inherited pipes.
	SubprocessWaitDelay   time.Duration
	Epoch                 int
	SourceConfig          string
	RepositoriesRoot      string
	EvaluatorImageDigest  string
	OpenApiPath           string
	ApiUrl                string
	OperatorToken         string
	GrafanaUrl            string
	GrafanaToken          string
	GrafanaCredential     *launchHttpCredential
	MetricsUrl            string
	MetricsToken          string
	MetricsCredential     *launchHttpCredential
	ArtifactCapacityBytes int64
	OutputPath            string
	Now                   func() time.Time
}

// Defaults belong to the invocation; callers may shorten them for diagnostics.
func defaultLaunchPreflightConfig() *launchPreflightConfig {
	return &launchPreflightConfig{Timeout: 10 * time.Minute, SubprocessWaitDelay: 5 * time.Second, ArtifactCapacityBytes: 10_000_000_000_000}
}

// The user's allocation is an upper bound, not permission for an arbitrary
// command-line denominator that could make a full namespace look empty.
func validateLaunchArtifactAllocation(capacityBytes int64) error {
	if capacityBytes <= 0 || capacityBytes > defaultLaunchPreflightConfig().ArtifactCapacityBytes {
		return errors.New("artifact allocation must be positive and at most the approved decimal 10 TB")
	}
	return nil
}

type handoffEvidence struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type launchHandoffManifest struct {
	Schema      int       `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	Epoch       int       `json:"epoch"`
	Source      struct {
		Branch       string            `json:"branch"`
		Repositories map[string]string `json:"repositories"`
		Config       handoffEvidence   `json:"config"`
	} `json:"source"`
	Evaluator struct {
		ImageDigest string `json:"image_digest"`
	} `json:"evaluator"`
	ControlPlane struct {
		ApiBranch               string `json:"api_branch"`
		WorkerBranch            string `json:"worker_branch"`
		FreezeMainCommits       bool   `json:"freeze_main_commits"`
		PersistRuntimeImageEach bool   `json:"persist_runtime_image_each_evaluation"`
	} `json:"control_plane"`
	OpenApi          handoffEvidence   `json:"openapi"`
	BaselineManifest handoffEvidence   `json:"baseline_manifest"`
	Preflight        handoffEvidence   `json:"preflight"`
	StagingEvidence  []handoffEvidence `json:"staging_evidence"`
	Operations       struct {
		EvidenceDeletionOwner string `json:"evidence_deletion_owner"`
		IncidentContact       string `json:"incident_contact"`
	} `json:"operations"`
	Apex struct {
		Status               string  `json:"status"`
		RegistryIdentifier   *string `json:"registry_identifier"`
		AdapterImageDigest   *string `json:"adapter_image_digest"`
		MacrocosmosSignature *string `json:"macrocosmos_signature"`
	} `json:"apex"`
}

func runLaunchPreflight(opts docopt.Opts) {
	config, err := launchPreflightConfigFromOptions(opts)
	if err != nil {
		fatalf("launch preflight options: %s", err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	result, preflightErr := executeLaunchPreflight(ctx, config)
	encoded, encodeErr := json.MarshalIndent(result, "", "  ")
	if encodeErr != nil {
		fatalf("launch preflight encode: %s", encodeErr)
	}
	encoded = append(encoded, '\n')
	if config.OutputPath == "" {
		fmt.Print(string(encoded))
	} else if err := writeAtomicFile(config.OutputPath, encoded, 0644); err != nil {
		fatalf("launch preflight write: %s", err)
	}
	if preflightErr != nil {
		fatalf("launch preflight failed: %s", preflightErr)
	}
}

func launchPreflightConfigFromOptions(opts docopt.Opts) (*launchPreflightConfig, error) {
	defaults := defaultLaunchPreflightConfig()
	timeout, err := time.ParseDuration(optString(opts, "--preflight-timeout", defaults.Timeout.String()))
	if err != nil || timeout <= 0 {
		return nil, errors.New("--preflight-timeout must be a positive duration")
	}
	epoch, err := configuredEpoch(opts)
	if err != nil {
		return nil, err
	}
	sourceConfig, repositoriesRoot, _, err := configuredSourcePaths(opts)
	if err != nil {
		return nil, err
	}
	capacityBytes, err := strconv.ParseInt(optString(opts, "--artifact-capacity-bytes", strconv.FormatInt(defaults.ArtifactCapacityBytes, 10)), 10, 64)
	if err != nil || validateLaunchArtifactAllocation(capacityBytes) != nil {
		return nil, errors.New("--artifact-capacity-bytes exceeds the approved allocation or is invalid")
	}
	readToken := func(option string) (string, error) {
		filePath := optString(opts, option, "")
		if filePath == "" {
			return "", fmt.Errorf("%s is required", option)
		}
		return readPrivateToken(filePath)
	}
	operatorToken, err := readToken("--operator-token-file")
	if err != nil {
		return nil, err
	}
	readMonitoringCredential := func(option string) (launchHttpCredential, error) {
		path := optString(opts, option, "")
		if path == "" {
			return launchHttpCredential{}, fmt.Errorf("%s is required", option)
		}
		return readLaunchHttpCredential(path, defaultLaunchHttpCredentialSettings())
	}
	grafanaCredential, err := readMonitoringCredential("--grafana-token-file")
	if err != nil {
		return nil, err
	}
	metricsCredential, err := readMonitoringCredential("--metrics-token-file")
	if err != nil {
		return nil, err
	}
	return &launchPreflightConfig{
		Timeout:               timeout,
		SubprocessWaitDelay:   defaults.SubprocessWaitDelay,
		Epoch:                 epoch,
		SourceConfig:          sourceConfig,
		RepositoriesRoot:      repositoriesRoot,
		EvaluatorImageDigest:  optString(opts, "--evaluator-image", ""),
		OpenApiPath:           optString(opts, "--openapi", filepath.Join(repositoriesRoot, "sn", "api", "competition.yml")),
		ApiUrl:                optString(opts, "--api-url", "https://api.bringyour.com"),
		OperatorToken:         operatorToken,
		GrafanaUrl:            optString(opts, "--grafana-url", ""),
		GrafanaCredential:     &grafanaCredential,
		MetricsUrl:            optString(opts, "--metrics-url", ""),
		MetricsCredential:     &metricsCredential,
		ArtifactCapacityBytes: capacityBytes,
		OutputPath:            optString(opts, "--out", ""),
		Now:                   server.NowUtc,
	}, nil
}

func executeLaunchPreflight(ctx context.Context, config *launchPreflightConfig) (*launchPreflightResult, error) {
	if config == nil || config.Now == nil || config.Timeout <= 0 || config.SubprocessWaitDelay <= 0 || !validSha256(strings.TrimPrefix(config.EvaluatorImageDigest, "sha256:")) ||
		!strings.HasPrefix(config.EvaluatorImageDigest, "sha256:") {
		return nil, errors.New("launch preflight configuration is incomplete")
	}
	ctx, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	result := &launchPreflightResult{
		Schema:               launchEvidenceSchema,
		Passed:               true,
		CheckedAt:            config.Now().UTC(),
		Epoch:                config.Epoch,
		EvaluatorImageDigest: config.EvaluatorImageDigest,
		Checks:               map[string]launchCheck{},
	}
	errorsByCheck := []string{}
	record := func(name string, evidence map[string]any, err error) {
		check := launchCheck{Passed: err == nil, Evidence: evidence}
		if err != nil {
			check.Error = err.Error()
			result.Passed = false
			errorsByCheck = append(errorsByCheck, name+": "+err.Error())
		}
		result.Checks[name] = check
	}

	sourceBytes, sourceErr := readSourceFile(config.SourceConfig)
	var manifest *sourceManifest
	if sourceErr == nil {
		manifest, sourceErr = loadSourceManifest(config.SourceConfig)
	}
	if sourceErr == nil {
		sourceErr = verifyRemoteSourceEpochModeContext(ctx, config.SubprocessWaitDelay, manifest, config.Epoch, config.RepositoriesRoot, manifest.EvaluationSource.Branch, true)
	}
	if len(sourceBytes) != 0 {
		digest := sha256.Sum256(sourceBytes)
		result.SourceConfigSha256 = hex.EncodeToString(digest[:])
	}
	record("frozen_source", map[string]any{"config": config.SourceConfig, "sha256": result.SourceConfigSha256}, sourceErr)

	imageId, imageErr := inspectEvaluatorImage(ctx, config.EvaluatorImageDigest)
	record("evaluator_image", map[string]any{"image_id": imageId}, imageErr)

	openApiEvidence, openApiErr := inspectEvidenceFile(config.OpenApiPath, maximumSourceConfigBytes)
	if openApiErr == nil {
		result.OpenApiSha256 = openApiEvidence.Sha256
	}
	record("openapi", map[string]any{"path": config.OpenApiPath, "sha256": result.OpenApiSha256}, openApiErr)

	apiEvidence, competitionInfo, apiErr := checkCompetitionApi(ctx, config, manifest)
	record("competition_api", apiEvidence, apiErr)

	minioEvidence, minioErr := checkCompetitionMinio(ctx, competitionInfo, config.ArtifactCapacityBytes)
	record("minio_retention_capacity", minioEvidence, minioErr)

	grafanaEvidence, grafanaErr := checkCompetitionGrafana(ctx, config)
	record("grafana_dashboard_alerts_routing", grafanaEvidence, grafanaErr)

	metricsEvidence, metricsErr := checkCompetitionMetrics(ctx, config)
	record("grafana_live_metrics", metricsEvidence, metricsErr)

	if len(errorsByCheck) != 0 {
		sort.Strings(errorsByCheck)
		return result, errors.New(strings.Join(errorsByCheck, "; "))
	}
	return result, nil
}

func verifyRemoteSourceEpoch(manifest *sourceManifest, epochNumber int, repositoriesRoot string) error {
	return verifyRemoteSourceEpochMode(
		manifest,
		epochNumber,
		repositoriesRoot,
		manifest.EvaluationSource.Branch,
		false,
	)
}

// verifyRemoteSourceEpochHead rejects the partial-promotion interval where a
// product branch advanced but the config ledger was not activated last.
func verifyRemoteSourceEpochHead(manifest *sourceManifest, epochNumber int, repositoriesRoot string) error {
	return verifyRemoteSourceEpochMode(
		manifest,
		epochNumber,
		repositoriesRoot,
		manifest.EvaluationSource.Branch,
		true,
	)
}

// verifyRemoteSourceEpochBranchHead proves that a named era branch is an
// exact alias of one configured checkpoint. Unlike production history checks,
// legacy staging aliases are never allowed to advance past the frozen baseline.
// Explicit v2 staging uses its own manifest and source-record --staging instead.
func verifyRemoteSourceEpochBranchHead(
	manifest *sourceManifest,
	epochNumber int,
	repositoriesRoot string,
	branch string,
) error {
	if manifest.Staging {
		return errors.New("legacy staging source aliases cannot use an explicit v2 staging manifest; use source-record --staging")
	}
	if branch != stagingEvaluationSourceBranch {
		return fmt.Errorf("unsupported staging source branch %q", branch)
	}
	return verifyRemoteSourceEpochMode(manifest, epochNumber, repositoriesRoot, branch, true)
}

func verifyRemoteSourceEpochMode(
	manifest *sourceManifest,
	epochNumber int,
	repositoriesRoot string,
	branch string,
	requireExactHead bool,
) error {
	return verifyRemoteSourceEpochModeContext(context.Background(), defaultLaunchPreflightConfig().SubprocessWaitDelay, manifest, epochNumber, repositoriesRoot, branch, requireExactHead)
}

// All Git work shares the invocation's cancellation boundary. Text and error
// output are independently bounded, including the initial remote clone.
func verifyRemoteSourceEpochModeContext(
	ctx context.Context,
	waitDelay time.Duration,
	manifest *sourceManifest,
	epochNumber int,
	repositoriesRoot string,
	branch string,
	requireExactHead bool,
) error {
	if manifest == nil || waitDelay <= 0 {
		return errors.New("source preflight requires a manifest and bounded subprocess cleanup")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	epoch, err := manifest.epoch(epochNumber)
	if err != nil {
		return err
	}
	temporaryRoot, err := os.MkdirTemp("", "sim-latency-source-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporaryRoot)
	for _, repositoryName := range sourceRepositoryNamesForMode(manifest.Staging) {
		localRoot := filepath.Join(repositoriesRoot, repositoryName)
		origin, err := launchSourceGitText(ctx, waitDelay, localRoot, "remote", "get-url", "origin")
		if err != nil {
			return fmt.Errorf("repository %s origin: %w", repositoryName, err)
		}
		cloneRoot := filepath.Join(temporaryRoot, repositoryName+".git")
		// Most checks pin the advertised tip and need no history or file blobs.
		// Older checkpoints deepen only their own branch before ancestry proof.
		cloneArgs := []string{"clone", "--template=", "--quiet", "--bare", "--single-branch", "--no-tags", "--branch", branch,
			"--depth=1", "--filter=blob:none"}
		cloneArgs = append(cloneArgs, origin, cloneRoot)
		command, err := sourceGitCommand(ctx, temporaryRoot, cloneArgs...)
		if err != nil {
			return err
		}
		configureLaunchSourceCommand(command, waitDelay)
		output := &sourceBoundedWriter{limit: maximumSourceConfigBytes}
		command.Stdout, command.Stderr = output, output
		if err := command.Run(); err != nil {
			return fmt.Errorf("clone repository %s competition branch: %w", repositoryName, errors.Join(ctx.Err(), err))
		}
		// Git's --branch also accepts tags and leaves HEAD detached for them.
		// Source policy requires the named branch, not an identically named tag.
		actualBranch, err := launchSourceGitText(ctx, waitDelay, cloneRoot, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || actualBranch != "refs/heads/"+branch {
			return fmt.Errorf("repository %s competition source %s is not the required branch", repositoryName, branch)
		}
		expectedCommit := epoch.Repositories.commitsForMode(manifest.Staging)[repositoryName]
		head, err := launchSourceGitText(ctx, waitDelay, cloneRoot, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("repository %s competition branch head: %w", repositoryName, err)
		}
		if requireExactHead {
			if head != expectedCommit {
				return fmt.Errorf("repository %s competition branch head %s does not match active epoch commit %s", repositoryName, head, expectedCommit)
			}
			continue
		}
		if head != expectedCommit {
			shallow, err := launchSourceGitText(ctx, waitDelay, cloneRoot, "rev-parse", "--is-shallow-repository")
			if err != nil || (shallow != "true" && shallow != "false") {
				return fmt.Errorf("repository %s shallow history state is unavailable", repositoryName)
			}
			if shallow == "true" {
				if _, err := launchSourceGitNetworkText(ctx, waitDelay, cloneRoot, "fetch", "--quiet", "--unshallow", "--filter=blob:none", "origin", branch); err != nil {
					return fmt.Errorf("repository %s checkpoint history unavailable: %w", repositoryName, err)
				}
			}
			// Local clones and promisor fetches can contain unrelated objects.
			// Availability alone is not proof of membership in this branch.
			if _, err := launchSourceGitText(ctx, waitDelay, cloneRoot, "merge-base", "--is-ancestor", expectedCommit, head); err != nil {
				return fmt.Errorf("repository %s competition branch does not contain epoch commit %s as an ancestor", repositoryName, expectedCommit)
			}
		}
	}
	return nil
}

// Reuses exact-root Git trust while bounding output and inherited-pipe drain.
func launchSourceGitText(ctx context.Context, waitDelay time.Duration, repositoryRoot string, args ...string) (string, error) {
	command, err := sourceGitLocalCommand(ctx, repositoryRoot, args...)
	if err != nil {
		return "", err
	}
	configureLaunchSourceCommand(command, waitDelay)
	output := &sourceBoundedWriter{limit: maximumSourceConfigBytes}
	command.Stdout, command.Stderr = output, &sourceBoundedWriter{limit: 4096}
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("launch source Git command failed: %w", errors.Join(ctx.Err(), err))
	}
	return strings.TrimSpace(string(output.content)), nil
}

// Explicit transport calls retain operator authentication; local object reads
// cannot gain it by changing their argv or candidate-controlled attributes.
func launchSourceGitNetworkText(ctx context.Context, waitDelay time.Duration, repositoryRoot string, args ...string) (string, error) {
	command, err := sourceGitCommand(ctx, repositoryRoot, args...)
	if err != nil {
		return "", err
	}
	configureLaunchSourceCommand(command, waitDelay)
	output := &sourceBoundedWriter{limit: maximumSourceConfigBytes}
	command.Stdout, command.Stderr = output, &sourceBoundedWriter{limit: 4096}
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("launch source Git transport failed: %w", errors.Join(ctx.Err(), err))
	}
	return strings.TrimSpace(string(output.content)), nil
}

// Cancellation retires the complete Git/SSH process group, not just the Git
// parent. Pipe drain has its own bound after process cancellation.
func configureLaunchSourceCommand(command *exec.Cmd, waitDelay time.Duration) {
	command.WaitDelay = waitDelay
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		} else {
			return err
		}
	}
}

func inspectEvaluatorImage(ctx context.Context, expectedDigest string) (string, error) {
	command := exec.CommandContext(ctx, "sudo", "docker", "image", "inspect", "--format={{.Id}}", expectedDigest)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect evaluator image: %s", strings.TrimSpace(string(output)))
	}
	imageId := strings.TrimSpace(string(output))
	if imageId != expectedDigest {
		return imageId, fmt.Errorf("loaded evaluator image id %s does not match %s", imageId, expectedDigest)
	}
	return imageId, nil
}

// Authenticate local source identity before contacting the live API. A failed
// independent source check must yield a failed receipt, never a nil panic.
func checkCompetitionApi(
	ctx context.Context,
	config *launchPreflightConfig,
	manifest *sourceManifest,
) (map[string]any, *controller.InfoResult, error) {
	if config == nil || manifest == nil {
		return nil, nil, errors.New("competition API check requires an authenticated source manifest")
	}
	if err := validateSourceManifest(manifest); err != nil {
		return nil, nil, fmt.Errorf("competition API source manifest: %w", err)
	}
	epoch, err := manifest.epoch(config.Epoch)
	if err != nil {
		return nil, nil, err
	}
	client := newLaunchHttpClient()
	health := controller.HealthResult{}
	if err := getLaunchJson(ctx, client, config.ApiUrl, "/competition/healthz", "", &health); err != nil {
		return nil, nil, err
	}
	if health.Status != "alive" {
		return nil, nil, errors.New("competition health response is not alive")
	}
	info := &controller.InfoResult{}
	if err := getLaunchJson(ctx, client, config.ApiUrl, "/competition/info", "", info); err != nil {
		return nil, nil, err
	}
	if !info.Enabled || info.ScoreSchema != controller.ScoreSchema || info.ScorerVersion != controller.ScorerVersion ||
		info.BaseSha != epoch.Repositories.Server.Commit || info.EvaluatorImageDigest != config.EvaluatorImageDigest {
		return nil, nil, errors.New("live competition policy does not match the frozen epoch and evaluator")
	}
	if manifest.Schema == 2 {
		policy := info.AutomationPolicy
		generation, err := manifest.generation(config.Epoch)
		if err != nil || policy == nil || !policy.Enabled || model.ValidateCompetitionAutomationPolicy(*policy) != nil ||
			policy.Staging != manifest.Staging || policy.SourceGeneration != config.Epoch || policy.SourceLockSha256 != generation.SourceLockSha256 ||
			info.DecisionMode != model.CompetitionDecisionModeV2 || info.RequiredApiCapability != policy.RequiredApiCapability ||
			!sourceMarginMatches(generation.SignificantImprovementPercent, info.EvaluationPolicy.TakeoverMargin) {
			return nil, nil, errors.New("live v2 competition does not match the exact source mode, generation, lock and margin")
		}
		round := info.ActiveRound
		if manifest.Staging {
			round = info.StagingRound
		}
		if round == nil || round.RoundId == (server.Id{}) || round.Staging != manifest.Staging || round.DecisionMode != info.DecisionMode || round.RequiredApiCapability != info.RequiredApiCapability {
			return nil, nil, errors.New("live v2 competition has no matching activated source round")
		}
		mapping, err := manifest.sourceRound(round.Epoch)
		if err != nil || mapping.SourceGeneration != config.Epoch || mapping.ActivationGeneration != policy.SourceActivationGeneration || mapping.SourceLockSha256 != policy.SourceLockSha256 {
			return nil, nil, errors.New("live v2 source activation does not match its immutable round mapping")
		}
	}
	ready := &controller.ReadinessResult{}
	if err := getLaunchJson(ctx, client, config.ApiUrl, "/competition/readyz", config.OperatorToken, ready); err != nil {
		return nil, nil, err
	}
	if !ready.Ready || len(ready.Checks) == 0 {
		return nil, nil, errors.New("competition readyz is false")
	}
	if manifest.Schema == 2 {
		for _, name := range []string{"configuration", "frozen_policy", "retention_window", "database", "fifo_slot", "queue_admission", "authoritative_evaluator_host", "artifact_storage", "host_rebaseline", "api_image_identity", "artifact_archive"} {
			if !ready.Checks[name] {
				return nil, nil, fmt.Errorf("competition readiness check %s is missing or failed", name)
			}
		}
	}
	for name, passed := range ready.Checks {
		if !passed {
			return nil, nil, fmt.Errorf("competition readiness check %s failed", name)
		}
	}
	leaderboards := &controller.SeasonLeaderboardResult{}
	if err := getLaunchJson(ctx, client, config.ApiUrl, "/competition/leaderboard", "", leaderboards); err != nil {
		return nil, nil, err
	}
	return map[string]any{
		"competition_id":    info.CompetitionId,
		"source_generation": config.Epoch,
		"automation_policy": info.AutomationPolicy,
		"active_round":      info.ActiveRound,
		"staging_round":     info.StagingRound,
		"readiness":         ready.Checks,
		"finalized_epochs":  len(leaderboards.Epochs),
	}, info, nil
}

func checkCompetitionMinio(ctx context.Context, info *controller.InfoResult, capacityBytes int64) (map[string]any, error) {
	if info == nil {
		return nil, errors.New("competition API identity is unavailable")
	}
	settings, err := controller.LoadSettings()
	if err != nil {
		return nil, errors.New("configured competition archive settings are unavailable")
	}
	if settings.CompetitionId != info.CompetitionId {
		return nil, errors.New("competition API and configured archive scopes differ")
	}
	store, bucket, err := settings.CompetitionArtifactStore()
	if err != nil {
		return nil, err
	}
	return checkCompetitionMinioStore(ctx, info, capacityBytes, store, bucket)
}

// The configured competition namespace includes every archive version and
// scope, including v2 runtime/host/review records. Charging extra competition
// history is conservative; charging only v1 would hide real allocation usage.
func checkCompetitionMinioStore(ctx context.Context, info *controller.InfoResult, capacityBytes int64, store server.BlobStore, bucket string) (map[string]any, error) {
	if err := validateLaunchArtifactAllocation(capacityBytes); err != nil {
		return nil, err
	}
	if info == nil || store == nil || bucket == "" || store.Bucket() != bucket || strings.HasPrefix(store.Authority(), "local:") {
		return nil, errors.New("production MinIO blob store is unavailable")
	}
	retainedStore, ok := store.(server.ProtectedBlobStore)
	if !ok {
		return nil, errors.New("blob store does not support immutable retention and replication proof")
	}
	protection, err := retainedStore.CheckProtection(ctx)
	if err != nil {
		return nil, err
	}
	if protection == nil || protection.Authority != store.Authority() || protection.Bucket != bucket || protection.ObjectLock != "COMPLIANCE" || !protection.Versioning || len(protection.ReplicationTargets) == 0 {
		return nil, errors.New("configured competition archive protection proof is incomplete")
	}
	for _, target := range protection.ReplicationTargets {
		if strings.TrimSpace(target) == "" {
			return nil, errors.New("configured competition replication target is empty")
		}
	}
	prefix := path.Join(store.Prefix(), "competition") + "/"
	usage, err := server.MeasureBlobVersionUsageAtPrefix(ctx, store, prefix, capacityBytes)
	if err != nil {
		return nil, err
	}
	if defaultLaunchGateSettings().ArtifactCapacityStopPercent <= usage.UsedPercent {
		return map[string]any{"usage": usage}, fmt.Errorf("competition evidence allocation is %.2f%% used", usage.UsedPercent)
	}
	return map[string]any{"protection": protection, "usage": usage, "usage_scope": "configured_competition_namespace_v1_v2_and_object_versions", "requested_competition_id": info.CompetitionId}, nil
}

// Read-only configuration proof checks the actual integration and canonical route, never inferred email delivery from a contact-name substring.
func checkCompetitionGrafana(ctx context.Context, config *launchPreflightConfig) (map[string]any, error) {
	client := newLaunchHttpClient()
	credential, err := selectLaunchHttpCredential(config.GrafanaCredential, config.GrafanaToken)
	if err != nil {
		return nil, err
	}
	requests := []struct {
		path     string
		required []string
	}{
		{path: "/api/health", required: []string{`"database":"ok"`}},
		{path: "/api/search?query=sim-latency%20competition", required: []string{`"uid":"urnetwork-competition"`}},
		{path: "/api/v1/provisioning/alert-rules", required: []string{
			"competition-runner-heartbeat-stale", "competition-minio-capacity-warning", "competition-minio-capacity-critical",
		}},
		{path: "/api/v1/provisioning/contact-points"},
		{path: "/api/v1/provisioning/policies"},
	}
	var contactsJson, policyJson []byte
	for _, probe := range requests {
		body, err := getLaunchBytesWithCredential(ctx, client, config.GrafanaUrl, probe.path, credential)
		if err != nil {
			return nil, err
		}
		switch probe.path {
		case "/api/v1/provisioning/contact-points":
			contactsJson = body
		case "/api/v1/provisioning/policies":
			policyJson = body
		}
		compact := strings.ReplaceAll(strings.ReplaceAll(string(body), " ", ""), "\n", "")
		for _, required := range probe.required {
			if !strings.Contains(compact, required) {
				return nil, fmt.Errorf("Grafana %s is missing %q", probe.path, required)
			}
		}
	}
	if err := servergrafana.ValidateCompetitionAlertRouting(contactsJson, policyJson); err != nil {
		return nil, err
	}
	return map[string]any{
		"dashboard_uid":                "urnetwork-competition",
		"contact_point":                servergrafana.DefaultAlertContactName,
		"contact_integration_uid":      servergrafana.DefaultAlertContactUid,
		"incident_contact":             servergrafana.CompetitionIncidentContact,
		"incident_owner":               servergrafana.CompetitionIncidentContact,
		"notification_transport":       "existing_main_slack_receiver",
		"notification_delivery_proven": false,
		"contact_response_sha256":      fmt.Sprintf("%x", sha256.Sum256(contactsJson)),
		"policy_response_sha256":       fmt.Sprintf("%x", sha256.Sum256(policyJson)),
	}, nil
}

func checkCompetitionMetrics(ctx context.Context, config *launchPreflightConfig) (map[string]any, error) {
	credential, err := selectLaunchHttpCredential(config.MetricsCredential, config.MetricsToken)
	if err != nil {
		return nil, err
	}
	heartbeatAge, err := queryPrometheusScalarWithCredential(
		ctx,
		config.MetricsUrl,
		credential,
		"time() - max(urnetwork_competition_runner_heartbeat_timestamp_seconds)",
	)
	if err != nil {
		return nil, err
	}
	if heartbeatAge < 0 || defaultLaunchGateSettings().RunnerHeartbeatMaximumSeconds < heartbeatAge {
		return nil, fmt.Errorf("sim-latency runner heartbeat is %.1f seconds old", heartbeatAge)
	}
	minioUsedPercent, err := queryPrometheusScalarWithCredential(
		ctx,
		config.MetricsUrl,
		credential,
		`100 * (1 - max(minio_cluster_health_capacity_usable_free_bytes{job="minio"}) / max(minio_cluster_health_capacity_usable_total_bytes{job="minio"}))`,
	)
	if err != nil {
		return nil, err
	}
	if minioUsedPercent < 0 || defaultLaunchGateSettings().ArtifactCapacityStopPercent <= minioUsedPercent {
		return nil, fmt.Errorf("MinIO cluster is %.2f%% used", minioUsedPercent)
	}
	return map[string]any{
		"runner_heartbeat_age_seconds": heartbeatAge,
		"minio_used_percent":           minioUsedPercent,
	}, nil
}

func queryPrometheusScalar(ctx context.Context, baseUrl string, token string, query string) (float64, error) {
	return queryPrometheusScalarWithCredential(ctx, baseUrl, launchHttpCredential{bearerToken: token}, query)
}

// The deployed Mimir /prometheus route uses its existing query-scoped Basic
// identity; bearer remains supported without sharing credentials with Grafana.
func queryPrometheusScalarWithCredential(ctx context.Context, baseUrl string, credential launchHttpCredential, query string) (float64, error) {
	parsed, err := url.Parse(baseUrl)
	if err != nil {
		return 0, errors.New("metrics URL must be an absolute URL")
	}
	if err := validateLaunchEndpoint(parsed); err != nil {
		return 0, err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/query"
	values := parsed.Query()
	values.Set("query", query)
	parsed.RawQuery = values.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return 0, err
	}
	if err := credential.apply(request); err != nil {
		return 0, err
	}
	response, err := newLaunchHttpClient().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	maximumBytes := defaultLaunchHttpReadSettings().MaximumResponseBytes
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil {
		return 0, err
	}
	if response.StatusCode != http.StatusOK || maximumBytes < int64(len(body)) {
		return 0, fmt.Errorf("metrics query returned HTTP %d", response.StatusCode)
	}
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Status != "success" || len(result.Data.Result) != 1 || len(result.Data.Result[0].Value) != 2 {
		return 0, errors.New("metrics query did not return one scalar result")
	}
	var valueString string
	if err := json.Unmarshal(result.Data.Result[0].Value[1], &valueString); err != nil {
		return 0, errors.New("metrics scalar is malformed")
	}
	value, err := strconv.ParseFloat(valueString, 64)
	if err != nil || !finite(value) {
		return 0, errors.New("metrics scalar is not finite")
	}
	return value, nil
}

func getLaunchJson(ctx context.Context, client *http.Client, baseUrl string, requestPath string, token string, result any) error {
	body, err := getLaunchBytes(ctx, client, baseUrl, requestPath, token)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(result); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("HTTP response contains trailing JSON")
	}
	return nil
}

// Permits plaintext only for isolated loopback conformance servers; live
// bearer-bearing probes require transport encryption.
func validateLaunchEndpoint(parsed *url.URL) error {
	if parsed == nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return errors.New("launch endpoint must be an absolute URL without userinfo")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	hostname := parsed.Hostname()
	address := net.ParseIP(hostname)
	if parsed.Scheme == "http" && (hostname == "localhost" || (address != nil && address.IsLoopback())) {
		return nil
	}
	return errors.New("launch endpoint requires HTTPS outside loopback tests")
}

// Refuses redirects so no credential can cross the configured endpoint
// boundary, including redirects that Go would otherwise consider related.
func newLaunchHttpClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Transport: transport,
		Timeout:   defaultLaunchHttpReadSettings().RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func getLaunchBytes(ctx context.Context, client *http.Client, baseUrl string, requestPath string, token string) ([]byte, error) {
	return getLaunchBytesWithCredential(ctx, client, baseUrl, requestPath, launchHttpCredential{bearerToken: token})
}

// Credential selection is typed private process state, not userinfo, an
// arbitrary supplied header or a field copied into the public proof envelope.
func getLaunchBytesWithCredential(ctx context.Context, client *http.Client, baseUrl, requestPath string, credential launchHttpCredential) ([]byte, error) {
	parsed, err := url.Parse(baseUrl)
	if err != nil {
		return nil, errors.New("launch endpoint must be an absolute URL without userinfo")
	}
	if err := validateLaunchEndpoint(parsed); err != nil {
		return nil, err
	}
	reference, err := url.Parse(requestPath)
	if err != nil {
		return nil, err
	}
	resolved := parsed.ResolveReference(reference)
	if resolved.Scheme != parsed.Scheme || resolved.Host != parsed.Host {
		return nil, errors.New("launch endpoint path escaped its configured origin")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, resolved.String(), nil)
	if err != nil {
		return nil, err
	}
	if err := credential.apply(request); err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	maximumBytes := defaultLaunchHttpReadSettings().MaximumResponseBytes
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned HTTP %d", requestPath, response.StatusCode)
	}
	if maximumBytes < int64(len(body)) {
		return nil, errors.New("launch HTTP response exceeded the size limit")
	}
	return body, nil
}

func readPrivateToken(filePath string) (string, error) {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return "", fmt.Errorf("credential file %s must be a private regular file", filePath)
	}
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(content))
	clear(content)
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", fmt.Errorf("credential file %s is malformed", filePath)
	}
	return token, nil
}

func runHandoffManifest(opts docopt.Opts) {
	epochNumber, err := configuredEpoch(opts)
	if err != nil {
		fatalf("handoff manifest epoch: %s", err)
	}
	sourceConfig, _, _, err := configuredSourcePaths(opts)
	if err != nil {
		fatalf("handoff manifest source: %s", err)
	}
	manifest, err := buildLaunchHandoffManifest(
		epochNumber,
		sourceConfig,
		optString(opts, "--evaluator-image", ""),
		optString(opts, "--openapi", ""),
		optString(opts, "--baseline-manifest", ""),
		optString(opts, "--preflight", ""),
		splitNonempty(optString(opts, "--staging-evidence", "")),
		server.NowUtc(),
	)
	if err != nil {
		fatalf("handoff manifest: %s", err)
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fatalf("handoff manifest encode: %s", err)
	}
	encoded = append(encoded, '\n')
	outputPath := optString(opts, "--out", "")
	if outputPath == "" {
		fmt.Print(string(encoded))
		return
	}
	if err := writeAtomicFile(outputPath, encoded, 0644); err != nil {
		fatalf("handoff manifest write: %s", err)
	}
}

func buildLaunchHandoffManifest(
	epochNumber int,
	sourceConfigPath string,
	evaluatorImageDigest string,
	openApiPath string,
	baselineManifestPath string,
	preflightPath string,
	stagingPaths []string,
	now time.Time,
) (*launchHandoffManifest, error) {
	if !strings.HasPrefix(evaluatorImageDigest, "sha256:") || !validSha256(strings.TrimPrefix(evaluatorImageDigest, "sha256:")) {
		return nil, errors.New("evaluator image must be an immutable SHA-256 digest")
	}
	sourceManifest, err := loadSourceManifest(sourceConfigPath)
	if err != nil {
		return nil, err
	}
	epoch, err := sourceManifest.epoch(epochNumber)
	if err != nil {
		return nil, err
	}
	sourceEvidence, err := inspectEvidenceFile(sourceConfigPath, maximumSourceConfigBytes)
	if err != nil {
		return nil, err
	}
	openApiEvidence, err := inspectEvidenceFile(openApiPath, maximumSourceConfigBytes)
	if err != nil {
		return nil, err
	}
	baselineEvidence, err := inspectEvidenceFile(baselineManifestPath, 16*1024*1024)
	if err != nil {
		return nil, err
	}
	preflightEvidence, err := inspectEvidenceFile(preflightPath, 16*1024*1024)
	if err != nil {
		return nil, err
	}
	preflightBytes, err := os.ReadFile(preflightPath)
	if err != nil {
		return nil, err
	}
	var preflight launchPreflightResult
	if err := decodeStrictJSONBytes(preflightBytes, &preflight, "launch preflight"); err != nil || preflight.Schema != launchEvidenceSchema || !preflight.Passed || preflight.Epoch != epochNumber ||
		preflight.EvaluatorImageDigest != evaluatorImageDigest || preflight.OpenApiSha256 != openApiEvidence.Sha256 || preflight.SourceConfigSha256 != sourceEvidence.Sha256 {
		return nil, errors.New("preflight evidence does not authenticate this handoff")
	}
	for _, name := range mandatoryLaunchChecks {
		check, found := preflight.Checks[name]
		if !found || !check.Passed {
			return nil, fmt.Errorf("preflight evidence is missing passing check %s", name)
		}
	}
	stagingEvidence := make([]handoffEvidence, 0, len(stagingPaths))
	for _, stagingPath := range stagingPaths {
		evidence, err := inspectEvidenceFile(stagingPath, 64*1024*1024)
		if err != nil {
			return nil, err
		}
		stagingEvidence = append(stagingEvidence, evidence)
	}

	result := &launchHandoffManifest{
		Schema:           launchEvidenceSchema,
		GeneratedAt:      now.UTC(),
		Epoch:            epochNumber,
		OpenApi:          openApiEvidence,
		BaselineManifest: baselineEvidence,
		Preflight:        preflightEvidence,
		StagingEvidence:  stagingEvidence,
	}
	result.Source.Branch = sourceManifest.EvaluationSource.Branch
	result.Source.Repositories = epoch.Repositories.commits()
	result.Source.Config = sourceEvidence
	result.Evaluator.ImageDigest = evaluatorImageDigest
	result.ControlPlane.ApiBranch = sourceManifest.ControlPlaneIdentity.ApiBranch
	result.ControlPlane.WorkerBranch = sourceManifest.ControlPlaneIdentity.WorkerBranch
	result.ControlPlane.FreezeMainCommits = sourceManifest.ControlPlaneIdentity.FreezeMainCommits
	result.ControlPlane.PersistRuntimeImageEach = sourceManifest.ControlPlaneIdentity.PersistPerEvaluation
	result.Operations.EvidenceDeletionOwner = "support@ur.xyz"
	result.Operations.IncidentContact = "support@ur.xyz"
	result.Apex.Status = "external_signature_and_registry_identifier_required"
	return result, nil
}

func inspectEvidenceFile(filePath string, maximumBytes int64) (handoffEvidence, error) {
	if strings.TrimSpace(filePath) == "" {
		return handoffEvidence{}, errors.New("evidence path is required")
	}
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || maximumBytes < info.Size() {
		return handoffEvidence{}, fmt.Errorf("evidence file %s is missing, unsafe, empty, or oversized", filePath)
	}
	file, err := os.Open(filePath)
	if err != nil {
		return handoffEvidence{}, err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximumBytes+1))
	if err != nil || written != info.Size() {
		return handoffEvidence{}, errors.New("evidence file changed while hashing")
	}
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return handoffEvidence{}, err
	}
	return handoffEvidence{Path: absPath, Sha256: hex.EncodeToString(hash.Sum(nil)), Bytes: written}, nil
}

func splitNonempty(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
