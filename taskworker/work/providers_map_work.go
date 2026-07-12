package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type ExportProvidersMapArgs struct {
}

type ExportProvidersMapResult struct {
}

// how often the /stats/providers-map blob is refreshed. This is light — it reads
// the already-aggregated client-location structure, not the primary db — so
// unlike ExportStats it runs on a normal cadence.
const exportProvidersMapInterval = 5 * time.Minute

func ScheduleExportProvidersMap(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		ExportProvidersMap,
		&ExportProvidersMapArgs{},
		clientSession,
		task.RunOnce("export_providers_map"),
		task.RunAt(server.NowUtc().Add(exportProvidersMapInterval)),
	)
}

func ExportProvidersMap(
	exportProvidersMap *ExportProvidersMapArgs,
	clientSession *session.ClientSession,
) (*ExportProvidersMapResult, error) {
	if err := model.ExportProvidersMap(clientSession.Ctx); err != nil {
		return nil, err
	}
	return &ExportProvidersMapResult{}, nil
}

func ExportProvidersMapPost(
	exportProvidersMap *ExportProvidersMapArgs,
	exportProvidersMapResult *ExportProvidersMapResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleExportProvidersMap(clientSession, tx)
	return nil
}
