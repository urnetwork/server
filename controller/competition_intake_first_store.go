// Durable source-intake grants are separate from grading runtime pointers.
// Every write shares the existing scope lock; no connection spans publication,
// archive traffic, model review, image construction or worker execution.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A single receipt never stands in for the absent independent worker role.
func sourceIntakeApiReceipt(ctx context.Context, reader competitionIntakeReader, settings *Settings, intent *model.CompetitionSourceActivation) (model.CompetitionSourceReadiness, error) {
	var content []byte
	err := reader.QueryRow(ctx, `SELECT receipt_json FROM competition_source_readiness WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4 AND role='api'`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, intent.Generation).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.CompetitionSourceReadiness{}, ErrReviewNotReady
	}
	if err != nil {
		return model.CompetitionSourceReadiness{}, err
	}
	var receipt model.CompetitionSourceReadiness
	if err := json.Unmarshal(content, &receipt); err != nil {
		return receipt, err
	}
	_, err = competitionIntakeFirstApiReceiptDigest(intent, receipt)
	return receipt, err
}

// Canonical typed digests survive PostgreSQL jsonb key ordering. Unknown or
// absent storage capability is not converted into a historical authority.
func sourceIntakeActivation(ctx context.Context, reader competitionIntakeReader, settings *Settings, generation int64) (*model.CompetitionSourceIntakeActivation, error) {
	var content []byte
	var expected string
	err := reader.QueryRow(ctx, `SELECT grant_json,grant_sha256 FROM competition_source_intake_activation WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, generation).Scan(&content, &expected)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var value model.CompetitionSourceIntakeActivation
	if err := json.Unmarshal(content, &value); err != nil {
		return nil, err
	}
	digest, err := model.CompetitionAutomationDigest(value)
	if err != nil || digest != expected || model.ValidateCompetitionSourceIntakeActivation(&value) != nil || value.Generation != generation ||
		value.Environment != settings.AutomationPolicy.Environment || value.CompetitionId != settings.CompetitionId || value.SeasonId != settings.AutomationPolicy.SeasonId {
		return nil, ErrConflict
	}
	return &value, nil
}

// Only the last disposition for this target can authorize publication; an
// earlier approval cannot bypass a later reject or a changed elected source.
func sourceOperatorHandoff(ctx context.Context, reader competitionIntakeReader, settings *Settings, target int) (*model.CompetitionSourceOperatorHandoff, error) {
	var content []byte
	err := reader.QueryRow(ctx, `SELECT handoff_json FROM competition_source_operator_handoff WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 ORDER BY revision DESC LIMIT 1`,
		settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, target).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errCompetitionSourceOperatorHandoff
	}
	if err != nil {
		return nil, err
	}
	var value model.CompetitionSourceOperatorHandoff
	if err := json.Unmarshal(content, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

// Configuration is the explicitly approved lifecycle capability, never a
// caller's claim that the operator or another process has completed a step.
func setCompetitionIntakeFirstAuthority(ctx context.Context, tx server.PgTx, settings *Settings) error {
	if settings.IntakeFirstPolicy == nil || model.ValidateCompetitionIntakeFirstPolicy(settings.IntakeFirstPolicy) != nil {
		return ErrConflict
	}
	content, err := json.Marshal(settings.IntakeFirstPolicy)
	if err != nil {
		return err
	}
	approvalDigest, approvalJson := "", ""
	if settings.FutureEpochPolicy != nil && settings.FutureEpochPolicy.TrustedInstrument != nil {
		approval := settings.FutureEpochPolicy.TrustedInstrument
		if err := ValidateCompetitionTrustedInstrumentApproval(approval); err != nil {
			return err
		}
		encoded, err := json.Marshal(approval)
		if err != nil {
			return err
		}
		approvalDigest, approvalJson = approval.ApprovalSha256, string(encoded)
	}
	_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.competition_intake_first_policy',$1,true),
 set_config('urnetwork.competition_v2_instrument_approval',$2,true),set_config('urnetwork.competition_v2_instrument_approval_json',$3,true)`, string(content), approvalDigest, approvalJson)
	return err
}

// This read method is intentionally not current-source activation: runtime
// consumers must retain the old genuine two-role grading-activation contract.
func (self PostgresStore) SourceIntakeActivation(ctx context.Context, settings *Settings, generation int64) (result *model.CompetitionSourceIntakeActivation, resultErr error) {
	var stateErr error
	resultErr = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, stateErr = sourceIntakeActivation(ctx, conn, settings, generation)
			if !errors.Is(stateErr, ErrNotFound) {
				server.Raise(stateErr)
			}
		})
	})
	if resultErr == nil {
		resultErr = stateErr
	}
	return result, resultErr
}

// The human capability is distinct from the automated source publisher. This
// journal supplements, rather than replaces, current election/provider review.
func (self PostgresStore) RecordSourceOperatorHandoff(ctx context.Context, settings *Settings, actor string, request model.CompetitionSourceOperatorHandoffArgs) (result *model.CompetitionSourceOperatorHandoff, resultErr error) {
	if model.ValidateCompetitionSourceOperatorHandoffArgs(request) != nil || !tokenNamePattern.MatchString(actor) || settings.IntakeFirstPolicy == nil {
		return nil, ErrConflict
	}
	requestDigest, err := model.CompetitionAutomationDigest(request)
	if err != nil {
		return nil, err
	}
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(setCompetitionIntakeFirstAuthority(ctx, tx, settings))
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_source_handoff_actor',$1,true),set_config('urnetwork.competition_source_handoff_capability','source_handoff',true)`, actor))
			var prior []byte
			loadErr := tx.QueryRow(ctx, `SELECT handoff_json FROM competition_source_operator_handoff WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND idempotency_key=$4`,
				settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, request.IdempotencyKey).Scan(&prior)
			if loadErr == nil {
				var existing model.CompetitionSourceOperatorHandoff
				server.Raise(json.Unmarshal(prior, &existing))
				if existing.RequestSha256 != requestDigest || existing.ActorId != actor {
					panic(ErrConflict)
				}
				result = &existing
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			var generation, revision int64
			server.Raise(tx.QueryRow(ctx, `SELECT generation FROM competition_source_activation WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 AND abandoned_at IS NULL ORDER BY generation DESC LIMIT 1`,
				settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, request.TargetEpoch).Scan(&generation))
			intent, err := sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(err)
			current, err := currentSourceActivationInTx(ctx, tx, settings)
			server.Raise(err)
			server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, intent, current))
			server.Raise(tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0) FROM competition_source_operator_handoff WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, request.TargetEpoch).Scan(&revision))
			if revision != request.ExpectedRevision || !competitionIntakeFirstApplies(settings, intent) {
				panic(ErrConflict)
			}
			if request.SelectedJobId != nil {
				var selected server.Id
				server.Raise(tx.QueryRow(ctx, `SELECT job_id FROM competition_integrity_review WHERE review_id=$1`, request.ReviewId).Scan(&selected))
				if selected != *request.SelectedJobId {
					panic(ErrConflict)
				}
			}
			value := &model.CompetitionSourceOperatorHandoff{Schema: 1, Kind: model.CompetitionSourceOperatorHandoffKind, Environment: intent.Environment,
				CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Revision: revision + 1, ActorId: actor, Request: request, RequestSha256: requestDigest, CreatedAt: self.nowUtc().Truncate(time.Microsecond)}
			if !competitionSourceOperatorHandoffTargets(intent, value, false) {
				panic(ErrConflict)
			}
			content, err := json.Marshal(value)
			server.Raise(err)
			canonicalRequest, err := json.Marshal(request)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_operator_handoff(environment,competition_id,season_id,target_epoch,revision,idempotency_key,handoff_json,request_canonical,created_at)
 VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9)`, intent.Environment, intent.CompetitionId, intent.SeasonId, request.TargetEpoch, value.Revision, request.IdempotencyKey, string(content), canonicalRequest, value.CreatedAt))
			result = value
		}, server.OptReadWrite())
	})
	return result, resultErr
}

// The scope lock serializes this check with a replacement review/decision and
// later rejection; SQL independently repeats the positive gate on intake writes.
func requireSourceOperatorHandoffInTx(ctx context.Context, tx server.PgTx, settings *Settings, intent *model.CompetitionSourceActivation) error {
	if !competitionIntakeFirstApplies(settings, intent) {
		return nil
	}
	handoff, err := sourceOperatorHandoff(ctx, tx, settings, intent.TargetEpoch)
	if err != nil {
		return err
	}
	if !competitionSourceOperatorHandoffMatches(intent, handoff) {
		return errCompetitionSourceOperatorHandoff
	}
	return nil
}

// External Git/config publication is last after this fenced re-read. The hook
// may prepare local objects earlier but cannot use a stale human decision.
func (self PostgresStore) CheckSourceOperatorHandoff(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, generation int64) error {
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			intent, err := sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(err)
			current, err := currentSourceActivationInTx(ctx, tx, settings)
			server.Raise(err)
			server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, intent, current))
			server.Raise(requireSourceOperatorHandoffInTx(ctx, tx, settings, intent))
		})
	})
}

// Independent API readiness and all immutable source/rule pins are sufficient
// for this purpose only. A successful call never records a worker receipt or
// changes the grading runtime pointer or any submission execution clock.
func (self PostgresStore) ActivateSourceIntake(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, generation int64) (result *model.CompetitionSourceIntakeActivation, resultErr error) {
	resultErr = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			server.Raise(setCompetitionIntakeFirstAuthority(ctx, tx, settings))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "source_activation"))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "admission"))
			intent, err := sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(err)
			current, err := currentSourceActivationInTx(ctx, tx, settings)
			server.Raise(err)
			if current.Generation == generation {
				// A later genuine runtime activation must still use the same grant;
				// it cannot synthesize an intake approval after the fact.
				result, err = sourceIntakeActivation(ctx, tx, settings, generation)
				server.Raise(err)
				return
			}
			server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, intent, current))
			server.Raise(requireSourceOperatorHandoffInTx(ctx, tx, settings, intent))
			api, err := sourceIntakeApiReceipt(ctx, tx, settings, intent)
			server.Raise(err)
			var published bool
			server.Raise(tx.QueryRow(ctx, `SELECT COALESCE((SELECT (transition_json->>'source_activation_generation')::bigint=$5 AND state IN ('preparation_pending','preparation_blocked','preparation_ready','round_created')
 FROM competition_supervisor_transition WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 ORDER BY revision DESC LIMIT 1),false)`,
				intent.Environment, intent.CompetitionId, intent.SeasonId, intent.TargetEpoch, generation).Scan(&published))
			if !published {
				panic(ErrReviewNotReady)
			}
			existing, err := sourceIntakeActivation(ctx, tx, settings, generation)
			if err == nil {
				expected, compareErr := competitionIntakeFirstActivationValue(settings, intent, api, existing.CreatedAt)
				server.Raise(compareErr)
				if *expected != *existing {
					panic(ErrConflict)
				}
				result = existing
				return
			}
			if !errors.Is(err, ErrNotFound) {
				server.Raise(err)
			}
			value, err := competitionIntakeFirstActivationValue(settings, intent, api, self.nowUtc().Truncate(time.Microsecond))
			server.Raise(err)
			content, err := json.Marshal(value)
			server.Raise(err)
			digest, err := model.CompetitionAutomationDigest(value)
			server.Raise(err)
			frozen, _, err := competitionIntakeFirstFrozenSettings(settings, intent)
			server.Raise(err)
			policy, err := policySnapshot(frozen)
			server.Raise(err)
			apiCanonical, err := json.Marshal(api)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_intake_activation(environment,competition_id,season_id,generation,target_round_id,target_epoch,grant_json,grant_sha256,round_policy_json,grant_canonical,round_policy_canonical,api_readiness_canonical,created_at)
 VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9::jsonb,$10,$11,$12,$13)`, intent.Environment, intent.CompetitionId, intent.SeasonId, generation, intent.TargetRoundId, intent.TargetEpoch, string(content), digest, string(policy), content, []byte(policy), apiCanonical, value.CreatedAt))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_intake_pointer(environment,competition_id,season_id,generation) VALUES($1,$2,$3,$4)
 ON CONFLICT(environment,competition_id,season_id) DO UPDATE SET generation=excluded.generation`, intent.Environment, intent.CompetitionId, intent.SeasonId, generation))
			result = value
		}, server.OptReadWrite())
	})
	return result, resultErr
}

// SQL reads use the exact known grant, never the presence of an optional field
// in caller-provided JSON. A stale API process cannot submit under another pin.
func checkCompetitionIntakeFirstAdmission(ctx context.Context, reader competitionIntakeReader, settings *Settings, round *roundRecord) error {
	value := settings.intakeFirstActivation
	if value == nil {
		return nil
	}
	policy, err := policySnapshot(settings)
	if err != nil {
		return err
	}
	policyHash := sha256.Sum256(policy)
	if hex.EncodeToString(policyHash[:]) != value.RoundPolicySha256 || settings.AutomationPolicy.SourceActivationGeneration != value.Generation {
		return ErrConflict
	}
	stored, err := sourceIntakeActivation(ctx, reader, settings, value.Generation)
	if err != nil || *stored != *value || round == nil || round.RoundId != value.TargetRoundId || round.Epoch != value.TargetEpoch || !storedPolicyMatches(settings, round.PolicyJson) {
		return ErrConflict
	}
	var valid bool
	err = reader.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_source_intake_pointer p JOIN competition_source_activation a USING(environment,competition_id,season_id,generation)
 WHERE p.environment=$1 AND p.competition_id=$2 AND p.season_id=$3 AND p.generation=$4 AND a.abandoned_at IS NULL AND a.intent_json->>'intent_sha256'=$5)`,
		value.Environment, value.CompetitionId, value.SeasonId, value.Generation, value.IntentSha256).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}
