// Holds and incidents are append-only control records, never edits to scores,
// prior elections, or the source of a committed round.
package model

import (
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// Freezes an independently authorized pause of one specific capability.
type CompetitionAutomationHoldArgs struct {
	Kind              string `json:"kind"`
	Active            bool   `json:"active"`
	ExpectedRevision  int    `json:"expected_revision"`
	IdempotencyKey    string `json:"idempotency_key"`
	ReasonCode        string `json:"reason_code"`
	PublicExplanation string `json:"public_explanation"`
	EvidenceSha256    string `json:"evidence_sha256"`
}

// Attributes a pause or release to its authenticated actor and immutable scope.
type CompetitionAutomationHold struct {
	HoldId        server.Id                     `json:"hold_id"`
	Environment   string                        `json:"environment"`
	CompetitionId string                        `json:"competition_id"`
	SeasonId      string                        `json:"season_id"`
	ActorId       string                        `json:"actor_id"`
	Revision      int                           `json:"revision"`
	CreatedAt     time.Time                     `json:"created_at"`
	Request       CompetitionAutomationHoldArgs `json:"request"`
	RequestSha256 string                        `json:"request_sha256"`
}

// Records evaluation invalidation separately from ordinary no improvement.
// Source incidents identify the exact compromised lock and its descendants.
type CompetitionAutomationIncidentArgs struct {
	IdempotencyKey         string `json:"idempotency_key"`
	ExpectedRewardRevision int    `json:"expected_reward_revision"`
	InvalidatesEvaluation  bool   `json:"invalidates_evaluation"`
	SourceCompromised      bool   `json:"source_compromised"`
	SourceLockSha256       string `json:"source_lock_sha256,omitempty"`
	ReasonCode             string `json:"reason_code"`
	PublicExplanation      string `json:"public_explanation"`
	EvidenceSha256         string `json:"evidence_sha256"`
}

// Preserves an incident and affected ancestry without substituting live source.
type CompetitionAutomationIncident struct {
	IncidentId                server.Id                         `json:"incident_id"`
	RoundId                   server.Id                         `json:"round_id"`
	Environment               string                            `json:"environment"`
	CompetitionId             string                            `json:"competition_id"`
	SeasonId                  string                            `json:"season_id"`
	ActorId                   string                            `json:"actor_id"`
	CreatedAt                 time.Time                         `json:"created_at"`
	Request                   CompetitionAutomationIncidentArgs `json:"request"`
	RequestSha256             string                            `json:"request_sha256"`
	AffectedSourceGenerations []int64                           `json:"affected_source_generations"`
}

// Exact bounded identities keep idempotency and public explanations meaningful.
func ValidateCompetitionAutomationHold(args CompetitionAutomationHoldArgs) error {
	if args.Kind != "admission" && args.Kind != "evaluation" && args.Kind != "source_activation" && args.Kind != "reward_delivery" {
		return errors.New("unknown automation hold capability")
	}
	if args.ExpectedRevision < 0 || args.IdempotencyKey == "" || len(args.IdempotencyKey) > 128 ||
		!competitionAutomationSha256.MatchString(args.EvidenceSha256) ||
		strings.TrimSpace(args.PublicExplanation) == "" || len(args.PublicExplanation) > 4096 {
		return errors.New("invalid automation hold identity or explanation")
	}
	if args.ReasonCode != "OPERATOR_HOLD" && args.ReasonCode != "EVALUATION_INTEGRITY_INCIDENT" &&
		args.ReasonCode != "SOURCE_COMPROMISE" && args.ReasonCode != "RECOVERY_APPROVED" {
		return errors.New("unknown automation hold reason")
	}
	if !args.Active && args.ReasonCode != "RECOVERY_APPROVED" {
		return errors.New("hold release requires an explicit recovery approval")
	}
	return nil
}

// An incident must identify an actual invalidation or a compromised source;
// reporting an incident alone cannot manufacture a favorable replacement score.
func ValidateCompetitionAutomationIncident(args CompetitionAutomationIncidentArgs) error {
	if args.IdempotencyKey == "" || len(args.IdempotencyKey) > 128 || args.ExpectedRewardRevision < 0 ||
		!competitionAutomationSha256.MatchString(args.EvidenceSha256) ||
		strings.TrimSpace(args.PublicExplanation) == "" || len(args.PublicExplanation) > 4096 ||
		!args.InvalidatesEvaluation && !args.SourceCompromised {
		return errors.New("invalid automation incident identity or explanation")
	}
	if args.InvalidatesEvaluation && args.ReasonCode != "EVALUATION_INTEGRITY_INCIDENT" {
		return errors.New("evaluation invalidation requires an evaluation integrity incident")
	}
	if args.SourceCompromised && !competitionAutomationSha256.MatchString(args.SourceLockSha256) {
		return errors.New("source compromise requires the exact source lock")
	}
	if !args.InvalidatesEvaluation && args.ReasonCode != "SOURCE_COMPROMISE" {
		return errors.New("source incident requires a source compromise reason")
	}
	return nil
}
