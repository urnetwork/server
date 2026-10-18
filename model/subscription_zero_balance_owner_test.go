// Cleanups read balance ids from table data, where the schema allows the zero
// id but its ownership key cannot be built. One such row must not stop them.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An expired zero-id grant shares retention's batch with an ordinary one. The
// batch admits the ordinary owner, still locks and retires both rows, and does
// not fail on every pass at the zero id.
func TestCompletedTransferBalanceRetentionRetiresZeroBalanceId(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		zeroId := server.Id{}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time=$2 WHERE balance_id=$1`, f.balanceId, now.Add(-8*24*time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time,
				start_balance_byte_count, balance_byte_count, net_revenue_nano_cents, purchase_token,
				subsidy_net_revenue_nano_cents, pro)
				SELECT $2, network_id, start_time, end_time, start_balance_byte_count, balance_byte_count,
					net_revenue_nano_cents, purchase_token, subsidy_net_revenue_nano_cents, pro
				FROM transfer_balance WHERE balance_id=$1`, f.balanceId, zeroId))
		}, server.TxReadCommitted, server.OptNoRetry())
		var retentionErr error
		server.HandleError(func() {
			removeCompletedTransferBalanceBatches(ctx, now.Add(-7*24*time.Hour))
		}, func(err error) { retentionErr = err })
		if retentionErr != nil {
			t.Fatal("a zero balance id failed its retention batch:", retentionErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var remaining int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_balance WHERE balance_id=ANY($1)`,
				[]server.Id{zeroId, f.balanceId}).Scan(&remaining))
			if remaining != 0 {
				t.Fatal("expired grants were retained", remaining)
			}
		})
	})
}

// A due contract holds an escrow row with the zero balance id beside an
// ordinary one, and a later due contract waits behind it at the head of the
// reap index. The slice admits the ordinary owner and deletes both contracts
// with their rows, instead of failing that same slice on every pass.
func TestDueContractReapRetiresZeroBalanceEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		first, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
		server.Raise(err)
		later, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
		server.Raise(err)
		now := server.NowUtc()
		old := now.Add(-400 * 24 * time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=$1`, first, old))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=$1`, later, old.Add(time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,0),($1,$3,0)`,
				first, server.Id{}, f.balanceId))
		}, server.TxReadCommitted, server.OptNoRetry())
		var reapErr error
		server.HandleError(func() {
			removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 8)
		}, func(err error) { reapErr = err })
		if reapErr != nil {
			t.Fatal("a zero balance id failed its reap slice:", reapErr)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var contracts, escrows int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)),
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1))`,
				[]server.Id{first, later}).Scan(&contracts, &escrows))
			if contracts != 0 || escrows != 0 {
				t.Fatal("due contracts or their escrow rows were retained", contracts, escrows)
			}
		})
	})
}
