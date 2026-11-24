// A hosted proxy's children keep the mint budget. Only the proxy's own
// session mint waits the hosted path's budget (session.MintHostedSession):
// the children's mints hold the owner's few registration slots (localclient
// MintAdmission, two shared and two reserved per proxy process, offered
// 1.4-2.0 Erlang at a 0.05-0.12s hold on 2026-11-18), so a longer wait there
// would turn away other devices' registrations, while a child Redis does not
// register in time is signed on its carried session unextended, not refused.
// Needs the local test PostgreSQL and Redis.
package model

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestHostedChildMintKeepsTheMintBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := session.WithStateQuerySource(t.Context(), session.StateQueryHostedMint)
		networkId, userId, deviceId, clientId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, "hosted-mint-budget-"+networkId.String(), userId)
		Testing_CreateDevice(ctx, networkId, deviceId, clientId, "hosted", "hosted")
		presented := session.NewByJwt(networkId, userId, "hosted-mint-budget", false, false).Client(deviceId, clientId)
		sid := server.NewId()
		presented.SessionId = &sid
		presented.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(2 * time.Hour))
		if _, err := session.RegisterNetworkSession(ctx, presented, "legacy_proxy", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$2,session_create_time=$3 WHERE client_id=$1`, clientId, sid, presented.CreateTime))
		})
		clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", presented)
		defer clientSession.Cancel()

		// Redis answers each step after 400ms: past the mint budget, within
		// the hosted one
		const late = 400 * time.Millisecond
		if late <= session.SessionMintRedisBudget || session.HostedRevocationCheckBudget <= late {
			t.Fatalf("the hold %s is not between the budgets %s and %s", late, session.SessionMintRedisBudget, session.HostedRevocationCheckBudget)
		}
		pop := server.Testing_DelayAuthRedisCommands(late)
		result, err := server.HandleError2(func() (*AuthNetworkClientResult, error) {
			return AuthNetworkClientFromParent(&AuthNetworkClientArgs{
				SourceClientId: &clientId, Description: "child", DeviceSpec: "child",
			}, clientSession)
		}, func(err error) (*AuthNetworkClientResult, error) { return nil, err })
		pop()
		if err != nil || result == nil || result.Error != nil || result.ByClientJwt == nil || result.ClientId == nil {
			t.Fatalf("the hosted child's mint failed while Redis answered after %s: %v %+v", late, err, result)
		}
		minted, err := session.ParseByJwt(ctx, *result.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		if minted.SessionId == nil || *minted.SessionId != sid {
			t.Fatalf("the hosted child carries session %v, want %s", minted.SessionId, sid)
		}
		// unextended: the mint did not wait for the late registration
		if minted.ExpiresAt == nil || minted.ExpiresAt.Time.After(presented.ExpiresAt.Time) {
			t.Fatalf("the hosted child expires %v, after the credential it was minted from (%v): its mint waited past the mint budget", minted.ExpiresAt, presented.ExpiresAt)
		}
	})
}
