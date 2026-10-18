// Independent append-only holds and integrity incidents do not discard admitted
// work, alter source pins, or conflate an invalid evaluation with no improvement.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var (
	ErrAutomationHeld        = errors.New("competition capability is paused by an authorized hold")
	ErrEvaluationInvalidated = errors.New("competition evaluation is invalidated by an integrity incident")
)

// Reads the latest revision for one capability on the caller's resource.
func ensureAutomationNotHeldInTx(ctx context.Context, tx server.PgTx, settings *Settings, kind string) error {
	var active bool
	err := tx.QueryRow(ctx, `
		SELECT active FROM competition_automation_hold
		WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND kind=$4
		ORDER BY revision DESC LIMIT 1
	`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, kind).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if active {
		return ErrAutomationHeld
	}
	return nil
}

// An incident cannot be cleared by an ordinary review or by releasing a hold.
// Re-rounding requires the separate authorized cancellation/replacement workflow.
func ensureAutomationRoundValidInTx(ctx context.Context, tx server.PgTx, roundId server.Id) error {
	var invalidated bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM competition_automation_incident WHERE round_id=$1 AND invalidates_evaluation
	)`, roundId).Scan(&invalidated); err != nil {
		return err
	}
	if invalidated {
		return ErrEvaluationInvalidated
	}
	return nil
}

// Serializes pause/release against admissions, reward delivery, and source
// activation. Same-key replays return the original record, never a new revision.
func (self PostgresStore) SetAutomationHold(ctx context.Context, settings *Settings, actorId string, args model.CompetitionAutomationHoldArgs) (result *model.CompetitionAutomationHold, err error) {
	if settings == nil || !tokenNamePattern.MatchString(settings.AutomationPolicy.Environment) ||
		!tokenNamePattern.MatchString(settings.AutomationPolicy.SeasonId) || !tokenNamePattern.MatchString(settings.CompetitionId) {
		return nil, ErrConflict
	}
	if !tokenNamePattern.MatchString(actorId) {
		return nil, errors.New("authenticated hold actor is required")
	}
	if err := model.ValidateCompetitionAutomationHold(args); err != nil {
		return nil, err
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			result, err = recordAutomationHoldInTx(ctx, tx, settings, actorId, args, self.nowUtc())
			server.Raise(err)
		})
	})
	return result, err
}

// Called only with the scope lock already held; it acquires no other resource.
func recordAutomationHoldInTx(ctx context.Context, tx server.PgTx, settings *Settings, actorId string, args model.CompetitionAutomationHoldArgs, now time.Time) (*model.CompetitionAutomationHold, error) {
	digest, err := model.CompetitionAutomationDigest(args)
	if err != nil {
		return nil, err
	}
	var existingDigest string
	var content []byte
	err = tx.QueryRow(ctx, `SELECT request_sha256,hold_json FROM competition_automation_hold
		WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND actor_id=$4 AND idempotency_key=$5`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, actorId, args.IdempotencyKey).Scan(&existingDigest, &content)
	if err == nil {
		if existingDigest != digest {
			return nil, ErrConflict
		}
		var result model.CompetitionAutomationHold
		if err := json.Unmarshal(content, &result); err != nil {
			return nil, err
		}
		return &result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var revision int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0) FROM competition_automation_hold
		WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND kind=$4`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, args.Kind).Scan(&revision); err != nil {
		return nil, err
	}
	if revision != args.ExpectedRevision {
		return nil, ErrConflict
	}
	result := &model.CompetitionAutomationHold{
		HoldId: server.NewId(), Environment: settings.AutomationPolicy.Environment,
		CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		ActorId: actorId, Revision: revision + 1, CreatedAt: now, Request: args, RequestSha256: digest,
	}
	content, err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO competition_automation_hold (
		hold_id,environment,competition_id,season_id,kind,revision,active,actor_id,idempotency_key,request_sha256,hold_json,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb,$12)`,
		result.HoldId, result.Environment, result.CompetitionId, result.SeasonId, args.Kind,
		result.Revision, args.Active, actorId, args.IdempotencyKey, digest, string(content), now)
	return result, err
}

// Incident-created holds append a real revision instead of silently overriding
// an existing pause. Its deterministic key makes transaction replays harmless.
func recordIncidentHoldInTx(ctx context.Context, tx server.PgTx, settings *Settings, actorId, kind string, incident *model.CompetitionAutomationIncident) error {
	var revision int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0) FROM competition_automation_hold
		WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND kind=$4`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, kind).Scan(&revision); err != nil {
		return err
	}
	_, err := recordAutomationHoldInTx(ctx, tx, settings, actorId, model.CompetitionAutomationHoldArgs{
		Kind: kind, Active: true, ExpectedRevision: revision,
		IdempotencyKey: "incident:" + incident.IncidentId.String() + ":" + kind,
		ReasonCode:     incident.Request.ReasonCode, PublicExplanation: incident.Request.PublicExplanation,
		EvidenceSha256: incident.Request.EvidenceSha256,
	}, incident.CreatedAt)
	return err
}

// Walks the exact source-lock ancestry, not numeric ordering or moving refs.
// A compromised lock can be reused by multiple carry-forward generations.
func affectedAutomationSourceGenerations(intents []model.CompetitionSourceActivation, compromisedLock string) []int64 {
	affectedLockKVs := map[string]bool{compromisedLock: true}
	affectedGenerationKVs := map[int64]bool{}
	for changed := true; changed; {
		changed = false
		for _, intent := range intents {
			if !affectedGenerationKVs[intent.Generation] &&
				(affectedLockKVs[intent.SourceLockSha256] || affectedLockKVs[intent.BaseSourceLockSha256]) {
				affectedGenerationKVs[intent.Generation] = true
				affectedLockKVs[intent.SourceLockSha256] = true
				changed = true
			}
		}
	}
	result := make([]int64, 0, len(affectedGenerationKVs))
	for generation := range affectedGenerationKVs {
		result = append(result, generation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// Records the incident, independent capability holds, and an explicit reward
// invalidation revision together. Original ranking and scores remain immutable.
func (self PostgresStore) RecordAutomationIncident(ctx context.Context, settings *Settings, roundId server.Id, actorId string, args model.CompetitionAutomationIncidentArgs) (result *model.CompetitionAutomationIncident, err error) {
	if settings == nil || !tokenNamePattern.MatchString(settings.AutomationPolicy.Environment) ||
		!tokenNamePattern.MatchString(settings.AutomationPolicy.SeasonId) || !tokenNamePattern.MatchString(settings.CompetitionId) {
		return nil, ErrConflict
	}
	if !tokenNamePattern.MatchString(actorId) {
		return nil, errors.New("authenticated incident actor is required")
	}
	if err := model.ValidateCompetitionAutomationIncident(args); err != nil {
		return nil, err
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			_, _, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			result, loadErr = self.recordAutomationIncidentInTx(ctx, tx, settings, roundId, actorId, args)
			server.Raise(loadErr)
		})
	})
	return result, err
}

// Owns no acquisition. Callers lock the scope, then the affected round.
func (self PostgresStore) recordAutomationIncidentInTx(ctx context.Context, tx server.PgTx, settings *Settings, roundId server.Id, actorId string, args model.CompetitionAutomationIncidentArgs) (*model.CompetitionAutomationIncident, error) {
	digest, err := model.CompetitionAutomationDigest(args)
	if err != nil {
		return nil, err
	}
	var storedDigest string
	var content []byte
	err = tx.QueryRow(ctx, `SELECT request_sha256,incident_json FROM competition_automation_incident
		WHERE round_id=$1 AND actor_id=$2 AND idempotency_key=$3`, roundId, actorId, args.IdempotencyKey).Scan(&storedDigest, &content)
	if err == nil {
		if storedDigest != digest {
			return nil, ErrConflict
		}
		var result model.CompetitionAutomationIncident
		if err := json.Unmarshal(content, &result); err != nil {
			return nil, err
		}
		return &result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var revision int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0) FROM competition_reward_decision WHERE round_id=$1`, roundId).Scan(&revision); err != nil {
		return nil, err
	}
	if revision != args.ExpectedRewardRevision {
		return nil, ErrConflict
	}
	result := &model.CompetitionAutomationIncident{
		IncidentId: server.NewId(), RoundId: roundId, Environment: settings.AutomationPolicy.Environment,
		CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		ActorId: actorId, CreatedAt: self.nowUtc(), Request: args, RequestSha256: digest,
		AffectedSourceGenerations: []int64{},
	}
	if args.SourceCompromised {
		intents := []model.CompetitionSourceActivation{}
		rows, queryErr := tx.Query(ctx, `SELECT intent_json FROM competition_source_activation
			WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, result.Environment, result.CompetitionId, result.SeasonId)
		server.WithPgResult(rows, queryErr, func() {
			for rows.Next() {
				var intent model.CompetitionSourceActivation
				server.Raise(rows.Scan(&content))
				server.Raise(json.Unmarshal(content, &intent))
				intents = append(intents, intent)
			}
		})
		result.AffectedSourceGenerations = affectedAutomationSourceGenerations(intents, args.SourceLockSha256)
		if len(result.AffectedSourceGenerations) == 0 {
			return nil, ErrConflict
		}
	}
	content, err = json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO competition_automation_incident (
		incident_id,round_id,environment,competition_id,season_id,invalidates_evaluation,actor_id,idempotency_key,request_sha256,incident_json,created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11)`, result.IncidentId, roundId,
		result.Environment, result.CompetitionId, result.SeasonId, args.InvalidatesEvaluation, actorId,
		args.IdempotencyKey, digest, string(content), result.CreatedAt); err != nil {
		return nil, err
	}
	holdKinds := []string{"admission", "source_activation"}
	if args.SourceCompromised {
		// Do not start more work with known unsafe incumbent code. An
		// already-running sandbox still needs the separate containment response.
		holdKinds = append(holdKinds, "evaluation")
	}
	for _, kind := range holdKinds {
		if err := recordIncidentHoldInTx(ctx, tx, settings, actorId, kind, result); err != nil {
			return nil, err
		}
	}
	if args.InvalidatesEvaluation && revision > 0 {
		current := automationResultInTx(ctx, tx, roundId)
		previous := current.Decisions[len(current.Decisions)-1]
		if previous.Action != "invalidated" {
			next := previous
			next.Revision, next.PreviousDecisionSha256 = previous.Revision+1, previous.DecisionSha256
			next.SupersededRevision, next.SupersededJobId = previous.Revision, previous.SelectedJobId
			next.SelectedJobId, next.EffectiveSettlementAnchor, next.ReviewId = nil, nil, nil
			next.Action, next.ReasonCode, next.DecidedAt = "invalidated", "EVALUATION_INTEGRITY_INCIDENT", result.CreatedAt
			if err := appendAutomationDecisionInTx(ctx, tx, &next, current.Seal.PublicationAt); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

// Extends an already authorized result read without opening a second connection.
func automationIncidentsInTx(ctx context.Context, tx server.PgTx, roundId server.Id) []model.CompetitionAutomationIncident {
	result := []model.CompetitionAutomationIncident{}
	rows, err := tx.Query(ctx, `SELECT incident_json FROM competition_automation_incident WHERE round_id=$1 ORDER BY created_at,incident_id`, roundId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var content []byte
			var incident model.CompetitionAutomationIncident
			server.Raise(rows.Scan(&content))
			server.Raise(json.Unmarshal(content, &incident))
			result = append(result, incident)
		}
	})
	return result
}

// Rejection after activation is an incident, not an instruction to rewrite an
// incumbent. Descendant admissions and further activations stop atomically with
// the review; the already active host still requires the operator incident runbook.
func (self PostgresStore) recordAutomationSourceRejectionInTx(ctx context.Context, tx server.PgTx, settings *Settings, review *model.CompetitionIntegrityReviewResult) error {
	if review.Request.RewardDisposition != "disqualified" && review.Request.SourceDisposition != "rejected" {
		return nil
	}
	intents := []model.CompetitionSourceActivation{}
	rows, err := tx.Query(ctx, `SELECT intent_json FROM competition_source_activation
		WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND activated_at IS NOT NULL`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var content []byte
			var intent model.CompetitionSourceActivation
			server.Raise(rows.Scan(&content))
			server.Raise(json.Unmarshal(content, &intent))
			if intent.ReviewId != nil {
				intents = append(intents, intent)
			}
		}
	})
	for _, intent := range intents {
		var approvedJobId server.Id
		if err := tx.QueryRow(ctx, `SELECT job_id FROM competition_integrity_review WHERE review_id=$1`, *intent.ReviewId).Scan(&approvedJobId); err != nil {
			return err
		}
		if approvedJobId != review.Request.JobId {
			continue
		}
		if _, err := self.recordAutomationIncidentInTx(ctx, tx, settings, review.RoundId, review.ActorId, model.CompetitionAutomationIncidentArgs{
			IdempotencyKey:         "source-rejection:" + review.ReviewId.String() + ":" + intent.SourceLockSha256,
			ExpectedRewardRevision: review.RewardDecision.Revision,
			SourceCompromised:      true, SourceLockSha256: intent.SourceLockSha256, ReasonCode: "SOURCE_COMPROMISE",
			PublicExplanation: review.Request.PublicExplanation, EvidenceSha256: review.Request.PrivateEvidenceSha256,
		}); err != nil {
			return err
		}
	}
	return nil
}
