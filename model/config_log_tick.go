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
//	activations  a recovery window's end (§7.9's Recovery; D-TEAM-21; QD-745): after
//	             each statement this instance lands, the sequencer schedules a
//	             run-once task per open proposal a window holds that the entry
//	             signed, its proposal or a cosign, at window_time_ms plus the
//	             verifier's window (72 h), so a recovery cosign, which restarts
//	             the window, and the cosign that brings k both schedule it; the
//	             task reads the proposal's row at the tip when it runs: closed,
//	             expired or unknown, it does nothing; its window still running
//	             (a later recovery signature moved it), it runs again at the
//	             window's end; else it appends one ApiTick naming it and every
//	             other proposal whose window ran, each kept only if the
//	             verifier takes the tick with it, so api appends no activation
//	             a reader refuses; one api does not take yet, short of its
//	             cosigns or past config_admissions (§20.4, the sequencer's
//	             ActivationCap), is looked at again an hour on, until it
//	             closes or expires
//
// A tick is judged by ConfigLog.CheckTick, the verdict every reader gives it at
// its position, an activation from its proposal's row, so api appends no tick
// a reader refuses and refuses none a reader takes. The tasks here append
// through AppendTickSpaced, which holds §7.9's at most one tick a minute per
// network against the newest tick in the log, whichever task or instance
// appended it: a task that comes too soon runs again when the minute is up.
//
// Reaps run in taskworker as well as api, so the tick appender is shared code,
// AppendApiTick through the process's sequencer, whose script stays the one
// writer of {acl_}e. A process with no sequencer appends nothing, and the task
// retries.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

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
	// The most activations one tick names, the run's own proposal first; the
	// rest go in their own tasks' ticks. Each is one trial of the verifier.
	TickActivationMaxCount int
	// How long an activation task waits before it looks again at a proposal
	// whose window ran and that the tip does not take yet: 1 h. A proposal
	// stays open at most its header's lifetime, which bounds the looks.
	ActivationRecheckInterval time.Duration
}

func DefaultConfigLogTickSettings() *ConfigLogTickSettings {
	return &ConfigLogTickSettings{
		TickInterval:              time.Minute,
		TickReapMaxCount:          512,
		AdmissionMaxCount:         10000,
		MaxTime:                   2 * time.Minute,
		TickActivationMaxCount:    64,
		ActivationRecheckInterval: time.Hour,
	}
}

// A tick refused because the network's newest tick is under a tick interval
// old: After is when the next may be appended.
type ConfigLogTickSpacingError struct {
	After time.Time
}

func (self *ConfigLogTickSpacingError) Error() string {
	return fmt.Sprintf("the network's next tick waits until %s", self.After.UTC().Format(time.RFC3339))
}

// Notes a tick entry's time, which spaces the next tick.
func (self *configLogNetwork) noteTickWithLock(entryBytes []byte) {
	entry := &bridge.AuthEntry{}
	if proto.Unmarshal(entryBytes, entry) != nil || entry.GetKind() != bridge.AuthEntryKindTick {
		return
	}
	if !self.lastTick || self.lastTickTimeMs < entry.GetTimeMs() {
		self.lastTickTimeMs, self.lastTick = entry.GetTimeMs(), true
	}
}

// The time_ms from which the network's next tick may be appended at interval,
// false when nothing spaces it: no interval, or no tick this verifier knows.
func (self *configLogNetwork) tickSpacedUntilWithLock(interval time.Duration) (int64, bool) {
	if interval <= 0 || !self.lastTick {
		return 0, false
	}
	return self.lastTickTimeMs + interval.Milliseconds(), true
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
	pin, err := sequencer.AppendTickSpaced(ctx, networkId, tick, settings.TickInterval)
	if err != nil {
		configLogError := &ConfigLogError{}
		if errors.As(err, &configLogError) && configLogError.Status == 403 {
			// another tick took this minute, or the tip moved past one of
			// them meanwhile: the next tick finds the rest
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

// The open proposals a recovery window holds at the tip that the statement at
// n signed: its proposal's signature or a cosign's, which is every entry that
// moves a window (a recovery-key signature) or brings a counted signature,
// each with the end of its window by the verifier's Recovery values. None
// for an n that carried no such statement.
func configLogActivationsMoved(log *sidecar.ConfigLog, n uint64) (proposalIds [][]byte, runAts []time.Time) {
	window := log.RecoverySettings().Window
	for _, row := range log.State().PendingProposals() {
		moved := row.GetWindowEntry() == n
		for _, signer := range row.GetSigners() {
			moved = moved || signer.GetAt() == n
		}
		if moved {
			proposalIds = append(proposalIds, bytes.Clone(row.GetProposalId()))
			runAts = append(runAts, time.UnixMilli(row.GetWindowTimeMs()).Add(window))
		}
	}
	return
}

// After this instance landed the entry at n, or a retry found it there:
// settings' Activation for each proposal configLogActivationsMoved names,
// none for an entry that is no statement or that the tip has passed. A
// schedule repeated for a retry merges with the pending one.
func (self *ConfigLogSequencer) scheduleActivationsWithLock(ctx context.Context, networkId server.Id, network *configLogNetwork, n uint64) {
	if self.settings.Activation == nil || network.log == nil {
		return
	}
	proposalIds, runAts := configLogActivationsMoved(network.log, n)
	for i, proposalId := range proposalIds {
		self.settings.Activation(ctx, networkId, proposalId, runAts[i])
	}
}

// Whether api takes tick at the network's tip, sequenced at timeMs: the
// verdict every reader gives it there (ConfigLog.CheckTick), then settings'
// ActivationCap, config_admissions for the admissions its activations take.
func (self *ConfigLogSequencer) checkTickWithLock(ctx context.Context, networkId server.Id, network *configLogNetwork, tick *bridge.ApiTick, timeMs int64) error {
	if err := network.log.CheckTick(tick, timeMs); err != nil {
		return err
	}
	if self.settings.ActivationCap != nil && 0 < len(tick.GetActivatedProposalIds()) {
		return self.settings.ActivationCap(ctx, networkId, network.log.State(), tick, timeMs)
	}
	return nil
}

// What an activation task finds of its proposal at the tip.
type configLogActivationRead struct {
	// the tick to append, nil for none
	tick *bridge.ApiTick
	// when to look again, nil for never: the proposal is closed, expired or
	// unknown, or tick is set
	runAt *time.Time
}

// Reads proposalId at the network's tip, caught up with the store, at this
// sequencer's clock: nothing to do for a proposal no window holds open; its
// window's end while that is ahead; the minute's end while the network's
// newest tick spaces the next; else the tick that activates it, with every
// other proposal whose window ran, at most settings' count, each kept only
// if api takes the tick with it (checkTickWithLock); or, when api does not
// take its own activation yet, short of its cosigns or past config_admissions,
// a recheck interval on.
func (self *ConfigLogSequencer) readActivation(ctx context.Context, networkId server.Id, proposalId []byte, settings *ConfigLogTickSettings) (*configLogActivationRead, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	read := &configLogActivationRead{}
	if network.log == nil {
		return read, nil
	}
	if err := network.catchUpWithLock(ctx, self); err != nil {
		return nil, err
	}
	nowMs := self.now().UnixMilli()
	state := network.log.State()
	window, held := state.RecoveryWindow(proposalId)
	if !held || window.ClosedAt != 0 || window.Row.GetNotAfterMs() < nowMs {
		return read, nil
	}
	windowMs := network.log.RecoverySettings().Window.Milliseconds()
	again := func(atMs int64) (*configLogActivationRead, error) {
		at := time.UnixMilli(atMs)
		read.runAt = &at
		return read, nil
	}
	if dueMs := window.Row.GetWindowTimeMs() + windowMs; nowMs < dueMs {
		return again(dueMs)
	}
	if after, spaced := network.tickSpacedUntilWithLock(settings.TickInterval); spaced && nowMs < after {
		return again(after)
	}
	tick := &bridge.ApiTick{ActivatedProposalIds: [][]byte{bytes.Clone(proposalId)}}
	if err := self.checkTickWithLock(ctx, networkId, network, tick, nowMs); err != nil {
		if glog.V(1) {
			glog.Infof("[config]%s the tip takes no activation of a proposal whose window ran: %v\n", networkId, err)
		}
		return again(nowMs + settings.ActivationRecheckInterval.Milliseconds())
	}
	tried := 1
	for _, row := range state.PendingProposals() {
		if settings.TickActivationMaxCount <= tried {
			break
		}
		if bytes.Equal(row.GetProposalId(), proposalId) || nowMs < row.GetWindowTimeMs()+windowMs {
			continue
		}
		tried += 1
		with := &bridge.ApiTick{ActivatedProposalIds: append(slices.Clone(tick.GetActivatedProposalIds()), bytes.Clone(row.GetProposalId()))}
		if self.checkTickWithLock(ctx, networkId, network, with, nowMs) == nil {
			tick = with
		}
	}
	read.tick = tick
	return read, nil
}

type AppendConfigLogActivationArgs struct {
	NetworkId server.Id `json:"network_id"`
	// the AuthorityProposal's id, 32 bytes
	ProposalId []byte `json:"proposal_id"`
}

type AppendConfigLogActivationResult struct {
	// the activating tick's position, nil for no tick
	N *uint64 `json:"n,omitempty"`
	// when the task runs again, nil for never
	RunAt *time.Time `json:"run_at,omitempty"`
}

// The sequencer's Activation hook: schedules the proposal's activation task
// at runAt, the end of its window. Best effort: a schedule that fails is
// logged, and the proposal's next signature schedules it again.
func ScheduleConfigLogActivation(ctx context.Context, networkId server.Id, proposalId []byte, runAt time.Time) {
	settings := DefaultConfigLogTickSettings()
	if r := server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			owner := session.NewLocalClientSession(ctx, "", nil)
			defer owner.Cancel()
			scheduleConfigLogActivationInTx(tx, owner, networkId, proposalId, runAt, settings)
		})
	}); r != nil {
		glog.Warningf("[config]%s activation task not scheduled: %v\n", networkId, r)
	}
}

// One run-once task a proposal. A second schedule of a pending one keeps the
// earlier time, and the run at it finds the window's end from the row.
func scheduleConfigLogActivationInTx(tx server.PgTx, owner *session.ClientSession, networkId server.Id, proposalId []byte, runAt time.Time, settings *ConfigLogTickSettings) {
	task.ScheduleTaskInTx(
		tx,
		AppendConfigLogActivationTask,
		&AppendConfigLogActivationArgs{NetworkId: networkId, ProposalId: proposalId},
		owner,
		task.RunOnce("config_log_activation", networkId, hex.EncodeToString(proposalId)),
		task.RunAt(runAt),
		task.MaxTime(settings.MaxTime),
	)
}

// One look at a proposal whose recovery window was due: the tick that
// activates it, when the tip's row says it may activate now. Without this
// process's sequencer it retries a tick interval on.
func AppendConfigLogActivationTask(
	appendConfigLogActivation *AppendConfigLogActivationArgs,
	clientSession *session.ClientSession,
) (*AppendConfigLogActivationResult, error) {
	settings := DefaultConfigLogTickSettings()
	sequencer, err := CurrentConfigLogSequencer()
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.TickInterval)
	}
	return appendConfigLogActivationAt(clientSession.Ctx, sequencer, appendConfigLogActivation.NetworkId, appendConfigLogActivation.ProposalId, settings)
}

func appendConfigLogActivationAt(ctx context.Context, sequencer *ConfigLogSequencer, networkId server.Id, proposalId []byte, settings *ConfigLogTickSettings) (*AppendConfigLogActivationResult, error) {
	read, err := sequencer.readActivation(ctx, networkId, proposalId, settings)
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.TickInterval)
	}
	if read.tick == nil {
		return &AppendConfigLogActivationResult{RunAt: read.runAt}, nil
	}
	pin, err := sequencer.AppendTickSpaced(ctx, networkId, read.tick, settings.TickInterval)
	if err != nil {
		quotaRefusal := &QuotaRefusalError{}
		if errors.As(err, &quotaRefusal) {
			// an admission landed since the read and took the room
			runAt := sequencer.now().Add(settings.ActivationRecheckInterval)
			return &AppendConfigLogActivationResult{RunAt: &runAt}, nil
		}
		configLogError := &ConfigLogError{}
		if errors.As(err, &configLogError) && configLogError.Status == 403 {
			// the tip moved since the read, a veto, a void or another tick:
			// the next look reads it again
			if glog.V(1) {
				glog.Infof("[config]%s activation tick refused at the tip: %v\n", networkId, err)
			}
			runAt := sequencer.now().Add(settings.TickInterval)
			spacing := &ConfigLogTickSpacingError{}
			if errors.As(err, &spacing) {
				runAt = spacing.After
			}
			return &AppendConfigLogActivationResult{RunAt: &runAt}, nil
		}
		return nil, task.WithRetryDelay(err, settings.TickInterval)
	}
	n := pin.GetVersion()
	return &AppendConfigLogActivationResult{N: &n}, nil
}

// Schedules the next look when this one set a time.
func AppendConfigLogActivationTaskPost(
	appendConfigLogActivation *AppendConfigLogActivationArgs,
	appendConfigLogActivationResult *AppendConfigLogActivationResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	if appendConfigLogActivationResult.RunAt != nil {
		scheduleConfigLogActivationInTx(tx, clientSession, appendConfigLogActivation.NetworkId, appendConfigLogActivation.ProposalId,
			*appendConfigLogActivationResult.RunAt, DefaultConfigLogTickSettings())
	}
	return nil
}
