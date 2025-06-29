package work

import (
	"time"

	"github.com/urnetwork/server/v2025"
	"github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/task"

	"github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/session"
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
		task.RunAt(time.Now().Add(model.NetworkClientHandlerHeartbeatTimeout)),
	)
}

func CloseExpiredNetworkClientHandlers(
	closeExpiredNetworkClientHandlers *CloseExpiredNetworkClientHandlersArgs,
	clientSession *session.ClientSession,
) (*CloseExpiredNetworkClientHandlersResult, error) {
	model.CloseExpiredNetworkClientHandlers(clientSession.Ctx, 2*model.NetworkClientHandlerHeartbeatTimeout)
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
	task.ScheduleTaskInTx(
		tx,
		RemoveDisconnectedNetworkClients,
		&RemoveDisconnectedNetworkClientsArgs{},
		clientSession,
		// legacy key
		task.RunOnce("delete_disconnected_network_clients"),
		task.RunAt(time.Now().Add(5*time.Minute)),
	)
}

func RemoveDisconnectedNetworkClients(
	removeDisconnectedNetworkClients *RemoveDisconnectedNetworkClientsArgs,
	clientSession *session.ClientSession,
) (*RemoveDisconnectedNetworkClientsResult, error) {
	minTime := time.Now().Add(-7 * 24 * time.Hour)
	model.RemoveDisconnectedNetworkClients(clientSession.Ctx, minTime)
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

type RemoveLocationLookupResultsArgs struct {
}

type RemoveLocationLookupResultsResult struct {
}

func ScheduleRemoveLocationLookupResults(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveLocationLookupResults,
		&RemoveLocationLookupResultsArgs{},
		clientSession,
		task.RunOnce("remove_lookup_results"),
		task.RunAt(time.Now().Add(30*time.Minute)),
	)
}

func RemoveLocationLookupResults(
	removeLocationLookupResults *RemoveLocationLookupResultsArgs,
	clientSession *session.ClientSession,
) (*RemoveLocationLookupResultsResult, error) {
	minTime := time.Now().Add(-controller.LocationLookupResultExpiration)
	model.RemoveLocationLookupResults(clientSession.Ctx, minTime)
	return &RemoveLocationLookupResultsResult{}, nil
}

func RemoveLocationLookupResultsPost(
	removeLocationLookupResults *RemoveLocationLookupResultsArgs,
	removeLocationLookupResultsResult *RemoveLocationLookupResultsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveLocationLookupResults(clientSession, tx)
	return nil
}

type SetMissingConnectionLocationsArgs struct {
}

type SetMissingConnectionLocationsResult struct {
}

func ScheduleSetMissingConnectionLocations(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		SetMissingConnectionLocations,
		&SetMissingConnectionLocationsArgs{},
		clientSession,
		task.RunOnce("set_missing_connection_locations"),
		task.RunAt(time.Now().Add(30*time.Minute)),
	)
}

func SetMissingConnectionLocations(
	setMissingConnectionLocations *SetMissingConnectionLocationsArgs,
	clientSession *session.ClientSession,
) (*SetMissingConnectionLocationsResult, error) {
	minTime := time.Now().Add(-30 * time.Second)
	controller.SetMissingConnectionLocations(clientSession.Ctx, minTime)
	return &SetMissingConnectionLocationsResult{}, nil
}

func SetMissingConnectionLocationsPost(
	setMissingConnectionLocations *SetMissingConnectionLocationsArgs,
	setMissingConnectionLocationsResult *SetMissingConnectionLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleSetMissingConnectionLocations(clientSession, tx)
	return nil
}
