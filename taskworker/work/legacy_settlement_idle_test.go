// The legacy dispatcher backs off while its partition is empty; its remaining
// producers wake it through the RunOnce merge.
package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// Through the real Post and queue row: the second consecutive empty fresh pass
// backs off to the idle delay, the first counts only, a pass that found owners
// or ended a round keeps the two-second cadence, and more work continues at
// once. Expiry recovery then pulls the backed-off dispatcher to now.
func TestLegacySettlementIdleDispatchBacksOffUntilWoken(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer owner.Cancel()
		settings := DefaultSettlementChainSettings()
		key := task.RunOnce("flush_legacy_settlements_3").String()
		readContinuation := func() (runAt time.Time, args FlushLegacySettlementsArgs) {
			var data []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT run_at,args_json FROM pending_task WHERE run_once_key=$1`, key).Scan(&runAt, &data))
			})
			server.Raise(json.Unmarshal(data, &args))
			return
		}
		fresh := &FlushLegacySettlementsArgs{Shard: 3}
		once := &FlushLegacySettlementsArgs{Shard: 3, EmptyPasses: 1}
		ended := &FlushLegacySettlementsArgs{Shard: 3, PayerCursor: &model.LegacySettlementPayerCursor{End: server.NewId()}, EmptyPasses: 1}
		for _, c := range []struct {
			name        string
			args        *FlushLegacySettlementsArgs
			dispatch    *model.LegacySettlementDispatchResult
			want        time.Duration
			emptyPasses int
		}{
			{name: "more", args: once, dispatch: &model.LegacySettlementDispatchResult{Private: true, More: true}, want: 0},
			{name: "probed", args: once, dispatch: &model.LegacySettlementDispatchResult{Private: true, Probes: 1}, want: settings.ActiveDelay},
			{name: "round_end", args: ended, dispatch: &model.LegacySettlementDispatchResult{Private: true}, want: settings.ActiveDelay},
			{name: "first_empty", args: fresh, dispatch: &model.LegacySettlementDispatchResult{Private: true}, want: settings.ActiveDelay, emptyPasses: 1},
			{name: "second_empty", args: once, dispatch: &model.LegacySettlementDispatchResult{Private: true}, want: settings.IdleDelay, emptyPasses: 2},
		} {
			result := &FlushLegacySettlementsResult{Dispatch: c.dispatch}
			before := server.NowUtc()
			withLegacyDispatcherQueueTestTx(ctx, 3, result, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE run_once_key=$1`, key))
				server.Raise(FlushLegacySettlementsPost(c.args, result, owner, tx))
			})
			after := server.NowUtc()
			runAt, next := readContinuation()
			if runAt.Before(before.Add(c.want)) || runAt.After(after.Add(c.want)) || next.EmptyPasses != c.emptyPasses {
				t.Fatal("dispatcher continuation has the wrong delay or count", c.name, runAt.Sub(before), c.want, next.EmptyPasses)
			}
		}
		// The last case left the dispatcher backed off. An explicit producer wake
		// merges into it at once.
		if _, err := QueueContractExpiryRecovery(ctx); err != nil {
			t.Fatal(err)
		}
		if runAt, _ := readContinuation(); runAt.After(server.NowUtc()) {
			t.Fatal("a producer wake did not pull the backed-off dispatcher to now", runAt)
		}
	})
}
