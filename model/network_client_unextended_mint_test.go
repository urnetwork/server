// The no-extend fallback for client mints. A client minted under a tagged
// credential carries the credential's session: a hosted proxy's child, which
// the proxy's local authority mints from the proxy's own credential
// (AuthNetworkClientFromParent), and an app's client, minted from its sign-in
// credential (AuthNetworkClient). While every Redis node's dials time out, the
// mint cannot register that session. Before, it failed unavailable, a 503 at
// the route; with the fallback the client is signed on the session, expiring
// no later than the credential it was minted from. Fixtures are synthetic.
package model

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestClientMintKeepsItsCarriedSessionWhileRedisDialsTimeOut(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, kind := range []string{"hosted child", "app client"} {
			ctx := session.WithStateQuerySource(t.Context(), session.StateQueryApiControl)
			networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
			Testing_CreateNetwork(ctx, networkId, "unextended-mint-"+networkId.String(), userId)
			Testing_CreateDevice(ctx, networkId, deviceId, clientId, "hosted", "hosted")
			presented := session.NewByJwt(networkId, userId, "unextended-mint", false, false)
			sessionKind := "password"
			if kind == "hosted child" {
				presented = presented.Client(deviceId, clientId)
				sessionKind = "legacy_proxy"
			}
			sid := server.NewId()
			presented.SessionId = &sid
			presented.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(2 * time.Hour))
			if _, err := session.RegisterNetworkSession(ctx, presented, sessionKind, nil, false, server.NowUtc()); err != nil {
				t.Fatal(err)
			}
			if kind == "hosted child" {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$2,session_create_time=$3 WHERE client_id=$1`, clientId, sid, presented.CreateTime))
				})
			}
			clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", presented)

			_, pop := server.Testing_StallAuthRedisDials(48)
			result, err := server.HandleError2(func() (*AuthNetworkClientResult, error) {
				if kind == "hosted child" {
					return AuthNetworkClientFromParent(&AuthNetworkClientArgs{
						SourceClientId: &clientId, Description: "child", DeviceSpec: "child",
					}, clientSession)
				}
				return AuthNetworkClient(&AuthNetworkClientArgs{Description: "app", DeviceSpec: "app"}, clientSession)
			}, func(err error) (*AuthNetworkClientResult, error) { return nil, err })
			pop()
			clientSession.Cancel()
			if err != nil || result == nil || result.Error != nil || result.ByClientJwt == nil || result.ClientId == nil {
				t.Fatalf("the %s mint failed while Redis did not answer: %v %+v", kind, err, result)
			}
			minted, err := session.ParseByJwt(ctx, *result.ByClientJwt)
			if err != nil {
				t.Fatal(err)
			}
			if minted.SessionId == nil || *minted.SessionId != sid {
				t.Fatalf("the %s carries session %v, want %s", kind, minted.SessionId, sid)
			}
			if minted.ClientId == nil || *minted.ClientId != *result.ClientId || *minted.ClientId == clientId {
				t.Fatalf("the %s token names client %v, want the new %s", kind, minted.ClientId, result.ClientId)
			}
			if minted.ExpiresAt == nil || minted.ExpiresAt.Time.After(presented.ExpiresAt.Time) {
				t.Fatalf("the %s expires %v, after the credential it was minted from (%v)", kind, minted.ExpiresAt, presented.ExpiresAt)
			}
			// the minted client is accepted once Redis answers
			if err := session.ValidateByJwtState(ctx, minted, true); err != nil {
				t.Fatalf("the %s is refused once Redis answers: %v", kind, err)
			}
		}
	})
}
