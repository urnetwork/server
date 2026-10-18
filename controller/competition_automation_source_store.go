// Fenced source intents and exact readiness receipts make activation a resumable transaction.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A nil query parameter preserves the historical unfenced v1 worker selection.
func optionalAutomationRoundId(settings *Settings) any {
	if settings.automationRoundId == (server.Id{}) {
		return nil
	}
	return settings.automationRoundId
}

// Every source and review mutation takes the same scope lock before individual round locks.
func automationScopeLockInTx(ctx context.Context, tx server.PgTx, settings *Settings) {
	server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "competition-automation:"+settings.AutomationPolicy.Environment+":"+settings.CompetitionId+":"+settings.AutomationPolicy.SeasonId))
}

// A lease takeover monotonically fences all work started by the former owner.
func (self PostgresStore) AcquireAutomationLease(ctx context.Context, settings *Settings, ownerId string, ttl time.Duration) (lease *model.CompetitionSupervisorLease, err error) {
	if !settings.AutomationPolicy.Enabled {
		return nil, ErrAutomationDisabled
	}
	if !tokenNamePattern.MatchString(ownerId) || ttl < 10*time.Second || ttl > 5*time.Minute {
		return nil, errors.New("invalid supervisor owner or lease duration")
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			lease = nil
			automationScopeLockInTx(ctx, tx, settings)
			now := self.nowUtc()
			var owner string
			var token int64
			var expires time.Time
			loadErr := tx.QueryRow(ctx, `SELECT owner_id,fencing_token,lease_expires_at FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 FOR UPDATE`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId).Scan(&owner, &token, &expires)
			if loadErr == nil && now.Before(expires) {
				if owner != ownerId {
					panic(ErrLeaseLost)
				}
			} else if loadErr == nil || errors.Is(loadErr, pgx.ErrNoRows) {
				token++
			} else {
				server.Raise(loadErr)
			}
			lease = &model.CompetitionSupervisorLease{Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId, OwnerId: ownerId, FencingToken: token, LeaseExpiresAt: now.Add(ttl)}
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_supervisor_lease(environment,competition_id,season_id,owner_id,fencing_token,lease_expires_at,heartbeat_at) VALUES($1,$2,$3,$4,$5,$6,$7)
	 ON CONFLICT(environment,competition_id,season_id) DO UPDATE SET owner_id=excluded.owner_id,fencing_token=excluded.fencing_token,lease_expires_at=excluded.lease_expires_at,heartbeat_at=excluded.heartbeat_at`, lease.Environment, lease.CompetitionId, lease.SeasonId, lease.OwnerId, lease.FencingToken, lease.LeaseExpiresAt, now))
		})
	})
	return lease, err
}

// A stale token cannot renew itself after another owner has acquired authority.
func (self PostgresStore) RenewAutomationLease(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, ttl time.Duration) (result *model.CompetitionSupervisorLease, err error) {
	if !settings.AutomationPolicy.Enabled {
		return nil, ErrAutomationDisabled
	}
	if ttl < 10*time.Second || ttl > 5*time.Minute {
		return nil, ErrConflict
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			now := self.nowUtc()
			lease.LeaseExpiresAt = now.Add(ttl)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_supervisor_lease SET lease_expires_at=$4,heartbeat_at=$5 WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, lease.Environment, lease.CompetitionId, lease.SeasonId, lease.LeaseExpiresAt, now))
			copyLease := lease
			result = &copyLease
		})
	})
	return result, err
}

// Called with the scope transaction lock; no other connection is acquired.
func (self PostgresStore) requireAutomationLeaseInTx(ctx context.Context, tx server.PgTx, settings *Settings, lease model.CompetitionSupervisorLease) error {
	if !settings.AutomationPolicy.Enabled {
		return ErrAutomationDisabled
	}
	if lease.Environment != settings.AutomationPolicy.Environment || lease.CompetitionId != settings.CompetitionId || lease.SeasonId != settings.AutomationPolicy.SeasonId {
		return ErrLeaseLost
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_supervisor_lease WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND owner_id=$4 AND fencing_token=$5 AND $6<lease_expires_at)`, lease.Environment, lease.CompetitionId, lease.SeasonId, lease.OwnerId, lease.FencingToken, self.nowUtc()).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrLeaseLost
	}
	// Database triggers independently fence stale and legacy source writers.
	_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.competition_v2_owner',$1,true),set_config('urnetwork.competition_v2_fence',$2,true),
	 set_config('urnetwork.competition_v2_bootstrap_lock',$3,true),set_config('urnetwork.competition_v2_bootstrap_base',$4,true),
	 set_config('urnetwork.competition_v2_bootstrap_margin',$5,true),set_config('urnetwork.competition_v2_policy_approval',$6,true),
	 set_config('urnetwork.competition_v2_staging',$7,true),set_config('urnetwork.competition_v2_staging_window',$8,true),set_config('urnetwork.competition_v2_staging_preparation',$9,true)`, lease.OwnerId, strconv.FormatInt(lease.FencingToken, 10), settings.AutomationPolicy.SourceLockSha256, settings.BaseSha, strconv.FormatFloat(settings.EvaluationPolicy.TakeoverMargin, 'g', -1, 64), settings.AutomationPolicy.PolicyApprovalSha256,
		strconv.FormatBool(settings.AutomationPolicy.Staging), strconv.Itoa(settings.AutomationPolicy.StagingRoundWindowSeconds), strconv.Itoa(settings.AutomationPolicy.StagingPreparationSeconds))
	return err
}

// Reads one immutable intent and its sole mutable activation timestamp.
func sourceActivationInTx(ctx context.Context, tx server.PgTx, settings *Settings, generation int64) (*model.CompetitionSourceActivation, error) {
	var content []byte
	var activatedAt, abandonedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT intent_json,activated_at,abandoned_at FROM competition_source_activation WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, generation).Scan(&content, &activatedAt, &abandonedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	intent := &model.CompetitionSourceActivation{}
	if err = json.Unmarshal(content, intent); err != nil {
		return nil, err
	}
	if !sourceModeMatches(settings, intent) {
		return nil, ErrConflict
	}
	intent.ActivatedAt = activatedAt
	intent.AbandonedAt = abandonedAt
	if activatedAt != nil {
		intent.State = "active"
	} else if abandonedAt != nil {
		intent.State = "abandoned"
	}
	return intent, nil
}

// Resolves only the database pointer, never a local yaml file or moving git branch.
func currentSourceActivationInTx(ctx context.Context, tx server.PgTx, settings *Settings) (*model.CompetitionSourceActivation, error) {
	var generation int64
	err := tx.QueryRow(ctx, `SELECT generation FROM competition_source_pointer WHERE environment=$1 AND competition_id=$2 AND season_id=$3`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return sourceActivationInTx(ctx, tx, settings, generation)
}

// Rechecks source authorization against the current review and exact incumbent at activation time.
func validateSourceAuthorizationInTx(ctx context.Context, tx server.PgTx, settings *Settings, intent *model.CompetitionSourceActivation, current *model.CompetitionSourceActivation) error {
	if !sourceModeMatches(settings, intent) || (current != nil && !sourceModeMatches(settings, current)) {
		return ErrConflict
	}
	if current == nil {
		if intent.Kind != "baseline" || intent.ExpectedGeneration != 0 || intent.TargetEpoch != 1 || intent.SourceGeneration != 0 || intent.SourceLockSha256 != settings.AutomationPolicy.SourceLockSha256 || intent.Commits["server"] != settings.BaseSha || intent.Threshold != settings.EvaluationPolicy.TakeoverMargin || !sha256Pattern.MatchString(settings.AutomationPolicy.PolicyApprovalSha256) {
			return ErrConflict
		}
		return nil
	}
	if intent.AbandonedAt != nil || intent.ExpectedGeneration != current.Generation || intent.Generation <= current.Generation || intent.TargetEpoch != current.TargetEpoch+1 || intent.BaseSourceLockSha256 != current.SourceLockSha256 {
		return ErrConflict
	}
	var predecessorId server.Id
	if err := tx.QueryRow(ctx, `SELECT round_id FROM competition_round WHERE competition_id=$1 AND epoch_number=$2 AND staging=$5
	 AND policy_json->'automation_policy'->>'environment'=$3 AND policy_json->'automation_policy'->>'season_id'=$4`,
		settings.CompetitionId, intent.TargetEpoch-1, intent.Environment, intent.SeasonId, intent.Staging).Scan(&predecessorId); err != nil {
		return err
	}
	if err := ensureAutomationRoundValidInTx(ctx, tx, predecessorId); err != nil {
		return err
	}
	switch intent.Kind {
	case "reviewed_source_carry_forward":
		if intent.SourceGeneration != current.SourceGeneration || intent.SourceLockSha256 != current.SourceLockSha256 || intent.Threshold != current.Threshold || intent.ThresholdEvidenceSha256 != current.ThresholdEvidenceSha256 || !reflect.DeepEqual(intent.Commits, current.Commits) || intent.ReviewId != nil {
			return errors.New("carry-forward cannot change reviewed source or threshold")
		}
		if intent.Reason != "review_pending" && intent.Reason != "promotion_not_ready" && intent.Reason != "no_reward_winner" && intent.Reason != "readiness_fallback" {
			return errors.New("carry-forward reason is required")
		}
	case "reviewed_source_promotion":
		if intent.ReviewId == nil || intent.SourceGeneration != current.SourceGeneration+1 || intent.SourceLockSha256 == current.SourceLockSha256 {
			return ErrConflict
		}
		var content []byte
		var roundId server.Id
		var jobId server.Id
		err := tx.QueryRow(ctx, `SELECT review_json,round_id,job_id FROM competition_integrity_review WHERE review_id=$1`, intent.ReviewId).Scan(&content, &roundId, &jobId)
		if err != nil {
			return err
		}
		if err = ensureAutomationRoundValidInTx(ctx, tx, roundId); err != nil {
			return err
		}
		var sealedEligible bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_evaluation_seal s,jsonb_array_elements(s.seal_json->'entries') e
		 WHERE s.round_id=$1 AND (e->>'job_id')::uuid=$2 AND COALESCE((e->>'eligible')::boolean,false))`, roundId, jobId).Scan(&sealedEligible); err != nil {
			return err
		}
		if !sealedEligible {
			return errors.New("source promotion requires an eligible candidate from the immutable evaluation seal")
		}
		var review model.CompetitionIntegrityReviewResult
		if err = json.Unmarshal(content, &review); err != nil {
			return err
		}
		if review.Request.SourceDisposition != "approved" || review.Request.BaseSourceLockSha256 != current.SourceLockSha256 || review.Request.PatchSha256 != intent.ApprovedPatchSha256 || review.RequestSha256 != intent.ReviewDecisionDigest {
			return errors.New("source intent does not match genuine exact-source approval")
		}
		var currentSourceReviewId server.Id
		var rewardDisposition string
		err = tx.QueryRow(ctx, `SELECT review_id FROM competition_integrity_review WHERE round_id=$1 AND job_id=$2 AND review_json->'request'->>'source_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1`, roundId, jobId).Scan(&currentSourceReviewId)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT review_json->'request'->>'reward_disposition' FROM competition_integrity_review WHERE round_id=$1 AND job_id=$2 AND review_json->'request'->>'reward_disposition'<>'unchanged' ORDER BY review_cursor DESC LIMIT 1`, roundId, jobId).Scan(&rewardDisposition)
		if err != nil {
			return err
		}
		if currentSourceReviewId != review.ReviewId || (rewardDisposition != "approved" && rewardDisposition != "restored") {
			return errors.New("source approval was superseded or disqualified")
		}
		var sourceApprovalRevoked bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM competition_integrity_review r JOIN competition_integrity_review approval ON approval.review_id=$1
		 WHERE r.round_id=approval.round_id AND r.job_id=approval.job_id AND r.review_json->'request'->>'reward_disposition'='disqualified'
		 AND r.review_cursor>=approval.review_cursor)`, review.ReviewId).Scan(&sourceApprovalRevoked); err != nil {
			return err
		}
		if sourceApprovalRevoked {
			return errors.New("reward disqualification requires a fresh explicit source approval after any appeal")
		}
		var scoreJson []byte
		var policyJson []byte
		err = tx.QueryRow(ctx, `SELECT j.score_json,r.policy_json FROM competition_job j JOIN competition_round r USING(round_id) WHERE j.job_id=$1 AND r.competition_id=$2`, jobId, settings.CompetitionId).Scan(&scoreJson, &policyJson)
		if err != nil {
			return err
		}
		policy, err := decodeRoundPolicySnapshot(policyJson)
		if err != nil {
			return err
		}
		if policy.AutomationPolicy == nil || policy.AutomationPolicy.Staging != intent.Staging || policy.AutomationPolicy.Environment != intent.Environment || policy.AutomationPolicy.SeasonId != intent.SeasonId || policy.EvaluatorImageDigest != intent.EvaluatedImageDigest {
			return ErrConflict
		}
		var score ScoreResult
		if err = json.Unmarshal(scoreJson, &score); err != nil {
			return err
		}
		if err = validateScore(&score); err != nil {
			return err
		}
		threshold := 0.0
		if !intent.Staging {
			if !score.Significance.RecommendedNextEpochTakeoverMarginSupported || score.Significance.RecommendedNextEpochTakeoverMarginPercent == nil {
				return ErrConflict
			}
			threshold = max(current.Threshold, *score.Significance.RecommendedNextEpochTakeoverMarginPercent/100)
		}
		if intent.Threshold != threshold || intent.ThresholdEvidenceSha256 != review.Request.ScoreSha256 {
			return errors.New("source threshold must follow the exact promoted implementation")
		}
		for repository, commit := range current.Commits {
			if repository != "server" && intent.Commits[repository] != commit {
				return errors.New("promotion cannot change protected dependency or runner commits")
			}
		}
	default:
		return errors.New("unsupported source transition")
	}
	return nil
}

// Persists a complete recovery intent before any source refs are published.
func (self PostgresStore) RecordSourceIntent(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, intent model.CompetitionSourceActivation) (result *model.CompetitionSourceActivation, err error) {
	if !sourceModeMatches(settings, &intent) || intent.Environment != settings.AutomationPolicy.Environment || intent.CompetitionId != settings.CompetitionId || intent.SeasonId != settings.AutomationPolicy.SeasonId || intent.Generation < 1 || intent.TargetEpoch < 1 || intent.TargetEpoch > 6 || intent.TargetRoundId == (server.Id{}) || intent.PreparedAt.IsZero() || (!intent.OpensAt.IsZero() && intent.OpensAt.Before(intent.PreparedAt.Add(sourcePreparationInterval(&intent)))) || !gitShaPattern.MatchString(intent.ManifestCommit) || !sha256Pattern.MatchString(intent.ManifestSha256) || !sha256Pattern.MatchString(intent.ThresholdEvidenceSha256) || !imageDigestPattern.MatchString(intent.EvaluatorImageDigest) || !sha256Pattern.MatchString(intent.HostQualificationSha256) {
		return nil, ErrConflict
	}
	digest, err := model.CompetitionSourceLockDigest(intent.Commits)
	if err != nil || digest != intent.SourceLockSha256 {
		return nil, ErrConflict
	}
	manifestDigest := sha256.Sum256(intent.Manifest)
	if len(intent.Manifest) == 0 || len(intent.Manifest) > 1024*1024 || hex.EncodeToString(manifestDigest[:]) != intent.ManifestSha256 {
		return nil, errors.New("immutable source manifest digest mismatch")
	}
	if !sha256Pattern.MatchString(intent.SourceDecisionDigest) || !sha256Pattern.MatchString(intent.PublicationJournalSha256) || !gitShaPattern.MatchString(intent.ConfigExpectedCommit) || len(intent.GitRefs) != len(intent.Commits)+1 {
		return nil, errors.New("source publication recovery identity is incomplete")
	}
	for repository, commit := range intent.Commits {
		ref, ok := intent.GitRefs[repository]
		if !ok || ref.Commit != commit || !gitShaPattern.MatchString(ref.ExpectedCommit) || ref.Branch == "" || strings.ContainsAny(ref.Branch, "\r\n\x00 ~^:?*[\\") || strings.Contains(ref.Branch, "..") || strings.HasPrefix(ref.Branch, "-") {
			return nil, errors.New("source repository publication ref is invalid")
		}
	}
	configRef, ok := intent.GitRefs["config"]
	if !ok || configRef.ExpectedCommit != intent.ConfigExpectedCommit || configRef.Commit != intent.ManifestCommit || configRef.Branch == "" {
		return nil, errors.New("config publication ref does not match manifest commit")
	}
	for repository, ref := range intent.GitRefs {
		if (intent.Staging && ((repository == "config" && ref.Branch != "main") || (repository != "config" && ref.Branch != "sim-latency-staging"))) || (!intent.Staging && ref.Branch == "sim-latency-staging") {
			return nil, errors.New("source publication branch does not match its explicit staging mode")
		}
	}
	intent.ActivatedAt = nil
	intent.AbandonedAt = nil
	intent.State = "prepared"
	intent.IntentSha256 = ""
	intent.IntentSha256, err = model.CompetitionAutomationDigest(intent)
	if err != nil {
		return nil, err
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			existing, loadErr := sourceActivationInTx(ctx, tx, settings, intent.Generation)
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "source_activation"))
			if loadErr == nil {
				if existing.AbandonedAt != nil || existing.IntentSha256 != intent.IntentSha256 {
					panic(ErrConflict)
				}
				current, currentErr := currentSourceActivationInTx(ctx, tx, settings)
				if currentErr != nil && !errors.Is(currentErr, ErrNotFound) {
					server.Raise(currentErr)
				}
				if existing.ActivatedAt != nil {
					if current == nil || current.Generation != existing.Generation {
						panic(ErrConflict)
					}
					current = nil
					if existing.ExpectedGeneration > 0 {
						current, currentErr = sourceActivationInTx(ctx, tx, settings, existing.ExpectedGeneration)
						server.Raise(currentErr)
					}
				}
				server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, existing, current))
				result = existing
				return
			}
			if !errors.Is(loadErr, ErrNotFound) {
				server.Raise(loadErr)
			}
			var nextGeneration int64
			server.Raise(tx.QueryRow(ctx, `SELECT COALESCE(max(generation),0)+1 FROM competition_source_activation WHERE environment=$1 AND competition_id=$2 AND season_id=$3`,
				intent.Environment, intent.CompetitionId, intent.SeasonId).Scan(&nextGeneration))
			if intent.Generation != nextGeneration {
				panic(ErrConflict)
			}
			current, loadErr := currentSourceActivationInTx(ctx, tx, settings)
			if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
				server.Raise(loadErr)
			}
			server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, &intent, current))
			if intent.TargetEpoch > 1 {
				var sourceChoiceAt time.Time
				var rewardDigest string
				server.Raise(tx.QueryRow(ctx, `SELECT (s.seal_json->>'source_choice_at')::timestamptz,d.decision_sha256 FROM competition_evaluation_seal s JOIN competition_round r USING(round_id)
		 JOIN competition_reward_decision d USING(round_id) WHERE r.competition_id=$1 AND r.epoch_number=$2 AND r.staging=$3
		 AND r.policy_json->'automation_policy'->>'environment'=$4 AND r.policy_json->'automation_policy'->>'season_id'=$5 ORDER BY d.revision DESC LIMIT 1`, settings.CompetitionId, intent.TargetEpoch-1, intent.Staging, intent.Environment, intent.SeasonId).Scan(&sourceChoiceAt, &rewardDigest))
				if self.nowUtc().Before(sourceChoiceAt) || intent.PreparedAt.Before(sourceChoiceAt) || intent.RewardDecisionDigest != rewardDigest {
					panic(ErrConflict)
				}
				if intent.Kind == "reviewed_source_promotion" {
					var reviewedAt time.Time
					server.Raise(tx.QueryRow(ctx, `SELECT created_at FROM competition_integrity_review WHERE review_id=$1`, intent.ReviewId).Scan(&reviewedAt))
					if sourceChoiceAt.Before(reviewedAt) || intent.BuildReadyAt.IsZero() || sourceChoiceAt.Before(intent.BuildReadyAt) {
						panic(errors.New("promotion was not genuinely reviewed and build-ready at the frozen source-choice deadline"))
					}
				}
			}
			content, marshalErr := json.Marshal(intent)
			server.Raise(marshalErr)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_activation(environment,competition_id,season_id,generation,target_epoch,intent_sha256,intent_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`, intent.Environment, intent.CompetitionId, intent.SeasonId, intent.Generation, intent.TargetEpoch, intent.IntentSha256, string(content), self.nowUtc()))
			copyIntent := intent
			result = &copyIntent
		})
	})
	return result, err
}

// Independent components acknowledge exact content only after their own qualified readiness checks.
func (self PostgresStore) RecordSourceReadiness(ctx context.Context, settings *Settings, receipt model.CompetitionSourceReadiness) error {
	if receipt.Staging != settings.AutomationPolicy.Staging || receipt.Environment != settings.AutomationPolicy.Environment || receipt.CompetitionId != settings.CompetitionId || receipt.SeasonId != settings.AutomationPolicy.SeasonId || receipt.TargetRoundId == (server.Id{}) || !tokenNamePattern.MatchString(receipt.ActorId) || !imageDigestPattern.MatchString(receipt.RuntimeImageDigest) || !receipt.Ready || (receipt.Role != "api" && receipt.Role != "worker") {
		return ErrConflict
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			intent, err := sourceActivationInTx(ctx, tx, settings, receipt.Generation)
			server.Raise(err)
			if intent.AbandonedAt != nil {
				panic(ErrConflict)
			}
			if receipt.TargetRoundId != intent.TargetRoundId || receipt.ManifestCommit != intent.ManifestCommit || receipt.ManifestSha256 != intent.ManifestSha256 || receipt.SourceLockSha256 != intent.SourceLockSha256 || receipt.QualificationSha256 != intent.HostQualificationSha256 {
				panic(ErrConflict)
			}
			receipt.RecordedAt = time.Time{}
			expected, err := model.CompetitionAutomationDigest(receipt)
			server.Raise(err)
			var content []byte
			err = tx.QueryRow(ctx, `SELECT receipt_json FROM competition_source_readiness WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4 AND role=$5`, receipt.Environment, receipt.CompetitionId, receipt.SeasonId, receipt.Generation, receipt.Role).Scan(&content)
			if err == nil {
				var existing model.CompetitionSourceReadiness
				server.Raise(json.Unmarshal(content, &existing))
				existing.RecordedAt = time.Time{}
				actual, hashErr := model.CompetitionAutomationDigest(existing)
				server.Raise(hashErr)
				if actual != expected {
					panic(ErrConflict)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
			receipt.RecordedAt = self.nowUtc()
			content, err = json.Marshal(receipt)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_readiness(environment,competition_id,season_id,generation,role,receipt_json,created_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)`, receipt.Environment, receipt.CompetitionId, receipt.SeasonId, receipt.Generation, receipt.Role, string(content), receipt.RecordedAt))
		})
	})
}

// Config activation is last, after exact api and worker receipts and a fresh source-approval recheck.
func (self PostgresStore) ActivateSource(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, generation, expectedGeneration int64) (result *model.CompetitionSourceActivation, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			intent, loadErr := sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(loadErr)
			if intent.AbandonedAt != nil {
				panic(ErrConflict)
			}
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "source_activation"))
			current, loadErr := currentSourceActivationInTx(ctx, tx, settings)
			if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
				server.Raise(loadErr)
			}
			if current != nil && current.Generation == generation {
				if intent.ExpectedGeneration != expectedGeneration {
					panic(ErrConflict)
				}
				result = current
				return
			}
			if intent.ExpectedGeneration != expectedGeneration || (current == nil && expectedGeneration != 0) || (current != nil && current.Generation != expectedGeneration) {
				panic(ErrConflict)
			}
			server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, intent, current))
			var ready int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM competition_source_readiness WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4 AND (receipt_json->>'ready')::boolean AND receipt_json->>'manifest_sha256'=$5 AND receipt_json->>'source_lock_sha256'=$6 AND (receipt_json->>'target_round_id')::uuid=$7`, intent.Environment, intent.CompetitionId, intent.SeasonId, generation, intent.ManifestSha256, intent.SourceLockSha256, intent.TargetRoundId).Scan(&ready))
			if ready != 2 {
				panic(ErrReviewNotReady)
			}
			now := self.nowUtc()
			server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_source_activation SET activated_at=$5 WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4 AND activated_at IS NULL`, intent.Environment, intent.CompetitionId, intent.SeasonId, generation, now))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_source_pointer(environment,competition_id,season_id,generation) VALUES($1,$2,$3,$4) ON CONFLICT(environment,competition_id,season_id) DO UPDATE SET generation=excluded.generation`, intent.Environment, intent.CompetitionId, intent.SeasonId, generation))
			intent.ActivatedAt = &now
			intent.State = "active"
			result = intent
		})
	})
	return result, err
}

// Reads an immutable staged or active generation for crash recovery.
func (self PostgresStore) SourceActivation(ctx context.Context, settings *Settings, generation int64) (result *model.CompetitionSourceActivation, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			var loadErr error
			result, loadErr = sourceActivationInTx(ctx, tx, settings, generation)
			server.Raise(loadErr)
		})
	})
	return result, err
}

// Returns only the active authority selected by the database pointer.
func (self PostgresStore) CurrentSourceActivation(ctx context.Context, settings *Settings) (result *model.CompetitionSourceActivation, err error) {
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			var loadErr error
			result, loadErr = currentSourceActivationInTx(ctx, tx, settings)
			server.Raise(loadErr)
		})
	})
	return result, err
}

// Appends an expected-revision checkpoint while preserving every earlier attempt and blockage.
func (self PostgresStore) RecordSupervisorTransition(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, transition model.CompetitionSupervisorTransition) (result *model.CompetitionSupervisorTransition, err error) {
	validStateKVs := map[string]bool{"waiting": true, "evaluating": true, "sealing": true, "source_choice": true, "preparing": true, "preparation_blocked": true, "activating": true, "ready": true, "opening": true, "open": true, "complete": true, "incident_hold": true, "source_choice_pending": true, "source_publication_pending": true, "publication_blocked": true, "preparation_pending": true, "preparation_ready": true, "source_active": true, "round_created": true}
	if !validStateKVs[transition.State] || transition.TargetEpoch < 1 || transition.TargetEpoch > 7 || transition.ExpectedRevision < 0 || len(transition.Reason) > 2048 {
		return nil, ErrConflict
	}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			var revision int
			server.Raise(tx.QueryRow(ctx, `SELECT COALESCE(max(revision),0) FROM competition_supervisor_transition WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, transition.TargetEpoch).Scan(&revision))
			if revision != transition.ExpectedRevision {
				panic(ErrConflict)
			}
			transition.Revision = revision + 1
			transition.UpdatedAt = self.nowUtc()
			content, marshalErr := json.Marshal(transition)
			server.Raise(marshalErr)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_supervisor_transition(environment,competition_id,season_id,target_epoch,revision,state,transition_json,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8)`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, transition.TargetEpoch, transition.Revision, transition.State, string(content), transition.UpdatedAt))
			copyTransition := transition
			result = &copyTransition
		})
	})
	return result, err
}

// Reads the durable desired state so a restarted reconciler resumes the same transition.
func (self PostgresStore) SupervisorTransition(ctx context.Context, settings *Settings, targetEpoch int) (result *model.CompetitionSupervisorTransition, err error) {
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result = nil
			var content []byte
			loadErr := conn.QueryRow(ctx, `SELECT transition_json FROM competition_supervisor_transition WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND target_epoch=$4 ORDER BY revision DESC LIMIT 1`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, targetEpoch).Scan(&content)
			if errors.Is(loadErr, pgx.ErrNoRows) {
				panic(ErrNotFound)
			}
			server.Raise(loadErr)
			result = &model.CompetitionSupervisorTransition{}
			server.Raise(json.Unmarshal(content, result))
		})
	})
	return result, err
}
