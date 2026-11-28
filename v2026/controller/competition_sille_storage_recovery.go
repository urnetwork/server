package controller

import "github.com/urnetwork/server/v2026/model"

const competitionSilleStorageRecoveryReason = model.CompetitionSilleStorageRecoveryReason

// The scope is explicit so an ordinary timeout or a future Sille incident
// cannot inherit this exception. The evidence digest pins the sealed manifest
// of the storage log, signed native terminal frame, and cleanup record.
func competitionSilleStorageRecoveryScope(a model.CompetitionJobReevaluationAuthorization) bool {
	d := a.Distributed
	return a.Schema == 2 && a.ExpectedCycle == 0 && a.ExpectedAttemptCount == 1 &&
		a.IncidentAttempt == 1 && a.SingleUse && a.PostCloseApproval != nil &&
		a.SecondRecovery == nil && d != nil && d.PatchRetry == nil &&
		d.Reason == competitionSilleStorageRecoveryReason &&
		model.MatchCompetitionSilleStorageRecoveryScope(a.Environment, a.CompetitionId, a.SeasonId, a.RoundId.String(), a.JobId.String(),
			d.OriginalHostId, d.OriginalRunId, d.ClosureEvidenceSha256)
}
