// An operator-stopped execution has no invented heartbeat diagnosis. Its
// separate attestation must match the immutable claim-to-expiry sequence.
package controller

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Only the late expired reclaim of a stopped attempt is covered. Other stops,
// genuine outcomes, archive failures and guessed causes retain their own rules.
func validateCompetitionReevaluationOperatorInterruptionShape(a model.CompetitionJobReevaluationAuthorization) error {
	proof := a.OperatorInterruptionEvidence
	if proof == nil || proof.Schema != 1 || proof.Action != "stop_worker" || proof.StoppedAt.IsZero() ||
		!proof.StoppedAt.Equal(proof.StoppedAt.Truncate(time.Microsecond)) || !proof.StoppedAt.Before(a.AuthorizedAt) ||
		proof.ClaimEventId < 1 || a.IncidentEventId <= proof.ClaimEventId ||
		!sha256Pattern.MatchString(proof.ClaimEventSha256) || !sha256Pattern.MatchString(proof.EvidenceSha256) ||
		a.IncidentAttempt < 1 || a.ExpectedAttemptCount <= a.IncidentAttempt || a.ExpectedAttemptCount-a.IncidentAttempt != 1 ||
		a.FailureManifestSha256 != "" || a.RejectedArtifactPath != "" || a.BackingServiceEvidence != nil {
		return ErrConflict
	}
	return nil
}

// The terminal event remains real and separately bound. Between the stopped
// claim and that terminal event, exactly one late claim must own the unchanged
// expired budget; an actual hand-back or intervening outcome cannot be hidden.
func validateCompetitionReevaluationOperatorInterruptionInTx(ctx context.Context, tx server.PgTx,
	a model.CompetitionJobReevaluationAuthorization, started, completed time.Time) error {
	if validateCompetitionReevaluationOperatorInterruptionShape(a) != nil {
		return ErrConflict
	}
	proof := a.OperatorInterruptionEvidence
	var claimedAt time.Time
	var digest string
	var payload []byte
	err := tx.QueryRow(ctx, `SELECT event_at,payload_sha256,payload_json FROM competition_job_event
	 WHERE job_id=$1 AND event_id=$2 AND event_type='claimed'`, a.JobId, proof.ClaimEventId).Scan(&claimedAt, &digest, &payload)
	var claim struct {
		Attempt int `json:"attempt"`
	}
	if err != nil || digest != proof.ClaimEventSha256 || json.Unmarshal(payload, &claim) != nil || claim.Attempt != a.IncidentAttempt ||
		claimedAt.Before(started) || proof.StoppedAt.Before(claimedAt) {
		return ErrConflict
	}
	var policyJson []byte
	if err := tx.QueryRow(ctx, `SELECT r.policy_json FROM competition_round r JOIN competition_job j ON j.round_id=r.round_id
	 WHERE j.job_id=$1 AND r.round_id=$2`, a.JobId, a.RoundId).Scan(&policyJson); err != nil {
		return ErrConflict
	}
	policy, err := decodeRoundPolicySnapshot(policyJson)
	if err != nil || policy.EvaluationPolicy.ScoreTimeoutSeconds != submissionEvaluationTimeoutSeconds {
		return ErrConflict
	}
	deadline := started.Add(time.Duration(policy.EvaluationPolicy.ScoreTimeoutSeconds) * time.Second)
	if !proof.StoppedAt.Before(deadline) || completed.Before(deadline) {
		return ErrConflict
	}
	var nextId int64
	var nextAt time.Time
	var nextType string
	if err := tx.QueryRow(ctx, `SELECT event_id,event_at,event_type,payload_json FROM competition_job_event
	 WHERE job_id=$1 AND event_id>$2 ORDER BY event_id LIMIT 1`, a.JobId, proof.ClaimEventId).Scan(&nextId, &nextAt, &nextType, &payload); err != nil ||
		nextType != "claimed" || nextId >= a.IncidentEventId || nextAt.Before(deadline) || nextAt.After(completed) ||
		json.Unmarshal(payload, &claim) != nil || claim.Attempt != a.ExpectedAttemptCount {
		return ErrConflict
	}
	var terminalId int64
	if err := tx.QueryRow(ctx, `SELECT event_id FROM competition_job_event
	 WHERE job_id=$1 AND event_id>$2 ORDER BY event_id LIMIT 1`, a.JobId, nextId).Scan(&terminalId); err != nil || terminalId != a.IncidentEventId {
		return ErrConflict
	}
	return nil
}
