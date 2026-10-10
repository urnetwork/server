// Session expiry, index repair, and revocation recovery have bounded auth work.
package work

import (
	"context"
	"errors"
	"fmt"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
	"time"
)

// A shard reschedules itself every 30 seconds. One persistently failing
// network or operation keeps its error and count, but the shard still retries
// at that cadence instead of the hour-long task cap.
const maintainNetworkSessionsErrorRetryCap = 30 * time.Second

// The registered shard target keeps its function name; only failure delay is capped.
func NewMaintainNetworkSessionsTaskTarget() task.Target {
	return task.WithErrorRetryCap(task.NewTaskTargetWithPost(MaintainNetworkSessions, MaintainNetworkSessionsPost), maintainNetworkSessionsErrorRetryCap)
}

type MaintainNetworkSessionsArgs struct {
	Shard int `json:"shard"`
}
type MaintainNetworkSessionsResult struct {
	Reviewed int `json:"reviewed"`
}

func scheduleNetworkSessionShard(clientSession *session.ClientSession, tx server.PgTx, shard int) {
	task.ScheduleTaskInTx(tx, MaintainNetworkSessions, &MaintainNetworkSessionsArgs{Shard: shard}, clientSession, task.RunOnce(fmt.Sprintf("maintain_network_sessions_%02d", shard)), task.RunAt(server.NowUtc().Add(30*time.Second)))
}
func ScheduleMaintainNetworkSessions(clientSession *session.ClientSession, tx server.PgTx) {
	for shard := 0; shard < session.SessionIndexShards; shard++ {
		scheduleNetworkSessionShard(clientSession, tx, shard)
	}
}
func MaintainNetworkSessions(args *MaintainNetworkSessionsArgs, clientSession *session.ClientSession) (*MaintainNetworkSessionsResult, error) {
	ctx, cancel := context.WithTimeout(clientSession.Ctx, 5*time.Second)
	defer cancel()
	if args.Shard != 0 {
		reviewed, err := session.SweepSessionIndexShard(ctx, args.Shard, 32, server.NowUtc())
		return &MaintainNetworkSessionsResult{Reviewed: reviewed}, err
	}
	return runNetworkSessionMaintenance(ctx,
		func(ctx context.Context) (int, error) {
			return session.SweepSessionIndexShard(ctx, 0, 32, server.NowUtc())
		},
		func(ctx context.Context) error { return session.ExpireSessionReceipts(ctx, server.NowUtc(), 128) },
		func(ctx context.Context) error { return session.RepairSessionIndexes(ctx, 64) },
		func(ctx context.Context) error { return model.RecoverSessionOperations(ctx, 32) },
		func(ctx context.Context) error {
			observationCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			_ = session.ObserveSessionMaintenance(observationCtx, server.NowUtc())
			return nil
		},
	)
}

// Expiry always gets the first budget. Each recovery owner checkpoints its own
// progress, so a blocked stage must not consume every later owner's budget.
func runNetworkSessionMaintenance(ctx context.Context, sweep func(context.Context) (int, error), stages ...func(context.Context) error) (*MaintainNetworkSessionsResult, error) {
	sweepCtx, cancel := context.WithTimeout(ctx, time.Second)
	reviewed, sweepErr := sweep(sweepCtx)
	cancel()
	errs := []error{sweepErr}
	for _, stage := range stages {
		stageCtx, cancel := context.WithTimeout(ctx, time.Second)
		errs = append(errs, stage(stageCtx))
		cancel()
	}
	return &MaintainNetworkSessionsResult{Reviewed: reviewed}, errors.Join(errs...)
}
func MaintainNetworkSessionsPost(args *MaintainNetworkSessionsArgs, _ *MaintainNetworkSessionsResult, clientSession *session.ClientSession, tx server.PgTx) error {
	scheduleNetworkSessionShard(clientSession, tx, args.Shard)
	return nil
}
