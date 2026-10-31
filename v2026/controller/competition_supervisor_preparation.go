// Preparation is cached only after both independent runtime acknowledgments
// exist for the exact immutable intent, not merely a successful adapter exit.
package controller

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Only immutable reviewed-source fields move; control-plane policy, command
// pins, vault access and containment settings retain their approved release.
// Callers obtain the intent from the store, never from a worker request body.
func sourceSettingsFromIntent(settings *Settings, intent *model.CompetitionSourceActivation) (*Settings, error) {
	if !sourceModeMatches(settings, intent) || intent.Environment != settings.AutomationPolicy.Environment || intent.CompetitionId != settings.CompetitionId || intent.SeasonId != settings.AutomationPolicy.SeasonId || intent.State == "abandoned" || intent.TargetRoundId == (server.Id{}) {
		return nil, ErrConflict
	}
	lock, err := model.CompetitionSourceLockDigestForMode(intent.Commits, intent.Staging)
	if err != nil || lock != intent.SourceLockSha256 || !imageDigestPattern.MatchString(intent.EvaluatorImageDigest) || !sha256Pattern.MatchString(intent.HostQualificationSha256) {
		return nil, ErrConflict
	}
	if _, err := model.CompetitionScorerVersionForG2Policy(intent.G2VolumePolicy); err != nil {
		return nil, err
	}
	// Selection follows the configured target epoch, not a request's omitted
	// policy. Otherwise legacy omission could bypass an approved transition.
	if settings.FutureEpochPolicy != nil && settings.FutureEpochPolicy.FromEpoch <= intent.TargetEpoch {
		expected, err := settings.ForCompetitionEpoch(intent.TargetEpoch)
		if err != nil || expected.EvaluationPolicy.G2VolumePolicy != intent.G2VolumePolicy {
			return nil, errors.New("source policy does not match the approved future epoch")
		}
	}
	copy := *settings
	if intent.G2VolumePolicy != model.CompetitionG2VolumePolicyLegacy {
		if intent.Staging || intent.TargetEpoch < 2 {
			return nil, ErrConflict
		}
		future, err := settings.ForCompetitionEpoch(intent.TargetEpoch)
		if err != nil || future.EvaluationPolicy.G2VolumePolicy != intent.G2VolumePolicy {
			return nil, ErrConflict
		}
		copy = *future
		if !sha256Pattern.MatchString(intent.SimulatorSha256) || intent.SimulatorSha256 != intent.ScorerSha256 {
			return nil, errors.New("future source is missing its qualified simulator/scorer binary pins")
		}
		copy.EvaluationPolicy.SimulatorSha256, copy.EvaluationPolicy.ScorerSha256 = intent.SimulatorSha256, intent.ScorerSha256
		if copy.roundBaselinePreparation != nil && copy.roundBaselinePreparation.Config.G2VolumePolicy != intent.G2VolumePolicy {
			return nil, errors.New("future source requires its separately pinned baseline preparation role")
		}
	}
	copy.EvaluationPolicy.G2VolumePolicy = intent.G2VolumePolicy
	copy.BaseSha, copy.EvaluatorImageDigest = intent.Commits["server"], intent.EvaluatorImageDigest
	copy.EvaluationPolicy.TakeoverMargin, copy.EvaluationPolicy.HostQualificationSha256 = intent.Threshold, intent.HostQualificationSha256
	copy.AutomationPolicy.SourceGeneration, copy.AutomationPolicy.SourceLockSha256, copy.AutomationPolicy.SourceActivationGeneration = intent.SourceGeneration, intent.SourceLockSha256, intent.Generation
	return &copy, nil
}

// Pure receipt checking preserves a deterministic boundary for malformed,
// missing, duplicate, and different-generation readiness evidence.
func sourcePreparationReceiptsMatch(intent *model.CompetitionSourceActivation, receipts []model.CompetitionSourceReadiness) error {
	if intent == nil || intent.TargetRoundId == (server.Id{}) || len(receipts) != 2 {
		return ErrReviewNotReady
	}
	roles := map[string]bool{}
	for _, receipt := range receipts {
		if roles[receipt.Role] || !sourcePreparationReceiptMatches(intent, receipt) {
			return ErrReviewNotReady
		}
		roles[receipt.Role] = true
	}
	return nil
}

// Single-role validation does not fabricate the absent role to reuse quorum
// validation. Both roles must still be independently persisted before activation.
func sourcePreparationReceiptMatches(intent *model.CompetitionSourceActivation, receipt model.CompetitionSourceReadiness) bool {
	return intent != nil && intent.TargetRoundId != (server.Id{}) && (receipt.Role == "api" || receipt.Role == "worker") && receipt.Ready &&
		receipt.Staging == intent.Staging && receipt.Environment == intent.Environment && receipt.CompetitionId == intent.CompetitionId && receipt.SeasonId == intent.SeasonId && receipt.Generation == intent.Generation && receipt.TargetRoundId == intent.TargetRoundId &&
		receipt.ManifestCommit == intent.ManifestCommit && receipt.ManifestSha256 == intent.ManifestSha256 && receipt.SourceLockSha256 == intent.SourceLockSha256 && receipt.QualificationSha256 == intent.HostQualificationSha256 && imageDigestPattern.MatchString(receipt.RuntimeImageDigest) && tokenNamePattern.MatchString(receipt.ActorId)
}

// A containment-ready host without this exact reserved-round marker cannot
// acknowledge preparation, even when its source and image are otherwise right.
func sourceWorkerHostReady(host HostSelfCheck, settings *Settings, intent *model.CompetitionSourceActivation) bool {
	return intent != nil && intent.TargetRoundId != (server.Id{}) && host.Eligible(settings) && host.RebaselinePassed && host.RebaselineRoundId != nil && *host.RebaselineRoundId == intent.TargetRoundId
}

// Reads only durable acknowledgments; activation independently repeats these
// checks plus current source approval, pointer CAS, incidents and lease fencing.
func (self PostgresStore) SourcePreparationReady(ctx context.Context, settings *Settings, generation int64) error {
	intent, err := self.SourceActivation(ctx, settings, generation)
	if err != nil {
		return err
	}
	if intent.State == "abandoned" {
		return ErrConflict
	}
	receipts, err := self.sourcePreparationReceipts(ctx, settings, generation)
	if err != nil {
		return err
	}
	return sourcePreparationReceiptsMatch(intent, receipts)
}

// Reads the original independently recorded role identities after external
// preparation has completed. No database connection spans external work.
func (self PostgresStore) sourcePreparationReceipts(ctx context.Context, settings *Settings, generation int64) ([]model.CompetitionSourceReadiness, error) {
	receipts := []model.CompetitionSourceReadiness{}
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT receipt_json FROM competition_source_readiness WHERE environment=$1 AND competition_id=$2 AND season_id=$3 AND generation=$4`, settings.AutomationPolicy.Environment, settings.CompetitionId, settings.AutomationPolicy.SeasonId, generation)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var content []byte
					server.Raise(rows.Scan(&content))
					var receipt model.CompetitionSourceReadiness
					server.Raise(json.Unmarshal(content, &receipt))
					receipts = append(receipts, receipt)
				}
			})
		})
	})
	return receipts, err
}

// The concrete worker derives its own readiness from installed configuration,
// authenticated host probes, and archive availability. Callers cannot supply a
// readiness boolean or a claimed worker/runtime digest.
func AcknowledgeCompetitionSourceWorker(ctx context.Context, settings *Settings, generation int64, workerId string) error {
	if settings == nil || !settings.AutomationPolicy.Enabled {
		return ErrAutomationDisabled
	}
	store := PostgresStore{}
	intent, err := store.SourceActivation(ctx, settings, generation)
	if err != nil {
		return err
	}
	settings, err = sourceSettingsFromIntent(settings, intent)
	if err != nil {
		return err
	}
	if err := LoadCompetitionRuntimeManifestContext(ctx, settings, intent); err != nil {
		return err
	}
	worker, err := NewWorker(settings, store, CommandEvaluator{}, workerId)
	if err != nil {
		return err
	}
	host, err := worker.preflight(ctx)
	if err != nil {
		return err
	}
	if !sourceWorkerHostReady(host, settings, intent) || settings.artifactArchive == nil || settings.artifactArchive.Check(ctx) != nil {
		return errors.New("source_not_ready: exact-round worker host or archive qualification is incomplete")
	}
	return store.RecordSourceReadiness(ctx, settings, model.CompetitionSourceReadiness{Staging: intent.Staging, Environment: intent.Environment, CompetitionId: intent.CompetitionId, SeasonId: intent.SeasonId, Generation: intent.Generation, TargetRoundId: intent.TargetRoundId, Role: "worker", ActorId: workerId,
		ManifestCommit: intent.ManifestCommit, ManifestSha256: intent.ManifestSha256, SourceLockSha256: intent.SourceLockSha256, RuntimeImageDigest: worker.workerImageDigest, QualificationSha256: host.QualificationSha256, Ready: true})
}
