package work

// §7.9's log jobs (TEAM-M2-14): retention, a pass every RetentionInterval
// through the process's sequencer (model.RetainConfigLogs), a run that stops
// short of the end handing its cursor to the next, which runs at once; and
// each network's tick, model.AppendConfigLogTickTask, which reaps schedule. A
// binary that does not link MG2, whose network_acl_head lists the networks
// with a log, schedules no retention; a process with no sequencer retries.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The chain's tunables, beside retention's (model.ConfigLogRetentionSettings).
type ConfigLogRetentionWorkSettings struct {
	// How often a pass starts: 1 h, against a 30-day keep, so nothing is
	// kept past its time by more than an hour of passes.
	RetentionInterval time.Duration
	// Each run's task max time.
	MaxTime time.Duration
}

func DefaultConfigLogRetentionWorkSettings() *ConfigLogRetentionWorkSettings {
	return &ConfigLogRetentionWorkSettings{
		RetentionInterval: time.Hour,
		MaxTime:           10 * time.Minute,
	}
}

type ConfigLogRetentionArgs struct {
	// Where the run starts; nil for the first network.
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type ConfigLogRetentionResult struct {
	model.ConfigLogRetentionResult
}

// Schedules the chain's first run a retention interval from now. A binary that
// does not link MG2 schedules nothing.
func ScheduleConfigLogRetention(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleConfigLogRetention(clientSession, tx, &ConfigLogRetentionArgs{}, server.NowUtc().Add(DefaultConfigLogRetentionWorkSettings().RetentionInterval))
}

func scheduleConfigLogRetention(clientSession *session.ClientSession, tx server.PgTx, args *ConfigLogRetentionArgs, runAt time.Time) {
	if !server.SidecarMg2Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		ConfigLogRetention,
		args,
		clientSession,
		task.RunOnce("config_log_retention"),
		task.RunAt(runAt),
		task.MaxTime(DefaultConfigLogRetentionWorkSettings().MaxTime),
	)
}

// One retention pass at real time. A process with no sequencer retries an
// interval on, deleting nothing.
func ConfigLogRetention(
	configLogRetention *ConfigLogRetentionArgs,
	clientSession *session.ClientSession,
) (*ConfigLogRetentionResult, error) {
	if !server.SidecarMg2Linked() {
		return &ConfigLogRetentionResult{}, nil
	}
	result, err := model.RetainConfigLogs(clientSession.Ctx, server.NowUtc(), configLogRetention.AfterNetworkId, model.DefaultConfigLogRetentionSettings())
	if err != nil {
		return nil, task.WithRetryDelay(err, DefaultConfigLogRetentionWorkSettings().RetentionInterval)
	}
	if 0 < result.DeletedCount && glog.V(1) {
		glog.Infof("[config]retention deleted %d objects of %d networks\n", result.DeletedCount, result.NetworkCount)
	}
	return &ConfigLogRetentionResult{ConfigLogRetentionResult: *result}, nil
}

// Schedules the next run: at once from this run's cursor when it stopped short,
// else a retention interval from now.
func ConfigLogRetentionPost(
	configLogRetention *ConfigLogRetentionArgs,
	configLogRetentionResult *ConfigLogRetentionResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if configLogRetentionResult.AfterNetworkId != nil {
		scheduleConfigLogRetention(clientSession, tx, &ConfigLogRetentionArgs{AfterNetworkId: configLogRetentionResult.AfterNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleConfigLogRetention(clientSession, tx)
	return nil
}
