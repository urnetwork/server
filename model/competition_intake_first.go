// Source-only admission and grading activation are different authorities.
// Neither an intake grant nor its genuine API receipt asserts worker readiness,
// a measured baseline, completed qualification, or a submission execution clock.
package model

import (
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

const CompetitionIntakeFirstKind = "published-source-intake/v1"

const CompetitionSourceOperatorHandoffKind = "source-operator-handoff/v1"

// A prospective operator policy selects the new lifecycle explicitly. Omission
// preserves every historical activation, snapshot and intake exception.
type CompetitionIntakeFirstPolicy struct {
	Kind           string `json:"kind" yaml:"kind"`
	FromEpoch      int    `json:"from_epoch" yaml:"from_epoch"`
	ApprovalSha256 string `json:"approval_sha256" yaml:"approval_sha256"`
}

// The approval is a protected control-policy pin, not a self-reported test pass.
func ValidateCompetitionIntakeFirstPolicy(policy *CompetitionIntakeFirstPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.Kind != CompetitionIntakeFirstKind || policy.FromEpoch != 2 || !competitionAutomationSha256.MatchString(policy.ApprovalSha256) {
		return errors.New("intake-first lifecycle requires its explicit prospective control approval")
	}
	return nil
}

// This append-only grant authenticates discovery and submissions only. The
// complete real-image schema-two round policy is frozen before opening; later
// grading cannot replace its product, numerical rules or instrument identity.
type CompetitionSourceIntakeActivation struct {
	Schema                int                          `json:"schema"`
	Kind                  string                       `json:"kind"`
	Policy                CompetitionIntakeFirstPolicy `json:"policy"`
	Environment           string                       `json:"environment"`
	CompetitionId         string                       `json:"competition_id"`
	SeasonId              string                       `json:"season_id"`
	Generation            int64                        `json:"generation"`
	ExpectedGeneration    int64                        `json:"expected_generation"`
	TargetEpoch           int                          `json:"target_epoch"`
	TargetRoundId         server.Id                    `json:"target_round_id"`
	IntentSha256          string                       `json:"intent_sha256"`
	RoundPolicySha256     string                       `json:"round_policy_sha256"`
	ApiReadinessSha256    string                       `json:"api_readiness_sha256"`
	ApiRuntimeImageDigest string                       `json:"api_runtime_image_digest"`
	SeasonEndsAt          time.Time                    `json:"season_ends_at"`
	RetainUntil           time.Time                    `json:"retain_until"`
	CreatedAt             time.Time                    `json:"created_at"`
}

// Shape checking is not source, promotion or receipt authority. The concrete
// store must independently re-read those facts under its existing scope fence.
func ValidateCompetitionSourceIntakeActivation(value *CompetitionSourceIntakeActivation) error {
	if value == nil || value.Schema != 1 || value.Kind != CompetitionIntakeFirstKind ||
		ValidateCompetitionIntakeFirstPolicy(&value.Policy) != nil ||
		!competitionAutomationScope.MatchString(value.Environment) || !competitionAutomationScope.MatchString(value.CompetitionId) || !competitionAutomationScope.MatchString(value.SeasonId) ||
		value.Generation <= value.ExpectedGeneration || value.ExpectedGeneration < 1 || value.TargetEpoch < value.Policy.FromEpoch || value.TargetEpoch > 6 ||
		value.TargetRoundId == (server.Id{}) || !competitionAutomationSha256.MatchString(value.IntentSha256) ||
		!competitionAutomationSha256.MatchString(value.RoundPolicySha256) || !competitionAutomationSha256.MatchString(value.ApiReadinessSha256) ||
		!strings.HasPrefix(value.ApiRuntimeImageDigest, "sha256:") || !competitionAutomationSha256.MatchString(strings.TrimPrefix(value.ApiRuntimeImageDigest, "sha256:")) || value.CreatedAt.IsZero() ||
		!value.CreatedAt.Before(value.SeasonEndsAt) || value.RetainUntil.Before(value.SeasonEndsAt) {
		return errors.New("source intake grant lacks exact prospective source, API or frozen-rule identity")
	}
	return nil
}

// Human handoff supplements the frozen provider review. It never replaces a
// review model, invents a winner or grants image/host readiness. The selected
// product lock is distinct from an independently approved instrument overlay.
type CompetitionSourceOperatorHandoffArgs struct {
	TargetEpoch             int        `json:"target_epoch"`
	ExpectedRevision        int64      `json:"expected_revision"`
	RewardDecisionSha256    string     `json:"reward_decision_sha256"`
	SelectedJobId           *server.Id `json:"selected_job_id,omitempty"`
	PatchSha256             string     `json:"patch_sha256,omitempty"`
	ReviewId                *server.Id `json:"review_id,omitempty"`
	ReviewDecisionSha256    string     `json:"review_decision_sha256,omitempty"`
	ProductSourceLockSha256 string     `json:"product_source_lock_sha256"`
	Disposition             string     `json:"disposition"`
	IdempotencyKey          string     `json:"idempotency_key"`
	EvidenceSha256          string     `json:"evidence_sha256"`
}

// A new revision never edits or erases the decision a previous operator saw.
type CompetitionSourceOperatorHandoff struct {
	Schema        int                                  `json:"schema"`
	Kind          string                               `json:"kind"`
	Environment   string                               `json:"environment"`
	CompetitionId string                               `json:"competition_id"`
	SeasonId      string                               `json:"season_id"`
	Revision      int64                                `json:"revision"`
	ActorId       string                               `json:"actor_id"`
	Request       CompetitionSourceOperatorHandoffArgs `json:"request"`
	RequestSha256 string                               `json:"request_sha256"`
	CreatedAt     time.Time                            `json:"created_at"`
}

// Shape validation is followed by a fenced reread of the elected decision,
// current complete review and exact proposed product graph in the store.
func ValidateCompetitionSourceOperatorHandoffArgs(value CompetitionSourceOperatorHandoffArgs) error {
<<<<<<< HEAD
	if value.TargetEpoch < 2 || value.TargetEpoch > 6 || value.ExpectedRevision < 0 || value.ExpectedRevision == 1<<63-1 ||
=======
	if value.TargetEpoch < 2 || value.TargetEpoch > 6 || value.ExpectedRevision < 0 ||
>>>>>>> 9cb51eb62ff634f3a73915d1f849659a879811f8
		!competitionAutomationSha256.MatchString(value.RewardDecisionSha256) ||
		!competitionAutomationSha256.MatchString(value.ProductSourceLockSha256) || !competitionAutomationSha256.MatchString(value.EvidenceSha256) ||
		!competitionAutomationScope.MatchString(value.IdempotencyKey) {
		return errors.New("operator source handoff has incomplete scope or evidence")
	}
	switch value.Disposition {
	case "approve_promotion", "reject_promotion":
		if value.SelectedJobId == nil || *value.SelectedJobId == (server.Id{}) || value.ReviewId == nil || *value.ReviewId == (server.Id{}) ||
			!competitionAutomationSha256.MatchString(value.PatchSha256) || !competitionAutomationSha256.MatchString(value.ReviewDecisionSha256) {
			return errors.New("operator promotion handoff must name the genuine elected job, patch and review")
		}
	case "approve_no_winner_carry":
		if value.SelectedJobId != nil || value.ReviewId != nil || value.PatchSha256 != "" || value.ReviewDecisionSha256 != "" {
			return errors.New("no-winner handoff cannot manufacture a selected job or review")
		}
	default:
		return errors.New("unsupported operator source handoff disposition")
	}
	return nil
}
