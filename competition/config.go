package competition

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

const (
	ResourceName                   = "competition.yml"
	evaluationStageOverheadSeconds = int64(600)
	evaluationJobOverheadSeconds   = int64(1200)
)

var (
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gitShaPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	tokenNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Settings struct {
	Enabled                    bool             `yaml:"enabled"`
	CompetitionId              string           `yaml:"competition_id"`
	BaseSha                    string           `yaml:"base_sha"`
	EvaluatorImageDigest       string           `yaml:"evaluator_image_digest"`
	PatchPolicy                PatchPolicy      `yaml:"patch_policy"`
	EvaluationPolicy           EvaluationPolicy `yaml:"evaluation_policy"`
	ArtifactRoot               string           `yaml:"artifact_root"`
	ConfigLocalDirectory       string           `yaml:"config_local_directory"`
	VaultLocalDirectory        string           `yaml:"vault_local_directory"`
	SeasonEndsAt               time.Time        `yaml:"season_ends_at"`
	RetainUntil                time.Time        `yaml:"retain_until"`
	WorkerLeaseSeconds         int              `yaml:"worker_lease_seconds"`
	WorkerHeartbeatSeconds     int              `yaml:"worker_heartbeat_seconds"`
	HostHeartbeatMaxAgeSeconds int              `yaml:"host_heartbeat_max_age_seconds"`
	MaxInfrastructureAttempts  int              `yaml:"max_infrastructure_attempts"`
	SimulatorCommand           string           `yaml:"simulator_command"`
	EvaluatorCommand           string           `yaml:"evaluator_command"`
	EvaluatorCommandSha256     string           `yaml:"evaluator_command_sha256"`
	SelfCheckCommand           string           `yaml:"self_check_command"`
	SelfCheckCommandSha256     string           `yaml:"self_check_command_sha256"`
	Tokens                     []Token          `yaml:"-"`
	SeedKey                    []byte           `yaml:"-"`
	workloadGenerator          WorkloadGenerator
}

type settingsFile struct {
	Enabled                    bool             `yaml:"enabled"`
	CompetitionId              string           `yaml:"competition_id"`
	BaseSha                    string           `yaml:"base_sha"`
	EvaluatorImageDigest       string           `yaml:"evaluator_image_digest"`
	PatchPolicy                PatchPolicy      `yaml:"patch_policy"`
	EvaluationPolicy           EvaluationPolicy `yaml:"evaluation_policy"`
	ArtifactRoot               string           `yaml:"artifact_root"`
	ConfigLocalDirectory       string           `yaml:"config_local_directory"`
	VaultLocalDirectory        string           `yaml:"vault_local_directory"`
	SeasonEndsAt               time.Time        `yaml:"season_ends_at"`
	RetainUntil                time.Time        `yaml:"retain_until"`
	WorkerLeaseSeconds         int              `yaml:"worker_lease_seconds"`
	WorkerHeartbeatSeconds     int              `yaml:"worker_heartbeat_seconds"`
	HostHeartbeatMaxAgeSeconds int              `yaml:"host_heartbeat_max_age_seconds"`
	MaxInfrastructureAttempts  int              `yaml:"max_infrastructure_attempts"`
	SimulatorCommand           string           `yaml:"simulator_command"`
	EvaluatorCommand           string           `yaml:"evaluator_command"`
	EvaluatorCommandSha256     string           `yaml:"evaluator_command_sha256"`
	SelfCheckCommand           string           `yaml:"self_check_command"`
	SelfCheckCommandSha256     string           `yaml:"self_check_command_sha256"`
}

type secretsFile struct {
	SeedKeyBase64 string  `yaml:"seed_key_base64"`
	Tokens        []Token `yaml:"tokens"`
}

type Token struct {
	Name   string `yaml:"name"`
	Role   string `yaml:"role"`
	Sha256 string `yaml:"sha256"`
}

func LoadSettings() (*Settings, error) {
	ordinary, err := server.Config.SimpleResource(ResourceName)
	if err != nil {
		return nil, fmt.Errorf("config %s unavailable: %w", ResourceName, err)
	}
	var public settingsFile
	if err := safeUnmarshal(ordinary, &public); err != nil {
		return nil, fmt.Errorf("config %s: %w", ResourceName, err)
	}
	secretResource, err := server.Vault.SimpleResource(ResourceName)
	if err != nil {
		return nil, fmt.Errorf("vault %s unavailable: %w", ResourceName, err)
	}
	var secret secretsFile
	if err := safeUnmarshal(secretResource, &secret); err != nil {
		return nil, fmt.Errorf("vault %s: %w", ResourceName, err)
	}
	seedKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(secret.SeedKeyBase64))
	if err != nil {
		return nil, fmt.Errorf("seed_key_base64: %w", err)
	}
	s := &Settings{
		Enabled:                    public.Enabled,
		CompetitionId:              strings.TrimSpace(public.CompetitionId),
		BaseSha:                    strings.TrimSpace(public.BaseSha),
		EvaluatorImageDigest:       strings.TrimSpace(public.EvaluatorImageDigest),
		PatchPolicy:                public.PatchPolicy,
		EvaluationPolicy:           public.EvaluationPolicy,
		ArtifactRoot:               filepath.Clean(public.ArtifactRoot),
		ConfigLocalDirectory:       filepath.Clean(public.ConfigLocalDirectory),
		VaultLocalDirectory:        filepath.Clean(public.VaultLocalDirectory),
		SeasonEndsAt:               public.SeasonEndsAt.UTC(),
		RetainUntil:                public.RetainUntil.UTC(),
		WorkerLeaseSeconds:         public.WorkerLeaseSeconds,
		WorkerHeartbeatSeconds:     public.WorkerHeartbeatSeconds,
		HostHeartbeatMaxAgeSeconds: public.HostHeartbeatMaxAgeSeconds,
		MaxInfrastructureAttempts:  public.MaxInfrastructureAttempts,
		SimulatorCommand:           filepath.Clean(public.SimulatorCommand),
		EvaluatorCommand:           filepath.Clean(public.EvaluatorCommand),
		EvaluatorCommandSha256:     strings.TrimSpace(public.EvaluatorCommandSha256),
		SelfCheckCommand:           filepath.Clean(public.SelfCheckCommand),
		SelfCheckCommandSha256:     strings.TrimSpace(public.SelfCheckCommandSha256),
		Tokens:                     secret.Tokens,
		SeedKey:                    seedKey,
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func safeUnmarshal(resource *server.SimpleResource, value any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%v", recovered)
		}
	}()
	resource.UnmarshalYaml(value)
	return nil
}

func (s *Settings) Validate() error {
	if s == nil {
		return errors.New("competition settings are nil")
	}
	if !s.Enabled {
		return errors.New("competition is disabled")
	}
	if s.CompetitionId == "" || len(s.CompetitionId) > 128 {
		return errors.New("competition_id must contain 1..128 characters")
	}
	if !gitShaPattern.MatchString(s.BaseSha) {
		return errors.New("base_sha must be a lowercase 40-character Git SHA")
	}
	if !imageDigestPattern.MatchString(s.EvaluatorImageDigest) {
		return errors.New("evaluator_image_digest must be a pinned sha256 digest")
	}
	if s.PatchPolicy.MaxPatchBytes <= 0 || 262144 < s.PatchPolicy.MaxPatchBytes {
		return errors.New("patch_policy.max_patch_bytes must be in 1..262144")
	}
	if len(s.PatchPolicy.AllowedPaths) == 0 || len(s.PatchPolicy.ForbiddenPaths) == 0 {
		return errors.New("patch allowlist and hard-forbidden list must both be nonempty")
	}
	if err := validatePatterns(s.PatchPolicy.AllowedPaths, "allowed_paths"); err != nil {
		return err
	}
	if err := validateLiteralAllowedPaths(s.PatchPolicy.AllowedPaths); err != nil {
		return err
	}
	if err := validatePatterns(s.PatchPolicy.ForbiddenPaths, "forbidden_paths"); err != nil {
		return err
	}
	p := s.EvaluationPolicy
	if p.HardwareId == "" ||
		!sha256Pattern.MatchString(p.HostQualificationSha256) ||
		!sha256Pattern.MatchString(p.ConfigLocalSha256) ||
		!sha256Pattern.MatchString(p.VaultLocalSha256) ||
		!sha256Pattern.MatchString(p.SimulatorSha256) ||
		!sha256Pattern.MatchString(p.ScorerSha256) ||
		p.DurationMs <= 0 || p.RequestTimeoutMs <= 0 {
		return errors.New("evaluation policy is not frozen")
	}
	if p.ProviderCount <= 0 || p.ClientPoolSize <= 0 || p.ArrivalsPerMinute <= 0 ||
		p.QualityWindowSize < 0 || 32 < p.QualityWindowSize ||
		p.ExchangeHosts <= 0 || 32 < p.ExchangeHosts || p.FleetShards < 0 || 32 < p.FleetShards {
		return errors.New("evaluation scale is outside its supported bounds")
	}
	host, port, err := net.SplitHostPort(p.SiteListen)
	if err != nil || port == "" || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || p.ApiPort <= 0 || 65535 < p.ApiPort {
		return errors.New("site_listen and api_port must be valid loopback endpoints")
	}
	if p.RampMs < 0 || p.PrewarmMs < 0 || p.SettleMs < 0 ||
		p.ClientWarmupTimeoutMs <= 0 || 24*time.Hour.Milliseconds() < p.ClientWarmupTimeoutMs ||
		p.PipelineIntervalMs <= 0 || p.TestTimeoutMs <= 0 || p.AnnounceTimeoutMs <= 0 {
		return errors.New("evaluation timing policy is invalid")
	}
	if 24*time.Hour.Milliseconds() < p.DurationMs || p.DurationMs < p.RequestTimeoutMs {
		return errors.New("duration must be at most 24h and at least the request timeout")
	}
	if p.Replicates <= 0 || 9 < p.Replicates || p.Replicates%2 == 0 {
		return errors.New("evaluation_policy.replicates must be an odd number in 1..9")
	}
	if math.IsNaN(p.TakeoverMargin) || math.IsInf(p.TakeoverMargin, 0) || p.TakeoverMargin <= 0 || .5 < p.TakeoverMargin {
		return errors.New("evaluation_policy.takeover_margin must be finite and in (0, 0.5]")
	}
	minimumScoreSeconds := minimumScoreTimeoutSeconds(p)
	if p.QueueLimit <= 0 || 10000 < p.QueueLimit || int64(p.ScoreTimeoutSeconds) < minimumScoreSeconds || 7*24*60*60 < p.ScoreTimeoutSeconds {
		return errors.New("queue_limit must be in 1..10000 and score_timeout_seconds must cover paired baseline/candidate replicates without exceeding seven days")
	}
	if len(s.SeedKey) != 32 {
		return errors.New("seed_key_base64 must decode to exactly 32 bytes")
	}
	if !filepath.IsAbs(s.ArtifactRoot) || s.ArtifactRoot == string(filepath.Separator) || s.ArtifactRoot == "." {
		return errors.New("artifact_root must be an absolute non-root path")
	}
	if err := validateLocalMountDirectory(s.ConfigLocalDirectory, "config"); err != nil {
		return err
	}
	if err := validateLocalMountDirectory(s.VaultLocalDirectory, "vault"); err != nil {
		return err
	}
	if s.SeasonEndsAt.IsZero() || s.RetainUntil.Before(s.SeasonEndsAt) {
		return errors.New("retain_until must be at or after season_ends_at")
	}
	if s.WorkerLeaseSeconds < 30 || s.WorkerHeartbeatSeconds <= 0 || s.WorkerLeaseSeconds <= 2*s.WorkerHeartbeatSeconds {
		return errors.New("worker lease must be at least 30s and more than twice its heartbeat")
	}
	if s.HostHeartbeatMaxAgeSeconds < 2*s.WorkerHeartbeatSeconds {
		return errors.New("host heartbeat max age must cover at least two worker heartbeats")
	}
	if s.MaxInfrastructureAttempts <= 0 || 10 < s.MaxInfrastructureAttempts {
		return errors.New("max_infrastructure_attempts must be in 1..10")
	}
	if err := validatePinnedCommand(s.SimulatorCommand, p.SimulatorSha256, "simulator"); err != nil {
		return err
	}
	if err := validatePinnedCommand(s.EvaluatorCommand, s.EvaluatorCommandSha256, "evaluator"); err != nil {
		return err
	}
	if err := validatePinnedCommand(s.SelfCheckCommand, s.SelfCheckCommandSha256, "self-check"); err != nil {
		return err
	}
	roles := map[string]bool{}
	names := map[string]bool{}
	for _, token := range s.Tokens {
		if !tokenNamePattern.MatchString(token.Name) || names[token.Name] {
			return errors.New("competition token names must be nonempty and unique")
		}
		if token.Role != "submitter" && token.Role != "operator" {
			return fmt.Errorf("competition token %q has invalid role", token.Name)
		}
		if !sha256Pattern.MatchString(token.Sha256) {
			return fmt.Errorf("competition token %q has an invalid SHA-256", token.Name)
		}
		names[token.Name] = true
		roles[token.Role] = true
	}
	if !roles["submitter"] || !roles["operator"] {
		return errors.New("at least one submitter token and one operator token are required")
	}
	return nil
}

// Includes the frozen per-stage startup/cleanup allowance used by the trusted
// evaluator's outer TERM/KILL boundary.
func evaluationStageTimeoutSeconds(p EvaluationPolicy) int64 {
	phaseMs := p.RampMs + p.SettleMs + p.ClientWarmupTimeoutMs + p.DurationMs + p.RequestTimeoutMs
	return (phaseMs+999)/1000 + evaluationStageOverheadSeconds
}

// Covers every paired replicate plus candidate build, scoring, and final
// artifact cleanup outside the individual simulator stages.
func minimumScoreTimeoutSeconds(p EvaluationPolicy) int64 {
	return 2*int64(p.Replicates)*evaluationStageTimeoutSeconds(p) + evaluationJobOverheadSeconds
}

func validateLocalMountDirectory(path, parent string) error {
	if !filepath.IsAbs(path) || filepath.Base(path) != "local" || filepath.Base(filepath.Dir(path)) != parent {
		return fmt.Errorf("%s_local_directory must be an absolute %s/local path", parent, parent)
	}
	if strings.ContainsAny(path, "\r\n\x00") {
		return fmt.Errorf("%s_local_directory contains unsafe characters", parent)
	}
	return nil
}

func validatePatterns(patterns []string, label string) error {
	seen := map[string]bool{}
	for _, pattern := range patterns {
		if pattern == "" || strings.HasPrefix(pattern, "/") || strings.Contains(pattern, "\\") || strings.Contains(pattern, "..") {
			return fmt.Errorf("%s contains unsafe pattern %q", label, pattern)
		}
		if _, err := filepath.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("%s contains invalid pattern %q: %w", label, pattern, err)
		}
		if seen[pattern] {
			return fmt.Errorf("%s contains duplicate pattern %q", label, pattern)
		}
		seen[pattern] = true
	}
	return nil
}

func validateLiteralAllowedPaths(paths []string) error {
	for _, value := range paths {
		if strings.ContainsAny(value, `*?[\\`) {
			return fmt.Errorf("allowed_paths must contain literal repository files, not pattern %q", value)
		}
		if err := validateRepositoryPath(value); err != nil {
			return fmt.Errorf("allowed_paths contains unsafe file %q: %w", value, err)
		}
		if filepath.Ext(value) != ".go" {
			return fmt.Errorf("allowed_paths contains non-Go file %q", value)
		}
		if matchAny(hardForbiddenPatchPaths, value) {
			return fmt.Errorf("allowed_paths contains protected file %q", value)
		}
	}
	return nil
}

func validatePinnedCommand(path, digest, label string) error {
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return fmt.Errorf("%s command must be an absolute non-root path", label)
	}
	if !sha256Pattern.MatchString(digest) {
		return fmt.Errorf("%s command SHA-256 is not pinned", label)
	}
	return nil
}

func (s *Settings) PublicInfo() InfoResult {
	return InfoResult{
		CompetitionId:        s.CompetitionId,
		Enabled:              s.Enabled,
		ScoreSchema:          ScoreSchema,
		ScorerVersion:        ScorerVersion,
		BaseSha:              s.BaseSha,
		EvaluatorImageDigest: s.EvaluatorImageDigest,
		PatchPolicy:          s.PatchPolicy,
		EvaluationPolicy:     s.EvaluationPolicy,
	}
}

func (s *Settings) TokenNames() []string {
	names := make([]string, 0, len(s.Tokens))
	for _, token := range s.Tokens {
		names = append(names, token.Name)
	}
	slices.Sort(names)
	return names
}
