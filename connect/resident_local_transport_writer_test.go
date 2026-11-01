// In process, a resident's frames reach the client's carrier writer as the
// read-only shares the resident's sender handed its route, where a socket used
// to deliver a fresh copy. The sender keeps the same bytes for retransmission,
// so no writer may change a share. These tests pass shares through the
// production H1, H1+ and H3 writers and compare the retained bytes after.
package connect

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	clientconnect "github.com/urnetwork/connect"
)

// Messages above the H1 control size, across the pool classes a resident
// sends, each with a share that stands in for the resident's route copy.
type localTransportShareSet struct {
	retained  [][]byte
	originals [][]byte
	shares    [][]byte
}

func newLocalTransportShareSet(byteCounts ...int) *localTransportShareSet {
	set := &localTransportShareSet{}
	for index, byteCount := range byteCounts {
		message := newLocalTransportTestMessage(index+1, byteCount)
		set.retained = append(set.retained, message)
		set.originals = append(set.originals, bytes.Clone(message))
		set.shares = append(set.shares, clientconnect.MessagePoolShareReadOnly(message))
	}
	return set
}

// Fails if a writer changed a retained message, then returns the retained
// references, which must be the final owners.
func (self *localTransportShareSet) requireUnchanged(t testing.TB, writer string) {
	t.Helper()
	for index, message := range self.retained {
		if !bytes.Equal(message, self.originals[index]) {
			t.Errorf("%s changed shared message %d in place", writer, index)
		}
	}
	requireResidentPoolOwnersReturned(t, self.retained, writer+" retained shares")
}

// Fails unless written holds the original messages in order.
func (self *localTransportShareSet) requireWritten(t testing.TB, writer string, written [][]byte) {
	t.Helper()
	if len(written) != len(self.originals) {
		t.Fatalf("%s wrote %d messages, want %d", writer, len(written), len(self.originals))
	}
	for index, message := range written {
		if !bytes.Equal(message, self.originals[index]) {
			t.Errorf("%s wrote message %d with different bytes", writer, index)
		}
	}
}

var localTransportShareByteCounts = []int{17, 200, 1400, 3000, 7000}

// The WebSocket (H1) writer over the production upgrade and batching wrapper.
func TestLocalTransportReadOnlyShareUnmodifiedByH1Writer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverConns := make(chan *websocket.Conn, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{ReadBufferSize: 4 * 1024, WriteBufferSize: 4 * 1024}
		ws, err := upgrader.Upgrade(&connectH1BatchResponseWriter{ResponseWriter: w}, r, nil)
		if err != nil {
			return
		}
		serverConns <- ws
	}))
	defer httpServer.Close()
	clientWs, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientWs.Close()
	serverWs := <-serverConns
	defer serverWs.Close()

	set := newLocalTransportShareSet(localTransportShareByteCounts...)
	receive := make(chan []byte, len(set.shares))
	for _, share := range set.shares[1:] {
		receive <- share
	}
	writeBatch := connectH1WriteBatchForConn(serverWs.UnderlyingConn())
	if writeBatch == nil {
		t.Fatal("the production upgrade did not install the batching wrapper")
	}
	if _, err := writeConnectH1UserReadyBatch(ctx, serverWs, writeBatch, receive, set.shares[0], true, 15*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	written := [][]byte{}
	for range set.shares {
		messageType, message, err := clientWs.ReadMessage()
		if err != nil || messageType != websocket.BinaryMessage {
			t.Fatalf("read = %d, %v", messageType, err)
		}
		written = append(written, message)
	}
	set.requireWritten(t, "the H1 writer", written)
	set.requireUnchanged(t, "the H1 writer")
}

// The framed (H1+) writer, which batches through FramedMessageConn.
func TestLocalTransportReadOnlyShareUnmodifiedByH1PlusWriter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	maximum := DefaultConnectHandlerSettings().FramerSettings.MaxMessageLen
	serverFramed, err := clientconnect.NewFramedMessageConn(serverConn, clientconnect.H1FramerProtocol, maximum, nil)
	if err != nil {
		t.Fatal(err)
	}
	clientFramed, err := clientconnect.NewFramedMessageConn(clientConn, clientconnect.H1FramerProtocol, maximum, nil)
	if err != nil {
		t.Fatal(err)
	}

	set := newLocalTransportShareSet(localTransportShareByteCounts...)
	receive := make(chan []byte, len(set.shares))
	for _, share := range set.shares[1:] {
		receive <- share
	}
	writeDone := make(chan error, 1)
	go func() {
		_, err := writeConnectH1UserReadyBatch(ctx, serverFramed, nil, receive, set.shares[0], true, 15*time.Second, nil)
		writeDone <- err
	}()
	written := [][]byte{}
	for range set.shares {
		messageType, message, err := clientFramed.ReadMessage()
		if err != nil || messageType != websocket.BinaryMessage {
			t.Fatalf("read = %d, %v", messageType, err)
		}
		written = append(written, bytes.Clone(message))
		clientconnect.MessagePoolReturn(message)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	set.requireWritten(t, "the H1+ writer", written)
	set.requireUnchanged(t, "the H1+ writer")
}

// A stream that records every byte written.
type localTransportRecordingStream struct {
	bytes.Buffer
}

func (self *localTransportRecordingStream) SetWriteDeadline(time.Time) error {
	return nil
}

// The H3 writers: the stream batch on the legacy wire and with the delay
// header, and DATAGRAM fragments on both.
func TestLocalTransportReadOnlyShareUnmodifiedByH3Writers(t *testing.T) {
	settings := DefaultConnectHandlerSettings()
	framer := clientconnect.NewFramer(settings.FramerSettings)
	delayH3 := clientconnect.NewTransportDelayH3Peer(clientconnect.Id{}, settings.FramerSettings)
	for _, c := range []struct {
		name    string
		framing connectQuicStreamFraming
	}{
		{name: "the H3 stream writer", framing: framer},
		{name: "the H3 delay stream writer", framing: delayH3},
	} {
		set := newLocalTransportShareSet(localTransportShareByteCounts...)
		stream := &localTransportRecordingStream{}
		storage := make([]byte, 2*connectH3WriteBatchMaxByteCount)
		err := writeConnectQuicBatchWithDeadline(c.framing, stream, settings.WriteTimeout, set.shares, storage, func(message []byte) {
			clientconnect.MessagePoolReturn(message)
		}, func([]byte) {})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		written := [][]byte{}
		for range set.shares {
			message, err := c.framing.Read(&stream.Buffer)
			if err != nil {
				t.Fatalf("%s: read: %v", c.name, err)
			}
			written = append(written, bytes.Clone(message))
			clientconnect.MessagePoolReturn(message)
		}
		if _, err := c.framing.Read(&stream.Buffer); err != io.EOF {
			t.Errorf("%s wrote past its messages: %v", c.name, err)
		}
		set.requireWritten(t, c.name, written)
		set.requireUnchanged(t, c.name)
	}

	datagramSettings := clientconnect.DefaultH3DatagramSettings()
	for _, c := range []struct {
		name string
		send func(fragmenter *clientconnect.H3DatagramFragmenter, message []byte, send func([]byte) error) (bool, error)
	}{
		{
			name: "the H3 DATAGRAM writer",
			send: func(fragmenter *clientconnect.H3DatagramFragmenter, message []byte, send func([]byte) error) (bool, error) {
				useStream, _, err := fragmenter.SendHybrid(message, datagramSettings.TargetDatagramByteCount, send)
				return useStream, err
			},
		},
		{
			name: "the H3 delay DATAGRAM writer",
			send: func(fragmenter *clientconnect.H3DatagramFragmenter, message []byte, send func([]byte) error) (bool, error) {
				useStream, _, err := delayH3.SendHybrid(fragmenter, message, datagramSettings.TargetDatagramByteCount, send)
				return useStream, err
			},
		},
	} {
		fragmenter, err := clientconnect.NewH3DatagramFragmenter(datagramSettings, nil)
		if err != nil {
			t.Fatal(err)
		}
		set := newLocalTransportShareSet(17, 200, 1000)
		datagramCount := 0
		for _, share := range set.shares {
			useStream, err := c.send(fragmenter, share, func(datagram []byte) error {
				datagramCount += 1
				return nil
			})
			if err != nil || useStream {
				t.Fatalf("%s: send = stream %t, %v", c.name, useStream, err)
			}
			// the caller keeps ownership, as the writer's loop returns it
			clientconnect.MessagePoolReturn(share)
		}
		if datagramCount < len(set.shares) {
			t.Errorf("%s sent %d datagrams for %d messages", c.name, datagramCount, len(set.shares))
		}
		set.requireUnchanged(t, c.name)
	}
}
