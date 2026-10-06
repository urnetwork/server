// Connection writers for unrelated clients must progress while one writer is held.
package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// A real legacy mutation remains uncommitted until all unrelated mixed writes
// and a signed contract finish. The held transaction, rather than elapsed time,
// proves the absence of a global writer queue.
func TestProviderWorkSessionDistinctClientWritesDoNotQueueBehindLegacyWriter(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, time.Minute)
		defer cancel()
		const clientCount = 128
		clientIds := make([]server.Id, clientCount+1)
		connectionIds := make([]server.Id, clientCount+1)
		clients := map[server.Id]server.Id{}
		for i := range clientIds {
			clientIds[i] = contractPayoutTestId(byte(i + 1))
			connectionIds[i] = server.NewId()
			clients[clientIds[i]] = f.sourceNetworkId
		}
		addContractPayoutTestClients(ctx, clients)
		const insertSql = `INSERT INTO network_client_connection
		 (client_id,connection_id,connect_time,connection_host,connection_service,connection_block,
		 client_address_hash,client_address_port,handler_id)
		 VALUES($1,$2,$3,'synthetic.example','test','test',$4,10001,$5)`
		server.Tx(ctx, func(tx server.PgTx) {
			var distinctKeys int
			server.Raise(tx.QueryRow(ctx, `SELECT count(DISTINCT ('x'||substr(md5(client_id::text),1,8))::bit(32)::int)
			 FROM unnest($1::uuid[]) AS client_id`, clientIds).Scan(&distinctKeys))
			if distinctKeys != len(clientIds) {
				t.Fatal("synthetic clients contain an advisory-key collision")
			}
			for i, clientId := range clientIds {
				server.RaisePgResult(tx.Exec(ctx, insertSql, clientId, connectionIds[i], server.NowUtc(), make([]byte, 32), f.handlerId))
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			held, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, held)
			server.RaisePgResult(held.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, connectionIds[clientCount], server.NowUtc()))

			jobs := make(chan int, clientCount)
			results := make(chan error, clientCount)
			for i := range clientCount {
				jobs <- i
			}
			close(jobs)
			var workers sync.WaitGroup
			for range 16 {
				workers.Go(func() {
					for i := range jobs {
						result := server.HandleError(func() {
							server.Tx(ctx, func(tx server.PgTx) {
								// A failure bound also makes the old global bridge
								// fail promptly while the barrier remains held.
								server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`))
								switch i % 4 {
								case 0:
									server.RaisePgResult(tx.Exec(ctx, insertSql, clientIds[i], server.NewId(), server.NowUtc(), make([]byte, 32), f.handlerId))
								case 1:
									server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, connectionIds[i], server.NowUtc()))
								case 2:
									server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET ip_family_intent=ip_family_intent WHERE connection_id=$1`, connectionIds[i]))
								case 3:
									server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_connection WHERE connection_id=$1`, connectionIds[i]))
								}
							}, server.OptNoRetry())
						})
						var resultErr error
						if result != nil {
							resultErr = fmt.Errorf("unrelated writer %d: %v", i, result)
						}
						results <- resultErr
					}
				})
			}
			workers.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			id, err := CreateContractNoEscrow(f.requestContext(t, nil), f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
			if err != nil || !providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, ctx, id), id).Reservation.Complete {
				t.Fatal("held unrelated legacy writer prevented a complete reservation", err)
			}
			// The held writer still owns its row and journal event; all other
			// commits above happened before releasing that exact transaction.
			server.Raise(held.Rollback(ctx))
		})
		server.Db(ctx, func(conn server.PgConn) {
			for i, clientId := range clientIds {
				want := 2
				if i == clientCount || i%4 == 2 {
					want = 1
				}
				var sequence, events, receipts int
				server.Raise(conn.QueryRow(ctx, `SELECT sequence,
				 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
				 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)
				 FROM provider_work_session_head WHERE client_id=$1`, clientId).Scan(&sequence, &events, &receipts))
				if sequence != want || events != want || receipts != 0 {
					t.Fatalf("writer %d retained sequence/events/receipts %d/%d/%d, want %d/%d/0", i, sequence, events, receipts, want, want)
				}
			}
		})
	})
}
