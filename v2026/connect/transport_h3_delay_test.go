// These tests pin the edge's half of the H3 delay header (connect DELAY.md
// §6, §11) without database-backed transport integration: the auth response
// that accepts it, the stream helpers that stamp and strip it, and the
// DATAGRAM frame limit the resident sees.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A pooled auth frame as a client of the given capabilities sends it.
func testConnectH3DelayAuthFrame(
	t *testing.T,
	offerDatagrams bool,
	offerTransportDelay bool,
	provideIntent bool,
) (*protocol.Auth, []byte) {
	t.Helper()
	auth := &protocol.Auth{
		ByJwt:              "synthetic-h3-delay-credential",
		AppVersion:         "synthetic-version",
		ClientInfo:         `{"v":1}`,
		StreamLeaseVersion: 1,
		InstanceId:         []byte{1, 2, 3, 4},
		ProvideIntent:      provideIntent,
	}
	clientconnect.SetH3DatagramAuthOffer(auth, offerDatagrams)
	if offerTransportDelay {
		// set directly, so a provide intent request can carry it too
		auth.TransportDelayVersion = clientconnect.TransportDelayVersion
	}
	authFrameBytes, err := clientconnect.EncodeFrame(auth, clientconnect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clientconnect.MessagePoolReturn(authFrameBytes)
	})
	return auth, authFrameBytes
}

// A response that accepts neither DATAGRAM nor the delay header stays the raw
// echo byte for byte, which old clients require. Accepting the delay header
// encodes the response even with DATAGRAM off, since the raw echo would leave
// the accepted field 0; the server setting and provide intent each refuse it.
func TestConnectQuicAuthResponseAcceptsTransportDelayByEncoding(t *testing.T) {
	for _, c := range []struct {
		name                 string
		offerDatagrams       bool
		offerTransportDelay  bool
		provideIntent        bool
		acceptTransportDelay bool
		wantRawEcho          bool
		wantDatagrams        bool
		wantTransportDelay   bool
	}{
		{name: "old client", acceptTransportDelay: true, wantRawEcho: true},
		{name: "new client", offerTransportDelay: true, acceptTransportDelay: true, wantTransportDelay: true},
		{name: "server setting off", offerTransportDelay: true, wantRawEcho: true},
		{name: "provide intent", offerTransportDelay: true, provideIntent: true, acceptTransportDelay: true, wantRawEcho: true},
		{name: "old hybrid client", offerDatagrams: true, acceptTransportDelay: true, wantDatagrams: true},
		{name: "new hybrid client", offerDatagrams: true, offerTransportDelay: true, acceptTransportDelay: true, wantDatagrams: true, wantTransportDelay: true},
	} {
		auth, authFrameBytes := testConnectH3DelayAuthFrame(t, c.offerDatagrams, c.offerTransportDelay, c.provideIntent)
		var written []byte
		useH3Datagrams, useTransportDelay, err := writeConnectQuicAuthResponse(
			auth,
			authFrameBytes,
			true,
			true,
			true,
			c.acceptTransportDelay,
			func(responseFrameBytes []byte) error {
				written = bytes.Clone(responseFrameBytes)
				return nil
			},
		)
		if err != nil || useH3Datagrams != c.wantDatagrams || useTransportDelay != c.wantTransportDelay {
			t.Fatalf("%s negotiated = (DATAGRAM %t, delay %t, %v)", c.name, useH3Datagrams, useTransportDelay, err)
		}
		if rawEcho := bytes.Equal(written, authFrameBytes); rawEcho != c.wantRawEcho {
			t.Fatalf("%s raw echo=%t want %t", c.name, rawEcho, c.wantRawEcho)
		}
		if c.wantRawEcho {
			continue
		}
		decoded, err := clientconnect.DecodeFrame(written)
		if err != nil {
			t.Fatal(err)
		}
		response, ok := decoded.(*protocol.Auth)
		if !ok {
			t.Fatalf("%s response type=%T", c.name, decoded)
		}
		wantAcceptedVersion := uint32(0)
		if c.wantTransportDelay {
			wantAcceptedVersion = clientconnect.TransportDelayVersion
		}
		if response.TransportDelayAcceptedVersion != wantAcceptedVersion ||
			(response.H3DatagramAcceptedVersion != 0) != c.wantDatagrams {
			t.Fatalf("%s response accepted delay=%d DATAGRAM=%d", c.name, response.TransportDelayAcceptedVersion, response.H3DatagramAcceptedVersion)
		}
		if accepted, err := clientconnect.ValidateH3DatagramAuthResponse(auth, response, c.offerDatagrams, true, true); accepted != c.wantDatagrams || err != nil {
			t.Fatalf("%s client validation = (%t, %v)", c.name, accepted, err)
		}
	}
}

// The stream helpers frame through the connection's delay state like a
// Framer: a batch is one physical write whose frames all carry the header, a
// heartbeat is stamped, and a read strips the header. A legacy framer would
// read the header as payload, which is why only a negotiated connection
// switches.
func TestConnectQuicStreamHelpersCarryTransportDelayHeader(t *testing.T) {
	framerSettings := clientconnect.DefaultFramerSettings(1024)
	edge := clientconnect.NewTransportDelayH3Peer(clientconnect.NewId(), framerSettings)
	client := clientconnect.NewTransportDelayH3Peer(clientconnect.NewId(), framerSettings)
	headerByteCount := edge.HeaderByteCount()
	if headerByteCount != 12 || (*clientconnect.TransportDelayH3)(nil).HeaderByteCount() != 0 {
		t.Fatalf("header byte count=%d", headerByteCount)
	}
	readClient := func(stream *transportDeadlineStream, want []byte) {
		t.Helper()
		message, err := client.Read(&stream.buffer)
		if err != nil {
			t.Fatal(err)
		}
		defer clientconnect.MessagePoolReturn(message)
		if !bytes.Equal(message, want) {
			t.Fatalf("client read %q want %q", message, want)
		}
	}

	stream := &transportDeadlineStream{}
	wants := [][]byte{[]byte("first-downstream"), []byte("second-downstream")}
	messages := [][]byte{clientconnect.MessagePoolCopy(wants[0]), clientconnect.MessagePoolCopy(wants[1])}
	recorded := 0
	err := writeConnectQuicBatchWithDeadline(edge, stream, time.Second, messages, make([]byte, 1024), returnConnectDeadlineMessage, func([]byte) { recorded++ })
	wantByteCount := 2*(4+headerByteCount) + len(wants[0]) + len(wants[1])
	if err != nil || recorded != 2 || stream.writes != 1 || stream.buffer.Len() != wantByteCount {
		t.Fatalf("batch err=%v recorded=%d writes=%d bytes=%d want %d", err, recorded, stream.writes, stream.buffer.Len(), wantByteCount)
	}
	for _, want := range wants {
		readClient(stream, want)
	}

	if err := writeConnectQuicHeartbeatWithDeadline(edge, stream, time.Second, nil); err != nil {
		t.Fatal(err)
	}
	if stream.buffer.Len() != 4+headerByteCount {
		t.Fatalf("heartbeat bytes=%d", stream.buffer.Len())
	}
	readClient(stream, []byte{})

	upstream := []byte("upstream")
	for _, legacy := range []bool{false, true} {
		if err := client.WriteBatchWithStorage(&stream.buffer, [][]byte{upstream}, make([]byte, 64)); err != nil {
			t.Fatal(err)
		}
		var framing connectQuicStreamFraming = edge
		wantByteCount := len(upstream)
		if legacy {
			framing = testConnectDeadlineFramer()
			wantByteCount += headerByteCount
		}
		frame, err := readConnectQuicFrameWithDeadline(framing, stream, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		matches := len(frame) == wantByteCount && bytes.HasSuffix(frame, upstream)
		clientconnect.MessagePoolReturn(frame)
		if !matches {
			t.Fatalf("legacy=%t edge read %d bytes, want %d", legacy, len(frame), wantByteCount)
		}
	}
}

// A negotiated connection's resident sees a DATAGRAM frame limit smaller by the
// header, which agrees with the live lane predicate at every size.
func TestConnectH3TransferCarrierSubtractsTransportDelayHeader(t *testing.T) {
	settings := clientconnect.DefaultH3DatagramSettings()
	headerByteCount := clientconnect.NewTransportDelayH3Peer(clientconnect.NewId(), clientconnect.DefaultFramerSettings(1024)).HeaderByteCount()
	for _, maxDatagramByteCount := range []int{clientconnect.H3InitialDatagramByteCount, settings.TargetDatagramByteCount} {
		legacy := connectH3TransferCarrierProperties(true, settings, maxDatagramByteCount, 0)
		delayed := connectH3TransferCarrierProperties(true, settings, maxDatagramByteCount, headerByteCount)
		if delayed.UnreliableMaxMessageByteCount != legacy.UnreliableMaxMessageByteCount-headerByteCount ||
			!delayed.Unreliable || !delayed.UnreliableFlowIsolation || !delayed.UnreliableFlowReserve {
			t.Fatalf("path=%d delayed properties=%+v legacy=%+v", maxDatagramByteCount, delayed, legacy)
		}
		for byteCount := 1; byteCount <= settings.MaxMessageByteCount; byteCount += 1 {
			if (byteCount <= delayed.UnreliableMaxMessageByteCount) !=
				settings.UseDatagramForPathWithHeader(byteCount, maxDatagramByteCount, headerByteCount) {
				t.Fatalf("path=%d bytes=%d resident limit disagrees with the live lane", maxDatagramByteCount, byteCount)
			}
		}
	}
	if properties := connectH3TransferCarrierProperties(false, settings, 0, headerByteCount); properties.Unreliable ||
		properties.UnreliableMaxMessageByteCount != 0 {
		t.Fatalf("stream-only properties=%+v", properties)
	}
}

// Two network peers reach a real edge over H3, one offering the delay header
// and one not. Messages each way prove the edge strips and stamps the header
// on both lanes of the negotiated connection, since a missing or extra header
// corrupts every Transfer frame, while the other connection stays on the
// legacy wire on the same edge. Only the offering client publishes intervals,
// with delay samples in both directions from the edge's stamps and echoes.
func TestConnectH3TransportDelayEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("requires local Postgres and Redis")
	}
	environment := server.DefaultTestEnv()
	environment.RerunCount = 0
	environment.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		env := testing_newPeerDiscoveryEnv(ctx, t)
		defer env.Close()
		// clients and transports close before the env, as in the other fixtures
		var closers []func()
		defer func() {
			for i := len(closers) - 1; 0 <= i; i -= 1 {
				closers[i]()
			}
		}()

		type peer struct {
			clientId server.Id
			client   *clientconnect.Client
			stats    chan clientconnect.TransportDelayStats
		}
		newPeer := func(transportDelay *clientconnect.TransportDelaySettings) *peer {
			clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{Description: "transport delay", DeviceSpec: "test"})
			client := env.newClient(clientId)
			closers = append(closers, client.Close)
			stats := make(chan clientconnect.TransportDelayStats, 1024)
			closers = append(closers, client.AddTransportDelayStatsCallback(func(interval *clientconnect.TransportDelayStats) {
				select {
				case stats <- *interval:
				default:
				}
			}))
			settings := clientconnect.DefaultPlatformTransportSettings()
			settings.QuicTlsConfig.InsecureSkipVerify = true
			settings.H3Port = env.h3Port
			settings.DnsPort = 0
			settings.TransportDelay = transportDelay
			transport := clientconnect.NewPlatformTransportWithTargetMode(
				ctx,
				clientconnect.NewClientStrategyWithDefaults(ctx),
				client.RouteManager(),
				fmt.Sprintf("ws://127.0.0.1:%d", env.port),
				&clientconnect.ClientAuth{ByJwt: byClientJwt, InstanceId: clientconnect.NewId(), AppVersion: "0.0.0"},
				clientconnect.TransportModeH3,
				settings,
			)
			closers = append(closers, transport.Close)
			return &peer{clientId: clientId, client: client, stats: stats}
		}
		transportDelay := clientconnect.DefaultTransportDelaySettings()
		transportDelay.Interval = 100 * time.Millisecond
		transportDelay.MinDelaySampleCount = 1
		offering := newPeer(transportDelay)
		legacy := newPeer(nil)
		for _, p := range []*peer{offering, legacy} {
			env.setProvideModes(p.client, map[protocol.ProvideMode]bool{protocol.ProvideMode_Network: true})
		}
		for _, c := range []struct {
			from *peer
			to   *peer
		}{
			{from: offering, to: legacy},
			{from: legacy, to: offering},
		} {
			received := recordPeerReceives(c.to.client, c.from.clientId)
			sendSimpleMessage(t, c.from.client, c.to.clientId)
			select {
			case <-received:
			case <-ctx.Done():
				t.Fatalf("message %s -> %s did not arrive", c.from.clientId, c.to.clientId)
			}
		}

		for sampled := false; !sampled; {
			select {
			case stats := <-offering.stats:
				sampled = stats.TransportType == clientconnect.TransportTypeH3 &&
					0 < stats.SendByteCount && 0 < stats.ReceiveByteCount &&
					stats.SendQueueDelayValid && stats.ReceiveQueueDelayValid
			case <-ctx.Done():
				t.Fatal("the offering client published no interval with delay samples in both directions")
			}
		}
		select {
		case stats := <-legacy.stats:
			t.Fatalf("the legacy client published %+v", stats)
		default:
		}
	})
}
