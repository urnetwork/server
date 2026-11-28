package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
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

// Pure: the cap refusal says where to remove a proxy. Clients show the message
// as the server wrote it (the published browser extensions show it word for
// word), and none of them can list or remove the network's other proxies. A
// refusal that said only "Remove a proxy" left a free user whose old extension
// proxies held the cap with nothing to do but wait for the 30-day idle reap
// (support inbox 8244). The owner, 2026-11-17: the cap message points to ur.io.
func TestProxyClientLimitMessageNamesTheProxiesPage(t *testing.T) {
	for _, c := range []struct {
		limit   int
		isPro   bool
		upgrade bool
		reached string
	}{
		{limit: 1, isPro: false, upgrade: true, reached: "Your plan's limit of 1 active proxy is reached."},
		{limit: 1000, isPro: true, upgrade: false, reached: "Your plan's limit of 1000 active proxies is reached."},
		// an Embed plan row's limit on a network without Pro
		{limit: 12, isPro: false, upgrade: true, reached: "Your plan's limit of 12 active proxies is reached."},
		// an Embed plan row of 1 on a Pro network
		{limit: 1, isPro: true, upgrade: false, reached: "Your plan's limit of 1 active proxy is reached."},
	} {
		message := proxyClientLimitMessage(c.limit, c.isPro)
		connect.AssertEqual(t, strings.HasPrefix(message, c.reached), true)
		connect.AssertEqual(t, strings.Contains(message, "https://ur.io/app/proxies"), true)
		connect.AssertEqual(t, strings.Contains(message, "upgrade to UR Pro"), c.upgrade)
	}
}

// Pure: the free plan's cap refusal, word for word, as the apps, the browser
// extensions and ur.io show it. The free cap went from 5 to 1 on 2026-11-24
// (owner: free plan, 1 hosted proxy per network). The message says 1 proxy in
// the singular, and asks to remove proxies rather than one, since a network
// that held up to 5 keeps them all and creates again only once it holds none.
// Before, the message said "5 active proxies" and "Remove a proxy".
func TestProxyClientLimitMessageForTheFreeCapOfOne(t *testing.T) {
	connect.AssertEqual(
		t,
		proxyClientLimitMessage(proxyClientLimitFor(false, 0, false), false),
		"Your plan's limit of 1 active proxy is reached. Remove proxies you no longer use at https://ur.io/app/proxies to create another, or upgrade to UR Pro for more.",
	)
	connect.AssertEqual(
		t,
		proxyClientLimitMessage(proxyClientLimitFor(true, 0, false), true),
		"Your plan's limit of 1000 active proxies is reached. Remove proxies you no longer use at https://ur.io/app/proxies to create another.",
	)
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

// The free plan holds 1 active hosted proxy per network (owner, 2026-11-24; it
// was 5, and on 2026-11-04/05 one network held 123,295 active hosted proxies).
// A free network with no proxy creates one. Its second is refused with the
// upgrade prompt and nothing is created, whether it is top-level or derived
// from one of the network's clients. A client that is not a proxy is neither
// counted nor refused, and removing the proxy makes room for the next. Before,
// the second proxy was created. The create rate is lifted here; it has its own
// tests.
func TestAuthNetworkClientFreeNetworkHoldsOneActiveProxy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)
		connect.AssertEqual(t, IsProFresh(ctx, &networkId), false)

		first := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, first.Error == nil, true)
		// a proxy created with the network token is top-level: no source client,
		// so the top-level walk's partial index has it
		connect.AssertEqual(t, testNetworkClientSourceClientId(ctx, *first.ClientId) == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)

		// the free cap is reached: refused, and nothing is created
		second := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, second.Error != nil, true)
		connect.AssertEqual(t, second.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, second.Error.UpgradeRequired, true)
		connect.AssertEqual(t, second.Error.Message, "Your plan's limit of 1 active proxy is reached. Remove proxies you no longer use at https://ur.io/app/proxies to create another, or upgrade to UR Pro for more.")
		connect.AssertEqual(t, second.ClientId == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)

		// a client that is not a proxy is neither counted nor refused, and a
		// proxy derived from it is refused like a top-level one
		device := testAuthDeviceClient(t, userSession)
		connect.AssertEqual(t, device.Error == nil, true)
		derived := testAuthDerivedClient(t, userSession, *device.ClientId, true)
		connect.AssertEqual(t, derived.Error != nil, true)
		connect.AssertEqual(t, derived.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, derived.ClientId == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)

		// removing the proxy makes room for the next
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *first.ClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		next := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, next.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)
	})
}

// Lowering the free cap from 5 to 1 (owner, 2026-11-24: block new creates
// only) removes nothing. A free network that created 5 hosted proxies under
// the old cap keeps all 5 usable: each stays active, listed and not removed,
// with its proxy client and device config, and each still mints the window
// clients a proxy device needs. The network is refused a sixth, and is still
// refused with 4 left after it removes one, since it holds 1 or more; once it
// holds none it creates one again. Before, the cap was 5, so the network
// created a new proxy as soon as it held 4.
func TestAuthNetworkClientFreeNetworkKeepsProxiesHeldOverTheCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()
		networkId, userSession := testProxyLimitNetwork(ctx)

		// the 5 proxies the network created under the free cap of 5
		var proxies []*AuthNetworkClientResult
		func() {
			defer Testing_SetProxyClientLimits(5, 1000)()
			for range 5 {
				proxy := testAuthProxyClient(t, userSession)
				connect.AssertEqual(t, proxy.Error == nil, true)
				proxies = append(proxies, proxy)
			}
		}()

		assertUsable := func(held []*AuthNetworkClientResult) {
			listed, err := GetNetworkProxies(userSession)
			connect.AssertEqual(t, err, nil)
			listedClientIds := map[server.Id]bool{}
			for _, client := range listed.Clients {
				listedClientIds[client.ClientId] = true
			}
			connect.AssertEqual(t, len(listedClientIds), len(held))
			for _, proxy := range held {
				connect.AssertEqual(t, listedClientIds[*proxy.ClientId], true)
				connect.AssertEqual(t, IsClientRemoved(ctx, *proxy.ClientId), false)
				proxyClient, err := GetProxyClient(ctx, proxy.ProxyConfigResult.ProxyId)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, proxyClient != nil && proxyClient.ClientId == *proxy.ClientId, true)
				connect.AssertEqual(t, GetProxyDeviceConfig(ctx, proxy.ProxyConfigResult.ProxyId) != nil, true)
				// a window client of the proxy is not a proxy: never capped
				window := testAuthDerivedClient(t, userSession, *proxy.ClientId, false)
				connect.AssertEqual(t, window.Error == nil, true)
			}
			connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), len(held))
		}
		assertRefused := func() {
			refused := testAuthProxyClient(t, userSession)
			connect.AssertEqual(t, refused.Error != nil, true)
			connect.AssertEqual(t, refused.Error.ClientLimitExceeded, true)
			connect.AssertEqual(t, refused.Error.UpgradeRequired, true)
			connect.AssertEqual(t, refused.Error.Message, proxyClientLimitMessage(1, false))
			connect.AssertEqual(t, refused.ClientId == nil, true)
		}
		remove := func(proxy *AuthNetworkClientResult) {
			removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: *proxy.ClientId}, userSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, removed.Error == nil, true)
		}

		assertUsable(proxies)
		assertRefused()
		assertUsable(proxies)

		// with 4 left the network still holds 1 or more
		remove(proxies[0])
		assertRefused()
		assertUsable(proxies[1:])

		// once it holds none, it creates one
		for _, proxy := range proxies[1:] {
			remove(proxy)
		}
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 0)
		next := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, next.Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)
	})
}

// Pro keeps its cap of 1000 (owner, 2026-11-24): a Pro network creates past
// the free cap of 1 while a free network beside it is refused its second
// proxy. Before, the free network created its second too.
func TestAuthNetworkClientProNetworkIsNotHeldToTheFreeCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()

		freeNetworkId, freeSession := testProxyLimitNetwork(ctx)
		connect.AssertEqual(t, testAuthProxyClient(t, freeSession).Error == nil, true)
		freeSecond := testAuthProxyClient(t, freeSession)
		connect.AssertEqual(t, freeSecond.Error != nil, true)
		connect.AssertEqual(t, freeSecond.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, freeSecond.Error.UpgradeRequired, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, freeNetworkId), 1)

		proNetworkId, proSession := testProxyLimitNetwork(ctx)
		testProxyLimitAddPro(t, ctx, proNetworkId)
		for range 3 {
			connect.AssertEqual(t, testAuthProxyClient(t, proSession).Error == nil, true)
		}
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, proNetworkId), 3)
	})
}

// A network's Embed plan row replaces the plan's cap, above the free cap or
// below Pro's. A free network that is refused its second proxy at the free cap
// of 1 creates a second and a third under a row of 3, and is refused a fourth.
// A Pro network under a row of 1 is refused its second, and creates it once the
// row is cleared. Before, the free cap was 5, so the free network created its
// second proxy without a row.
func TestAuthNetworkClientProxyCapFollowsTheEmbedRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_ClearNetworkClientLimitCache()
		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		ctx := context.Background()

		// a row above the free cap
		networkId, userSession := testProxyLimitNetwork(ctx)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		atFreeCap := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, atFreeCap.Error != nil, true)
		connect.AssertEqual(t, atFreeCap.Error.ClientLimitExceeded, true)
		server.Raise(SetNetworkTopLevelClientLimit(ctx, networkId, 3))
		for range 2 {
			connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)
		}
		atRow := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, atRow.Error != nil, true)
		connect.AssertEqual(t, atRow.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, strings.HasPrefix(atRow.Error.Message, "Your plan's limit of 3 active proxies is reached."), true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 3)

		// a row below the Pro cap
		proNetworkId, proSession := testProxyLimitNetwork(ctx)
		testProxyLimitAddPro(t, ctx, proNetworkId)
		server.Raise(SetNetworkTopLevelClientLimit(ctx, proNetworkId, 1))
		connect.AssertEqual(t, testAuthProxyClient(t, proSession).Error == nil, true)
		proAtRow := testAuthProxyClient(t, proSession)
		connect.AssertEqual(t, proAtRow.Error != nil, true)
		connect.AssertEqual(t, proAtRow.Error.ClientLimitExceeded, true)
		connect.AssertEqual(t, proAtRow.Error.UpgradeRequired, false)
		connect.AssertEqual(t, proAtRow.Error.Message, "Your plan's limit of 1 active proxy is reached. Remove proxies you no longer use at https://ur.io/app/proxies to create another.")
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, proNetworkId), 1)
		ClearNetworkTopLevelClientLimit(ctx, proNetworkId)
		connect.AssertEqual(t, testAuthProxyClient(t, proSession).Error == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, proNetworkId), 2)
	})
}

// Pure: the plan caps (free 1 since 2026-11-24, Pro 1000), and the Embed row
// wins over either plan, above or below its cap. Before, free was 5.
func TestProxyClientLimitFor(t *testing.T) {
	connect.AssertEqual(t, proxyClientLimitFor(false, 0, false), 1)
	connect.AssertEqual(t, proxyClientLimitFor(true, 0, false), 1000)
	connect.AssertEqual(t, proxyClientLimitFor(false, 3, true), 3)
	connect.AssertEqual(t, proxyClientLimitFor(false, 1, true), 1)
	connect.AssertEqual(t, proxyClientLimitFor(true, 1, true), 1)
	connect.AssertEqual(t, proxyClientLimitFor(true, 5000, true), 5000)
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

// a free network with a name of its own (network names are unique), so a test
// can create several
func testProxyLimitNetwork(ctx context.Context) (networkId server.Id, userSession *session.ClientSession) {
	networkId = server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("plim%s", strings.ReplaceAll(networkId.String(), "-", "")), userId)
	userSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return
}

// gives the network Pro for an hour with a Pro balance
func testProxyLimitAddPro(t testing.TB, ctx context.Context, networkId server.Id) {
	balanceTime := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024*1024), balanceTime, balanceTime.Add(time.Hour)))
	})
	connect.AssertEqual(t, IsProFresh(ctx, &networkId), true)
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
