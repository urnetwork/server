package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Statement triggers advance each affected balance exactly once, retaining
// disputed and signed historical rows while ignoring zero/settled/terminal
// reservations. Unchanged outcomes cannot amplify revision writes.
func TestNetEscrowRevisionContractBatchPreservesPredicateScope(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		balances := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count,settled)
				SELECT md5('revision-scope-'||n)::uuid,
				 ($1::uuid[])[CASE WHEN n<=2 THEN 1 ELSE n-1 END],
				 CASE WHEN n=4 THEN 0 WHEN n=7 THEN -1 ELSE 1 END,n=5
				FROM generate_series(1,7) n`, balances))
		})
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE", "UNCHANGED"} {
			server.Db(ctx, func(conn server.PgConn) {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(context.WithoutCancel(ctx))
				insert := func() {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
						(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,
						 transfer_byte_count,dispute,outcome,close_time)
						SELECT md5('revision-scope-'||n)::uuid,$1,$2,$3,$4,$1,1,n=3,
						 CASE WHEN n=6 THEN 'canceled' ELSE NULL END,
						 CASE WHEN n=6 THEN now() ELSE NULL END
						FROM generate_series(1,8) n`, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId))
				}
				read := func() []int64 {
					values := []int64{}
					rows, err := tx.Query(ctx, `SELECT COALESCE(revision,0)
						FROM unnest($1::uuid[]) WITH ORDINALITY AS selected(balance_id,ordinal)
						LEFT JOIN transfer_balance_net_escrow_revision USING(balance_id) ORDER BY ordinal`, balances)
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
							var v int64
							server.Raise(rows.Scan(&v))
							values = append(values, v)
						}
					})
					return values
				}
				if operation != "INSERT" {
					insert()
				}
				before := read()
				switch operation {
				case "INSERT":
					insert()
				case "UPDATE":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET outcome='canceled',close_time=now()`))
				case "DELETE":
					server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract`))
				case "UNCHANGED":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=NOT dispute`))
				}
				after := read()
				for i := range balances {
					want := before[i]
					if operation != "UNCHANGED" && (i == 0 || i == 1 || i == 5) {
						want++
					}
					if after[i] != want {
						t.Fatalf("%s balance category %d revision %d -> %d, want %d", operation, i, before[i], after[i], want)
					}
				}
			})
		}
	})
}
