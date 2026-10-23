package model

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// contract_id_floor (CONTRACT-PARTITION.md) is the smallest contract id of a
// millisecond: the 12 hex digits of the millisecond and a zero tail. An id
// minted in that millisecond sorts at or above it and below the next
// millisecond's floor, whatever the session time zone, and the function is
// immutable so a partition bound check can use it.
func TestContractIdFloor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		floor := func(timeZone string, at time.Time) (value string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL TIME ZONE '%s'`, timeZone)))
				server.Raise(tx.QueryRow(ctx, `SELECT contract_id_floor($1)::text`, at).Scan(&value))
			})
			return
		}
		expected := func(at time.Time) string {
			hex := fmt.Sprintf("%012x", at.UnixMilli())
			return hex[0:8] + "-" + hex[8:12] + "-0000-0000-000000000000"
		}

		monday := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
		within := time.Date(2026, 10, 22, 13, 37, 5, 987654321, time.UTC)
		for _, at := range []time.Time{monday, within} {
			connect.AssertEqual(t, expected(at), floor("UTC", at))
			connect.AssertEqual(t, expected(at), floor("America/Chicago", at))
		}

		// the largest id of the millisecond sorts at or above its floor and
		// below the next millisecond's
		var ordered bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT
					contract_id_floor($1) <= $2::uuid AND $2::uuid < contract_id_floor($1 + interval '1 millisecond') AND
					contract_id_floor($1 - interval '1 millisecond') < contract_id_floor($1)
			`, within, expected(within)[:13]+"-ffff-ffff-ffffffffffff").Scan(&ordered))
		})
		connect.AssertEqual(t, true, ordered)

		// immutable, so a CHECK constraint accepts it
		var volatility string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT provolatile::text FROM pg_proc WHERE proname = 'contract_id_floor'`).Scan(&volatility))
		})
		connect.AssertEqual(t, "i", volatility)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE TEMPORARY TABLE contract_id_floor_check (
					contract_id uuid NOT NULL CHECK (contract_id < contract_id_floor('2026-10-26 00:00:00+00'))
				) ON COMMIT DROP
			`))
		})
	})
}
