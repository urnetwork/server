// Versioned reward decisions deliberately do not confer source or payment authority.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

const (
	CompetitionDecisionModeV2       = "provisional_reward_v2"
	CompetitionAutomationCapability = "competition.reward.v2"
	CompetitionElectionRule         = "raw_latency_g1_g6_welch_v1"
)

var competitionAutomationSha256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var competitionAutomationScope = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Zero values preserve legacy behavior; enabling requires prospectively approved policies.
type CompetitionAutomationPolicy struct {
	Enabled                    bool   `json:"enabled" yaml:"enabled"`
	Staging                    bool   `json:"staging,omitempty" yaml:"staging,omitempty"`
	StagingRoundWindowSeconds  int    `json:"staging_round_window_seconds,omitempty" yaml:"staging_round_window_seconds,omitempty"`
	StagingPreparationSeconds  int    `json:"staging_preparation_seconds,omitempty" yaml:"staging_preparation_seconds,omitempty"`
	Version                    int    `json:"version" yaml:"version"`
	Environment                string `json:"environment" yaml:"environment"`
	SeasonId                   string `json:"season_id" yaml:"season_id"`
	ElectionRule               string `json:"election_rule" yaml:"election_rule"`
	ReviewCriteriaVersion      string `json:"review_criteria_version" yaml:"review_criteria_version"`
	SettlementPolicyVersion    string `json:"settlement_policy_version" yaml:"settlement_policy_version"`
	BurnPolicyVersion          string `json:"burn_policy_version" yaml:"burn_policy_version"`
	RepeatRewardPolicy         string `json:"repeat_reward_policy" yaml:"repeat_reward_policy"`
	SettlementMode             string `json:"settlement_mode" yaml:"settlement_mode"`
	SourceChoiceDelaySeconds   int    `json:"source_choice_delay_seconds" yaml:"source_choice_delay_seconds"`
	RequiredApiCapability      string `json:"required_api_capability" yaml:"required_api_capability"`
	PolicyApprovalSha256       string `json:"policy_approval_sha256" yaml:"policy_approval_sha256"`
	SourceGeneration           int    `json:"source_generation" yaml:"source_generation"`
	SourceLockSha256           string `json:"source_lock_sha256" yaml:"source_lock_sha256"`
	SourceActivationGeneration int64  `json:"source_activation_generation" yaml:"source_activation_generation"`
}

// Validates authority references, not economic rules that must be agreed externally.
func ValidateCompetitionAutomationPolicy(policy CompetitionAutomationPolicy) error {
	if !policy.Enabled {
		return nil
	}
	if policy.Version != 2 || policy.ElectionRule != CompetitionElectionRule || policy.RequiredApiCapability != CompetitionAutomationCapability {
		return errors.New("unsupported automation contract")
	}
	if !competitionAutomationScope.MatchString(policy.Environment) || !competitionAutomationScope.MatchString(policy.SeasonId) {
		return errors.New("automation scope cannot be represented by scoped credentials")
	}
	for _, value := range []string{policy.Environment, policy.SeasonId, policy.ReviewCriteriaVersion, policy.SettlementPolicyVersion, policy.BurnPolicyVersion} {
		if strings.TrimSpace(value) != value || value == "" || len(value) > 128 {
			return errors.New("automation policy identity is required and bounded")
		}
	}
	if policy.RepeatRewardPolicy != "allow" && policy.RepeatRewardPolicy != "exclude_exact_identity" {
		return errors.New("repeat reward policy must be explicitly approved")
	}
	if policy.SettlementMode != "shadow" && policy.SettlementMode != "live" {
		return errors.New("settlement mode must be explicit")
	}
	if policy.Staging {
		if policy.SettlementMode != "shadow" || policy.StagingRoundWindowSeconds < 60 || policy.StagingRoundWindowSeconds > 604800 || policy.StagingPreparationSeconds < 0 || policy.StagingPreparationSeconds > 57600 {
			return errors.New("staging automation requires shadow settlement and bounded explicit timing")
		}
	} else if policy.StagingRoundWindowSeconds != 0 || policy.StagingPreparationSeconds != 0 {
		return errors.New("production automation cannot use staging timing")
	}
	if policy.SourceChoiceDelaySeconds < 0 || policy.SourceChoiceDelaySeconds > 86400 || policy.SourceGeneration < 0 || policy.SourceGeneration > 6 || policy.SourceActivationGeneration < 1 ||
		!competitionAutomationSha256.MatchString(policy.PolicyApprovalSha256) || !competitionAutomationSha256.MatchString(policy.SourceLockSha256) {
		return errors.New("automation source or policy approval identity is invalid")
	}
	return nil
}

// Stable json encoding supplies content identities; a digest alone is not a signature or approval.
func CompetitionAutomationDigest(value any) (string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}

// Includes every terminal job, not only successful or eligible entries.
type CompetitionSealedEntry struct {
	JobId                  server.Id               `json:"job_id"`
	PatchSha256            string                  `json:"patch_sha256"`
	BaseSourceLockSha256   string                  `json:"base_source_lock_sha256"`
	State                  string                  `json:"state"`
	SubmittedAt            time.Time               `json:"submitted_at"`
	StartedAt              *time.Time              `json:"started_at,omitempty"`
	CompletedAt            time.Time               `json:"completed_at"`
	Score                  *CompetitionScoreResult `json:"score,omitempty"`
	ScoreSha256            string                  `json:"score_sha256,omitempty"`
	ArtifactManifestSha256 string                  `json:"artifact_manifest_sha256,omitempty"`
	Rank                   int                     `json:"rank"`
	EligibleCandidateRank  int                     `json:"eligible_candidate_rank,omitempty"`
	Eligible               bool                    `json:"eligible"`
	IneligibilityReason    string                  `json:"ineligibility_reason,omitempty"`
}

// One immutable inventory, ranking, control and frozen-policy identity per round.
type CompetitionEvaluationSeal struct {
	Staging          bool                     `json:"staging,omitempty"`
	RoundId          server.Id                `json:"round_id"`
	Environment      string                   `json:"environment"`
	CompetitionId    string                   `json:"competition_id"`
	SeasonId         string                   `json:"season_id"`
	Epoch            int                      `json:"epoch"`
	SealedAt         time.Time                `json:"sealed_at"`
	PublicationAt    time.Time                `json:"publication_at"`
	SourceChoiceAt   time.Time                `json:"source_choice_at"`
	PolicySha256     string                   `json:"policy_sha256"`
	BaselineSha256   string                   `json:"baseline_sha256,omitempty"`
	BaselineRawScore float64                  `json:"baseline_raw_score"`
	TakeoverMargin   float64                  `json:"takeover_margin"`
	InventorySha256  string                   `json:"inventory_sha256"`
	RankingSha256    string                   `json:"ranking_sha256"`
	SealSha256       string                   `json:"seal_sha256"`
	TerminalCounts   map[string]int           `json:"terminal_counts"`
	Entries          []CompetitionSealedEntry `json:"entries"`
}

// The same exact predicate is used by sealing, review replacement, and adapter validation.
func CompetitionRewardEligibility(score *CompetitionScoreResult, baselineRaw, margin float64) string {
	return CompetitionRewardEligibilityForMode(score, baselineRaw, margin, false)
}

// Staging elects the best gate-passing measured entry without conferring source authority.
// Replicate and zero-margin identity remain mandatory even when significance is disabled.
func CompetitionRewardEligibilityForMode(score *CompetitionScoreResult, baselineRaw, margin float64, staging bool) string {
	if ValidateCompetitionScore(score) != nil {
		return "invalid_score"
	}
	if !score.Placeable {
		return "not_placeable"
	}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources"} {
		gate, ok := score.Gates[name]
		if !ok || !gate.Passed {
			return "gate_failed_or_missing"
		}
	}
	for _, gate := range score.Gates {
		if !gate.Passed {
			return "gate_failed_or_missing"
		}
	}
	if baselineRaw <= 0 || math.IsNaN(baselineRaw) || math.IsInf(baselineRaw, 0) || margin < 0 || margin >= 1 || math.IsNaN(margin) {
		return "invalid_control"
	}
	if staging {
		if margin != 0 || score.Significance.TakeoverMarginPercent != 0 || score.Significance.ReplicateCount != 9 {
			return "statistical_identity_mismatch"
		}
		return ""
	}
	if *score.RawScore > baselineRaw*(1-margin) {
		return "below_takeover_margin"
	}
	significance := score.Significance
	// Raw rank and takeover use replicate medians; Welch independently uses means.
	if math.Abs(significance.TakeoverMarginPercent-margin*100) > 1e-9 || significance.ReplicateCount != 9 {
		return "statistical_identity_mismatch"
	}
	if !score.TakeoverEligible || !significance.StatisticallySignificant {
		return "not_significant"
	}
	if !significance.RecommendedNextEpochTakeoverMarginSupported {
		return "unsupported_next_margin"
	}
	return ""
}

// Orders exact raw scores then admission time then id; no display rounding or epsilon ties.
func RankCompetitionSealedEntries(entries []CompetitionSealedEntry, baselineRaw, margin float64, excludedPatchKVs map[string]bool) ([]CompetitionSealedEntry, error) {
	return RankCompetitionSealedEntriesForMode(entries, baselineRaw, margin, excludedPatchKVs, false)
}

// Mode is explicit frozen authority, never inferred from a zero margin or environment label.
func RankCompetitionSealedEntriesForMode(entries []CompetitionSealedEntry, baselineRaw, margin float64, excludedPatchKVs map[string]bool, staging bool) ([]CompetitionSealedEntry, error) {
	ranked := append([]CompetitionSealedEntry{}, entries...)
	seenJobIds := map[server.Id]bool{}
	for i := range ranked {
		entry := &ranked[i]
		if seenJobIds[entry.JobId] || entry.JobId == (server.Id{}) || entry.CompletedAt.IsZero() || entry.SubmittedAt.IsZero() {
			return nil, errors.New("invalid terminal inventory")
		}
		seenJobIds[entry.JobId] = true
		entry.Rank, entry.EligibleCandidateRank, entry.Eligible = 0, 0, false
		switch entry.State {
		case "succeeded":
			if err := ValidateCompetitionScore(entry.Score); err != nil {
				return nil, fmt.Errorf("sealed score: %w", err)
			}
			entry.IneligibilityReason = CompetitionRewardEligibilityForMode(entry.Score, baselineRaw, margin, staging)
			if entry.IneligibilityReason == "" && excludedPatchKVs[entry.PatchSha256] {
				entry.IneligibilityReason = "reward_duplicate"
			}
			entry.Eligible = entry.IneligibilityReason == ""
		case "failed", "canceled":
			if entry.Score != nil {
				return nil, errors.New("failed terminal inventory cannot carry a score")
			}
			entry.IneligibilityReason = "terminal_failure"
		default:
			return nil, errors.New("cannot seal nonterminal work")
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if (a.Score != nil) != (b.Score != nil) {
			return a.Score != nil
		}
		if a.Score != nil && b.Score != nil && *a.Score.RawScore != *b.Score.RawScore {
			return *a.Score.RawScore < *b.Score.RawScore
		}
		if !a.SubmittedAt.Equal(b.SubmittedAt) {
			return a.SubmittedAt.Before(b.SubmittedAt)
		}
		return a.JobId.String() < b.JobId.String()
	})
	eligibleRank := 0
	for i := range ranked {
		ranked[i].Rank = i + 1
		if ranked[i].Eligible {
			eligibleRank++
			ranked[i].EligibleCandidateRank = eligibleRank
		}
	}
	return ranked, nil
}

// Every revision is immutable; the previous digest authenticates a contiguous history.
type CompetitionRewardDecision struct {
	Staging                   bool       `json:"staging,omitempty"`
	RoundId                   server.Id  `json:"round_id"`
	Environment               string     `json:"environment"`
	CompetitionId             string     `json:"competition_id"`
	SeasonId                  string     `json:"season_id"`
	Revision                  int        `json:"revision"`
	PreviousDecisionSha256    string     `json:"previous_decision_sha256,omitempty"`
	DecisionSha256            string     `json:"decision_sha256"`
	SealSha256                string     `json:"seal_sha256"`
	Action                    string     `json:"action"`
	SelectedJobId             *server.Id `json:"selected_job_id"`
	SupersededJobId           *server.Id `json:"superseded_job_id,omitempty"`
	SupersededRevision        int        `json:"superseded_revision,omitempty"`
	ReasonCode                string     `json:"reason_code"`
	ReviewId                  *server.Id `json:"review_id,omitempty"`
	DecidedAt                 time.Time  `json:"decided_at"`
	EffectiveSettlementAnchor *time.Time `json:"effective_settlement_anchor,omitempty"`
	SettlementPolicyVersion   string     `json:"settlement_policy_version"`
	BurnPolicyVersion         string     `json:"burn_policy_version"`
	SettlementMode            string     `json:"settlement_mode"`
}

// Selects from the original sealed evidence; appeal restoration is just removal from exclusions.
func SelectCompetitionReward(entries []CompetitionSealedEntry, disqualifiedJobIds map[server.Id]bool) (*CompetitionSealedEntry, string) {
	if len(entries) == 0 {
		return nil, "no_submissions"
	}
	hadEligible, hadGatePassing, hadDuplicate := false, false, false
	for i := range entries {
		entry := &entries[i]
		if entry.IneligibilityReason == "reward_duplicate" {
			hadDuplicate = true
		}
		if entry.Score != nil && entry.Score.Placeable && entry.IneligibilityReason != "gate_failed_or_missing" {
			hadGatePassing = true
		}
		if entry.Eligible {
			hadEligible = true
			if !disqualifiedJobIds[entry.JobId] {
				return entry, "first_eligible"
			}
		}
	}
	if hadEligible {
		return nil, "all_eligible_disqualified"
	}
	if hadDuplicate {
		return nil, "no_new_reward_entitlement"
	}
	if hadGatePassing {
		return nil, "no_significant_improvement"
	}
	return nil, "no_gate_passing_candidate"
}

// Findings carry bounded public explanations and private evidence identities, never secret content.
type CompetitionReviewFinding struct {
	Criterion      string `json:"criterion"`
	Disposition    string `json:"disposition"`
	ReasonCode     string `json:"reason_code"`
	Explanation    string `json:"explanation"`
	EvidenceSha256 string `json:"evidence_sha256"`
}

// Actor identity comes from authentication, never from this request.
type CompetitionIntegrityReviewRequest struct {
	IdempotencyKey              string                     `json:"idempotency_key"`
	ExpectedRevision            int                        `json:"expected_revision"`
	JobId                       server.Id                  `json:"job_id"`
	PatchSha256                 string                     `json:"patch_sha256"`
	BaseSourceLockSha256        string                     `json:"base_source_lock_sha256"`
	ScoreSha256                 string                     `json:"score_sha256"`
	BaselineSha256              string                     `json:"baseline_sha256"`
	ArtifactManifestSha256      string                     `json:"artifact_manifest_sha256"`
	CriteriaVersion             string                     `json:"criteria_version"`
	RewardDisposition           string                     `json:"reward_disposition"`
	SourceDisposition           string                     `json:"source_disposition"`
	ReasonCodes                 []string                   `json:"reason_codes"`
	PublicExplanation           string                     `json:"public_explanation"`
	WithheldEvidenceExplanation string                     `json:"withheld_evidence_explanation"`
	PrivateEvidenceSha256       string                     `json:"private_evidence_sha256"`
	Findings                    []CompetitionReviewFinding `json:"findings"`
	SupersedesReviewId          *server.Id                 `json:"supersedes_review_id,omitempty"`
	AppealId                    string                     `json:"appeal_id,omitempty"`
	ExpectedReplacementJobId    *server.Id                 `json:"expected_replacement_job_id,omitempty"`
}

// Review records remain separate even when approval does not change the reward recipient.
type CompetitionIntegrityReviewResult struct {
	ReviewId       server.Id                         `json:"review_id"`
	RoundId        server.Id                         `json:"round_id"`
	ActorId        string                            `json:"actor_id"`
	CreatedAt      time.Time                         `json:"created_at"`
	Request        CompetitionIntegrityReviewRequest `json:"request"`
	RequestSha256  string                            `json:"request_sha256"`
	RewardDecision CompetitionRewardDecision         `json:"reward_decision"`
}

// Requires complete structured criteria and distinguishes source-only concerns from fraud.
func ValidateCompetitionIntegrityReview(request CompetitionIntegrityReviewRequest, policy CompetitionAutomationPolicy) error {
	content, err := json.Marshal(request)
	if err != nil || len(content) > 64*1024 {
		return errors.New("review exceeds bounded contract")
	}
	if request.ExpectedRevision < 1 || request.JobId == (server.Id{}) || request.IdempotencyKey == "" || len(request.IdempotencyKey) > 128 || request.CriteriaVersion != policy.ReviewCriteriaVersion || strings.TrimSpace(request.PublicExplanation) == "" || len(request.PublicExplanation) > 4096 || strings.TrimSpace(request.WithheldEvidenceExplanation) == "" {
		return errors.New("review identity and public explanation are required")
	}
	for _, digest := range []string{request.PatchSha256, request.BaseSourceLockSha256, request.ScoreSha256, request.BaselineSha256, request.ArtifactManifestSha256, request.PrivateEvidenceSha256} {
		if !competitionAutomationSha256.MatchString(digest) {
			return errors.New("review evidence digest is invalid")
		}
	}
	if request.RewardDisposition != "approved" && request.RewardDisposition != "disqualified" && request.RewardDisposition != "restored" && request.RewardDisposition != "unchanged" {
		return errors.New("invalid reward review disposition")
	}
	if request.SourceDisposition != "approved" && request.SourceDisposition != "rejected" && request.SourceDisposition != "deferred" && request.SourceDisposition != "unchanged" {
		return errors.New("invalid source review disposition")
	}
	if request.RewardDisposition == "restored" && (request.SupersedesReviewId == nil || request.AppealId == "") {
		return errors.New("restoration requires an append-only appeal reference")
	}
	if request.SourceDisposition == "approved" && request.RewardDisposition != "approved" && request.RewardDisposition != "restored" {
		return errors.New("source approval requires an affirmative integrity review")
	}
	allowedReasonKVs := map[string]bool{}
	for _, code := range []string{"APPROVED", "SCORE_TAMPERING", "MEASUREMENT_FABRICATION", "WORKLOAD_SPECIAL_CASING", "BENCHMARK_DETECTION", "SANDBOX_ESCAPE", "SECRET_ACCESS", "EXFILTRATION", "UNSAFE_BEHAVIOR", "PERSISTENCE", "DENIAL_OF_SERVICE", "PATCH_POLICY_VIOLATION", "UNRELATED_CHANGE", "REWARD_DUPLICATE", "REWARD_RULE_EVASION", "SOURCE_NOT_READY", "PRODUCT_INTEGRATION_RISK", "EVALUATION_INTEGRITY_INCIDENT", "APPEAL_UPHELD"} {
		allowedReasonKVs[code] = true
	}
	if len(request.ReasonCodes) == 0 || len(request.ReasonCodes) > 16 {
		return errors.New("specific review reasons are required")
	}
	for _, code := range request.ReasonCodes {
		if !allowedReasonKVs[code] {
			return errors.New("unknown review reason")
		}
		if (code == "SOURCE_NOT_READY" || code == "PRODUCT_INTEGRATION_RISK" || code == "EVALUATION_INTEGRITY_INCIDENT" || code == "APPROVED" || code == "APPEAL_UPHELD") && request.RewardDisposition == "disqualified" {
			return errors.New("source or evaluator concern alone cannot disqualify a reward")
		}
		if (code == "REWARD_DUPLICATE" || code == "REWARD_RULE_EVASION") && policy.RepeatRewardPolicy != "exclude_exact_identity" {
			return errors.New("repeat exclusion was not prospectively adopted")
		}
	}
	criteriaKVs := map[string]bool{"measurement_integrity": false, "workload_generalization": false, "containment": false, "patch_scope": false, "reproducibility": false, "source_safety": false}
	for _, finding := range request.Findings {
		seen, ok := criteriaKVs[finding.Criterion]
		if !ok || seen || !competitionAutomationSha256.MatchString(finding.EvidenceSha256) || finding.Explanation == "" || len(finding.Explanation) > 4096 || !allowedReasonKVs[finding.ReasonCode] {
			return errors.New("invalid or duplicate structured finding")
		}
		if finding.Disposition != "pass" && finding.Disposition != "fail" && finding.Disposition != "not_applicable" {
			return errors.New("invalid finding disposition")
		}
		if request.SourceDisposition == "approved" && finding.Disposition == "fail" {
			return errors.New("failed finding cannot authorize source promotion")
		}
		criteriaKVs[finding.Criterion] = true
	}
	for _, seen := range criteriaKVs {
		if !seen {
			return errors.New("review is missing a required criterion")
		}
	}
	return nil
}

// The cursor orders durable insertion; the payload binds every scope and revision.
type CompetitionRewardEvent struct {
	Cursor        int64                     `json:"cursor"`
	EventId       server.Id                 `json:"event_id"`
	PayloadSha256 string                    `json:"payload_sha256"`
	PublicationAt time.Time                 `json:"publication_at"`
	Decision      CompetitionRewardDecision `json:"decision"`
}

// Unpublished head events stop the cursor rather than being skipped forever.
type CompetitionRewardEventPage struct {
	Events     []CompetitionRewardEvent `json:"events"`
	NextCursor int64                    `json:"next_cursor"`
}

// An acknowledgment is not proof of irrevocable transfer; accounting references are explicit.
type CompetitionSettlementReceipt struct {
	EventId             server.Id  `json:"event_id"`
	Environment         string     `json:"environment"`
	CompetitionId       string     `json:"competition_id"`
	SeasonId            string     `json:"season_id"`
	RoundId             server.Id  `json:"round_id"`
	Revision            int        `json:"revision"`
	PayloadSha256       string     `json:"payload_sha256"`
	DecisionSha256      string     `json:"decision_sha256"`
	Status              string     `json:"status"`
	AccountingReference string     `json:"accounting_reference"`
	ChainTransaction    string     `json:"chain_transaction,omitempty"`
	AppliedAt           *time.Time `json:"applied_at,omitempty"`
	ReceivedAt          time.Time  `json:"received_at"`
	ActorId             string     `json:"actor_id"`
}

// This result is public only after close, complete drain, and reveal.
type CompetitionV2RoundResult struct {
	Staging                 bool                               `json:"staging,omitempty"`
	Incidents               []CompetitionAutomationIncident    `json:"incidents"`
	Invalidated             bool                               `json:"invalidated"`
	DecisionMode            string                             `json:"decision_mode"`
	RequiredApiCapability   string                             `json:"required_api_capability"`
	Seal                    CompetitionEvaluationSeal          `json:"seal"`
	RewardRevision          int                                `json:"reward_revision"`
	ProvisionalRewardWinner *server.Id                         `json:"provisional_reward_winner"`
	IntegrityReview         string                             `json:"integrity_review"`
	SourceApproval          string                             `json:"source_approval"`
	Decisions               []CompetitionRewardDecision        `json:"decisions"`
	IntegrityReviews        []CompetitionIntegrityReviewResult `json:"integrity_reviews"`
	SourceDecision          *CompetitionSourceActivation       `json:"source_decision,omitempty"`
	SettlementStatus        string                             `json:"settlement_status"`
	SettlementReceipts      []CompetitionSettlementReceipt     `json:"settlement_receipts"`
}

// References expected and new immutable commits; retries never resolve a moving head.
type CompetitionSourceGitRef struct {
	Branch         string `json:"branch"`
	ExpectedCommit string `json:"expected_commit"`
	Commit         string `json:"commit"`
}

// An immutable intent stages source; only a separately recorded activation changes runtime authority.
type CompetitionSourceActivation struct {
	Staging                   bool                               `json:"staging,omitempty"`
	StagingRoundWindowSeconds int                                `json:"staging_round_window_seconds,omitempty"`
	StagingPreparationSeconds int                                `json:"staging_preparation_seconds,omitempty"`
	Environment               string                             `json:"environment"`
	CompetitionId             string                             `json:"competition_id"`
	SeasonId                  string                             `json:"season_id"`
	Generation                int64                              `json:"generation"`
	ExpectedGeneration        int64                              `json:"expected_generation"`
	SourceGeneration          int                                `json:"source_generation"`
	TargetEpoch               int                                `json:"target_epoch"`
	TargetRoundId             server.Id                          `json:"target_round_id"`
	SourceDecisionDigest      string                             `json:"source_decision_digest"`
	ApprovedPatchSha256       string                             `json:"approved_patch_sha256,omitempty"`
	Kind                      string                             `json:"kind"`
	Reason                    string                             `json:"reason"`
	BaseSourceLockSha256      string                             `json:"base_source_lock_sha256"`
	SourceLockSha256          string                             `json:"source_lock_sha256"`
	Commits                   map[string]string                  `json:"commits"`
	GitRefs                   map[string]CompetitionSourceGitRef `json:"git_refs"`
	ReviewId                  *server.Id                         `json:"review_id,omitempty"`
	ReviewDecisionDigest      string                             `json:"review_decision_digest,omitempty"`
	RewardDecisionDigest      string                             `json:"reward_decision_digest,omitempty"`
	ManifestCommit            string                             `json:"manifest_commit"`
	ManifestSha256            string                             `json:"manifest_sha256"`
	Manifest                  []byte                             `json:"manifest,omitempty"`
	PublicationJournalSha256  string                             `json:"publication_journal_sha256"`
	ConfigExpectedCommit      string                             `json:"config_expected_commit"`
	EvaluatorImageDigest      string                             `json:"evaluator_image_digest"`
	EvaluatedImageDigest      string                             `json:"evaluated_image_digest,omitempty"`
	HostQualificationSha256   string                             `json:"host_qualification_sha256"`
	Threshold                 float64                            `json:"threshold"`
	ThresholdEvidenceSha256   string                             `json:"threshold_evidence_sha256"`
	PreparedAt                time.Time                          `json:"prepared_at"`
	BuildReadyAt              time.Time                          `json:"build_ready_at,omitempty"`
	OpensAt                   time.Time                          `json:"opens_at"`
	ActivatedAt               *time.Time                         `json:"activated_at,omitempty"`
	AbandonedAt               *time.Time                         `json:"abandoned_at,omitempty"`
	IntentSha256              string                             `json:"intent_sha256"`
	State                     string                             `json:"state"`
}

// An unpublished failed promotion is retained forever when safe carry-forward replaces it.
type CompetitionSourceAbandonment struct {
	Environment            string    `json:"environment"`
	CompetitionId          string    `json:"competition_id"`
	SeasonId               string    `json:"season_id"`
	Generation             int64     `json:"generation"`
	IntentSha256           string    `json:"intent_sha256"`
	ActorId                string    `json:"actor_id"`
	FencingToken           int64     `json:"fencing_token"`
	Reason                 string    `json:"reason"`
	UnpublishedProofSha256 string    `json:"unpublished_proof_sha256"`
	CreatedAt              time.Time `json:"created_at"`
}

// Api and worker independently acknowledge the exact manifest and production readiness.
type CompetitionSourceReadiness struct {
	Staging             bool      `json:"staging,omitempty"`
	Environment         string    `json:"environment"`
	CompetitionId       string    `json:"competition_id"`
	SeasonId            string    `json:"season_id"`
	Generation          int64     `json:"generation"`
	TargetRoundId       server.Id `json:"target_round_id"`
	Role                string    `json:"role"`
	ActorId             string    `json:"actor_id"`
	ManifestCommit      string    `json:"manifest_commit"`
	ManifestSha256      string    `json:"manifest_sha256"`
	RuntimeImageDigest  string    `json:"runtime_image_digest"`
	QualificationSha256 string    `json:"qualification_sha256"`
	SourceLockSha256    string    `json:"source_lock_sha256"`
	Ready               bool      `json:"ready"`
	RecordedAt          time.Time `json:"recorded_at"`
}

// Every transition rechecks this monotonic fencing token inside its database transaction.
type CompetitionSupervisorLease struct {
	Environment    string    `json:"environment"`
	CompetitionId  string    `json:"competition_id"`
	SeasonId       string    `json:"season_id"`
	OwnerId        string    `json:"owner_id"`
	FencingToken   int64     `json:"fencing_token"`
	LeaseExpiresAt time.Time `json:"lease_expires_at"`
}

// Append-only reconciliation checkpoints describe external work without doing it in a transaction.
type CompetitionSupervisorTransition struct {
	TargetEpoch                int        `json:"target_epoch"`
	Revision                   int        `json:"revision"`
	ExpectedRevision           int        `json:"expected_revision"`
	State                      string     `json:"state"`
	Reason                     string     `json:"reason"`
	SourceActivationGeneration int64      `json:"source_activation_generation,omitempty"`
	RoundId                    *server.Id `json:"round_id,omitempty"`
	UpdatedAt                  time.Time  `json:"updated_at"`
	SourceChoiceAt             time.Time  `json:"source_choice_at"`
	PreparationStartedAt       time.Time  `json:"preparation_started_at"`
	OpensAt                    time.Time  `json:"opens_at"`
}
