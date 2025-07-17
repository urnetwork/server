package work

import (
	"time"

	"github.com/urnetwork/server"
	// "github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/api/handlers"
	"github.com/urnetwork/server/session"
)

type WarmNetworkGetProviderLocationsArgs struct {
}

type WarmNetworkGetProviderLocationsResult struct {
}

func ScheduleWarmNetworkGetProviderLocations(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		WarmNetworkGetProviderLocations,
		&WarmNetworkGetProviderLocationsArgs{},
		clientSession,
		task.RunOnce("api_warm_network_get_provider_locations"),
		task.RunAt(server.NowUtc().Add(1*time.Second)),
		task.Priority(task.TaskPriorityFastest),
	)
}

func WarmNetworkGetProviderLocations(
	warmNetworkGetProviderLocations *WarmNetworkGetProviderLocationsArgs,
	clientSession *session.ClientSession,
) (*WarmNetworkGetProviderLocationsResult, error) {
	handlers.WarmNetworkGetProviderLocations(clientSession)
	return &WarmNetworkGetProviderLocationsResult{}, nil
}

func WarmNetworkGetProviderLocationsPost(
	warmNetworkGetProviderLocations *WarmNetworkGetProviderLocationsArgs,
	warmNetworkGetProviderLocationsResult *WarmNetworkGetProviderLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleWarmNetworkGetProviderLocations(clientSession, tx)
	return nil
}
