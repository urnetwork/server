package work

// st_ledger_v2_usage_work.go — the settlement v2 usage statement's tasks
// (controller/st_ledger_v2_usage.go). They are their own chain, never part of a v2
// driver step or a v1 task, so a failure here delays no close, commit or phase C
// sweep. Each StSyncChain turn starts a chain for a due step that has none; a step
// retries itself through its Post until it concludes, and a chain that reaches its
// cap is restarted by the next turn.
//
// An extender shadow step reads a whole epoch, so each attempt is bounded on its
// own: its work stops at ShadowAttemptTime, inside the task's bound, and returns
// through the Post, which counts it as timed out. Cut by the task framework's
// own bound instead, the same row would be rescheduled with no Post and no
// count, without end (server CPUOPT5.md #1). Once a chain has timed out
// ShadowMaxTimeouts times, the epoch's shadow is given up: an alert is raised
// once, and the chain is held past the epoch's sync window, so no restart
// starts another read of it.

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Tunables of the usage statement's tasks.
type StLedgerV2UsageStepSettings struct {
	// Turns a step's chain may take before it stops; the next sync turn
	// restarts a stopped chain.
	MaxAttempts int
	// Least delay between turns: users are captured as close to the boundary
	// as a failed read allows.
	RetryDelay time.Duration
	// Bound of one step that reads the epoch's provider chunks and publishes
	// one small object.
	MaxTime time.Duration
	// Bound of one extender shadow step, which reads the epoch's contracts as
	// a root's build does, so it has the v2 driver's time.
	ShadowMaxTime time.Duration
	// Bound of one shadow attempt's own work, inside ShadowMaxTime so it ends
	// first: an attempt cut here returns through the Post, which counts it as
	// timed out. Non-positive leaves the attempt to the task's bound, which
	// counts nothing.
	ShadowAttemptTime time.Duration
	// Timed-out attempts a shadow chain may take before the epoch's shadow is
	// given up. Non-positive never gives up.
	ShadowMaxTimeouts int
	// How long a given-up shadow chain is held, so that the sync, which plans
	// an epoch only until the next one ends, starts no other: an epoch of 7
	// days, and a day more.
	ShadowGiveUpHold time.Duration
}

// The defaults the usage steps have always had, and the v2 driver's two hours
// for a shadow step. A shadow attempt's work stops at 1 h 50 min, and a chain
// gives its epoch up after two timed-out attempts, for 8 days. A shadow read in
// short slices takes about 45 to 70 minutes for a 7-day epoch
// (controller.StLedgerV2ExtenderShadowSettings), so a timeout means the
// database is struggling, and a third read would add to it.
func DefaultStLedgerV2UsageStepSettings() *StLedgerV2UsageStepSettings {
	return &StLedgerV2UsageStepSettings{
		MaxAttempts:       100,
		RetryDelay:        15 * time.Second,
		MaxTime:           30 * time.Minute,
		ShadowMaxTime:     2 * time.Hour,
		ShadowAttemptTime: 110 * time.Minute,
		ShadowMaxTimeouts: 2,
		ShadowGiveUpHold:  8 * 24 * time.Hour,
	}
}

// Reports whether a shadow chain has timed out as many times as it may.
func (self *StLedgerV2UsageStepSettings) shadowGivenUp(args *StLedgerV2UsageStepArgs) bool {
	return args.Kind == controller.StLedgerV2UsageStepExtenderShadow && 0 < self.ShadowMaxTimeouts && self.ShadowMaxTimeouts <= args.Timeouts
}

// A usage step's time limit by kind.
func (self *StLedgerV2UsageStepSettings) maxTimeOf(kind string) time.Duration {
	if kind == controller.StLedgerV2UsageStepExtenderShadow {
		return self.ShadowMaxTime
	}
	return self.MaxTime
}

// scheduleStLedgerV2UsageSteps starts a task chain for each due usage step that has
// none. Nothing it does can reach the sync turn's other work: a database or chain
// failure here, including tables not migrated yet, ends here.
func scheduleStLedgerV2UsageSteps(clientSession *session.ClientSession, deploymentKey model.StDeploymentKey, state *controller.StEpochState) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[st]v2 usage sync failed: %v\n", r)
		}
	}()
	steps, err := controller.StLedgerV2UsageSync(clientSession.Ctx, state)
	if err != nil {
		glog.Infof("[st]v2 usage sync failed: %s\n", err)
		return
	}
	settings := DefaultStLedgerV2UsageStepSettings()
	for _, step := range steps {
		args := &StLedgerV2UsageStepArgs{DeploymentKey: string(deploymentKey), Kind: step.Kind, Epoch: step.Epoch}
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			scheduleStLedgerV2UsageStepInTx(tx, clientSession, args, step.RunAt, settings)
		})
	}
}

// Starts a chain for a due step that has none, and gives a chain an earlier
// binary left pending this binary's bound: it keeps the bound it was scheduled
// with otherwise, and a shadow attempt's own bound must fit inside it.
func scheduleStLedgerV2UsageStepInTx(tx server.PgTx, clientSession *session.ClientSession, args *StLedgerV2UsageStepArgs, runAt time.Time, settings *StLedgerV2UsageStepSettings) {
	runOnce := stLedgerV2UsageStepRunOnce(args)
	maxTime := settings.maxTimeOf(args.Kind)
	task.ScheduleTaskInTxIfAbsent(tx, StLedgerV2UsageStep, args, clientSession, runOnce, task.RunAt(runAt), task.MaxTime(maxTime))
	task.RaiseRunOnceMaxTimeInTx(clientSession.Ctx, tx, runOnce, maxTime)
}

func stLedgerV2UsageStepRunOnce(args *StLedgerV2UsageStepArgs) *task.RunOnceOption {
	return task.RunOnce("st_ledger_v2_usage_step", args.DeploymentKey, args.Kind, args.Epoch)
}

// StLedgerV2UsageStep(kind, epoch)

type StLedgerV2UsageStepArgs struct {
	DeploymentKey string `json:"deployment_key"`
	Kind          string `json:"kind"`
	Epoch         uint64 `json:"epoch"`
	Attempt       int    `json:"attempt"`
	// the chain's shadow attempts that ran past ShadowAttemptTime
	Timeouts int `json:"timeouts,omitempty"`
}

type StLedgerV2UsageStepResult struct {
	Retry   bool       `json:"retry"`
	RetryAt *time.Time `json:"retry_at,omitempty"`
	// a shadow attempt whose work ran past ShadowAttemptTime
	TimedOut bool `json:"timed_out,omitempty"`
}

func StLedgerV2UsageStep(
	stLedgerV2UsageStep *StLedgerV2UsageStepArgs,
	clientSession *session.ClientSession,
) (*StLedgerV2UsageStepResult, error) {
	return runStLedgerV2UsageStep(stLedgerV2UsageStep, clientSession, DefaultStLedgerV2UsageStepSettings(), controller.StLedgerV2RunUsageStep)
}

// Runs one usage step with run, under settings. A shadow attempt's work is
// bounded by ShadowAttemptTime, and one cut there is timed out; a chain that has
// given its epoch up ends without reading anything.
func runStLedgerV2UsageStep(
	args *StLedgerV2UsageStepArgs,
	clientSession *session.ClientSession,
	settings *StLedgerV2UsageStepSettings,
	run func(context.Context, *controller.StLedgerV2UsageStep) (*controller.StPublishOutcome, error),
) (*StLedgerV2UsageStepResult, error) {
	if _, current := stTaskDeploymentCurrent(args.DeploymentKey); !current {
		return &StLedgerV2UsageStepResult{}, nil
	}
	if settings.shadowGivenUp(args) {
		glog.Infof("[st]v2 %s step epoch %d: given up after %d timed-out attempts; the held chain ends\n", args.Kind, args.Epoch, args.Timeouts)
		return &StLedgerV2UsageStepResult{}, nil
	}
	ctx := clientSession.Ctx
	bounded := args.Kind == controller.StLedgerV2UsageStepExtenderShadow && 0 < settings.ShadowAttemptTime
	if bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settings.ShadowAttemptTime)
		defer cancel()
	}
	outcome, err := run(ctx, &controller.StLedgerV2UsageStep{Kind: args.Kind, Epoch: args.Epoch})
	if bounded && (err != nil || outcome.Retry) && errors.Is(ctx.Err(), context.DeadlineExceeded) && clientSession.Ctx.Err() == nil {
		glog.Infof("[st]v2 %s step epoch %d ran past %s (attempt %d), timed out %d of %d\n", args.Kind, args.Epoch, settings.ShadowAttemptTime, args.Attempt, args.Timeouts+1, settings.ShadowMaxTimeouts)
		return &StLedgerV2UsageStepResult{Retry: true, TimedOut: true}, nil
	}
	if err != nil {
		glog.Infof("[st]v2 %s step epoch %d failed (attempt %d): %s\n", args.Kind, args.Epoch, args.Attempt, err)
		return &StLedgerV2UsageStepResult{Retry: true}, nil
	}
	glog.Infof("[st]v2 %s step epoch %d: %s\n", args.Kind, args.Epoch, outcome)
	result := &StLedgerV2UsageStepResult{Retry: outcome.Retry}
	if outcome.Retry && !outcome.RetryAt.IsZero() {
		retryAt := outcome.RetryAt
		result.RetryAt = &retryAt
	}
	return result, nil
}

func StLedgerV2UsageStepPost(
	stLedgerV2UsageStep *StLedgerV2UsageStepArgs,
	stLedgerV2UsageStepResult *StLedgerV2UsageStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return postStLedgerV2UsageStep(stLedgerV2UsageStep, stLedgerV2UsageStepResult, clientSession, tx, DefaultStLedgerV2UsageStepSettings())
}

// Schedules a step's next attempt under settings. A timed-out shadow attempt
// counts; the one that reaches ShadowMaxTimeouts gives the epoch's shadow up
// (controller.StLedgerV2GiveUpExtenderShadow) and holds the chain for
// ShadowGiveUpHold, a last attempt that ends it without reading.
func postStLedgerV2UsageStep(
	args *StLedgerV2UsageStepArgs,
	result *StLedgerV2UsageStepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
	settings *StLedgerV2UsageStepSettings,
) error {
	if _, current := stTaskDeploymentCurrent(args.DeploymentKey); !current {
		return nil
	}
	next := *args
	next.Attempt += 1
	if result.TimedOut {
		next.Timeouts += 1
		if settings.shadowGivenUp(&next) {
			controller.StLedgerV2GiveUpExtenderShadow(next.Epoch, next.Timeouts, settings.ShadowAttemptTime)
			task.ScheduleTaskInTx(tx, StLedgerV2UsageStep, &next, clientSession, stLedgerV2UsageStepRunOnce(&next), task.RunAt(server.NowUtc().Add(settings.ShadowGiveUpHold)), task.MaxTime(settings.maxTimeOf(next.Kind)))
			return nil
		}
	}
	if result.Retry && args.Attempt+1 < settings.MaxAttempts {
		runAt := server.NowUtc().Add(settings.RetryDelay)
		if result.RetryAt != nil && runAt.Before(*result.RetryAt) {
			runAt = *result.RetryAt
		}
		task.ScheduleTaskInTx(tx, StLedgerV2UsageStep, &next, clientSession, stLedgerV2UsageStepRunOnce(&next), task.RunAt(runAt), task.MaxTime(settings.maxTimeOf(next.Kind)))
	}
	return nil
}
