package work

// sn_claim_relay_work.go — the claim relay's one task (controller/sn_claim_relay.go).
// It runs in the taskworker for three reasons: a RunOnce chain with the task's
// session lock is exactly one instance at a time across every worker, a pass that
// dies is repeated by the next worker, and the relay's state is the durable
// transaction intents and the ledger, which the taskworker already has. Without a
// relayer key each pass does nothing but say so.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A pass may wait for one claim's finality, which the engine bounds at 8 minutes.
const snClaimRelayMaxTime = 30 * time.Minute

type SnClaimRelayArgs struct {
	DeploymentKey string `json:"deployment_key"`
}

type SnClaimRelayResult struct {
	RunAt time.Time `json:"run_at"`
}

func snClaimRelayRunOnce(deploymentKey string) *task.RunOnceOption {
	return task.RunOnce("sn_claim_relay", deploymentKey)
}

func ScheduleSnClaimRelay(clientSession *session.ClientSession, tx server.PgTx) {
	deploymentKey, _ := controller.StDeploymentKey()
	task.ScheduleTaskInTx(
		tx,
		SnClaimRelay,
		&SnClaimRelayArgs{DeploymentKey: string(deploymentKey)},
		clientSession,
		snClaimRelayRunOnce(string(deploymentKey)),
		task.RunAt(server.NowUtc()),
		task.MaxTime(snClaimRelayMaxTime),
	)
}

func SnClaimRelay(
	snClaimRelay *SnClaimRelayArgs,
	clientSession *session.ClientSession,
) (*SnClaimRelayResult, error) {
	if _, current := stTaskDeploymentCurrent(snClaimRelay.DeploymentKey); !current {
		return &SnClaimRelayResult{}, nil
	}
	result := controller.SnClaimRelayPass(clientSession.Ctx)
	glog.V(1).Infof("[sn]claim relay: %s\n", result)
	return &SnClaimRelayResult{RunAt: server.NowUtc().Add(result.Delay)}, nil
}

func SnClaimRelayPost(
	snClaimRelay *SnClaimRelayArgs,
	snClaimRelayResult *SnClaimRelayResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(snClaimRelay.DeploymentKey); !current {
		return nil
	}
	task.ScheduleTaskInTx(
		tx,
		SnClaimRelay,
		snClaimRelay,
		clientSession,
		snClaimRelayRunOnce(snClaimRelay.DeploymentKey),
		task.RunAt(snClaimRelayResult.RunAt),
		task.MaxTime(snClaimRelayMaxTime),
	)
	return nil
}
