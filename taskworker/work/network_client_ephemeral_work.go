package work

// taskworker's ephemeral job (§18.8, D-SDK-17; TEAM-M1-10): every
// NetworkClientLeaseSettings.ReapInterval a run reaps each ephemeral client with
// no open connection whose lease and newest disconnect are both past the
// threshold (model.ReapEphemeralClients). A run that stops short of the end
// hands its cursor to the next, which runs at once. A binary that does not link
// MG1 neither schedules the chain nor, should an older row run, touches
// anything.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The chain's own tunables, beside the reap's (model.NetworkClientLeaseSettings).
type ReapEphemeralClientsWorkSettings struct {
	// Each run's task max time: past the reap's pass timeout.
	MaxTime time.Duration
}

func DefaultReapEphemeralClientsWorkSettings() *ReapEphemeralClientsWorkSettings {
	return &ReapEphemeralClientsWorkSettings{
		MaxTime: 2 * time.Minute,
	}
}

type ReapEphemeralClientsArgs struct {
	// Where the run starts; nil for the oldest lease.
	After *model.ReapEphemeralClientsCursor `json:"after,omitempty"`
}

type ReapEphemeralClientsResult struct {
	model.ReapEphemeralClientsResult
}

// Schedules the chain's first run a reap interval from now. A binary that does
// not link MG1 schedules nothing.
func ScheduleReapEphemeralClients(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleReapEphemeralClients(clientSession, tx, &ReapEphemeralClientsArgs{}, server.NowUtc().Add(model.DefaultNetworkClientLeaseSettings().ReapInterval))
}

func scheduleReapEphemeralClients(clientSession *session.ClientSession, tx server.PgTx, args *ReapEphemeralClientsArgs, runAt time.Time) {
	if !server.SidecarMg1Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		ReapEphemeralClients,
		args,
		clientSession,
		task.RunOnce("reap_ephemeral_clients"),
		task.RunAt(runAt),
		task.MaxTime(DefaultReapEphemeralClientsWorkSettings().MaxTime),
	)
}

// One run of the reap at real time.
func ReapEphemeralClients(
	reapEphemeralClients *ReapEphemeralClientsArgs,
	clientSession *session.ClientSession,
) (*ReapEphemeralClientsResult, error) {
	if !server.SidecarMg1Linked() {
		return &ReapEphemeralClientsResult{}, nil
	}
	result, err := model.ReapEphemeralClients(
		clientSession.Ctx,
		server.NowUtc(),
		reapEphemeralClients.After,
		model.DefaultNetworkClientLeaseSettings(),
	)
	if err != nil {
		return nil, err
	}
	if 0 < result.ReapedCount+result.ClearedCount && glog.V(1) {
		glog.Infof("[lease]reaped %d and cleared %d of %d leases looked at\n", result.ReapedCount, result.ClearedCount, result.LookedCount)
	}
	return &ReapEphemeralClientsResult{ReapEphemeralClientsResult: *result}, nil
}

// Schedules the next run: at once from this run's cursor when it stopped short,
// else a reap interval from now, from the oldest lease.
func ReapEphemeralClientsPost(
	reapEphemeralClients *ReapEphemeralClientsArgs,
	reapEphemeralClientsResult *ReapEphemeralClientsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if reapEphemeralClientsResult.Cursor != nil {
		scheduleReapEphemeralClients(clientSession, tx, &ReapEphemeralClientsArgs{After: reapEphemeralClientsResult.Cursor}, server.NowUtc())
		return nil
	}
	ScheduleReapEphemeralClients(clientSession, tx)
	return nil
}
