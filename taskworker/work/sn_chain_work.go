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
//   - SnFleetAuto and SnFleetAutoRelay (every few minutes, only once binding is
//     open): the Top 200 claim's server side (controller/sn_fleet_auto.go).
//
// Each no-ops when st.yml is absent or disabled, and a chain from an earlier
// deployment ends itself.

import (
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	snSyncFleetBindingsInterval = 1 * time.Minute
	snReadIncentiveInterval     = 2 * time.Minute
	snFleetAutoInterval         = 5 * time.Minute
	snFleetAutoRelayInterval    = 2 * time.Minute
)

// SnFleetAuto (periodic, only while binding is open: closed, nothing is scheduled)

type SnFleetAutoArgs struct {
	DeploymentKey string `json:"deployment_key"`
}

type SnFleetAutoResult struct {
}

func ScheduleSnFleetAuto(clientSession *session.ClientSession, tx server.PgTx) {
	deploymentKey, ok := controller.StDeploymentKey()
	if !ok || !controller.StEnabled() || !controller.SnFleetAutoOpen() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SnFleetAuto,
		&SnFleetAutoArgs{DeploymentKey: string(deploymentKey)},
		clientSession,
		task.RunOnce("sn_fleet_auto", deploymentKey),
		task.RunAt(server.NowUtc().Add(snFleetAutoInterval)),
		task.MaxTime(15*time.Minute),
	)
}

func SnFleetAuto(snFleetAuto *SnFleetAutoArgs, clientSession *session.ClientSession) (*SnFleetAutoResult, error) {
	if _, current := stTaskDeploymentCurrent(snFleetAuto.DeploymentKey); !current {
		return &SnFleetAutoResult{}, nil
	}
	controller.SnFleetAutoStep(clientSession.Ctx)
	return &SnFleetAutoResult{}, nil
}

func SnFleetAutoPost(snFleetAuto *SnFleetAutoArgs, snFleetAutoResult *SnFleetAutoResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if _, current := stTaskDeploymentCurrent(snFleetAuto.DeploymentKey); !current {
		return nil
	}
	ScheduleSnFleetAuto(clientSession, tx)
	return nil
}

// SnFleetAutoRelay (periodic, only while binding is open)

type SnFleetAutoRelayArgs struct {
	DeploymentKey string `json:"deployment_key"`
}

type SnFleetAutoRelayResult struct {
}

func ScheduleSnFleetAutoRelay(clientSession *session.ClientSession, tx server.PgTx) {
	deploymentKey, ok := controller.StDeploymentKey()
	if !ok || !controller.StEnabled() || !controller.SnFleetAutoOpen() {
		return
	}
	task.ScheduleTaskInTx(
		tx,
		SnFleetAutoRelay,
		&SnFleetAutoRelayArgs{DeploymentKey: string(deploymentKey)},
		clientSession,
		task.RunOnce("sn_fleet_auto_relay", deploymentKey),
		task.RunAt(server.NowUtc().Add(snFleetAutoRelayInterval)),
		// each transaction waits for finality, one at a time
		task.MaxTime(30*time.Minute),
	)
}

func SnFleetAutoRelay(snFleetAutoRelay *SnFleetAutoRelayArgs, clientSession *session.ClientSession) (*SnFleetAutoRelayResult, error) {
	if _, current := stTaskDeploymentCurrent(snFleetAutoRelay.DeploymentKey); !current {
		return &SnFleetAutoRelayResult{}, nil
	}
	controller.SnFleetAutoRelayStep(clientSession.Ctx)
	return &SnFleetAutoRelayResult{}, nil
}

func SnFleetAutoRelayPost(snFleetAutoRelay *SnFleetAutoRelayArgs, snFleetAutoRelayResult *SnFleetAutoRelayResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if _, current := stTaskDeploymentCurrent(snFleetAutoRelay.DeploymentKey); !current {
		return nil
	}
	ScheduleSnFleetAutoRelay(clientSession, tx)
	return nil
}

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
