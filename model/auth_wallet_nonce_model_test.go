package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// TestRemoveExpiredWalletNonces verifies the reaper deletes expired nonces
// (regardless of `used`) and keeps unexpired ones (even used ones -- they age
// out at their short expiry). Guards the new indexable `expire_time <= $1`
// delete that replaced the unindexable `used = true OR expire_time <= $1`.
func TestRemoveExpiredWalletNonces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		insert := func(nonce string, expireTime time.Time, used bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO auth_wallet_nonce (nonce, expire_time, used) VALUES ($1, $2, $3)`,
					nonce, expireTime, used,
				))
			})
		}
		count := func() int {
			c := 0
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT COUNT(*) FROM auth_wallet_nonce`)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&c))
					}
				})
			})
			return c
		}

		insert("expired-unused", now.Add(-10*time.Minute), false)
		insert("expired-used", now.Add(-1*time.Minute), true)
		insert("active-unused", now.Add(5*time.Minute), false)
		insert("active-used", now.Add(5*time.Minute), true)

		removed := RemoveExpiredWalletNonces(ctx, now, 50000)
		if removed != 2 {
			t.Fatalf("removed = %d, want 2 (both expired, used or not)", removed)
		}
		// the two unexpired nonces remain, including the used-but-unexpired one
		if c := count(); c != 2 {
			t.Fatalf("remaining = %d, want 2 (unexpired kept)", c)
		}
	})
}
