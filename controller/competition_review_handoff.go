// The native handoff owns one prospective model clock and an append-only call
// journal. It never elects, approves, publishes source, or releases a hold.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var ErrReviewCallAlreadyClaimed = errors.New("review call already claimed; retained outcome or explicit recovery required, never resample")

func reviewHandoffDatabaseTime(ctx context.Context, tx server.PgTx) (time.Time, error) {
	var now time.Time
	err := tx.QueryRow(ctx, `SELECT timezone('UTC',transaction_timestamp())`).Scan(&now)
	return now.UTC(), err
}

// The callback is bounded pure planning only. It must perform no filesystem,
// network, process, archive or provider operation while the transaction is open.
type CompetitionReviewHandoffPlanner func(model.CompetitionReviewHandoff) (model.CompetitionReviewHandoffPlan, error)

func reviewHandoffInTx(ctx context.Context, tx server.PgTx, id server.Id) (*model.CompetitionReviewHandoff, error) {
	var content []byte
	if err := tx.QueryRow(ctx, `SELECT owner_json FROM competition_review_handoff WHERE handoff_id=$1`, id).Scan(&content); err != nil {
		return nil, err
	}
	var owner model.CompetitionReviewHandoff
	if err := json.Unmarshal(content, &owner); err != nil {
		return nil, err
	}
	return &owner, nil
}

func reviewHandoffCompletionInTx(ctx context.Context, tx server.PgTx, id server.Id) (*model.CompetitionReviewHandoffCompletion, error) {
	var content []byte
	if err := tx.QueryRow(ctx, `SELECT completion_json FROM competition_review_handoff_completion WHERE handoff_id=$1`, id).Scan(&content); err != nil {
		return nil, err
	}
	var completion model.CompetitionReviewHandoffCompletion
	if err := json.Unmarshal(content, &completion); err != nil {
		return nil, err
	}
	return &completion, nil
}

// Rechecks the current election and all sealed artifact identities; a revised
// decision, evaluator incident or publication gap cannot reuse an old owner.
func validateReviewHandoffElection(current *model.CompetitionV2RoundResult, args model.CompetitionReviewHandoffArgs, now time.Time) error {
	if current == nil || len(current.Decisions) == 0 || current.Seal.RoundId != args.RoundId || current.Seal.SealSha256 != args.SealSha256 || current.Seal.BaselineSha256 != args.BaselineSha256 || now.Before(current.Seal.PublicationAt) {
		return ErrConflict
	}
	decision := current.Decisions[len(current.Decisions)-1]
	if decision.SelectedJobId == nil || *decision.SelectedJobId != args.JobId || decision.Revision != args.RewardRevision || decision.DecisionSha256 != args.DecisionSha256 {
		return ErrConflict
	}
	for _, entry := range current.Seal.Entries {
		if entry.JobId == args.JobId && entry.State == "succeeded" && entry.Eligible && entry.PatchSha256 == args.PatchSha256 && entry.BaseSourceLockSha256 == args.BaseSourceLockSha256 && entry.ScoreSha256 == args.ScoreSha256 && entry.ArtifactManifestSha256 == args.ArtifactManifestSha256 {
			return nil
		}
	}
	return ErrConflict
}

// Existing owners replay exactly, including expired clocks. A pre-existing
// legacy review checkpoint is never adopted, replaced, reset or extended.
func (self PostgresStore) BeginReviewHandoff(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, actor string, args model.CompetitionReviewHandoffArgs, planner CompetitionReviewHandoffPlanner) (owner *model.CompetitionReviewHandoff, err error) {
	if model.ValidateCompetitionReviewHandoffArgs(args) != nil || !tokenNamePattern.MatchString(actor) || planner == nil {
		return nil, ErrConflict
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			owner = nil
			_, policy, loadErr := loadAutomationRoundInTx(ctx, tx, settings, args.RoundId)
			server.Raise(loadErr)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, args.RoundId))
			if policy.AutomationPolicy.WinnerReviewPolicy != model.CompetitionSnowWinnerReviewPolicy {
				panic(ErrConflict)
			}
			now, loadErr := reviewHandoffDatabaseTime(ctx, tx)
			server.Raise(loadErr)
			current := automationResultInTx(ctx, tx, args.RoundId)
			server.Raise(validateReviewHandoffElection(current, args, now))
			var existing server.Id
			loadErr = tx.QueryRow(ctx, `SELECT handoff_id FROM competition_review_handoff WHERE round_id=$1 AND decision_sha256=$2`, args.RoundId, args.DecisionSha256).Scan(&existing)
			if loadErr == nil {
				owner, loadErr = reviewHandoffInTx(ctx, tx, existing)
				server.Raise(loadErr)
				if owner.Args != args || owner.ActorId != actor {
					panic(ErrConflict)
				}
				plan, planErr := planner(*owner)
				server.Raise(planErr)
				wanted, _ := model.CompetitionAutomationDigest(owner.Plan)
				actual, _ := model.CompetitionAutomationDigest(plan)
				if wanted != actual {
					panic(ErrConflict)
				}
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			var transition model.CompetitionSupervisorTransition
			var previous []byte
			loadErr = tx.QueryRow(ctx, `SELECT transition_json FROM competition_supervisor_transition WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 ORDER BY revision DESC LIMIT 1`, current.Seal.Environment, current.Seal.CompetitionId, current.Seal.SeasonId, current.Seal.Epoch+1).Scan(&previous)
			if loadErr == nil {
				server.Raise(json.Unmarshal(previous, &transition))
			} else if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			if transition.WinnerReview != nil || transition.SourceActivationGeneration != 0 {
				panic(ErrConflict)
			}
			for _, review := range current.IntegrityReviews {
				if review.Request.JobId == args.JobId {
					panic(ErrConflict)
				}
			}
			owner = &model.CompetitionReviewHandoff{HandoffId: server.NewId(), ActorId: actor, Args: args, StartedAt: now, Deadline: now.Add(time.Duration(args.TimeoutSeconds) * time.Second)}
			owner.Plan, loadErr = planner(*owner)
			server.Raise(loadErr)
			server.Raise(model.ValidateCompetitionReviewHandoffPlan(owner.Plan))
			content, marshalErr := json.Marshal(owner)
			server.Raise(marshalErr)
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_review_handoff_actor',$1,true)`, actor))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_review_handoff(handoff_id,round_id,job_id,decision_sha256,owner_json,started_at,deadline) VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, owner.HandoffId, args.RoundId, args.JobId, args.DecisionSha256, string(content), now, owner.Deadline))
			checkpoint := owner.Checkpoint()
			transition.ExpectedRevision, transition.Revision = transition.Revision, transition.Revision+1
			transition.TargetEpoch, transition.State, transition.Reason, transition.UpdatedAt, transition.WinnerReview = current.Seal.Epoch+1, "winner_review_pending", "multipass_pending_explicit_operator_review", now, &checkpoint
			content, marshalErr = json.Marshal(transition)
			server.Raise(marshalErr)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_supervisor_transition(environment,competition_id,season_id,target_epoch,revision,state,transition_json,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`, current.Seal.Environment, current.Seal.CompetitionId, current.Seal.SeasonId, transition.TargetEpoch, transition.Revision, transition.State, string(content), now))
		})
	})
	return owner, err
}

// Read-only recovery exposes exact stored clocks and journal; it does not
// authorize repeating an inflight claim or starting a new attempt directory.
func (self PostgresStore) ReviewHandoff(ctx context.Context, settings *Settings, roundId, id server.Id) (owner *model.CompetitionReviewHandoff, calls []model.CompetitionReviewHandoffCall, completion *model.CompetitionReviewHandoffCompletion, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			owner, loadErr = reviewHandoffInTx(ctx, tx, id)
			server.Raise(loadErr)
			if owner.Args.RoundId != roundId {
				panic(ErrConflict)
			}
			calls = []model.CompetitionReviewHandoffCall{}
			rows, queryErr := tx.Query(ctx, `SELECT call_json FROM competition_review_handoff_call WHERE handoff_id=$1 ORDER BY event_cursor`, id)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var content []byte
					server.Raise(rows.Scan(&content))
					var call model.CompetitionReviewHandoffCall
					server.Raise(json.Unmarshal(content, &call))
					calls = append(calls, call)
				}
			})
			completion, loadErr = reviewHandoffCompletionInTx(ctx, tx, id)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				completion = nil
			} else {
				server.Raise(loadErr)
			}
		})
	})
	return
}

// No duplicate claim is returned as permission to dispatch. An outcome can be
// replayed byte-identically, including errors; late evidence is still retained.
func (self PostgresStore) RecordReviewHandoffCall(ctx context.Context, settings *Settings, roundId server.Id, actor string, value model.CompetitionReviewHandoffCall) (result *model.CompetitionReviewHandoffCall, err error) {
	if !tokenNamePattern.MatchString(actor) || value.RecordedAt != (time.Time{}) || !sha256Pattern.MatchString(value.RequestSha256) || (value.Kind != "claim" && value.Kind != "receipt") || len(value.Failure) > 2048 || value.Kind == "claim" && (value.EvidenceSha256 != "" || value.Failure != "") || value.Kind == "receipt" && !sha256Pattern.MatchString(value.EvidenceSha256) {
		return nil, ErrConflict
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			_, _, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			owner, loadErr := reviewHandoffInTx(ctx, tx, value.HandoffId)
			server.Raise(loadErr)
			if owner.ActorId != actor || owner.Args.RoundId != roundId || !slices.Contains(owner.Plan.CallIds, value.CallId) {
				panic(ErrConflict)
			}
			now, loadErr := reviewHandoffDatabaseTime(ctx, tx)
			server.Raise(loadErr)
			var content []byte
			loadErr = tx.QueryRow(ctx, `SELECT call_json FROM competition_review_handoff_call WHERE handoff_id=$1 AND call_id=$2 AND kind=$3`, value.HandoffId, value.CallId, value.Kind).Scan(&content)
			if loadErr == nil {
				if value.Kind == "claim" {
					panic(ErrReviewCallAlreadyClaimed)
				}
				var prior model.CompetitionReviewHandoffCall
				server.Raise(json.Unmarshal(content, &prior))
				value.RecordedAt = prior.RecordedAt
				if prior != value {
					panic(ErrConflict)
				}
				result = &prior
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			if value.Kind == "claim" {
				server.Raise(ensureAutomationRoundValidInTx(ctx, tx, roundId))
				server.Raise(validateReviewHandoffElection(automationResultInTx(ctx, tx, roundId), owner.Args, now))
				if !now.Before(owner.Deadline) {
					panic(ErrWinnerReviewTimedOut)
				}
				var completed bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_review_handoff_completion WHERE handoff_id=$1)`, value.HandoffId).Scan(&completed))
				if completed {
					panic(ErrConflict)
				}
			} else {
				var requestDigest string
				server.Raise(tx.QueryRow(ctx, `SELECT call_json->>'request_sha256' FROM competition_review_handoff_call WHERE handoff_id=$1 AND call_id=$2 AND kind='claim'`, value.HandoffId, value.CallId).Scan(&requestDigest))
				if requestDigest != value.RequestSha256 {
					panic(ErrConflict)
				}
			}
			value.RecordedAt = now
			content, loadErr = json.Marshal(value)
			server.Raise(loadErr)
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_review_handoff_actor',$1,true)`, actor))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_review_handoff_call(handoff_id,call_id,kind,call_json,recorded_at) VALUES($1,$2,$3,$4::jsonb,$5)`, value.HandoffId, value.CallId, value.Kind, string(content), now))
			result = &value
		})
	})
	return
}

// Complete only after native evidence validation and protected retention.
// Failures remain durable without a proposal, including completion after expiry.
func (self PostgresStore) CompleteReviewHandoff(ctx context.Context, settings *Settings, roundId server.Id, actor string, value model.CompetitionReviewHandoffCompletion) (result *model.CompetitionReviewHandoffCompletion, err error) {
	if !tokenNamePattern.MatchString(actor) || !value.CompletedAt.IsZero() || value.Evidence.Key == "" || value.Evidence.VersionId == "" || !sha256Pattern.MatchString(value.Evidence.Sha256) || !sha256Pattern.MatchString(value.PlanSha256) || value.Evidence.RetainUntil.IsZero() || len(value.Failure) > 2048 {
		return nil, ErrConflict
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			_, policy, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			owner, loadErr := reviewHandoffInTx(ctx, tx, value.HandoffId)
			server.Raise(loadErr)
			if owner.ActorId != actor || owner.Args.RoundId != roundId || owner.Plan.PlanSha256 != value.PlanSha256 {
				panic(ErrConflict)
			}
			existing, loadErr := reviewHandoffCompletionInTx(ctx, tx, value.HandoffId)
			if loadErr == nil {
				value.CompletedAt = existing.CompletedAt
				wanted, _ := model.CompetitionAutomationDigest(existing)
				actual, _ := model.CompetitionAutomationDigest(value)
				if wanted != actual {
					panic(ErrConflict)
				}
				result = existing
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			now, loadErr := reviewHandoffDatabaseTime(ctx, tx)
			server.Raise(loadErr)
			if !value.Evidence.RetainUntil.After(now) {
				panic(ErrConflict)
			}
			if value.Proposal != nil {
				server.Raise(ensureAutomationRoundValidInTx(ctx, tx, roundId))
				server.Raise(validateReviewHandoffElection(automationResultInTx(ctx, tx, roundId), owner.Args, now))
				if value.Failure != "" || !now.Before(owner.Deadline) || value.Proposal.HandoffId != value.HandoffId.String() || value.Proposal.PrivateEvidenceSha256 != value.Evidence.Sha256 {
					panic(ErrConflict)
				}
				server.Raise(validateReviewHandoffProposal(owner, value.Proposal, *policy.AutomationPolicy))
				digest, digestErr := model.CompetitionAutomationDigest(value.Proposal)
				server.Raise(digestErr)
				if digest != value.ProposalSha256 {
					panic(ErrConflict)
				}
				var receipts, failures int
				server.Raise(tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE COALESCE(call_json->>'failure','')<>'' OR recorded_at>=$2) FROM competition_review_handoff_call WHERE handoff_id=$1 AND kind='receipt'`, value.HandoffId, owner.Deadline).Scan(&receipts, &failures))
				if receipts != len(owner.Plan.CallIds) || failures != 0 {
					panic(ErrConflict)
				}
			} else if value.ProposalSha256 != "" || value.Failure == "" {
				panic(ErrConflict)
			}
			value.CompletedAt = now
			content, marshalErr := json.Marshal(value)
			server.Raise(marshalErr)
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_review_handoff_actor',$1,true)`, actor))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_review_handoff_completion(handoff_id,completion_json,completed_at) VALUES($1,$2::jsonb,$3)`, value.HandoffId, string(content), now))
			result = &value
		})
	})
	return
}

func validateReviewHandoffProposal(owner *model.CompetitionReviewHandoff, request *model.CompetitionIntegrityReviewRequest, policy model.CompetitionAutomationPolicy) error {
	args := owner.Args
	if request == nil || request.HandoffId != owner.HandoffId.String() || request.JobId != args.JobId || request.ExpectedRevision != args.RewardRevision || request.ExpectedDecisionSha256 != args.DecisionSha256 || request.PatchSha256 != args.PatchSha256 || request.BaseSourceLockSha256 != args.BaseSourceLockSha256 || request.ScoreSha256 != args.ScoreSha256 || request.ArtifactManifestSha256 != args.ArtifactManifestSha256 || request.BaselineSha256 != args.BaselineSha256 || request.SupersedesReviewId != nil || request.AppealId != "" || request.ExpectedReplacementJobId != nil || request.RewardDisposition == "restored" {
		return ErrConflict
	}
	return model.ValidateCompetitionIntegrityReview(*request, policy)
}

// Only this capability may turn a pending proposal into an integrity record.
// Reading the proposal is harmless; the insertion rechecks everything under lock.
func (self PostgresStore) ImportReviewHandoff(ctx context.Context, settings *Settings, actor string, request model.CompetitionReviewHandoffImport) (*model.CompetitionIntegrityReviewResult, error) {
	if model.ValidateCompetitionReviewHandoffImport(request) != nil {
		return nil, ErrConflict
	}
	_, _, completion, err := self.ReviewHandoff(ctx, settings, request.RoundId, request.HandoffId)
	if err != nil {
		return nil, err
	}
	if completion == nil || completion.Proposal == nil {
		return nil, ErrReviewNotReady
	}
	return self.recordIntegrityReview(ctx, settings, request.RoundId, actor, *completion.Proposal, &request)
}

func validateReviewHandoffImportInTx(ctx context.Context, tx server.PgTx, roundId server.Id, request model.CompetitionIntegrityReviewRequest, expected model.CompetitionReviewHandoffImport) (*time.Time, error) {
	if model.ValidateCompetitionReviewHandoffImport(expected) != nil || expected.RoundId != roundId || expected.HandoffId.String() != request.HandoffId || expected.DecisionSha256 != request.ExpectedDecisionSha256 {
		return nil, ErrConflict
	}
	owner, err := reviewHandoffInTx(ctx, tx, expected.HandoffId)
	if err != nil {
		return nil, err
	}
	completion, err := reviewHandoffCompletionInTx(ctx, tx, expected.HandoffId)
	if err != nil {
		return nil, err
	}
	digest, err := model.CompetitionAutomationDigest(request)
	if err != nil || owner.Args.RoundId != roundId || completion.Proposal == nil || completion.Failure != "" || completion.ProposalSha256 != digest || expected.ProposalSha256 != digest || expected.EvidenceSha256 != completion.Evidence.Sha256 || request.PrivateEvidenceSha256 != completion.Evidence.Sha256 || completion.CompletedAt.Before(owner.StartedAt) || !completion.CompletedAt.Before(owner.Deadline) {
		return nil, ErrConflict
	}
	return &completion.CompletedAt, nil
}
