package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/task"

	// "github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
)

type IndexSearchLocationsArgs struct {
}

type IndexSearchLocationsResult struct {
}

func ScheduleIndexSearchLocations(
	clientSession *session.ClientSession,
	tx server.PgTx,
) {
	task.ScheduleTaskInTx(
		tx,
		IndexSearchLocations,
		&IndexSearchLocationsArgs{},
		clientSession,
		task.RunOnce("index_search_locations"),
		task.MaxTime(4*time.Hour),
	)
}

func IndexSearchLocations(
	indexSearchLocations *IndexSearchLocationsArgs,
	clientSession *session.ClientSession,
) (*IndexSearchLocationsResult, error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		model.IndexSearchLocationsInTx(clientSession.Ctx, tx)
	})
	return &IndexSearchLocationsResult{}, nil
}

func IndexSearchLocationsPost(
	indexSearchLocations *IndexSearchLocationsArgs,
	indexSearchLocationsResult *IndexSearchLocationsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	// do nothing
	return nil
}
