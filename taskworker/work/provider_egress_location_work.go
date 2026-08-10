package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type RemoveExpiredProviderEgressLocationsArgs struct{}

type RemoveExpiredProviderEgressLocationsResult struct{}

func ScheduleRemoveExpiredProviderEgressLocations(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredProviderEgressLocations,
		&RemoveExpiredProviderEgressLocationsArgs{},
		clientSession,
		task.RunOnce("remove_expired_provider_egress_locations"),
		task.RunAt(server.NowUtc().Add(6*time.Hour)),
	)
}

// RemoveExpiredProviderEgressLocations drops probed locations well past their
// trust window, so a provider that stops being probed eventually falls back to
// the mmdb path instead of being pinned to a stale location forever. The cutoff
// is deliberately looser than ProviderEgressLocationMaxAge: reads already
// ignore stale rows, so this is only reclaiming storage.
func RemoveExpiredProviderEgressLocations(
	_ *RemoveExpiredProviderEgressLocationsArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredProviderEgressLocationsResult, error) {
	now := server.NowUtc()
	minObservedAt := now.Add(-4 * model.ProviderEgressLocationMaxAge)
	model.RemoveExpiredProviderEgressLocations(clientSession.Ctx, minObservedAt)
	// probe attempts stop meaning anything once they no longer defer the
	// provider; same reasoning as above, a looser multiple of the window that
	// actually matters.
	minAttemptAt := now.Add(-4 * model.ProviderEgressProbeAttemptBackoff)
	model.RemoveExpiredProviderEgressProbeAttempts(clientSession.Ctx, minAttemptAt)
	return &RemoveExpiredProviderEgressLocationsResult{}, nil
}

func RemoveExpiredProviderEgressLocationsPost(
	_ *RemoveExpiredProviderEgressLocationsArgs,
	_ *RemoveExpiredProviderEgressLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredProviderEgressLocations(clientSession, tx)
	return nil
}
