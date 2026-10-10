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
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
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

// auth-client refuses a source_client_id that is not a client of the
// caller's network, and creates nothing. The refusal used to be set inside
// the lookup's result callback, and returning from that callback did not stop
// the transaction: the client was created anyway, on the zero device id, and
// the call answered with its credentials.
func TestAuthNetworkClientRefusesUnknownSourceClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		otherNetworkId, otherSession := authClientTestNetwork(ctx, "other")
		otherResult, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				Description: "device",
				DeviceSpec:  "device",
			},
			otherSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, otherResult.Error, nil)

		for _, c := range []struct {
			name           string
			sourceClientId server.Id
		}{
			{
				name:           "a client that does not exist",
				sourceClientId: server.NewId(),
			},
			{
				name:           "a client of another network",
				sourceClientId: *otherResult.ClientId,
			},
		} {
			clientCountBefore, deviceCountBefore, _ := authClientTestNetworkCounts(ctx, networkId)
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					SourceClientId: &c.sourceClientId,
					Description:    "device",
					DeviceSpec:     "device",
				},
				userSession,
			)
			if err != nil {
				t.Errorf("%s: the refusal failed the call: %s", c.name, err)
			} else if result == nil {
				t.Errorf("%s: no result", c.name)
			} else if result.Error == nil {
				t.Errorf("%s: the source client was accepted: client_id=%v", c.name, result.ClientId)
			} else {
				if result.Error.Message != "Client does not exist." {
					t.Errorf("%s: refused with %q", c.name, result.Error.Message)
				}
				if result.ClientId != nil || result.ByClientJwt != nil {
					t.Errorf(
						"%s: the refusal returned client_id=%v by_client_jwt=%t",
						c.name,
						result.ClientId,
						result.ByClientJwt != nil,
					)
				}
			}
			clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
			if clientCount != clientCountBefore || deviceCount != deviceCountBefore {
				t.Errorf(
					"%s: created %d clients and %d devices",
					c.name,
					clientCount-clientCountBefore,
					deviceCount-deviceCountBefore,
				)
			}
		}
		otherClientCount, _, _ := authClientTestNetworkCounts(ctx, otherNetworkId)
		connect.AssertEqual(t, otherClientCount, 1)
	})
}

// A source client of the caller's network still gets its ancillary client,
// on the source's own device, now that the source is checked before any
// write.
func TestAuthNetworkClientCreatesAncillaryClientOnSourceDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userSession := authClientTestNetwork(ctx, "test")
		authClient := func(sourceClientId *server.Id) *AuthNetworkClientResult {
			result, err := AuthNetworkClient(
				&AuthNetworkClientArgs{
					SourceClientId: sourceClientId,
					Description:    "device",
					DeviceSpec:     "device",
				},
				userSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, result.Error, nil)
			connect.AssertNotEqual(t, result.ClientId, nil)
			connect.AssertNotEqual(t, result.ByClientJwt, nil)
			return result
		}

		sourceResult := authClient(nil)
		result := authClient(sourceResult.ClientId)

		var deviceId server.Id
		var sourceClientId *server.Id
		var sourceDeviceId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT device_id, source_client_id FROM network_client WHERE client_id = $1`,
				*result.ClientId,
			).Scan(&deviceId, &sourceClientId))
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT device_id FROM network_client WHERE client_id = $1`,
				*sourceResult.ClientId,
			).Scan(&sourceDeviceId))
		})
		connect.AssertEqual(t, deviceId, sourceDeviceId)
		connect.AssertNotEqual(t, sourceClientId, nil)
		connect.AssertEqual(t, *sourceClientId, *sourceResult.ClientId)

		clientCount, deviceCount, _ := authClientTestNetworkCounts(ctx, networkId)
		connect.AssertEqual(t, clientCount, 2)
		connect.AssertEqual(t, deviceCount, 1)
	})
}

// A re-auth (client_id set) checks the client and its device before writing
// either, so a refusal writes nothing. It used to update the client's
// description and auth time first: "Device does not exist." then kept that
// update, and a client with no device failed the call on a scan of its null
// device id instead of being refused.
func TestAuthNetworkClientReauthRefusalsWriteNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")

		// a client with no device, from before clients had devices
		noDeviceClientId, _ := authClientTestClient(ctx, t, userSession)
		// a client whose device is gone
		goneDeviceClientId, goneDeviceId := authClientTestClient(ctx, t, userSession)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET device_id = NULL WHERE client_id = $1`,
				noDeviceClientId,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM device WHERE device_id = $1`,
				goneDeviceId,
			))
		})

		for _, c := range []struct {
			name     string
			clientId server.Id
			message  string
		}{
			{
				name:     "a client with no device",
				clientId: noDeviceClientId,
				message:  "Client needs to be migrated (support@ur.io).",
			},
			{
				name:     "a client whose device is gone",
				clientId: goneDeviceClientId,
				message:  "Device does not exist.",
			},
			{
				name:     "a client that does not exist",
				clientId: server.NewId(),
				message:  "Client does not exist.",
			},
		} {
			descriptionBefore, authTimeBefore, foundBefore := authClientTestClientState(ctx, c.clientId)
			// a failed scan panics, and the other cases still run
			result, panicValue, err := func() (result *AuthNetworkClientResult, panicValue any, err error) {
				defer func() {
					panicValue = recover()
				}()
				result, err = AuthNetworkClient(
					&AuthNetworkClientArgs{
						ClientId:    &c.clientId,
						Description: "after",
						DeviceSpec:  "after",
					},
					userSession,
				)
				return
			}()
			if panicValue != nil {
				t.Errorf("%s: the re-auth panicked: %v", c.name, panicValue)
			} else if err != nil {
				t.Errorf("%s: the re-auth failed: %s", c.name, err)
			} else if result == nil {
				t.Errorf("%s: no result", c.name)
			} else if result.Error == nil || result.Error.Message != c.message {
				t.Errorf("%s: answered %+v, want the refusal %q", c.name, result, c.message)
			} else if result.ClientId != nil || result.ByClientJwt != nil {
				t.Errorf("%s: the refusal returned credentials", c.name)
			}
			descriptionAfter, authTimeAfter, foundAfter := authClientTestClientState(ctx, c.clientId)
			if foundAfter != foundBefore || descriptionAfter != descriptionBefore || !authTimeAfter.Equal(authTimeBefore) {
				t.Errorf(
					"%s: the refused re-auth wrote the client: description %q -> %q, auth time %s -> %s",
					c.name,
					descriptionBefore,
					descriptionAfter,
					authTimeBefore,
					authTimeAfter,
				)
			}
		}
	})
}

// A re-auth of a client and its device updates both, as it did before the
// checks moved ahead of the writes.
func TestAuthNetworkClientReauthUpdatesClientAndDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		clientId, deviceId := authClientTestClient(ctx, t, userSession)
		_, authTimeBefore, _ := authClientTestClientState(ctx, clientId)

		result, err := AuthNetworkClient(
			&AuthNetworkClientArgs{
				ClientId:    &clientId,
				Description: "after",
				DeviceSpec:  "after",
			},
			userSession,
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, *result.ClientId, clientId)
		connect.AssertNotEqual(t, result.ByClientJwt, nil)

		description, authTime, _ := authClientTestClientState(ctx, clientId)
		connect.AssertEqual(t, description, "after")
		if !authTimeBefore.Before(authTime) {
			t.Fatalf("the re-auth did not update the auth time: %s -> %s", authTimeBefore, authTime)
		}
		var deviceSpec string
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT device_spec FROM device WHERE device_id = $1`,
				deviceId,
			).Scan(&deviceSpec))
		})
		connect.AssertEqual(t, deviceSpec, "after")
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
	userSession = session.Testing_CreateClientSession(ctx, &session.ByJwt{
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

// Creates a client with the description and device spec "before" and an auth
// time a day back, so that a later write to any of them shows.
func authClientTestClient(
	ctx context.Context,
	t testing.TB,
	userSession *session.ClientSession,
) (clientId server.Id, deviceId server.Id) {
	result, err := AuthNetworkClient(
		&AuthNetworkClientArgs{
			Description: "before",
			DeviceSpec:  "before",
		},
		userSession,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, nil)
	clientId = *result.ClientId
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
			clientId,
			server.NowUtc().Add(-24*time.Hour),
		))
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT device_id FROM network_client WHERE client_id = $1`,
			clientId,
		).Scan(&deviceId))
	})
	return
}

// Reads the client's stored description and auth time, and whether the client
// exists.
func authClientTestClientState(ctx context.Context, clientId server.Id) (description string, authTime time.Time, found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT description, auth_time FROM network_client WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&description, &authTime))
				found = true
			}
		})
	})
	return
}
