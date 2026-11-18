package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Daily free grants on a shard's private network, as the free grant wrote them
// before it skipped prober networks: one per day back from today.
func proberShardTestFreeGrants(t testing.TB, ctx context.Context, networkId server.Id, count int) []server.Id {
	t.Helper()
	grantIds := []server.Id{}
	server.Tx(ctx, func(tx server.PgTx) {
		grantIds = []server.Id{}
		for day := range count {
			startTime, endTime := FreeGrantWindow(server.NowUtc().Add(-time.Duration(day) * 24 * time.Hour))
			server.Raise(AddGrantTransferBalanceInTx(tx, ctx, networkId, GrantKindFree, 30*Gib, startTime, endTime))
		}
		rows, err := tx.Query(ctx, `SELECT balance_id FROM transfer_balance WHERE network_id=$1 AND grant_kind=$2`, networkId, GrantKindFree)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				grantIds = append(grantIds, id)
			}
		})
	})
	if len(grantIds) != count {
		t.Fatalf("fixture wrote %d free grants, want %d", len(grantIds), count)
	}
	return grantIds
}

// A drained shard whose network holds plain grants besides its own credit is
// removed whole, the grants with it: before, any second balance kept the shard
// draining forever (61,649 of Main's 77,348 shards on 2026-11-01). Each
// removed balance keeps its net escrow revision tombstone, as the credit does.
func TestProberShardReapRemovesPlainGrantsWithItsNetwork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		server.Raise(DrainProberShard(ctx, owner.Key))
		grantIds := proberShardTestFreeGrants(t, ctx, owner.NetworkId, 2)
		if _, _, balances := shardTestRows(t, ctx, owner); balances != 3 {
			t.Fatalf("fixture network holds %d balances, want the credit and two grants", balances)
		}
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatalf("a drained shard with plain grants was held: deleted=%t err=%v", deleted, err)
		}
		if n, c, b := shardTestRows(t, ctx, owner); n != 0 || c != 0 || b != 0 {
			t.Fatalf("the reap left network=%d clients=%d balances=%d", n, c, b)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var tombstones int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_balance_net_escrow_revision WHERE balance_id=ANY($1) AND revision>0`,
				append([]server.Id{owner.BalanceId}, grantIds...)).Scan(&tombstones))
			if tombstones != 3 {
				t.Fatalf("%d of 3 removed balances kept a revision tombstone", tombstones)
			}
		})
	})
}

// A second balance that is not a plain grant, or a plain grant with an
// unsettled escrow, still keeps the shard draining, with every row in place.
func TestProberShardReapKeepsANetworkWithAnotherObligation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, obligation := range []string{"paid", "pro", "unsettled_grant_escrow", "pending_grant_debit"} {
			owner := shardTestOwner(t, ctx, shardTestKey(0))
			server.Raise(DrainProberShard(ctx, owner.Key))
			grantId := proberShardTestFreeGrants(t, ctx, owner.NetworkId, 1)[0]
			contractId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				switch obligation {
				case "paid":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=1 WHERE balance_id=$1`, grantId))
				case "pro":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET pro=true WHERE balance_id=$1`, grantId))
				case "unsettled_grant_escrow":
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count,settled) VALUES($1,$2,1024,false)`, contractId, grantId))
				case "pending_grant_debit":
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,shard,debit_byte_count) VALUES($1,$2,get_byte(uuid_send($2::uuid),15)%16,1024)`, contractId, grantId))
				}
			})
			if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || deleted {
				t.Fatalf("%s: the reap removed a network with another obligation: deleted=%t err=%v", obligation, deleted, err)
			}
			if n, c, b := shardTestRows(t, ctx, owner); n != 1 || c != 1 || b != 2 {
				t.Fatalf("%s: the held shard lost rows: network=%d clients=%d balances=%d", obligation, n, c, b)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_escrow WHERE contract_id=$1`, contractId))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_debit_journal WHERE contract_id=$1`, contractId))
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_balance WHERE balance_id=$1`, grantId))
			})
			if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
				t.Fatalf("%s: the shard stayed held once the obligation was gone: deleted=%t err=%v", obligation, deleted, err)
			}
		}
	})
}

// The reap removes a shard client's location reliability and provider intent
// rows with the client, and leaves another client's.
func TestProberShardReapRemovesClientLocationReliability(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		peer := newEscrowSelectionTestClients(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			for _, clientId := range []server.Id{owner.ClientId, peer.providerId} {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_location_reliability(client_id,update_block_number) VALUES($1,1)`, clientId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_provider_intent(client_id,create_time) VALUES($1,now())`, clientId))
			}
		})
		server.Raise(DrainProberShard(ctx, owner.Key))
		if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
			t.Fatalf("shard was not reaped: deleted=%t err=%v", deleted, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			for _, check := range []struct {
				clientId server.Id
				want     int
			}{{owner.ClientId, 0}, {peer.providerId, 2}} {
				var rows int
				server.Raise(conn.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM network_client_location_reliability WHERE client_id=$1)+
					(SELECT count(*) FROM network_client_provider_intent WHERE client_id=$1)`, check.clientId).Scan(&rows))
				if rows != check.want {
					t.Fatalf("client %s holds %d location reliability and provider intent rows, want %d", check.clientId, rows, check.want)
				}
			}
		})
	})
}
