package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
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
	// reaped `NetworkClientReapAfterDeactivate` after deactivation, since
	// provisioned child clients (e.g. proxy devices) cannot recover from a
	// reaped client_id; abandoned top-level clients are marked inactive after
	// `TopLevelClientIdleExpiration` unseen (auth_time).
	//
	// Both windows are named rather than restated here: this comment said "90
	// days" for three weeks after TopLevelClientIdleExpiration was tightened
	// to 30 (2026-07-18), and that stale number was still being quoted as the
	// retention answer. The constants carry their own rationale.
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
// catches orphans from other deletion paths or older releases.
//
// A pass is far too big for one run: provide_key alone is ~1.1B keys on Main,
// about 7 hours of slices. The pass is therefore spread the way
// SweepOrphanContractData's is: each run pages a bounded
// sweepOrphanNetworkClientMaxRowCount rows and returns where it stopped, Post
// hands that cursor to the next run, and a full pass completes over days.
//
// The row budget is the load-bearing part. Before, a run paged until its 4-hour
// deadline canceled it, so Post never ran, the weekly cadence was never re-armed,
// and each error retry restarted the pass at row zero. On 2026-10-11 the task was
// on its 46th claim, re-walking the same first half of provide_key around the
// clock for 8% of all buffer reads. Keep the budget well under MaxTime so a run
// always completes.

type SweepOrphanNetworkClientDataArgs struct {
	// where the previous run stopped; the zero value starts a fresh pass
	Cursor model.SweepOrphanCursor `json:"cursor"`
}

type SweepOrphanNetworkClientDataResult struct {
	RemovedCount int64 `json:"removed_count"`
	// where this run stopped, handed back by Post as the next run's start
	Cursor model.SweepOrphanCursor `json:"cursor"`
	// every table was fully paged, so the next run starts a fresh pass
	Done bool `json:"done"`
}

const (
	sweepOrphanNetworkClientSliceSize = 50000
	// rows paged per run: 200 slices, about 4 minutes of provide_key slices on
	// Main, so a ~1.14B-row pass lands over a few days of resume runs
	sweepOrphanNetworkClientMaxRowCount = 10 * 1000 * 1000
	// gap between the resume runs of one pass
	sweepOrphanNetworkClientResumeTimeout = 30 * time.Minute
	// bounds a stranded claim if a worker dies mid-run; with the row budget a
	// run is minutes even at the slowest measured slice, so this is headroom
	sweepOrphanNetworkClientMaxTime = 1 * time.Hour
)

// ScheduleSweepOrphanNetworkClientData starts a fresh pass at the next weekly slot.
func ScheduleSweepOrphanNetworkClientData(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleSweepOrphanNetworkClientData(
		clientSession,
		tx,
		model.SweepOrphanCursor{},
		// weekly, anchored off-peak (~10:00 UTC): steady state finds ~zero
		// orphans, so a weekly pass is plenty, and `bringyourctl db
		// sweep-orphans` covers on-demand cleanup
		nextWeeklyOffPeak(server.NowUtc()),
	)
}

func scheduleSweepOrphanNetworkClientData(
	clientSession *session.ClientSession,
	tx server.PgTx,
	cursor model.SweepOrphanCursor,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		SweepOrphanNetworkClientData,
		&SweepOrphanNetworkClientDataArgs{
			Cursor: cursor,
		},
		clientSession,
		task.RunOnce("sweep_orphan_network_client_data"),
		task.RunAt(runAt),
		task.MaxTime(sweepOrphanNetworkClientMaxTime),
	)
}

func SweepOrphanNetworkClientData(
	sweepOrphanNetworkClientData *SweepOrphanNetworkClientDataArgs,
	clientSession *session.ClientSession,
) (*SweepOrphanNetworkClientDataResult, error) {
	// the model fn pages each child table by primary key in sliceSize batches,
	// one maintenance tx per slice -- unlike the pre-2026-07-14
	// NOT EXISTS ... LIMIT form, which full-scanned each driver table when
	// orphans were rare (prod incident 2026-07-14)
	removedCount, cursor, done := model.SweepOrphanNetworkClientDataFrom(
		clientSession.Ctx,
		sweepOrphanNetworkClientData.Cursor,
		sweepOrphanNetworkClientMaxRowCount,
		sweepOrphanNetworkClientSliceSize,
	)
	return &SweepOrphanNetworkClientDataResult{
		RemovedCount: removedCount,
		Cursor:       cursor,
		Done:         done,
	}, nil
}

func SweepOrphanNetworkClientDataPost(
	sweepOrphanNetworkClientData *SweepOrphanNetworkClientDataArgs,
	sweepOrphanNetworkClientDataResult *SweepOrphanNetworkClientDataResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if sweepOrphanNetworkClientDataResult.Done {
		// pass complete; start the next one on the weekly cadence
		ScheduleSweepOrphanNetworkClientData(clientSession, tx)
		return nil
	}
	// mid-pass: resume from where this run stopped
	scheduleSweepOrphanNetworkClientData(
		clientSession,
		tx,
		sweepOrphanNetworkClientDataResult.Cursor,
		server.NowUtc().Add(sweepOrphanNetworkClientResumeTimeout),
	)
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
