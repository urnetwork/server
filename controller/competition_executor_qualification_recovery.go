// An operator-approved replacement has a separate immutable clock and job id.
// Its canonical authority cannot change source, workload, hosts or scoring.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// No configurable attempt count, deadline, threshold or replacement source.
type CompetitionExecutorQualificationRecovery struct {
	Schema                 int       `json:"schema"`
	Kind                   string    `json:"kind"`
	RoundId                server.Id `json:"round_id"`
	HostId                 string    `json:"host_id"`
	OriginalJobId          server.Id `json:"original_job_id"`
	ReplacementJobId       server.Id `json:"replacement_job_id"`
	BaselineSha256         string    `json:"baseline_sha256"`
	QualificationSha256    string    `json:"qualification_sha256"`
	ControlAmendmentSha256 string    `json:"control_amendment_sha256"`
	PolicyApprovalSha256   string    `json:"policy_approval_sha256"`
	SourceLockSha256       string    `json:"source_lock_sha256"`
	EvaluatorImageDigest   string    `json:"evaluator_image_digest"`
	ProvidersSha256        string    `json:"providers_sha256"`
	ConfigLocalSha256      string    `json:"config_local_sha256"`
	VaultLocalSha256       string    `json:"vault_local_sha256"`
	OriginalScoreSha256    string    `json:"original_score_sha256"`
	EvidenceSha256         string    `json:"evidence_sha256"`
}

// Private runtime authority is reconstructed from exact canonical stored bytes.
type competitionExecutorQualificationRecoveryRecord struct {
	Authorization CompetitionExecutorQualificationRecovery
	Sha256        string
}

// Canonical bytes are retained independently of PostgreSQL JSONB formatting.
func competitionExecutorQualificationRecoveryBytes(value CompetitionExecutorQualificationRecovery) ([]byte, string, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(content)
	return content, hex.EncodeToString(digest[:]), nil
}

// Scope is exact even when an attacker recomputes the outer document digest.
func validateCompetitionExecutorQualificationRecovery(settings *Settings, round *roundRecord, value CompetitionExecutorQualificationRecovery) error {
	if settings == nil || round == nil || settings.DistributedEvaluationPolicy == nil || round.Epoch < 2 || round.Staging ||
		value.Schema != 1 || value.Kind != "executor-qualification-single-recovery/v1" || value.RoundId != round.RoundId ||
		value.OriginalJobId != competitionExecutorQualificationJobId(round.RoundId, value.HostId, settings.DistributedEvaluationPolicy.ApprovalSha256) ||
		value.ReplacementJobId == (server.Id{}) || value.ReplacementJobId == value.OriginalJobId ||
		value.ControlAmendmentSha256 != competitionExecutorControlAmendmentSha256(settings) ||
		value.PolicyApprovalSha256 != settings.DistributedEvaluationPolicy.ApprovalSha256 || value.SourceLockSha256 != settings.AutomationPolicy.SourceLockSha256 ||
		value.EvaluatorImageDigest != settings.EvaluatorImageDigest || value.ProvidersSha256 != round.ProvidersSha256 ||
		value.ConfigLocalSha256 != settings.EvaluationPolicy.ConfigLocalSha256 || value.VaultLocalSha256 != settings.EvaluationPolicy.VaultLocalSha256 {
		return errors.New("qualification recovery differs from its frozen scope")
	}
	for _, digest := range []string{value.BaselineSha256, value.QualificationSha256, value.OriginalScoreSha256, value.EvidenceSha256} {
		if !sha256Pattern.MatchString(digest) {
			return errors.New("qualification recovery omitted its original failure or evidence")
		}
	}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		if host.HostId == value.HostId && competitionExecutorControlHost(settings, host).QualificationSha256 == value.QualificationSha256 {
			return nil
		}
	}
	return errors.New("qualification recovery changed host membership or identity")
}

// Authenticate before using a row to select a new clock or accept its receipt.
func decodeCompetitionExecutorQualificationRecovery(settings *Settings, round *roundRecord, canonical []byte, expected string) (*competitionExecutorQualificationRecoveryRecord, error) {
	if len(canonical) == 0 || len(canonical) > 32768 || !sha256Pattern.MatchString(expected) {
		return nil, errors.New("qualification recovery authority exceeds its bounds")
	}
	var value CompetitionExecutorQualificationRecovery
	if err := decodeFailureEvidenceJson(canonical, &value); err != nil {
		return nil, err
	}
	content, digest, err := competitionExecutorQualificationRecoveryBytes(value)
	if err != nil || digest != expected || !bytes.Equal(content, canonical) || validateCompetitionExecutorQualificationRecovery(settings, round, value) != nil {
		return nil, errors.New("qualification recovery authority failed authentication")
	}
	return &competitionExecutorQualificationRecoveryRecord{Authorization: value, Sha256: expected}, nil
}

// No result means the historical admission path. Reads own no long-lived I/O.
func (self PostgresStore) loadExecutorQualificationRecovery(ctx context.Context, settings *Settings, round *roundRecord, hostId string) (*competitionExecutorQualificationRecoveryRecord, error) {
	var canonical []byte
	var digest string
	err := captureDatabaseError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			err := conn.QueryRow(ctx, `SELECT authorization_canonical,authorization_sha256 FROM competition_executor_qualification_recovery WHERE round_id=$1 AND host_id=$2`, round.RoundId, hostId).Scan(&canonical, &digest)
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
		}, server.OptReadOnly())
	})
	if err != nil || len(canonical) == 0 {
		return nil, err
	}
	record, err := decodeCompetitionExecutorQualificationRecovery(settings, round, canonical, digest)
	if err != nil || record.Authorization.HostId != hostId {
		return nil, errors.New("stored qualification recovery has the wrong host or authority")
	}
	return record, nil
}

// Registration follows a real idle same-round self-check; only then does SQL
// reserve a database-generated three-hour clock. Repeats return the same row.
func (self PostgresStore) RecordExecutorQualificationRecovery(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, value CompetitionExecutorQualificationRecovery, expected string) error {
	if settings == nil || value.RoundId == (server.Id{}) || !sha256Pattern.MatchString(expected) {
		return errors.New("operator qualification recovery requires exact scope and digest")
	}
	intent, err := self.CurrentSourceActivation(ctx, settings)
	if err != nil {
		return err
	}
	if intent == nil || intent.TargetRoundId != value.RoundId {
		return ErrConflict
	}
	derived, err := sourceSettingsFromIntent(settings, intent)
	if err != nil {
		return err
	}
	round, err := self.GetRound(ctx, settings, value.RoundId)
	if err != nil {
		return err
	}
	frozen, err := settingsForFrozenRound(derived, round)
	if err != nil {
		return err
	}
	frozen.automationRoundId, frozen.automationLease = value.RoundId, &lease
	if err := self.loadExecutorControlAmendment(ctx, frozen, round); err != nil {
		return err
	}
	content, digest, err := competitionExecutorQualificationRecoveryBytes(value)
	if err != nil || digest != expected || validateCompetitionExecutorQualificationRecovery(frozen, round, value) != nil {
		return errors.New("operator qualification recovery does not match its approved bytes")
	}
	for _, host := range frozen.DistributedEvaluationPolicy.Hosts {
		if host.HostId != value.HostId {
			continue
		}
		host = competitionExecutorControlHost(frozen, host)
		selected := *frozen
		selected.executorHostId, selected.executorSlotId = host.HostId, host.SlotId
		transport, err := competitionTransportForHost(&selected, host)
		if err != nil {
			return err
		}
		evaluator := competitionAgentEvaluator{host: host, transport: transport}
		check, err := evaluator.SelfCheck(ctx, &selected)
		if err != nil || !check.RebaselinePassed || check.RebaselineRoundId == nil || *check.RebaselineRoundId != value.RoundId {
			return errors.New("replacement qualification requires genuine idle same-round readiness")
		}
	}
	workerImage, err := runtimeImageDigest()
	if err != nil {
		return err
	}
	receipts, err := self.sourcePreparationReceipts(ctx, frozen, intent.Generation)
	if err != nil {
		return err
	}
	apiImage, err := competitionSourceApiRuntimeIdentity(intent, "", receipts)
	if err != nil {
		return err
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, frozen)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, frozen, lease))
			server.Raise(setAutomationLeaseGuardsInTx(ctx, tx, frozen, lease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, frozen, "evaluation"))
			var existing string
			var original []byte
			err := tx.QueryRow(ctx, `SELECT authorization_sha256,authorization_canonical FROM competition_executor_qualification_recovery WHERE round_id=$1 AND host_id=$2`, value.RoundId, value.HostId).Scan(&existing, &original)
			if err == nil {
				if existing != expected || !bytes.Equal(original, content) {
					panic(ErrConflict)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				server.Raise(err)
			}
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_qualification_recovery',$1,true)`, expected))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_qualification_recovery(round_id,host_id,authorization_sha256,authorization_json,authorization_canonical,job_id,api_image_digest,worker_image_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
				value.RoundId, value.HostId, expected, json.RawMessage(content), content, value.ReplacementJobId, apiImage, workerImage))
		}, server.OptReadWrite())
	})
}
