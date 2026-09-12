package connect

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

func TestIpFamilyIntentParsing(t *testing.T) {
	for value, intent := range map[string]int32{
		"4": 4, "6": 6, "": 0, "5": 0, "v6": 0, "06": 6, " 4": 0,
	} {
		connect.AssertEqual(t, ipFamilyIntentFromHeader(value), intent)
	}
	connect.AssertEqual(t, authIpFamilyIntent(nil), 0)
	connect.AssertEqual(t, authIpFamilyIntent(&protocol.Auth{IpFamily: 6}), 6)
	connect.AssertEqual(t, authIpFamilyIntent(&protocol.Auth{IpFamily: 7}), 0)

	clientId := server.NewId()
	ipVersion, ipFamilyIntent := connectionIpFamily(clientId, "10.0.0.1:443", &protocol.Auth{IpFamily: 6})
	connect.AssertEqual(t, ipVersion, 4)
	connect.AssertEqual(t, ipFamilyIntent, 6)
	ipVersion, ipFamilyIntent = connectionIpFamily(clientId, "[2001:db8::1]:443", &protocol.Auth{IpFamily: 6})
	connect.AssertEqual(t, ipVersion, 6)
	connect.AssertEqual(t, ipFamilyIntent, 6)
	// the v4-mapped form is observed as v4
	ipVersion, _ = connectionIpFamily(clientId, "[::ffff:10.0.0.1]:443", nil)
	connect.AssertEqual(t, ipVersion, 4)
	ipVersion, ipFamilyIntent = connectionIpFamily(clientId, "garbage", nil)
	connect.AssertEqual(t, ipVersion, 0)
	connect.AssertEqual(t, ipFamilyIntent, 0)
}

// An h1 connection records the family it arrived on and the intent it
// declared, on both loopback families (connect/IPV6.md A3). The proof rule
// itself is judged at aggregation time; see the model tests.
func TestConnectRecordsIpFamilyIntent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		exchangeSettings := DefaultExchangeSettings()
		exchangeSettings.KeyEventDelivery.Enabled = false
		exchange := NewExchange(
			ctx,
			"host0",
			"test",
			"test",
			map[int]int{},
			map[string]string{"host0": "127.0.0.1"},
			exchangeSettings,
		)
		defer exchange.Close()

		handlerId := model.CreateNetworkClientHandler(ctx)
		settings := DefaultConnectHandlerSettings()
		settings.ConnectionAnnounceTimeout = 0
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1000
		handler := NewConnectHandler(ctx, handlerId, exchange, settings)
		defer handler.Close()

		serve := func(network string, address string) string {
			listener, err := net.Listen(network, address)
			if err != nil {
				t.Fatalf("%s loopback is required for dual-stack tests: %v", network, err)
			}
			httpServer := &http.Server{
				Handler: http.HandlerFunc(handler.Connect),
			}
			go httpServer.Serve(listener)
			go func() {
				<-ctx.Done()
				httpServer.Close()
			}()
			return listener.Addr().String()
		}
		v4Address := serve("tcp4", "127.0.0.1:0")
		v6Address := serve("tcp6", "[::1]:0")

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "testConnectIpFamily"
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

		readConnection := func(clientId server.Id) (ipVersion int, ipFamilyIntent int, ok bool) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT ip_version, ip_family_intent
					FROM network_client_connection
					WHERE client_id = $1
					ORDER BY connect_time DESC
					LIMIT 1
					`,
					clientId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&ipVersion, &ipFamilyIntent))
						ok = true
					}
				})
			})
			return
		}

		connectAndRead := func(address string, intentHeader string) (ipVersion int, ipFamilyIntent int) {
			t.Helper()
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "d", "d")
			byJwt := jwt.NewByJwt(networkId, userId, networkName, false, false).Client(deviceId, clientId)

			header := http.Header{}
			header.Set("Authorization", "Bearer "+byJwt.Sign())
			header.Set("X-UR-AppVersion", "0.0.0")
			header.Set("X-UR-InstanceId", server.NewId().String())
			header.Set("X-UR-TransportVersion", "2")
			if intentHeader != "" {
				header.Set(connect.HeaderIpFamily, intentHeader)
			}
			ws, _, err := websocket.DefaultDialer.DialContext(ctx, "ws://"+address+"/", header)
			if err != nil {
				t.Fatalf("dial %s: %v", address, err)
			}
			defer ws.Close()

			deadline := time.Now().Add(30 * time.Second)
			for {
				ipVersion, ipFamilyIntent, ok := readConnection(clientId)
				if ok {
					return ipVersion, ipFamilyIntent
				}
				if deadline.Before(time.Now()) {
					t.Fatalf("no connection row for %s", clientId)
				}
				select {
				case <-ctx.Done():
					t.Fatal("canceled")
				case <-time.After(100 * time.Millisecond):
				}
			}
		}

		ipVersion, ipFamilyIntent := connectAndRead(v6Address, "6")
		connect.AssertEqual(t, ipVersion, 6)
		connect.AssertEqual(t, ipFamilyIntent, 6)

		ipVersion, ipFamilyIntent = connectAndRead(v4Address, "4")
		connect.AssertEqual(t, ipVersion, 4)
		connect.AssertEqual(t, ipFamilyIntent, 4)

		// a mismatch is stored as declared; the aggregation judges it
		ipVersion, ipFamilyIntent = connectAndRead(v4Address, "6")
		connect.AssertEqual(t, ipVersion, 4)
		connect.AssertEqual(t, ipFamilyIntent, 6)

		// no header is legacy
		ipVersion, ipFamilyIntent = connectAndRead(v4Address, "")
		connect.AssertEqual(t, ipVersion, 4)
		connect.AssertEqual(t, ipFamilyIntent, 0)

		// an unparseable header is legacy too
		ipVersion, ipFamilyIntent = connectAndRead(v6Address, "both")
		connect.AssertEqual(t, ipVersion, 6)
		connect.AssertEqual(t, ipFamilyIntent, 0)
	})
}
