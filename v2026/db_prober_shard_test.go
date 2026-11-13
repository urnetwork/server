package server

import "testing"

func TestProberShardMigrationFrom751PreservesExistingFunding(t *testing.T) {
	if index := sqlMigrationIndex(t, "CREATE TABLE prober_shard_run ("); index != 751 {
		t.Fatalf("probe shard migration index=%d, want751 for schema752", index)
	}
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 751)
		networkId, balanceId := NewId(), NewId()
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance(balance_id,network_id,start_time,end_time,start_balance_byte_count,balance_byte_count,net_revenue_nano_cents)
				VALUES($1,$2,now(),now()+interval '1 day',8192,4096,0)`, balanceId, networkId))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO prober_identity(singleton,network_id) VALUES(true,$1)`, networkId))
		})
		for range 2 {
			ApplyDbMigrations(ctx)
			assertAppendedMigrationArtifacts(t, ctx)
			Db(ctx, func(conn PgConn) {
				var balance int64
				var legacy Id
				var owners int
				Raise(conn.QueryRow(ctx, `SELECT balance_byte_count,(SELECT network_id FROM prober_identity WHERE singleton),
					(SELECT count(*) FROM prober_shard_run) FROM transfer_balance WHERE balance_id=$1`, balanceId).Scan(&balance, &legacy, &owners))
				if balance != 4096 || legacy != networkId || owners != 0 {
					t.Fatal("schema752 modified existing funding or synthesized shard ownership")
				}
			})
		}
	})
}
