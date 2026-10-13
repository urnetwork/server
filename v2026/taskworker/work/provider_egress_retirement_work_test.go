package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

func TestProviderEgressCredentialsRetireReconstructedClientOnlyOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
		defer cancel()
		credentials, _ := providerEgressCredentialFixture(t)
		child, foreign := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			for id, network := range map[server.Id]server.Id{
				child: credentials.networkId, foreign: server.NewId(), credentials.clientId: credentials.networkId,
			} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client
					(client_id,network_id,active,create_time,auth_time) VALUES($1,$2,true,$3,$3)`, id, network, server.NowUtc()))
			}
		})
		// Construction retains the actual model retirement function. Only parent
		// parsing/state validation are fixture-controlled; the child was restored
		// from durable cleanup state and is deliberately absent from the mint map.
		if _, present := credentials.children.Load(child); present {
			tb.Fatal("reconstructed cleanup unexpectedly had in-memory ownership")
		}
		var firstXmin string
		var firstTime time.Time
		for attempt := 0; attempt < 3; attempt++ {
			result, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(child)})
			if err != nil || result == nil {
				tb.Fatal("actual reconstructed retirement failed", err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var xmin string
				var at time.Time
				var active bool
				server.Raise(conn.QueryRow(ctx, `SELECT xmin::text,deactivate_time,active FROM network_client WHERE client_id=$1`, child).Scan(&xmin, &at, &active))
				if active || at.IsZero() {
					tb.Fatal("actual retirement did not deactivate its client")
				}
				if attempt == 0 {
					firstXmin, firstTime = xmin, at
				} else if xmin != firstXmin || !at.Equal(firstTime) {
					tb.Fatal("private credential constructor repeated completed model work")
				}
			})
		}
		for _, id := range []server.Id{foreign, credentials.clientId, server.NewId()} {
			if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(id)}); err == nil {
				tb.Fatal("foreign, parent, or missing client passed private retirement")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var active int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE client_id=ANY($1) AND active`, []server.Id{foreign, credentials.clientId}).Scan(&active))
			if active != 2 {
				tb.Fatal("private retirement changed foreign or parent state")
			}
		})
	})
}
