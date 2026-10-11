package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Observe the real participant queries through the owning PgTx. A second
// PostgreSQL transaction must still acquire the shared grant at each read;
// the outcome/debit owner retains its contract lock throughout.
type settlementParticipantLockScopeTx struct {
	server.PgTx
	observe func()
}

func (self *settlementParticipantLockScopeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM contract_extender") {
		self.observe()
	}
	return self.PgTx.Query(ctx, sql, args...)
}

func TestSettlementParticipantReadsDoNotHoldSharedGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		{
			f := newNetEscrowOrderingTestFixture(t, ctx)
			escrow := createRedisAdmissionTest(ctx, f, 64)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=true WHERE contract_id=$1`, escrow.ContractId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint) VALUES($1,'source',11,$2,false),($1,'destination',11,$2,false)`, escrow.ContractId, server.NowUtc()))
			})
			probes, blocked := 0, 0
			probe := func() {
				probes++
				conn := acquireContractLifecycleTestConnection(t, ctx)
				defer conn.Release()
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(context.Background())
				_, err = tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, f.balanceId)
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
					blocked++
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			var posts []func() any
			server.Tx(ctx, func(tx server.PgTx) {
				var closed bool
				var err error
				posts, closed, err = settleEscrowInTx(ctx, &settlementParticipantLockScopeTx{PgTx: tx, observe: probe}, escrow.ContractId, ContractOutcomeSettled)
				server.Raise(err)
				if !closed {
					t.Fatal("outcome not claimed")
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			server.RunPosts(ctx, posts...)
			_, _, _, flushErr := flushTransferDebitBalance(ctx, f.balanceId)
			server.Raise(flushErr)
			var remaining ByteCount
			var raw []byte
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1`, f.balanceId).Scan(&remaining))
				server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, escrow.ContractId).Scan(&raw))
			})
			usage, err := decodeContractUsageSnapshot(raw)
			if err != nil || usage.ByteCount != 11 || remaining != 989 {
				t.Fatalf("financial owner changed: remaining=%d usageError=%v", remaining, err)
			}
			if probes < 2 {
				t.Fatal("billing and usage participant reads not exercised")
			}
			if blocked != 0 {
				t.Errorf("participant lookups held shared grant: blocked=%d/%d", blocked, probes)
			}
		}
	})
}
