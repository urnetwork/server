// A prospective, single-use model run records observations, never approval.
// Its fixed clock and exact proposal survive the later independent operator gate.
package model

import (
	"errors"
	"regexp"
	"time"

	"github.com/urnetwork/server"
)

const CompetitionReviewHandoffProtocol = "elected-winner-qg-multipass/1"

var competitionReviewCallId = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// No caller-supplied clock, disposition or selected source is accepted here.
// Authorization identifies the independently approved, exact prospective run.
type CompetitionReviewHandoffArgs struct {
	RoundId                server.Id `json:"round_id"`
	JobId                  server.Id `json:"job_id"`
	RewardRevision         int       `json:"reward_revision"`
	DecisionSha256         string    `json:"decision_sha256"`
	SealSha256             string    `json:"seal_sha256"`
	PatchSha256            string    `json:"patch_sha256"`
	BaseSourceLockSha256   string    `json:"base_source_lock_sha256"`
	ScoreSha256            string    `json:"score_sha256"`
	ArtifactManifestSha256 string    `json:"artifact_manifest_sha256"`
	BaselineSha256         string    `json:"baseline_sha256"`
	AuthorizationSha256    string    `json:"authorization_sha256"`
	ProviderSha256         string    `json:"provider_sha256"`
	Protocol               string    `json:"protocol"`
	TimeoutSeconds         int       `json:"timeout_seconds"`
}

// The pure planner binds clocks after database ownership is established, but
// before commit or the first provider call. Call order is immutable and bounded.
type CompetitionReviewHandoffPlan struct {
	PlanSha256   string   `json:"plan_sha256"`
	BundleSha256 string   `json:"bundle_sha256"`
	CallIds      []string `json:"call_ids"`
}

type CompetitionReviewHandoff struct {
	HandoffId server.Id                    `json:"handoff_id"`
	ActorId   string                       `json:"actor_id"`
	Args      CompetitionReviewHandoffArgs `json:"args"`
	Plan      CompetitionReviewHandoffPlan `json:"plan"`
	StartedAt time.Time                    `json:"started_at"`
	Deadline  time.Time                    `json:"deadline"`
}

// Claims commit before dispatch. A lost or ambiguous call never gets a second
// claim. Outcomes may be retained after expiry, but cannot renew authority.
type CompetitionReviewHandoffCall struct {
	HandoffId      server.Id `json:"handoff_id"`
	CallId         string    `json:"call_id"`
	Kind           string    `json:"kind"`
	RequestSha256  string    `json:"request_sha256"`
	EvidenceSha256 string    `json:"evidence_sha256,omitempty"`
	Failure        string    `json:"failure,omitempty"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// A protected exact-version retention receipt is not itself a model judgment.
type CompetitionReviewHandoffRetention struct {
	Key         string    `json:"key"`
	VersionId   string    `json:"version_id"`
	Sha256      string    `json:"sha256"`
	RetainUntil time.Time `json:"retain_until"`
}

// Proposal is nil for failed/incomplete/uncertain evidence. Even a complete
// proposal has no effect until the independent review_operator capability acts.
type CompetitionReviewHandoffCompletion struct {
	HandoffId      server.Id                          `json:"handoff_id"`
	PlanSha256     string                             `json:"plan_sha256"`
	Evidence       CompetitionReviewHandoffRetention  `json:"evidence"`
	Proposal       *CompetitionIntegrityReviewRequest `json:"proposal,omitempty"`
	ProposalSha256 string                             `json:"proposal_sha256,omitempty"`
	Failure        string                             `json:"failure,omitempty"`
	CompletedAt    time.Time                          `json:"completed_at"`
}

// The operator selects no findings, clocks or replacement recipient. Import
// means publishing precisely the retained model proposal, including rejection.
type CompetitionReviewHandoffImport struct {
	HandoffId      server.Id `json:"handoff_id"`
	RoundId        server.Id `json:"round_id"`
	DecisionSha256 string    `json:"decision_sha256"`
	ProposalSha256 string    `json:"proposal_sha256"`
	EvidenceSha256 string    `json:"evidence_sha256"`
	Disposition    string    `json:"disposition"`
}

func ValidateCompetitionReviewHandoffArgs(value CompetitionReviewHandoffArgs) error {
	if value.RoundId == (server.Id{}) || value.JobId == (server.Id{}) || value.RewardRevision < 1 || value.Protocol != CompetitionReviewHandoffProtocol || value.TimeoutSeconds < 60 || value.TimeoutSeconds > 24*60*60 {
		return errors.New("review handoff requires one bounded prospective elected-winner run")
	}
	for _, digest := range []string{value.DecisionSha256, value.SealSha256, value.PatchSha256, value.BaseSourceLockSha256, value.ScoreSha256, value.ArtifactManifestSha256, value.BaselineSha256, value.AuthorizationSha256, value.ProviderSha256} {
		if !competitionAutomationSha256.MatchString(digest) {
			return errors.New("review handoff identity digest is invalid")
		}
	}
	return nil
}

func ValidateCompetitionReviewHandoffPlan(value CompetitionReviewHandoffPlan) error {
	if !competitionAutomationSha256.MatchString(value.PlanSha256) || !competitionAutomationSha256.MatchString(value.BundleSha256) || len(value.CallIds) < 2 || len(value.CallIds) > 384 {
		return errors.New("review handoff requires an exact bounded plan and call census")
	}
	seen := map[string]bool{}
	for _, id := range value.CallIds {
		if !competitionReviewCallId.MatchString(id) || seen[id] {
			return errors.New("review handoff call census is invalid or duplicated")
		}
		seen[id] = true
	}
	return nil
}

func ValidateCompetitionReviewHandoffImport(value CompetitionReviewHandoffImport) error {
	if value.HandoffId == (server.Id{}) || value.RoundId == (server.Id{}) || value.Disposition != "publish_exact_model_proposal" || !competitionAutomationSha256.MatchString(value.DecisionSha256) || !competitionAutomationSha256.MatchString(value.ProposalSha256) || !competitionAutomationSha256.MatchString(value.EvidenceSha256) {
		return errors.New("operator import requires the exact pending proposal and completion evidence")
	}
	return nil
}

// Value fields preserve comparability and omit the new identity on legacy JSON.
func (self CompetitionReviewHandoff) Checkpoint() CompetitionWinnerReviewCheckpoint {
	return CompetitionWinnerReviewCheckpoint{RoundId: self.Args.RoundId, JobId: self.Args.JobId, RewardRevision: self.Args.RewardRevision,
		RewardDecisionSha256: self.Args.DecisionSha256, StartedAt: self.StartedAt, Deadline: self.Deadline, HandoffId: self.HandoffId.String()}
}

// Only native storage may populate ModelCompletedAt. The reference must match
// the durable checkpoint; an absent completion cannot replace the legacy clock.
func CompetitionReviewWithinCheckpoint(review *CompetitionIntegrityReviewResult, checkpoint *CompetitionWinnerReviewCheckpoint) bool {
	if review == nil || checkpoint == nil || review.Request.HandoffId != checkpoint.HandoffId {
		return false
	}
	observed := review.CreatedAt
	if checkpoint.HandoffId != "" {
		if review.ModelCompletedAt == nil {
			return false
		}
		observed = *review.ModelCompletedAt
	} else if review.ModelCompletedAt != nil {
		return false
	}
	return !observed.Before(checkpoint.StartedAt) && observed.Before(checkpoint.Deadline)
}
