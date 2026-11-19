package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// TestProJwtRefreshTokenRederives pins that RefreshToken re-derives Pro from the source of
// truth on every refresh: a network that turned Pro after the client token was minted is
// reflected on the next refresh, without a re-login. (The app refreshes on a schedule and
// after an upgrade — this is the path that heals a stale Pro claim.)
func TestProJwtRefreshTokenRederives(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "test", userId)

		// create a client while the network is not Pro -> its token is Pro=false
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test", Pro: false,
		})
		authResult, err := model.AuthNetworkClient(
			&model.AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		clientByJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, clientByJwt.Pro, false)

		// the network becomes Pro
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := model.AddProTransferBalanceInTx(
				tx, ctx, networkId, model.ByteCount(1024*1024*1024), now, now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		model.UpdateProNetwork(ctx, networkId)

		// refreshing the (still Pro=false) client token re-derives Pro=true
		clientSession := session.Testing_CreateClientSession(ctx, clientByJwt)
		refreshResult, err := controller.RefreshToken(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refreshResult.Error, nil)
		refreshed, err := session.ParseByJwt(ctx, refreshResult.ByJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, refreshed.Pro, true)
	})
}

// A client token says whether UR Pro raises its network's concurrent client
// limit (client_limit_upgrade), and a refresh says it again for the plan the
// network has then. The apps offer the upgrade for a client limit hold only
// when it is true: a free network yes, a Pro network or an Embed allowance no.
// Before, a hold's prompt followed the pro claim alone, and offered the upgrade
// to a free network whose Embed allowance no plan lifts.
func TestClientLimitUpgradeClaimFollowsThePlan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetConcurrentClientsLimit(2, 1000)()
		defer model.Testing_ClearNetworkClientLimitCache()

		claim := func(byJwt string) *bool {
			parsed, err := session.ParseByJwt(ctx, byJwt)
			connect.AssertEqual(t, err, nil)
			return parsed.ClientLimitUpgrade
		}
		authClient := func(networkId server.Id, userId server.Id) string {
			userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
				NetworkId: networkId, UserId: userId, NetworkName: "test",
			})
			result, err := model.AuthNetworkClient(
				&model.AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"},
				userSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*model.AuthNetworkClientError)(nil))
			return *result.ByClientJwt
		}
		refresh := func(byJwt string) string {
			parsed, err := session.ParseByJwt(ctx, byJwt)
			connect.AssertEqual(t, err, nil)
			result, err := controller.RefreshToken(session.Testing_CreateClientSession(ctx, parsed))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, (*controller.RefreshTokenError)(nil))
			return result.ByJwt
		}

		// a free network: Pro raises its limit
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "free", userId)
		freeToken := authClient(networkId, userId)
		if upgrade := claim(freeToken); upgrade == nil || !*upgrade {
			t.Fatalf("a free network's client token claims %v, want true", upgrade)
		}
		if upgrade := claim(refresh(freeToken)); upgrade == nil || !*upgrade {
			t.Fatalf("a free network's refreshed token claims %v, want true", upgrade)
		}

		// the network turns Pro: the refresh says no upgrade raises it now
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := model.AddProTransferBalanceInTx(
				tx, ctx, networkId, model.ByteCount(1024*1024*1024), now, now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		model.UpdateProNetwork(ctx, networkId)
		if upgrade := claim(refresh(freeToken)); upgrade == nil || *upgrade {
			t.Fatalf("a Pro network's refreshed token claims %v, want false", upgrade)
		}
		if upgrade := claim(authClient(networkId, userId)); upgrade == nil || *upgrade {
			t.Fatalf("a Pro network's client token claims %v, want false", upgrade)
		}

		// a free network with an Embed allowance: no plan lifts it
		embedNetworkId, embedUserId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, embedNetworkId, "embed", embedUserId)
		connect.AssertEqual(t, model.SetNetworkTopLevelClientLimit(ctx, embedNetworkId, 5), nil)
		model.Testing_ClearNetworkClientLimitCache()
		embedToken := authClient(embedNetworkId, embedUserId)
		if upgrade := claim(embedToken); upgrade == nil || *upgrade {
			t.Fatalf("an Embed allowance's client token claims %v, want false", upgrade)
		}
		if upgrade := claim(refresh(embedToken)); upgrade == nil || *upgrade {
			t.Fatalf("an Embed allowance's refreshed token claims %v, want false", upgrade)
		}
	})
}
