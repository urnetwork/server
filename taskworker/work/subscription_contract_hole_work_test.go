package work

// The real task scheduler retains the half-TTL cadence and durable page cursor.

import (
	"encoding/json"
	"testing"
	"time"

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
						&RefreshContractHolesResult{Cursor: cursor, Pairs: 1}, clientSession, tx))
				case "completed":
					server.Raise(RefreshContractHolesPost(&RefreshContractHolesArgs{PassStarted: start},
						&RefreshContractHolesResult{Pairs: 1}, clientSession, tx))
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
					if !runAt.Equal(start.Add(model.ContractHoleRefreshInterval).Truncate(time.Microsecond)) || args.Cursor != nil {
						t.Fatalf("completion moved cadence: run=%s args=%+v", runAt, args)
					}
				} else if runAt.Before(before.Truncate(time.Microsecond)) || runAt.After(server.NowUtc()) {
					t.Fatalf("%s was not scheduled immediately: %s", scenario, runAt)
				}
				if scenario == "continuation" && (args.Cursor == nil || args.Cursor.ContractId != cursor.ContractId || !args.PassStarted.Equal(start)) {
					t.Fatalf("continuation lost cursor: %+v", args)
				}
			})
		}
	})
}
