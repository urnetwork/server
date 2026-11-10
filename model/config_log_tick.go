package model

// §7.9's ticks: api's own facts, appended as kind TICK at most once a minute per
// network (D-TEAM-19), each checked by every reader by content.
//
//	reaps        a reap of an ephemeral client (network_client_lease_model.go)
//	             schedules its network's tick task in the reap's transaction,
//	             one run-once task a network, so the reaps of a minute
//	             coalesce into one tick; the task names every live ephemeral
//	             admission at the tip whose sidecar's clients are all inactive,
//	             which also catches a reap whose tick a crash lost
//	activations  a recovery window's end, by a run-once task at T_p + 72 h,
//	             checked against state when it runs: not built, since which
//	             proposals are open and where their windows start is
//	             DESIGN-DETAILS.md section 5's ProposalRow (TEAM-M2-05), held
//	             for the core lane (question team-m1-04-activation-ticks); a
//	             tick naming one is refused here and by every reader
//
// Reaps run in taskworker as well as api, so the tick appender is shared code,
// AppendApiTick through the process's sequencer, whose script stays the one
// writer of {acl_}e. A process with no sequencer appends nothing, and the task
// retries.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Tunables of ticks.
type ConfigLogTickSettings struct {
	// The least time between two ticks of a network: 1 min (§7.9's tick row),
	// which is also how long reaps wait to coalesce.
	TickInterval time.Duration
	// The most reaped ids one tick names; more go in the next.
	TickReapMaxCount int
	// The most ephemeral admissions one run looks at.
	AdmissionMaxCount int
	// A run's max time.
	MaxTime time.Duration
}

func DefaultConfigLogTickSettings() *ConfigLogTickSettings {
	return &ConfigLogTickSettings{
		TickInterval:      time.Minute,
		TickReapMaxCount:  512,
		AdmissionMaxCount: 10000,
		MaxTime:           2 * time.Minute,
	}
}

// What a tick's content must be at the tip, as every reader judges it
// (sidecar's configCheckTick): every reaped id an ephemeral admission, and no
// activation until TEAM-M2-05's recovery window lands (section 5).
func checkConfigLogTick(state *sidecar.ConfigState, tick *bridge.ApiTick) error {
	if len(tick.GetReapedSidecarIds()) == 0 && len(tick.GetActivatedProposalIds()) == 0 {
		return fmt.Errorf("%w: an empty tick", sidecar.ErrConfigRule)
	}
	for _, reapedBytes := range tick.GetReapedSidecarIds() {
		sidecarId, err := sidecar.IdFromBytes(reapedBytes)
		if err != nil {
			return fmt.Errorf("%w: a reaped id of %d bytes", sidecar.ErrConfigRule, len(reapedBytes))
		}
		row := state.SidecarRow(sidecarId)
		if row == nil || !row.GetEphemeral() {
			return fmt.Errorf("%w: a reaped id names no ephemeral admission", sidecar.ErrConfigRule)
		}
	}
	if 0 < len(tick.GetActivatedProposalIds()) {
		return fmt.Errorf("%w: a tick's activations wait on the recovery window (TEAM-M2-05)", sidecar.ErrConfigRuleMissing)
	}
	return nil
}

// The live ephemeral admissions at the network's tip, at most limit: the
// candidates a reap tick may name.
func (self *ConfigLogSequencer) LiveEphemeralAdmissions(ctx context.Context, networkId server.Id, limit int) ([]sidecar.Id, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	if network.log == nil {
		return nil, nil
	}
	state := network.log.State()
	sidecarIds := []sidecar.Id{}
	for _, sidecarId := range state.SidecarIds() {
		if limit <= len(sidecarIds) {
			break
		}
		row := state.SidecarRow(sidecarId)
		if row != nil && row.GetEphemeral() && row.GetRevokedAt() == 0 {
			sidecarIds = append(sidecarIds, sidecarId)
		}
	}
	return sidecarIds, nil
}

// Of sidecarIds, those whose bound clients are all inactive or gone: reaped,
// since only the reap deactivates an ephemeral sidecar's client without its
// own revocation. A sidecar with no binding is left: its client has not shown
// which sidecar it is.
func reapedEphemeralSidecars(ctx context.Context, sidecarIds []sidecar.Id) (reapedIds []sidecar.Id) {
	if len(sidecarIds) == 0 || !server.SidecarMg1Linked() {
		return nil
	}
	ids := make([]server.Id, 0, len(sidecarIds))
	for _, sidecarId := range sidecarIds {
		ids = append(ids, server.Id(sidecarId))
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT sidecar_identity_binding.sidecar_id
				FROM sidecar_identity_binding
				LEFT JOIN network_client ON network_client.client_id = sidecar_identity_binding.client_id
				WHERE sidecar_identity_binding.sidecar_id = ANY($1)
				GROUP BY sidecar_identity_binding.sidecar_id
				HAVING NOT bool_or(COALESCE(network_client.active, false))
			`,
			ids,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var sidecarId server.Id
				server.Raise(result.Scan(&sidecarId))
				reapedIds = append(reapedIds, sidecar.Id(sidecarId))
			}
		})
	})
	return
}

// The reaped ephemeral admissions of the network, at most limit, as a tick
// names them.
func reapedEphemeralAdmissions(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, settings *ConfigLogTickSettings) ([]sidecar.Id, error) {
	live, err := sequencer.LiveEphemeralAdmissions(ctx, networkId, settings.AdmissionMaxCount)
	if err != nil {
		return nil, err
	}
	reaped := reapedEphemeralSidecars(ctx, live)
	if settings.TickReapMaxCount < len(reaped) {
		reaped = reaped[:settings.TickReapMaxCount]
	}
	return reaped, nil
}

type AppendConfigLogTickArgs struct {
	NetworkId server.Id `json:"network_id"`
}

type AppendConfigLogTickResult struct {
	// the tick's position, nil for no tick
	N *uint64 `json:"n,omitempty"`
	// reaps left for the next tick
	More bool `json:"more,omitempty"`
}

// Schedules the network's tick task a tick interval from now, in tx, merging
// with one already pending, so the reaps of that minute share its tick.
func ScheduleConfigLogTickInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	settings := DefaultConfigLogTickSettings()
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	scheduleConfigLogTickInTx(tx, owner, networkId, server.NowUtc().Add(settings.TickInterval), settings)
}

func scheduleConfigLogTickInTx(tx server.PgTx, owner *session.ClientSession, networkId server.Id, runAt time.Time, settings *ConfigLogTickSettings) {
	task.ScheduleTaskInTx(
		tx,
		AppendConfigLogTickTask,
		&AppendConfigLogTickArgs{NetworkId: networkId},
		owner,
		task.RunOnce("config_log_tick", networkId),
		task.RunAt(runAt),
		task.MaxTime(settings.MaxTime),
	)
}

func init() {
	reapedEphemeralClientCallbacks = append(reapedEphemeralClientCallbacks, func(ctx context.Context, tx server.PgTx, networkId server.Id, clientId server.Id) {
		ScheduleConfigLogTickInTx(ctx, tx, networkId)
	})
}

// One tick of the network: the reaped ephemeral admissions at the tip, if any,
// appended as one ApiTick. Without this process's sequencer it retries a tick
// interval on.
func AppendConfigLogTickTask(
	appendConfigLogTick *AppendConfigLogTickArgs,
	clientSession *session.ClientSession,
) (*AppendConfigLogTickResult, error) {
	settings := DefaultConfigLogTickSettings()
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.TickInterval)
	}
	return appendConfigLogTickAt(clientSession.Ctx, sequencer, appendConfigLogTick.NetworkId, settings)
}

func appendConfigLogTickAt(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, settings *ConfigLogTickSettings) (*AppendConfigLogTickResult, error) {
	reaped, err := reapedEphemeralAdmissions(ctx, sequencer, networkId, settings)
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.TickInterval)
	}
	if len(reaped) == 0 {
		return &AppendConfigLogTickResult{}, nil
	}
	tick := &bridge.ApiTick{}
	for _, sidecarId := range reaped {
		tick.ReapedSidecarIds = append(tick.ReapedSidecarIds, sidecarId.Bytes())
	}
	pin, err := sequencer.AppendTick(ctx, networkId, tick)
	if err != nil {
		configLogError := &ConfigLogError{}
		if errors.As(err, &configLogError) && configLogError.Status == 403 {
			// the tip moved past one of them meanwhile: the next tick finds the rest
			if glog.V(1) {
				glog.Infof("[config]%s tick refused at the tip: %v\n", networkId, err)
			}
			return &AppendConfigLogTickResult{More: true}, nil
		}
		return nil, task.WithRetryDelay(err, settings.TickInterval)
	}
	n := pin.GetVersion()
	return &AppendConfigLogTickResult{
		N:    &n,
		More: len(reaped) == settings.TickReapMaxCount,
	}, nil
}

// Schedules the next tick a tick interval on when this one left reaps behind.
func AppendConfigLogTickTaskPost(
	appendConfigLogTick *AppendConfigLogTickArgs,
	appendConfigLogTickResult *AppendConfigLogTickResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if appendConfigLogTickResult.More {
		settings := DefaultConfigLogTickSettings()
		scheduleConfigLogTickInTx(tx, clientSession, appendConfigLogTick.NetworkId, server.NowUtc().Add(settings.TickInterval), settings)
	}
	return nil
}
