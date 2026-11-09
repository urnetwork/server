package work

// The spent pass (QUOTA-M2-04; DESIGN.md 20.6's Empty balance, plan-QUOTA-small-readings reading 4):
// every SpentPassInterval, a run clears each spent network that again holds an in-window balance with
// room (model.ClearSpentNetworks), so a new grant reopens a network within about a minute. A run that
// stops short of the end hands its cursor to the next, which runs at once. A binary that does not link
// MG2 neither schedules the chain nor, should an older row run, touches anything.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// The chain's own tunables, beside the pass's (model.NetworkQuotaStateSettings).
type QuotaSpentPassWorkSettings struct {
	// Each run's task max time: 2 min, past the pass's pages.
	MaxTime time.Duration
}

func DefaultQuotaSpentPassWorkSettings() *QuotaSpentPassWorkSettings {
	return &QuotaSpentPassWorkSettings{
		MaxTime: 2 * time.Minute,
	}
}

type QuotaSpentPassArgs struct {
	// Where the run starts; nil for the start of the spent networks.
	AfterNetworkId *server.Id `json:"after_network_id,omitempty"`
}

type QuotaSpentPassResult struct {
	model.ClearSpentNetworksResult
}

// Schedules the chain's first run a pass interval from now. A binary that does not link MG2 schedules
// nothing.
func ScheduleQuotaSpentPass(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleQuotaSpentPass(clientSession, tx, &QuotaSpentPassArgs{}, server.NowUtc().Add(model.DefaultNetworkQuotaStateSettings().SpentPassInterval))
}

func scheduleQuotaSpentPass(clientSession *session.ClientSession, tx server.PgTx, args *QuotaSpentPassArgs, runAt time.Time) {
	if !server.SidecarMg2Linked() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		QuotaSpentPass,
		args,
		clientSession,
		task.RunOnce("quota_spent_pass"),
		task.RunAt(runAt),
		task.MaxTime(DefaultQuotaSpentPassWorkSettings().MaxTime),
	)
}

// One run of the pass at real time.
func QuotaSpentPass(
	quotaSpentPass *QuotaSpentPassArgs,
	clientSession *session.ClientSession,
) (*QuotaSpentPassResult, error) {
	if !server.SidecarMg2Linked() {
		return &QuotaSpentPassResult{}, nil
	}
	result, err := model.ClearSpentNetworks(clientSession.Ctx, server.NowUtc(), quotaSpentPass.AfterNetworkId)
	if err != nil {
		return nil, err
	}
	if 0 < result.ClearedCount && glog.V(1) {
		glog.Infof("[quota]spent pass cleared %d of %d spent networks\n", result.ClearedCount, result.LookedCount)
	}
	return &QuotaSpentPassResult{ClearSpentNetworksResult: result}, nil
}

// Schedules the next run: at once from this run's cursor when it stopped short, else a pass interval
// from now, from the start.
func QuotaSpentPassPost(
	quotaSpentPass *QuotaSpentPassArgs,
	quotaSpentPassResult *QuotaSpentPassResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if quotaSpentPassResult.LastNetworkId != nil {
		scheduleQuotaSpentPass(clientSession, tx, &QuotaSpentPassArgs{AfterNetworkId: quotaSpentPassResult.LastNetworkId}, server.NowUtc())
		return nil
	}
	ScheduleQuotaSpentPass(clientSession, tx)
	return nil
}
