// Readiness failures preserve the original financial task and its custody.
package work

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// An absent optional payer index must not turn a previously serviceable shard
// into an endlessly failing dispatcher during migration or mixed deployment.
func TestLegacyPayerDispatchMissingDueIndexKeepsFinancialFallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		payer, source, provider, destination := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, client := range []struct{ networkId, clientId server.Id }{
			{networkId: payer, clientId: source},
			{networkId: provider, clientId: destination},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, client.networkId, "synthetic-payer-fallback-"+client.networkId.String(), server.NewId()))
			})
			model.Testing_CreateDevice(ctx, client.networkId, server.NewId(), client.clientId, "synthetic-payer-fallback", "synthetic")
		}
		model.AddBasicTransferBalance(ctx, payer, 1000, server.NowUtc(), server.NowUtc().Add(time.Hour))
		balances := model.GetActiveTransferBalances(ctx, payer)
		if len(balances) != 1 {
			t.Fatal("synthetic fallback requires one grant")
		}
		id := server.NewId()
		shard := int(id[15]) % 16
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
 VALUES($1,$2,$3,$4,$5,$2,100,true)`, id, payer, source, provider, destination))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,100)`, id, balances[0].BalanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',11,false),($1,'destination',11,false)`, id))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome) VALUES($1,$2,'settled')`, id, shard))
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX legacy_settlement_intent_payer_due`))
		})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		args := &FlushLegacySettlementsArgs{Shard: shard}
		result, err := FlushLegacySettlements(args, owner)
		if err != nil || result.Dispatch != nil || result.Completed != 1 || result.Failed != 0 {
			t.Fatal("readiness failure dropped original financial service", result, err)
		}
		withLegacyDispatcherQueueTestTx(ctx, shard, result, func(tx server.PgTx) {
			server.Raise(FlushLegacySettlementsPost(args, result, owner, tx))
			var exact bool
			server.Raise(tx.QueryRow(ctx, `SELECT
 (SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
 AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=989
 AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1)=11
 AND EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=$3)`, id, balances[0].BalanceId, "[\"flush_legacy_settlements_"+fmt.Sprint(shard)+"\"]").Scan(&exact))
			if !exact {
				t.Fatal("fallback lost money or its recurring recovery owner")
			}
		})
	})
}
