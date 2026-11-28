package server

// The ctf_award network migration (ctf_schema.go, ctfAwardNetworkSql) over
// awards written before it.

import (
	"context"
	"fmt"
	"testing"
)

// Each award written before the migration gets the network it was won for:
// the network the census member of its epoch, role and client names, else
// the network of its client's network_client row. With neither it stays
// null. Run again, the migration changes nothing, and the head still applies.
func TestCtfAwardNetworkMigrationBackfillsKnownNetworks(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		version := CtfAwardNetworkMigrationVersion()
		if version == 0 {
			t.Fatal("the ctf award network migration is not in the migration list")
		}
		ApplyDbMigrationsUpTo(ctx, version-1)

		key := "ctf-award-network-test"
		member, both, client, unknown := NewId(), NewId(), NewId(), NewId()
		networkA, networkB, networkC := NewId(), NewId(), NewId()
		coldkey := make([]byte, 32)
		unit := make([]byte, 32)
		MaintenanceTx(ctx, func(tx PgTx) {
			// census members of epoch 8's providers: one only a member (of C),
			// one a member (of A) with a network_client row (of B)
			for _, row := range []struct {
				clientId  Id
				networkId Id
			}{{member, networkC}, {both, networkA}} {
				RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO ctf_census_member (
						deployment_key, no_id, epoch, role, client_id, network_id, candidate, reason, failed,
						country_code, draw_country_code, present_samples, unit, unit_family, coldkey
					)
					VALUES ($1, 1, 8, 1, $2, $3, true, 0, 0, 'is', 'is', 400, $4, 4, $5)
					`,
					key, row.clientId, row.networkId, unit, coldkey,
				))
			}
			// network_client rows: the member of A's (of B), and one only a
			// client (of B)
			for _, clientId := range []Id{both, client} {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id) VALUES ($1, $2)`, clientId, networkB))
			}
			// the awards: epoch 8's providers, the member of A's extender win
			// (no member of that role) and its provider win of epoch 9 (no
			// member of that epoch), and a client known to neither
			for _, award := range []struct {
				epoch    int
				role     int
				country  string
				clientId Id
			}{
				{8, 1, "is", member}, {8, 1, "de", both}, {8, 2, "de", both}, {9, 1, "is", both},
				{8, 1, "fr", client}, {8, 1, "tm", unknown},
			} {
				RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO ctf_award (
						deployment_key, no_id, epoch, role, country_code, rank, unit, unit_family,
						client_id, coldkey, slot_rao, pot_rao, deferred
					)
					VALUES ($1, 1, $2, $3, $4, 1, $5, 4, $6, $7, 100, 0, false)
					`,
					key, award.epoch, award.role, award.country, unit, award.clientId, coldkey,
				))
			}
		})

		networks := func() map[string]string {
			read := map[string]string{}
			MaintenanceDb(ctx, func(conn PgConn) {
				result, err := conn.Query(ctx, `SELECT epoch, role, country_code, network_id FROM ctf_award WHERE deployment_key = $1`, key)
				WithPgResult(result, err, func() {
					for result.Next() {
						var epoch int64
						var role int16
						var country string
						var networkId *Id
						Raise(result.Scan(&epoch, &role, &country, &networkId))
						network := "null"
						if networkId != nil {
							network = networkId.String()
						}
						read[fmt.Sprintf("%d %d %s", epoch, role, country)] = network
					}
				})
			})
			return read
		}
		want := map[string]string{
			// the census member's network, whatever network_client says
			"8 1 is": networkC.String(),
			"8 1 de": networkA.String(),
			// no member of that role or that epoch: network_client's
			"8 2 de": networkB.String(),
			"9 1 is": networkB.String(),
			"8 1 fr": networkB.String(),
			// neither
			"8 1 tm": "null",
		}

		ApplyDbMigrationsUpTo(ctx, version)
		if got := networks(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("backfilled networks %v, want %v", got, want)
		}
		var indexed bool
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE tablename = 'ctf_award' AND indexname = 'ctf_award_network')`).Scan(&indexed))
		})
		if !indexed {
			t.Fatal("ctf_award_network is not created")
		}

		// run again it changes nothing: a row the census or network_client
		// learns of later is not rewritten, and the unknown one stays null
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET network_id = $2 WHERE client_id = $1`, client, networkC))
			RaisePgResult(tx.Exec(ctx, ctfAwardNetworkSql))
		})
		if got := networks(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run again: networks %v, want %v", got, want)
		}

		ApplyDbMigrations(ctx)
	})
}
