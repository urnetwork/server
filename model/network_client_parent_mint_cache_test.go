package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// A fresh entitlement read is preparation, not permission to publish. Seed
// stale Pro caches, lapse the authoritative row and revoke the parent, then
// require the refused mint to leave both caches and child storage untouched.
func TestAuthNetworkClientFromParentDoesNotPublishRefusedEntitlement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, "refused-mint-cache", userId)
		Testing_CreateDevice(ctx, networkId, deviceId, clientId, "original", "original")
		claims := jwt.NewByJwt(networkId, userId, "refused-mint-cache", false, false).Client(deviceId, clientId)
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", claims)
		defer clientSession.Cancel()
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024*1024), now, now.Add(time.Hour)))
		})
		if !IsProFresh(ctx, &networkId) {
			t.Fatal("fixture did not seed authoritative Pro caches")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET pro=false WHERE network_id=$1`, networkId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, clientId))
		})
		loaded := false
		hook := func(id server.Id) {
			if id == networkId {
				loaded = true
			}
		}
		testingProNetworkLoaded.Store(&hook)
		defer testingProNetworkLoaded.Store(nil)
		result, err := server.HandleError2(func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClientFromParent(&AuthNetworkClientArgs{
				SourceClientId: &clientId, Description: "changed", DeviceSpec: "changed",
			}, clientSession)
		}, func(err error) (*AuthNetworkClientResult, error) { return nil, err })
		if !loaded || result != nil || !errors.Is(err, ErrClientParentInactive) {
			t.Fatal("refused mint did not reach its fresh entitlement and authority boundary")
		}
		local, localOK, cached, cachedOK := Testing_ProNetworkCacheEntries(ctx, networkId)
		if !localOK || !cachedOK || !local || !cached {
			t.Fatal("refused mint published its preliminary entitlement")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var children int
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE source_client_id=$1`, clientId).Scan(&children))
			if children != 0 {
				t.Fatal("refused mint wrote a child")
			}
		})
	})
}
