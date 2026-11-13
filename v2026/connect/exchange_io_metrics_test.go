package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	connectlib "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

type exchangeWriteBudgetConn struct {
	remaining int
	err       error
}

func (c *exchangeWriteBudgetConn) Read([]byte) (int, error) {
	return 0, net.ErrClosed
}

func (c *exchangeWriteBudgetConn) Write(bytes []byte) (int, error) {
	if c.remaining == 0 {
		return 0, c.err
	}
	if len(bytes) <= c.remaining {
		c.remaining -= len(bytes)
		return len(bytes), nil
	}
	written := c.remaining
	c.remaining = 0
	return written, c.err
}

func (c *exchangeWriteBudgetConn) Close() error                     { return nil }
func (c *exchangeWriteBudgetConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *exchangeWriteBudgetConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *exchangeWriteBudgetConn) SetDeadline(time.Time) error      { return nil }
func (c *exchangeWriteBudgetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *exchangeWriteBudgetConn) SetWriteDeadline(time.Time) error { return nil }

type exchangeIOMetricSnapshot struct {
	frames float64
	bytes  float64
}

// Sums the series of every exchange operation.
func snapshotExchangeIO(direction string, kind string) exchangeIOMetricSnapshot {
	var snapshot exchangeIOMetricSnapshot
	for _, op := range exchangeIOOpLabels {
		opSnapshot := snapshotExchangeIOOp(op, direction, kind)
		snapshot.frames += opSnapshot.frames
		snapshot.bytes += opSnapshot.bytes
	}
	return snapshot
}

func snapshotExchangeIOOp(op string, direction string, kind string) exchangeIOMetricSnapshot {
	return exchangeIOMetricSnapshot{
		frames: testutil.ToFloat64(exchangeIOFramesCounter.WithLabelValues(op, direction, kind)),
		bytes:  testutil.ToFloat64(exchangeIOBytesCounter.WithLabelValues(op, direction, kind)),
	}
}

func requireExchangeIODelta(
	t testing.TB,
	direction string,
	kind string,
	before exchangeIOMetricSnapshot,
	wantFrames float64,
	wantBytes float64,
) {
	t.Helper()
	after := snapshotExchangeIO(direction, kind)
	if delta := after.frames - before.frames; delta != wantFrames {
		t.Errorf("%s %s frame delta = %v, want %v", direction, kind, delta, wantFrames)
	}
	if delta := after.bytes - before.bytes; delta != wantBytes {
		t.Errorf("%s %s byte delta = %v, want %v", direction, kind, delta, wantBytes)
	}
}

func TestExchangeBufferRecordsCompletedFramesOnce(t *testing.T) {
	settings := DefaultExchangeSettings()
	sender := NewDefaultExchangeBuffer(settings)
	receiver := NewReceiveOnlyExchangeBuffer(settings)
	senderConn, receiverConn := net.Pipe()
	t.Cleanup(func() {
		senderConn.Close()
		receiverConn.Close()
	})

	t.Run("handshake", func(t *testing.T) {
		sentBefore := snapshotExchangeIO("sent", "handshake")
		receivedBefore := snapshotExchangeIO("received", "handshake")
		header := &ExchangeHeader{
			Version:    1,
			ClientId:   server.NewId(),
			ResidentId: server.NewId(),
			Op:         ExchangeOpTransport,
		}
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- sender.WriteHeader(context.Background(), senderConn, header)
		}()
		if _, err := receiver.ReadHeader(context.Background(), receiverConn); err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}

		sentAfter := snapshotExchangeIO("sent", "handshake")
		sentBytes := sentAfter.bytes - sentBefore.bytes
		if sentBytes <= exchangeIOFrameHeaderByteCount {
			t.Fatalf("handshake wire bytes = %v, want a framed payload", sentBytes)
		}
		requireExchangeIODelta(t, "sent", "handshake", sentBefore, 1, sentBytes)
		requireExchangeIODelta(t, "received", "handshake", receivedBefore, 1, sentBytes)
	})

	t.Run("batched data", func(t *testing.T) {
		messageLengths := []int{3, 17, 513}
		batch := make([][]byte, 0, len(messageLengths))
		wantBytes := 0
		for messageIndex, messageLength := range messageLengths {
			message := connectlib.MessagePoolGet(messageLength)
			for byteIndex := range message {
				message[byteIndex] = byte(messageIndex + 1)
			}
			batch = append(batch, message)
			wantBytes += exchangeIOFrameHeaderByteCount + messageLength
		}
		sentBefore := snapshotExchangeIO("sent", "data")
		receivedBefore := snapshotExchangeIO("received", "data")
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- sender.WriteMessages(senderConn, batch)
		}()
		for messageIndex, messageLength := range messageLengths {
			message, err := receiver.ReadMessage(receiverConn)
			if err != nil {
				t.Fatal(err)
			}
			if len(message) != messageLength || message[0] != byte(messageIndex+1) {
				t.Errorf("message %d = len %d prefix %d, want len %d prefix %d", messageIndex, len(message), message[0], messageLength, messageIndex+1)
			}
			connectlib.MessagePoolReturn(message)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		requireExchangeIODelta(t, "sent", "data", sentBefore, float64(len(messageLengths)), float64(wantBytes))
		requireExchangeIODelta(t, "received", "data", receivedBefore, float64(len(messageLengths)), float64(wantBytes))
	})

	t.Run("ping", func(t *testing.T) {
		sentBefore := snapshotExchangeIO("sent", "ping")
		receivedBefore := snapshotExchangeIO("received", "ping")
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- sender.WriteMessage(senderConn, connectlib.MessagePoolGet(0))
		}()
		message, err := receiver.ReadMessage(receiverConn)
		if err != nil {
			t.Fatal(err)
		}
		connectlib.MessagePoolReturn(message)
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		requireExchangeIODelta(t, "sent", "ping", sentBefore, 1, exchangeIOFrameHeaderByteCount)
		requireExchangeIODelta(t, "received", "ping", receivedBefore, 1, exchangeIOFrameHeaderByteCount)
	})
}

func TestExchangeBufferDoesNotRecordFailedFrame(t *testing.T) {
	settings := DefaultExchangeSettings()
	buffer := NewDefaultExchangeBuffer(settings)
	localConn, peerConn := net.Pipe()
	peerConn.Close()
	defer localConn.Close()

	before := snapshotExchangeIO("sent", "data")
	message := connectlib.MessagePoolGet(32)
	if err := buffer.WriteMessage(localConn, message); err == nil {
		t.Fatal("write to a closed exchange peer unexpectedly succeeded")
	}
	requireExchangeIODelta(t, "sent", "data", before, 0, 0)
}

// A partial middle frame cannot credit a smaller later frame, and every
// accepted input owner is returned even after accounting stops.
func TestExchangeBufferRecordsOnlyCompletedBatchPrefix(t *testing.T) {
	settings := DefaultExchangeSettings()
	buffer := NewDefaultExchangeBuffer(settings)
	messageLengths := []int{23, 41, 7}
	batch := make([][]byte, 0, len(messageLengths))
	witnesses := make([][]byte, 0, len(messageLengths))
	t.Cleanup(func() {
		for i, witness := range witnesses {
			if !connectlib.MessagePoolReturn(witness) {
				t.Errorf("partial batch retained or over-returned input owner %d", i)
			}
		}
	})
	for _, messageLength := range messageLengths {
		message := connectlib.MessagePoolGet(messageLength)
		batch = append(batch, message)
		witnesses = append(witnesses, connectlib.MessagePoolShareReadOnly(message))
	}
	firstFrameByteCount := exchangeIOFrameHeaderByteCount + messageLengths[0]
	conn := &exchangeWriteBudgetConn{
		remaining: firstFrameByteCount + exchangeIOFrameHeaderByteCount + 16,
		err:       errors.New("injected batch write failure"),
	}

	before := snapshotExchangeIO("sent", "data")
	if err := buffer.WriteMessages(conn, batch); !errors.Is(err, conn.err) {
		t.Fatalf("batch write error = %v, want %v", err, conn.err)
	}
	requireExchangeIODelta(t, "sent", "data", before, 1, float64(firstFrameByteCount))
}

func TestExchangeConnectionTracksActiveOutboundEndpoint(t *testing.T) {
	for _, op := range []ExchangeOp{ExchangeOpTransport, ExchangeOpForward} {
		opLabel := exchangeOpMetricLabel(op)
		t.Run(opLabel, func(t *testing.T) {
			before := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", opLabel))
			clientConn, serverConn := net.Pipe()
			settings := DefaultExchangeSettings()
			settings.ExchangePingTimeout = time.Hour
			serverDone := make(chan error, 1)
			go func() {
				serverDone <- serveExchangeHeaderEcho(serverConn, settings)
			}()
			settings.DialContext = func(context.Context, string, string) (net.Conn, error) {
				return clientConn, nil
			}
			connection, err := NewExchangeConnection(
				context.Background(),
				ExchangeHeader{
					Version:    1,
					ClientId:   server.NewId(),
					ResidentId: server.NewId(),
					Op:         op,
				},
				"edge-a",
				18443,
				nil,
				settings,
			)
			if err != nil {
				t.Fatal(err)
			}
			if active := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", opLabel)); active != before+1 {
				t.Fatalf("active outbound %s = %v, want %v", opLabel, active, before+1)
			}
			connection.Close()
			if active := testutil.ToFloat64(exchangeActiveConnectionsGauge.WithLabelValues("outbound", opLabel)); active != before {
				t.Fatalf("active outbound %s after close = %v, want %v", opLabel, active, before)
			}
			select {
			case serverErr := <-serverDone:
				if !isExpectedExchangeFixtureCloseError(serverErr) {
					t.Fatal(serverErr)
				}
			case <-time.After(time.Second):
				t.Fatal("exchange fixture did not observe connection close")
			}
		})
	}
}

// Every frame carries its connection's exchange operation: a dialed buffer
// takes it from the header it sends, and an accepted buffer learns it from the
// header it reads. The dark release measures the transport share by it.
func TestExchangeBufferLabelsFramesWithTheOperation(t *testing.T) {
	settings := DefaultExchangeSettings()
	for _, op := range []ExchangeOp{ExchangeOpTransport, ExchangeOpForward} {
		opLabel := exchangeOpMetricLabel(op)
		sender := NewDefaultExchangeBuffer(settings)
		sender.setIOOp(op)
		receiver := NewReceiveOnlyExchangeBuffer(settings)
		senderConn, receiverConn := net.Pipe()

		type labelSet struct{ op, direction, kind string }
		labelSets := []labelSet{}
		for _, labelOp := range exchangeIOOpLabels {
			for _, direction := range exchangeIODirectionLabels {
				for _, kind := range exchangeIOFrameKindLabels {
					labelSets = append(labelSets, labelSet{op: labelOp, direction: direction, kind: kind})
				}
			}
		}
		before := map[labelSet]exchangeIOMetricSnapshot{}
		for _, labels := range labelSets {
			before[labels] = snapshotExchangeIOOp(labels.op, labels.direction, labels.kind)
		}

		writeDone := make(chan error, 1)
		go func() {
			if err := sender.WriteHeader(context.Background(), senderConn, &ExchangeHeader{
				Version:    1,
				ClientId:   server.NewId(),
				ResidentId: server.NewId(),
				Op:         op,
			}); err != nil {
				writeDone <- err
				return
			}
			if err := sender.WriteMessage(senderConn, connectlib.MessagePoolGet(21)); err != nil {
				writeDone <- err
				return
			}
			writeDone <- sender.WriteMessage(senderConn, connectlib.MessagePoolGet(0))
		}()
		if _, err := receiver.ReadHeader(context.Background(), receiverConn); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			message, err := receiver.ReadMessage(receiverConn)
			if err != nil {
				t.Fatal(err)
			}
			connectlib.MessagePoolReturn(message)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		senderConn.Close()
		receiverConn.Close()

		for _, labels := range labelSets {
			after := snapshotExchangeIOOp(labels.op, labels.direction, labels.kind)
			wantFrames := 0.0
			if labels.op == opLabel {
				wantFrames = 1
			}
			if delta := after.frames - before[labels].frames; delta != wantFrames {
				t.Errorf("%s: %+v frame delta = %v, want %v", opLabel, labels, delta, wantFrames)
			}
		}
	}
}

// An accepted forward labels its handshake and its writer's pings with the
// operation it read from the header.
func TestExchangeAcceptedForwardLabelsItsFrames(t *testing.T) {
	warmLocalTransportPool()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		settings := DefaultExchangeSettings()
		settings.ExchangePingTimeout = time.Second
		resident := newResidentCallbackLifecycleFixture(t, ctx, settings)
		exchange := &Exchange{
			ctx:             ctx,
			cancel:          cancel,
			settings:        settings,
			residents:       map[server.Id]*Resident{resident.clientId: resident},
			residentChanges: map[server.Id]chan struct{}{},
			connections:     map[server.Id]map[server.Id]context.CancelFunc{},
		}
		resident.exchange = exchange
		resident.residentId = server.NewId()
		dialerConn, acceptedConn := net.Pipe()
		handleDone := make(chan struct{})
		go func() {
			defer close(handleDone)
			exchange.handleExchangeConnection(acceptedConn)
		}()

		handshakeReceivedBefore := snapshotExchangeIOOp("forward", "received", "handshake")
		pingSentBefore := snapshotExchangeIOOp("forward", "sent", "ping")
		unknownBefore := snapshotExchangeIOOp("unknown", "received", "handshake")
		dialer := NewDefaultExchangeBuffer(settings)
		dialer.setIOOp(ExchangeOpForward)
		dialerReceive := NewReceiveOnlyExchangeBuffer(settings)
		dialerReceive.setIOOp(ExchangeOpForward)
		if err := dialer.WriteHeader(ctx, dialerConn, &ExchangeHeader{
			Version:    1,
			ClientId:   resident.clientId,
			ResidentId: resident.residentId,
			Op:         ExchangeOpForward,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := dialerReceive.ReadHeader(ctx, dialerConn); err != nil {
			t.Fatal(err)
		}
		ping, err := dialerReceive.ReadMessage(dialerConn)
		if err != nil || len(ping) != 0 {
			t.Fatalf("accepted forward ping = %d bytes, %v", len(ping), err)
		}
		connectlib.MessagePoolReturn(ping)
		dialerConn.Close()
		<-handleDone

		// the accepted side read the header, and the dialer read its echo
		if delta := snapshotExchangeIOOp("forward", "received", "handshake").frames - handshakeReceivedBefore.frames; delta != 2 {
			t.Errorf("forward handshakes received = %v, want 2", delta)
		}
		if delta := snapshotExchangeIOOp("forward", "sent", "ping").frames - pingSentBefore.frames; delta != 1 {
			t.Errorf("forward pings sent by the accepted writer = %v, want 1", delta)
		}
		if snapshotExchangeIOOp("unknown", "received", "handshake").frames != unknownBefore.frames {
			t.Error("the accepted header was labeled with an unknown operation")
		}
		if err := resident.CloseAndWait(context.Background()); err != nil {
			t.Error(err)
		}
	})
}
