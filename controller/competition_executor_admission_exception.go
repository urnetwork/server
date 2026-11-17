// Operator exceptions are separate immutable decisions over retained failed
// admission documents. They never rewrite a sample, gate or failed judgement.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Approval binds one failed document, not a host-wide or season-wide waiver.
type CompetitionExecutorAdmissionException struct {
	Schema                int       `json:"schema"`
	Kind                  string    `json:"kind"`
	RoundId               server.Id `json:"round_id"`
	HostId                string    `json:"host_id"`
	JobId                 server.Id `json:"job_id"`
	BaselineSha256        string    `json:"baseline_sha256"`
	ReceiptDocumentSha256 string    `json:"receipt_document_sha256"`
	ExceptedMetric        string    `json:"excepted_metric"`
	EvidenceSha256        string    `json:"evidence_sha256"`
}

// Canonical authority and exact receipt content are authenticated on every read.
func validateCompetitionAdmissionException(value CompetitionExecutorAdmissionException, receipt *competitionExecutorEquivalenceReceipt) error {
	if receipt == nil || value.Schema != 1 || value.Kind != "executor-retained-admission-exception/v1" || value.RoundId.String() != receipt.RoundId || value.HostId != receipt.HostId || value.JobId.String() != receipt.QualificationJobId || value.BaselineSha256 != receipt.BaselineSha256 || value.ExceptedMetric != "throughput_p50_bytes_per_s" || !sha256Pattern.MatchString(value.EvidenceSha256) || receipt.Retention == nil || value.ReceiptDocumentSha256 != receipt.Retention.Sha256 {
		return errors.New("admission exception differs from its exact failed evidence")
	}
	return nil
}

// Separate authority does not consume, extend or create any evaluation clock.
func (self PostgresStore) RecordExecutorAdmissionException(ctx context.Context, settings *Settings, lease model.CompetitionSupervisorLease, approval []byte, expected string, receiptContent []byte) error {
	if settings == nil || len(approval) == 0 || len(approval) > 32768 || len(receiptContent) > maxEvaluatorResultBytes || !sha256Pattern.MatchString(expected) {
		return errors.New("admission exception requires bounded canonical approval")
	}
	var value CompetitionExecutorAdmissionException
	var receipt competitionExecutorEquivalenceReceipt
	if decodeFailureEvidenceJson(approval, &value) != nil || decodeFailureEvidenceJson(receiptContent, &receipt) != nil {
		return errors.New("admission exception has an invalid closed document")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != expected || string(canonical) != string(approval) || validateCompetitionAdmissionException(value, &receipt) != nil {
		return errors.New("admission exception approval authentication failed")
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
	frozen.executorQualificationRecovery, err = self.loadExecutorQualificationRecovery(ctx, frozen, round, value.HostId)
	if err != nil {
		return err
	}
	baseline, err := self.distributedRoundBaseline(ctx, frozen, value.RoundId)
	if err != nil {
		return err
	}
	if baseline == nil {
		return ErrConflict
	}
	found := false
	for _, host := range frozen.DistributedEvaluationPolicy.Hosts {
		if host.HostId != value.HostId {
			continue
		}
		found = true
		host = competitionExecutorControlHost(frozen, host)
		if err := validateCompetitionExecutorReceiptWithException(frozen, round, baseline, host, &receipt, value.ReceiptDocumentSha256); err != nil {
			return err
		}
	}
	if !found {
		return ErrConflict
	}
	reader, ok := frozen.artifactArchive.(interface {
		VerifyExecutorQualification(context.Context, *retainedArtifact) error
	})
	if !ok {
		return errors.New("admission exception requires exact-version retention authentication")
	}
	if err := reader.VerifyExecutorQualification(ctx, receipt.Retention); err != nil {
		return err
	}
	receiptContent, err = json.Marshal(&receipt)
	if err != nil {
		return err
	}
	receiptDigest := sha256.Sum256(receiptContent)
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, frozen)
			server.Raise(self.requireAutomationLeaseInTx(ctx, tx, frozen, lease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, frozen, "evaluation"))
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_admission_exception',$1,true)`, expected))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_admission_exception(round_id,host_id,approval_sha256,approval_json,approval_canonical,receipt_json,receipt_canonical,receipt_sha256,baseline_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT(round_id,host_id) DO NOTHING`, value.RoundId, value.HostId, expected, json.RawMessage(approval), approval, json.RawMessage(receiptContent), receiptContent, hex.EncodeToString(receiptDigest[:]), baseline.Sha256, receipt.CreatedAt))
			var stored string
			server.Raise(tx.QueryRow(ctx, `SELECT approval_sha256 FROM competition_executor_admission_exception WHERE round_id=$1 AND host_id=$2`, value.RoundId, value.HostId).Scan(&stored))
			if stored != expected {
				panic(ErrConflict)
			}
		}, server.OptReadWrite())
	})
}
