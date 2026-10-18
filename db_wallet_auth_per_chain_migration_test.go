package server

// The migration that keys network_user_auth_wallet by (user_id, blockchain).

import (
	"context"
	"slices"
	"testing"
)

func walletAuthPerChainMigrationIndex(t testing.TB) int {
	return sqlMigrationIndex(t, "network_user_auth_wallet_pkey PRIMARY KEY (user_id, blockchain)")
}

// Production holds at most one wallet row per user (the old PRIMARY KEY
// (user_id)). The key swap must apply over that data unchanged, and afterwards
// a user may hold a second row for another chain but not a second row for the
// same label, while a wallet still binds to one account.
func TestWalletAuthPerChainMigrationAppliesOverOneWalletPerUser(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		index := walletAuthPerChainMigrationIndex(t)
		ApplyDbMigrationsUpTo(ctx, index)

		solUserId := NewId()
		taoUserId := NewId()
		legacyUserId := NewId()
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, user := range []struct {
				id         Id
				address    string
				blockchain string
			}{
				{solUserId, "migration-sol-wallet", "SOL"},
				{taoUserId, "migration-tao-wallet", "TAO"},
				// the legacy label MigrateNetworkUserChildAuths wrote
				{legacyUserId, "migration-legacy-wallet", "solana"},
			} {
				// the foreign key is NOT VALID but still checks new rows
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_user (user_id, user_name, auth_type, wallet_address, wallet_blockchain)
					VALUES ($1, 'migration-wallet', 'solana', $2, $3)
				`, user.id, user.address, user.blockchain))
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_user_auth_wallet (user_id, wallet_address, blockchain)
					VALUES ($1, $2, $3)
				`, user.id, user.address, user.blockchain))
			}
		})

		ApplyDbMigrationsUpTo(ctx, index+1)
		if version := DbVersion(ctx); version != index+1 {
			t.Fatalf("DB version = %d, want %d", version, index+1)
		}

		var keyColumns []string
		var constraintNames []string
		var rowCount int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `
				SELECT array_agg(a.attname::text ORDER BY k.ordinality)
				FROM pg_index i
				CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ordinality)
				JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
				WHERE i.indrelid = 'network_user_auth_wallet'::regclass AND i.indisprimary
			`).Scan(&keyColumns))
			Raise(conn.QueryRow(ctx, `
				SELECT array_agg(conname::text ORDER BY conname)
				FROM pg_constraint
				WHERE conrelid = 'network_user_auth_wallet'::regclass
			`).Scan(&constraintNames))
			Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_user_auth_wallet`).Scan(&rowCount))
		}, OptReadOnly())
		if !slices.Equal(keyColumns, []string{"user_id", "blockchain"}) {
			t.Fatalf("primary key columns = %v, want [user_id blockchain]", keyColumns)
		}
		for _, name := range []string{
			"network_user_auth_wallet_pkey",
			"network_user_auth_wallet_user_fk",
			"network_user_auth_wallet_wallet_address_blockchain_key",
		} {
			if !slices.Contains(constraintNames, name) {
				t.Fatalf("constraints = %v, want %s kept", constraintNames, name)
			}
		}
		if rowCount != 3 {
			t.Fatalf("wallet rows = %d after the migration, want the 3 written before it", rowCount)
		}

		insert := func(userId Id, address string, blockchain string) (err any) {
			defer func() {
				err = recover()
			}()
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO network_user_auth_wallet (user_id, wallet_address, blockchain)
					VALUES ($1, $2, $3)
				`, userId, address, blockchain))
			}, OptNoRetry())
			return nil
		}
		// a second chain for one user is a second row
		if err := insert(solUserId, "migration-sol-user-tao-wallet", "TAO"); err != nil {
			t.Fatalf("a TAO wallet for a user holding a SOL wallet was refused: %v", err)
		}
		// the same label twice for one user is still refused by the key
		if err := insert(taoUserId, "migration-second-tao-wallet", "TAO"); err == nil {
			t.Fatal("a second TAO row for one user passed the primary key")
		}
		// and a wallet still binds to one account per label
		if err := insert(legacyUserId, "migration-sol-wallet", "SOL"); err == nil {
			t.Fatal("UNIQUE (wallet_address, blockchain) no longer holds")
		}
	})
}
