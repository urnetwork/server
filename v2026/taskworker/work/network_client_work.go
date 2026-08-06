package work

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"

	// "github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
)

type CloseExpiredNetworkClientHandlersArgs struct {
}

type CloseExpiredNetworkClientHandlersResult struct {
}

func ScheduleCloseExpiredNetworkClientHandlers(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		CloseExpiredNetworkClientHandlers,
		&CloseExpiredNetworkClientHandlersArgs{},
		clientSession,
		task.RunOnce("close_expired_network_client_handlers"),
		task.MaxTime(15*time.Minute),
		task.RunAt(server.NowUtc().Add(model.NetworkClientHandlerHeartbeatTimeout)),
	)
}

func CloseExpiredNetworkClientHandlers(
	closeExpiredNetworkClientHandlers *CloseExpiredNetworkClientHandlersArgs,
	clientSession *session.ClientSession,
) (*CloseExpiredNetworkClientHandlersResult, error) {
	minTime := server.NowUtc().Add(-2 * model.NetworkClientHandlerHeartbeatTimeout)
	model.CloseExpiredNetworkClientHandlers(clientSession.Ctx, minTime)
	return &CloseExpiredNetworkClientHandlersResult{}, nil
}

func CloseExpiredNetworkClientHandlersPost(
	closeExpiredNetworkClientHandlers *CloseExpiredNetworkClientHandlersArgs,
	closeExpiredNetworkClientHandlersResult *CloseExpiredNetworkClientHandlersResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleCloseExpiredNetworkClientHandlers(clientSession, tx)
	return nil
}

type RemoveDisconnectedNetworkClientsArgs struct {
}

type RemoveDisconnectedNetworkClientsResult struct {
}

func ScheduleRemoveDisconnectedNetworkClients(clientSession *session.ClientSession, tx server.PgTx) {
	runAt := func() time.Time {
		now := server.NowUtc()
		year, month, day := now.Date()
		hour, minute, _ := now.Clock()
		return time.Date(year, month, day, hour, 5*(minute/5)+5, 0, 0, time.UTC)
	}()

	task.ScheduleTaskInTx(
		tx,
		RemoveDisconnectedNetworkClients,
		&RemoveDisconnectedNetworkClientsArgs{},
		clientSession,
		// legacy key
		task.RunOnce("delete_disconnected_network_clients"),
		task.RunAt(runAt),
		task.MaxTime(4*time.Hour),
	)
}

func RemoveDisconnectedNetworkClients(
	removeDisconnectedNetworkClients *RemoveDisconnectedNetworkClientsArgs,
	clientSession *session.ClientSession,
) (*RemoveDisconnectedNetworkClientsResult, error) {
	// connection rows are kept briefly for diagnostics; inactive clients are
	// reaped 30 days after deactivation, since provisioned child clients
	// (e.g. proxy devices) cannot recover from a reaped client_id; abandoned
	// top-level clients are marked inactive after 90 days unseen (auth_time)
	minConnectionTime := server.NowUtc().Add(-8 * time.Hour)
	minClientTime := server.NowUtc().Add(-model.NetworkClientReapAfterDeactivate)
	minTopLevelAuthTime := server.NowUtc().Add(-model.TopLevelClientIdleExpiration)
	model.RemoveDisconnectedNetworkClients(clientSession.Ctx, minConnectionTime, minClientTime, minTopLevelAuthTime)
	return &RemoveDisconnectedNetworkClientsResult{}, nil
}

func RemoveDisconnectedNetworkClientsPost(
	removeDisconnectedNetworkClients *RemoveDisconnectedNetworkClientsArgs,
	removeDisconnectedNetworkClientsResult *RemoveDisconnectedNetworkClientsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveDisconnectedNetworkClients(clientSession, tx)
	return nil
}

// SweepOrphanNetworkClientData is the low-cadence safety net for orphaned
// network-client dependent rows. RemoveDisconnectedNetworkClients cascades
// dependents together with the parent deletes on every run, so this only
// catches orphans from other deletion paths or older releases. Each pass is a
// full anti-join scan of the dependent tables, which is why it runs daily and
// not on the reap cadence.

type SweepOrphanNetworkClientDataArgs struct {
}

type SweepOrphanNetworkClientDataResult struct {
	RemovedCount int64 `json:"removed_count"`
}

func ScheduleSweepOrphanNetworkClientData(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SweepOrphanNetworkClientData,
		&SweepOrphanNetworkClientDataArgs{},
		clientSession,
		task.RunOnce("sweep_orphan_network_client_data"),
		// weekly, anchored off-peak (~10:00 UTC): the bounded cursor sweep pages
		// entire large child tables per pass (measured ~2% of db time for the
		// provide_key slices alone) while finding ~zero orphans in steady state
		// -- a weekly safety net is plenty, and `bringyourctl db sweep-orphans`
		// covers on-demand cleanup
		task.RunAt(nextWeeklyOffPeak(server.NowUtc())),
		task.MaxTime(4*time.Hour),
	)
}

func SweepOrphanNetworkClientData(
	sweepOrphanNetworkClientData *SweepOrphanNetworkClientDataArgs,
	clientSession *session.ClientSession,
) (*SweepOrphanNetworkClientDataResult, error) {
	// Re-enabled 2026-07-14 on the bounded cursor implementation (daily safety
	// net for orphans left by crashes mid-delete or older releases; the main
	// reaper cascades dependents inline, so steady state finds ~nothing). The
	// model fn pages each child table by primary key in sliceSize batches, one
	// maintenance tx per slice -- unlike the previous NOT EXISTS ... LIMIT form,
	// which full-scanned each driver table when orphans were rare (prod
	// incident 2026-07-14).
	sliceSize := 50000
	removedCount := model.SweepOrphanNetworkClientData(clientSession.Ctx, sliceSize)
	return &SweepOrphanNetworkClientDataResult{
		RemovedCount: removedCount,
	}, nil
}

func SweepOrphanNetworkClientDataPost(
	sweepOrphanNetworkClientData *SweepOrphanNetworkClientDataArgs,
	sweepOrphanNetworkClientDataResult *SweepOrphanNetworkClientDataResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSweepOrphanNetworkClientData(clientSession, tx)
	return nil
}

// FIXME remove
type RemoveLocationLookupResultsArgs struct {
}

type RemoveLocationLookupResultsResult struct {
}

func ScheduleRemoveLocationLookupResults(clientSession *session.ClientSession, tx server.PgTx) {
	// task.ScheduleTaskInTx(
	// 	tx,
	// 	RemoveLocationLookupResults,
	// 	&RemoveLocationLookupResultsArgs{},
	// 	clientSession,
	// 	task.RunOnce("remove_lookup_results"),
	// 	task.RunAt(server.NowUtc().Add(30*time.Minute)),
	// )
}

func RemoveLocationLookupResults(
	removeLocationLookupResults *RemoveLocationLookupResultsArgs,
	clientSession *session.ClientSession,
) (*RemoveLocationLookupResultsResult, error) {
	// minTime := server.NowUtc().Add(-controller.LocationLookupResultExpiration)
	// model.RemoveLocationLookupResults(clientSession.Ctx, minTime)
	return &RemoveLocationLookupResultsResult{}, nil
}

func RemoveLocationLookupResultsPost(
	removeLocationLookupResults *RemoveLocationLookupResultsArgs,
	removeLocationLookupResultsResult *RemoveLocationLookupResultsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	// ScheduleRemoveLocationLookupResults(clientSession, tx)
	return nil
}

type SetMissingConnectionLocationsArgs struct {
}

type SetMissingConnectionLocationsResult struct {
}

func ScheduleSetMissingConnectionLocations(clientSession *session.ClientSession, tx server.PgTx) {
	// nothing to do
}

func SetMissingConnectionLocations(
	setMissingConnectionLocations *SetMissingConnectionLocationsArgs,
	clientSession *session.ClientSession,
) (*SetMissingConnectionLocationsResult, error) {
	// nothing to do
	return &SetMissingConnectionLocationsResult{}, nil
}

func SetMissingConnectionLocationsPost(
	setMissingConnectionLocations *SetMissingConnectionLocationsArgs,
	setMissingConnectionLocationsResult *SetMissingConnectionLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

type RemoveOldProvideKeyChangesArgs struct {
}

type RemoveOldProvideKeyChangesResult struct {
}

func ScheduleRemoveOldProvideKeyChanges(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveOldProvideKeyChanges,
		&RemoveOldProvideKeyChangesArgs{},
		clientSession,
		task.RunOnce("remove_old_provide_key_changes"),
		task.RunAt(server.NowUtc().Add(15*time.Minute)),
	)
}

func RemoveOldProvideKeyChanges(
	removeOldProvideKeyChanges *RemoveOldProvideKeyChangesArgs,
	clientSession *session.ClientSession,
) (*RemoveOldProvideKeyChangesResult, error) {
	minTime := server.NowUtc().Add(-1 * time.Hour)
	model.RemoveOldProvideKeyChanges(clientSession.Ctx, minTime)
	return &RemoveOldProvideKeyChangesResult{}, nil
}

func RemoveOldProvideKeyChangesPost(
	removeOldProvideKeyChanges *RemoveOldProvideKeyChangesArgs,
	removeOldProvideKeyChangesResult *RemoveOldProvideKeyChangesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveOldProvideKeyChanges(clientSession, tx)
	return nil
}
