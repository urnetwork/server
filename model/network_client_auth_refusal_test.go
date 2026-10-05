package model

// auth-client decides each refusal before it writes anything, so a refused
// request creates nothing and returns no credentials. Each refusal has a test,
// and so does the valid request beside it, which still writes what it wrote
// before.

import (
	"context"
	"net/netip"
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

// A proxy config that auth-client cannot use fails the call before anything
// is created, and no credentials come back. It used to create the client
// first: "Invalid location" then came back with the new client's client_id
// and by_client_jwt, and a lock list entry that is not an ip or a subnet, or a
// caller ip lock on a session without an address, failed the call after the
// client was created. Either way the client stayed in the network with no
// proxy.
func TestAuthNetworkClientRefusesInvalidProxyConfigBeforeCreate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		// the caller's ip comes from the session, and this one has none
		noAddressSession := *userSession
		noAddressSession.ClientAddress = ""

		bestAvailableLocation := &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
		}

		for _, c := range []struct {
			name          string
			clientSession *session.ClientSession
			proxyConfig   *ProxyConfig
			// a refusal of the caller's input
			message string
			// the server's own failure
			errMessage string
		}{
			{
				// the uk is "gb"
				name:          "a country code that is not a country",
				clientSession: userSession,
				proxyConfig: &ProxyConfig{
					InitialDeviceState: &ExtendedProxyDeviceState{CountryCode: "uk"},
				},
				message: "Invalid location",
			},
			{
				name:          "no location or country code",
				clientSession: userSession,
				proxyConfig: &ProxyConfig{
					InitialDeviceState: &ExtendedProxyDeviceState{},
				},
				message: "Invalid location",
			},
			{
				name:          "no initial device state",
				clientSession: userSession,
				proxyConfig:   &ProxyConfig{EnableWg: true},
				message:       "Invalid location",
			},
			{
				name:          "a lock ip that is not an ip or a subnet",
				clientSession: userSession,
				proxyConfig: &ProxyConfig{
					LockIpList: []string{"192.0.2.1", "192.0.2.300"},
					InitialDeviceState: &ExtendedProxyDeviceState{
						ProxyDeviceState: ProxyDeviceState{Location: bestAvailableLocation},
					},
				},
				message: "Could not parse lock ip 192.0.2.300",
			},
			{
				name:          "a caller ip lock without the caller's ip",
				clientSession: &noAddressSession,
				proxyConfig: &ProxyConfig{
					LockCallerIp: true,
					InitialDeviceState: &ExtendedProxyDeviceState{
						ProxyDeviceState: ProxyDeviceState{Location: bestAvailableLocation},
					},
				},
				errMessage: "Could not lock caller ip",
			},
		} {
			clientCountBefore, deviceCountBefore, proxyCountBefore := authClientTestNetworkCounts(ctx, networkId)
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					Description: "proxy",
					DeviceSpec:  "proxy",
					ProxyConfig: c.proxyConfig,
				},
				c.clientSession,
			)
			if c.errMessage != "" {
				if err == nil || err.Error() != c.errMessage {
					t.Errorf("%s: failed with %v, want the error %q", c.name, err, c.errMessage)
				}
				if result != nil {
					t.Errorf("%s: the error came with a result: client_id=%v", c.name, result.ClientId)
				}
			} else if err != nil {
				t.Errorf("%s: the refusal failed the call: %s", c.name, err)
			} else if result == nil {
				t.Errorf("%s: no result", c.name)
			} else if result.Error == nil {
				t.Errorf("%s: the proxy config was accepted: client_id=%v", c.name, result.ClientId)
			} else {
				if result.Error.Message != c.message || result.Error.ClientLimitExceeded || result.Error.UpgradeRequired {
					t.Errorf("%s: refused with %+v, want the message %q", c.name, result.Error, c.message)
				}
				if result.ClientId != nil || result.ByClientJwt != nil || result.ProxyConfigResult != nil {
					t.Errorf(
						"%s: the refusal returned client_id=%v by_client_jwt=%t proxy_config_result=%t",
						c.name,
						result.ClientId,
						result.ByClientJwt != nil,
						result.ProxyConfigResult != nil,
					)
				}
			}
			clientCount, deviceCount, proxyCount := authClientTestNetworkCounts(ctx, networkId)
			if clientCount != clientCountBefore || deviceCount != deviceCountBefore || proxyCount != proxyCountBefore {
				t.Errorf(
					"%s: created %d clients, %d devices and %d proxies",
					c.name,
					clientCount-clientCountBefore,
					deviceCount-deviceCountBefore,
					proxyCount-proxyCountBefore,
				)
			}
		}
	})
}

// A proxy config that auth-client can use is created as it was before it was
// resolved ahead of the client: the location from the config or from its
// country code, the lock list as subnets, and the country's dns
// recommendation unless the caller set a resolver.
func TestAuthNetworkClientCreatesValidProxyConfig(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")

		// a country that a country code selects, with a dns recommendation
		countryLocation := &Location{
			LocationType: LocationTypeCountry,
			Country:      "Russia",
			CountryCode:  "ru",
		}
		CreateLocation(ctx, countryLocation)
		ruLocation := GetConnectLocationForCountryCode(ctx, "RU")
		connect.AssertNotEqual(t, ruLocation, nil)
		connect.AssertEqual(t, ruLocation.CountryCode, "ru")
		ruDnsResolverSettings := connect.RegionalDnsResolverSettings("ru")
		connect.AssertNotEqual(t, ruDnsResolverSettings, nil)
		callerDnsResolverSettings := &connect.DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
		}
		bestAvailableLocation := &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
		}

		for _, c := range []struct {
			name                string
			proxyConfig         *ProxyConfig
			lockSubnets         []netip.Prefix
			location            *sdk.ConnectLocation
			dnsResolverSettings *connect.DnsResolverSettings
		}{
			{
				name: "a location and a lock list",
				proxyConfig: &ProxyConfig{
					LockCallerIp: true,
					LockIpList:   []string{"192.0.2.1", "198.51.100.0/24", "2001:db8::1"},
					InitialDeviceState: &ExtendedProxyDeviceState{
						ProxyDeviceState: ProxyDeviceState{Location: bestAvailableLocation},
					},
				},
				lockSubnets: []netip.Prefix{
					// the test session's address
					netip.MustParsePrefix("0.0.0.0/32"),
					netip.MustParsePrefix("192.0.2.1/32"),
					netip.MustParsePrefix("198.51.100.0/24"),
					netip.MustParsePrefix("2001:db8::1/128"),
				},
				location: bestAvailableLocation,
			},
			{
				name: "a country code",
				proxyConfig: &ProxyConfig{
					InitialDeviceState: &ExtendedProxyDeviceState{CountryCode: "RU"},
				},
				location:            ruLocation,
				dnsResolverSettings: ruDnsResolverSettings,
			},
			{
				name: "a country code and the caller's resolver",
				proxyConfig: &ProxyConfig{
					InitialDeviceState: &ExtendedProxyDeviceState{
						ProxyDeviceState: ProxyDeviceState{DnsResolverSettings: callerDnsResolverSettings},
						CountryCode:      "ru",
					},
				},
				location:            ruLocation,
				dnsResolverSettings: callerDnsResolverSettings,
			},
		} {
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					Description: "proxy",
					DeviceSpec:  "proxy",
					ProxyConfig: c.proxyConfig,
				},
				userSession,
			)
			if err != nil {
				t.Fatalf("%s: %s", c.name, err)
			}
			if result.Error != nil {
				t.Fatalf("%s: refused: %s", c.name, result.Error.Message)
			}
			connect.AssertNotEqual(t, result.ClientId, nil)
			connect.AssertNotEqual(t, result.ByClientJwt, nil)
			connect.AssertNotEqual(t, result.ProxyConfigResult, nil)
			connect.AssertEqual(t, result.ProxyConfigResult.ClientId, *result.ClientId)

			proxyDeviceConfig := GetProxyDeviceConfig(ctx, result.ProxyConfigResult.ProxyId)
			connect.AssertNotEqual(t, proxyDeviceConfig, nil)
			connect.AssertEqual(t, proxyDeviceConfig.ClientId, *result.ClientId)
			if !reflect.DeepEqual(proxyDeviceConfig.LockSubnets, c.lockSubnets) {
				t.Fatalf("%s: stored lock subnets %v, want %v", c.name, proxyDeviceConfig.LockSubnets, c.lockSubnets)
			}
			connect.AssertNotEqual(t, proxyDeviceConfig.InitialDeviceState, nil)
			if !reflect.DeepEqual(proxyDeviceConfig.InitialDeviceState.Location, c.location) {
				t.Fatalf("%s: stored location %+v, want %+v", c.name, proxyDeviceConfig.InitialDeviceState.Location, c.location)
			}
			if !reflect.DeepEqual(proxyDeviceConfig.InitialDeviceState.DnsResolverSettings, c.dnsResolverSettings) {
				t.Fatalf(
					"%s: stored dns resolver settings %+v, want %+v",
					c.name,
					proxyDeviceConfig.InitialDeviceState.DnsResolverSettings,
					c.dnsResolverSettings,
				)
			}
		}

		clientCount, deviceCount, proxyCount := authClientTestNetworkCounts(ctx, networkId)
		connect.AssertEqual(t, clientCount, 3)
		connect.AssertEqual(t, deviceCount, 3)
		connect.AssertEqual(t, proxyCount, 3)
	})
}

// The countries that country_code selects are read once per process. A test
// env starts each test on a new database, so the read is reset with the env,
// as the other location caches are. Otherwise every later test would see the
// countries of the first test that looked a country code up.
func TestConnectLocationForCountryCodeReadsEachTestDatabase(t *testing.T) {
	for range 2 {
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx := context.Background()

			countryLocation := &Location{
				LocationType: LocationTypeCountry,
				Country:      "Russia",
				CountryCode:  "ru",
			}
			CreateLocation(ctx, countryLocation)

			connectLocation := GetConnectLocationForCountryCode(ctx, "ru")
			connect.AssertNotEqual(t, connectLocation, nil)
			connect.AssertEqual(t, connectLocation.ConnectLocationId.LocationId, server.ToSdkId(countryLocation.LocationId))
			connect.AssertEqual(t, connectLocation.CountryLocationId, server.ToSdkId(countryLocation.LocationId))
		})
	}
}

// Creates a network and a session of its admin, a network-level caller.
func authClientTestNetwork(ctx context.Context, networkName string) (networkId server.Id, userSession *session.ClientSession) {
	networkId = server.NewId()
	userId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, networkName, userId)
	userSession = session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
		NetworkId: networkId,
		UserId:    userId,
	})
	return
}

// Counts the network's clients, devices and proxy configs.
func authClientTestNetworkCounts(ctx context.Context, networkId server.Id) (clientCount int, deviceCount int, proxyCount int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				(SELECT count(*) FROM network_client WHERE network_id = $1),
				(SELECT count(*) FROM device WHERE network_id = $1),
				(SELECT count(*)
					FROM proxy_device_config
					JOIN network_client ON network_client.client_id = proxy_device_config.client_id
					WHERE network_client.network_id = $1)
			`,
			networkId,
		).Scan(&clientCount, &deviceCount, &proxyCount))
	})
	return
}
