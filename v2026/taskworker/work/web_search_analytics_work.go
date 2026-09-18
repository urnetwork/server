package work

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

type WebSearchAnalyticsArgs struct{}

type WebSearchAnalyticsResult struct {
	ProvidersAttempted int `json:"providers_attempted"`
	ProvidersSkipped   int `json:"providers_skipped"`
	RowsAccepted       int `json:"rows_accepted"`
	RowsRejected       int `json:"rows_rejected"`
	RowsUnchanged      int `json:"rows_unchanged"`
	RowsRemoved        int `json:"rows_removed"`
}

func scheduleWebSearchAnalytics(clientSession *session.ClientSession, tx server.PgTx, initial bool) {
	config, err := model.LoadAnalyticsConfig()
	if err != nil {
		glog.Infof("[analytics]invalid configuration; search analytics task not scheduled (%s)\n", err)
		return
	}
	if !config.Enabled || !config.Search.Enabled {
		return
	}
	delay := config.ScheduleInterval()
	if initial {
		delay = time.Minute
	}
	task.ScheduleTaskInTx(
		tx,
		WebSearchAnalytics,
		&WebSearchAnalyticsArgs{},
		clientSession,
		task.RunOnce("web_search_analytics"),
		task.RunAt(server.NowUtc().Add(delay)),
		task.MaxTime(55*time.Minute),
	)
}

func ScheduleWebSearchAnalytics(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleWebSearchAnalytics(clientSession, tx, true)
}

func WebSearchAnalytics(
	args *WebSearchAnalyticsArgs,
	clientSession *session.ClientSession,
) (*WebSearchAnalyticsResult, error) {
	result, err := controller.RunWebSearchAnalytics(clientSession.Ctx, server.NowUtc())
	if err != nil {
		return nil, err
	}
	return &WebSearchAnalyticsResult{
		ProvidersAttempted: result.ProvidersAttempted,
		ProvidersSkipped:   result.ProvidersSkipped,
		RowsAccepted:       result.RowsAccepted,
		RowsRejected:       result.RowsRejected,
		RowsUnchanged:      result.RowsUnchanged,
		RowsRemoved:        result.RowsRemoved,
	}, nil
}

func WebSearchAnalyticsPost(
	args *WebSearchAnalyticsArgs,
	result *WebSearchAnalyticsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleWebSearchAnalytics(clientSession, tx, false)
	return nil
}
