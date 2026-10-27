package model

// auth-client writes a new client and its proxy in one transaction, so a
// failure to create the proxy rolls the client back with it: the call hands
// out no credentials, leaves nothing behind, and a retry starts clean. After
// the commit only redis is written, and a redis failure there keeps the
// committed client and its proxy.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// A failure to write any of the proxy's rows rolls back the client, its
// device and its roles with the proxy, and the call returns no credentials.
// The proxy used to be created after the client's transaction committed, so
// the same failure left the client, its device and its roles in the network,
// and a failure at the proxy client also left the proxy device config. A retry
// once the failure clears creates one client with its proxy.
func TestAuthNetworkClientProxyWriteFailureCreatesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		sourceClientId, _ := authClientTestClient(ctx, t, userSession)

		for _, c := range []struct {
			name           string
			table          string
			sourceClientId *server.Id
			// a new top-level client has its own new device; an ancillary
			// client uses its source's device
			deviceCount int
		}{
			{
				name:        "the proxy device config of a top-level client",
				table:       "proxy_device_config",
				deviceCount: 1,
			},
			{
				name:        "the proxy client of a top-level client",
				table:       "proxy_client",
				deviceCount: 1,
			},
			{
				name:        "the proxy client change of a top-level client",
				table:       "proxy_client_change",
				deviceCount: 1,
			},
			{
				name:           "the proxy client of an ancillary client",
				table:          "proxy_client",
				sourceClientId: &sourceClientId,
				deviceCount:    0,
			},
		} {
			writesBefore := authClientTestWritesNow(ctx)
			removeFailure := authClientTestFailInserts(ctx, c.table)
			result, err, panicValue := authClientTestCreateProxy(userSession, c.sourceClientId)
			removeFailure()

			// the failure is the server's own, and the endpoint answers a
			// database failure with a 500
			failure := fmt.Sprintf("injected failure writing %s", c.table)
			var pgErr *pgconn.PgError
			if panicErr, ok := panicValue.(error); !ok || !errors.As(panicErr, &pgErr) || pgErr.Message != failure {
				t.Errorf("%s: the call failed with %v, want the injected failure %q", c.name, panicValue, failure)
			}
			if result != nil || err != nil {
				t.Errorf("%s: the failure returned client_id=%v error=%v", c.name, authClientTestResultClientId(result), err)
			}
			if writes := authClientTestWritesNow(ctx); writes != writesBefore {
				t.Errorf("%s: the failure left %+v, want %+v", c.name, writes, writesBefore)
			}

			// a retry once the failure clears
			result, err, panicValue = authClientTestCreateProxy(userSession, c.sourceClientId)
			if panicValue != nil || err != nil {
				t.Fatalf("%s: the retry failed: %v %v", c.name, panicValue, err)
			}
			if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil || result.ProxyConfigResult == nil {
				t.Fatalf("%s: the retry answered %+v", c.name, result)
			}
			connect.AssertEqual(t, result.ProxyConfigResult.ClientId, *result.ClientId)
			wantWrites := authClientTestWrites{
				clientCount:            writesBefore.clientCount + 1,
				deviceCount:            writesBefore.deviceCount + c.deviceCount,
				clientRoleCount:        writesBefore.clientRoleCount + 1,
				proxyDeviceConfigCount: writesBefore.proxyDeviceConfigCount + 1,
				proxyClientCount:       writesBefore.proxyClientCount + 1,
				proxyClientChangeCount: writesBefore.proxyClientChangeCount + 1,
			}
			if writes := authClientTestWritesNow(ctx); writes != wantWrites {
				t.Errorf("%s: the retry left %+v, want %+v", c.name, writes, wantWrites)
			}
		}
	})
}

// A server with no proxy hosts (proxy.yml has no hosts block) refuses a proxy
// with "Could not create proxy client" before anything is created. The refusal
// used to come after the client and its proxy device config were created, and
// it came back with the new client's client_id and by_client_jwt.
func TestAuthNetworkClientWithoutProxyHostsCreatesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")

		loadServerProxyConfig := LoadServerProxyConfig
		LoadServerProxyConfig = func() ServerProxyConfig {
			return ServerProxyConfig{}
		}
		defer func() {
			LoadServerProxyConfig = loadServerProxyConfig
		}()

		writesBefore := authClientTestWritesNow(ctx)
		result, err, panicValue := authClientTestCreateProxy(userSession, nil)
		if panicValue != nil || err != nil {
			t.Fatalf("the refusal failed the call: %v %v", panicValue, err)
		}
		if result.Error == nil || result.Error.Message != "Could not create proxy client" {
			t.Fatalf("answered %+v, want the refusal \"Could not create proxy client\"", result.Error)
		}
		if result.ClientId != nil || result.ByClientJwt != nil || result.ProxyConfigResult != nil {
			t.Fatalf(
				"the refusal returned client_id=%v by_client_jwt=%t proxy_config_result=%t",
				result.ClientId,
				result.ByClientJwt != nil,
				result.ProxyConfigResult != nil,
			)
		}
		if writes := authClientTestWritesNow(ctx); writes != writesBefore {
			t.Fatalf("the refusal left %+v, want %+v", writes, writesBefore)
		}
	})
}

// After the commit auth-client writes only redis: the client identity cache,
// the proxy device config mirror and the proxy hosts' wakeup. Here the caller
// goes away right after the commit (committedIdentityCacheCancelHook cancels
// it at its first redis call once the client has committed), so each of those
// writes fails. The call still returns the committed client with its
// wireguard proxy, which reads back from postgres. The proxy used to be
// created only after the client's commit, so the call failed and left the
// client without a proxy.
func TestAuthNetworkClientRedisFailureAfterCommitKeepsClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, userSession := authClientTestNetwork(ctx, "test")
		sourceClientId, _ := authClientTestClient(ctx, t, userSession)
		authClientTestSeedProxyClientIpv4s(ctx)

		callCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		hook := &committedIdentityCacheCancelHook{cancel: cancel, source: sourceClientId}
		hook.enabled.Store(true)
		defer hook.enabled.Store(false)
		callCtx = context.WithValue(callCtx, committedIdentityCacheContextKey{}, hook)
		server.Redis(ctx, func(client server.RedisClient) {
			client.AddHook(hook)
		})
		callSession := session.Testing_CreateClientSession(callCtx, userSession.ByJwt)
		defer callSession.Cancel()

		writesBefore := authClientTestWritesNow(ctx)
		result, err, panicValue := func() (result *AuthNetworkClientResult, err error, panicValue any) {
			defer func() {
				panicValue = recover()
			}()
			result, err = AuthNetworkClient(
				&AuthNetworkClientArgs{
					SourceClientId: &sourceClientId,
					Description:    "proxy",
					DeviceSpec:     "proxy",
					Roles:          []string{"proxy-test"},
					ProxyConfig: &ProxyConfig{
						EnableWg: true,
						InitialDeviceState: &ExtendedProxyDeviceState{
							ProxyDeviceState: ProxyDeviceState{Location: authClientTestBestAvailableLocation()},
						},
					},
				},
				callSession,
			)
			return
		}()
		hook.enabled.Store(false)
		// the caller went away after the commit
		if !hook.fired.Load() || hook.committed.Load() != 1 || callCtx.Err() == nil {
			t.Fatalf("the caller was not canceled after the commit")
		}
		if panicValue != nil || err != nil {
			t.Fatalf("a redis failure after the commit failed the call: %v %v", panicValue, err)
		}
		if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil || result.ProxyConfigResult == nil {
			t.Fatalf("answered %+v", result)
		}
		clientId := *result.ClientId
		proxyClient := result.ProxyConfigResult.ProxyClient
		connect.AssertEqual(t, proxyClient.ClientId, clientId)
		connect.AssertNotEqual(t, proxyClient.WgConfig, nil)

		// an ancillary client uses its source's device
		wantWrites := authClientTestWrites{
			clientCount:            writesBefore.clientCount + 1,
			deviceCount:            writesBefore.deviceCount,
			clientRoleCount:        writesBefore.clientRoleCount + 1,
			proxyDeviceConfigCount: writesBefore.proxyDeviceConfigCount + 1,
			proxyClientCount:       writesBefore.proxyClientCount + 1,
			proxyClientChangeCount: writesBefore.proxyClientChangeCount + 1,
		}
		if writes := authClientTestWritesNow(ctx); writes != wantWrites {
			t.Fatalf("the call left %+v, want %+v", writes, wantWrites)
		}

		// the redis writes failed
		server.Redis(ctx, func(r server.RedisClient) {
			keyCount, err := r.Exists(ctx, proxyDeviceConfigKey(proxyClient.ProxyId), clientIdentityKey(clientId)).Result()
			server.Raise(err)
			connect.AssertEqual(t, keyCount, int64(0))
		})

		// and the proxy reads back from postgres
		proxyDeviceConfig := GetProxyDeviceConfig(ctx, proxyClient.ProxyId)
		connect.AssertNotEqual(t, proxyDeviceConfig, nil)
		connect.AssertEqual(t, proxyDeviceConfig.ClientId, clientId)
		connect.AssertEqual(t, proxyDeviceConfig.InstanceId, proxyClient.InstanceId)
		storedProxyClient, err := GetProxyClient(ctx, proxyClient.ProxyId)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, storedProxyClient, nil)
		connect.AssertEqual(t, storedProxyClient.ClientId, clientId)
		connect.AssertNotEqual(t, storedProxyClient.WgConfig, nil)
		connect.AssertEqual(t, storedProxyClient.WgConfig.ClientIpv4, proxyClient.WgConfig.ClientIpv4)
	})
}

// What auth-client writes, counted across this test's own database.
type authClientTestWrites struct {
	clientCount            int
	deviceCount            int
	clientRoleCount        int
	proxyDeviceConfigCount int
	proxyClientCount       int
	proxyClientChangeCount int
}

// Counts what auth-client writes, now.
func authClientTestWritesNow(ctx context.Context) (writes authClientTestWrites) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				(SELECT count(*) FROM network_client),
				(SELECT count(*) FROM device),
				(SELECT count(*) FROM network_client_role),
				(SELECT count(*) FROM proxy_device_config),
				(SELECT count(*) FROM proxy_client),
				(SELECT count(*) FROM proxy_client_change)
			`,
		).Scan(
			&writes.clientCount,
			&writes.deviceCount,
			&writes.clientRoleCount,
			&writes.proxyDeviceConfigCount,
			&writes.proxyClientCount,
			&writes.proxyClientChangeCount,
		))
	})
	return
}

// Creates a client with a proxy and one role, ancillary to the source client
// when it is set. A panic is returned, not raised.
func authClientTestCreateProxy(
	userSession *session.ClientSession,
	sourceClientId *server.Id,
) (result *AuthNetworkClientResult, err error, panicValue any) {
	defer func() {
		panicValue = recover()
	}()
	result, err = AuthNetworkClient(
		&AuthNetworkClientArgs{
			SourceClientId: sourceClientId,
			Description:    "proxy",
			DeviceSpec:     "proxy",
			Roles:          []string{"proxy-test"},
			ProxyConfig: &ProxyConfig{
				InitialDeviceState: &ExtendedProxyDeviceState{
					ProxyDeviceState: ProxyDeviceState{Location: authClientTestBestAvailableLocation()},
				},
			},
		},
		userSession,
	)
	return
}

// Best available, the default location of a new proxy.
func authClientTestBestAvailableLocation() *sdk.ConnectLocation {
	return &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{BestAvailable: true},
	}
}

// The result's client id, or nil when there is no result.
func authClientTestResultClientId(result *AuthNetworkClientResult) *server.Id {
	if result == nil {
		return nil
	}
	return result.ClientId
}

// Fails every insert into the table with an injected error, in this test's
// own database, until the returned func removes the failure.
func authClientTestFailInserts(ctx context.Context, table string) (removeFailure func()) {
	sanitizedTable := pgx.Identifier{table}.Sanitize()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			CREATE OR REPLACE FUNCTION auth_client_test_fail_insert() RETURNS trigger
			LANGUAGE plpgsql AS $$
			BEGIN
				RAISE EXCEPTION 'injected failure writing %', TG_TABLE_NAME;
			END
			$$
			`,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			fmt.Sprintf(
				`
				CREATE TRIGGER auth_client_test_fail_insert
				BEFORE INSERT ON %s
				FOR EACH ROW EXECUTE FUNCTION auth_client_test_fail_insert()
				`,
				sanitizedTable,
			),
		))
	})
	return func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				fmt.Sprintf(`DROP TRIGGER auth_client_test_fail_insert ON %s`, sanitizedTable),
			))
		})
	}
}

// Seeds a few free wireguard client addresses at the top of the sequence, so
// that every random start of the allocation finds one.
func authClientTestSeedProxyClientIpv4s(ctx context.Context) {
	const ipv4Count = 8
	firstIpv4 := Ipv4ToInt(netip.MustParseAddr("198.51.100.1"))
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO proxy_client_ipv4 (sequence_id, client_ipv4)
			SELECT $1::bigint + i, $2::bigint + i
			FROM generate_series(0, $3::integer - 1) AS fixture(i)
			`,
			ProxyClientIpv4Count-ipv4Count,
			firstIpv4,
			ipv4Count,
		))
	})
}
