// Post-close recovery is an explicit, finite operator approval, not a change
// to submission admission, original ordering, scoring policy or publication.
package controller

import (
	"time"

	"github.com/urnetwork/server/model"
)

// The authorization digest covers this extension. A close time or deadline
// copied from another round cannot widen its original recovery authority.
func validateCompetitionReevaluationPostCloseApproval(a model.CompetitionJobReevaluationAuthorization) error {
	approval := a.PostCloseApproval
	if approval == nil {
		return nil
	}
	if approval.Schema != 1 || approval.ClosesAt.IsZero() || approval.ExpiresAt.IsZero() ||
		!approval.ClosesAt.Before(approval.ExpiresAt) || !sha256Pattern.MatchString(approval.EvidenceSha256) ||
		a.AuthorizedAt.Before(approval.ClosesAt) || a.ExpiresAt.After(approval.ExpiresAt) {
		return ErrConflict
	}
	return nil
}

// Both authorization clock checks use the same closed-round semantics. The
// caller still locks the round and rejects cancellation, sealing and winners.
func competitionReevaluationWithinApprovedWindow(a model.CompetitionJobReevaluationAuthorization, round *roundRecord, now time.Time) bool {
	if now.Before(a.AuthorizedAt) || !now.Before(a.ExpiresAt) {
		return false
	}
	if a.PostCloseApproval == nil {
		return submissionWithinEpoch(round, now)
	}
	approval := a.PostCloseApproval
	return validateCompetitionReevaluationPostCloseApproval(a) == nil &&
		approval.ClosesAt.Equal(round.ClosesAt) && !now.Before(round.ClosesAt) && now.Before(approval.ExpiresAt)
}
