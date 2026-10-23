package main

import (
	"context"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// A migration owns transfer_escrow_sweep.destination_id, so the backfill
// command changes no schema. An open reader of the table blocks only a schema
// change. The command must backfill while the reader stays open, and nothing
// may wait behind it.
func TestDbBackfillSweepDestinationIdTakesNoSchemaLock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		var commandDone chan any
		defer func() {
			cancel()
			if commandDone != nil {
				<-commandDone
			}
		}()
		networkId, contractId, destinationId := server.NewId(), server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
				VALUES($1,$2,$2,$2,$3,0)`, contractId, networkId, destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents)
				VALUES($1,$2,$2,0,0)`, contractId, networkId))
		}, server.TxReadCommitted, server.OptNoRetry())
		opts, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage,
			[]string{"db", "backfill-sweep-destination-id", "--batch=10"}, "test")
		if err != nil {
			t.Fatal(err)
		}
		reader, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Release()
		held, err := reader.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		var readerPid int32
		server.Raise(held.QueryRow(ctx, `SELECT pg_backend_pid() FROM (SELECT count(*) FROM transfer_escrow_sweep) AS reader`).Scan(&readerPid))
		commandDone = make(chan any, 1)
		go func() {
			commandDone <- server.HandleError(func() {
				dbBackfillSweepDestinationId(opts)
			})
		}()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for commandDone != nil {
			select {
			case commandErr := <-commandDone:
				commandDone = nil
				if commandErr != nil {
					t.Fatal("backfill failed", commandErr)
				}
			case <-tick.C:
				var waiting bool
				server.Raise(held.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
					WHERE NOT granted AND $1::integer = ANY(pg_blocking_pids(pid)))`, readerPid).Scan(&waiting))
				if waiting {
					t.Fatal("the backfill waited behind an open reader, which only a schema change does")
				}
			case <-ctx.Done():
				t.Fatal("backfill did not finish")
			}
		}
		server.Raise(held.Rollback(ctx))
		server.Db(ctx, func(conn server.PgConn) {
			var backfilled *server.Id
			server.Raise(conn.QueryRow(ctx, `SELECT destination_id FROM transfer_escrow_sweep WHERE contract_id=$1`, contractId).Scan(&backfilled))
			if backfilled == nil || *backfilled != destinationId {
				t.Fatal("backfill did not copy the contract's destination", backfilled)
			}
		})
	})
}
