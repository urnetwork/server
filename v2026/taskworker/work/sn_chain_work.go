package work

// sn_chain_work.go — the SN top board's chain-side readers (LEADERTOP.md
// §5.7, §6.2), each a periodic RunOnce chain per st deployment like
// StSyncChain:
//
//   - SnSyncFleetBindings (about every minute): applies the coordinator's
//     finalized FleetBound, FleetBindingRevoked and FleetBindingCleaned logs
//     to the live binding census (controller/sn_fleet_binding.go), then
//     refreshes the verify cohort's priority set of live claimed-fleet
//     members and the claimed fleet counts.
//   - SnReadIncentive (about every two minutes): reads the on-chain incentive
//     census once per native tempo (controller/sn_incentive.go). Most runs
//     make no RPC at all; the reader decides.
//
// Both no-op when st.yml is absent or disabled, and a chain from an earlier
// deployment ends itself.

import (
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

const (
	snSyncFleetBindingsInterval = 1 * time.Minute
	snReadIncentiveInterval     = 2 * time.Minute
)

// SnSyncFleetBindings (periodic)

type SnSyncFleetBindingsArgs struct {
	DeploymentKey string `json:"deployment_key"`
}

type SnSyncFleetBindingsResult struct {
}

func ScheduleSnSyncFleetBindings(clientSession *session.ClientSession, tx server.PgTx) {
	deploymentKey, ok := controller.StDeploymentKey()
	if !ok || !controller.StEnabled() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SnSyncFleetBindings,
		&SnSyncFleetBindingsArgs{DeploymentKey: string(deploymentKey)},
		clientSession,
		task.RunOnce("sn_sync_fleet_bindings", deploymentKey),
		task.RunAt(server.NowUtc().Add(snSyncFleetBindingsInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func SnSyncFleetBindings(
	snSyncFleetBindings *SnSyncFleetBindingsArgs,
	clientSession *session.ClientSession,
) (*SnSyncFleetBindingsResult, error) {
	if _, current := stTaskDeploymentCurrent(snSyncFleetBindings.DeploymentKey); !current {
		return &SnSyncFleetBindingsResult{}, nil
	}
	ctx := clientSession.Ctx
	result, err := controller.SnSyncFleetBindings(ctx)
	if err != nil {
		glog.Infof("[sn]fleet binding sync failed: %s\n", err)
	} else if 0 < result.Applied {
		glog.Infof("[sn]fleet binding sync applied %d logs through block %d\n", result.Applied, result.NextBlock-1)
	}
	controller.RefreshSnClaimedFleetsNow(ctx)
	return &SnSyncFleetBindingsResult{}, nil
}

func SnSyncFleetBindingsPost(
	snSyncFleetBindings *SnSyncFleetBindingsArgs,
	snSyncFleetBindingsResult *SnSyncFleetBindingsResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(snSyncFleetBindings.DeploymentKey); !current {
		return nil
	}
	ScheduleSnSyncFleetBindings(clientSession, tx)
	return nil
}

// SnReadIncentive (periodic)

type SnReadIncentiveArgs struct {
	DeploymentKey string `json:"deployment_key"`
}

type SnReadIncentiveResult struct {
}

func ScheduleSnReadIncentive(clientSession *session.ClientSession, tx server.PgTx) {
	deploymentKey, ok := controller.StDeploymentKey()
	if !ok || !controller.StEnabled() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SnReadIncentive,
		&SnReadIncentiveArgs{DeploymentKey: string(deploymentKey)},
		clientSession,
		task.RunOnce("sn_read_incentive", deploymentKey),
		task.RunAt(server.NowUtc().Add(snReadIncentiveInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func SnReadIncentive(
	snReadIncentive *SnReadIncentiveArgs,
	clientSession *session.ClientSession,
) (*SnReadIncentiveResult, error) {
	if _, current := stTaskDeploymentCurrent(snReadIncentive.DeploymentKey); !current {
		return &SnReadIncentiveResult{}, nil
	}
	result, err := controller.SnReadIncentive(clientSession.Ctx)
	if err != nil {
		glog.Infof("[sn]incentive read failed: %s\n", err)
	} else if result.Snapshot != nil {
		glog.Infof("[sn]incentive census for tempo %d at block %d (stale %t %v)\n", result.Snapshot.SubnetEpochIndex, result.Snapshot.Block, result.Snapshot.Stale, result.Snapshot.StaleReasons)
	}
	return &SnReadIncentiveResult{}, nil
}

func SnReadIncentivePost(
	snReadIncentive *SnReadIncentiveArgs,
	snReadIncentiveResult *SnReadIncentiveResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if _, current := stTaskDeploymentCurrent(snReadIncentive.DeploymentKey); !current {
		return nil
	}
	ScheduleSnReadIncentive(clientSession, tx)
	return nil
}
