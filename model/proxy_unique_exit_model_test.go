package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Pure

// The key is the exact IPv4 address, or the IPv6 /64, under its own domain:
// never the verify index's hash of the same address, and never the address.
func TestUniqueExitEgressKeyIsTheAddressOrTheIpv6Slash64(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, 32)
	defer Testing_SetUniqueExitKeySecret(secret)()

	a := UniqueExitEgressKey(netip.MustParseAddr("203.0.113.10"))
	connect.AssertEqual(t, bytes.Equal(a, UniqueExitEgressKey(netip.MustParseAddr("203.0.113.11"))), false)
	connect.AssertEqual(t, bytes.Equal(a, UniqueExitEgressKey(netip.MustParseAddr("::ffff:203.0.113.10"))), true)

	v6 := UniqueExitEgressKey(netip.MustParseAddr("2001:db8:1:2::1"))
	connect.AssertEqual(t, bytes.Equal(v6, UniqueExitEgressKey(netip.MustParseAddr("2001:db8:1:2:ffff::9"))), true)
	connect.AssertEqual(t, bytes.Equal(v6, UniqueExitEgressKey(netip.MustParseAddr("2001:db8:1:3::1"))), false)

	settings := DefaultVerifySettings()
	settings.EgressHashKey = secret
	verifyHash := VerifyEgressIndexHashWithSettings(netip.MustParseAddr("203.0.113.10"), settings)
	connect.AssertEqual(t, bytes.Equal(a, verifyHash[:]), false)
	connect.AssertEqual(t, bytes.Contains(a, netip.MustParseAddr("203.0.113.10").AsSlice()), false)

	restore := Testing_SetUniqueExitKeySecret(bytes.Repeat([]byte{8}, 32))
	other := UniqueExitEgressKey(netip.MustParseAddr("203.0.113.10"))
	restore()
	connect.AssertEqual(t, bytes.Equal(a, other), false)
}

// The allocator picks a unique exit's provider itself: a location naming one
// provider is refused, the others become provider specs.
func TestUniqueExitProviderSpecsRefuseANamedProvider(t *testing.T) {
	_, err := UniqueExitProviderSpecs(&sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{ClientId: sdk.RequireIdFromBytes(server.NewId().Bytes())},
	})
	connect.AssertEqual(t, err, errUniqueExitNamedProvider)

	specs, err := UniqueExitProviderSpecs(&sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, specs[0].BestAvailable, true)

	locationId := server.NewId()
	specs, err = UniqueExitProviderSpecs(&sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{LocationId: sdk.RequireIdFromBytes(locationId.Bytes())},
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *specs[0].LocationId, locationId)

	_, err = UniqueExitProviderSpecs(nil)
	connect.AssertEqual(t, err != nil, true)
}

// A pinned proxy's location names exactly one device, which the sdk makes a
// fixed window of one client (sdk/device_local.go FixedDestinationSize).
func TestUniqueExitPinnedLocationNamesOneProvider(t *testing.T) {
	providerClientId := server.NewId()
	location := UniqueExitPinnedLocation(providerClientId, "us")
	connect.AssertEqual(t, location.ConnectLocationId.IsDevice(), true)
	connect.AssertEqual(t, location.ConnectLocationId.BestAvailable, false)
	connect.AssertEqual(t, location.ConnectLocationId.LocationId == nil, true)
	connect.AssertEqual(t, server.RequireIdFromBytes(location.ConnectLocationId.ClientId.Bytes()), providerClientId)
}

func TestNetworkReplacementBudgetIsATenthAndAtLeastOne(t *testing.T) {
	settings := DefaultUniqueExitSettings()
	connect.AssertEqual(t, networkReplacementBudget(0, settings), 1)
	connect.AssertEqual(t, networkReplacementBudget(10, settings), 1)
	connect.AssertEqual(t, networkReplacementBudget(11, settings), 2)
	connect.AssertEqual(t, networkReplacementBudget(1000, settings), 100)
}

// The wire names the ur.io app, the docs and the examples use.
func TestUniqueExitApiFieldsRoundTrip(t *testing.T) {
	var config ProxyConfig
	server.Raise(json.Unmarshal([]byte(`{"unique_exit": true, "unique_exit_replace": "manual"}`), &config))
	connect.AssertEqual(t, config.UniqueExit, true)
	connect.AssertEqual(t, config.UniqueExitReplace, "manual")

	exitId := server.NewId()
	since := time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC)
	result := &ProxyConfigResult{Exit: &ProxyExit{
		ExitId:      &exitId,
		CountryCode: "us",
		Region:      "California",
		City:        "Oakland",
		ExitSince:   &since,
		State:       ProxyExitStateAssigned,
		ReplaceMode: UniqueExitReplaceAuto,
	}}
	resultJson, err := json.Marshal(result)
	server.Raise(err)
	var wire map[string]any
	server.Raise(json.Unmarshal(resultJson, &wire))
	exit := wire["exit"].(map[string]any)
	for _, key := range []string{"exit_id", "country_code", "region", "city", "exit_since", "state", "replace_mode"} {
		if _, ok := exit[key]; !ok {
			t.Fatalf("the exit has no %s: %s", key, resultJson)
		}
	}
	for key := range exit {
		if strings.Contains(key, "ip") || strings.Contains(key, "addr") {
			t.Fatalf("the exit exposes %s: %s", key, resultJson)
		}
	}
	connect.AssertEqual(t, exit["state"], "assigned")

	errorJson, _ := json.Marshal(&AuthNetworkClientError{NoUniqueSupply: true, Message: "none"})
	connect.AssertEqual(t, strings.Contains(string(errorJson), `"no_unique_supply":true`), true)
	infoJson, _ := json.Marshal(&NetworkClientInfo{Exit: result.Exit})
	connect.AssertEqual(t, strings.Contains(string(infoJson), `"exit":{`), true)
	rotateJson, _ := json.Marshal(&RotateProxyExitResult{Error: &RotateProxyExitError{RateLimited: true, Message: "later"}})
	connect.AssertEqual(t, strings.Contains(string(rotateJson), `"rate_limited":true`), true)
}

// Fixtures

type testExitProvider struct {
	clientId server.Id
	ip       netip.Addr
}

// testExitProviders creates one provider per address, each probed
// stableProbes times with it. Providers given one address share its key, as a
// farm's do.
func testExitProviders(ctx context.Context, addresses []string, stableProbes int) []*testExitProvider {
	now := server.NowUtc()
	providers := []*testExitProvider{}
	for _, address := range addresses {
		provider := &testExitProvider{clientId: server.NewId(), ip: netip.MustParseAddr(address)}
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{
			ClientId:    provider.clientId,
			LocationId:  server.NewId(),
			CountryCode: "us",
			ObservedAt:  now,
		})
		for i := range stableProbes {
			SetProviderEgressKey(ctx, provider.clientId, provider.ip, now.Add(time.Duration(i-stableProbes)*time.Minute))
		}
		providers = append(providers, provider)
	}
	return providers
}

// testExitSource answers draws from the providers in their order, as
// find-providers2 answers from its rank, honoring the exclusions.
func testExitSource(providers func() []*testExitProvider) UniqueExitCandidateSource {
	return func(clientSession *session.ClientSession, specs []*ProviderSpec, excludeClientIds []server.Id, count int) ([]*FindProvidersProvider, error) {
		excluded := map[server.Id]bool{}
		for _, clientId := range excludeClientIds {
			excluded[clientId] = true
		}
		out := []*FindProvidersProvider{}
		for _, provider := range providers() {
			if excluded[provider.clientId] {
				continue
			}
			out = append(out, &FindProvidersProvider{
				ClientId:                   provider.clientId,
				EstimatedBytesPerSecond:    ByteCount(4_000_000),
				HasEstimatedBytesPerSecond: true,
			})
			if count <= len(out) {
				break
			}
		}
		return out, nil
	}
}

// a network with a name of its own: tests here create several
func testUniqueExitNetwork(ctx context.Context) (networkId server.Id, userSession *session.ClientSession) {
	networkId = server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("upe%s", strings.ReplaceAll(networkId.String(), "-", "")), userId)
	userSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return
}

func testUniqueExitProNetwork(ctx context.Context) (server.Id, *session.ClientSession) {
	networkId, userSession := testUniqueExitNetwork(ctx)
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(AddProTransferBalanceInTx(tx, ctx, networkId, ByteCount(1024*1024), now, now.Add(24*time.Hour)))
	})
	return networkId, userSession
}

func testAuthUniqueExitProxy(t testing.TB, userSession *session.ClientSession, replace string) *AuthNetworkClientResult {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			Description: "unique exit",
			DeviceSpec:  "unique exit",
			ProxyConfig: &ProxyConfig{
				UniqueExit:        true,
				UniqueExitReplace: replace,
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

func testLiveExitKeys(ctx context.Context, networkId server.Id) (keys [][]byte) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT egress_key FROM proxy_exit_assignment
				WHERE network_id = $1 AND state IN ('pending', 'active')
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var key []byte
				server.Raise(result.Scan(&key))
				keys = append(keys, key)
			}
		})
	})
	return
}

func testDistinctKeys(keys [][]byte) bool {
	seen := map[string]bool{}
	for _, key := range keys {
		if seen[string(key)] {
			return false
		}
		seen[string(key)] = true
	}
	return true
}

func testUniqueExitEnv(t testing.TB, providers func() []*testExitProvider) func() {
	restores := []func(){
		Testing_SetProxyClientCreatesPerMinute(0, 0),
		Testing_SetUniqueExitKeySecret(bytes.Repeat([]byte{3}, 32)),
		Testing_SetUniqueExitCandidateSource(testExitSource(providers)),
		Testing_SetUniqueExitSlotLike(func(ctx context.Context, clientIds []server.Id, settings *UniqueExitSettings) map[server.Id]bool {
			return map[server.Id]bool{}
		}),
	}
	return func() {
		for i := len(restores) - 1; 0 <= i; i -= 1 {
			restores[i]()
		}
		Testing_ClearNetworkClientLimitCache()
	}
}

// The provider signals the sweeper reads, set by the test.
type testExitSignals struct {
	mutex    sync.Mutex
	offline  map[server.Id]bool
	excluded map[server.Id]bool
}

func newTestExitSignals() *testExitSignals {
	return &testExitSignals{offline: map[server.Id]bool{}, excluded: map[server.Id]bool{}}
}

func (self *testExitSignals) set(offline map[server.Id]bool, excluded map[server.Id]bool) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.offline, self.excluded = offline, excluded
}

func (self *testExitSignals) install() func() {
	pick := func(set func() map[server.Id]bool, clientIds []server.Id) map[server.Id]bool {
		self.mutex.Lock()
		defer self.mutex.Unlock()
		out := map[server.Id]bool{}
		s := set()
		for _, clientId := range clientIds {
			if s[clientId] {
				out[clientId] = true
			}
		}
		return out
	}
	return Testing_SetUniqueExitProviderSignals(
		func(ctx context.Context, clientIds []server.Id, now time.Time) (map[server.Id]bool, error) {
			return pick(func() map[server.Id]bool { return self.offline }, clientIds), nil
		},
		func(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
			return pick(func() map[server.Id]bool { return self.excluded }, clientIds), nil
		},
	)
}

// The cause, pinned (HOSTED-PROXY-UNIQUE-EXIT.md §1): engageub made 5,000
// proxies for 1,000 IPs because every proxy draws its exits from the same
// ranked pool on its own, so proxies share exits, and a farm's providers share
// one address. Drawn independently, as auto-window proxies draw, four proxies
// over a pool led by a three-provider farm all start on the farm's one key.
// With unique_exit the same pool gives four pairwise distinct keys, the farm
// at most once, and a fifth proxy is refused rather than given a duplicate.
func TestUniqueExitProxiesNeverShareAnExitWhereIndependentDrawsDo(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		providers = testExitProviders(ctx, []string{
			"198.51.100.7", "198.51.100.7", "198.51.100.7", // a farm: one address
			"198.51.100.8", "198.51.100.9", "198.51.100.10",
		}, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)

		// independent draws, the pre-unique-exit shape: every proxy's best exit
		// is the farm's, so all four share one key
		source := getUniqueExitCandidateSource()
		independentKeys := [][]byte{}
		for range 4 {
			drawn, err := source(userSession, []*ProviderSpec{{BestAvailable: true}}, nil, 1)
			connect.AssertEqual(t, err, nil)
			for _, provider := range providers {
				if provider.clientId == drawn[0].ClientId {
					independentKeys = append(independentKeys, UniqueExitEgressKey(provider.ip))
				}
			}
		}
		connect.AssertEqual(t, len(independentKeys), 4)
		connect.AssertEqual(t, testDistinctKeys(independentKeys), false)

		for range 4 {
			result := testAuthUniqueExitProxy(t, userSession, "")
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.ProxyConfigResult.Exit.State, ProxyExitStateAssigned)
		}
		keys := testLiveExitKeys(ctx, networkId)
		connect.AssertEqual(t, len(keys), 4)
		connect.AssertEqual(t, testDistinctKeys(keys), true)

		fifth := testAuthUniqueExitProxy(t, userSession, "")
		connect.AssertEqual(t, fifth.Error != nil, true)
		connect.AssertEqual(t, fifth.Error.NoUniqueSupply, true)
		connect.AssertEqual(t, fifth.ClientId == nil, true)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 4)
	})
}

// Uniqueness is Postgres's, not the application's: many concurrent creates for
// one network over a pool where pairs of providers share keys commit one live
// exit per key, and the rest are refused.
func TestUniqueExitAllocationIsUniqueUnderConcurrency(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		addresses := []string{}
		for i := range 8 {
			address := fmt.Sprintf("192.0.2.%d", 10+i)
			addresses = append(addresses, address, address)
		}
		providers = testExitProviders(ctx, addresses, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)

		var wg sync.WaitGroup
		var mutex sync.Mutex
		created, refused := 0, 0
		for range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result := testAuthUniqueExitProxy(t, userSession, "")
				mutex.Lock()
				defer mutex.Unlock()
				if result.Error == nil {
					created += 1
				} else if result.Error.NoUniqueSupply {
					refused += 1
				} else {
					t.Errorf("unexpected refusal: %s", result.Error.Message)
				}
			}()
		}
		wg.Wait()
		keys := testLiveExitKeys(ctx, networkId)
		connect.AssertEqual(t, testDistinctKeys(keys), true)
		connect.AssertEqual(t, len(keys), created)
		connect.AssertEqual(t, created, 8)
		connect.AssertEqual(t, refused, 12)
	})
}

// A provider carries at most PinsPerProvider live unique-exit pins across
// networks, so a scraper's load cannot concentrate on a few homes.
func TestUniqueExitPinsPerProviderAcrossNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		providers = testExitProviders(ctx, []string{"192.0.2.77"}, 2)

		outcomes := []bool{}
		for range 4 {
			_, userSession := testUniqueExitProNetwork(ctx)
			outcomes = append(outcomes, testAuthUniqueExitProxy(t, userSession, "").Error == nil)
		}
		connect.AssertEqual(t, outcomes, []bool{true, true, true, false})
	})
}

// A slot-like provider (a proxy miner's exit, sharing one connection address
// with many) counts only once its key was stable across two probes; an
// ordinary provider needs one fresh probe.
func TestUniqueExitSlotLikeProviderNeedsAStableKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		providers = append(testExitProviders(ctx, []string{"192.0.2.90"}, 1), testExitProviders(ctx, []string{"192.0.2.91"}, 2)...)
		slot := providers[0].clientId
		defer Testing_SetUniqueExitSlotLike(func(ctx context.Context, clientIds []server.Id, settings *UniqueExitSettings) map[server.Id]bool {
			return map[server.Id]bool{slot: true}
		})()
		networkId, userSession := testUniqueExitProNetwork(ctx)
		result := testAuthUniqueExitProxy(t, userSession, "")
		connect.AssertEqual(t, result.Error == nil, true)
		assignment := loadLiveProxyExitAssignment(ctx, *result.ClientId)
		connect.AssertEqual(t, assignment.ProviderClientId, providers[1].clientId)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 1)
		// the slot-like provider is the only one left, and it is not stable
		connect.AssertEqual(t, testAuthUniqueExitProxy(t, userSession, "").Error.NoUniqueSupply, true)
	})
}

// Pro only, refused with the upgrade prompt and nothing created. A Pro proxy is
// created pinned: its stored device config names the assigned provider, and
// its result carries the exit.
func TestAuthNetworkClientUniqueExitIsProOnlyAndCreatesPinned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		providers = testExitProviders(ctx, []string{"192.0.2.100", "192.0.2.101"}, 2)

		freeNetworkId, freeSession := testUniqueExitNetwork(ctx)
		free := testAuthUniqueExitProxy(t, freeSession, "")
		connect.AssertEqual(t, free.Error != nil, true)
		connect.AssertEqual(t, free.Error.UpgradeRequired, true)
		connect.AssertEqual(t, free.ClientId == nil, true)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, freeNetworkId), 0)

		networkId, userSession := testUniqueExitProNetwork(ctx)
		result := testAuthUniqueExitProxy(t, userSession, "manual")
		connect.AssertEqual(t, result.Error == nil, true)
		exit := result.ProxyConfigResult.Exit
		connect.AssertEqual(t, exit.State, ProxyExitStateAssigned)
		connect.AssertEqual(t, exit.ReplaceMode, UniqueExitReplaceManual)
		connect.AssertEqual(t, exit.CountryCode, "us")
		connect.AssertEqual(t, exit.ExitId != nil, true)

		assignment := loadLiveProxyExitAssignment(ctx, *result.ClientId)
		connect.AssertEqual(t, *exit.ExitId, assignment.AssignmentId)
		config := GetProxyDeviceConfig(ctx, result.ProxyConfigResult.ProxyId)
		connect.AssertEqual(t, config.UniqueExit, true)
		pinned := config.InitialDeviceState.Location.ConnectLocationId
		connect.AssertEqual(t, pinned.IsDevice(), true)
		connect.AssertEqual(t, server.RequireIdFromBytes(pinned.ClientId.Bytes()), assignment.ProviderClientId)
		connect.AssertEqual(t, testNetworkActiveProxyCount(ctx, networkId), 1)

		// the listing reports the exit; a proxy without unique exit has none
		plain := testAuthProxyClient(t, userSession)
		connect.AssertEqual(t, plain.Error == nil, true)
		listed, err := GetNetworkProxies(userSession)
		connect.AssertEqual(t, err, nil)
		exits := map[server.Id]*ProxyExit{}
		for _, client := range listed.Clients {
			exits[client.ClientId] = client.Exit
		}
		connect.AssertEqual(t, exits[*result.ClientId] != nil, true)
		connect.AssertEqual(t, *exits[*result.ClientId].ExitId, assignment.AssignmentId)
		connect.AssertEqual(t, exits[*plain.ClientId] == nil, true)

		// a location naming one provider is refused
		named, err := AuthNetworkClient(&AuthNetworkClientArgs{
			Description: "named",
			DeviceSpec:  "named",
			ProxyConfig: &ProxyConfig{
				UniqueExit: true,
				InitialDeviceState: &ExtendedProxyDeviceState{ProxyDeviceState: ProxyDeviceState{
					Location: UniqueExitPinnedLocation(providers[1].clientId, "us"),
				}},
			},
		}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, named.Error.Message, errUniqueExitNamedProvider.Error())
	})
}

// A location without supply refuses before the create is counted: the next
// create in the same minute, at a rate of one a minute, still goes through.
// A unique-exit proxy still counts against the active proxy cap.
func TestAuthNetworkClientUniqueExitNoSupplySpendsNeitherRateNorCap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		defer Testing_SetProxyClientCreatesPerMinute(1, 1)()
		now := time.Unix(1_762_380_000, 0)
		defer Testing_SetProxyClientCreateNow(func() time.Time { return now })()
		_, userSession := testUniqueExitProNetwork(ctx)

		refused := testAuthUniqueExitProxy(t, userSession, "")
		connect.AssertEqual(t, refused.Error.NoUniqueSupply, true)
		connect.AssertEqual(t, testAuthProxyClient(t, userSession).Error == nil, true)

		defer Testing_SetProxyClientCreatesPerMinute(0, 0)()
		defer Testing_SetProxyClientLimits(5, 2)()
		providers = testExitProviders(ctx, []string{"192.0.2.120", "192.0.2.121", "192.0.2.122"}, 2)
		connect.AssertEqual(t, testAuthUniqueExitProxy(t, userSession, "").Error == nil, true)
		capped := testAuthUniqueExitProxy(t, userSession, "")
		connect.AssertEqual(t, capped.Error.ClientLimitExceeded, true)
	})
}

// A lost provider is replaced once the 120 s grace has passed, never before,
// by a new unique exit that is not the old provider; the stored device config
// follows. A provider the proxy left is excluded for a day.
func TestSweepProxyExitsReplacesALostProviderAfterTheGrace(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		providers = testExitProviders(ctx, []string{"192.0.2.130", "192.0.2.131"}, 2)
		_, userSession := testUniqueExitProNetwork(ctx)
		result := testAuthUniqueExitProxy(t, userSession, "")
		proxyClientId := *result.ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)
		settings := DefaultUniqueExitSettings()
		// one proxy: let the network spend two replacements an hour, so the
		// second loss below is decided by supply, not by the network's cap
		settings.MaxNetworkReplacementFractionPerHour = 2

		t0 := server.NowUtc()
		signals.set(map[server.Id]bool{first.ProviderClientId: true}, nil)
		sweep := SweepProxyExits(ctx, t0, settings)
		connect.AssertEqual(t, sweep.MarkedFailingCount, 1)
		connect.AssertEqual(t, sweep.ReplacedCount, 0)

		sweep = SweepProxyExits(ctx, t0.Add(119*time.Second), settings)
		connect.AssertEqual(t, sweep.ReplacedCount, 0)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).AssignmentId, first.AssignmentId)

		sweep = SweepProxyExits(ctx, t0.Add(121*time.Second), settings)
		connect.AssertEqual(t, sweep.ReplacedCount, 1)
		second := loadLiveProxyExitAssignment(ctx, proxyClientId)
		connect.AssertEqual(t, second.AssignmentId != first.AssignmentId, true)
		connect.AssertEqual(t, second.ProviderClientId != first.ProviderClientId, true)
		config := GetProxyDeviceConfig(ctx, result.ProxyConfigResult.ProxyId)
		connect.AssertEqual(t, server.RequireIdFromBytes(config.InitialDeviceState.Location.ConnectLocationId.ClientId.Bytes()), second.ProviderClientId)

		// the first provider is back, the second is lost: the first is excluded
		// for a day, so there is no supply and the proxy is held
		signals.set(map[server.Id]bool{second.ProviderClientId: true}, nil)
		t1 := t0.Add(10 * time.Minute)
		SweepProxyExits(ctx, t1, settings)
		sweep = SweepProxyExits(ctx, t1.Add(121*time.Second), settings)
		connect.AssertEqual(t, sweep.HeldCount, 1)
		held := GetProxyExits(ctx, []server.Id{proxyClientId})[proxyClientId]
		connect.AssertEqual(t, held.State, ProxyExitStateNoSupply)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).ProviderClientId, second.ProviderClientId)
	})
}

// A bad verdict replaces at once, with no grace. The per-proxy and the
// per-network caps hold a proxy rather than churn it.
func TestSweepProxyExitsVerdictsAndCaps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		providers = testExitProviders(ctx, []string{"192.0.2.140", "192.0.2.141", "192.0.2.142", "192.0.2.143", "192.0.2.144"}, 2)
		_, userSession := testUniqueExitProNetwork(ctx)
		a := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		b := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		settings := DefaultUniqueExitSettings()
		settings.MaxReplacementsPerProxyPerHour = 1
		// two proxies at a tenth an hour: the network may replace one
		now := server.NowUtc()

		aFirst := loadLiveProxyExitAssignment(ctx, a)
		signals.set(nil, map[server.Id]bool{aFirst.ProviderClientId: true})
		connect.AssertEqual(t, SweepProxyExits(ctx, now, settings).ReplacedCount, 1)
		aSecond := loadLiveProxyExitAssignment(ctx, a)
		connect.AssertEqual(t, aSecond.ProviderClientId != aFirst.ProviderClientId, true)

		// the network's hourly budget is spent: b is held for the cap
		bFirst := loadLiveProxyExitAssignment(ctx, b)
		signals.set(nil, map[server.Id]bool{bFirst.ProviderClientId: true})
		sweep := SweepProxyExits(ctx, now.Add(time.Minute), settings)
		connect.AssertEqual(t, sweep.ReplacedCount, 0)
		connect.AssertEqual(t, sweep.HeldCount, 1)
		heldB := GetProxyExits(ctx, []server.Id{b})[b]
		connect.AssertEqual(t, heldB.State, ProxyExitStateReplacing)
		connect.AssertEqual(t, heldB.StateReason, ProxyExitReasonCap)

		// a's own cap of one an hour holds it too
		signals.set(nil, map[server.Id]bool{aSecond.ProviderClientId: true})
		SweepProxyExits(ctx, now.Add(2*time.Minute), settings)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, a).State, proxyExitAssignmentReplacing)

		// an hour later the caps have room: the held proxies move
		signals.set(nil, nil)
		sweep = SweepProxyExits(ctx, now.Add(62*time.Minute), settings)
		connect.AssertEqual(t, 1 <= sweep.ReplacedCount, true)
	})
}

// A held proxy is retried at most once a HeldRetryInterval, since each retry
// may draw from find-providers2: a proxy held for no supply is not retried
// within the interval even once supply appears, and is moved when it ends.
func TestSweepProxyExitsRetriesAHeldProxyAtTheRetryInterval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		providers = testExitProviders(ctx, []string{"192.0.2.160"}, 2)
		_, userSession := testUniqueExitProNetwork(ctx)
		proxyClientId := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)
		settings := DefaultUniqueExitSettings()
		now := server.NowUtc()

		// a verdict on the only provider: there is no other exit to take
		signals.set(nil, map[server.Id]bool{first.ProviderClientId: true})
		sweep := SweepProxyExits(ctx, now, settings)
		connect.AssertEqual(t, sweep.HeldCount, 1)
		connect.AssertEqual(t, GetProxyExits(ctx, []server.Id{proxyClientId})[proxyClientId].State, ProxyExitStateNoSupply)

		providers = append(providers, testExitProviders(ctx, []string{"192.0.2.161"}, 2)...)
		sweep = SweepProxyExits(ctx, now.Add(settings.HeldRetryInterval-time.Second), settings)
		connect.AssertEqual(t, sweep.HeldCount, 0)
		connect.AssertEqual(t, sweep.ReplacedCount, 0)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).AssignmentId, first.AssignmentId)

		sweep = SweepProxyExits(ctx, now.Add(settings.HeldRetryInterval), settings)
		connect.AssertEqual(t, sweep.ReplacedCount, 1)
		connect.AssertEqual(t, GetProxyExits(ctx, []server.Id{proxyClientId})[proxyClientId].State, ProxyExitStateAssigned)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).ProviderClientId != first.ProviderClientId, true)
	})
}

// Manual mode holds a failing exit until the customer rotates it.
func TestSweepProxyExitsManualModeHoldsUntilRotate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		rotateNow := server.NowUtc()
		defer Testing_SetUniqueExitRotateNow(func() time.Time { return rotateNow })()
		providers = testExitProviders(ctx, []string{"192.0.2.150", "192.0.2.151"}, 2)
		_, userSession := testUniqueExitProNetwork(ctx)
		proxyClientId := *testAuthUniqueExitProxy(t, userSession, "manual").ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)
		settings := DefaultUniqueExitSettings()

		signals.set(nil, map[server.Id]bool{first.ProviderClientId: true})
		sweep := SweepProxyExits(ctx, server.NowUtc(), settings)
		connect.AssertEqual(t, sweep.HeldCount, 1)
		held := GetProxyExits(ctx, []server.Id{proxyClientId})[proxyClientId]
		connect.AssertEqual(t, held.State, ProxyExitStateReplacing)
		connect.AssertEqual(t, held.StateReason, ProxyExitReasonManual)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).AssignmentId, first.AssignmentId)

		rotated, err := RotateProxyExit(&RotateProxyExitArgs{ClientId: proxyClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rotated.Error == nil, true)
		connect.AssertEqual(t, rotated.Exit.State, ProxyExitStateAssigned)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).ProviderClientId != first.ProviderClientId, true)
	})
}

// A confirm with the stored key activates the exit; a new key the network does
// not hold replaces it; a new key another exit holds is a collision, which the
// sweeper replaces with no grace.
func TestConfirmProxyExitMatchChangeAndCollision(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		providers = testExitProviders(ctx, []string{"192.0.2.160", "192.0.2.161", "192.0.2.162"}, 2)
		_, userSession := testUniqueExitProNetwork(ctx)
		a := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		b := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		aExit := loadLiveProxyExitAssignment(ctx, a)
		bExit := loadLiveProxyExitAssignment(ctx, b)
		now := server.NowUtc()

		connect.AssertEqual(t, ConfirmProxyExit(ctx, a, aExit.AssignmentId, aExit.EgressKey, now), ProxyExitConfirmMatch)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, a).State, proxyExitAssignmentActive)

		moved := UniqueExitEgressKey(netip.MustParseAddr("203.0.113.200"))
		connect.AssertEqual(t, ConfirmProxyExit(ctx, a, aExit.AssignmentId, moved, now), ProxyExitConfirmChangedUnique)
		connect.AssertEqual(t, bytes.Equal(loadLiveProxyExitAssignment(ctx, a).EgressKey, moved), true)

		// a now egresses where b does
		connect.AssertEqual(t, ConfirmProxyExit(ctx, a, aExit.AssignmentId, bExit.EgressKey, now), ProxyExitConfirmChangedCollision)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, a).FailingReason, ProxyExitReasonCollision)
		connect.AssertEqual(t, ConfirmProxyExit(ctx, a, server.NewId(), aExit.EgressKey, now), ProxyExitConfirmStale)

		sweep := SweepProxyExits(ctx, now, DefaultUniqueExitSettings())
		connect.AssertEqual(t, sweep.ReplacedCount, 1)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, a).AssignmentId != aExit.AssignmentId, true)
		connect.AssertEqual(t, testDistinctKeys(testLiveExitKeys(ctx, aExit.NetworkId)), true)
	})
}

// Rotate moves the proxy to a new unique exit, once a minute; without another
// exit it keeps the one it has. A removed proxy's exit is released and its key
// is free again.
func TestRotateProxyExitAndRemovedProxyRelease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		rotateNow := server.NowUtc()
		defer Testing_SetUniqueExitRotateNow(func() time.Time { return rotateNow })()
		providers = testExitProviders(ctx, []string{"192.0.2.170", "192.0.2.171"}, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)
		proxyClientId := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)

		rotated, err := RotateProxyExit(&RotateProxyExitArgs{ClientId: proxyClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, rotated.Error == nil, true)
		second := loadLiveProxyExitAssignment(ctx, proxyClientId)
		connect.AssertEqual(t, second.ProviderClientId != first.ProviderClientId, true)
		connect.AssertEqual(t, *rotated.Exit.ExitId, second.AssignmentId)

		again, _ := RotateProxyExit(&RotateProxyExitArgs{ClientId: proxyClientId}, userSession)
		connect.AssertEqual(t, again.Error.RateLimited, true)

		// a minute later: the only other provider is the one it left a minute
		// ago, excluded for a day, so the proxy keeps its exit
		rotateNow = rotateNow.Add(2 * time.Minute)
		none, _ := RotateProxyExit(&RotateProxyExitArgs{ClientId: proxyClientId}, userSession)
		connect.AssertEqual(t, none.Error.NoUniqueSupply, true)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).AssignmentId, second.AssignmentId)

		other, _ := RotateProxyExit(&RotateProxyExitArgs{ClientId: server.NewId()}, userSession)
		connect.AssertEqual(t, other.Error != nil, true)

		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: proxyClientId}, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, removed.Error == nil, true)
		sweep := SweepProxyExits(ctx, server.NowUtc(), DefaultUniqueExitSettings())
		connect.AssertEqual(t, sweep.ReleasedCount, 1)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 0)
		connect.AssertEqual(t, testAuthUniqueExitProxy(t, userSession, "").Error == nil, true)
	})
}

const testNotAUniqueExitProxy = "Not a unique-exit proxy of this network."

func testRotateExit(t testing.TB, userSession *session.ClientSession, proxyClientId server.Id) *RotateProxyExitResult {
	result, err := RotateProxyExit(&RotateProxyExitArgs{ClientId: proxyClientId}, userSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result != nil, true)
	return result
}

// the provider a proxy's stored device config is pinned to
func testPinnedProvider(t testing.TB, ctx context.Context, proxyId server.Id) server.Id {
	config := GetProxyDeviceConfig(ctx, proxyId)
	connect.AssertEqual(t, config != nil, true)
	return server.RequireIdFromBytes(config.InitialDeviceState.Location.ConnectLocationId.ClientId.Bytes())
}

func testRemoveClient(t testing.TB, userSession *session.ClientSession, clientId server.Id) {
	removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, userSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, removed.Error == nil, true)
}

// a whole hour that is not the day's last, so a test's minutes from it share
// one UTC day, and the day after it is the next rotate day
func testRotateStart() time.Time {
	start := server.NowUtc().Truncate(time.Hour)
	if start.Hour() == 23 {
		start = start.Add(-time.Hour)
	}
	return start
}

// A removed proxy is not a unique-exit proxy of the network. remove-client
// only deactivates its client: the proxy_unique_exit row and the exit stay
// until the sweeper releases them. Until then a rotate must neither move the
// proxy nor repin its stored device config. On 2026-11-06 the production smoke
// test rotated a removed proxy and got HTTP 200, a new live exit and a
// repinned config, because the rotate checked only the row and the live exit.
func TestRotateProxyExitRefusesARemovedProxy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		signals := newTestExitSignals()
		defer signals.install()()
		rotateNow := server.NowUtc()
		defer Testing_SetUniqueExitRotateNow(func() time.Time { return rotateNow })()
		providers = testExitProviders(ctx, []string{"192.0.2.180", "192.0.2.181"}, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)
		result := testAuthUniqueExitProxy(t, userSession, "manual")
		proxyClientId := *result.ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)

		testRemoveClient(t, userSession, proxyClientId)

		// no sweep yet, and a second provider is free to move to
		rotated := testRotateExit(t, userSession, proxyClientId)
		connect.AssertEqual(t, rotated.Exit == nil, true)
		connect.AssertEqual(t, rotated.Error != nil && rotated.Error.Message == testNotAUniqueExitProxy, true)
		live := loadLiveProxyExitAssignment(ctx, proxyClientId)
		connect.AssertEqual(t, live != nil && live.AssignmentId == first.AssignmentId, true)
		connect.AssertEqual(t, testPinnedProvider(t, ctx, result.ProxyConfigResult.ProxyId), first.ProviderClientId)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 1)

		// the sweeper releases the exit; the answer stays the same
		sweep := SweepProxyExits(ctx, server.NowUtc(), DefaultUniqueExitSettings())
		connect.AssertEqual(t, sweep.ReleasedCount, 1)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 0)
		after := testRotateExit(t, userSession, proxyClientId)
		connect.AssertEqual(t, after.Error != nil && after.Error.Message == testNotAUniqueExitProxy, true)
	})
}

// A removal that commits while a rotate is under way, after its checks and
// before its move: the move's transaction holds the proxy's client row and
// finds the client removed, so the rotate is refused and leaves no new exit
// behind.
func TestRotateProxyExitRefusesAProxyRemovedDuringTheRotate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		rotateNow := server.NowUtc()
		defer Testing_SetUniqueExitRotateNow(func() time.Time { return rotateNow })()
		providers = testExitProviders(ctx, []string{"192.0.2.185", "192.0.2.186"}, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)
		result := testAuthUniqueExitProxy(t, userSession, "")
		proxyClientId := *result.ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)

		// a rotate draws its candidates after its checks and before its move:
		// the proxy is removed there
		source := testExitSource(func() []*testExitProvider { return providers })
		var once sync.Once
		defer Testing_SetUniqueExitCandidateSource(func(clientSession *session.ClientSession, specs []*ProviderSpec, excludeClientIds []server.Id, count int) ([]*FindProvidersProvider, error) {
			once.Do(func() {
				testRemoveClient(t, userSession, proxyClientId)
			})
			return source(clientSession, specs, excludeClientIds, count)
		})()

		rotated := testRotateExit(t, userSession, proxyClientId)
		connect.AssertEqual(t, rotated.Exit == nil, true)
		connect.AssertEqual(t, rotated.Error != nil && rotated.Error.Message == testNotAUniqueExitProxy, true)
		live := loadLiveProxyExitAssignment(ctx, proxyClientId)
		connect.AssertEqual(t, live != nil && live.AssignmentId == first.AssignmentId, true)
		connect.AssertEqual(t, testPinnedProvider(t, ctx, result.ProxyConfigResult.ProxyId), first.ProviderClientId)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 1)
	})
}

// The same for the sweeper: a proxy removed after the pass read which proxies
// are active, and before it replaces the proxy's failed exit, is not moved.
// The next pass releases its exit.
func TestSweepProxyExitsLeavesAProxyRemovedDuringThePass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		providers = testExitProviders(ctx, []string{"192.0.2.190", "192.0.2.191"}, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)
		result := testAuthUniqueExitProxy(t, userSession, "")
		proxyClientId := *result.ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)

		// a pass reads the verdicts after the active proxies: the proxy is
		// removed there, and its provider has a verdict, which replaces at once
		var once sync.Once
		defer Testing_SetUniqueExitProviderSignals(
			func(ctx context.Context, clientIds []server.Id, now time.Time) (map[server.Id]bool, error) {
				return map[server.Id]bool{}, nil
			},
			func(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
				once.Do(func() {
					testRemoveClient(t, userSession, proxyClientId)
				})
				return map[server.Id]bool{first.ProviderClientId: true}, nil
			},
		)()

		sweep := SweepProxyExits(ctx, server.NowUtc(), DefaultUniqueExitSettings())
		connect.AssertEqual(t, sweep.ReplacedCount, 0)
		live := loadLiveProxyExitAssignment(ctx, proxyClientId)
		connect.AssertEqual(t, live != nil && live.AssignmentId == first.AssignmentId, true)
		connect.AssertEqual(t, testPinnedProvider(t, ctx, result.ProxyConfigResult.ProxyId), first.ProviderClientId)

		sweep = SweepProxyExits(ctx, server.NowUtc(), DefaultUniqueExitSettings())
		connect.AssertEqual(t, sweep.ReleasedCount, 1)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 0)
	})
}

// Only rotations count against a proxy's rotate limits. The spec's
// rate_limited is "The proxy rotated in the last minute, or 60 times today"
// (connect/api/bringyour.yml), so a refused call, rate limited or without
// supply, spends nothing. The limits used to count every call before comparing
// it: 61 calls in one minute made one rotation and locked the proxy out until
// 00:00Z.
func TestRotateProxyExitCountsOnlyRotations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		start := testRotateStart()
		rotateNow := start
		defer Testing_SetUniqueExitRotateNow(func() time.Time { return rotateNow })()
		providers = testExitProviders(ctx, []string{"192.0.2.200", "192.0.2.201"}, 2)
		_, userSession := testUniqueExitProNetwork(ctx)
		proxyClientId := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		settings := DefaultUniqueExitSettings()
		connect.AssertEqual(t, settings.RotatesPerDay, 60)

		// 61 calls in one minute: one rotation, and 60 refusals
		connect.AssertEqual(t, testRotateExit(t, userSession, proxyClientId).Exit != nil, true)
		for range settings.RotatesPerDay {
			again := testRotateExit(t, userSession, proxyClientId)
			connect.AssertEqual(t, again.Error != nil && again.Error.RateLimited, true)
		}

		// later minutes without supply: the only other provider is the one the
		// proxy left, excluded for a day. A call without supply did not rotate
		// the proxy, so a second call in its minute is not rate limited either
		for minute := 1; minute <= 3; minute += 1 {
			rotateNow = start.Add(time.Duration(minute) * time.Minute)
			for range 2 {
				none := testRotateExit(t, userSession, proxyClientId)
				connect.AssertEqual(t, none.Error != nil && none.Error.NoUniqueSupply, true)
			}
		}

		// supply returns: the proxy rotates
		providers = append(providers, testExitProviders(ctx, []string{"192.0.2.202"}, 2)...)
		rotateNow = start.Add(4 * time.Minute)
		second := testRotateExit(t, userSession, proxyClientId)
		connect.AssertEqual(t, second.Error == nil, true)
		connect.AssertEqual(t, second.Exit != nil, true)

		// every rotation still counts: at two a day, the third rotate of the
		// day is refused though there is supply, and the next day it goes
		limited := DefaultUniqueExitSettings()
		limited.RotatesPerDay = 2
		defer Testing_SetUniqueExitSettings(limited)()
		providers = append(providers, testExitProviders(ctx, []string{"192.0.2.203"}, 2)...)
		rotateNow = start.Add(5 * time.Minute)
		third := testRotateExit(t, userSession, proxyClientId)
		connect.AssertEqual(t, third.Error != nil && third.Error.RateLimited, true)
		rotateNow = start.Add(24 * time.Hour)
		connect.AssertEqual(t, testRotateExit(t, userSession, proxyClientId).Exit != nil, true)
	})
}

// Concurrent rotates of one proxy in one minute move it once, and only that
// rotation counts: at two a day, the proxy may rotate once more that day and
// then no more.
func TestRotateProxyExitConcurrentRotatesMoveOnceAndCountOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		var providers []*testExitProvider
		defer testUniqueExitEnv(t, func() []*testExitProvider { return providers })()
		settings := DefaultUniqueExitSettings()
		settings.RotatesPerDay = 2
		defer Testing_SetUniqueExitSettings(settings)()
		start := testRotateStart()
		rotateNow := start
		defer Testing_SetUniqueExitRotateNow(func() time.Time { return rotateNow })()
		providers = testExitProviders(ctx, []string{"192.0.2.210", "192.0.2.211", "192.0.2.212", "192.0.2.213"}, 2)
		networkId, userSession := testUniqueExitProNetwork(ctx)
		proxyClientId := *testAuthUniqueExitProxy(t, userSession, "").ClientId
		first := loadLiveProxyExitAssignment(ctx, proxyClientId)

		var wg sync.WaitGroup
		var mutex sync.Mutex
		rotated, limited := 0, 0
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := RotateProxyExit(&RotateProxyExitArgs{ClientId: proxyClientId}, userSession)
				mutex.Lock()
				defer mutex.Unlock()
				switch {
				case err != nil:
					t.Errorf("rotate: %s", err)
				case result.Exit != nil:
					rotated += 1
				case result.Error != nil && result.Error.RateLimited:
					limited += 1
				default:
					t.Errorf("unexpected rotate result: %+v", result.Error)
				}
			}()
		}
		wg.Wait()
		connect.AssertEqual(t, rotated, 1)
		connect.AssertEqual(t, limited, 15)
		connect.AssertEqual(t, loadLiveProxyExitAssignment(ctx, proxyClientId).AssignmentId != first.AssignmentId, true)
		connect.AssertEqual(t, len(testLiveExitKeys(ctx, networkId)), 1)

		// the burst spent one of the day's two
		rotateNow = start.Add(time.Minute)
		connect.AssertEqual(t, testRotateExit(t, userSession, proxyClientId).Exit != nil, true)
		rotateNow = start.Add(2 * time.Minute)
		third := testRotateExit(t, userSession, proxyClientId)
		connect.AssertEqual(t, third.Error != nil && third.Error.RateLimited, true)
	})
}

// The ingest's key: a probe seeing the same address again counts it stable, a
// new address restarts the count, and an older probe never overwrites a newer.
func TestSetProviderEgressKeyCountsStabilityAndStaysMonotonic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetUniqueExitKeySecret(bytes.Repeat([]byte{5}, 32))()
		clientId := server.NewId()
		now := server.NowUtc()
		SetProviderEgressLocation(ctx, &ProviderEgressLocation{ClientId: clientId, LocationId: server.NewId(), CountryCode: "us", ObservedAt: now})
		read := func() (key []byte, stable int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT egress_key, egress_key_stable_count FROM provider_egress_location WHERE client_id = $1`,
					clientId,
				).Scan(&key, &stable))
			})
			return
		}
		ip := netip.MustParseAddr("198.51.100.200")
		SetProviderEgressKey(ctx, clientId, ip, now.Add(-3*time.Minute))
		SetProviderEgressKey(ctx, clientId, ip, now.Add(-2*time.Minute))
		key, stable := read()
		connect.AssertEqual(t, bytes.Equal(key, UniqueExitEgressKey(ip)), true)
		connect.AssertEqual(t, stable, 2)

		SetProviderEgressKey(ctx, clientId, netip.MustParseAddr("198.51.100.201"), now.Add(-time.Minute))
		_, stable = read()
		connect.AssertEqual(t, stable, 1)

		SetProviderEgressKey(ctx, clientId, ip, now.Add(-10*time.Minute))
		key, _ = read()
		connect.AssertEqual(t, bytes.Equal(key, UniqueExitEgressKey(netip.MustParseAddr("198.51.100.201"))), true)
	})
}
