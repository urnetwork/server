package work

// The real task scheduler retains the half-TTL cadence and durable page cursor.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A completed pass does not defer refresh by another interval after its runtime.
func TestContractHoleRefreshCadenceUsesHalfTtl(t *testing.T) {
	start := time.Unix(1000, 0)
	due := start.Add(model.ContractHoleRefreshInterval)
	for _, elapsed := range []time.Duration{0, time.Second, model.ContractHoleRefreshInterval, model.ContractHoleTtl} {
		now := start.Add(elapsed)
		got := maxContractHoleRefreshTime(now, due)
		want := due
		if elapsed > model.ContractHoleRefreshInterval {
			want = now
		}
		if !got.Equal(want) || model.ContractHoleRefreshInterval*2 != model.ContractHoleTtl {
			t.Fatalf("elapsed=%s next=%s want=%s", elapsed, got, want)
		}
	}
}

// Startup is immediate, pages continue immediately, and the final page retains
// the original pass start instead of turning page count into schedule drift.
func TestContractHoleRefreshTaskScheduleAndContinuation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		start := server.NowUtc()
		cursor := &model.ContractHoleCursor{SourceClientId: server.NewId(), DestinationClientId: server.NewId(),
			CreateTime: start, ContractId: server.NewId()}
		for _, scenario := range []string{"bootstrap", "continuation", "completed"} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key='["refresh_contract_holes"]'`))
				before := server.NowUtc()
				switch scenario {
				case "bootstrap":
					ScheduleRefreshContractHoles(clientSession, tx)
				case "continuation":
					server.Raise(RefreshContractHolesPost(&RefreshContractHolesArgs{PassStarted: start},
						&RefreshContractHolesResult{Cursor: cursor, Pairs: 1, FailedPairs: 3}, clientSession, tx))
				case "completed":
					server.Raise(RefreshContractHolesPost(&RefreshContractHolesArgs{PassStarted: start},
						&RefreshContractHolesResult{Pairs: 1, FailedPairs: 3}, clientSession, tx))
				}
				var runAt time.Time
				var args RefreshContractHolesArgs
				var argsJson string
				var maxTime int
				server.Raise(tx.QueryRow(ctx, `SELECT run_at,args_json,run_max_time_seconds FROM pending_task
 WHERE run_once_key='["refresh_contract_holes"]'`).Scan(&runAt, &argsJson, &maxTime))
				server.Raise(json.Unmarshal([]byte(argsJson), &args))
				if maxTime != 20 {
					t.Fatalf("task deadline=%d", maxTime)
				}
				if scenario == "completed" {
					if !runAt.Equal(start.Add(model.ContractHoleRefreshInterval).Truncate(time.Microsecond)) || args.Cursor != nil || args.FailedPairs != 0 {
						t.Fatalf("completion moved cadence: run=%s args=%+v", runAt, args)
					}
				} else if runAt.Before(before.Truncate(time.Microsecond)) || runAt.After(server.NowUtc()) {
					t.Fatalf("%s was not scheduled immediately: %s", scenario, runAt)
				}
				if scenario == "continuation" && (args.Cursor == nil || args.Cursor.ContractId != cursor.ContractId || !args.PassStarted.Equal(start) || args.FailedPairs != 3) {
					t.Fatalf("continuation lost cursor: %+v", args)
				}
			})
		}
	})
}

// Failed/superseded observations keep coverage unknown through later healthy
// pages. Only a whole later pass may advance the complete-source timestamp.
func TestContractHoleRefreshPartialCoverageStaysUnknownUntilHealthyPass(t *testing.T) {
	start := time.Unix(1000, 0)
	args := &RefreshContractHolesArgs{PassStarted: start}
	observeContractHoleRefreshPage(args, &model.ContractHoleRefreshPageResult{}, start.Add(time.Second))
	completed := testutil.ToFloat64(contractHoleRefreshCompleted)
	contractHoleRefreshAvailable.Set(1)
	cursor := &model.ContractHoleCursor{SourceClientId: server.NewId()}
	partial := observeContractHoleRefreshPage(args, &model.ContractHoleRefreshPageResult{Cursor: cursor, Pairs: 3, FailedPairs: 1}, start.Add(2*time.Second))
	if partial.FailedPairs != 1 || testutil.ToFloat64(contractHoleRefreshAvailable) != 0 || testutil.ToFloat64(contractHoleRefreshCompleted) != completed {
		t.Fatal("partial failure advanced complete coverage")
	}
	observeContractHoleRefreshPage(&RefreshContractHolesArgs{PassStarted: start, FailedPairs: partial.FailedPairs},
		&model.ContractHoleRefreshPageResult{Pairs: 3}, start.Add(3*time.Second))
	if testutil.ToFloat64(contractHoleRefreshAvailable) != 0 || testutil.ToFloat64(contractHoleRefreshFailedPairs) != 1 || testutil.ToFloat64(contractHoleRefreshCompleted) != completed {
		t.Fatal("healthy tail hid earlier failed pair")
	}
	observeContractHoleRefreshPage(&RefreshContractHolesArgs{PassStarted: start.Add(30 * time.Second)},
		&model.ContractHoleRefreshPageResult{Pairs: 6}, start.Add(31*time.Second))
	if testutil.ToFloat64(contractHoleRefreshAvailable) != 0 || testutil.ToFloat64(contractHoleRefreshFailedPairs) != 0 || testutil.ToFloat64(contractHoleRefreshCompleted) <= completed {
		t.Fatal("later complete pass did not recover coverage")
	}
}

// The actual task qualifies a populated initial pass. A later source acquisition
// failure returns no progress and preserves the exact unread cursor arguments.
func TestContractHoleRefreshTaskReadinessAndSourceFailure(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination, contract := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 VALUES($1,$2,$3,$4,$5,0)`, contract, server.NewId(), source, server.NewId(), destination))
		})
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		args := &RefreshContractHolesArgs{PassStarted: server.NowUtc()}
		result, err := RefreshContractHoles(args, clientSession)
		if err != nil || result == nil || !result.WarmReady || result.PairVisits != 1 || result.SuccessfulPairs != 1 || result.PositivePairs != 1 || result.FailedPairs != 0 || testutil.ToFloat64(contractHoleRefreshAvailable) != 1 {
			t.Fatalf("initial task did not establish readiness: result=%+v error=%v", result, err)
		}
		if receipt, err := model.ReadContractHoleReadiness(ctx); err != nil || receipt == nil || receipt.PairVisits != 1 {
			t.Fatalf("initial task receipt=%+v error=%v", receipt, err)
		}
		args.Cursor = &model.ContractHoleCursor{SourceClientId: source, DestinationClientId: destination, CreateTime: args.PassStarted, ContractId: contract}
		args.PairVisits, args.SuccessfulPairs, args.Pages = 17, 17, 3
		before, err := json.Marshal(args)
		server.Raise(err)
		failedCtx, cancel := context.WithCancel(ctx)
		cancel()
		failedSession := session.NewLocalClientSession(failedCtx, "0.0.0.0:0", nil)
		defer failedSession.Cancel()
		result, err = RefreshContractHoles(args, failedSession)
		after, encodeErr := json.Marshal(args)
		server.Raise(encodeErr)
		if err == nil || result != nil || string(before) != string(after) || testutil.ToFloat64(contractHoleRefreshAvailable) != 0 {
			t.Fatalf("source failure moved progress: result=%+v error=%v before=%s after=%s", result, err, before, after)
		}
		if !model.HasOpenContractHole(ctx, source, destination) {
			t.Fatal("readiness loss revoked an independent healthy pair")
		}
	})
}
