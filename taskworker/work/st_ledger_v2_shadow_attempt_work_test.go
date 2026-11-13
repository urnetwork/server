package work

// st_ledger_v2_shadow_attempt_work_test.go — an extender shadow step's attempts
// are bounded on their own and counted (server CPUOPT5.md #1: epoch 4's step
// was cut by the task framework's 30-minute bound 13 times, rescheduled each
// time with no Post, so no attempt was ever counted and no cap applied), and a
// chain an earlier binary left pending takes this binary's bound.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfoundation/sn/amendment"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// One pending usage step row.
type stUsageStepPendingRow struct {
	args    StLedgerV2UsageStepArgs
	runAt   time.Time
	maxTime int
}

// The pending usage step rows.
func stUsageStepPendingRows(t testing.TB, ctx context.Context) []stUsageStepPendingRow {
	t.Helper()
	rows := []stUsageStepPendingRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT args_json, run_at, run_max_time_seconds FROM pending_task WHERE function_name = $1 ORDER BY run_at`, task.NewTaskTarget(StLedgerV2UsageStep).TargetFunctionName())
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var argsJson string
				var row stUsageStepPendingRow
				server.Raise(result.Scan(&argsJson, &row.runAt, &row.maxTime))
				server.Raise(json.Unmarshal([]byte(argsJson), &row.args))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// Finishes a step's attempt as the task framework does: its pending row goes,
// then its Post runs, in one transaction.
func stUsageStepFinish(t testing.TB, ctx context.Context, owner *session.ClientSession, args *StLedgerV2UsageStepArgs, result *StLedgerV2UsageStepResult, settings *StLedgerV2UsageStepSettings) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name = $1`, task.NewTaskTarget(StLedgerV2UsageStep).TargetFunctionName()))
		server.Raise(postStLedgerV2UsageStep(args, result, owner, tx, settings))
	})
}

// The shadow phase's timed-out attempts gauge, from the default registry.
func stShadowTimedOutAttempts(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_st_extender_pool_count" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["phase"] == "shadow" && labels["count"] == "timed_out_attempts" {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}

// A shadow attempt whose read runs past ShadowAttemptTime ends on its own,
// before the task's bound, and its Post counts it: the next attempt carries
// one timeout. The second gives the epoch's shadow up: the gauge counts it,
// and the chain is held for ShadowGiveUpHold by a last attempt that ends it
// without reading. A retry that is not a timeout is not counted.
func TestStLedgerV2UsageStepCountsAndCapsTimedOutShadowAttempts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		controller.SetStConfig(cfg)
		controller.SetStClient(&ledgerV2WorkV1Chain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
		}()
		settings := DefaultStLedgerV2UsageStepSettings()
		// the attempt's own bound, reached at once: the read below waits on it
		settings.ShadowAttemptTime = time.Millisecond
		reads := 0
		readUntilCut := func(ctx context.Context, step *controller.StLedgerV2UsageStep) (*controller.StPublishOutcome, error) {
			reads++
			<-ctx.Done()
			return &controller.StPublishOutcome{Status: model.StPublishStatusFailed, Retry: true, Reason: "epoch 6 shadow rows: " + ctx.Err().Error()}, nil
		}
		args := &StLedgerV2UsageStepArgs{DeploymentKey: string(cfg.DeploymentKey()), Kind: controller.StLedgerV2UsageStepExtenderShadow, Epoch: 6}

		result, err := runStLedgerV2UsageStep(args, owner, settings, readUntilCut)
		if err != nil || !result.TimedOut || !result.Retry {
			tb.Fatalf("the first attempt: %+v, %v", result, err)
		}
		stUsageStepFinish(tb, ctx, owner, args, result, settings)
		rows := stUsageStepPendingRows(tb, ctx)
		if len(rows) != 1 || rows[0].args.Timeouts != 1 || rows[0].args.Attempt != 1 || rows[0].maxTime != int(settings.ShadowMaxTime/time.Second) || time.Hour < time.Until(rows[0].runAt) {
			tb.Fatalf("after one timeout %+v", rows)
		}

		// a wait, such as for the epoch's close, is not a timeout
		wait := func(ctx context.Context, step *controller.StLedgerV2UsageStep) (*controller.StPublishOutcome, error) {
			return &controller.StPublishOutcome{Status: model.StPublishStatusFailed, Retry: true, Reason: "the pool's epoch 6 is not closed yet"}, nil
		}
		second := rows[0].args
		result, err = runStLedgerV2UsageStep(&second, owner, settings, wait)
		if err != nil || result.TimedOut || !result.Retry {
			tb.Fatalf("a wait: %+v, %v", result, err)
		}
		stUsageStepFinish(tb, ctx, owner, &second, result, settings)
		rows = stUsageStepPendingRows(tb, ctx)
		if len(rows) != 1 || rows[0].args.Timeouts != 1 || rows[0].args.Attempt != 2 {
			tb.Fatalf("after a wait %+v", rows)
		}

		// the second timeout gives the epoch's shadow up and holds the chain
		third := rows[0].args
		result, err = runStLedgerV2UsageStep(&third, owner, settings, readUntilCut)
		if err != nil || !result.TimedOut {
			tb.Fatalf("the third attempt: %+v, %v", result, err)
		}
		stUsageStepFinish(tb, ctx, owner, &third, result, settings)
		rows = stUsageStepPendingRows(tb, ctx)
		if len(rows) != 1 || rows[0].args.Timeouts != 2 || time.Until(rows[0].runAt) < settings.ShadowGiveUpHold-time.Hour || settings.ShadowGiveUpHold < time.Until(rows[0].runAt) {
			tb.Fatalf("given up %+v", rows)
		}
		if got := stShadowTimedOutAttempts(tb); got != 2 {
			tb.Fatalf("timed-out attempts gauge %v", got)
		}
		if reads != 2 {
			tb.Fatalf("%d reads", reads)
		}

		// the held attempt ends the chain without reading
		held := rows[0].args
		result, err = runStLedgerV2UsageStep(&held, owner, settings, func(context.Context, *controller.StLedgerV2UsageStep) (*controller.StPublishOutcome, error) {
			tb.Fatal("a given-up chain read its epoch")
			return nil, nil
		})
		if err != nil || result.Retry || result.TimedOut {
			tb.Fatalf("the held attempt: %+v, %v", result, err)
		}
		stUsageStepFinish(tb, ctx, owner, &held, result, settings)
		if rows := stUsageStepPendingRows(tb, ctx); len(rows) != 0 {
			tb.Fatalf("after the held attempt %+v", rows)
		}
	})
}

// A shadow chain an earlier binary left pending with the 30-minute bound takes
// this binary's two hours when the sync next sees its step due, and keeps its
// own schedule and arguments; a new chain gets its bound from the start, and
// another kind's chain keeps its own.
func TestStLedgerV2UsageStepSeedRaisesAnEarlierShadowBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := "945:0x2000000000000000000000000000000000000002"
		settings := DefaultStLedgerV2UsageStepSettings()
		earlier := &StLedgerV2UsageStepArgs{DeploymentKey: key, Kind: controller.StLedgerV2UsageStepExtenderShadow, Epoch: 4, Attempt: 1}
		later := server.NowUtc().Add(time.Hour)
		task.ScheduleTask(StLedgerV2UsageStep, earlier, owner, stLedgerV2UsageStepRunOnce(earlier), task.RunAt(later), task.MaxTime(30*time.Minute))
		users := &StLedgerV2UsageStepArgs{DeploymentKey: key, Kind: controller.StLedgerV2UsageStepUsers, Epoch: 4}
		task.ScheduleTask(StLedgerV2UsageStep, users, owner, stLedgerV2UsageStepRunOnce(users), task.RunAt(later), task.MaxTime(30*time.Minute))
		if rows := stUsageStepPendingRows(tb, ctx); len(rows) != 2 || rows[0].maxTime != 1800 || rows[1].maxTime != 1800 {
			tb.Fatalf("the earlier binary's rows %+v", rows)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, args := range []*StLedgerV2UsageStepArgs{
				{DeploymentKey: key, Kind: controller.StLedgerV2UsageStepExtenderShadow, Epoch: 4},
				{DeploymentKey: key, Kind: controller.StLedgerV2UsageStepUsers, Epoch: 4},
				{DeploymentKey: key, Kind: controller.StLedgerV2UsageStepExtenderShadow, Epoch: 5},
			} {
				scheduleStLedgerV2UsageStepInTx(tx, owner, args, server.NowUtc(), settings)
			}
		})
		bounds := map[string]int{}
		for _, row := range stUsageStepPendingRows(tb, ctx) {
			bounds[fmt.Sprintf("%s %d", row.args.Kind, row.args.Epoch)] = row.maxTime
			if row.args.Epoch == 4 && (row.runAt.Sub(later).Abs() > time.Second || (row.args.Kind == controller.StLedgerV2UsageStepExtenderShadow && row.args.Attempt != 1)) {
				tb.Fatalf("the pending chain's schedule or arguments moved: %+v", row)
			}
		}
		want := map[string]int{
			controller.StLedgerV2UsageStepExtenderShadow + " 4": int(settings.ShadowMaxTime / time.Second),
			controller.StLedgerV2UsageStepUsers + " 4":          int(settings.MaxTime / time.Second),
			controller.StLedgerV2UsageStepExtenderShadow + " 5": int(settings.ShadowMaxTime / time.Second),
		}
		if len(bounds) != len(want) {
			tb.Fatalf("bounds %v, want %v", bounds, want)
		}
		for kind, bound := range want {
			if bounds[kind] != bound {
				tb.Fatalf("bounds %v, want %v", bounds, want)
			}
		}
	})
}

// The sync gives the shadow row an earlier binary queued, as d1ea65129 queued
// epoch 4's with the usage steps' 30 minutes, the two hours its step needs, the
// next time it sees the step due: before this, it left the row alone, and every
// attempt was cut at 30 minutes (server CPUOPT5.md #1). The row keeps its own
// schedule and arguments.
func TestStLedgerV2UsageSyncRaisesAQueuedShadowRowsBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		cfg.Netuid = 7
		line, err := amendment.NewExtenders(cfg.ChainId, uint16(cfg.Netuid), cfg.NoId, 8).Line()
		if err != nil {
			tb.Fatal(err)
		}
		if err := cfg.Amendments.Encode([]string{line}); err != nil {
			tb.Fatal(err)
		}
		controller.SetStConfig(cfg)
		controller.SetStClient(&ledgerV2WorkV1Chain{})
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		key := cfg.DeploymentKey()
		// epoch 6, one of the two before E_x = 8, with its census captured and
		// its rows recorded without an extender component: its shadow is due
		model.CaptureStLedgerV2ExtenderCensus(ctx, key, cfg.NoId, 6, model.EncodeStExtenderCensus(nil), 0, server.NowUtc())
		if err := model.RecordStLedgerV2EpochRows(ctx, key, cfg.NoId, 6, nil); err != nil {
			tb.Fatal(err)
		}
		queued := &StLedgerV2UsageStepArgs{DeploymentKey: string(key), Kind: controller.StLedgerV2UsageStepExtenderShadow, Epoch: 6, Attempt: 1}
		later := server.NowUtc().Add(time.Hour)
		task.ScheduleTask(StLedgerV2UsageStep, queued, owner, stLedgerV2UsageStepRunOnce(queued), task.RunAt(later), task.MaxTime(30*time.Minute))
		scheduleStLedgerV2UsageSteps(owner, key, &controller.StEpochState{Epoch: 7})
		shadows := 0
		for _, row := range stUsageStepPendingRows(tb, ctx) {
			if row.args.Kind != controller.StLedgerV2UsageStepExtenderShadow {
				continue
			}
			shadows++
			if row.maxTime != int(2*time.Hour/time.Second) || row.args.Attempt != 1 || row.runAt.Sub(later).Abs() > time.Second {
				tb.Fatalf("the queued shadow row after the sync: %d s, %+v at %s", row.maxTime, row.args, row.runAt)
			}
		}
		if shadows != 1 {
			tb.Fatalf("%d shadow rows", shadows)
		}
	})
}
