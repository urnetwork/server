package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// These tests pin that every ByJwt-issuing path stamps the network's CURRENT Pro,
// re-derived from the source of truth (an in-window pro transfer_balance), rather than
// copying a caller's stale jwt claim or reading a possibly-stale cache. A ByJwt lasts
// expiryDuration (30 days), so a wrong Pro at issue time is frozen for the token's whole
// lifetime — which is how a Pro network ends up showing no Pro in the app.

// grant an in-window pro balance and refresh the cache, so the network is Pro.
func proJwtGrantPro(t testing.TB, ctx context.Context, networkId server.Id) {
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		err := AddProTransferBalanceInTx(
			tx, ctx, networkId, ByteCount(1024*1024*1024), now, now.Add(30*24*time.Hour),
		)
		connect.AssertEqual(t, err, nil)
	})
	UpdateProNetwork(ctx, networkId)
}

func proJwtParse(t testing.TB, ctx context.Context, signed *string) *jwt.ByJwt {
	connect.AssertEqual(t, signed != nil, true)
	byJwt, err := jwt.ParseByJwt(ctx, *signed)
	connect.AssertEqual(t, err, nil)
	return byJwt
}

func proJwtAuthClient(t testing.TB, sess *session.ClientSession, args *AuthNetworkClientArgs) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(args, sess)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, nil)
	return result
}

// A network that turns Pro AFTER the caller's token was minted: the issued client token
// must carry Pro=true (re-derived), not the caller's stale Pro=false claim. This is the
// reported bug — an app calling /network/auth-client with a pre-upgrade token showed no Pro.
func TestProJwtAuthClientRederivesUp(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		proJwtGrantPro(t, ctx, networkId)

		// caller presents a stale token minted before the upgrade (Pro=false)
		sess := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test", Pro: false,
		})
		result := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"})
		connect.AssertEqual(t, proJwtParse(t, ctx, result.ByClientJwt).Pro, true)
	})
}

// The reverse: a network that is NOT Pro must issue Pro=false even when the caller's token
// still claims Pro=true (e.g. a lapsed subscription) — the stale true is not honored either.
func TestProJwtAuthClientRederivesDown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		// no pro balance granted: the network is not Pro

		sess := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test", Pro: true,
		})
		result := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"})
		connect.AssertEqual(t, proJwtParse(t, ctx, result.ByClientJwt).Pro, false)
	})
}

// The existing-client re-auth branch (args.ClientId set) must also re-derive Pro, not copy
// the caller's stale claim.
func TestProJwtAuthClientReauthRederives(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)

		// create a client while the network is not yet Pro (stale-false token)
		sess := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test", Pro: false,
		})
		created := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"})
		connect.AssertEqual(t, proJwtParse(t, ctx, created.ByClientJwt).Pro, false)

		// the network becomes Pro, then the SAME client re-auths (still with a stale-false token)
		proJwtGrantPro(t, ctx, networkId)
		reauth := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{
			ClientId: created.ClientId, Description: "d", DeviceSpec: "s",
		})
		connect.AssertEqual(t, proJwtParse(t, ctx, reauth.ByClientJwt).Pro, true)
	})
}

// Pro at issue time is read from the source of truth (the db), not the read-through cache:
// a stale-false cache entry (e.g. a node whose entry predates the upgrade) must NOT get
// baked into the 30-day token. This pins the IsProFresh path.
func TestProJwtAuthClientReadsSourceOfTruthNotCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		proJwtGrantPro(t, ctx, networkId)

		// poison both cache tiers with a stale false, as if cached just before the upgrade
		setProNetworkLocal(networkId, false)
		setProNetworkCached(ctx, networkId, false)
		// the read-through cache lies...
		connect.AssertEqual(t, IsProNetwork(ctx, networkId), false)
		// ...but the source of truth is authoritative
		connect.AssertEqual(t, IsProNetworkFresh(ctx, networkId), true)

		// re-poison (the fresh read above refreshed the cache) and issue a token
		setProNetworkLocal(networkId, false)
		setProNetworkCached(ctx, networkId, false)
		sess := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test", Pro: false,
		})
		result := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"})
		// the token read the db, not the poisoned cache
		connect.AssertEqual(t, proJwtParse(t, ctx, result.ByClientJwt).Pro, true)
	})
}

// Client tokens must carry the ROOT jwt's create time, not a fresh one: the
// root-lineage convention (see AuthCodeCreate) lets all derivative auth be
// expired by expiring the root. Pins both AuthNetworkClient branches.
func TestClientJwtPreservesRootCreateTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)

		// a distinctive root create time, far from now
		rootCreateTime := server.CodecTime(server.NowUtc().Add(-45 * 24 * time.Hour))
		sess := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId, UserId: userId, NetworkName: "test",
			CreateTime: rootCreateTime,
		})

		// new-client branch
		result := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{Description: "d", DeviceSpec: "s"})
		newClientJwt := proJwtParse(t, ctx, result.ByClientJwt)
		connect.AssertEqual(t, newClientJwt.CreateTime.Equal(rootCreateTime), true)

		// re-auth branch (same client id presented back)
		reauthResult := proJwtAuthClient(t, sess, &AuthNetworkClientArgs{
			Description: "d", DeviceSpec: "s", ClientId: result.ClientId,
		})
		reauthJwt := proJwtParse(t, ctx, reauthResult.ByClientJwt)
		connect.AssertEqual(t, reauthJwt.CreateTime.Equal(rootCreateTime), true)
	})
}
