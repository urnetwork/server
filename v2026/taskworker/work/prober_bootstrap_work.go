package work

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// ProberBootstrapTimeout bounds crash recovery for disposable shard accounts.
// Each live shard creates its own credit and credential at execution admission.
const ProberBootstrapTimeout = 5 * time.Minute

// Shards one pass fences or settles. Each settle is a few indexed reads; a
// reported contract adds one settlement.
const proberBootstrapSettleLimit = 64

type ProberBootstrapArgs struct{}

type ProberBootstrapResult struct{}

// ScheduleProberBootstrap starts crash recovery immediately at deployment.
func ScheduleProberBootstrap(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleProberBootstrapAt(clientSession, tx, server.NowUtc())
}

func scheduleProberBootstrapAt(clientSession *session.ClientSession, tx server.PgTx, runAt time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ProberBootstrap,
		&ProberBootstrapArgs{},
		clientSession,
		task.RunOnce("prober_bootstrap"),
		task.RunAt(runAt),
	)
}

// ProberBootstrap retains the deployed recurring task name for crash cleanup.
// The legacy singleton remains readable while pre-upgrade work drains, but
// this task no longer creates, refreshes, or replenishes shared probe accounts.
// It fences live shards past their deadline and settles drained ones, and
// deletes nothing: a shard stays until its generation drops
// (RemoveProberShardGenerations).
func ProberBootstrap(
	_ *ProberBootstrapArgs,
	clientSession *session.ClientSession,
) (*ProberBootstrapResult, error) {
	settled, err := model.SettleDueProberShards(clientSession.Ctx, proberBootstrapSettleLimit)
	if err != nil {
		return nil, err
	}

	if settled > 0 {
		glog.Infof("[proberboot]settled %d probe shards\n", settled)
	}

	return &ProberBootstrapResult{}, nil
}

// ProberBootstrapPost keeps durable crash cleanup scheduled after each pass.
func ProberBootstrapPost(
	_ *ProberBootstrapArgs,
	_ *ProberBootstrapResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleProberBootstrapAt(clientSession, tx, server.NowUtc().Add(ProberBootstrapTimeout))
	return nil
}
