package work

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/demand/pricesheet"
	"github.com/urfoundation/sn/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// stDemandDbSetup prepares a database test's database. The migration list creates the
// tables, so it does nothing here. Until the branch merges, a harness overlay creates
// them in the test's scratch database instead.
var stDemandDbSetup = func(context.Context) {}

// A deposit retries when its outcome named, after the refusal delay when it named
// nothing, and never in a tight loop.
func TestStDepositNextRun(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	at := func(delay time.Duration) *time.Time {
		value := now.Add(delay)
		return &value
	}
	for _, test := range []struct {
		name    string
		retryAt *time.Time
		want    time.Duration
	}{
		{"a refusal", nil, 15 * time.Minute},
		{"waiting on source totals", at(time.Minute), time.Minute},
		{"the price guard", at(5 * time.Minute), 5 * time.Minute},
		{"already due", at(-time.Hour), stDepositMinRetryDelay},
		{"nearly due", at(time.Second), stDepositMinRetryDelay},
	} {
		if got := stDepositNextRun(now, test.retryAt); !got.Equal(now.Add(test.want)) {
			t.Fatalf("%s: %s", test.name, got)
		}
	}
}

// Only a deposit row that says the chain is over ends it. A failed row does not.
func TestStDepositConcludes(t *testing.T) {
	for _, test := range []struct {
		kind, status string
		concludes    bool
	}{
		{model.StPublishKindDeposit, model.StPublishStatusConfirmed, true},
		{model.StPublishKindDeposit, model.StPublishStatusSkipped, true},
		{model.StPublishKindDeposit, model.StPublishStatusRefused, true},
		{model.StPublishKindDeposit, model.StPublishStatusFailed, false},
		{model.StPublishKindDeposit, model.StPublishStatusPending, false},
		{model.StPublishKindDepositPush, model.StPublishStatusConfirmed, false},
		{model.StPublishKindDepositPush, model.StPublishStatusRefused, false},
	} {
		if got := stDepositConcludes(&model.StPublish{Kind: test.kind, Status: test.status}); got != test.concludes {
			t.Fatalf("%s %s: %t", test.kind, test.status, got)
		}
	}
}

type stDepositPendingRow struct {
	RunAt      time.Time
	MaxSeconds int
	Attempt    int
}

func stDepositPendingRows(t testing.TB, ctx context.Context) []stDepositPendingRow {
	t.Helper()
	rows := []stDepositPendingRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT run_at, run_max_time_seconds, args_json FROM pending_task WHERE function_name = $1`, task.NewTaskTarget(StDeposit).TargetFunctionName())
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row stDepositPendingRow
				var argsJson string
				server.Raise(result.Scan(&row.RunAt, &row.MaxSeconds, &argsJson))
				var args StDepositArgs
				server.Raise(json.Unmarshal([]byte(argsJson), &args))
				row.Attempt = args.Attempt
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// The delay a refusal asks for survives the sync's seeding, which runs every 5 seconds:
// seeding a pending chain changes nothing. A chain that is not pending is seeded for
// now, with the task's whole 60 minutes at both scheduling sites, and a chain that
// retried keeps its attempt count.
func TestStDepositRefusalDelaySurvivesTheSyncSeed(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		stDemandDbSetup(ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := &controller.StConfig{Enabled: true, ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000002")}
		controller.SetStConfig(cfg)
		defer controller.SetStConfig(nil)
		key := cfg.DeploymentKey()
		args := &StDepositArgs{DeploymentKey: string(key), Epoch: 7}

		scheduleStDeposit(owner, key, 7)
		seeded := stDepositPendingRows(tb, ctx)
		if len(seeded) != 1 || seeded[0].MaxSeconds != 3600 || seeded[0].Attempt != 0 || time.Minute < time.Until(seeded[0].RunAt) {
			tb.Fatalf("%+v", seeded)
		}
		// The attempt ran and was refused: its row is gone, and its Post schedules the next.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name = $1`, task.NewTaskTarget(StDeposit).TargetFunctionName()))
			server.Raise(StDepositPost(args, &StDepositResult{Retry: true}, owner, tx))
		})
		retried := stDepositPendingRows(tb, ctx)
		if len(retried) != 1 || retried[0].MaxSeconds != 3600 || retried[0].Attempt != 1 || time.Until(retried[0].RunAt) < 14*time.Minute {
			tb.Fatalf("%+v", retried)
		}
		for range 3 {
			scheduleStDeposit(owner, key, 7)
		}
		again := stDepositPendingRows(tb, ctx)
		if len(again) != 1 || !again[0].RunAt.Equal(retried[0].RunAt) || again[0].Attempt != 1 {
			tb.Fatalf("seeding moved the retry from %s to %+v", retried[0].RunAt, again)
		}
		// An outcome that names a time keeps it.
		soon := server.NowUtc().Add(time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name = $1`, task.NewTaskTarget(StDeposit).TargetFunctionName()))
			server.Raise(StDepositPost(args, &StDepositResult{Retry: true, RetryAt: &soon}, owner, tx))
		})
		named := stDepositPendingRows(tb, ctx)
		if len(named) != 1 || 2*time.Minute < time.Until(named[0].RunAt) {
			tb.Fatalf("%+v", named)
		}
		// An outcome that does not retry schedules nothing.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name = $1`, task.NewTaskTarget(StDeposit).TargetFunctionName()))
			server.Raise(StDepositPost(args, &StDepositResult{}, owner, tx))
		})
		if rows := stDepositPendingRows(tb, ctx); len(rows) != 0 {
			tb.Fatalf("%+v", rows)
		}
	})
}

// A concluded deposit row stops the seeding, and a failed one does not, so the chain
// keeps trying past any number of attempts until the epoch ends.
func TestStDepositConcludedStopsSeeding(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		stDemandDbSetup(ctx)
		key := model.StDeploymentKey("945:0x2000000000000000000000000000000000000002")
		if stDepositConcluded(ctx, key, 7) {
			tb.Fatal("no rows concluded")
		}
		failed := "refused"
		model.UpdateStPublish(ctx, model.AddStPublish(ctx, key, 7, model.StPublishKindDeposit), model.StPublishStatusFailed, nil, &failed)
		if stDepositConcluded(ctx, key, 7) {
			tb.Fatal("a failed row concluded the chain")
		}
		model.UpdateStPublish(ctx, model.AddStPublish(ctx, key, 7, model.StPublishKindDeposit), model.StPublishStatusConfirmed, nil, nil)
		if !stDepositConcluded(ctx, key, 7) {
			tb.Fatal("a confirmed row did not conclude the chain")
		}
		model.UpdateStPublish(ctx, model.AddStPublish(ctx, key, 8, model.StPublishKindDeposit), model.StPublishStatusRefused, nil, &failed)
		if !stDepositConcluded(ctx, key, 8) {
			tb.Fatal("a terminal row did not conclude the chain")
		}
	})
}

// stDemandSeedSyncTurn is the mirror the v1 chain below a sync turn would have built.
func stDemandSeedSyncTurn(ctx context.Context, key model.StDeploymentKey) {
	for epoch := uint64(4); epoch <= 6; epoch++ {
		model.UpsertStEpoch(ctx, key, &model.StEpoch{Epoch: epoch, StartBlock: epoch * 50_400, Status: model.StEpochStatusOpen})
	}
	model.SetStChainCheckpoint(ctx, key, model.StChainCheckpoint{NextBlock: 352_000})
}

// A usage step that fails, here because its table cannot be read at all, delays nothing
// else in the sync turn: the v2 epoch step and the v1 tasks are still scheduled, and the
// turn completes. The usage task's chain retries on its own.
func TestStLedgerV2UsageFailureLeavesTheSyncTurn(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		controller.SetStConfig(cfg)
		controller.SetStClient(&ledgerV2WorkV1Chain{})
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		server.Db(ctx, func(conn server.PgConn) {
			_, err := conn.Exec(ctx, `DROP TABLE IF EXISTS st_ledger_v2_epoch_usage`)
			server.Raise(err)
		}, server.OptReadWrite())
		key := cfg.DeploymentKey()
		stDemandSeedSyncTurn(ctx, key)
		runProjectionAuditTask(tb, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: string(key)}), true)
		if got := pendingStArgs(tb, ctx, task.NewTaskTarget(StLedgerV2Step).TargetFunctionName()); len(got) != 1 || got[0] != "epoch 6 attempt 0" {
			tb.Fatalf("v2 steps %v", got)
		}
		if got := pendingStArgs(tb, ctx, task.NewTaskTarget(StLedgerV2UsageStep).TargetFunctionName()); len(got) != 0 {
			tb.Fatalf("usage steps %v", got)
		}
		if got := pendingStArgs(tb, ctx, task.NewTaskTarget(StDeposit).TargetFunctionName()); len(got) != 1 || got[0] != "7 attempt 0" {
			tb.Fatalf("deposit tasks %v", got)
		}
		if got := pendingStArgs(tb, ctx, task.NewTaskTarget(StSyncChain).TargetFunctionName()); len(got) != 1 {
			tb.Fatalf("the sync turn did not recur: %v", got)
		}
	})
}

// stDemandWorkV1Chain answers the checkpoint check of a second sync turn.
type stDemandWorkV1Chain struct {
	ledgerV2WorkV1Chain
}

func (self *stDemandWorkV1Chain) BlockHash(context.Context, uint64) ([32]byte, error) {
	return [32]byte{}, nil
}

// A sync turn starts the ended epoch's users capture and its extender census capture
// alongside its v2 step, each its own chain, and a pending chain keeps its own retry.
func TestStSyncChainSchedulesTheUsageStep(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		stDemandDbSetup(ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		controller.SetStConfig(cfg)
		controller.SetStClient(&stDemandWorkV1Chain{})
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		key := cfg.DeploymentKey()
		stDemandSeedSyncTurn(ctx, key)
		runProjectionAuditTask(tb, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: string(key)}), true)
		function := task.NewTaskTarget(StLedgerV2UsageStep).TargetFunctionName()
		want := []string{controller.StLedgerV2UsageStepExtenderCensus + " 6 attempt 0", controller.StLedgerV2UsageStepUsers + " 6 attempt 0"}
		if got := pendingStArgs(tb, ctx, function); !slices.Equal(got, want) {
			tb.Fatalf("usage steps %v, want %v", got, want)
		}
		// The next turn sees them pending and leaves them as they are.
		runProjectionAuditTask(tb, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: string(key)}), true)
		if got := pendingStArgs(tb, ctx, function); !slices.Equal(got, want) {
			tb.Fatalf("usage steps %v, want %v", got, want)
		}
		// A step of a replaced deployment does nothing and schedules nothing.
		stale := &StLedgerV2UsageStepArgs{DeploymentKey: "945:0x1000000000000000000000000000000000000001", Kind: controller.StLedgerV2UsageStepUsers, Epoch: 6}
		result, err := StLedgerV2UsageStep(stale, owner)
		if err != nil || result.Retry {
			tb.Fatal(result, err)
		}
		if err := StLedgerV2UsageStepPost(stale, &StLedgerV2UsageStepResult{Retry: true}, owner, nil); err != nil {
			tb.Fatal(err)
		}
	})
}

// stLateEntrySheet is a published sheet whose entries, one per epoch, key signed for the
// deployment of chainId and netuid.
func stLateEntrySheet(tb testing.TB, key *ecdsa.PrivateKey, chainId uint64, netuid uint16, epochs ...uint64) []byte {
	tb.Helper()
	alpha := func(rao uint64) string { return fmt.Sprintf("%d.%09d", rao/1_000_000_000, rao%1_000_000_000) }
	var file strings.Builder
	fmt.Fprintf(&file, "sn: %d\n", netuid)
	if len(epochs) != 0 {
		file.WriteString("signed_schedule:\n")
	}
	for _, epoch := range epochs {
		entry := pricesheet.Entry{EffectiveEpoch: epoch, EpochCapRao: 1_000_000_000, Tiers: []protocol.DepositTier{{RateNumeratorRaoPerGiB: 100, RateNumeratorRaoPerUser: 5, RateDenominator: 1}}}
		message, err := entry.Message(chainId, netuid)
		if err != nil {
			tb.Fatal(err)
		}
		signature, err := crypto.Sign(crypto.Keccak256(fmt.Appendf(nil, "\x19Ethereum Signed Message:\n%d%s", len(message), message)), key)
		if err != nil {
			tb.Fatal(err)
		}
		fmt.Fprintf(&file, "  %d:\n    epoch_cap_alpha: %s\n    signature: \"0x%s\"\n    tiers:\n      0:\n        alpha_per_gib: %s\n        alpha_per_user: %s\n", epoch, alpha(1_000_000_000), hex.EncodeToString(signature), alpha(100), alpha(5))
	}
	return []byte(file.String())
}

// The sync turn keeps a late price check scheduled 15 minutes out for an epoch whose
// deposit chain concluded with a zero-price skip, and for no other; the check seeds the
// epoch's deposit chain once a verified sheet prices the epoch, and not before; and a
// confirmed deposit ends the checks.
func TestStDepositLateEntryChecksAndSeeds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		stDemandDbSetup(ctx)
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		cfg := ledgerV2WorkConfig()
		cfg.Netuid = 25
		cfg.DepositZeroRateAction, cfg.DepositTiers = protocol.DepositZeroRateEqualDemand, []controller.StDepositTier{{RateDenominator: 1}}
		controller.SetStConfig(cfg)
		controller.SetStClient(&stDemandWorkV1Chain{})
		controller.SetStLedgerV2Client(&ledgerV2WorkChain{})
		defer func() {
			controller.SetStConfig(nil)
			controller.SetStClient(nil)
			controller.SetStLedgerV2Client(nil)
		}()
		key, err := crypto.ToECDSA(bytes.Repeat([]byte{0x7a}, 32))
		if err != nil {
			tb.Fatal(err)
		}
		var served atomic.Pointer[[]byte]
		zero := stLateEntrySheet(tb, key, cfg.ChainId, uint16(cfg.Netuid))
		served.Store(&zero)
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(*served.Load())
		}))
		defer site.Close()
		defer controller.Testing_SetStDemandSheet(site.URL, crypto.PubkeyToAddress(key.PublicKey), cfg.ChainId, uint16(cfg.Netuid))()
		deploymentKey := cfg.DeploymentKey()
		stDemandSeedSyncTurn(ctx, deploymentKey)
		lateEntries, deposits := task.NewTaskTarget(StDepositLateEntry).TargetFunctionName(), task.NewTaskTarget(StDeposit).TargetFunctionName()
		sync := func() {
			tb.Helper()
			runProjectionAuditTask(tb, owner, newAsyncMaintenanceCase(StSyncChain, StSyncChainPost, &StSyncChainArgs{DeploymentKey: string(deploymentKey)}), true)
		}
		clear := func(function string) {
			server.Db(ctx, func(conn server.PgConn) {
				_, err := conn.Exec(ctx, `DELETE FROM pending_task WHERE function_name = $1`, function)
				server.Raise(err)
			}, server.OptReadWrite())
		}

		// Another skip is no late price to wait for.
		other := model.AddStPublish(ctx, deploymentKey, 7, model.StPublishKindDeposit)
		reason := "deposit size is zero (no usage, or deposit rate/cap not configured)"
		model.UpdateStPublish(ctx, other, model.StPublishStatusSkipped, nil, &reason)
		sync()
		if got := pendingStArgs(tb, ctx, lateEntries); len(got) != 0 {
			tb.Fatalf("late checks %v", got)
		}
		server.Db(ctx, func(conn server.PgConn) {
			_, err := conn.Exec(ctx, `DELETE FROM st_publish WHERE publish_id = $1`, other)
			server.Raise(err)
		}, server.OptReadWrite())
		clear(deposits)

		// The zero-price skip: the regular chain is concluded, and a check is kept 15
		// minutes out.
		skip := model.AddStPublish(ctx, deploymentKey, 7, model.StPublishKindDeposit)
		reason = "price is zero: no demand deposit is required for epoch 7 (epoch 6 artifact: 0 bytes, 0 users)"
		model.UpdateStPublish(ctx, skip, model.StPublishStatusSkipped, nil, &reason)
		sync()
		if got := pendingStArgs(tb, ctx, lateEntries); len(got) != 1 || got[0] != "7 attempt 0" {
			tb.Fatalf("late checks %v", got)
		}
		if got := pendingStArgs(tb, ctx, deposits); len(got) != 0 {
			tb.Fatalf("deposit tasks %v", got)
		}
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE function_name = $1`, lateEntries).Scan(&runAt))
		})
		if wait := runAt.Sub(server.NowUtc()); wait < 14*time.Minute || 16*time.Minute < wait {
			tb.Fatalf("the check runs in %s", wait)
		}

		// At zero price the check seeds nothing.
		args := &StDepositLateEntryArgs{DeploymentKey: string(deploymentKey), Epoch: 7}
		if result, err := StDepositLateEntry(args, owner); err != nil || result.Seeded {
			tb.Fatal(result, err)
		}
		if got := pendingStArgs(tb, ctx, deposits); len(got) != 0 {
			tb.Fatalf("deposit tasks %v", got)
		}
		// The entry is published: the next check seeds the epoch's deposit chain.
		priced := stLateEntrySheet(tb, key, cfg.ChainId, uint16(cfg.Netuid), 7)
		served.Store(&priced)
		if result, err := StDepositLateEntry(args, owner); err != nil || !result.Seeded {
			tb.Fatal(result, err)
		}
		if got := pendingStArgs(tb, ctx, deposits); len(got) != 1 || got[0] != "7 attempt 0" {
			tb.Fatalf("deposit tasks %v", got)
		}
		// Gone without landing, it is seeded again by the next check.
		clear(deposits)
		if result, err := StDepositLateEntry(args, owner); err != nil || !result.Seeded {
			tb.Fatal(result, err)
		}
		if got := pendingStArgs(tb, ctx, deposits); len(got) != 1 {
			tb.Fatalf("deposit tasks %v", got)
		}

		// Confirmed: no check is kept, and a pending one seeds nothing.
		clear(deposits)
		landed := model.AddStPublish(ctx, deploymentKey, 7, model.StPublishKindDeposit)
		model.UpdateStPublish(ctx, landed, model.StPublishStatusConfirmed, nil, nil)
		clear(lateEntries)
		sync()
		if got := pendingStArgs(tb, ctx, lateEntries); len(got) != 0 {
			tb.Fatalf("late checks %v", got)
		}
		if result, err := StDepositLateEntry(args, owner); err != nil || result.Seeded {
			tb.Fatal(result, err)
		}
		if got := pendingStArgs(tb, ctx, deposits); len(got) != 0 {
			tb.Fatalf("deposit tasks %v", got)
		}
	})
}

// A deposit chain an earlier binary left pending, with its 30-minute run time, takes this
// binary's 60 minutes when the sync seeds, and keeps its own schedule. A new chain gets
// them from the start.
func TestStDepositSeedRaisesAnEarlierMaxTime(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		key := model.StDeploymentKey("945:0x2000000000000000000000000000000000000002")
		function := task.NewTaskTarget(StDeposit).TargetFunctionName()
		read := func(epoch uint64) (time.Time, int) {
			tb.Helper()
			var runAt time.Time
			var maxTime int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT run_at, run_max_time_seconds FROM pending_task WHERE function_name = $1 AND run_once_key = $2`, function, stDepositRunOnce(string(key), epoch).String()).Scan(&runAt, &maxTime))
			})
			return runAt, maxTime
		}
		later := server.NowUtc().Add(time.Hour)
		task.ScheduleTask(StDeposit, &StDepositArgs{DeploymentKey: string(key), Epoch: 7, Attempt: 3}, owner, stDepositRunOnce(string(key), 7), task.RunAt(later), task.MaxTime(30*time.Minute))
		if _, maxTime := read(7); maxTime != 1800 {
			tb.Fatalf("the earlier binary's row runs for %ds", maxTime)
		}
		scheduleStDeposit(owner, key, 7)
		runAt, maxTime := read(7)
		if maxTime != int(stDepositMaxTime/time.Second) || runAt.Sub(later).Abs() > time.Second {
			tb.Fatalf("run at %s for %ds", runAt, maxTime)
		}
		scheduleStDeposit(owner, key, 8)
		if _, maxTime := read(8); maxTime != int(stDepositMaxTime/time.Second) {
			tb.Fatalf("a new chain runs for %ds", maxTime)
		}
	})
}

// A panic while scheduling the late price check is logged and goes no further, so the
// rest of the sync turn runs. Here the session itself is missing.
func TestStDepositLateEntryScheduleRecovers(t *testing.T) {
	scheduleStDepositLateEntry(nil, model.StDeploymentKey("945:0x2000000000000000000000000000000000000002"), 7)
}
