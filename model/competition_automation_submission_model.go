// Submission screening discloses known exact-identity exclusions before a fee side effect.
package model

import "github.com/urnetwork/server"

// Screening is not admission, a payment instruction, or a promise of a future reward.
type CompetitionV2SubmissionPreflight struct {
	RoundId               server.Id `json:"round_id"`
	PatchSha256           string    `json:"patch_sha256"`
	BaseSourceLockSha256  string    `json:"base_source_lock_sha256"`
	DecisionMode          string    `json:"decision_mode"`
	RequiredApiCapability string    `json:"required_api_capability"`
	RewardEligible        bool      `json:"reward_eligible"`
	IneligibilityReason   string    `json:"ineligibility_reason,omitempty"`
}
