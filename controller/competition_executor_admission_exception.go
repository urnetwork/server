// Operator exceptions are separate immutable decisions over retained failed
// admission documents. They never rewrite a sample, gate or failed judgement.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The caller supplies existing authority; this entry point cannot acquire or
// renew a lease and never starts the worker loop.
func RecordCompetitionExecutorAdmissionRequest(ctx context.Context, settings *Settings, input io.Reader) error {
	const limit = maxEvaluatorResultBytes + 65536
	content, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil || len(content) > limit {
		return errors.New("invalid executor admission request size or read failure")
	}
	var request struct {
		Lease          model.CompetitionSupervisorLease `json:"lease"`
		Approval       json.RawMessage                  `json:"approval"`
		ApprovalSha256 string                           `json:"approval_sha256"`
		Receipt        json.RawMessage                  `json:"receipt"`
	}
	if err := decodeFailureEvidenceJson(content, &request); err != nil {
		return errors.New("invalid executor admission request")
	}
	return (PostgresStore{}).RecordExecutorAdmissionException(ctx, settings, request.Lease, request.Approval, request.ApprovalSha256, request.Receipt)
}

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

// Bind the epoch-three override to the original Goofe evidence, not a
// replacement run or a later qualification with similar metrics.
func isEpochThreeGoofeAdmissionReceipt(receipt *competitionExecutorEquivalenceReceipt) bool {
	return receipt != nil &&
		receipt.RoundId == "01a1f4d0-47c9-ac2d-ce45-55ef159a9b41" &&
		receipt.HostId == "goofe" &&
		receipt.QualificationJobId == "0196802b-b5d9-3dfe-a52a-b9eb97dd118b" &&
		receipt.BaselineSha256 == "3b6c73d5cb2fd3c370ed5e79990184500fc9bef9358c36ffb2062d746212bc02" &&
		receipt.ArchiveManifestSha256 == "d8925103a48981c2a6d3e34c18963fc6886393042270e65b945ecabfb21b5486" &&
		receipt.Retention != nil &&
		receipt.Retention.Sha256 == "f48570db8860e5fcfac81eae5fdf04128e15fba4f94b24c051693c0ba56e3d3f" &&
		receipt.Retention.VersionId == "7b18931c-57c4-4cc3-989d-1256c0b8e04d"
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
