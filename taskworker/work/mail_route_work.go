package work

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §15.15's route projection pass (MAIL-M7-22): each run projects the networks
// whose label map entry changed, so a suspension, an alias change or a
// deletion reaches mxedge's {mxr_} keys within one run, and one page of every
// mail network, so a lost Redis is whole again within a round
// (model.ProjectMailRoutesPass). The cursor rides in the task's arguments, so
// a run that failed starts where it stood. A binary that does not link MG7
// neither schedules the chain nor, should an older row run, touches anything.

// Cadences of the chain.
type MailRouteProjectionSettings struct {
	// The chain runs again this long after a run: how long a label map change
	// waits for its routes.
	RunInterval time.Duration
	// After a run that failed, Redis or Postgres being away.
	RetryInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

// Every 15 seconds, so a suspension closes a network's mail within the
// minute §12.9 gives its records.
func DefaultMailRouteProjectionSettings() *MailRouteProjectionSettings {
	return &MailRouteProjectionSettings{
		RunInterval:   15 * time.Second,
		RetryInterval: 30 * time.Second,
		MaxTime:       5 * time.Minute,
	}
}

type MailRouteProjectionArgs struct {
	// Where the last run stood; nil reads everything.
	Cursor *model.MailRouteCursor `json:"cursor,omitempty"`
}

type MailRouteProjectionResult struct {
	Pass *model.MailRoutePass `json:"pass,omitempty"`
	// The run failed and its cursor did not move.
	Failed bool `json:"failed"`
}

// Schedules the chain's first run. A binary that does not link MG7 schedules
// nothing.
func ScheduleMailRouteProjection(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleMailRouteProjection(clientSession, tx, nil, server.NowUtc().Add(DefaultMailRouteProjectionSettings().RunInterval))
}

func scheduleMailRouteProjection(clientSession *session.ClientSession, tx server.PgTx, cursor *model.MailRouteCursor, runAt time.Time) {
	if !server.SidecarMg7Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		MailRouteProjection,
		&MailRouteProjectionArgs{Cursor: cursor},
		clientSession,
		task.RunOnce("mail_route_projection"),
		task.RunAt(runAt),
		task.MaxTime(DefaultMailRouteProjectionSettings().MaxTime),
	)
}

// One run. A failure is the run's result, never the task's error, so the chain
// goes on from the cursor it had; a process with no dns root projects nothing.
func MailRouteProjection(
	args *MailRouteProjectionArgs,
	clientSession *session.ClientSession,
) (*MailRouteProjectionResult, error) {
	root := model.DnsUserRoot()
	if !server.SidecarMg7Linked() || root == "" {
		return &MailRouteProjectionResult{}, nil
	}
	pass, err := model.ProjectMailRoutesPass(clientSession.Ctx, root, args.Cursor)
	if err != nil {
		glog.Warningf("[mailroute]the pass stopped and runs again from its cursor: %v\n", err)
		return &MailRouteProjectionResult{Failed: true}, nil
	}
	if pass.Rebuilt {
		glog.Infof("[mailroute]redis held no routes, rebuilding\n")
	}
	return &MailRouteProjectionResult{Pass: pass}, nil
}

// Schedules the next run from where this one stood, or from where it started
// when it failed.
func MailRouteProjectionPost(
	args *MailRouteProjectionArgs,
	result *MailRouteProjectionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := DefaultMailRouteProjectionSettings()
	cursor := args.Cursor
	runAt := server.NowUtc().Add(settings.RunInterval)
	switch {
	case result.Failed:
		runAt = server.NowUtc().Add(settings.RetryInterval)
	case result.Pass != nil:
		cursor = result.Pass.Cursor
	}
	scheduleMailRouteProjection(clientSession, tx, cursor, runAt)
	return nil
}
