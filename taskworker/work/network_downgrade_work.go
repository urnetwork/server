package work

// §20.7's downgrade job (QUOTA-M6-05; D-QUOTA-6, FS-51), production-only as the other quota chains are, one
// once-keyed chain that reschedules itself:
//
//	ApplyNetworkDowngrades   daily at Offset past 00:00 UTC: a page of the networks a downgrade's effects could
//	                         touch, from its cursor, each brought to where it stands on the downgrade clock
//	                         (model.ApplyNetworkDowngrades). A page that came back full hands its cursor to the
//	                         next run, which runs at once
//
// Every effect converges, so a run killed anywhere is finished by the next. The job reads each network's published
// names from the source this process installs over its gate cache (tls_published_names.go); in a process with
// none, a network whose kills need them is counted as not finished and nothing of its kills changes. Not
// scheduled, and each run returns at once, in a binary that does not link MG2, MG5 and MG6.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type NetworkDowngradeWorkSettings struct {
	// When in each UTC day the job starts: 03:00, after the day's rollup and the nightly reconcile.
	Offset time.Duration
	// Each run's task max time: a page's networks, each a few reads and at most a transaction an effect.
	MaxTime time.Duration
}

func DefaultNetworkDowngradeWorkSettings() *NetworkDowngradeWorkSettings {
	return &NetworkDowngradeWorkSettings{
		Offset:  3 * time.Hour,
		MaxTime: 30 * time.Minute,
	}
}

func networkDowngradeWorkLinked() bool {
	return server.SidecarMg2Linked() && server.SidecarMg5Linked() && server.SidecarMg6Linked()
}

type ApplyNetworkDowngradesArgs struct {
	// Where the run starts; nil for the first network.
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type ApplyNetworkDowngradesResult struct {
	model.ApplyNetworkDowngradesResult
}

// Schedules the next day's job. A binary that does not link MG2, MG5 and MG6 schedules nothing.
func ScheduleApplyNetworkDowngrades(clientSession *session.ClientSession, tx server.PgTx) {
	settings := DefaultNetworkDowngradeWorkSettings()
	scheduleApplyNetworkDowngrades(clientSession, tx, &ApplyNetworkDowngradesArgs{}, nextUtcDayOffset(server.NowUtc(), settings.Offset))
}

func scheduleApplyNetworkDowngrades(clientSession *session.ClientSession, tx server.PgTx, args *ApplyNetworkDowngradesArgs, runAt time.Time) {
	if !networkDowngradeWorkLinked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		ApplyNetworkDowngrades,
		args,
		clientSession,
		task.RunOnce("quota_apply_network_downgrades"),
		task.RunAt(runAt),
		task.MaxTime(DefaultNetworkDowngradeWorkSettings().MaxTime),
	)
}

func ApplyNetworkDowngrades(args *ApplyNetworkDowngradesArgs, clientSession *session.ClientSession) (*ApplyNetworkDowngradesResult, error) {
	if !networkDowngradeWorkLinked() {
		return &ApplyNetworkDowngradesResult{}, nil
	}
	result, err := model.ApplyNetworkDowngrades(clientSession.Ctx, server.NowUtc(), args.AfterNetworkId)
	if err != nil {
		return nil, err
	}
	if glog.V(1) {
		glog.Infof(
			"[quota]downgrades looked at %d networks: dns %d withheld %d released, custom hosts %d withheld %d released, tls %d suspended %d lifted, storage %d corrected, %d not finished\n",
			result.LookedCount,
			result.DnsWithheldCount,
			result.DnsReleasedCount,
			result.CustomHostsWithheldCount,
			result.CustomHostsReleasedCount,
			result.TlsSuspendedCount,
			result.TlsLiftedCount,
			result.StorageOverCorrected,
			result.ErrorCount,
		)
	}
	return &ApplyNetworkDowngradesResult{ApplyNetworkDowngradesResult: result}, nil
}

// The next page at once from the run's cursor when it stopped short, else the next day's job.
func ApplyNetworkDowngradesPost(args *ApplyNetworkDowngradesArgs, result *ApplyNetworkDowngradesResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if result.LastNetworkId != nil {
		scheduleApplyNetworkDowngrades(clientSession, tx, &ApplyNetworkDowngradesArgs{AfterNetworkId: result.LastNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleApplyNetworkDowngrades(clientSession, tx)
	return nil
}
