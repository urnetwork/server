// Competition API and persistence models shared by the controller, API
// transport, evaluator worker, and epoch-promotion tooling.
package model

import (
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
)

const (
	CompetitionScoreSchema   = 1
	CompetitionScorerVersion = "sim-latency-score/1"
)

type CompetitionHealthResult struct {
	Status  string    `json:"status"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

type CompetitionReadinessResult struct {
	Ready     bool            `json:"ready"`
	Checks    map[string]bool `json:"checks"`
	CheckedAt time.Time       `json:"checked_at"`
}

type CompetitionPatchPolicy struct {
	MaxPatchBytes  int      `json:"max_patch_bytes" yaml:"max_patch_bytes"`
	AllowedPaths   []string `json:"allowed_paths" yaml:"allowed_paths"`
	ForbiddenPaths []string `json:"forbidden_paths" yaml:"forbidden_paths"`
}

type CompetitionEvaluationPolicy struct {
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

type CompetitionSeasonPolicy struct {
	EpochCount               int `json:"epoch_count" yaml:"epoch_count"`
	SubmissionWindowSeconds  int `json:"submission_window_seconds" yaml:"submission_window_seconds"`
	PreparationWindowSeconds int `json:"preparation_window_seconds" yaml:"preparation_window_seconds"`
	SubmissionFeeUsd         int `json:"submission_fee_usd" yaml:"submission_fee_usd"`
}

type CompetitionInfoResult struct {
	CompetitionId        string                      `json:"competition_id"`
	Enabled              bool                        `json:"enabled"`
	ScoreSchema          int                         `json:"score_schema"`
	ScorerVersion        string                      `json:"scorer_version"`
	BaseSha              string                      `json:"base_sha"`
	EvaluatorImageDigest string                      `json:"evaluator_image_digest"`
	PatchPolicy          CompetitionPatchPolicy      `json:"patch_policy"`
	EvaluationPolicy     CompetitionEvaluationPolicy `json:"evaluation_policy"`
	SeasonPolicy         CompetitionSeasonPolicy     `json:"season_policy"`
	ActiveRound          *CompetitionRoundResult     `json:"active_round,omitempty"`
	StagingRound         *CompetitionRoundResult     `json:"staging_round,omitempty"`
}

type CompetitionGenerateRoundArgs struct {
	OpensAt  time.Time `json:"opens_at"`
	ClosesAt time.Time `json:"closes_at"`
	RevealAt time.Time `json:"reveal_at"`
}

type CompetitionRoundResult struct {
	RoundId            server.Id  `json:"round_id"`
	Epoch              int        `json:"epoch"`
	Staging            bool       `json:"staging"`
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

type CompetitionSeasonLeaderboardResult struct {
	CompetitionId string                         `json:"competition_id"`
	Epochs        []CompetitionLeaderboardResult `json:"epochs"`
}

type CompetitionLeaderboardResult struct {
	CompetitionId string                        `json:"competition_id"`
	RoundId       server.Id                     `json:"round_id"`
	Epoch         int                           `json:"epoch"`
	Status        string                        `json:"status"`
	FinalizedAt   time.Time                     `json:"finalized_at"`
	WinnerJobId   *server.Id                    `json:"winner_job_id,omitempty"`
	Entries       []CompetitionLeaderboardEntry `json:"entries"`
}

type CompetitionLeaderboardEntry struct {
	Rank           int                    `json:"rank"`
	JobId          server.Id              `json:"job_id"`
	PatchSha256    string                 `json:"patch_sha256"`
	SubmittedAt    time.Time              `json:"submitted_at"`
	Winner         bool                   `json:"winner"`
	HonestyReview  string                 `json:"honesty_review"`
	Score          CompetitionScoreResult `json:"score"`
	SubmitterCount int                    `json:"submitter_count"`
}

// CompetitionCandidateReviewState is the trusted operator view of a closed epoch. Score
// results remain embargoed while Status is pending_review. A review decision
// is append-only; rejecting the current candidate advances to the next ranked
// statistically significant candidate, while approving it finalizes the epoch.
type CompetitionCandidateReviewState struct {
	CompetitionId string                               `json:"competition_id"`
	RoundId       server.Id                            `json:"round_id"`
	Epoch         int                                  `json:"epoch"`
	Status        string                               `json:"status"`
	RejectedCount int                                  `json:"rejected_count"`
	Candidate     *CompetitionCandidateReviewCandidate `json:"candidate,omitempty"`
	FinalizedAt   *time.Time                           `json:"finalized_at,omitempty"`
	WinnerJobId   *server.Id                           `json:"winner_job_id,omitempty"`
}

// CompetitionCandidateReviewCandidate contains the exact ranked score and authenticated
// canonical patch needed by the operator-controlled honesty-review harness.
// Patch is intentionally excluded from JSON and is materialized into a private
// temporary directory by the CLI.
type CompetitionCandidateReviewCandidate struct {
	Rank        int                    `json:"rank"`
	JobId       server.Id              `json:"job_id"`
	PatchSha256 string                 `json:"patch_sha256"`
	SubmittedAt time.Time              `json:"submitted_at"`
	Score       CompetitionScoreResult `json:"score"`
	Patch       []byte                 `json:"-"`
}

type CompetitionCandidateReviewDecision struct {
	JobId          server.Id
	Decision       string
	ReviewerId     string
	Reason         string
	Evidence       json.RawMessage
	EvidenceSha256 string
}

type CompetitionScoreArgs struct {
	RoundId server.Id `json:"round_id"`
	Patch   string    `json:"patch"`
}

type CompetitionScoreAcceptedResult struct {
	JobId       server.Id `json:"job_id"`
	RoundId     server.Id `json:"round_id"`
	Staging     bool      `json:"staging"`
	PatchSha256 string    `json:"patch_sha256"`
	State       string    `json:"state"`
	CacheHit    bool      `json:"cache_hit"`
	StatusUrl   string    `json:"status_url"`
}

type CompetitionScoreJobResult struct {
	JobId                server.Id               `json:"job_id"`
	RoundId              server.Id               `json:"round_id"`
	Staging              bool                    `json:"staging"`
	PatchSha256          string                  `json:"patch_sha256"`
	State                string                  `json:"state"`
	SubmittedAt          time.Time               `json:"submitted_at"`
	StartedAt            *time.Time              `json:"started_at,omitempty"`
	CompletedAt          *time.Time              `json:"completed_at,omitempty"`
	CacheKey             string                  `json:"cache_key"`
	EvaluatorImageDigest string                  `json:"evaluator_image_digest"`
	ApiImageDigest       string                  `json:"api_image_digest"`
	WorkerImageDigest    string                  `json:"worker_image_digest,omitempty"`
	Score                *CompetitionScoreResult `json:"score,omitempty"`
	EvalError            *CompetitionError       `json:"eval_error,omitempty"`
}

type CompetitionScoreResult struct {
	ScoreSchema      int                           `json:"score_schema"`
	RawScore         *float64                      `json:"raw_score,omitempty"`
	NormalizedScore  *float64                      `json:"normalized_score,omitempty"`
	Placeable        bool                          `json:"placeable"`
	TakeoverEligible bool                          `json:"takeover_eligible,omitempty"`
	Gates            map[string]CompetitionGate    `json:"gates"`
	Significance     *CompetitionScoreSignificance `json:"significance"`
	Diagnostics      map[string]any                `json:"diagnostics,omitempty"`
}

// CompetitionScoreSignificance is the immutable statistical record for one evaluation.
// Percent fields use 100 for one hundred percent; variance is sample variance.
type CompetitionScoreSignificance struct {
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

type CompetitionGate struct {
	Passed  bool           `json:"passed"`
	Details map[string]any `json:"details"`
}

type CompetitionError struct {
	Kind      string `json:"kind"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retriable bool   `json:"retriable"`
}

func (self *CompetitionError) Error() string {
	if self == nil {
		return ""
	}
	return self.Code + ": " + self.Message
}
