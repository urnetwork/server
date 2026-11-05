package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Pure: the threshold, and that no pro.yml (limit 0) never refuses.
func TestProxyClientLimitReached(t *testing.T) {
	connect.AssertEqual(t, proxyClientLimitReached(0, 1_000_000), false)
	connect.AssertEqual(t, proxyClientLimitReached(-1, 5), false)
	connect.AssertEqual(t, proxyClientLimitReached(2, 1), false)
	connect.AssertEqual(t, proxyClientLimitReached(2, 2), true)
	connect.AssertEqual(t, proxyClientLimitReached(1000, 300006), true)
	connect.AssertEqual(t, strings.Contains(proxyClientLimitMessage(1000, true), "1000 active proxies"), true)
	connect.AssertEqual(t, strings.Contains(proxyClientLimitMessage(2, false), "upgrade to UR Pro"), true)
}

// Pure: the count stops at the limit itself (the first release counted to
// limit+1), and a count capped there decides every limit as the full count does.
func TestProxyClientCountBoundIsTheLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 1000} {
		bound := proxyClientCountBound(limit)
		connect.AssertEqual(t, bound, limit)
		for count := 0; count <= limit+3; count += 1 {
			capped := min(count, bound)
			connect.AssertEqual(t, proxyClientLimitReached(limit, capped), proxyClientLimitReached(limit, count))
		}
	}
}

func testAuthProxyClient(t testing.TB, userSession *session.ClientSession) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			Description: "proxy",
			DeviceSpec:  "proxy",
			ProxyConfig: &ProxyConfig{
				InitialDeviceState: &ExtendedProxyDeviceState{
					ProxyDeviceState: ProxyDeviceState{
						Location: &sdk.ConnectLocation{
							ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
						},
					},
				},
			},
		},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	return result
}

func testAuthDeviceClient(t testing.TB, userSession *session.ClientSession) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{Description: "device", DeviceSpec: "device"},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	return result
}

// a client derived from sourceClientId, a hosted proxy when proxy is set
func testAuthDerivedClient(t testing.TB, userSession *session.ClientSession, sourceClientId server.Id, proxy bool) *AuthNetworkClientResult {
	args := &AuthNetworkClientArgs{
		SourceClientId: &sourceClientId,
		Description:    "derived",
		DeviceSpec:     "derived",
	}
	if proxy {
		args.ProxyConfig = &ProxyConfig{
			InitialDeviceState: &ExtendedProxyDeviceState{
				ProxyDeviceState: ProxyDeviceState{
					Location: &sdk.ConnectLocation{
						ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
					},
				},
			},
		}
	}
	result, err := AuthNetworkClient(args, userSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	return result
}

func testNetworkClientSourceClientId(ctx context.Context, clientId server.Id) (sourceClientId *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT source_client_id FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&sourceClientId))
	})
	return
}

func testNetworkActiveProxyCount(ctx context.Context, networkId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT count(*)
				FROM network_client
				INNER JOIN proxy_client ON proxy_client.client_id = network_client.client_id
				WHERE network_client.network_id = $1 AND network_client.active = true
			`,
			networkId,
		).Scan(&count))
	})
	return
}

// 2026-11-04/05: the two proxy hosts ran at 99.6% CPU with 91,000 installed
// hosted devices, and one network held 123,295 active hosted proxies. A free
// network now holds at most 5 active hosted proxies (Pro 1000): the sixth is
// refused and nothing is created, a client that is not a proxy is neither
// counted nor refused, and removing a proxy makes room for the next. Before the
// cap, the sixth proxy was created. The create rate is lifted here; it has its
// own tests.
func TestAuthNetworkClientCapsActiveProxiesAtThePlanLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		var proxies []*AuthNetworkClientResult
		for range 5 {
			proxy := testAuthProxyClient(t, userSession)
			connect.AssertEqual(t, proxy.Error == nil, true)
			// a proxy created with the network token is top-level: no source
			// client, so the top-level walk's partial index has it
			connect.AssertEqual(t, testNetworkClientSourceClientId(ctx, *proxy.ClientId) == nil, true)
			proxies = append(proxies, proxy)
		}
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 5)

		// the free cap is reached: refused, and nothing is created
		sixth := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, sixth.Error != nil, true)
		connect.AssertEqual(t, sixth.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, sixth.Error.UpgradeRequired, true)
		connect.AssertEqual(t, strings.Contains(sixth.Error.Message, "5 active proxies"), true)
		connect.AssertEqual(t, sixth.ClientId == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 5)

		// a client that is not a proxy is neither counted nor refused
		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)

		// removing a proxy makes room for the next
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *proxies[0].ClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		next := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, next.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 5)
	})
}

// A network's Embed plan row replaces the plan's cap: disposablenetwork's row
// of 1 (2026-11-05) caps it at 1 active proxy, not the free 5.
func TestAuthNetworkClientProxyCapFollowsTheEmbedRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)
		server.Raise(SetNetworkTopLevelClientLimit(ctx, networkId, 1))

		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		second := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, second.Error != nil, true)
		connect.AssertEqual(t, second.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, strings.Contains(second.Error.Message, "1 active proxies"), true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)
	})
}

// Pure: the plan caps, and the Embed row wins over either plan.
func TestProxyClientLimitFor(t *testing.T) {
	connect.AssertEqual(t, proxyClientLimitFor(false, 0, false), 5)
	connect.AssertEqual(t, proxyClientLimitFor(true, 0, false), 1000)
	connect.AssertEqual(t, proxyClientLimitFor(false, 1, true), 1)
	connect.AssertEqual(t, proxyClientLimitFor(true, 1, true), 1)
}

// Pure: the minute block, the key, and the rate's decision (free 5, Pro 20 a
// minute; the create that makes the count exceed the rate is refused).
func TestProxyClientCreateRate(t *testing.T) {
	connect.AssertEqual(t, proxyClientCreateBlock(time.Unix(0, 0)), int64(0))
	connect.AssertEqual(t, proxyClientCreateBlock(time.Unix(59, 999_999_999)), int64(0))
	connect.AssertEqual(t, proxyClientCreateBlock(time.Unix(60, 0)), int64(1))
	connect.AssertEqual(t, proxyClientCreateBlock(time.Unix(1_762_320_000, 0)), int64(29_372_000))

	networkId := server.RequireParseId("01a18015-1399-9880-7939-bbb9d5c23277")
	connect.AssertEqual(t, proxyClientCreateKey(networkId, 29_372_000), "cl-01a18015-1399-9880-7939-bbb9d5c23277-29372000")

	connect.AssertEqual(t, proxyClientCreatesPerMinute(false), 5)
	connect.AssertEqual(t, proxyClientCreatesPerMinute(true), 20)
	connect.AssertEqual(t, proxyClientCreateRateExceeded(5, 5), false)
	connect.AssertEqual(t, proxyClientCreateRateExceeded(5, 6), true)
	connect.AssertEqual(t, proxyClientCreateRateExceeded(20, 20), false)
	connect.AssertEqual(t, proxyClientCreateRateExceeded(20, 21), true)
	connect.AssertEqual(t, proxyClientCreateRateExceeded(0, 1_000), false)

	connect.AssertEqual(t, proxyClientCreateRateMessage(5, false), "Your plan allows 5 new proxies a minute. Try again in a minute, or upgrade to UR Pro for more.")
	connect.AssertEqual(t, proxyClientCreateRateMessage(20, true), "Your plan allows 20 new proxies a minute. Try again in a minute.")
	connect.AssertEqual(t, proxyClientCreateRateMessage(1, true), "Your plan allows 1 new proxy a minute. Try again in a minute.")
}

// The minute's counter in redis: each create counts once in its minute's key,
// the key expires within proxyClientCreateKeyTtl, the next minute starts at 1,
// and a redis failure (here a canceled context) reports the create as not
// counted, so it goes ahead.
func TestCountProxyClientCreate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		minute := time.Unix(1_762_320_000, 0)

		for i := int64(1); i <= 3; i += 1 {
			count, counted := countProxyClientCreate(ctx, networkId, minute.Add(time.Duration(i)*time.Second))
			connect.AssertEqual(t, counted, true)
			connect.AssertEqual(t, count, i)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			ttl, err := r.TTL(ctx, proxyClientCreateKey(networkId, proxyClientCreateBlock(minute))).Result()
			server.Raise(err)
			connect.AssertEqual(t, 0 < ttl && ttl <= proxyClientCreateKeyTtl, true)
		})
		count, counted := countProxyClientCreate(ctx, networkId, minute.Add(time.Minute))
		connect.AssertEqual(t, counted, true)
		connect.AssertEqual(t, count, int64(1))

		canceledCtx, cancel := context.WithCancel(ctx)
		cancel()
		count, counted = countProxyClientCreate(canceledCtx, networkId, minute)
		connect.AssertEqual(t, counted, false)
		connect.AssertEqual(t, count, int64(0))
	})
}

// A free network creates at most 5 hosted proxies a minute: the sixth in the
// same minute is refused with nothing created, and the next minute allows
// more. Before the rate, the sixth was created. The cap is lifted here; it has
// its own tests.
func TestAuthNetworkClientRatesFreeProxyCreatesPerMinute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientLimits(0, 0)()
		now := time.Unix(1_762_320_010, 0)
		defer Testing_SetProxyClientCreateNow(func() time.Time { return now })()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		for range 5 {
			connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		}
		sixth := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, sixth.Error != nil, true)
		connect.AssertEqual(t, sixth.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, sixth.Error.UpgradeRequired, true)
		connect.AssertEqual(t, sixth.Error.Message, "Your plan allows 5 new proxies a minute. Try again in a minute, or upgrade to UR Pro for more.")
		connect.AssertEqual(t, sixth.ClientId == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 5)

		// the next minute
		now = now.Add(time.Minute)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 6)
	})
}

// A Pro network creates at most 20 hosted proxies a minute: the 21st in the
// same minute is refused, with no upgrade offered.
func TestAuthNetworkClientRatesProProxyCreatesPerMinute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientLimits(0, 0)()
		now := time.Unix(1_762_320_010, 0)
		defer Testing_SetProxyClientCreateNow(func() time.Time { return now })()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)
		balanceTime := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024*1024), balanceTime, balanceTime.Add(time.Hour)))
		})
		connect.AssertEqual(t, IsProFresh(ctx, &networkId), true)

		for range 20 {
			connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		}
		refused := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, refused.Error != nil, true)
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, refused.Error.UpgradeRequired, false)
		connect.AssertEqual(t, refused.Error.Message, "Your plan allows 20 new proxies a minute. Try again in a minute.")
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 20)
	})
}

func testProxyLimitNetwork(ctx context.Context) (networkId server.Id, userSession *session.ClientSession) {
	networkId = server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "test", userId)
	userSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return
}

// The count's walks visit at most proxyClientLimitScanBudget clients each, so a
// network with hundreds of thousands of active clients does not scan them all
// on every proxy create (0.4 s warm, 3.6 s cold for 324,000 on Main). With a
// budget of 1, neither walk can find a cap of 2, so a third proxy is
// allowed. With the full budget it is refused. Before the budget, the count
// walked every active client and refused the third proxy at any budget.
func TestAuthNetworkClientProxyLimitScanBudgetBoundsTheWalk(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientLimits(2, 1000)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)

		allowed := func() *AuthNetworkClientResult {
			defer Testing_SetProxyClientLimitScanBudget(1)()
			return testAuthProxyClient(t, userSession)
		}()
		connect.AssertEqual(t, allowed.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 3)

		refused := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, refused.Error != nil, true)
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 3)
	})
}

// Top-level proxies are counted through the partial index of top-level active
// clients, which skips derived clients. With a budget of 3 and a cap of 2: a
// device, two top-level proxies, then three clients derived from the
// device. The newest 3 active clients are the derived ones, none a proxy, but
// the network has only three top-level clients, so the top-level walk sees both
// proxies and the next proxy is refused. Before the top-level walk, the count
// saw only the newest window and allowed it.
func TestAuthNetworkClientProxyLimitCountsTopLevelProxiesOutsideTheNewestWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientLimits(2, 1000)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		defer Testing_SetProxyClientLimitScanBudget(3)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		for range 3 {
			derived := testAuthDerivedClient(t, userSession, *device.ClientId, false)
			connect.AssertEqual(t, derived.Error == nil, true)
			connect.AssertEqual(t, testNetworkClientSourceClientId(ctx, *derived.ClientId) != nil, true)
		}

		refused := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, refused.Error != nil, true)
		connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)
	})
}

// A proxy created with a source client is derived: every proxy a client token
// creates is, since a client token can create only children of its own client.
// The partial index of top-level clients cannot see derived proxies, so the
// newest window counts them: two derived proxies reach a cap of 2,
// and a third, derived or top-level, is refused. A count of top-level clients
// alone let a network create derived proxies without limit.
func TestAuthNetworkClientProxyLimitCountsDerivedProxies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientLimits(2, 1000)()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)
		for range 2 {
			proxy := testAuthDerivedClient(t, userSession, *device.ClientId, true)
			connect.AssertEqual(t, proxy.Error == nil, true)
			sourceClientId := testNetworkClientSourceClientId(ctx, *proxy.ClientId)
			connect.AssertEqual(t, sourceClientId != nil && *sourceClientId == *device.ClientId, true)
		}
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)

		derived := testAuthDerivedClient(t, userSession, *device.ClientId, true)
		connect.AssertEqual(t, derived.Error != nil, true)
		connect.AssertEqual(t, derived.Error.ClientLimitExceeded, true)
		topLevel := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, topLevel.Error != nil, true)
		connect.AssertEqual(t, topLevel.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 2)
	})
}
