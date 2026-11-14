// Child mint checks current parent authority in the same transaction that
// chooses the device and writes the child. Fixtures contain synthetic owners.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
)

// A committed revocation while the independent entitlement read is paused
// must be observed when the later mint transaction takes its first snapshot.
func TestAuthNetworkClientFromParentRechecksAfterEntitlementRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, mutation := range []string{"active", "rotation", "admin", "device"} {
			func() {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
				Testing_CreateNetwork(ctx, networkId, "parent-mint-"+mutation, userId)
				Testing_CreateDevice(ctx, networkId, deviceId, clientId, "original", "original")
				claims := session.NewByJwt(networkId, userId, "parent-mint-"+mutation, false, false).Client(deviceId, clientId)
				clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", claims)
				defer clientSession.Cancel()
				otherNetworkId, otherUserId, otherDeviceId, otherClientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
				Testing_CreateNetwork(ctx, otherNetworkId, "other-parent-mint-"+mutation, otherUserId)
				Testing_CreateDevice(ctx, networkId, otherDeviceId, otherClientId, "other", "other")
				hold := testingHoldProNetworkLoad(networkId)
				type mintResult struct {
					result *AuthNetworkClientResult
					err    error
				}
				joined := make(chan struct{})
				result := mintResult{}
				go func() {
					defer close(joined)
					result.result, result.err = server.HandleError2(func() (*AuthNetworkClientResult, error) {
						return AuthNetworkClientFromParent(&AuthNetworkClientArgs{
							SourceClientId: &clientId, Description: "changed", DeviceSpec: "changed",
						}, clientSession)
					}, func(err error) (*AuthNetworkClientResult, error) { return nil, err })
				}()
				defer func() {
					hold.Release()
					cancel()
					<-joined
				}()
				hold.WaitLoaded(t)
				server.Tx(ctx, func(tx server.PgTx) {
					switch mutation {
					case "active":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active=false WHERE client_id=$1`, clientId))
					case "rotation":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, userId, claims.CreateTime.Add(time.Minute)))
					case "admin":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET admin_user_id=$2 WHERE network_id=$1`, networkId, otherUserId))
					case "device":
						server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET device_id=$2 WHERE client_id=$1`, clientId, otherDeviceId))
					}
				})
				hold.Release()
				select {
				case <-joined:
				case <-ctx.Done():
					t.Fatal("mint did not finish after its pretransaction barrier", mutation)
				}
				if result.result != nil || !errors.Is(result.err, ErrClientParentInactive) {
					t.Fatalf("committed %s change did not refuse child mint: result=%t inactive=%t", mutation, result.result != nil, errors.Is(result.err, ErrClientParentInactive))
				}
				server.Db(ctx, func(conn server.PgConn) {
					var children int
					server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE source_client_id=$1`, clientId).Scan(&children))
					var spec string
					server.Raise(conn.QueryRow(ctx, `SELECT device_spec FROM device WHERE device_id=$1`, deviceId).Scan(&spec))
					if children != 0 || spec != "original" {
						t.Fatal("refused parent wrote a child or changed its device", mutation)
					}
				})
			}()
		}
	})
}
