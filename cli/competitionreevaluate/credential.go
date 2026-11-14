// The grant command alone consumes the optional protected credential registry.
// Ordinary API settings and their shared vault remain untouched.
package main

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Exact raw-file custody is checked by readRecoveryFile before this strict
// document decoder; the registration never supplies a Principal directly.
func bindRecoveryCredential(settings *controller.Settings, content []byte, auth model.CompetitionJobReevaluationAuthorization, limits recoverySettings, now time.Time) (*controller.Settings, error) {
	var registration controller.CompetitionOperatorPatchRetryCredential
	if decodeRecoveryDocument(content, &registration, limits.DocumentBytes) != nil ||
		auth.Schema != 2 || auth.Kind != model.CompetitionOperatorRequestedPatchRetryAuthorizationKind ||
		auth.IncidentClass != model.CompetitionJobReevaluationOperatorRequestedPatchRetry ||
		registration.RoundId != auth.RoundId || registration.JobId != auth.JobId || registration.Environment != auth.Environment ||
		registration.CompetitionId != auth.CompetitionId || registration.SeasonId != auth.SeasonId {
		return nil, errors.New("local patch-retry credential registration rejected")
	}
	local, err := controller.WithCompetitionOperatorPatchRetryCredential(settings, registration, now)
	if err != nil {
		return nil, errors.New("local patch-retry credential registration rejected")
	}
	return local, nil
}

// Expiry caps cancellable admission, native observations and SQL-body work,
// without changing evaluation deadlines. An observed stop refuses new
// statements, but successful bodies retain the existing bounded,
// cancellation-independent commit/ack completion. Timeout or expiry does not
// prove that no grant committed; reconcile the exact idempotency key.
func newRecoveryCredentialContext(parent context.Context, limits recoverySettings, startedAt time.Time, principal *controller.Principal) (context.Context, context.CancelFunc, error) {
	if principal != nil && !principal.ExpiresAt.IsZero() && principal.ExpiresAt.Before(startedAt.Add(limits.Timeout)) {
		limits.Timeout = principal.ExpiresAt.Sub(startedAt)
	}
	return newRecoveryControlContext(parent, limits, startedAt)
}
