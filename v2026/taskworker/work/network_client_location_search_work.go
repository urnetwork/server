package work

import (
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/task"

	// "github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
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
	cityLimit := 0
	model.AddDefaultLocations(clientSession.Ctx, cityLimit)

	// the location searches' in-memory indexes change, in the transaction's
	// order, once it has committed. Unchanged values are skipped, so this is
	// short; a full re-index (a changed search format) rewrites every value in
	// this one transaction and can outlast the application role's 15-minute
	// transaction bound, which it lifts for itself.
	var posts []server.PostFunction
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		server.SetTransactionTimeoutInTx(clientSession.Ctx, tx, 0)
		posts = model.IndexSearchLocationsInTx(clientSession.Ctx, tx)
	})
	server.RunPosts(clientSession.Ctx, server.SequencePosts(posts...))
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
