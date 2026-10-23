package model

// The rendered contract id bound (contract_id_bound.go) is contract_id_floor's
// expression, inline, so a hot read never depends on the migration that
// creates the function. These tests hold the two to the same value, and the
// bounded reads' tests build ids minted at a chosen time.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An id as connect.NewId would mint it at the given time: the unix millisecond
// big-endian in bytes 0-5 and a random tail.
func contractIdMintedAt(at time.Time) server.Id {
	id := ContractIdTimeBound(at)
	for index := 6; index < len(id); index += 1 {
		id[index] = byte(rand.Uint32())
	}
	return id
}

// The helper's SQL equals contract_id_floor, and the id Go computes for the
// millisecond, on a millisecond boundary, one microsecond before it (which a
// rounding cast would carry into the boundary's millisecond), in both halves
// of a millisecond and across a daylight saving change, in UTC and in a
// session time zone with daylight saving.
func TestContractIdFloorSqlMatchesFunction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		boundary := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
		// America/Chicago leaves daylight saving at 2026-11-01 07:00 UTC
		fallBack := time.Date(2026, 11, 1, 7, 0, 0, 0, time.UTC)
		times := []time.Time{
			boundary,
			boundary.Add(-time.Microsecond),
			time.Date(2026, 10, 22, 13, 37, 5, 987654000, time.UTC),
			time.Date(2026, 10, 22, 13, 37, 5, 987123000, time.UTC),
			fallBack.Add(-time.Microsecond),
			fallBack.Add(500 * time.Microsecond),
			time.UnixMicro(1500).UTC(),
		}
		for _, timeZone := range []string{"UTC", "America/Chicago"} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL TIME ZONE '%s'`, timeZone)))
				for _, at := range times {
					var rendered, function server.Id
					server.Raise(tx.QueryRow(
						ctx,
						`SELECT `+contractIdFloorSql("$1::timestamptz")+`, contract_id_floor($1)`,
						at,
					).Scan(&rendered, &function))
					if rendered != function || rendered != ContractIdTimeBound(at) {
						t.Fatalf("%s at %s: rendered %s, contract_id_floor %s, want %s",
							timeZone, at.Format(time.RFC3339Nano), rendered, function, ContractIdTimeBound(at))
					}
				}

				// the hot reads' bound is the floor of the statement time less
				// the unexpired age, in the same statement
				var equal bool
				server.Raise(tx.QueryRow(
					ctx,
					`SELECT `+unexpiredContractIdFloorSql+` = contract_id_floor(statement_timestamp() - $1::interval)`,
					unexpiredContractIdAge,
				).Scan(&equal))
				if !equal {
					t.Fatalf("%s: the unexpired bound is not contract_id_floor of the statement time less %s", timeZone, unexpiredContractIdAge)
				}
			})
		}
	})
}

// An id minted now is inside the unexpired bound, and an id minted just past
// the unexpired age is outside it.
func TestUnexpiredContractIdFloorSqlBoundsMintedIds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		for _, test := range []struct {
			id     server.Id
			inside bool
		}{
			{id: server.NewId(), inside: true},
			{id: contractIdMintedAt(now.Add(-unexpiredContractIdAge + time.Minute)), inside: true},
			{id: contractIdMintedAt(now.Add(-unexpiredContractIdAge - time.Minute)), inside: false},
		} {
			var inside bool
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT $1::uuid >= `+unexpiredContractIdFloorSql, test.id).Scan(&inside))
			})
			if inside != test.inside {
				t.Fatalf("id %s inside the unexpired bound = %t, want %t", test.id, inside, test.inside)
			}
		}
	})
}
