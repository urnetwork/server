// Resolves a durable, independently approved correction only for the exact
// epoch-three source. Other rounds keep their original readiness identity.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type competitionRuntimeCorrectionQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Persists one independently approved payload under the exact live fence.
// The intake source is not yet the current grading source, so read generation
// three under the scope lock rather than selecting the active source pointer.
func (self PostgresStore) RecordEpochThreeRuntimeCorrection(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, value model.CompetitionEpochThreeRuntimeCorrection, expected string) error {
	if settings == nil || !settings.AutomationPolicy.Enabled || settings.AutomationPolicy.Staging ||
		settings.AutomationPolicy.Environment != "main" || !sha256Pattern.MatchString(expected) {
		return ErrConflict
	}
	canonical, err := json.Marshal(value)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(canonical)) != expected {
		return ErrConflict
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, settings, lease))
			server.Raise(setAutomationLeaseGuardsInTx(ctx, tx, settings, lease))
			intent, err := sourceActivationInTx(ctx, tx, settings, value.SourceGeneration)
			server.Raise(err)
			server.Raise(model.ValidateCompetitionEpochThreeRuntimeCorrection(value, intent, expected))
			var existing []byte
			var existingSha256 string
			err = tx.QueryRow(ctx, `SELECT correction_canonical,correction_sha256
				FROM competition_epoch_three_runtime_correction WHERE round_id=$1`, value.RoundId).Scan(&existing, &existingSha256)
			if err == nil {
				if existingSha256 != expected || !bytes.Equal(existing, canonical) {
					panic(ErrConflict)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
			_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.competition_epoch_three_runtime_correction',$1,true)`, expected)
			server.Raise(err)
			_, err = tx.Exec(ctx, `INSERT INTO competition_epoch_three_runtime_correction
				(round_id,correction_sha256,correction_canonical,correction_json,created_at)
				VALUES($1,$2,$3,$4,transaction_timestamp())`, value.RoundId, expected, canonical, json.RawMessage(canonical))
			server.Raise(err)
		}, server.OptReadWrite())
	})
}

// Historical and synthetic rounds never query the correction journal.
func epochThreeRuntimeCorrectionApplies(intent *model.CompetitionSourceActivation) bool {
	return intent != nil && !intent.Staging && intent.Environment == "main" && intent.TargetEpoch == 3 && intent.Generation == 3 &&
		intent.TargetRoundId.String() == "01a1f4d0-47c9-ac2d-ce45-55ef159a9b41"
}

// A pre-migration reader sees no correction. An existing but invalid
// document is an error, not permission to retry the old identity.
func loadEpochThreeRuntimeCorrection(ctx context.Context, query competitionRuntimeCorrectionQuerier, intent *model.CompetitionSourceActivation) (*model.CompetitionEpochThreeRuntimeCorrection, error) {
	if !epochThreeRuntimeCorrectionApplies(intent) {
		return nil, nil
	}
	var installed bool
	if err := query.QueryRow(ctx, `SELECT to_regclass('competition_epoch_three_runtime_correction') IS NOT NULL`).Scan(&installed); err != nil || !installed {
		return nil, err
	}
	var raw []byte
	var approvedSha256 string
	err := query.QueryRow(ctx, `SELECT correction_canonical,correction_sha256
		FROM competition_epoch_three_runtime_correction WHERE round_id=$1`, intent.TargetRoundId).Scan(&raw, &approvedSha256)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value model.CompetitionEpochThreeRuntimeCorrection
	if len(raw) > 8192 || json.Unmarshal(raw, &value) != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != approvedSha256 ||
		model.ValidateCompetitionEpochThreeRuntimeCorrection(value, intent, approvedSha256) != nil {
		return nil, errors.New("stored epoch-three runtime correction failed authentication")
	}
	return &value, nil
}

// Releases the connection before any host, artifact, or evaluator checks.
func readEpochThreeRuntimeCorrection(ctx context.Context, intent *model.CompetitionSourceActivation) (correction *model.CompetitionEpochThreeRuntimeCorrection, err error) {
	if !epochThreeRuntimeCorrectionApplies(intent) {
		return nil, nil
	}
	err = captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var readErr error
			correction, readErr = loadEpochThreeRuntimeCorrection(ctx, conn, intent)
			server.Raise(readErr)
		})
	})
	return
}

// A correction changes only the worker identity. It never makes a receipt
// for another source, role, or runtime image eligible.
func sourcePreparationReceiptMatchesCorrection(intent *model.CompetitionSourceActivation, receipt model.CompetitionSourceReadiness, correction *model.CompetitionEpochThreeRuntimeCorrection) bool {
	if receipt.Role != "worker" || correction == nil {
		return sourcePreparationReceiptMatches(intent, receipt)
	}
	if intent == nil || receipt.RuntimeImageDigest != correction.WorkerImageDigest {
		return false
	}
	effective := *intent
	effective.HostQualificationSha256 = correction.WorkerQualificationSha256
	return sourcePreparationReceiptMatches(&effective, receipt)
}

// Baseline evidence carries its actual producer identity; the immutable
// evaluation policy continues to carry its original fingerprint.
func competitionBaselineQualification(settings *Settings, roundId server.Id) string {
	original := settings.EvaluationPolicy.HostQualificationSha256
	if correction := settings.epochThreeRuntimeCorrection; correction != nil &&
		correction.RoundId == roundId && correction.PreviousQualificationSha256 == original &&
		correction.SourceGeneration == settings.AutomationPolicy.SourceActivationGeneration &&
		correction.SourceLockSha256 == settings.AutomationPolicy.SourceLockSha256 &&
		correction.EvaluatorImageDigest == settings.EvaluatorImageDigest {
		return correction.WorkerQualificationSha256
	}
	return original
}
