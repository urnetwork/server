package work

import (
	"time"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/task"
	// "bringyour.com/bringyour/controller"
	"bringyour.com/bringyour/session"
)

type CloseExpiredNetworkClientHandlersArgs struct {
}

type CloseExpiredNetworkClientHandlersResult struct {
}

func ScheduleCloseExpiredNetworkClientHandlers(clientSession *session.ClientSession, tx bringyour.PgTx) {
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
	tx bringyour.PgTx,
) error {
	ScheduleCloseExpiredNetworkClientHandlers(clientSession, tx)
	return nil
}

type DeleteDisconnectedNetworkClientsArgs struct {
}

type DeleteDisconnectedNetworkClientsResult struct {
}

func ScheduleDeleteDisconnectedNetworkClients(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		DeleteDisconnectedNetworkClients,
		&DeleteDisconnectedNetworkClientsArgs{},
		clientSession,
		task.RunOnce("delete_disconnected_network_clients"),
		task.RunAt(time.Now().Add(1*time.Hour)),
	)
}

func DeleteDisconnectedNetworkClients(
	deleteDisconnectedNetworkClients *DeleteDisconnectedNetworkClientsArgs,
	clientSession *session.ClientSession,
) (*DeleteDisconnectedNetworkClientsResult, error) {
	// keep disconnected records around for a little while to help debug
	model.DeleteDisconnectedNetworkClients(clientSession.Ctx, 24*time.Hour)
	return &DeleteDisconnectedNetworkClientsResult{}, nil
}

func DeleteDisconnectedNetworkClientsPost(
	deleteDisconnectedNetworkClients *DeleteDisconnectedNetworkClientsArgs,
	deleteDisconnectedNetworkClientsResult *DeleteDisconnectedNetworkClientsResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	ScheduleDeleteDisconnectedNetworkClients(clientSession, tx)
	return nil
}