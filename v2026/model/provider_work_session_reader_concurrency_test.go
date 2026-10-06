package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// A contract reader must admit another contract for the same provider while
// keeping both current and rolling session mutations outside its endpoint cut.
func TestProviderWorkSessionContractReadersPreserveOriginals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
		defer cancel()
		f.ctx = ctx
		var id server.Id
		server.Db(ctx, func(conn server.PgConn) {
			reader, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, reader)
			providerWorkLockEndpointsInTx(ctx, reader, f.sourceId, f.destinationId)
			if err := providerWorkRequireEndpointFencesInTx(ctx, reader, f.sourceId, f.destinationId); err != nil {
				t.Fatal("first contract reader did not retain its endpoint cut", err)
			}

			server.Db(ctx, func(other server.PgConn) {
				peer, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer rollbackCloseReportTestTransaction(ctx, peer)
				server.RaisePgResult(peer.Exec(ctx, "SET LOCAL lock_timeout='250ms'"))
				providerWorkLockEndpointsInTx(ctx, peer, f.destinationId, f.sourceId, f.destinationId)
				if err := providerWorkRequireEndpointFencesInTx(ctx, peer, f.sourceId, f.destinationId); err != nil {
					t.Fatal("same-provider contract reader serialized behind another reader", err)
				}
				server.Raise(peer.Commit(ctx))
			})

			server.Db(ctx, func(other server.PgConn) {
				writer, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer rollbackCloseReportTestTransaction(ctx, writer)
				server.RaisePgResult(writer.Exec(ctx, "SET LOCAL lock_timeout='250ms'"))
				if providerWorkLockSessionMutationInTx(ctx, writer, f.destinationId) {
					t.Fatal("current session writer bypassed a contract reader")
				}
				server.Raise(writer.Rollback(ctx))

				legacy, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer rollbackCloseReportTestTransaction(ctx, legacy)
				_, err = legacy.Exec(ctx, "UPDATE network_client_connection SET connected=false WHERE connection_id=$1", f.destinationConnectionId)
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
					t.Fatal("rolling session writer did not retry outside the shared endpoint cut")
				}
				server.Raise(legacy.Rollback(ctx))
			})

			// This is the actual no-escrow contract publisher and receipt replay,
			// while the first transaction still holds both endpoint read fences.
			id = f.contract(t)
			reservation := providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, ctx, id), id)
			if !reservation.Reservation.Complete || reservation.Reservation.Capacity != 121 {
				t.Fatal("concurrent contract publication lost its complete original")
			}
		})
		f.close(t, id)
	})
}
