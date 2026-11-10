package work

// §7.6's daily domain recheck (TEAM-M6-06): a pass every PassInterval through
// model.RecheckNetworkDomains, which looks up each verified domain whose
// newest check is a day old and drops a domain after 7 failed days. A pass
// that stops at its batch bound hands its cursor to the next, which runs at
// once. A binary that does not link MG6, which holds network_domain,
// schedules nothing.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The chain's tunables, beside the recheck's (model.NetworkDomainSettings).
type NetworkDomainRecheckWorkSettings struct {
	// How often a pass starts: 1 h, against a day between one domain's
	// checks, so a domain is looked up at most an hour late and the day's
	// lookups spread over its passes.
	PassInterval time.Duration
	// Each run's task max time: a full batch of lookups that each time out.
	MaxTime time.Duration
}

func DefaultNetworkDomainRecheckWorkSettings() *NetworkDomainRecheckWorkSettings {
	return &NetworkDomainRecheckWorkSettings{
		PassInterval: time.Hour,
		MaxTime:      2 * time.Hour,
	}
}

type NetworkDomainRecheckArgs struct {
	// Where the run starts; nil for the first row.
	After *model.NetworkDomainCursor `json:"after,omitempty"`
}

type NetworkDomainRecheckResult struct {
	model.NetworkDomainRecheckResult
}

// Schedules the chain's first run a pass interval from now. A binary that
// does not link MG6 schedules nothing.
func ScheduleNetworkDomainRecheck(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleNetworkDomainRecheck(clientSession, tx, &NetworkDomainRecheckArgs{}, server.NowUtc().Add(DefaultNetworkDomainRecheckWorkSettings().PassInterval))
}

func scheduleNetworkDomainRecheck(clientSession *session.ClientSession, tx server.PgTx, args *NetworkDomainRecheckArgs, runAt time.Time) {
	if !server.SidecarMg6Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		NetworkDomainRecheck,
		args,
		clientSession,
		task.RunOnce("network_domain_recheck"),
		task.RunAt(runAt),
		task.MaxTime(DefaultNetworkDomainRecheckWorkSettings().MaxTime),
	)
}

// One recheck pass at real time, through the process's resolver.
func NetworkDomainRecheck(
	networkDomainRecheck *NetworkDomainRecheckArgs,
	clientSession *session.ClientSession,
) (*NetworkDomainRecheckResult, error) {
	result := model.RecheckNetworkDomains(clientSession.Ctx, server.NowUtc(), networkDomainRecheck.After, model.DefaultNetworkDomainSettings())
	if 0 < result.DroppedCount || 0 < result.FailedCount {
		glog.Infof("[domain]recheck looked up %d domains: %d failed, %d rows dropped\n", result.CheckedCount, result.FailedCount, result.DroppedCount)
	}
	return &NetworkDomainRecheckResult{NetworkDomainRecheckResult: *result}, nil
}

// Schedules the next run: at once from this run's cursor when it stopped at
// its bound, else a pass interval from now.
func NetworkDomainRecheckPost(
	networkDomainRecheck *NetworkDomainRecheckArgs,
	networkDomainRecheckResult *NetworkDomainRecheckResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if networkDomainRecheckResult.After != nil {
		scheduleNetworkDomainRecheck(clientSession, tx, &NetworkDomainRecheckArgs{After: networkDomainRecheckResult.After}, server.NowUtc())
		return nil
	}
	ScheduleNetworkDomainRecheck(clientSession, tx)
	return nil
}
