package perfvar

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	clientconnect "github.com/urnetwork/connect"
)

// Real gVisor TCP, TLS 1.3, HTTP upgrade and production H1+ framing distinguish
// accepted application writes from bytes delivered through TCP recovery. The
// controlled loss is a diagnostic positive control, not historical root proof.
func TestH1BurstLossBoundaryUsesTCPRecoveryNotUnflushedFraming(t *testing.T) {
	for _, losses := range []int{0, 6, 10} {
		t.Run(fmt.Sprintf("payload_losses_%d", losses), func(t *testing.T) { testH1BurstLossBoundary(t, losses) })
	}
}

func testH1BurstLossBoundary(t *testing.T, losses int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	profile := initialNetworkProfiles(20260810)["clean-lan"]
	path, err := newTunPath(ctx, profile, mobileTunResourceProfile())
	if err != nil {
		t.Fatal(err)
	}
	defer path.close()
	serverTLS, clientTLS, err := newWorkloadTlsConfigs()
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.NextProtos, clientTLS.NextProtos = []string{"http/1.1"}, []string{"http/1.1"}
	listener, err := path.right.ListenTCP(&net.TCPAddr{IP: path.endpointAddress(false)})
	if err != nil {
		t.Fatal(err)
	}
	type acceptedPeer struct {
		connection *clientconnect.FramedMessageConn
		err        error
	}
	accepted := make(chan acceptedPeer, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// This hermetic test listener admits its one synthetic peer only.
		connection, err := clientconnect.AcceptFramedUpgrade(w, r, clientconnect.H1FramerProtocol, 5*time.Second)
		if err != nil {
			accepted <- acceptedPeer{err: err}
			return
		}
		framed, err := clientconnect.NewFramedMessageConn(connection, clientconnect.H1FramerProtocol, 16*1024, nil)
		if err != nil {
			connection.Close()
		}
		accepted <- acceptedPeer{connection: framed, err: err}
	})}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = server.Serve(tls.NewListener(listener, serverTLS)) }()
	defer func() { server.Close(); listener.Close(); <-serverDone }()
	var raw atomic.Pointer[clientconnect.TunTcpConn]
	dialer := &websocket.Dialer{
		TLSClientConfig:  clientTLS,
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := path.left.DialContext(ctx, network, address)
			if err == nil {
				tcp, ok := connection.(*clientconnect.TunTcpConn)
				if !ok {
					connection.Close()
					return nil, fmt.Errorf("unexpected TUN connection %T", connection)
				}
				raw.Store(tcp)
			}
			return connection, err
		},
	}
	upgraded, err := clientconnect.DialFramedUpgrade(ctx, "wss://"+listener.Addr().String(), nil, dialer, clientconnect.H1FramerProtocol)
	if err != nil {
		t.Fatal(err)
	}
	stats := &clientconnect.H1PlusStats{}
	client, err := clientconnect.NewFramedMessageConn(upgraded, clientconnect.H1FramerProtocol, 16*1024, stats)
	if err != nil {
		upgraded.Close()
		t.Fatal(err)
	}
	defer client.Close()
	var peer *clientconnect.FramedMessageConn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		peer = result.connection
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer peer.Close()
	deadline, _ := ctx.Deadline()
	client.SetDeadline(deadline)
	peer.SetDeadline(deadline)
	readExact := func(connection *clientconnect.FramedMessageConn, want []byte) error {
		kind, message, err := clientconnect.ReadH1PooledMessage(connection, 16*1024)
		if err != nil {
			return err
		}
		defer clientconnect.MessagePoolReturn(message)
		if kind != websocket.BinaryMessage || !bytes.Equal(message, want) {
			return fmt.Errorf("H1+ frame content changed")
		}
		return nil
	}
	warm := []byte("warm production H1+ framing")
	if err := client.WriteMessage(websocket.BinaryMessage, warm); err != nil {
		t.Fatal(err)
	}
	if err := readExact(peer, warm); err != nil {
		t.Fatal(err)
	}
	if err := peer.WriteMessage(websocket.BinaryMessage, warm); err != nil {
		t.Fatal(err)
	}
	if err := readExact(client, warm); err != nil {
		t.Fatal(err)
	}
	if !path.waitForTerminalIdle(ctx) {
		t.Fatal("warm network did not drain")
	}

	var payloadLosses atomic.Int64
	burstReached := make(chan struct{})
	var reachedOnce sync.Once
	if losses > 0 {
		path.forwardLink.setAfterPacketScheduledForTest(func(observation linkScheduleObservation) {
			// TCP ACK-only packets are at most 80 bytes; the one encrypted
			// H1+ data segment is larger. Ignore delayed warm-up TCP ACKs.
			if observation.terminalDropCause == linkTerminalDropLoss && observation.packetByteCount > 80 && payloadLosses.Add(1) >= int64(losses) {
				reachedOnce.Do(func() { close(burstReached) })
			}
		})
		defer path.forwardLink.setAfterPacketScheduledForTest(nil)
		burst := profile.Forward
		burst.LossModel, burst.LossProbability = lossModelIndependent, 1
		if _, err := path.forwardLink.updateProfile(burst, "controlled-H1-payload-burst", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	frames := [][]byte{bytes.Repeat([]byte{0x5a}, 256), nil, bytes.Repeat([]byte{0x71}, 128)}
	received := make(chan error, 1)
	go func() {
		for _, frame := range frames {
			if err := readExact(peer, frame); err != nil {
				received <- err
				return
			}
		}
		received <- nil
	}()
	// Close and join the actual reader even when an assertion fails.
	readerJoined := false
	defer func() {
		if !readerJoined {
			peer.Close()
			<-received
		}
	}()
	beforeTimeouts := path.left.Stats().TCP.Timeouts.Value()
	beforeRetransmits := path.left.Stats().TCP.Retransmits.Value()
	start := time.Now()
	if err := client.WriteMessages(frames); err != nil {
		t.Fatal(err)
	}
	writeDuration := time.Since(start)
	if writeDuration >= 2*time.Second {
		t.Fatalf("small H1+ write did not return after TCP admission: %s", writeDuration)
	}
	if losses > 0 {
		select {
		case <-burstReached:
		case err := <-received:
			readerJoined = true
			t.Fatalf("H1 frame arrived during forced loss: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		info, err := raw.Load().TcpInfo()
		if err != nil {
			t.Fatal(err)
		}
		timeouts := path.left.Stats().TCP.Timeouts.Value() - beforeTimeouts
		retransmits := path.left.Stats().TCP.Retransmits.Value() - beforeRetransmits
		if timeouts == 0 || retransmits < uint64(losses-1) || info.SndCwnd != 1 || info.RTO < time.Second {
			t.Fatalf("forced loss lacks TCP recovery evidence: info=%+v timeouts=%d retransmits=%d", info, timeouts, retransmits)
		}
		if losses == 10 && info.RTO != 8*time.Second {
			t.Fatalf("long-burst control did not reach the production RTO cap: %+v", info)
		}
		t.Logf("H1+ accepted write=%s; TCP delivery blocked=%s payload_losses=%d RTO=%s cwnd=%d timeouts=%d retransmits=%d", writeDuration, time.Since(start), payloadLosses.Load(), info.RTO, info.SndCwnd, timeouts, retransmits)
		if _, err := path.forwardLink.updateProfile(profile.Forward, "controlled-H1-burst-restored", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case err := <-received:
		readerJoined = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// No extra write or flush is allowed to release the retained encrypted
	// frames. TCP's retransmission after restoration alone must deliver them.
	if stats.Snapshot().Messages != 4 {
		t.Fatalf("unexpected framing writes: %+v", stats.Snapshot())
	}
	if losses == 10 && time.Since(start) <= 30*time.Second {
		t.Fatal("long-burst control did not cross the unchanged logical ACK lifetime")
	}
	t.Logf("all H1+ frames delivered without another application write after %s", time.Since(start))
}
