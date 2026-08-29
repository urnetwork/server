// Package competition implements the durable control plane and worker protocol
// for the sim-latency competition API.
package competition

import (
	"encoding/json"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	ScoreSchema   = 1
	ScorerVersion = "sim-latency-score/1"
)

type HealthResult struct {
	Status  string    `json:"status"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

type ReadinessResult struct {
	Ready     bool            `json:"ready"`
	Checks    map[string]bool `json:"checks"`
	CheckedAt time.Time       `json:"checked_at"`
}

type PatchPolicy struct {
	MaxPatchBytes  int      `json:"max_patch_bytes" yaml:"max_patch_bytes"`
	AllowedPaths   []string `json:"allowed_paths" yaml:"allowed_paths"`
	ForbiddenPaths []string `json:"forbidden_paths" yaml:"forbidden_paths"`
}

type EvaluationPolicy struct {
	HardwareId              string  `json:"hardware_id" yaml:"hardware_id"`
	HostQualificationSha256 string  `json:"host_qualification_sha256" yaml:"host_qualification_sha256"`
	ConfigLocalSha256       string  `json:"config_local_sha256" yaml:"config_local_sha256"`
	VaultLocalSha256        string  `json:"vault_local_sha256" yaml:"vault_local_sha256"`
	SimulatorSha256         string  `json:"simulator_sha256" yaml:"simulator_sha256"`
	ScorerSha256            string  `json:"scorer_sha256" yaml:"scorer_sha256"`
	ProviderCount           int     `json:"provider_count" yaml:"provider_count"`
	ClientPoolSize          int     `json:"client_pool_size" yaml:"client_pool_size"`
	ArrivalsPerMinute       int     `json:"arrivals_per_minute" yaml:"arrivals_per_minute"`
	QualityWindowSize       int     `json:"quality_window_size" yaml:"quality_window_size"`
	ExchangeHosts           int     `json:"exchange_hosts" yaml:"exchange_hosts"`
	FleetShards             int     `json:"fleet_shards" yaml:"fleet_shards"`
	SiteListen              string  `json:"site_listen" yaml:"site_listen"`
	ApiPort                 int     `json:"api_port" yaml:"api_port"`
	RampMs                  int64   `json:"ramp_ms" yaml:"ramp_ms"`
	PrewarmMs               int64   `json:"prewarm_ms" yaml:"prewarm_ms"`
	SettleMs                int64   `json:"settle_ms" yaml:"settle_ms"`
	ClientWarmupTimeoutMs   int64   `json:"client_warmup_timeout_ms" yaml:"client_warmup_timeout_ms"`
	DurationMs              int64   `json:"duration_ms" yaml:"duration_ms"`
	RequestTimeoutMs        int64   `json:"request_timeout_ms" yaml:"request_timeout_ms"`
	PipelineIntervalMs      int64   `json:"pipeline_interval_ms" yaml:"pipeline_interval_ms"`
	TestTimeoutMs           int64   `json:"test_timeout_ms" yaml:"test_timeout_ms"`
	AnnounceTimeoutMs       int64   `json:"announce_timeout_ms" yaml:"announce_timeout_ms"`
	ImpairmentEnabled       bool    `json:"impairment_enabled" yaml:"impairment_enabled"`
	Replicates              int     `json:"replicates" yaml:"replicates"`
	TakeoverMargin          float64 `json:"takeover_margin" yaml:"takeover_margin"`
	QueueLimit              int     `json:"queue_limit" yaml:"queue_limit"`
	ScoreTimeoutSeconds     int     `json:"score_timeout_seconds" yaml:"score_timeout_seconds"`
}

type SeasonPolicy struct {
	EpochCount               int `json:"epoch_count" yaml:"epoch_count"`
	SubmissionWindowSeconds  int `json:"submission_window_seconds" yaml:"submission_window_seconds"`
	PreparationWindowSeconds int `json:"preparation_window_seconds" yaml:"preparation_window_seconds"`
	SubmissionFeeUsd         int `json:"submission_fee_usd" yaml:"submission_fee_usd"`
}

type InfoResult struct {
	CompetitionId        string           `json:"competition_id"`
	Enabled              bool             `json:"enabled"`
	ScoreSchema          int              `json:"score_schema"`
	ScorerVersion        string           `json:"scorer_version"`
	BaseSha              string           `json:"base_sha"`
	EvaluatorImageDigest string           `json:"evaluator_image_digest"`
	PatchPolicy          PatchPolicy      `json:"patch_policy"`
	EvaluationPolicy     EvaluationPolicy `json:"evaluation_policy"`
	SeasonPolicy         SeasonPolicy     `json:"season_policy"`
	ActiveRound          *RoundResult     `json:"active_round,omitempty"`
}

type GenerateRoundArgs struct {
	OpensAt  time.Time `json:"opens_at"`
	ClosesAt time.Time `json:"closes_at"`
	RevealAt time.Time `json:"reveal_at"`
}

type RoundResult struct {
	RoundId            server.Id  `json:"round_id"`
	Epoch              int        `json:"epoch"`
	Status             string     `json:"status"`
	WorkloadCommitment string     `json:"workload_commitment"`
	ProvidersSha256    string     `json:"providers_sha256"`
	ScoreSchema        int        `json:"score_schema"`
	OpensAt            time.Time  `json:"opens_at"`
	ClosesAt           time.Time  `json:"closes_at"`
	RevealAt           time.Time  `json:"reveal_at"`
	CreatedAt          time.Time  `json:"created_at"`
	FinalizedAt        *time.Time `json:"finalized_at,omitempty"`
	WinnerJobId        *server.Id `json:"winner_job_id,omitempty"`
	RevealedSeed       *string    `json:"revealed_seed,omitempty"`
	ProvidersUrl       string     `json:"providers_url,omitempty"`
}

type SeasonLeaderboardResult struct {
	CompetitionId string              `json:"competition_id"`
	Epochs        []LeaderboardResult `json:"epochs"`
}

type LeaderboardResult struct {
	CompetitionId string             `json:"competition_id"`
	RoundId       server.Id          `json:"round_id"`
	Epoch         int                `json:"epoch"`
	Status        string             `json:"status"`
	FinalizedAt   time.Time          `json:"finalized_at"`
	WinnerJobId   *server.Id         `json:"winner_job_id,omitempty"`
	Entries       []LeaderboardEntry `json:"entries"`
}

type LeaderboardEntry struct {
	Rank           int         `json:"rank"`
	JobId          server.Id   `json:"job_id"`
	PatchSha256    string      `json:"patch_sha256"`
	SubmittedAt    time.Time   `json:"submitted_at"`
	Winner         bool        `json:"winner"`
	HonestyReview  string      `json:"honesty_review"`
	Score          ScoreResult `json:"score"`
	SubmitterCount int         `json:"submitter_count"`
}

// CandidateReviewState is the trusted operator view of a closed epoch. Score
// results remain embargoed while Status is pending_review. A review decision
// is append-only; rejecting the current candidate advances to the next ranked
// statistically significant candidate, while approving it finalizes the epoch.
type CandidateReviewState struct {
	CompetitionId string                    `json:"competition_id"`
	RoundId       server.Id                 `json:"round_id"`
	Epoch         int                       `json:"epoch"`
	Status        string                    `json:"status"`
	RejectedCount int                       `json:"rejected_count"`
	Candidate     *CandidateReviewCandidate `json:"candidate,omitempty"`
	FinalizedAt   *time.Time                `json:"finalized_at,omitempty"`
	WinnerJobId   *server.Id                `json:"winner_job_id,omitempty"`
}

// CandidateReviewCandidate contains the exact ranked score and authenticated
// canonical patch needed by the operator-controlled honesty-review harness.
// Patch is intentionally excluded from JSON and is materialized into a private
// temporary directory by the CLI.
type CandidateReviewCandidate struct {
	Rank        int         `json:"rank"`
	JobId       server.Id   `json:"job_id"`
	PatchSha256 string      `json:"patch_sha256"`
	SubmittedAt time.Time   `json:"submitted_at"`
	Score       ScoreResult `json:"score"`
	Patch       []byte      `json:"-"`
}

type CandidateReviewDecision struct {
	JobId          server.Id
	Decision       string
	ReviewerId     string
	Reason         string
	Evidence       json.RawMessage
	EvidenceSha256 string
}

type ScoreArgs struct {
	RoundId server.Id `json:"round_id"`
	Patch   string    `json:"patch"`
}

type ScoreAcceptedResult struct {
	JobId       server.Id `json:"job_id"`
	RoundId     server.Id `json:"round_id"`
	PatchSha256 string    `json:"patch_sha256"`
	State       string    `json:"state"`
	CacheHit    bool      `json:"cache_hit"`
	StatusUrl   string    `json:"status_url"`
}

type ScoreJobResult struct {
	JobId                server.Id         `json:"job_id"`
	RoundId              server.Id         `json:"round_id"`
	PatchSha256          string            `json:"patch_sha256"`
	State                string            `json:"state"`
	SubmittedAt          time.Time         `json:"submitted_at"`
	StartedAt            *time.Time        `json:"started_at,omitempty"`
	CompletedAt          *time.Time        `json:"completed_at,omitempty"`
	CacheKey             string            `json:"cache_key"`
	EvaluatorImageDigest string            `json:"evaluator_image_digest"`
	ApiImageDigest       string            `json:"api_image_digest"`
	WorkerImageDigest    string            `json:"worker_image_digest,omitempty"`
	Score                *ScoreResult      `json:"score,omitempty"`
	EvalError            *CompetitionError `json:"eval_error,omitempty"`
}

type ScoreResult struct {
	ScoreSchema      int                `json:"score_schema"`
	RawScore         *float64           `json:"raw_score,omitempty"`
	NormalizedScore  *float64           `json:"normalized_score,omitempty"`
	Placeable        bool               `json:"placeable"`
	TakeoverEligible bool               `json:"takeover_eligible,omitempty"`
	Gates            map[string]Gate    `json:"gates"`
	Significance     *ScoreSignificance `json:"significance"`
	Diagnostics      map[string]any     `json:"diagnostics,omitempty"`
}

// ScoreSignificance is the immutable statistical record for one evaluation.
// Percent fields use 100 for one hundred percent; variance is sample variance.
type ScoreSignificance struct {
	Method                                      string   `json:"method"`
	Alpha                                       float64  `json:"alpha"`
	ReplicateCount                              int      `json:"replicate_count"`
	BaselineMeanRawScore                        float64  `json:"baseline_mean_raw_score"`
	CandidateMeanRawScore                       float64  `json:"candidate_mean_raw_score"`
	BaselineSampleVariance                      *float64 `json:"baseline_sample_variance"`
	CandidateSampleVariance                     *float64 `json:"candidate_sample_variance"`
	ObservedImprovementPercent                  float64  `json:"observed_improvement_percent"`
	TakeoverMarginPercent                       float64  `json:"takeover_margin_percent"`
	MinimumSignificantImprovementPercent        *float64 `json:"minimum_significant_improvement_percent"`
	RequiredImprovementPercent                  *float64 `json:"required_improvement_percent"`
	OneSidedPValue                              *float64 `json:"one_sided_p_value"`
	WelchT                                      *float64 `json:"welch_t,omitempty"`
	WelchDegreesOfFreedom                       *float64 `json:"welch_degrees_of_freedom,omitempty"`
	StatisticallySignificant                    bool     `json:"statistically_significant"`
	NextEpochMinimumImprovementPercent          *float64 `json:"next_epoch_minimum_improvement_percent"`
	RecommendedNextEpochTakeoverMarginPercent   *float64 `json:"recommended_next_epoch_takeover_margin_percent"`
	RecommendedNextEpochTakeoverMarginSupported bool     `json:"recommended_next_epoch_takeover_margin_supported"`
}

type Gate struct {
	Passed  bool           `json:"passed"`
	Details map[string]any `json:"details"`
}

type CompetitionError struct {
	Kind      string `json:"kind"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retriable bool   `json:"retriable"`
}

func (e *CompetitionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

type roundRecord struct {
	RoundResult
	CompetitionId  string
	SeedNonce      []byte
	SeedCiphertext []byte
	ProvidersPath  string
	PolicyJson     json.RawMessage
	Canceled       bool
}

type queuedJob struct {
	ScoreJobResult
	Patch          []byte
	PrincipalId    string
	AttemptCount   int
	LeaseOwner     string
	LeaseExpiresAt *time.Time
	Round          roundRecord
}

type HostSelfCheck struct {
	Schema                      int             `json:"schema"`
	HostId                      string          `json:"host_id"`
	HardwareId                  string          `json:"hardware_id"`
	QualificationSha256         string          `json:"qualification_sha256"`
	ImageDigest                 string          `json:"image_digest"`
	KernelRelease               string          `json:"kernel_release"`
	MicrocodeRevision           string          `json:"microcode_revision"`
	LogicalCpuCount             int             `json:"logical_cpu_count"`
	SMTDisabled                 bool            `json:"smt_disabled"`
	GovernorPinned              bool            `json:"governor_pinned"`
	TurboPinned                 bool            `json:"turbo_pinned"`
	NumaPinned                  bool            `json:"numa_pinned"`
	IrqPinned                   bool            `json:"irq_pinned"`
	CgroupV2                    bool            `json:"cgroup_v2"`
	ServicesInJobCgroup         bool            `json:"services_in_job_cgroup"`
	DefaultDenyNetwork          bool            `json:"default_deny_network"`
	OfflineBuildCache           bool            `json:"offline_build_cache"`
	TemplateDatabase            bool            `json:"template_database"`
	RedisReset                  bool            `json:"redis_reset"`
	ArtifactStorage             bool            `json:"artifact_storage"`
	ImmutableReports            bool            `json:"immutable_reports"`
	NoProductionSecrets         bool            `json:"no_production_secrets"`
	CleanupVerified             bool            `json:"cleanup_verified"`
	ResourceLimitsVerified      bool            `json:"resource_limits_verified"`
	ManagementCpuReserved       bool            `json:"management_cpu_reserved"`
	ManagementMemoryReserved    bool            `json:"management_memory_reserved"`
	ResourceBombCleanupVerified bool            `json:"resource_bomb_cleanup_verified"`
	RebaselinePassed            bool            `json:"rebaseline_passed"`
	RebaselineRoundId           *server.Id      `json:"rebaseline_round_id,omitempty"`
	Checks                      map[string]bool `json:"checks"`
}

func (s HostSelfCheck) Eligible(settings *Settings) bool {
	return s.Schema == 1 &&
		s.HostId != "" &&
		s.HardwareId == settings.EvaluationPolicy.HardwareId &&
		s.QualificationSha256 == settings.EvaluationPolicy.HostQualificationSha256 &&
		s.ImageDigest == settings.EvaluatorImageDigest &&
		s.KernelRelease != "" && s.MicrocodeRevision != "" &&
		s.LogicalCpuCount == 12 &&
		s.SMTDisabled && s.GovernorPinned && s.TurboPinned && s.NumaPinned && s.IrqPinned &&
		s.CgroupV2 && s.ServicesInJobCgroup && s.DefaultDenyNetwork &&
		s.OfflineBuildCache && s.TemplateDatabase && s.RedisReset &&
		s.ArtifactStorage && s.ImmutableReports && s.NoProductionSecrets &&
		s.CleanupVerified && s.ResourceLimitsVerified &&
		s.ManagementCpuReserved && s.ManagementMemoryReserved &&
		s.ResourceBombCleanupVerified &&
		(!s.RebaselinePassed || s.RebaselineRoundId != nil) &&
		allChecks(s.Checks)
}
