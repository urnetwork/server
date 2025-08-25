package work

import (
	"time"

	"github.com/urnetwork/server/v2025"
	// "github.com/urnetwork/server/v2025/model"
	"github.com/urnetwork/server/v2025/task"

	// "github.com/urnetwork/server/v2025/controller"
	"github.com/urnetwork/server/v2025/api/handlers"
	"github.com/urnetwork/server/v2025/session"
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
		task.RunAt(server.NowUtc().Add(5*time.Second)),
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
