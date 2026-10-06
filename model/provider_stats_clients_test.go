package model

import (
	"reflect"
	"testing"

	"github.com/urnetwork/server"
)

// Keep the original population, including non-Public keys. A missing key is
// different from an unavailable query; the normal query error path still raises.
func TestProviderStatsClientsMembership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.ApplyDbMigrations = false
	env.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(t.Context(), `
				CREATE TEMP TABLE network_client (
					client_id uuid PRIMARY KEY, network_id uuid NOT NULL,
					active boolean NOT NULL, source_client_id uuid
				) ON COMMIT DROP;
				CREATE TEMP TABLE provide_key (
					client_id uuid, provide_mode integer,
					PRIMARY KEY (client_id, provide_mode)
				) ON COMMIT DROP;
			`))
			network, other := server.NewId(), server.NewId()
			multi, private, absent, inactive, child, foreign := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
			server.RaisePgResult(tx.Exec(t.Context(), `
				INSERT INTO network_client VALUES
					($1,$7,true,NULL), ($2,$7,true,NULL), ($3,$7,true,NULL),
					($4,$7,false,NULL), ($5,$7,true,$1), ($6,$8,true,NULL);
			`, multi, private, absent, inactive, child, foreign, network, other))
			server.RaisePgResult(tx.Exec(t.Context(), `
				INSERT INTO provide_key VALUES
					($1,0), ($1,1), ($1,2), ($1,3), ($2,1), ($3,3), ($4,3), ($5,3)
			`, multi, private, inactive, child, foreign))
			read := func(network server.Id) map[server.Id]int {
				got := map[server.Id]int{}
				rows, err := tx.Query(t.Context(), providerStatsClientsSQL, network)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var id server.Id
						server.Raise(rows.Scan(&id))
						got[id]++
					}
				})
				return got
			}
			if got := read(network); !reflect.DeepEqual(got, map[server.Id]int{multi: 1, private: 1}) {
				t.Fatal("provider membership or uniqueness changed")
			}
			if len(read(server.NewId())) != 0 {
				t.Fatal("empty network inherited another network's providers")
			}
			server.RaisePgResult(tx.Exec(t.Context(), `DELETE FROM provide_key WHERE client_id=$1`, private))
			if got := read(network); !reflect.DeepEqual(got, map[server.Id]int{multi: 1}) {
				t.Fatal("deleted keys did not immediately change provider membership")
			}
		}, server.OptNoRetry())
	})
}
