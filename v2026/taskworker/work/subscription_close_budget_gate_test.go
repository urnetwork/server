package work

import (
	"encoding/json"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"testing"
	"time"
)

func TestCloseBudgetZeroCountContinuesTask(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		client := session.Testing_CreateClientSession(ctx, nil)
		defer client.Cancel()
		stamp := server.NowUtc().Truncate(time.Microsecond)
		cursor := &model.ContractExpiryCursor{ScanBefore: stamp, Open: &model.ContractExpiryPosition{CreateTime: stamp.Add(-time.Hour), ContractId: server.NewId()}, DisputeDone: true}
		args := &CloseExpiredContractsArgs{BlockSize: 1, BlockIndex: 0}
		result, err := closeExpiredContractsPageResult(ctx, args, 0, cursor, nil)
		if err != nil || !result.Full || result.Cursor != cursor {
			t.Fatalf("zero-close raw progress parked: %+v %v", result, err)
		}
		server.Tx(ctx, func(tx server.PgTx) { server.Raise(CloseExpiredContractsPost(args, result, client, tx)) })
		var raw string
		var runAt time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT args_json,run_at FROM pending_task WHERE function_name=$1`, "github.com/urnetwork/server/v2026/taskworker/work.CloseExpiredContracts").Scan(&raw, &runAt))
		})
		var stored CloseExpiredContractsArgs
		server.Raise(json.Unmarshal([]byte(raw), &stored))
		if stored.Cursor == nil || stored.Cursor.Open == nil || stored.Cursor.Open.ContractId != cursor.Open.ContractId || !stored.Cursor.ScanBefore.Equal(stamp) || runAt.After(stamp.Add(10*time.Second)) {
			t.Fatalf("zero-close continuation lost cursor/cadence: cursor=%+v runAt=%v", stored.Cursor, runAt)
		}
	})
}
