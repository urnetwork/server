package model

// The initial performance profile of a hosted proxy device: the validation
// that mirrors connect's, the refusal on auth-client, and the read back of a
// stored profile that connect refuses.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"

	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/session"
)

// The server checks a profile with connect's own validation of the profile the
// sdk device would install, so the server, the sdk and connect cannot
// disagree. Each case pins the conversion, and the server's verdict must be
// connect's verdict. The verdict of the cases marked "connect" depends on the
// connect version: a connect that refuses negative sizes, a negative
// reconnect scale and a fixed window with max 0 makes the server refuse them
// too, with no server change.
func TestValidatePerformanceProfile(t *testing.T) {
	for _, c := range []struct {
		name               string
		performanceProfile *sdk.PerformanceProfile
		connectProfile     *connect.PerformanceProfile
		// "valid", "invalid", or "connect" (decided by the connect version)
		want string
	}{
		{
			name: "nil is auto",
			want: "valid",
		},
		{
			name:               "auto",
			performanceProfile: &sdk.PerformanceProfile{WindowType: sdk.WindowTypeAuto, PostQuantumEncryption: true},
			connectProfile: &connect.PerformanceProfile{
				WindowType:            connect.WindowTypeAuto,
				WindowSize:            connect.DefaultWindowSizeSettings(),
				PostQuantumEncryption: true,
			},
			want: "valid",
		},
		{
			name: "fixed ip",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeQuality,
				WindowSize: connect.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1, FixedWindowSize: 1},
			},
			want: "valid",
		},
		{
			name: "quality 2..4",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{
					WindowSizeMin:            2,
					WindowSizeMinP2pOnly:     1,
					WindowSizeMax:            4,
					WindowSizeHardMax:        6,
					WindowSizeReconnectScale: 0.5,
					KeepHealthiestCount:      1,
					Ulimit:                   8,
				},
				AllowDirect: true,
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeQuality,
				WindowSize: connect.WindowSizeSettings{
					WindowSizeMin:            2,
					WindowSizeMinP2pOnly:     1,
					WindowSizeMax:            4,
					WindowSizeHardMax:        6,
					WindowSizeReconnectScale: 0.5,
					KeepHealthiestCount:      1,
					Ulimit:                   8,
				},
				AllowDirect: true,
			},
			want: "valid",
		},
		{
			name:               "speed with the default window",
			performanceProfile: &sdk.PerformanceProfile{WindowType: sdk.WindowTypeSpeed},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeSpeed,
				WindowSize: connect.DefaultWindowSizeSettings(),
			},
			want: "valid",
		},
		{
			name: "an unknown window type is auto",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: "fastest",
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeAuto,
				WindowSize: connect.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
			},
			want: "valid",
		},
		{
			name: "quality max below min",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 1},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeQuality,
				WindowSize: connect.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 1},
			},
			want: "invalid",
		},
		{
			name: "auto max below min",
			performanceProfile: &sdk.PerformanceProfile{
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 2},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeAuto,
				WindowSize: connect.WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 2},
			},
			want: "invalid",
		},
		{
			name: "negative window",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: sdk.WindowTypeSpeed,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: -1, WindowSizeMax: -1},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeSpeed,
				WindowSize: connect.WindowSizeSettings{WindowSizeMin: -1, WindowSizeMax: -1, FixedWindowSize: -1},
			},
			want: "connect",
		},
		{
			name: "negative reconnect scale",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 2, WindowSizeReconnectScale: -1},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeQuality,
				WindowSize: connect.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 2, WindowSizeReconnectScale: -1},
			},
			want: "connect",
		},
		{
			name: "fixed window with max 0",
			performanceProfile: &sdk.PerformanceProfile{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{},
			},
			connectProfile: &connect.PerformanceProfile{
				WindowType: connect.WindowTypeQuality,
			},
			want: "connect",
		},
	} {
		err := validatePerformanceProfile(c.performanceProfile)
		if c.performanceProfile != nil {
			if got := connectPerformanceProfile(c.performanceProfile); !reflect.DeepEqual(got, c.connectProfile) {
				t.Fatalf("%s: converted to %+v, want %+v", c.name, got, c.connectProfile)
			}
			connectErr := c.connectProfile.Validate()
			if (err == nil) != (connectErr == nil) || (err != nil && err.Error() != connectErr.Error()) {
				t.Fatalf("%s: validated %v, connect validates %v", c.name, err, connectErr)
			}
		}
		switch c.want {
		case "valid":
			if err != nil {
				t.Fatalf("%s: refused a valid profile: %v", c.name, err)
			}
		case "invalid":
			if err == nil {
				t.Fatalf("%s: accepted an invalid profile", c.name)
			}
		}
	}
}

// auth-client refuses an initial performance profile that connect refuses,
// before anything is created, and tells the client why. It used to store the
// profile, and the hosted proxy device applied it at every creation, which
// panicked connect on the proxy host. A client that sends no profile, or a
// valid one, is unaffected, and its profile is stored as sent.
func TestAuthNetworkClientRefusesInvalidPerformanceProfile(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "test", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
		})

		authProxy := func(performanceProfile *sdk.PerformanceProfile) *AuthNetworkClientResult {
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
								PerformanceProfile: performanceProfile,
							},
						},
					},
				},
				userSession,
			)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result, nil)
			return result
		}
		networkCounts := func() (clientCount int, deviceCount int, proxyCount int) {
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
		refusedCount := func() float64 {
			return testutil.ToFloat64(proxyInvalidPerformanceProfilesAuthClient)
		}

		refusedBefore := refusedCount()
		invalidProfiles := []*sdk.PerformanceProfile{
			// the window that panicked connect on the proxy host
			{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 1},
			},
			// auto ignores the window, but connect still refuses it
			{
				WindowType: sdk.WindowTypeAuto,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 2},
			},
		}
		for _, performanceProfile := range invalidProfiles {
			result := authProxy(performanceProfile)
			if result.Error == nil {
				t.Fatalf("an invalid performance profile was accepted: %+v", result)
			}
			if !strings.HasPrefix(result.Error.Message, "Invalid performance profile: ") {
				t.Fatalf("unexpected refusal message: %q", result.Error.Message)
			}
			// the client learns which value was wrong
			if !strings.Contains(result.Error.Message, connectPerformanceProfile(performanceProfile).Validate().Error()) {
				t.Fatalf("the refusal does not say why: %q", result.Error.Message)
			}
			connect.AssertEqual(t, result.Error.ClientLimitExceeded, false)
			connect.AssertEqual(t, result.Error.UpgradeRequired, false)
			connect.AssertEqual(t, result.ClientId == nil, true)
			connect.AssertEqual(t, result.ByClientJwt == nil, true)
			connect.AssertEqual(t, result.ProxyConfigResult == nil, true)
		}
		connect.AssertEqual(t, refusedCount()-refusedBefore, float64(len(invalidProfiles)))

		// nothing was created for a refused request
		clientCount, deviceCount, proxyCount := networkCounts()
		connect.AssertEqual(t, clientCount, 0)
		connect.AssertEqual(t, deviceCount, 0)
		connect.AssertEqual(t, proxyCount, 0)

		validProfiles := []*sdk.PerformanceProfile{
			// every existing client sends no profile
			nil,
			{WindowType: sdk.WindowTypeAuto, PostQuantumEncryption: true},
			// fixed ip
			{
				WindowType: sdk.WindowTypeQuality,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
			},
			{
				WindowType: sdk.WindowTypeSpeed,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
			},
		}
		for _, performanceProfile := range validProfiles {
			result := authProxy(performanceProfile)
			if result.Error != nil {
				t.Fatalf("a valid performance profile %+v was refused: %s", performanceProfile, result.Error.Message)
			}
			connect.AssertNotEqual(t, result.ClientId, nil)
			connect.AssertNotEqual(t, result.ProxyConfigResult, nil)

			proxyDeviceConfig := GetProxyDeviceConfig(ctx, result.ProxyConfigResult.ProxyId)
			connect.AssertNotEqual(t, proxyDeviceConfig, nil)
			connect.AssertNotEqual(t, proxyDeviceConfig.InitialDeviceState, nil)
			if !reflect.DeepEqual(proxyDeviceConfig.InitialDeviceState.PerformanceProfile, performanceProfile) {
				t.Fatalf(
					"stored performance profile %+v, sent %+v",
					proxyDeviceConfig.InitialDeviceState.PerformanceProfile,
					performanceProfile,
				)
			}
		}
		connect.AssertEqual(t, refusedCount()-refusedBefore, float64(len(invalidProfiles)))

		clientCount, deviceCount, proxyCount = networkCounts()
		connect.AssertEqual(t, clientCount, len(validProfiles))
		connect.AssertEqual(t, deviceCount, len(validProfiles))
		connect.AssertEqual(t, proxyCount, len(validProfiles))
	})
}

// A stored initial performance profile that connect refuses reads back in
// auto, keeping its direct and post-quantum choices, as the sdk device reads
// back a saved one. Before auth-client refused them, any client could store
// one, and the hosted proxy device applies the stored profile at every
// creation (proxy/proxy_device.go reads it with GetProxyDeviceConfig). The
// stored config is not rewritten, and a valid profile reads back as stored.
func TestProxyDeviceConfigReadsInvalidPerformanceProfileAsAuto(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		createProxyDeviceConfig := func(performanceProfile *sdk.PerformanceProfile) *ProxyDeviceConfig {
			proxyDeviceConfig := &ProxyDeviceConfig{
				InitialDeviceState: &ProxyDeviceState{
					Location: &sdk.ConnectLocation{
						ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
					},
					PerformanceProfile: performanceProfile,
				},
			}
			proxyDeviceConfig.ClientId = server.NewId()
			err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
			connect.AssertEqual(t, err, nil)
			return proxyDeviceConfig
		}
		storedPerformanceProfile := func(proxyId server.Id) *sdk.PerformanceProfile {
			var configJson string
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT config_json FROM proxy_device_config WHERE proxy_id = $1`,
					proxyId,
				).Scan(&configJson))
			})
			var proxyDeviceConfig ProxyDeviceConfig
			err := json.Unmarshal([]byte(configJson), &proxyDeviceConfig)
			connect.AssertEqual(t, err, nil)
			return proxyDeviceConfig.InitialDeviceState.PerformanceProfile
		}
		// the three reads of a stored config: the redis mirror, the pg
		// fallback once the mirror is gone, and the per-client read
		readPerformanceProfiles := func(proxyDeviceConfig *ProxyDeviceConfig) []*sdk.PerformanceProfile {
			var performanceProfiles []*sdk.PerformanceProfile
			read := func(readConfig *ProxyDeviceConfig) {
				connect.AssertNotEqual(t, readConfig, nil)
				connect.AssertEqual(t, readConfig.ProxyId, proxyDeviceConfig.ProxyId)
				connect.AssertNotEqual(t, readConfig.InitialDeviceState, nil)
				connect.AssertNotEqual(t, readConfig.InitialDeviceState.Location, nil)
				performanceProfiles = append(performanceProfiles, readConfig.InitialDeviceState.PerformanceProfile)
			}
			read(GetProxyDeviceConfig(ctx, proxyDeviceConfig.ProxyId))
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.Del(ctx, proxyDeviceConfigKey(proxyDeviceConfig.ProxyId)).Err())
			})
			read(GetProxyDeviceConfig(ctx, proxyDeviceConfig.ProxyId))
			read(GetProxyDeviceConfigForClient(ctx, proxyDeviceConfig.ClientId, proxyDeviceConfig.InstanceId))
			return performanceProfiles
		}
		readBackCount := func() float64 {
			return testutil.ToFloat64(proxyInvalidPerformanceProfilesStoredConfig)
		}

		readBackBefore := readBackCount()
		for _, invalidProfile := range []*sdk.PerformanceProfile{
			// max below min, the window that panicked connect on the proxy host
			{
				WindowType:            sdk.WindowTypeQuality,
				WindowSize:            &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 1},
				AllowDirect:           true,
				PostQuantumEncryption: true,
			},
			{
				WindowType:            sdk.WindowTypeAuto,
				WindowSize:            &sdk.WindowSizeSettings{WindowSizeMin: 3, WindowSizeMax: 2},
				PostQuantumEncryption: true,
			},
			{
				WindowType: sdk.WindowTypeSpeed,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 5, WindowSizeMax: 4},
			},
		} {
			proxyDeviceConfig := createProxyDeviceConfig(invalidProfile)

			autoProfile := &sdk.PerformanceProfile{
				WindowType:            sdk.WindowTypeAuto,
				AllowDirect:           invalidProfile.AllowDirect,
				PostQuantumEncryption: invalidProfile.PostQuantumEncryption,
			}
			for _, performanceProfile := range readPerformanceProfiles(proxyDeviceConfig) {
				if !reflect.DeepEqual(performanceProfile, autoProfile) {
					t.Fatalf("stored profile %+v read back as %+v, want auto %+v", invalidProfile, performanceProfile, autoProfile)
				}
				// what the hosted device receives, connect installs
				connect.AssertEqual(t, validatePerformanceProfile(performanceProfile), nil)
			}

			// the stored config is not rewritten
			connect.AssertEqual(t, reflect.DeepEqual(storedPerformanceProfile(proxyDeviceConfig.ProxyId), invalidProfile), true)
		}
		connect.AssertEqual(t, readBackCount()-readBackBefore, float64(3*3))

		readBackBefore = readBackCount()
		for _, validProfile := range []*sdk.PerformanceProfile{
			nil,
			// fixed ip
			{
				WindowType:            sdk.WindowTypeQuality,
				WindowSize:            &sdk.WindowSizeSettings{WindowSizeMin: 1, WindowSizeMax: 1},
				PostQuantumEncryption: true,
			},
			{
				WindowType: sdk.WindowTypeSpeed,
				WindowSize: &sdk.WindowSizeSettings{WindowSizeMin: 2, WindowSizeMax: 4},
			},
		} {
			proxyDeviceConfig := createProxyDeviceConfig(validProfile)
			for _, performanceProfile := range readPerformanceProfiles(proxyDeviceConfig) {
				if !reflect.DeepEqual(performanceProfile, validProfile) {
					t.Fatalf("stored profile %+v read back as %+v", validProfile, performanceProfile)
				}
			}
		}
		connect.AssertEqual(t, readBackCount(), readBackBefore)

		// a config with no initial state reads back without one
		proxyDeviceConfig := &ProxyDeviceConfig{}
		proxyDeviceConfig.ClientId = server.NewId()
		err := CreateProxyDeviceConfig(ctx, proxyDeviceConfig)
		connect.AssertEqual(t, err, nil)
		readConfig := GetProxyDeviceConfig(ctx, proxyDeviceConfig.ProxyId)
		connect.AssertNotEqual(t, readConfig, nil)
		connect.AssertEqual(t, readConfig.InitialDeviceState == nil, true)
	})
}
