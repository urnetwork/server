package model

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The apps' "Pro features in use" notice reads the proxies list: whether the
// network holds Pro now, whether enforce_features is on, and which Pro-only
// features each proxy used recently, as the proxies recorded them (a SOCKS
// sign-in, a WireGuard handshake). The list is read as an app reads it, and
// the notice decided by the sdk's own rule (testProFeaturesNotice).
//
// The proxies' uses are written as the proxy's flush writes them
// (redisProxyFeatureUseStore.markAll), at the times a test needs: yesterday,
// or before the window.
func TestNetworkProxiesNoticeFollowsSocksAndWireguardUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProFeaturesWgAddresses(t, ctx)
		mark := func(proxyId server.Id, feature string, useTime time.Time) {
			err := redisProxyFeatureUseStore{}.markAll(ctx, []*ProxyFeatureUseMark{
				{ProxyId: proxyId, Feature: feature, UseTime: useTime},
			})
			connect.AssertEqual(t, err, nil)
		}
		now := server.NowUtc()
		yesterday := now.Add(-24 * time.Hour)
		beforeWindow := now.Add(-ProxyFeatureUseWindow - time.Hour)

		// enforcement dark, as pro.yml ships today
		testProFeaturesProOnlyPlans(t)
		connect.AssertEqual(t, Pro().EnforceFeatures, false)

		// SOCKS: a free network whose SOCKS proxy signed in yesterday sees the
		// notice, and one whose SOCKS proxy was never used does not
		socksSession := testProFeaturesNetwork(t, ctx, "profeaturessocks")
		socksClientId, socksProxyId := testProFeaturesProxy(t, socksSession, "socks proxy", false)
		unusedSession := testProFeaturesNetwork(t, ctx, "profeaturesunusedsocks")
		unusedClientId, _ := testProFeaturesProxy(t, unusedSession, "unused proxy", false)

		status, _ := testProFeaturesNotice(t, socksSession)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
		mark(socksProxyId, FeatureSocksProxy, yesterday)
		status, features := testProFeaturesNotice(t, socksSession)
		connect.AssertEqual(t, status.PlanKnown, true)
		connect.AssertEqual(t, status.Pro, false)
		connect.AssertEqual(t, features[socksClientId], []string{FeatureSocksProxy})
		connect.AssertEqual(t, status.SocksProxyCount, 1)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptUpgrade)

		status, features = testProFeaturesNotice(t, unusedSession)
		connect.AssertEqual(t, len(features[unusedClientId]), 0)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)

		// WireGuard: the same three cases
		wgSession := testProFeaturesNetwork(t, ctx, "profeatureswg")
		wgClientId, wgProxyId := testProFeaturesProxy(t, wgSession, "wireguard proxy", true)
		unusedWgSession := testProFeaturesNetwork(t, ctx, "profeaturesunusedwg")
		unusedWgClientId, _ := testProFeaturesProxy(t, unusedWgSession, "unused wireguard proxy", true)

		status, _ = testProFeaturesNotice(t, wgSession)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
		mark(wgProxyId, FeatureWireguardProxy, yesterday)
		status, features = testProFeaturesNotice(t, wgSession)
		connect.AssertEqual(t, features[wgClientId], []string{FeatureWireguardProxy})
		connect.AssertEqual(t, status.WireguardProxyCount, 1)
		connect.AssertEqual(t, status.SocksProxyCount, 0)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptUpgrade)

		status, features = testProFeaturesNotice(t, unusedWgSession)
		connect.AssertEqual(t, len(features[unusedWgClientId]), 0)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)

		// a use before the window no longer counts, for either feature
		staleSession := testProFeaturesNetwork(t, ctx, "profeaturesstale")
		staleClientId, staleProxyId := testProFeaturesProxy(t, staleSession, "stale proxy", true)
		mark(staleProxyId, FeatureSocksProxy, beforeWindow)
		mark(staleProxyId, FeatureWireguardProxy, beforeWindow)
		status, features = testProFeaturesNotice(t, staleSession)
		connect.AssertEqual(t, len(features[staleClientId]), 0)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
		// and a network that starts using SOCKS later sees the notice once
		// its proxy signs in
		mark(staleProxyId, FeatureSocksProxy, now)
		status, features = testProFeaturesNotice(t, staleSession)
		connect.AssertEqual(t, features[staleClientId], []string{FeatureSocksProxy})
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptUpgrade)

		// the devices list carries no plan and no Pro features
		devicesResult, err := GetNetworkClients(socksSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, devicesResult.Plan == nil, true)

		// Pro, read live: a Pro network never sees the notice, though its
		// proxies still say what they use
		for _, proCase := range []struct {
			userSession *session.ClientSession
			clientId    server.Id
			feature     string
		}{
			{socksSession, socksClientId, FeatureSocksProxy},
			{wgSession, wgClientId, FeatureWireguardProxy},
		} {
			userSession := proCase.userSession
			server.Tx(ctx, func(tx server.PgTx) {
				err := AddProTransferBalanceInTx(
					tx, ctx, userSession.ByJwt.NetworkId, ByteCount(10*1024*1024*1024*1024), now.Add(-time.Minute), now.Add(30*24*time.Hour),
				)
				connect.AssertEqual(t, err, nil)
			})
			connect.AssertEqual(t, UpdateProNetwork(ctx, userSession.ByJwt.NetworkId), true)
			status, features = testProFeaturesNotice(t, userSession)
			connect.AssertEqual(t, status.Pro, true)
			connect.AssertEqual(t, features[proCase.clientId], []string{proCase.feature})
			connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
		}

		// enforcement on: a free network still signing in over SOCKS (the
		// proxy marks the refused sign-ins too) is told SOCKS stopped, and a
		// new proxy is issued neither and has used neither
		restore := Testing_SetEnforceFeatures(true)
		defer restore()
		status, _ = testProFeaturesNotice(t, staleSession)
		connect.AssertEqual(t, status.ProFeaturesEnforced, true)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptStopped)
		status, _ = testProFeaturesNotice(t, unusedSession)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
		enforcedSession := testProFeaturesNetwork(t, ctx, "profeaturesenforced")
		enforcedClientId, _ := testProFeaturesProxy(t, enforcedSession, "enforced proxy", true)
		status, features = testProFeaturesNotice(t, enforcedSession)
		connect.AssertEqual(t, len(features), 1)
		connect.AssertEqual(t, len(features[enforcedClientId]), 0)
		connect.AssertEqual(t, status.Prompt, sdk.ProFeaturesPromptNone)
	})
}

// The marks round-trip through redis as the proxy writes them and the list
// reads them: the newest use per feature, a key that expires past the window,
// and nothing for a proxy that never wrote one.
func TestProxyFeatureUseMarksRoundTripThroughRedis(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		store := redisProxyFeatureUseStore{}
		socksProxyId, bothProxyId, unusedProxyId := server.NewId(), server.NewId(), server.NewId()
		useTime := time.Date(2026, 11, 10, 9, 30, 0, 123_000_000, time.UTC)

		connect.AssertEqual(t, store.markAll(ctx, []*ProxyFeatureUseMark{
			{ProxyId: socksProxyId, Feature: FeatureSocksProxy, UseTime: useTime},
			{ProxyId: bothProxyId, Feature: FeatureSocksProxy, UseTime: useTime},
			{ProxyId: bothProxyId, Feature: FeatureWireguardProxy, UseTime: useTime.Add(time.Hour)},
		}), nil)
		// a later write of a feature replaces its use and keeps the other's
		connect.AssertEqual(t, store.markAll(ctx, []*ProxyFeatureUseMark{
			{ProxyId: bothProxyId, Feature: FeatureSocksProxy, UseTime: useTime.Add(2 * time.Hour)},
		}), nil)

		uses, err := GetProxyFeatureUses(ctx, []server.Id{socksProxyId, bothProxyId, unusedProxyId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(uses), 2)
		connect.AssertEqual(t, uses[socksProxyId][FeatureSocksProxy].Equal(useTime), true)
		connect.AssertEqual(t, len(uses[socksProxyId]), 1)
		connect.AssertEqual(t, uses[bothProxyId][FeatureSocksProxy].Equal(useTime.Add(2*time.Hour)), true)
		connect.AssertEqual(t, uses[bothProxyId][FeatureWireguardProxy].Equal(useTime.Add(time.Hour)), true)
		_, ok := uses[unusedProxyId]
		connect.AssertEqual(t, ok, false)

		empty, err := GetProxyFeatureUses(ctx, nil)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(empty), 0)

		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.PTTL(ctx, proxyFeatureUseKey(bothProxyId)).Result()
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, ProxyFeatureUseWindow < ttl && ttl <= proxyFeatureUseTtl, true)
			// a value that is not a use reads as no mark
			connect.AssertEqual(t, r.HSet(ctx, proxyFeatureUseKey(unusedProxyId), FeatureSocksProxy, "x").Err(), nil)
			connect.AssertEqual(t, r.Expire(ctx, proxyFeatureUseKey(unusedProxyId), time.Minute).Err(), nil)
			_, err = r.HGet(ctx, proxyFeatureUseKey(unusedProxyId), FeatureWireguardProxy).Result()
			connect.AssertEqual(t, err, redis.Nil)
		})
		uses, err = GetProxyFeatureUses(ctx, []server.Id{unusedProxyId})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(uses), 0)
	})
}

// The marks counter reads each proxy's network once and its plan through
// IsProNetwork: free, then pro once the network holds Pro and the cached plan
// is past its ttl. A proxy with no proxy client has no plan.
func TestProxyFeatureUsePlansReadEachProxysNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProFeaturesWgAddresses(t, ctx)
		userSession := testProFeaturesNetwork(t, ctx, "profeaturesplans")
		_, proxyId := testProFeaturesProxy(t, userSession, "plan proxy", false)
		goneProxyId := server.NewId()

		settings := DefaultProxyFeatureUseSettings()
		planner := newDbProxyFeatureUsePlans(settings)
		plans := planner.plans(ctx, []server.Id{proxyId, goneProxyId})
		connect.AssertEqual(t, plans, map[server.Id]string{proxyId: proxyFeatureUsePlanFree})

		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			err := AddProTransferBalanceInTx(
				tx, ctx, userSession.ByJwt.NetworkId, ByteCount(1024*1024*1024), now.Add(-time.Minute), now.Add(30*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
		})
		connect.AssertEqual(t, UpdateProNetwork(ctx, userSession.ByJwt.NetworkId), true)
		// the plan is kept for its ttl
		connect.AssertEqual(t, planner.plans(ctx, []server.Id{proxyId}), map[server.Id]string{proxyId: proxyFeatureUsePlanFree})
		settings.PlanCacheTtl = 0
		connect.AssertEqual(t, planner.plans(ctx, []server.Id{proxyId}), map[server.Id]string{proxyId: proxyFeatureUsePlanPro})
	})
}
