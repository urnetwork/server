// Publication declares every typed close owner before its completion writes.
package work

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Equal UUID values remain two real pending tasks, while repeated dispatch
// retains each earliest deadline and the independent source-discovery cursor.
func TestLegacyCloseDispatcherPublishesIndependentSourceAndPayerOwners(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		id := server.NewId()
		cursor := &model.LegacySettlementPayerCursor{End: id, PassEndTime: server.NowUtc().Truncate(time.Microsecond)}
		registrationCursor := &model.LegacySettlementOwnerCursor{After: &id, End: server.NewId()}
		result := &FlushLegacySettlementsResult{Dispatch: &model.LegacySettlementDispatchResult{
			Private: true, PayerNetworkIds: []server.Id{id}, SourceClientIds: []server.Id{id}, SourceCursor: cursor, RegistrationCursor: registrationCursor, More: true,
		}}
		args := &FlushLegacySettlementsArgs{Shard: 3}
		withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
			server.Raise(FlushLegacySettlementsPost(args, result, clientSession, tx))
		})
		for _, owner := range []struct {
			kind   model.ContractCloseOwnerKind
			key    *task.RunOnceOption
			target task.Target
		}{
			{kind: model.ContractCloseOwnerPayerNetwork, key: task.RunOnce("flush_legacy_payer_settlements", id), target: model.NewLegacyPayerSettlementTaskTarget()},
			{kind: model.ContractCloseOwnerSourceClient, key: task.RunOnce("flush_legacy_source_settlements", id), target: model.NewLegacySourceSettlementTaskTarget()},
		} {
			var first time.Time
			server.Db(ctx, func(conn server.PgConn) {
				var function, raw string
				server.Raise(conn.QueryRow(ctx, `SELECT function_name,args_json,run_at FROM pending_task WHERE run_once_key=$1`, owner.key.String()).Scan(&function, &raw, &first))
				var pending model.LegacyPayerSettlementArgs
				server.Raise(json.Unmarshal([]byte(raw), &pending))
				if function != owner.target.TargetFunctionName() || pending.Owner == nil || pending.Owner.Kind != owner.kind || pending.Owner.Id != id || !pending.Private {
					t.Fatal("dispatcher changed the logical owner or omitted its registered target", function, raw)
				}
			})
			withLegacyDispatcherQueueTestTx(ctx, args.Shard, result, func(tx server.PgTx) {
				server.Raise(FlushLegacySettlementsPost(args, result, clientSession, tx))
				var next time.Time
				server.Raise(tx.QueryRow(ctx, `SELECT run_at FROM pending_task WHERE run_once_key=$1`, owner.key.String()).Scan(&next))
				if !first.Equal(next) {
					t.Fatal("repeated typed discovery postponed its existing owner", first, next)
				}
			})
		}
		server.Db(ctx, func(conn server.PgConn) {
			var raw []byte
			server.Raise(conn.QueryRow(ctx, `SELECT args_json FROM pending_task WHERE run_once_key=$1`, task.RunOnce("flush_legacy_settlements_3").String()).Scan(&raw))
			var next FlushLegacySettlementsArgs
			server.Raise(json.Unmarshal(raw, &next))
			if next.SourceCursor == nil || next.SourceCursor.End != cursor.End || !next.SourceCursor.PassEndTime.Equal(cursor.PassEndTime) {
				t.Fatal("completion lost source discovery continuation", string(raw))
			}
			if next.RegistrationCursor == nil || next.RegistrationCursor.After == nil || *next.RegistrationCursor.After != id || next.RegistrationCursor.End != registrationCursor.End {
				t.Fatal("completion lost classification continuation", string(raw))
			}
		})
	})
}

func TestLegacyCloseDispatcherRejectsUnboundedOrEmptySourceOwners(t *testing.T) {
	target := NewLegacySettlementDispatcherTaskTarget().(task.TaskCompletionOwnershipTarget)
	tooMany := make([]server.Id, 17)
	for index := range tooMany {
		tooMany[index] = server.NewId()
	}
	for _, ids := range [][]server.Id{{{}}, tooMany} {
		raw, err := json.Marshal(FlushLegacySettlementsResult{Dispatch: &model.LegacySettlementDispatchResult{SourceClientIds: ids}})
		server.Raise(err)
		if _, err := target.TaskCompletionOwnershipKeys(nil, string(raw)); err == nil {
			t.Fatal("dispatcher admitted an invalid source publication scope", ids)
		}
	}
}
