package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The real handler's H1 path negotiates the delay header (connect DELAY.md §6,
// §11). It accepts an offer on header auth for a connection that declares no
// provide intent, on both carriers, and from the first message after the 101
// stamps and echoes every message, its heartbeats included. It never accepts
// for a connection that declares provide intent or when the setting is off,
// and a client that does not offer keeps the legacy wire.
//
// The client here speaks the wire by hand, so the header layout is pinned
// independently of connect's own implementation: [sendStartMicros uint32]
// [previousWriteMicros uint32][echoOneWayMicros uint32], big-endian.
func TestConnectH1TransportDelay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		type delayCase struct {
			carrier       string
			offer         bool
			provideIntent bool
		}
		const (
			carrierWebSocket = "websocket"
			carrierH1Plus    = "h1plus"
		)
		for _, enableTransportDelay := range []bool{true, false} {
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
					settings.EnableH1Plus = true
					settings.EnableTransportDelay = enableTransportDelay
					// heartbeats every 50-100ms, and no latency or speed tests,
					// so every server write in the window is a heartbeat
					settings.MinPingTimeout = 50 * time.Millisecond
					settings.MaxPingTimeout = 100 * time.Millisecond
					settings.ConnectionTestConfig = V0TestConfig()
				})
				defer env.Close()
				address := fmt.Sprintf("ws://127.0.0.1:%d/", env.port)

				delayCases := []delayCase{
					{carrier: carrierWebSocket, offer: true},
					{carrier: carrierH1Plus, offer: true},
					{carrier: carrierWebSocket, offer: false},
					{carrier: carrierH1Plus, offer: false},
					{carrier: carrierWebSocket, offer: true, provideIntent: true},
					{carrier: carrierH1Plus, offer: true, provideIntent: true},
				}
				for _, delay := range delayCases {
					name := fmt.Sprintf("enable=%t carrier=%s offer=%t provideIntent=%t", enableTransportDelay, delay.carrier, delay.offer, delay.provideIntent)
					accepted := enableTransportDelay && delay.offer && !delay.provideIntent
					func() {
						_, token := env.authClient(&model.AuthNetworkClientArgs{Description: "h1 transport delay", DeviceSpec: "test"})
						header := http.Header{
							"Authorization":           []string{"Bearer " + token},
							"X-Ur-Instanceid":         []string{server.NewId().String()},
							"X-Ur-Transportversion":   []string{"2"},
							"X-Ur-Streamleaseversion": []string{"1"},
						}
						if delay.offer {
							header.Add("Sec-WebSocket-Protocol", connect.TransportDelayH1Subprotocol)
						}
						if delay.provideIntent {
							header.Set(HeaderProvideIntent, "1")
						}
						dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
						var conn connect.H1MessageConn
						var subprotocol string
						switch delay.carrier {
						case carrierH1Plus:
							raw, err := connect.DialFramedUpgrade(ctx, address, header, dialer, connect.H1FramerProtocol)
							if err != nil {
								t.Fatalf("%s: dial: %s", name, err)
							}
							// a legacy framed reader: each message body is the
							// header followed by the payload
							framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerProtocol, 65535, nil)
							if err != nil {
								raw.Close()
								t.Fatalf("%s: framed conn: %s", name, err)
							}
							conn, subprotocol = framed, framed.Subprotocol()
						default:
							webSocket, response, err := dialer.DialContext(ctx, address, header)
							if response != nil && response.Body != nil {
								response.Body.Close()
							}
							if err != nil {
								t.Fatalf("%s: dial: %s", name, err)
							}
							conn, subprotocol = webSocket, webSocket.Subprotocol()
						}
						defer conn.Close()

						if accepted != (subprotocol == connect.TransportDelayH1Subprotocol) {
							t.Fatalf("%s: the 101 selected %q, expected accepted=%t", name, subprotocol, accepted)
						}
						// a client from before the header must see the 101 it
						// always saw: no subprotocol at all
						if !accepted && subprotocol != "" {
							t.Fatalf("%s: the 101 selected %q, expected none", name, subprotocol)
						}
						if delay.provideIntent {
							// a provider connection runs its own admission check
							return
						}
						if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
							t.Fatalf("%s: %s", name, err)
						}
						if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
							t.Fatalf("%s: %s", name, err)
						}
						if !accepted {
							// a legacy heartbeat is a zero-length message both
							// ways, and a stamped one is the bare 12-byte header.
							// A connection stamps every message or none, so
							// several empty heartbeats show it stamps none. The
							// server's own frames may arrive between them (a new
							// connection's control messages), and each must
							// parse as a legacy transfer frame.
							heartbeatCount := 0
							for index := 0; heartbeatCount < 4; index += 1 {
								if 32 <= index {
									t.Fatalf("%s: %d server messages, %d legacy heartbeats", name, index, heartbeatCount)
								}
								if err := conn.WriteMessage(websocket.BinaryMessage, []byte{}); err != nil {
									t.Fatalf("%s: write: %s", name, err)
								}
								_, message, err := conn.ReadMessage()
								if err != nil {
									t.Fatalf("%s: read: %s", name, err)
								}
								if len(message) == 0 {
									heartbeatCount += 1
									continue
								}
								if err := proto.Unmarshal(message, &protocol.TransferFrame{}); err != nil {
									t.Fatalf("%s: server message %d of %d bytes is not a legacy frame: %s", name, index, len(message), err)
								}
							}
							return
						}

						// one stamped heartbeat per server message, each its own
						// write group, until the server has sent a stamped
						// heartbeat and echoed the one-way delay of our writes
						sendStart := uint32(1000)
						previousServerSendStart := uint32(0)
						heartbeatCount := 0
						echoed := false
						for index := 0; !echoed || heartbeatCount == 0; index += 1 {
							if 100 <= index {
								t.Fatalf("%s: %d server messages: %d heartbeats, echoed=%t", name, index, heartbeatCount, echoed)
							}
							sendStart += 1000
							var clientHeader [12]byte
							binary.BigEndian.PutUint32(clientHeader[0:4], sendStart)
							if err := conn.WriteMessage(websocket.BinaryMessage, clientHeader[:]); err != nil {
								t.Fatalf("%s: write: %s", name, err)
							}
							kind, message, err := conn.ReadMessage()
							if err != nil {
								t.Fatalf("%s: read: %s", name, err)
							}
							if kind != websocket.BinaryMessage || len(message) < 12 {
								t.Fatalf("%s: server message %d of %d bytes carries no header", name, index, len(message))
							}
							// the 12-byte header alone is a stamped heartbeat
							if len(message) == 12 {
								heartbeatCount += 1
							}
							serverSendStart := binary.BigEndian.Uint32(message[0:4])
							previousWrite := binary.BigEndian.Uint32(message[4:8])
							echo := binary.BigEndian.Uint32(message[8:12])
							// the frames of one batch share a header; a new write
							// starts later and follows a completed write
							if 0 < index && serverSendStart != previousServerSendStart {
								if int32(serverSendStart-previousServerSendStart) <= 0 {
									t.Fatalf("%s: server write at message %d starts at %d, not after %d", name, index, serverSendStart, previousServerSendStart)
								}
								if previousWrite == 0 {
									t.Fatalf("%s: server write at message %d reports no previous write", name, index)
								}
							}
							previousServerSendStart = serverSendStart
							echoed = echoed || echo != 0
						}
					}()
				}
			}()
		}
	})
}

// Two clients exchange messages through the real handler over delayed H1
// connections on each carrier: transfer frames, contracts and acks ride the
// header wire in both directions, the server's ready batches included, and
// each client publishes its connection's intervals to its delay callbacks.
//
// The last case is a client from before the header on h1+: it never offers,
// beside one that does. The server stamps only the offering client's
// connection, so every frame the legacy client reads, the ones forwarded from
// the offering client included, is the legacy wire it always read. A stamped
// frame would carry 12 bytes it cannot parse, and the exchange would stall.
func TestConnectH1TransportDelayExchange(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, nil, func(settings *ConnectHandlerSettings) {
			settings.EnableH1Plus = true
		})
		defer env.Close()

		// collects one client's published intervals from transport goroutines
		type delayStatsCollector struct {
			stateLock sync.Mutex
			stats     []connect.TransportDelayStats
		}
		type exchangeCase struct {
			enableH1Plus bool
			// client b never offers the header
			legacyB bool
		}
		for _, exchange := range []exchangeCase{
			{enableH1Plus: false},
			{enableH1Plus: true},
			{enableH1Plus: true, legacyB: true},
		} {
			enableH1Plus := exchange.enableH1Plus
			name := fmt.Sprintf("enableH1Plus=%t legacyB=%t", enableH1Plus, exchange.legacyB)
			func() {
				clientIdA, byClientJwtA := env.authClient(&model.AuthNetworkClientArgs{Description: "delay a", DeviceSpec: "test"})
				clientIdB, byClientJwtB := env.authClient(&model.AuthNetworkClientArgs{Description: "delay b", DeviceSpec: "test"})
				clientA := env.newClient(clientIdA)
				defer clientA.Close()
				clientB := env.newClient(clientIdB)
				defer clientB.Close()

				collect := func(client *connect.Client) *delayStatsCollector {
					collector := &delayStatsCollector{}
					client.AddTransportDelayStatsCallback(func(stats *connect.TransportDelayStats) {
						collector.stateLock.Lock()
						defer collector.stateLock.Unlock()
						collector.stats = append(collector.stats, *stats)
					})
					return collector
				}
				collectorA := collect(clientA)
				collectorB := collect(clientB)
				receiveB := recordPeerReceives(clientB, clientIdA)
				receiveA := recordPeerReceives(clientA, clientIdB)

				h1ConnectionStats := &connect.H1ConnectionStats{}
				newTransport := func(byClientJwt string, routeManager *connect.RouteManager, offer bool) *connect.PlatformTransport {
					settings := connect.DefaultPlatformTransportSettings()
					settings.QuicTlsConfig.InsecureSkipVerify = true
					settings.H3Port = env.h3Port
					settings.DnsPort = 0
					settings.EnableH1Plus = enableH1Plus
					settings.H1ConnectionStats = h1ConnectionStats
					if offer {
						settings.TransportDelay = connect.DefaultTransportDelaySettings()
						settings.TransportDelay.Warmup = 0
						settings.TransportDelay.MinDelaySampleCount = 1
					} else {
						// a client from before the header offers nothing
						settings.TransportDelay = nil
					}
					return connect.NewPlatformTransportWithTargetMode(
						ctx,
						connect.NewClientStrategyWithDefaults(ctx),
						routeManager,
						fmt.Sprintf("ws://127.0.0.1:%d", env.port),
						&connect.ClientAuth{ByJwt: byClientJwt, InstanceId: connect.Id(server.NewId()), AppVersion: "0.0.0"},
						connect.TransportModeH1,
						settings,
					)
				}
				transportA := newTransport(byClientJwtA, clientA.RouteManager(), true)
				defer transportA.Close()
				transportB := newTransport(byClientJwtB, clientB.RouteManager(), !exchange.legacyB)
				defer transportB.Close()

				env.setProvideModes(clientA, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
				env.setProvideModes(clientB, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})

				const messageCount = 16
				for range messageCount {
					sendSimpleMessage(t, clientA, clientIdB)
					sendSimpleMessage(t, clientB, clientIdA)
				}
				for _, receive := range []chan connect.Peer{receiveB, receiveA} {
					for index := range messageCount {
						select {
						case <-receive:
						case <-time.After(60 * time.Second):
							t.Fatalf("%s: received %d of %d messages", name, index, messageCount)
						}
					}
				}
				h1Snapshot := h1ConnectionStats.Snapshot()
				if enableH1Plus && h1Snapshot.H1PlusConnectionCount != 2 || !enableH1Plus && h1Snapshot.WebSocketConnectionCount != 2 {
					t.Fatalf("%s: h1 connections %+v", name, h1Snapshot)
				}

				// each connection publishes its last interval when it ends
				closeCtx, closeCancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer closeCancel()
				for _, transport := range []*connect.PlatformTransport{transportA, transportB} {
					if err := transport.CloseAndWait(closeCtx); err != nil {
						t.Fatalf("%s: %s", name, err)
					}
				}
				for clientName, collector := range map[string]*delayStatsCollector{"a": collectorA, "b": collectorB} {
					collector.stateLock.Lock()
					intervals := slices.Clone(collector.stats)
					collector.stateLock.Unlock()
					if clientName == "b" && exchange.legacyB {
						// the legacy client negotiated nothing, so it measured nothing
						if len(intervals) != 0 {
							t.Fatalf("%s: legacy client b published %d intervals", name, len(intervals))
						}
						continue
					}
					sendByteCount := connect.ByteCount(0)
					receiveByteCount := connect.ByteCount(0)
					receiveQueueDelayValid := false
					sendQueueDelayValid := false
					for _, interval := range intervals {
						if interval.TransportType != connect.TransportTypeH1 {
							t.Fatalf("%s: client %s published a %v interval", name, clientName, interval.TransportType)
						}
						sendByteCount += interval.SendByteCount
						receiveByteCount += interval.ReceiveByteCount
						receiveQueueDelayValid = receiveQueueDelayValid || interval.ReceiveQueueDelayValid
						sendQueueDelayValid = sendQueueDelayValid || interval.SendQueueDelayValid
					}
					if sendByteCount == 0 || receiveByteCount == 0 {
						t.Fatalf("%s: client %s sent %d and received %d bytes in %d intervals", name, clientName, sendByteCount, receiveByteCount, len(intervals))
					}
					// down from the server's stamps, up from its echo
					if !receiveQueueDelayValid || !sendQueueDelayValid {
						t.Fatalf("%s: client %s queue delay valid receive=%t send=%t", name, clientName, receiveQueueDelayValid, sendQueueDelayValid)
					}
				}
			}()
		}
	})
}

// The handler's acceptance of the delay header. A client from before the
// header sends no offer, or only other subprotocols, and must keep the legacy
// wire whatever the server's setting; an offer is accepted only on header auth,
// with no provide intent, with the setting on. Hermetic: no database.
func TestConnectH1TransportDelayResponseHeader(t *testing.T) {
	type acceptCase struct {
		name          string
		enable        bool
		headerAuth    bool
		subprotocols  []string
		provideIntent bool
		accepted      bool
	}
	offer := connect.TransportDelayH1Subprotocol
	cases := []acceptCase{
		{name: "legacy client", enable: true, headerAuth: true},
		{name: "legacy client with other subprotocols", enable: true, headerAuth: true, subprotocols: []string{"chat, superchat"}},
		{name: "offer", enable: true, headerAuth: true, subprotocols: []string{offer}, accepted: true},
		{name: "offer among others", enable: true, headerAuth: true, subprotocols: []string{"chat, " + offer}, accepted: true},
		{name: "offer on a second line", enable: true, headerAuth: true, subprotocols: []string{"chat", offer}, accepted: true},
		{name: "offer with the setting off", headerAuth: true, subprotocols: []string{offer}},
		{name: "offer on first-frame auth", enable: true, subprotocols: []string{offer}},
		{name: "offer with provide intent", enable: true, headerAuth: true, subprotocols: []string{offer}, provideIntent: true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
		for _, line := range c.subprotocols {
			r.Header.Add("Sec-WebSocket-Protocol", line)
		}
		if c.provideIntent {
			r.Header.Set(HeaderProvideIntent, "1")
		}
		header := connectH1TransportDelayResponseHeader(c.enable, c.headerAuth, r)
		if c.accepted {
			if header.Get("Sec-WebSocket-Protocol") != connect.TransportDelayH1Subprotocol {
				t.Fatalf("%s: accepted with %v", c.name, header)
			}
		} else if header != nil {
			t.Fatalf("%s: accepted with %v, expected the legacy wire", c.name, header)
		}
	}
}

// An h1+ client from before the delay header, which never offers it, reads
// every message the server writes byte for byte: the empty heartbeat stays
// empty and each frame arrives with no 12-byte header in front, whatever the
// server's setting. The server half is the handler's h1+ path: its acceptance
// (connectH1TransportDelayResponseHeader), then the upgrade, the framer and the
// peer conn, in its order. An offering client is the control, so the test sees
// a header when there is one. Hermetic: no database.
func TestConnectH1PlusLegacyClientGetsNoDelayHeader(t *testing.T) {
	type upgradeCase struct {
		name          string
		enable        bool
		offer         bool
		provideIntent bool
		// the control: the header is negotiated
		delayed bool
	}
	cases := []upgradeCase{
		{name: "legacy client, setting on", enable: true},
		{name: "legacy client, setting off"},
		{name: "legacy provider, setting on", enable: true, provideIntent: true},
		{name: "offering client, setting off", offer: true},
		{name: "offering provider, setting on", enable: true, offer: true, provideIntent: true},
		{name: "offering client, setting on", enable: true, offer: true, delayed: true},
	}
	// a heartbeat, frames of a few sizes, and a frame of exactly a bare
	// header's 12 bytes
	payloads := [][]byte{
		{},
		[]byte("transfer frame"),
		bytes.Repeat([]byte{0xa5}, 12),
		bytes.Repeat([]byte{0x5a}, 4096),
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			clientDone := make(chan struct{})
			serverDone := make(chan error, 1)
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serverDone <- func() error {
					if !connect.IsFramedUpgrade(r, connect.H1FramerProtocol) {
						return fmt.Errorf("not an h1+ upgrade")
					}
					if err := connect.ValidateFramedUpgradeRequest(r, connect.H1FramerProtocol); err != nil {
						return err
					}
					// h1+ is header auth only
					responseHeader := connectH1TransportDelayResponseHeader(c.enable, true, r)
					conn, err := connect.AcceptFramedUpgradeWithHeader(w, r, connect.H1FramerProtocol, 5*time.Second, responseHeader)
					if err != nil {
						return err
					}
					framed, err := connect.NewFramedMessageConn(conn, connect.H1FramerProtocol, 65535, nil)
					if err != nil {
						conn.Close()
						return err
					}
					var ws connect.H1MessageConn = framed
					if delayed, ok := connect.NewTransportDelayH1PeerConn(ws, connect.Id{}); ok {
						ws = delayed
					}
					defer ws.Close()
					if err := ws.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
						return err
					}
					for _, payload := range payloads {
						if err := ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
							return err
						}
					}
					// hold the connection until the client has read it all
					select {
					case <-clientDone:
					case <-ctx.Done():
					}
					return nil
				}()
			}))
			defer listener.Close()

			header := http.Header{}
			if c.offer {
				header.Add("Sec-WebSocket-Protocol", connect.TransportDelayH1Subprotocol)
			}
			if c.provideIntent {
				header.Set(HeaderProvideIntent, "1")
			}
			address := "ws" + listener.URL[len("http"):] + "/"
			raw, err := connect.DialFramedUpgrade(ctx, address, header, &websocket.Dialer{HandshakeTimeout: 5 * time.Second}, connect.H1FramerProtocol)
			if err != nil {
				t.Fatalf("dial: %s", err)
			}
			// the legacy reader: a client from before the header frames each
			// message this way and strips nothing
			framed, err := connect.NewFramedMessageConn(raw, connect.H1FramerProtocol, 65535, nil)
			if err != nil {
				raw.Close()
				t.Fatalf("framed conn: %s", err)
			}
			defer framed.Close()

			subprotocol := framed.Subprotocol()
			if c.delayed && subprotocol != connect.TransportDelayH1Subprotocol {
				t.Fatalf("the 101 selected %q, expected the delay header", subprotocol)
			}
			if !c.delayed && subprotocol != "" {
				t.Fatalf("the 101 selected %q, expected none", subprotocol)
			}
			if err := framed.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			for index, payload := range payloads {
				_, message, err := framed.ReadMessage()
				if err != nil {
					t.Fatalf("read %d: %s", index, err)
				}
				if c.delayed {
					if len(message) != 12+len(payload) || !bytes.Equal(message[12:], payload) {
						t.Fatalf("message %d of %d bytes, expected the 12-byte header and %d bytes", index, len(message), len(payload))
					}
					continue
				}
				if !bytes.Equal(message, payload) {
					t.Fatalf("message %d of %d bytes, expected exactly the %d-byte frame", index, len(message), len(payload))
				}
			}
			close(clientDone)
			if err := <-serverDone; err != nil {
				t.Fatalf("server: %s", err)
			}
		})
	}
}
