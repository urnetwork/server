// Real H1 reader regressions hold an announcement's measurement transaction
// while subsequent application frames cross the production resident path.
package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"google.golang.org/protobuf/proto"
)

// A matching latency reply cannot park the production WebSocket reader in
// telemetry storage and prevent the next ordinary Transfer frame from arriving.
func TestConnectH1LatencyMeasurementDoesNotBlockReader(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testConnectH1MeasurementDoesNotBlockReader(t, false)
	})
}

// The speed-stop branch has the same shared reader and must preserve ordinary
// traffic even while its accepted measurement waits inside a transaction.
func TestConnectH1SpeedMeasurementDoesNotBlockReader(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		testConnectH1MeasurementDoesNotBlockReader(t, true)
	})
}

// Owns one read-only share obtained inside the resident client's borrowed
// receive callback; the test returns it only after the real owners join.
type connectH1MeasurementDeliveryForTest struct {
	source       clientconnect.TransferPath
	messageBytes []byte
	pooled       bool
}

// Uses the existing authenticated HTTP handler/exchange fixture, with only
// announcement and resident observers. All reads, framing, queue admission,
// Transfer decoding, and application callbacks are production implementations.
func testConnectH1MeasurementDoesNotBlockReader(t testing.TB, speed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	registered := make(chan *ConnectionAnnounce, 1)
	writeEntered := make(chan struct{})
	writeRelease := make(chan struct{})
	workersJoining := make(chan struct{})
	releaseWrite := sync.OnceFunc(func() { close(writeRelease) })
	var writeOnce sync.Once
	var joinOnce sync.Once
	latencyTestId := server.NewId()
	const speedTestId uint32 = 19
	var warmupByteCount ByteCount
	configured := make(chan struct{})
	configuredClosed := false

	deliveries := make(chan connectH1MeasurementDeliveryForTest, 4)
	var retainedMessages [][]byte
	var env *peerDiscoveryEnv
	var ws *websocket.Conn
	defer func() {
		// Every failure path frees the transaction before joining the real
		// handler and exchange, then reconciles each borrowed payload share.
		releaseWrite()
		if !configuredClosed {
			close(configured)
		}
		cancel()
		if ws != nil {
			ws.Close()
		}
		if env != nil {
			env.Close()
		}
		for {
			select {
			case delivery := <-deliveries:
				retainedMessages = append(retainedMessages, delivery.messageBytes)
			default:
				for _, message := range retainedMessages {
					if !clientconnect.MessagePoolReturn(message) {
						t.Error("H1 delivery retained another pooled owner after handler and resident teardown")
					}
				}
				return
			}
		}
	}()

	env = testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
		settings.EnableNetworkPeers = false
	}, func(settings *ConnectHandlerSettings) {
		settings.EnableH1Plus = false
		settings.ConnectionAnnounceTimeout = 0
		settings.ConnectionTestConfig = V0TestConfig()
		settings.ConnectionAnnounceSettings.EnableNetworkPeers = false
		settings.ConnectionAnnounceSettings.LocationRetryTimeout = 0
		settings.ConnectionAnnounceSettings.MaxLatencyCount = 1
		settings.ConnectionAnnounceSettings.SyncConnectionTimeout = time.Hour
		settings.ConnectionAnnounceSettings.PassiveSpeedWindowDuration = time.Hour
		settings.ConnectionAnnounceSettings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
			<-configured
			announce.beforeMeasurementWriteForTest = func() {
				writeOnce.Do(func() {
					close(writeEntered)
					// Keep the transaction held through cancellation as well:
					// the owner must join this exact publisher before done.
					<-writeRelease
				})
			}
			announce.beforeWorkersWaitForTest = func() {
				joinOnce.Do(func() { close(workersJoining) })
			}
			func() {
				announce.stateLock.Lock()
				defer announce.stateLock.Unlock()
				if speed {
					announce.speedTest = &SpeedTest{
						TestId:         speedTestId,
						TotalByteCount: warmupByteCount,
					}
					announce.speedTestSendTime = time.Now().Add(-time.Second)
				} else {
					announce.latencyTest = &LatencyTest{TestId: latencyTestId}
					announce.latencyTestSendTime = time.Now().Add(-time.Second)
				}
			}()
			select {
			case registered <- announce:
			case <-ctx.Done():
			}
		}
	})
	clientId, token := env.authClient(&model.AuthNetworkClientArgs{
		Description: "synthetic H1 measurement reader",
		DeviceSpec:  "synthetic",
	})
	env.exchange.beforeResidentProfileForTest = func(resident *Resident) {
		resident.client.AddReceiveCallback(func(source clientconnect.TransferPath, frames []*protocol.Frame, _ clientconnect.Peer) {
			for _, frame := range frames {
				if frame.MessageType != protocol.MessageType_TestSimpleMessage {
					continue
				}
				pooled, _ := clientconnect.MessagePoolCheck(frame.MessageBytes)
				delivery := connectH1MeasurementDeliveryForTest{
					source:       source,
					messageBytes: clientconnect.MessagePoolShareReadOnly(frame.MessageBytes),
					pooled:       pooled,
				}
				select {
				case deliveries <- delivery:
				default:
					clientconnect.MessagePoolReturn(delivery.messageBytes)
				}
			}
		})
	}

	sequenceId := clientconnect.NewId()
	encode := func(sequenceNumber uint64, content string) (wireBytes []byte, messageBytes []byte) {
		t.Helper()
		messageBytes, err := proto.Marshal(&protocol.SimpleMessage{Content: content})
		if err != nil {
			t.Fatal(err)
		}
		wireBytes, err = proto.Marshal(&protocol.TransferFrame{
			TransferPath: clientconnect.NewTransferPath(
				clientconnect.Id(clientId), clientconnect.ControlId, clientconnect.Id{},
			).ToProtobuf(),
			Pack: &protocol.Pack{
				MessageId:      clientconnect.NewId().Bytes(),
				SequenceId:     sequenceId.Bytes(),
				SequenceNumber: sequenceNumber,
				Head:           true,
				Frames: []*protocol.Frame{{
					MessageType:  protocol.MessageType_TestSimpleMessage,
					MessageBytes: messageBytes,
				}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return wireBytes, messageBytes
	}
	warmupWire, warmupMessage := encode(0, "synthetic H1 reader warmup")
	followingWire, followingMessage := encode(1, "synthetic H1 payload after held measurement")
	warmupByteCount = ByteCount(len(warmupWire))
	close(configured)
	configuredClosed = true

	requestHeaders := http.Header{}
	requestHeaders.Set("Authorization", "Bearer "+token)
	requestHeaders.Set("X-UR-InstanceId", server.NewId().String())
	requestHeaders.Set("X-UR-TransportVersion", "2")
	dialer := &websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	var err error
	ws, _, err = dialer.DialContext(ctx, fmt.Sprintf("ws://127.0.0.1:%d/", env.port), requestHeaders)
	if err != nil {
		t.Fatalf("production H1 WebSocket admission: %v", err)
	}
	var announce *ConnectionAnnounce
	select {
	case announce = <-registered:
	case <-ctx.Done():
		t.Fatal("production H1 announcement did not finish registration")
	}
	if announce.ConnectionId() == nil {
		t.Fatal("measurement observer ran before the real connection was registered")
	}

	write := func(message []byte) {
		t.Helper()
		deadline, _ := ctx.Deadline()
		if err := ws.SetWriteDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		if err := ws.WriteMessage(websocket.BinaryMessage, message); err != nil {
			t.Fatalf("write real H1 frame: %v", err)
		}
	}
	waitDelivery := func(want []byte) {
		t.Helper()
		select {
		case delivery := <-deliveries:
			retainedMessages = append(retainedMessages, delivery.messageBytes)
			if delivery.source.SourceId != clientconnect.Id(clientId) || !bytes.Equal(delivery.messageBytes, want) {
				t.Fatal("production resident received the wrong source or ordinary payload")
			}
			if !delivery.pooled {
				t.Fatal("production receive callback did not borrow a live pooled payload")
			}
			if pooled, shared := clientconnect.MessagePoolCheck(delivery.messageBytes); !pooled || !shared {
				t.Fatal("test did not retain its owned read-only payload share")
			}
		case <-ctx.Done():
			t.Fatal("ordinary H1 payload did not cross the production reader and resident while measurement write was held")
		}
	}

	if speed {
		start := []byte{clientconnect.TransportControlSpeedStart, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(start[1:], speedTestId)
		write(start)
	}
	write(warmupWire)
	waitDelivery(warmupMessage)
	if speed {
		stop := []byte{clientconnect.TransportControlSpeedStop, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(stop[1:], speedTestId)
		write(stop)
	} else {
		write(latencyTestId.Bytes())
	}
	select {
	case <-writeEntered:
	case <-ctx.Done():
		t.Fatal("real H1 telemetry reply did not reach the measurement transaction")
	}

	// The barrier proves the telemetry write is held before this later frame
	// enters the socket. Success must come from the same production reader.
	write(followingWire)
	waitDelivery(followingMessage)
	select {
	case <-writeRelease:
		t.Fatal("ordinary payload was checked only after releasing measurement storage")
	default:
	}
	receiveMessageCount, receiveByteCount := func() (uint64, ByteCount) {
		announce.stateLock.Lock()
		defer announce.stateLock.Unlock()
		return announce.receiveMessageCount, announce.receiveByteCount
	}()
	wantMessages := uint64(3)
	wantBytes := ByteCount(len(warmupWire) + 16 + len(followingWire))
	if speed {
		wantMessages = 4
		wantBytes = ByteCount(5 + len(warmupWire) + 5 + len(followingWire))
	}
	if receiveMessageCount != wantMessages || receiveByteCount != wantBytes {
		t.Fatalf("H1 packet accounting while transaction held = (%d, %d), want (%d, %d)", receiveMessageCount, receiveByteCount, wantMessages, wantBytes)
	}

	announce.Close()
	select {
	case <-workersJoining:
	case <-ctx.Done():
		t.Fatal("announcement cancellation did not enter its child-worker join")
	}
	select {
	case <-announce.done:
		t.Fatal("announcement completed before its held measurement publisher joined")
	default:
	}
	releaseWrite()
	select {
	case <-announce.done:
	case <-ctx.Done():
		t.Fatal("announcement did not finish after its measurement publisher was released")
	}
}
