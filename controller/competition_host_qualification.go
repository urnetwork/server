// Qualifies the real evaluator host for one reserved source transition. These
// small, explicitly labeled probes never create public jobs or epoch baselines.
package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const competitionHostQualificationProfile = "sim-latency-host-readiness/v1"
const competitionHostQualificationTimeout = 15 * time.Minute
const competitionHostQualificationComment = "// competitionHostReadinessProbe exercises the authenticated evaluator without changing behavior."

// These independent root-provisioned pins are not accepted through the
// supervisor request. The bomb report must belong to the current Linux boot.
type CompetitionHostQualificationConfig struct {
	Schema             int    `json:"schema"`
	Environment        string `json:"environment"`
	CompetitionId      string `json:"competition_id"`
	SeasonId           string `json:"season_id"`
	Staging            bool   `json:"staging"`
	HostConfig         string `json:"host_config"`
	StateDirectory     string `json:"state_directory"`
	PromotionCommand   string `json:"promotion_command"`
	PromotionSha256    string `json:"promotion_sha256"`
	ResourceBombReport string `json:"resource_bomb_report"`
	ResourceBombSha256 string `json:"resource_bomb_sha256"`
	ResourceBombBootId string `json:"resource_bomb_boot_id"`
	WorkerCommand      string `json:"worker_command"`
	WorkerSha256       string `json:"worker_sha256"`
	WorkerImageDigest  string `json:"worker_image_digest"`
	// Optional deployment restriction; retained evidence always records the
	// independently observed API identity for this exact source intent.
	ApiImageDigest              string `json:"api_image_digest"`
	ReviewedBuildStateDirectory string `json:"reviewed_build_state_directory,omitempty"`
}

// A successful result identifies retained proof, not a worker readiness
// receipt. Only the independently executed worker may acknowledge readiness.
type CompetitionHostQualificationResult struct {
	Profile             string `json:"profile"`
	ProfileSha256       string `json:"profile_sha256"`
	IntentSha256        string `json:"intent_sha256"`
	TargetRoundId       string `json:"target_round_id"`
	ArtifactManifestKey string `json:"artifact_manifest_key"`
	ArtifactVersionId   string `json:"artifact_version_id"`
	ArtifactSha256      string `json:"artifact_sha256"`
	Cached              bool   `json:"cached"`
}

// Only this marker shape is emitted by the qualifier; its extra provenance
// keeps the one-replicate readiness probe distinct from the scoring control.
type competitionHostQualificationMarker struct {
	Schema        int              `json:"schema"`
	Kind          string           `json:"kind"`
	Passed        bool             `json:"passed"`
	RoundId       string           `json:"round_id"`
	BootId        string           `json:"boot_id"`
	ProfileSha256 string           `json:"profile_sha256"`
	IntentSha256  string           `json:"intent_sha256"`
	Receipt       retainedArtifact `json:"receipt"`
}

// Existing host configuration contains additional hardware facts. Preserve
// them byte-for-value while updating only the authenticated rebaseline link.
type competitionQualificationHostConfig struct {
	Schema                   int    `json:"schema"`
	HardwareId               string `json:"hardware_id"`
	ImageDigest              string `json:"image_digest"`
	QualificationSha256      string `json:"qualification_sha256"`
	ConfigLocalDirectory     string `json:"config_local_directory"`
	VaultLocalDirectory      string `json:"vault_local_directory"`
	ConfigLocalSha256        string `json:"config_local_sha256"`
	VaultLocalSha256         string `json:"vault_local_sha256"`
	RebaselineManifest       string `json:"rebaseline_manifest"`
	RebaselineManifestSha256 string `json:"rebaseline_manifest_sha256"`
}

// Rejects symlinks and writable ancestors, not just an apparently safe leaf.
// All qualification authority is root-owned, including the installed programs.
func competitionQualificationRootPath(path string, directory bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("qualification path must be absolute, canonical and non-root")
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 || info.Mode()&os.ModeSymlink != 0 ||
			(current != path || directory) && !info.IsDir() || current == path && !directory && !info.Mode().IsRegular() {
			return errors.New("qualification path or ancestor is not root-owned and protected")
		}
		if current == "/" {
			return nil
		}
	}
}

// Configuration is a bounded strict document outside candidate workspaces.
func LoadCompetitionHostQualificationConfig(path string) (*CompetitionHostQualificationConfig, error) {
	if err := competitionQualificationRootPath(path, false); err != nil {
		return nil, err
	}
	content, err := readCompetitionPreparationFile(path, 64*1024, false)
	if err != nil {
		return nil, err
	}
	config := &CompetitionHostQualificationConfig{}
	if err := decodeCompetitionPreparationJson(bytes.NewReader(content), 64*1024, config); err != nil {
		return nil, err
	}
	return config, nil
}

// The fixed readiness workload exercises both pristine and comment-only
// candidate paths. The evaluator wire budget stays unchanged; the owner gives
// the entire operation a separate fifteen-minute cancellation boundary.
func competitionHostQualificationSettings(settings *Settings) *Settings {
	copy := *settings
	policy := &copy.EvaluationPolicy
	policy.ProviderCount, policy.ClientPoolSize, policy.ArrivalsPerMinute, policy.QualityWindowSize = 32, 8, 16, 8
	policy.ExchangeHosts, policy.FleetShards = 1, 0
	policy.RampMs, policy.PrewarmMs, policy.SettleMs = 0, 5000, 0
	policy.ClientWarmupTimeoutMs, policy.DurationMs, policy.RequestTimeoutMs = 60000, 30000, 30000
	policy.PipelineIntervalMs, policy.TestTimeoutMs, policy.AnnounceTimeoutMs = 100, 3000, 2000
	policy.ImpairmentEnabled, policy.Replicates = false, 1
	return &copy
}

// The authenticated durable receipt freezes identity for one intent. Main may
// advance between intents without pretending that the evaluated source changed.
// An optional deployment restriction still rejects any other API image.
func competitionSourceApiRuntimeIdentity(intent *model.CompetitionSourceActivation, expectedImage string, receipts []model.CompetitionSourceReadiness) (string, error) {
	if expectedImage != "" && !imageDigestPattern.MatchString(expectedImage) {
		return "", errors.New("API runtime restriction is malformed")
	}
	count := 0
	image := ""
	for _, receipt := range receipts {
		if receipt.Role != "api" {
			continue
		}
		count++
		if !sourcePreparationReceiptMatches(intent, receipt) || (expectedImage != "" && receipt.RuntimeImageDigest != expectedImage) {
			return "", errors.New("host qualification requires the exact genuine API readiness receipt")
		}
		image = receipt.RuntimeImageDigest
	}
	if count != 1 {
		return "", errors.New("host qualification requires exactly one durable API role receipt")
	}
	return image, nil
}

// An existing singleton lease blocks every ordinary worker Claim. No public
// job is inserted: the slot's nullable job id remains null throughout this probe.
func reserveCompetitionQualificationSlot(ctx context.Context, owner string, until time.Time) error {
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var worker *string
			var job *server.Id
			var deadline *time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT worker_id,job_id,lease_expires_at FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`).Scan(&worker, &job, &deadline))
			if job != nil || worker != nil && deadline != nil && server.NowUtc().Before(*deadline) {
				panic(ErrEvaluationRunning)
			}
			var running bool
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_job WHERE state='running')`).Scan(&running))
			if running {
				panic(ErrEvaluationRunning)
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=$1,job_id=NULL,lease_expires_at=$2,heartbeat_at=$3 WHERE slot_id=1`, owner, until, server.NowUtc()))
		}, server.OptReadWrite())
	})
}

// Expiry is longer than the operation plus bounded process cleanup. Release
// uses the unique owner and cannot erase a worker that acquired a later lease.
func releaseCompetitionQualificationSlot(ctx context.Context, owner string) error {
	return captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE competition_worker_slot SET worker_id=NULL,job_id=NULL,lease_expires_at=NULL,heartbeat_at=$2 WHERE slot_id=1 AND worker_id=$1 AND job_id IS NULL`, owner, server.NowUtc()))
		}, server.OptReadWrite())
	})
}

// The supervisor lease and singleton slot are checked independently; neither
// is renewed by this child after its supervising process has disappeared.
func checkCompetitionQualificationSlot(ctx context.Context, owner string) error {
	return captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var held bool
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(worker_id=$1 AND job_id IS NULL AND lease_expires_at>$2,false) FROM competition_worker_slot WHERE slot_id=1`, owner, server.NowUtc()).Scan(&held))
			if !held {
				panic(ErrLeaseLost)
			}
		}, server.OptReadWrite())
	})
}

// Only root-owned configuration names privileged output paths and programs.
// Source, image, local leaves and the independently captured worker identity
// must all agree before any Docker process or marker mutation is possible.
func validateCompetitionQualificationConfig(settings *Settings, config CompetitionHostQualificationConfig, bootId, workerImage string) error {
	if settings == nil || config.Schema != 1 || config.Environment != settings.AutomationPolicy.Environment || config.CompetitionId != settings.CompetitionId ||
		config.SeasonId != settings.AutomationPolicy.SeasonId || config.Staging != settings.AutomationPolicy.Staging ||
		(config.ApiImageDigest != "" && !imageDigestPattern.MatchString(config.ApiImageDigest)) || !imageDigestPattern.MatchString(config.WorkerImageDigest) || workerImage != config.WorkerImageDigest ||
		len(bootId) != 36 || config.ResourceBombBootId != bootId || !sha256Pattern.MatchString(config.ResourceBombSha256) {
		return errors.New("qualification scope, actual worker image or current-boot bomb evidence does not match")
	}
	if settings.AutomationPolicy.WinnerReviewPolicy == model.CompetitionWinnerReviewPolicy && config.ReviewedBuildStateDirectory == "" {
		return errors.New("automatic winner-source preparation requires its private reviewed-build inventory")
	}
	if config.ReviewedBuildStateDirectory != "" {
		if err := competitionQualificationRootPath(config.ReviewedBuildStateDirectory, true); err != nil {
			return err
		}
		info, err := os.Lstat(config.ReviewedBuildStateDirectory)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			return errors.New("reviewed build inventory must remain root-private")
		}
	}
	for _, command := range []struct{ path, digest string }{
		{config.WorkerCommand, config.WorkerSha256}, {config.PromotionCommand, config.PromotionSha256},
		{settings.EvaluatorCommand, settings.EvaluatorCommandSha256}, {settings.SelfCheckCommand, settings.SelfCheckCommandSha256},
	} {
		if err := competitionQualificationRootPath(command.path, false); err != nil {
			return err
		}
		if err := verifyPinnedExecutable(command.path, command.digest); err != nil {
			return err
		}
	}
	for _, file := range []string{config.HostConfig, config.ResourceBombReport, "/usr/bin/docker"} {
		if err := competitionQualificationRootPath(file, false); err != nil {
			return err
		}
	}
	if config.HostConfig != "/etc/urnetwork/competition-host.json" {
		return errors.New("qualification must update the host configuration actually read by self-check")
	}
	if err := competitionQualificationRootPath(config.StateDirectory, true); err != nil {
		return err
	}
	digest, _, err := hashRegularFile(config.ResourceBombReport)
	if err != nil || digest != config.ResourceBombSha256 {
		return errors.New("current-boot resource-bomb evidence failed authentication")
	}
	return nil
}

// The whole host configuration stays intact; this reads only the identity
// and marker locations consumed by the root-owned checker and promoter.
func readCompetitionQualificationHostConfig(path string, settings *Settings) (*competitionQualificationHostConfig, map[string]json.RawMessage, error) {
	if err := competitionQualificationRootPath(path, false); err != nil {
		return nil, nil, err
	}
	content, err := readCompetitionPreparationFile(path, 64*1024, false)
	if err != nil {
		return nil, nil, err
	}
	var raw map[string]json.RawMessage
	var host competitionQualificationHostConfig
	if decodeFailureEvidenceJson(content, &raw) != nil || json.Unmarshal(content, &host) != nil || host.Schema != 1 ||
		host.HardwareId != settings.EvaluationPolicy.HardwareId || host.ImageDigest != settings.EvaluatorImageDigest ||
		host.QualificationSha256 != settings.EvaluationPolicy.HostQualificationSha256 ||
		host.ConfigLocalDirectory != settings.ConfigLocalDirectory || host.VaultLocalDirectory != settings.VaultLocalDirectory ||
		host.ConfigLocalSha256 != settings.EvaluationPolicy.ConfigLocalSha256 || host.VaultLocalSha256 != settings.EvaluationPolicy.VaultLocalSha256 {
		return nil, nil, errors.New("root host facts do not match the approved source and local leaves")
	}
	if !filepath.IsAbs(host.RebaselineManifest) || filepath.Clean(host.RebaselineManifest) != host.RebaselineManifest ||
		!strings.HasPrefix(host.RebaselineManifest, "/run/urnetwork/") {
		return nil, nil, errors.New("rebaseline marker must be a canonical transient host path")
	}
	if err := competitionQualificationRootPath(filepath.Dir(host.RebaselineManifest), true); err != nil {
		return nil, nil, err
	}
	return &host, raw, nil
}

// The fixed comment is appended after existing Go source, never interpolated
// into executable syntax. Source bytes come only from the authenticated image.
func competitionQualificationPatch(path string, source []byte, policy PatchPolicy) (*CanonicalPatch, error) {
	if err := validateRepositoryPath(path); err != nil || filepath.Ext(path) != ".go" || len(source) == 0 || len(source) > 1024*1024 ||
		source[len(source)-1] != '\n' || bytes.ContainsAny(source, "\x00\r") {
		return nil, errors.New("qualification source must be a bounded newline-terminated Go file")
	}
	lines := strings.Split(strings.TrimSuffix(string(source), "\n"), "\n")
	count := min(3, len(lines))
	start := len(lines) - count + 1
	var patch strings.Builder
	fmt.Fprintf(&patch, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n", path, path, path, path, start, count, start, count+2)
	for _, line := range lines[len(lines)-count:] {
		fmt.Fprintf(&patch, " %s\n", line)
	}
	fmt.Fprintf(&patch, "+\n+%s\n", competitionHostQualificationComment)
	canonical, evalError := ValidateAndCanonicalizePatch(patch.String(), policy)
	if evalError != nil {
		return nil, errors.New("trusted qualification comment is not allowed by the frozen patch policy")
	}
	return canonical, nil
}

// Reads a committed blob, not an arbitrary image working-tree path. The real
// evaluator independently verifies the image/source lock before applying it.
func readCompetitionQualificationSource(ctx context.Context, directory string, settings *Settings) (string, []byte, error) {
	paths := append([]string(nil), settings.PatchPolicy.AllowedPaths...)
	sort.Strings(paths)
	if len(paths) == 0 {
		return "", nil, errors.New("host qualification has no approved Go patch surface")
	}
	path := paths[0]
	if err := validateRepositoryPath(path); err != nil || filepath.Ext(path) != ".go" {
		return "", nil, errors.New("host qualification patch path is invalid")
	}
	stdout, stderr := &boundedBuffer{limit: 1024 * 1024}, &boundedBuffer{limit: 64 * 1024}
	exitCode, err := runContainedCommand(ctx, directory, "/usr/bin/docker", []string{
		"run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--entrypoint", "/usr/bin/git", settings.EvaluatorImageDigest, "-C", "/workspace/server", "show", settings.BaseSha + ":" + path,
	}, stdout, stderr)
	if err != nil || exitCode != 0 {
		return "", nil, errors.New("could not read the locked Go source from the evaluator image")
	}
	return path, stdout.Bytes(), nil
}

// Completion binds every top-level artifact; the evidence manifest binds
// every retained container, mount and cleanup observation underneath it.
type competitionQualificationCompletion struct {
	Schema           int               `json:"schema"`
	Kind             string            `json:"kind"`
	JobId            string            `json:"job_id"`
	RoundId          string            `json:"round_id"`
	Attempt          int               `json:"attempt"`
	BaseImageId      string            `json:"base_image_id"`
	CandidateImageId string            `json:"candidate_image_id"`
	PatchSha256      string            `json:"patch_sha256"`
	ProvidersSha256  string            `json:"providers_sha256"`
	CleanupComplete  bool              `json:"cleanup_complete"`
	Artifacts        map[string]string `json:"artifacts"`
}

// This closed manifest format is emitted by the pinned host evaluator.
type competitionQualificationEvidence struct {
	Schema    int                  `json:"schema"`
	Kind      string               `json:"kind"`
	JobId     string               `json:"job_id"`
	RoundId   string               `json:"round_id"`
	Artifacts []evaluationArtifact `json:"artifacts"`
}

// Parent symlinks are forbidden as well as symlink leaves. This check runs
// before the ordinary artifact authenticator opens any declared evidence.
func competitionQualificationArtifactPath(root, relative string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.HasPrefix(relative, "..") {
		return errors.New("qualification evidence path escapes its owned directory")
	}
	for directory := filepath.Dir(filepath.Join(root, relative)); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("qualification evidence has an unsafe parent")
		}
		if directory == root {
			return nil
		}
	}
}

// Gate names and diagnostic maps carry scorer data, not typed protocol fields.
// Permit their mixed-case keys without relaxing gate/score/security field names.
func decodeCompetitionQualificationResult(content []byte, result *evaluatorResult) error {
	return decodeCompetitionEvidenceJson(content, result, func(path []string) bool {
		if len(path) < 2 || path[0] != "score" {
			return false
		}
		return path[1] == "diagnostics" || path[1] == "gates" && (len(path) == 2 || len(path) >= 4 && path[3] == "details")
	})
}

// Validates real output without storing a scoring result, baseline or job.
func authenticateCompetitionQualification(directory string, request evaluatorRequest, settings *Settings) error {
	resultBytes, err := readRegularFile(filepath.Join(directory, "worker-result.json"), maxEvaluatorResultBytes)
	if err != nil {
		return err
	}
	var result evaluatorResult
	if err := decodeCompetitionQualificationResult(resultBytes, &result); err != nil {
		return err
	}
	if result.Schema != 1 || result.JobId != request.JobId || result.EvalError != nil || validateScore(result.Score) != nil || !result.Score.Placeable ||
		!scoreMatchesFrozenMargin(result.Score, request.EvaluationPolicy.TakeoverMargin) || result.Score.Significance.ReplicateCount != 1 || !result.Security.passedFor(nil) {
		return errors.New("host-readiness evaluation did not pass its score and security gates")
	}
	if len(result.Score.Gates) != 6 {
		return errors.New("host-readiness score does not contain exactly the six required gates")
	}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		if gate, ok := result.Score.Gates[name]; !ok || !gate.Passed {
			return errors.New("host-readiness score has a missing or failed required gate")
		}
	}
	for _, artifact := range result.Artifacts {
		if err := competitionQualificationArtifactPath(directory, artifact.Path); err != nil {
			return err
		}
	}
	artifacts, err := authenticateArtifactsRequired(directory, result.Artifacts, map[string]bool{
		"accounting.json": false, "baseline.json": false, "resources.json": false, "score.json": false,
		"evaluation.complete.json": false, "evidence-manifest.json": false,
	})
	if err != nil {
		return err
	}
	artifactDigests := map[string]string{}
	for _, artifact := range artifacts {
		artifactDigests[artifact.Path] = artifact.Sha256
	}
	completionBytes, err := readRegularFile(filepath.Join(directory, "evaluation.complete.json"), 64*1024)
	if err != nil {
		return err
	}
	var completion competitionQualificationCompletion
	if err := decodeCompetitionPreparationJson(bytes.NewReader(completionBytes), 64*1024, &completion); err != nil {
		return err
	}
	if completion.Schema != 1 || completion.Kind != "sim-latency-worker-evaluation-complete" || completion.JobId != request.JobId || completion.RoundId != request.RoundId ||
		completion.Attempt != request.Attempt || completion.BaseImageId != request.EvaluatorImageDigest || !imageDigestPattern.MatchString(completion.CandidateImageId) ||
		completion.PatchSha256 != request.PatchSha256 || completion.ProvidersSha256 != request.ProvidersSha256 || !completion.CleanupComplete || len(completion.Artifacts) != 5 {
		return errors.New("host-readiness completion does not bind the exact probe")
	}
	for kind, file := range map[string]string{"accounting": "accounting.json", "baseline": "baseline.json", "resources": "resources.json", "score": "score.json", "evidence_manifest": "evidence-manifest.json"} {
		if completion.Artifacts[kind] != artifactDigests[file] {
			return errors.New("host-readiness completion artifact hash mismatch")
		}
	}
	// Match the pinned evaluator's projection from the complete scorer document,
	// whose extra diagnostic fields are not part of the public score type.
	scoreBytes, err := readRegularFile(filepath.Join(directory, "score.json"), maxEvaluatorResultBytes)
	if err != nil {
		return err
	}
	var scoreDocument struct {
		ScoreResult
		EvalError json.RawMessage `json:"eval_error"`
	}
	if json.Unmarshal(scoreBytes, &scoreDocument) != nil || len(scoreDocument.EvalError) != 0 && !bytes.Equal(bytes.TrimSpace(scoreDocument.EvalError), []byte("null")) {
		return errors.New("host-readiness scorer document contains an evaluation error")
	}
	scoreDocument.TakeoverEligible, _ = scoreDocument.Diagnostics["baseline_takeover_eligible"].(bool)
	expectedScore, err := json.Marshal(&scoreDocument.ScoreResult)
	if err != nil {
		return err
	}
	actualScore, err := json.Marshal(result.Score)
	if err != nil || !bytes.Equal(actualScore, expectedScore) {
		return errors.New("host-readiness worker score differs from its authenticated scorer document")
	}
	baselineBytes, err := readRegularFile(filepath.Join(directory, "baseline.json"), maxRoundBaselineBytes)
	if err != nil {
		return err
	}
	policy, err := policySnapshot(settings)
	if err != nil {
		return err
	}
	roundId, err := server.ParseId(request.RoundId)
	if err != nil {
		return err
	}
	baselineDigest, err := validateRoundBaselineJson(settings, &roundRecord{RoundResult: RoundResult{RoundId: roundId, ProvidersSha256: request.ProvidersSha256}, PolicyJson: policy}, baselineBytes)
	if err != nil || result.Score.Diagnostics["baseline_sha256"] != baselineDigest {
		return errors.New("host-readiness control is not authenticated; it is not an epoch baseline")
	}
	evidenceBytes, err := readRegularFile(filepath.Join(directory, "evidence-manifest.json"), 8*1024*1024)
	if err != nil {
		return err
	}
	var evidence competitionQualificationEvidence
	if err := decodeCompetitionPreparationJson(bytes.NewReader(evidenceBytes), 8*1024*1024, &evidence); err != nil {
		return err
	}
	if evidence.Schema != 1 || evidence.Kind != "sim-latency-evidence-manifest" || evidence.JobId != request.JobId || evidence.RoundId != request.RoundId || len(evidence.Artifacts) == 0 || len(evidence.Artifacts) > 20000 {
		return errors.New("host-readiness evidence identity is invalid")
	}
	for _, artifact := range evidence.Artifacts {
		if !strings.HasPrefix(artifact.Path, "evidence/") {
			return errors.New("host-readiness evidence is outside its evidence tree")
		}
		if err := competitionQualificationArtifactPath(directory, artifact.Path); err != nil {
			return err
		}
	}
	_, err = authenticateArtifactsRequired(directory, evidence.Artifacts, map[string]bool{"evidence/local-mounts.json": false, "evidence/docker-id-map.json": false})
	return err
}

// Every file and the final inventory are retained under a distinct namespace.
// No public round, baseline, job, or reward record is involved in this archive.
func retainCompetitionQualification(ctx context.Context, settings *Settings, intent *model.CompetitionSourceActivation, directory, profileDigest string) (*retainedArtifact, error) {
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil {
		return nil, errors.New("host qualification requires the protected artifact archive")
	}
	store, err := archive.runtimeManifestStore(ctx)
	if err != nil {
		return nil, err
	}
	rootKey := filepath.ToSlash(filepath.Join(store.Prefix(), "competition", "v2", "host-readiness", intent.Environment, intent.CompetitionId, intent.SeasonId, intent.IntentSha256, profileDigest, filepath.Base(directory)))
	retained := []retainedArtifact{}
	var total int64
	err = filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil || len(retained) >= 20000 {
			return errors.New("host-readiness archive exceeds its file bound")
		}
		if err := competitionQualificationArtifactPath(directory, relative); err != nil {
			return err
		}
		digest, size, err := hashRegularFile(path)
		if err != nil {
			return err
		}
		total += size
		if total > 4*1024*1024*1024 {
			return errors.New("host-readiness archive exceeds its evidence budget")
		}
		receipt, err := archive.retainFile(ctx, settings, relative, rootKey+"/"+filepath.ToSlash(relative), path, digest, size, "application/octet-stream")
		if err != nil || receipt == nil || receipt.Mode != "COMPLIANCE" || receipt.VersionId == "" {
			return errors.Join(err, errors.New("host-readiness retention is not proven"))
		}
		retained = append(retained, *receipt)
		return nil
	})
	if err != nil {
		return nil, err
	}
	content, err := json.Marshal(struct {
		Schema        int                `json:"schema"`
		Kind          string             `json:"kind"`
		IntentSha256  string             `json:"intent_sha256"`
		ProfileSha256 string             `json:"profile_sha256"`
		TargetRoundId string             `json:"target_round_id"`
		Objects       []retainedArtifact `json:"objects"`
	}{Schema: 1, Kind: competitionHostQualificationProfile, IntentSha256: intent.IntentSha256, ProfileSha256: profileDigest, TargetRoundId: intent.TargetRoundId.String(), Objects: retained})
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(directory, "host-readiness-retention.json")
	if err := writeExclusiveFile(manifestPath, content, 0400); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	receipt, err := archive.retainFile(ctx, settings, "host-readiness-retention.json", rootKey+"/host-readiness-retention.json", manifestPath, hex.EncodeToString(digest[:]), int64(len(content)), "application/json")
	if err != nil || !competitionQualificationReceiptValid(receipt, settings.RetainUntil) {
		return nil, errors.Join(err, errors.New("host-readiness inventory retention is not proven"))
	}
	return receipt, nil
}

// Only a content-addressed, versioned, retained inventory can support reuse.
func competitionQualificationReceiptValid(receipt *retainedArtifact, retainUntil time.Time) bool {
	return receipt != nil && receipt.Path == "host-readiness-retention.json" && receipt.Key != "" &&
		sha256Pattern.MatchString(receipt.Sha256) && receipt.Bytes > 0 && receipt.VersionId != "" &&
		receipt.Mode == "COMPLIANCE" && !receipt.RetainUntil.Before(retainUntil.Add(-time.Second))
}

// A transient exact-round marker never authorizes another profile, boot, or
// source generation. Live host checks and retained-byte verification follow.
func competitionQualificationMarkerMatches(marker competitionHostQualificationMarker, intent *model.CompetitionSourceActivation, profileDigest, bootId string, retainUntil time.Time) bool {
	return intent != nil && marker.Schema == 1 && marker.Kind == competitionHostQualificationProfile && marker.Passed &&
		marker.BootId == bootId && marker.ProfileSha256 == profileDigest && marker.IntentSha256 == intent.IntentSha256 && marker.RoundId == intent.TargetRoundId.String() &&
		competitionQualificationReceiptValid(&marker.Receipt, retainUntil)
}

// Cached markers remain fail-closed if their immutable retained inventory is
// absent or corrupt, even while runtime-envelope storage is otherwise healthy.
func readCompetitionQualificationReceipt(ctx context.Context, settings *Settings, receipt retainedArtifact) error {
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil || !competitionQualificationReceiptValid(&receipt, settings.RetainUntil) {
		return errors.New("cached host-readiness retention identity is invalid")
	}
	reader, err := archive.store.GetVersion(ctx, receipt.Key, receipt.VersionId)
	if err != nil {
		return err
	}
	hash := sha256.New()
	count, readErr := io.Copy(hash, io.LimitReader(reader, receipt.Bytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || count != receipt.Bytes || hex.EncodeToString(hash.Sum(nil)) != receipt.Sha256 {
		return errors.New("cached host-readiness retained inventory failed authentication")
	}
	return nil
}

// Replaces one protected file atomically and durably. Marker-first/config-last
// ordering keeps any interruption fail-closed through the old pinned hash.
func replaceCompetitionQualificationFile(path string, content []byte) error {
	parent := filepath.Dir(path)
	if err := competitionQualificationRootPath(parent, true); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := competitionQualificationRootPath(path, false); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(parent, ".competition-qualification-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// Production binds all side effects here. The caller cannot supply an
// evaluator result, marker, successful callback, or claimed API acknowledgment.
func RunCompetitionHostQualification(ctx context.Context, settings *Settings, config CompetitionHostQualificationConfig, request CompetitionPreparationRequest) (*CompetitionHostQualificationResult, error) {
	if os.Geteuid() != 0 || server.RequireEnv() != "main" {
		return nil, errors.New("host qualification requires the trusted root main-environment launcher")
	}
	ctx, timeoutCancel := context.WithTimeout(ctx, competitionHostQualificationTimeout)
	defer timeoutCancel()
	intent, err := CheckCompetitionPreparationFence(ctx, settings, request)
	if err != nil {
		return nil, err
	}
	request.Intent = *intent
	settings, err = sourceSettingsFromIntent(settings, intent)
	if err != nil {
		return nil, err
	}
	bootBytes, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return nil, err
	}
	bootId := strings.TrimSpace(string(bootBytes))
	workerImage, err := runtimeImageDigest()
	if err != nil {
		return nil, err
	}
	if err := validateCompetitionQualificationConfig(settings, config, bootId, workerImage); err != nil {
		return nil, err
	}
	if err := LoadCompetitionRuntimeManifestContext(ctx, settings, intent); err != nil {
		return nil, err
	}
	receipts, err := (PostgresStore{}).sourcePreparationReceipts(ctx, settings, intent.Generation)
	if err != nil {
		return nil, err
	}
	apiImage, err := competitionSourceApiRuntimeIdentity(intent, config.ApiImageDigest, receipts)
	if err != nil {
		return nil, err
	}
	// Retained qualification evidence records the observed release even when
	// deployment configuration deliberately does not pin continuously moving main.
	config.ApiImageDigest = apiImage
	profileSettings := competitionHostQualificationSettings(settings)
	profileBytes, err := json.Marshal(struct {
		Kind            string                             `json:"kind"`
		IntentSha256    string                             `json:"intent_sha256"`
		Policy          EvaluationPolicy                   `json:"evaluation_policy"`
		Config          CompetitionHostQualificationConfig `json:"qualification_config"`
		EvaluatorSha256 string                             `json:"evaluator_sha256"`
	}{Kind: competitionHostQualificationProfile, IntentSha256: intent.IntentSha256, Policy: profileSettings.EvaluationPolicy, Config: config, EvaluatorSha256: settings.EvaluatorCommandSha256})
	if err != nil {
		return nil, err
	}
	profileHash := sha256.Sum256(profileBytes)
	profileDigest := hex.EncodeToString(profileHash[:])
	owner := "host-qualification-" + server.NewId().String()
	if err := reserveCompetitionQualificationSlot(ctx, owner, server.NowUtc().Add(competitionHostQualificationTimeout+2*time.Minute)); err != nil {
		return nil, err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = releaseCompetitionQualificationSlot(releaseCtx, owner)
	}()
	operationCtx, stop := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-operationCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			probeCtx, cancel := context.WithTimeout(operationCtx, 5*time.Second)
			_, err := CheckCompetitionPreparationFence(probeCtx, settings, request)
			if err == nil {
				err = checkCompetitionQualificationSlot(probeCtx, owner)
			}
			cancel()
			if err != nil {
				stop(err)
				return
			}
		}
	}()
	defer func() { stop(nil); <-done }()
	ctx = operationCtx
	// Source construction pins the next image's expected hardware fingerprint,
	// not its runtime readiness. Install only that reviewed transition after
	// acquiring the singleton slot, then perform the full real qualification.
	if err := ensureCompetitionReviewedHostConfig(ctx, settings, config, request, owner, bootId); err != nil {
		return nil, err
	}
	hostConfig, _, err := readCompetitionQualificationHostConfig(config.HostConfig, settings)
	if err != nil {
		return nil, err
	}
	if markerBytes, readErr := readRegularFile(hostConfig.RebaselineManifest, 64*1024); readErr == nil && competitionQualificationRootPath(hostConfig.RebaselineManifest, false) == nil {
		var marker competitionHostQualificationMarker
		digest := sha256.Sum256(markerBytes)
		if decodeCompetitionPreparationJson(bytes.NewReader(markerBytes), 64*1024, &marker) == nil &&
			competitionQualificationMarkerMatches(marker, intent, profileDigest, bootId, settings.RetainUntil) && hex.EncodeToString(digest[:]) == hostConfig.RebaselineManifestSha256 {
			if host, checkErr := (CommandEvaluator{}).SelfCheck(ctx, settings); checkErr == nil && sourceWorkerHostReady(host, settings, intent) {
				if err := readCompetitionQualificationReceipt(ctx, settings, marker.Receipt); err != nil {
					return nil, err
				}
				if _, err := CheckCompetitionPreparationFence(ctx, settings, request); err != nil {
					return nil, err
				}
				if err := checkCompetitionQualificationSlot(ctx, owner); err != nil {
					return nil, err
				}
				return &CompetitionHostQualificationResult{Profile: competitionHostQualificationProfile, ProfileSha256: profileDigest, IntentSha256: intent.IntentSha256, TargetRoundId: marker.RoundId,
					ArtifactManifestKey: marker.Receipt.Key, ArtifactVersionId: marker.Receipt.VersionId, ArtifactSha256: marker.Receipt.Sha256, Cached: true}, nil
			}
		}
	}
	directory, err := os.MkdirTemp(config.StateDirectory, "probe-"+intent.IntentSha256+"-")
	if err != nil {
		return nil, err
	}
	// Failed attempts are retained for diagnosis; never remove their evidence.
	if err := writeExclusiveFile(filepath.Join(directory, "host-readiness-profile.json"), profileBytes, 0400); err != nil {
		return nil, err
	}
	bombBytes, err := readCompetitionPreparationFile(config.ResourceBombReport, 64*1024, false)
	if err != nil {
		return nil, err
	}
	bombDigest := sha256.Sum256(bombBytes)
	if hex.EncodeToString(bombDigest[:]) != config.ResourceBombSha256 {
		return nil, errors.New("current-boot resource evidence changed before archival")
	}
	if err := writeExclusiveFile(filepath.Join(directory, "host-readiness-resource-bomb.json"), bombBytes, 0400); err != nil {
		return nil, err
	}
	path, source, err := readCompetitionQualificationSource(ctx, directory, profileSettings)
	if err != nil {
		return nil, err
	}
	patch, err := competitionQualificationPatch(path, source, settings.PatchPolicy)
	if err != nil {
		return nil, err
	}
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	seedHex := hex.EncodeToString(seed)
	clear(seed)
	workload, err := (ControlPlaneWorkloadGenerator{}).Generate(ctx, profileSettings, intent.TargetRoundId, seedHex)
	if err != nil {
		return nil, err
	}
	defer os.Remove(workload.Path)
	providers, err := readRegularFile(workload.Path, maxProvidersFileSize)
	if err != nil {
		return nil, err
	}
	providersPath, err := materializeRoundWorkload(directory, providers)
	clear(providers)
	if err != nil {
		return nil, err
	}
	patchPath := filepath.Join(directory, "canonical.patch")
	if err := writeExclusiveFile(patchPath, patch.Bytes, 0400); err != nil {
		return nil, err
	}
	evaluationRequest := evaluatorRequest{Staging: intent.Staging, Schema: 1, JobId: server.NewId().String(), RoundId: intent.TargetRoundId.String(), SourceEpoch: intent.SourceGeneration, Attempt: 1,
		CompetitionId: intent.CompetitionId, BaseSha: settings.BaseSha, EvaluatorImageDigest: intent.EvaluatorImageDigest, ApiImageDigest: config.ApiImageDigest, WorkerImageDigest: workerImage,
		ScorerVersion: ScorerVersion, RoundSeedHex: seedHex, ProvidersPath: providersPath, ProvidersSha256: workload.Sha256, PatchPath: patchPath, PatchSha256: patch.Sha256, ArtifactDirectory: directory,
		ConfigLocalDirectory: settings.ConfigLocalDirectory, VaultLocalDirectory: settings.VaultLocalDirectory, PatchPolicy: settings.PatchPolicy, EvaluationPolicy: profileSettings.EvaluationPolicy}
	requestBytes, err := json.Marshal(evaluationRequest)
	if err != nil {
		return nil, err
	}
	requestPath := filepath.Join(directory, "worker-request.json")
	if err := writeExclusiveFile(requestPath, requestBytes, 0400); err != nil {
		return nil, err
	}
	stderr, err := os.OpenFile(filepath.Join(directory, "worker.stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return nil, err
	}
	exitCode, runErr := runContainedCommand(ctx, directory, settings.EvaluatorCommand, []string{"--request", requestPath, "--result", filepath.Join(directory, "worker-result.json")}, &boundedBuffer{limit: 1024 * 1024}, stderr)
	closeErr := stderr.Close()
	if runErr != nil || closeErr != nil || exitCode != 0 {
		return nil, fmt.Errorf("host-readiness evaluator did not complete; restricted evidence retained: %w", errors.Join(runErr, closeErr, fmt.Errorf("exit %d", exitCode)))
	}
	afterRequest, err := readRegularFile(requestPath, 2*1024*1024)
	if err != nil || !bytes.Equal(afterRequest, requestBytes) {
		return nil, errors.New("host-readiness request changed during evaluation")
	}
	if err := authenticateCompetitionQualification(directory, evaluationRequest, profileSettings); err != nil {
		return nil, err
	}
	receipt, err := retainCompetitionQualification(ctx, settings, intent, directory, profileDigest)
	if err != nil {
		return nil, err
	}
	if _, err := CheckCompetitionPreparationFence(ctx, settings, request); err != nil {
		return nil, err
	}
	if err := checkCompetitionQualificationSlot(ctx, owner); err != nil {
		return nil, err
	}
	if err := validateCompetitionQualificationConfig(settings, config, bootId, workerImage); err != nil {
		return nil, err
	}
	exitCode, err = runContainedCommand(ctx, directory, config.PromotionCommand, []string{"--host-config", config.HostConfig, "--evaluation-dir", directory, "--resource-bomb-report", config.ResourceBombReport}, &boundedBuffer{limit: 1024 * 1024}, &boundedBuffer{limit: 64 * 1024})
	if err != nil || exitCode != 0 {
		return nil, errors.New("authenticated host containment promotion failed")
	}
	if _, err := CheckCompetitionPreparationFence(ctx, settings, request); err != nil {
		return nil, err
	}
	hostConfig, rawConfig, err := readCompetitionQualificationHostConfig(config.HostConfig, settings)
	if err != nil {
		return nil, err
	}
	marker := competitionHostQualificationMarker{Schema: 1, Kind: competitionHostQualificationProfile, Passed: true, RoundId: intent.TargetRoundId.String(), BootId: bootId, ProfileSha256: profileDigest, IntentSha256: intent.IntentSha256, Receipt: *receipt}
	markerBytes, err := json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	markerDigest := sha256.Sum256(markerBytes)
	rawConfig["rebaseline_manifest_sha256"], err = json.Marshal(hex.EncodeToString(markerDigest[:]))
	if err != nil {
		return nil, err
	}
	configBytes, err := json.Marshal(rawConfig)
	if err != nil {
		return nil, err
	}
	if err := replaceCompetitionQualificationFile(hostConfig.RebaselineManifest, markerBytes); err != nil {
		return nil, err
	}
	if err := replaceCompetitionQualificationFile(config.HostConfig, configBytes); err != nil {
		return nil, err
	}
	host, err := (CommandEvaluator{}).SelfCheck(ctx, settings)
	if err != nil || !sourceWorkerHostReady(host, settings, intent) {
		return nil, errors.New("promoted host does not pass exact-round live self-check")
	}
	if _, err := CheckCompetitionPreparationFence(ctx, settings, request); err != nil {
		return nil, err
	}
	if err := sealArtifactDirectory(directory); err != nil {
		return nil, err
	}
	return &CompetitionHostQualificationResult{Profile: competitionHostQualificationProfile, ProfileSha256: profileDigest, IntentSha256: intent.IntentSha256, TargetRoundId: intent.TargetRoundId.String(),
		ArtifactManifestKey: receipt.Key, ArtifactVersionId: receipt.VersionId, ArtifactSha256: receipt.Sha256}, nil
}
