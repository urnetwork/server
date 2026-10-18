// Durable v2 transitions serialize on the round and never perform external effects in a transaction.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

var (
	ErrAutomationDisabled      = errors.New("competition automation is disabled")
	ErrUnsupportedDecisionMode = errors.New("competition requires the explicit v2 decision capability")
	ErrAutomationNotPublished  = errors.New("competition v2 evaluation has not reached its publication boundary")
	ErrAutomationConflict      = ErrConflict
)

// Mode belongs to the frozen round, not the current process toggle.
func roundUsesAutomation(round *roundRecord) bool {
	if round == nil {
		return false
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	return err == nil && policy.AutomationPolicy != nil && policy.AutomationPolicy.Enabled
}

// Reads and locks the existing round before any v2 transition; acquisition stays outermost.
func loadAutomationRoundInTx(ctx context.Context, tx server.PgTx, settings *Settings, roundId server.Id) (*roundRecord, *roundPolicySnapshot, error) {
	automationScopeLockInTx(ctx, tx, settings)
	round, err := scanRound(tx.QueryRow(ctx, `SELECT round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,
	 seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled,
	 finalized_at,winner_job_id,admission_closed_at,automation_sealed_at,automation_publication_at
	 FROM competition_round WHERE round_id=$1 AND competition_id=$2 FOR UPDATE`, roundId, settings.CompetitionId))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	policy, err := decodeRoundPolicySnapshot(round.PolicyJson)
	if err != nil {
		return nil, nil, err
	}
	if policy.AutomationPolicy == nil || !policy.AutomationPolicy.Enabled {
		return nil, nil, ErrUnsupportedDecisionMode
	}
	if policy.AutomationPolicy.Environment != settings.AutomationPolicy.Environment || policy.AutomationPolicy.SeasonId != settings.AutomationPolicy.SeasonId ||
		policy.AutomationPolicy.Staging != round.Staging || round.Staging != settings.AutomationPolicy.Staging {
		return nil, nil, ErrConflict
	}
	return round, policy, nil
}

// Persists a decision and its one durable event atomically; delivery is independently replayable.
func appendAutomationDecisionInTx(ctx context.Context, tx server.PgTx, decision *model.CompetitionRewardDecision, publicationAt time.Time) error {
	decision.DecisionSha256 = ""
	digest, err := model.CompetitionAutomationDigest(decision)
	if err != nil {
		return err
	}
	decision.DecisionSha256 = digest
	content, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	payloadDigest, err := model.CompetitionAutomationDigest(decision)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO competition_reward_decision(round_id,revision,decision_sha256,previous_decision_sha256,decision_json,created_at)
	 VALUES($1,$2,$3,$4,$5::jsonb,$6)`, decision.RoundId, decision.Revision, decision.DecisionSha256, decision.PreviousDecisionSha256, string(content), decision.DecidedAt)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO competition_reward_outbox(event_id,round_id,revision,payload_sha256,payload_json,publication_at,created_at)
	 VALUES($1,$2,$3,$4,$5::jsonb,$6,$7)`, server.NewId(), decision.RoundId, decision.Revision, payloadDigest, string(content), publicationAt, decision.DecidedAt)
	return err
}

// Constructs a revision from the immutable selector without deriving any unspecified burn formula.
func automationDecision(seal *model.CompetitionEvaluationSeal, policy model.CompetitionAutomationPolicy, previous *model.CompetitionRewardDecision, reviews []model.CompetitionIntegrityReviewResult, now time.Time) model.CompetitionRewardDecision {
	disqualifiedJobIds := map[server.Id]bool{}
	for _, review := range reviews {
		switch review.Request.RewardDisposition {
		case "disqualified":
			disqualifiedJobIds[review.Request.JobId] = true
		case "approved", "restored":
			delete(disqualifiedJobIds, review.Request.JobId)
		}
	}
	selected, reason := model.SelectCompetitionReward(seal.Entries, disqualifiedJobIds)
	decision := model.CompetitionRewardDecision{Staging: seal.Staging, RoundId: seal.RoundId, Environment: seal.Environment, CompetitionId: seal.CompetitionId, SeasonId: seal.SeasonId,
		Revision: 1, SealSha256: seal.SealSha256, Action: "elect", ReasonCode: reason, DecidedAt: now,
		SettlementPolicyVersion: policy.SettlementPolicyVersion, BurnPolicyVersion: policy.BurnPolicyVersion, SettlementMode: policy.SettlementMode}
	if previous != nil {
		decision.Revision = previous.Revision + 1
		decision.PreviousDecisionSha256 = previous.DecisionSha256
		decision.SupersededRevision = previous.Revision
		decision.SupersededJobId = previous.SelectedJobId
		decision.Action = "replace"
	}
	if selected != nil {
		jobId := selected.JobId
		decision.SelectedJobId = &jobId
		// This records the original completion timestamp, not a computed entitlement or payout time.
		anchor := selected.CompletedAt
		decision.EffectiveSettlementAnchor = &anchor
	} else {
		decision.Action = "no_winner"
	}
	return decision
}

// Seals only complete authoritative inventories. Replays return the original seal and revision.
func (self PostgresStore) SealAndElect(ctx context.Context, settings *Settings, roundId server.Id) (result *model.CompetitionV2RoundResult, err error) {
	if !settings.AutomationPolicy.Enabled {
		return nil, ErrAutomationDisabled
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			round, policy, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			var existing []byte
			sealErr := tx.QueryRow(ctx, `SELECT seal_json FROM competition_evaluation_seal WHERE round_id=$1`, roundId).Scan(&existing)
			if sealErr == nil {
				result = automationResultInTx(ctx, tx, roundId)
				return
			}
			if !errors.Is(sealErr, pgx.ErrNoRows) {
				server.Raise(sealErr)
			}
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, roundId))
			now := self.nowUtc()
			if round.Canceled || now.Before(round.ClosesAt) {
				panic(ErrReviewNotReady)
			}
			var active int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM competition_job WHERE round_id=$1 AND state IN ('queued','running')`, roundId).Scan(&active))
			if active != 0 {
				panic(ErrReviewNotReady)
			}
			seal := model.CompetitionEvaluationSeal{Staging: round.Staging, RoundId: roundId, Environment: policy.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: policy.AutomationPolicy.SeasonId,
				Epoch: round.Epoch, SealedAt: now, PublicationAt: now, TerminalCounts: map[string]int{}, Entries: []model.CompetitionSealedEntry{}}
			if now.Before(round.RevealAt) {
				seal.PublicationAt = round.RevealAt
			}
			seal.SourceChoiceAt = now.Add(time.Duration(policy.AutomationPolicy.SourceChoiceDelaySeconds) * time.Second)
			seal.PolicySha256, loadErr = model.CompetitionAutomationDigest(policy)
			server.Raise(loadErr)
			baselineRaw := 0.0
			baseline, baselineErr := scanRoundBaseline(tx.QueryRow(ctx, `SELECT round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,
		 base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at FROM competition_round_baseline WHERE round_id=$1`, roundId))
			seal.TakeoverMargin = policy.EvaluationPolicy.TakeoverMargin
			if baselineErr == nil {
				frozenSettings := *settings
				frozenSettings.BaseSha = policy.BaseSha
				frozenSettings.EvaluatorImageDigest = policy.EvaluatorImageDigest
				frozenSettings.EvaluationPolicy = policy.EvaluationPolicy
				server.Raise(validateRoundBaselineRecord(&frozenSettings, round, baseline))
				seal.BaselineSha256 = baseline.Sha256
				var manifest roundBaselineManifest
				server.Raise(json.Unmarshal(baseline.Json, &manifest))
				values := make([]float64, 0, len(manifest.Replicates))
				for _, replicate := range manifest.Replicates {
					values = append(values, replicate.RawScore)
				}
				sort.Float64s(values)
				if len(values) != 9 {
					panic(errors.New("v2 baseline must contain nine immutable replicates"))
				}
				baselineRaw = values[4]
				seal.BaselineRawScore = baselineRaw
			} else if !errors.Is(baselineErr, pgx.ErrNoRows) {
				server.Raise(baselineErr)
			}
			rows, queryErr := tx.Query(ctx, `SELECT job_id,patch_sha256,state,submitted_at,started_at,completed_at,score_json,COALESCE(artifact_manifest_sha256,''),artifact_manifest_json FROM competition_job WHERE round_id=$1 ORDER BY submitted_at,job_id`, roundId)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					entry := model.CompetitionSealedEntry{BaseSourceLockSha256: policy.AutomationPolicy.SourceLockSha256}
					var scoreJson, manifestJson []byte
					server.Raise(rows.Scan(&entry.JobId, &entry.PatchSha256, &entry.State, &entry.SubmittedAt, &entry.StartedAt, &entry.CompletedAt, &scoreJson, &entry.ArtifactManifestSha256, &manifestJson))
					if entry.State == "succeeded" {
						if baseline == nil || !sha256Pattern.MatchString(entry.ArtifactManifestSha256) {
							panic(errors.New("successful v2 entry lacks authenticated baseline or archive"))
						}
						entry.Score = &ScoreResult{}
						server.Raise(json.Unmarshal(scoreJson, entry.Score))
						var manifest artifactManifest
						server.Raise(json.Unmarshal(manifestJson, &manifest))
						for _, artifact := range manifest.Artifacts {
							if artifact.Path == "score.json" {
								entry.ScoreSha256 = artifact.Sha256
							}
						}
						if !sha256Pattern.MatchString(entry.ScoreSha256) {
							panic(errors.New("sealed score artifact lacks exact archived byte digest"))
						}
					}
					seal.TerminalCounts[entry.State]++
					seal.Entries = append(seal.Entries, entry)
				}
			})
			seal.InventorySha256, loadErr = model.CompetitionAutomationDigest(seal.Entries)
			server.Raise(loadErr)
			excludedPatchKVs := map[string]bool{}
			if policy.AutomationPolicy.RepeatRewardPolicy == "exclude_exact_identity" {
				rows, queryErr = tx.Query(ctx, `SELECT DISTINCT j.patch_sha256 FROM competition_reward_decision d JOIN competition_job j ON j.job_id=(d.decision_json->>'selected_job_id')::uuid
			 JOIN competition_round r ON r.round_id=d.round_id WHERE r.competition_id=$1 AND r.policy_json->'automation_policy'->>'environment'=$2
			 AND r.policy_json->'automation_policy'->>'season_id'=$3 AND r.policy_json->'automation_policy'->>'source_lock_sha256'=$4`, settings.CompetitionId, policy.AutomationPolicy.Environment, policy.AutomationPolicy.SeasonId, policy.AutomationPolicy.SourceLockSha256)
				server.WithPgResult(rows, queryErr, func() {
					for rows.Next() {
						var digest string
						server.Raise(rows.Scan(&digest))
						excludedPatchKVs[digest] = true
					}
				})
			}
			seal.Entries, loadErr = model.RankCompetitionSealedEntriesForMode(seal.Entries, baselineRaw, policy.EvaluationPolicy.TakeoverMargin, excludedPatchKVs, round.Staging)
			server.Raise(loadErr)
			seal.RankingSha256, loadErr = model.CompetitionAutomationDigest(seal.Entries)
			server.Raise(loadErr)
			seal.SealSha256, loadErr = model.CompetitionAutomationDigest(seal)
			server.Raise(loadErr)
			content, marshalErr := json.Marshal(seal)
			server.Raise(marshalErr)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_evaluation_seal(round_id,sealed_at,publication_at,seal_sha256,seal_json) VALUES($1,$2,$3,$4,$5::jsonb)`, roundId, seal.SealedAt, seal.PublicationAt, seal.SealSha256, string(content)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_round SET automation_sealed_at=$2,automation_publication_at=$3 WHERE round_id=$1`, roundId, seal.SealedAt, seal.PublicationAt))
			decision := automationDecision(&seal, *policy.AutomationPolicy, nil, nil, now)
			server.Raise(appendAutomationDecisionInTx(ctx, tx, &decision, seal.PublicationAt))
			result = automationResultInTx(ctx, tx, roundId)
		})
	})
	return result, err
}

// Loads all immutable records on the caller's transaction; private callers enforce their own scope.
func automationResultInTx(ctx context.Context, tx server.PgTx, roundId server.Id) *model.CompetitionV2RoundResult {
	result := &model.CompetitionV2RoundResult{DecisionMode: model.CompetitionDecisionModeV2, RequiredApiCapability: model.CompetitionAutomationCapability,
		Decisions: []model.CompetitionRewardDecision{}, IntegrityReviews: []model.CompetitionIntegrityReviewResult{}, SettlementReceipts: []model.CompetitionSettlementReceipt{}, SettlementStatus: "pending"}
	var content []byte
	result.Incidents = automationIncidentsInTx(ctx, tx, roundId)
	for _, incident := range result.Incidents {
		if incident.Request.InvalidatesEvaluation {
			result.Invalidated = true
		}
	}
	server.Raise(tx.QueryRow(ctx, `SELECT seal_json FROM competition_evaluation_seal WHERE round_id=$1`, roundId).Scan(&content))
	server.Raise(json.Unmarshal(content, &result.Seal))
	result.Staging = result.Seal.Staging
	rows, err := tx.Query(ctx, `SELECT decision_json FROM competition_reward_decision WHERE round_id=$1 ORDER BY revision`, roundId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var decision model.CompetitionRewardDecision
			server.Raise(rows.Scan(&content))
			server.Raise(json.Unmarshal(content, &decision))
			result.Decisions = append(result.Decisions, decision)
		}
	})
	if len(result.Decisions) > 0 {
		current := result.Decisions[len(result.Decisions)-1]
		result.RewardRevision = current.Revision
		result.ProvisionalRewardWinner = current.SelectedJobId
	}
	rows, err = tx.Query(ctx, `SELECT review_json FROM competition_integrity_review WHERE round_id=$1 ORDER BY review_cursor`, roundId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var review model.CompetitionIntegrityReviewResult
			server.Raise(rows.Scan(&content))
			server.Raise(json.Unmarshal(content, &review))
			result.IntegrityReviews = append(result.IntegrityReviews, review)
		}
	})
	rows, err = tx.Query(ctx, `SELECT receipt_json FROM competition_settlement_receipt r JOIN competition_reward_outbox e USING(event_id) WHERE e.round_id=$1 ORDER BY r.created_at,e.event_cursor,r.status`, roundId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var receipt model.CompetitionSettlementReceipt
			server.Raise(rows.Scan(&content))
			server.Raise(json.Unmarshal(content, &receipt))
			result.SettlementReceipts = append(result.SettlementReceipts, receipt)
			if receipt.Revision == result.RewardRevision {
				precedence := map[string]int{"pending": 0, "received": 1, "rejected": 2, "applied": 3, "confirmed": 4}
				if precedence[result.SettlementStatus] < precedence[receipt.Status] {
					result.SettlementStatus = receipt.Status
				}
			}
		}
	})
	var sourceContent []byte
	var activatedAt *time.Time
	sourceErr := tx.QueryRow(ctx, `SELECT intent_json,activated_at FROM competition_source_activation
	 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 AND abandoned_at IS NULL ORDER BY generation DESC LIMIT 1`,
		result.Seal.Environment, result.Seal.CompetitionId, result.Seal.SeasonId, result.Seal.Epoch+1).Scan(&sourceContent, &activatedAt)
	if sourceErr == nil {
		result.SourceDecision = &model.CompetitionSourceActivation{}
		server.Raise(json.Unmarshal(sourceContent, result.SourceDecision))
		result.SourceDecision.ActivatedAt = activatedAt
		if activatedAt != nil {
			result.SourceDecision.State = "active"
		}
	} else if !errors.Is(sourceErr, pgx.ErrNoRows) {
		server.Raise(sourceErr)
	}
	return result
}

// Public reads require the immutable publication boundary, independently of new-automation enablement.
func (self PostgresStore) AutomationResult(ctx context.Context, settings *Settings, roundId server.Id) (result *model.CompetitionV2RoundResult, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			_, _, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			var publicationAt time.Time
			loadErr = tx.QueryRow(ctx, `SELECT publication_at FROM competition_evaluation_seal WHERE round_id=$1`, roundId).Scan(&publicationAt)
			if errors.Is(loadErr, pgx.ErrNoRows) || self.nowUtc().Before(publicationAt) {
				panic(ErrAutomationNotPublished)
			}
			server.Raise(loadErr)
			result = automationResultInTx(ctx, tx, roundId)
		})
	})
	return result, err
}

// Private supervisor reconciliation can inspect a seal without publishing it.
func (self PostgresStore) AutomationSeal(ctx context.Context, settings *Settings, roundId server.Id) (seal *model.CompetitionEvaluationSeal, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			seal = nil
			_, _, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			var content []byte
			loadErr = tx.QueryRow(ctx, `SELECT seal_json FROM competition_evaluation_seal WHERE round_id=$1`, roundId).Scan(&content)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				panic(ErrNotFound)
			}
			server.Raise(loadErr)
			seal = &model.CompetitionEvaluationSeal{}
			server.Raise(json.Unmarshal(content, seal))
		})
	})
	return seal, err
}

// A review replay returns the original response. A stale expected revision never creates a second replacement.
func (self PostgresStore) RecordIntegrityReview(ctx context.Context, settings *Settings, roundId server.Id, actorId string, request model.CompetitionIntegrityReviewRequest) (result *model.CompetitionIntegrityReviewResult, err error) {
	if !tokenNamePattern.MatchString(actorId) {
		return nil, errors.New("authenticated reviewer identity is required")
	}
	requestDigest, err := model.CompetitionAutomationDigest(request)
	if err != nil {
		return nil, err
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			_, policy, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_v2_review_actor',$1,true)`, actorId))
			server.Raise(model.ValidateCompetitionIntegrityReview(request, *policy.AutomationPolicy))
			var storedDigest string
			var content []byte
			loadErr = tx.QueryRow(ctx, `SELECT request_sha256,review_json FROM competition_integrity_review WHERE round_id=$1 AND actor_id=$2 AND idempotency_key=$3`, roundId, actorId, request.IdempotencyKey).Scan(&storedDigest, &content)
			if loadErr == nil {
				if storedDigest != requestDigest {
					panic(ErrConflict)
				}
				result = &model.CompetitionIntegrityReviewResult{}
				server.Raise(json.Unmarshal(content, result))
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, roundId))
			current := automationResultInTx(ctx, tx, roundId)
			if current.RewardRevision != request.ExpectedRevision {
				panic(ErrConflict)
			}
			var entry *model.CompetitionSealedEntry
			for i := range current.Seal.Entries {
				if current.Seal.Entries[i].JobId == request.JobId {
					entry = &current.Seal.Entries[i]
					break
				}
			}
			if entry == nil || entry.State != "succeeded" || entry.PatchSha256 != request.PatchSha256 || entry.BaseSourceLockSha256 != request.BaseSourceLockSha256 || entry.ScoreSha256 != request.ScoreSha256 || entry.ArtifactManifestSha256 != request.ArtifactManifestSha256 || current.Seal.BaselineSha256 != request.BaselineSha256 {
				panic(ErrConflict)
			}
			var latest *model.CompetitionIntegrityReviewResult
			for i := range current.IntegrityReviews {
				review := &current.IntegrityReviews[i]
				if review.Request.JobId == request.JobId && review.Request.RewardDisposition != "unchanged" {
					latest = review
				}
			}
			if request.RewardDisposition == "restored" && (latest == nil || latest.ReviewId != *request.SupersedesReviewId || latest.Request.RewardDisposition != "disqualified") {
				panic(ErrConflict)
			}
			if latest != nil && latest.Request.RewardDisposition == "disqualified" && request.RewardDisposition == "approved" {
				panic(ErrConflict)
			}
			now := self.nowUtc()
			result = &model.CompetitionIntegrityReviewResult{ReviewId: server.NewId(), RoundId: roundId, ActorId: actorId, CreatedAt: now, Request: request, RequestSha256: requestDigest}
			previous := current.Decisions[len(current.Decisions)-1]
			next := automationDecision(&current.Seal, *policy.AutomationPolicy, &previous, append(current.IntegrityReviews, *result), now)
			changed := (next.SelectedJobId == nil) != (previous.SelectedJobId == nil) || next.SelectedJobId != nil && *next.SelectedJobId != *previous.SelectedJobId
			if request.ExpectedReplacementJobId != nil && (next.SelectedJobId == nil || *next.SelectedJobId != *request.ExpectedReplacementJobId) {
				panic(ErrConflict)
			}
			if changed {
				next.ReviewId = &result.ReviewId
				result.RewardDecision = next
			} else {
				result.RewardDecision = previous
			}
			// Insert the authentic review before the database computes the next eligible recipient.
			if changed {
				digestErr := error(nil)
				result.RewardDecision.DecisionSha256, digestErr = model.CompetitionAutomationDigest(result.RewardDecision)
				server.Raise(digestErr)
			}
			content, loadErr = json.Marshal(result)
			server.Raise(loadErr)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_integrity_review(review_id,round_id,job_id,actor_id,idempotency_key,request_sha256,review_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`, result.ReviewId, roundId, request.JobId, actorId, request.IdempotencyKey, requestDigest, string(content), now))
			if changed {
				server.Raise(appendAutomationDecisionInTx(ctx, tx, &result.RewardDecision, current.Seal.PublicationAt))
			}
			server.Raise(self.recordAutomationSourceRejectionInTx(ctx, tx, settings, result))
		})
	})
	return result, err
}

// A future head publication blocks the feed rather than losing a skipped revision forever.
func (self PostgresStore) RewardEvents(ctx context.Context, settings *Settings, afterCursor int64, limit int) (page *model.CompetitionRewardEventPage, err error) {
	if afterCursor < 0 || limit < 1 || limit > 200 {
		return nil, errors.New("invalid event cursor or limit")
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(conn server.PgTx) {
			automationScopeLockInTx(ctx, conn, settings)
			server.Raise(ensureAutomationNotHeldInTx(ctx, conn, settings, "reward_delivery"))
			page = &model.CompetitionRewardEventPage{Events: []model.CompetitionRewardEvent{}, NextCursor: afterCursor}
			rows, queryErr := conn.Query(ctx, `SELECT event_cursor,event_id,payload_sha256,publication_at,payload_json FROM competition_reward_outbox
	 WHERE payload_json->>'competition_id'=$1 AND payload_json->>'environment'=$2 AND payload_json->>'season_id'=$3 AND event_cursor>$4
	 AND COALESCE((payload_json->>'staging')::boolean,false)=$6
	 ORDER BY event_cursor LIMIT $5`, settings.CompetitionId, settings.AutomationPolicy.Environment, settings.AutomationPolicy.SeasonId, afterCursor, limit, settings.AutomationPolicy.Staging)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					var event model.CompetitionRewardEvent
					var content []byte
					server.Raise(rows.Scan(&event.Cursor, &event.EventId, &event.PayloadSha256, &event.PublicationAt, &content))
					if self.nowUtc().Before(event.PublicationAt) {
						break
					}
					server.Raise(json.Unmarshal(content, &event.Decision))
					page.Events = append(page.Events, event)
					page.NextCursor = event.Cursor
				}
			})
		})
	})
	return page, err
}

// Receipt stages append independently. Reusing the same event/stage with different accounting conflicts.
func (self PostgresStore) RecordSettlementReceipt(ctx context.Context, settings *Settings, actorId string, receipt model.CompetitionSettlementReceipt) (result *model.CompetitionSettlementReceipt, err error) {
	if !tokenNamePattern.MatchString(actorId) || receipt.Environment != settings.AutomationPolicy.Environment || receipt.CompetitionId != settings.CompetitionId || receipt.SeasonId != settings.AutomationPolicy.SeasonId || !sha256Pattern.MatchString(receipt.PayloadSha256) || !sha256Pattern.MatchString(receipt.DecisionSha256) || len(receipt.AccountingReference) > 1024 || len(receipt.ChainTransaction) > 256 {
		return nil, ErrConflict
	}
	if receipt.Status != "received" && receipt.Status != "applied" && receipt.Status != "confirmed" && receipt.Status != "rejected" {
		return nil, errors.New("invalid settlement receipt status")
	}
	if (receipt.Status == "applied" || receipt.Status == "confirmed") && (receipt.AppliedAt == nil || receipt.AccountingReference == "") {
		return nil, errors.New("application requires an explicit accounting receipt")
	}
	receipt.ActorId = actorId
	receipt.ReceivedAt = time.Time{}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			receipt.ReceivedAt = time.Time{}
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_v2_settlement_actor',$1,true)`, actorId))
			var content []byte
			var digest string
			var publicationAt time.Time
			server.Raise(tx.QueryRow(ctx, `SELECT payload_sha256,payload_json,publication_at FROM competition_reward_outbox WHERE event_id=$1 FOR UPDATE`, receipt.EventId).Scan(&digest, &content, &publicationAt))
			var decision model.CompetitionRewardDecision
			server.Raise(json.Unmarshal(content, &decision))
			if decision.Staging != settings.AutomationPolicy.Staging || (decision.Staging && (decision.SettlementMode != "shadow" || receipt.ChainTransaction != "" || receipt.AppliedAt != nil || (receipt.Status != "received" && receipt.Status != "rejected"))) {
				panic(ErrConflict)
			}
			if self.nowUtc().Before(publicationAt) || digest != receipt.PayloadSha256 || decision.DecisionSha256 != receipt.DecisionSha256 || decision.RoundId != receipt.RoundId || decision.Revision != receipt.Revision || decision.Environment != receipt.Environment || decision.CompetitionId != receipt.CompetitionId || decision.SeasonId != receipt.SeasonId {
				panic(ErrConflict)
			}
			loadErr := tx.QueryRow(ctx, `SELECT receipt_json FROM competition_settlement_receipt WHERE event_id=$1 AND actor_id=$2 AND status=$3`, receipt.EventId, actorId, receipt.Status).Scan(&content)
			if loadErr == nil {
				var existing model.CompetitionSettlementReceipt
				server.Raise(json.Unmarshal(content, &existing))
				savedAt := existing.ReceivedAt
				existing.ReceivedAt = time.Time{}
				expected, _ := model.CompetitionAutomationDigest(receipt)
				actual, _ := model.CompetitionAutomationDigest(existing)
				if expected != actual {
					panic(ErrConflict)
				}
				existing.ReceivedAt = savedAt
				result = &existing
				return
			}
			if !errors.Is(loadErr, pgx.ErrNoRows) {
				server.Raise(loadErr)
			}
			receipt.ReceivedAt = self.nowUtc()
			content, loadErr = json.Marshal(receipt)
			server.Raise(loadErr)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_settlement_receipt(event_id,actor_id,status,payload_sha256,receipt_json,created_at) VALUES($1,$2,$3,$4,$5::jsonb,$6)`, receipt.EventId, actorId, receipt.Status, receipt.PayloadSha256, string(content), receipt.ReceivedAt))
			copyReceipt := receipt
			result = &copyReceipt
		})
	})
	return result, err
}

// Exposes a clear mode fence to legacy review and source-promotion tooling.
func requireLegacyCompetitionRound(round *roundRecord) error {
	if roundUsesAutomation(round) {
		return fmt.Errorf("%w: legacy review and promotion commands cannot mutate this round", ErrUnsupportedDecisionMode)
	}
	return nil
}

// Checks exact previously rewarded identities before admission and before the adapter charges a fee.
func (self PostgresStore) ScreenAutomationSubmission(ctx context.Context, settings *Settings, roundId server.Id, patch *CanonicalPatch) (eligible bool, reason string, err error) {
	if !settings.AutomationPolicy.Enabled {
		return false, "", ErrAutomationDisabled
	}
	if patch == nil {
		return false, "", ErrConflict
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			eligible = false
			reason = ""
			round, policy, loadErr := loadAutomationRoundInTx(ctx, tx, settings, roundId)
			server.Raise(loadErr)
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "admission"))
			server.Raise(ensureAutomationRoundValidInTx(ctx, tx, roundId))
			canonical, patchErr := ValidateAndCanonicalizePatch(string(patch.Bytes), policy.PatchPolicy)
			if patchErr != nil {
				panic(patchErr)
			}
			if canonical.Sha256 != patch.Sha256 {
				panic(ErrConflict)
			}
			if round.Canceled || round.AutomationSealedAt != nil || !submissionWithinEpoch(round, self.nowUtc()) {
				panic(ErrRoundClosed)
			}
			eligible = true
			if policy.AutomationPolicy.RepeatRewardPolicy == "exclude_exact_identity" {
				duplicate, checkErr := automationRewardDuplicateInTx(ctx, tx, settings, *policy.AutomationPolicy, patch.Sha256)
				server.Raise(checkErr)
				if duplicate {
					eligible = false
					reason = "reward_duplicate"
				}
			}
		})
	})
	return eligible, reason, err
}

// Reuses the caller's admission transaction so a new entitlement cannot race preflight.
func automationRewardDuplicateInTx(ctx context.Context, tx server.PgTx, settings *Settings, policy model.CompetitionAutomationPolicy, patchSha256 string) (bool, error) {
	var duplicate bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_reward_decision d JOIN competition_job j ON j.job_id=(d.decision_json->>'selected_job_id')::uuid
	 JOIN competition_round r ON r.round_id=d.round_id WHERE r.competition_id=$1 AND r.policy_json->'automation_policy'->>'environment'=$2 AND r.policy_json->'automation_policy'->>'season_id'=$3
	 AND r.policy_json->'automation_policy'->>'source_lock_sha256'=$4 AND j.patch_sha256=$5)`, settings.CompetitionId, policy.Environment, policy.SeasonId, policy.SourceLockSha256, patchSha256).Scan(&duplicate)
	return duplicate, err
}

// Creates exactly the activated target using a fenced lease; workload generation runs before its transaction.
func (self PostgresStore) CreateAutomationRound(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, intentGeneration int64, opensAt time.Time) (*RoundResult, error) {
	if !settings.AutomationPolicy.Enabled {
		return nil, ErrAutomationDisabled
	}
	intent, err := self.SourceActivation(ctx, settings, intentGeneration)
	if err != nil {
		return nil, err
	}
	if !sourceModeMatches(settings, intent) || intent.TargetRoundId == (server.Id{}) || intent.AbandonedAt != nil {
		return nil, ErrConflict
	}
	if intent.ActivatedAt == nil || (!intent.OpensAt.IsZero() && !intent.OpensAt.Equal(opensAt)) || opensAt.Before(intent.PreparedAt.Add(sourcePreparationInterval(intent))) || opensAt.Before(*intent.ActivatedAt) {
		return nil, ErrReviewNotReady
	}
	// A retry after a committed creation returns that exact round without preparing a replacement workload.
	current, err := self.CurrentAutomationRound(ctx, settings)
	if err != nil {
		return nil, err
	}
	if current != nil && current.Epoch == intent.TargetEpoch {
		policy, decodeErr := decodeRoundPolicySnapshot(current.PolicyJson)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if current.RoundId != intent.TargetRoundId || policy.AutomationPolicy == nil || policy.AutomationPolicy.SourceActivationGeneration != intentGeneration || !current.OpensAt.Equal(opensAt) {
			return nil, ErrConflict
		}
		return &current.RoundResult, nil
	}
	frozen := *settings
	frozen.automationLease = &lease
	frozen.automationTargetRoundId = intent.TargetRoundId
	frozen.AutomationPolicy.SourceActivationGeneration = intent.Generation
	frozen.AutomationPolicy.SourceGeneration = intent.SourceGeneration
	frozen.AutomationPolicy.SourceLockSha256 = intent.SourceLockSha256
	frozen.BaseSha = intent.Commits["server"]
	frozen.EvaluatorImageDigest = intent.EvaluatorImageDigest
	frozen.EvaluationPolicy.TakeoverMargin = intent.Threshold
	frozen.EvaluationPolicy.HostQualificationSha256 = intent.HostQualificationSha256
	window := 7 * 24 * time.Hour
	if intent.Staging {
		window = time.Duration(intent.StagingRoundWindowSeconds) * time.Second
	}
	args := GenerateRoundArgs{OpensAt: opensAt, ClosesAt: opensAt.Add(window), RevealAt: opensAt.Add(window)}
	var round *roundRecord
	if intent.Staging {
		round, err = self.createAutomationStagingRound(ctx, &frozen, args)
	} else {
		round, err = self.CreateRound(ctx, &frozen, args)
	}
	if err != nil {
		return nil, err
	}
	return &round.RoundResult, nil
}
