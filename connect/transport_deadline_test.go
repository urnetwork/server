package connect

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	clientconnect "github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// A fake H1 connection can read or write successfully if the owner ignores a
// rejected setter. No socket, clock advance, or authenticated DB state is used.
type transportDeadlineH1Conn struct {
	clientconnect.H1MessageConn
	readErr, writeErr error
	reads, writes     int
}

func (self *transportDeadlineH1Conn) SetReadDeadline(time.Time) error  { return self.readErr }
func (self *transportDeadlineH1Conn) SetWriteDeadline(time.Time) error { return self.writeErr }
func (self *transportDeadlineH1Conn) ReadMessage() (int, []byte, error) {
	self.reads++
	return websocket.BinaryMessage, []byte{1}, nil
}
func (self *transportDeadlineH1Conn) NextReader() (int, io.Reader, error) {
	self.reads++
	return websocket.BinaryMessage, bytes.NewReader([]byte{1}), nil
}
func (self *transportDeadlineH1Conn) WriteMessage(int, []byte) error {
	self.writes++
	return nil
}

type transportDeadlineStream struct {
	buffer                   bytes.Buffer
	readErr, writeErr, ioErr error
	reads, writes            int
}

func (self *transportDeadlineStream) SetReadDeadline(time.Time) error  { return self.readErr }
func (self *transportDeadlineStream) SetWriteDeadline(time.Time) error { return self.writeErr }
func (self *transportDeadlineStream) Read(p []byte) (int, error) {
	self.reads++
	return self.buffer.Read(p)
}
func (self *transportDeadlineStream) Write(p []byte) (int, error) {
	self.writes++
	if self.ioErr != nil {
		return 0, self.ioErr
	}
	return self.buffer.Write(p)
}

func testConnectDeadlineFramer() *clientconnect.Framer {
	return clientconnect.NewFramer(clientconnect.DefaultFramerSettings(1024))
}

func returnConnectDeadlineMessage(message []byte) {
	clientconnect.MessagePoolReturn(message)
}

func TestConnectTransportDeadlineH1AuthReadRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H1 auth read deadline rejection")
	ws := &transportDeadlineH1Conn{readErr: want}
	_, _, err := readConnectH1AuthWithDeadline(ws, time.Second)
	if !errors.Is(err, want) || ws.reads != 0 {
		t.Fatalf("rejected auth read: err=%v reads=%d", err, ws.reads)
	}
}

func TestConnectTransportDeadlineH1AuthEchoRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H1 auth write deadline rejection")
	ws := &transportDeadlineH1Conn{writeErr: want}
	err := echoConnectH1AuthWithDeadline(ws, time.Second, []byte{1})
	if !errors.Is(err, want) || ws.writes != 0 {
		t.Fatalf("rejected auth echo: err=%v writes=%d", err, ws.writes)
	}
}

func TestConnectTransportDeadlineH1SteadyReadRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H1 steady read deadline rejection")
	ws := &transportDeadlineH1Conn{readErr: want}
	_, _, err := readConnectH1PooledWithDeadline(ws, time.Second, 1024)
	if !errors.Is(err, want) || ws.reads != 0 {
		t.Fatalf("rejected steady read: err=%v reads=%d", err, ws.reads)
	}
}

func TestConnectTransportDeadlineH3AuthReadRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H3 auth read deadline rejection")
	stream := &transportDeadlineStream{readErr: want}
	used := false
	err := withConnectQuicAuthFrameWithDeadline(testConnectDeadlineFramer(), stream, time.Second, func(*protocol.Auth, []byte) error {
		used = true
		return nil
	})
	if !errors.Is(err, want) || stream.reads != 0 || used {
		t.Fatalf("rejected H3 auth: err=%v reads=%d callback=%t", err, stream.reads, used)
	}
}

func TestConnectTransportDeadlineH3SteadyReadRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H3 steady read deadline rejection")
	stream := &transportDeadlineStream{readErr: want}
	frame, err := readConnectQuicFrameWithDeadline(testConnectDeadlineFramer(), stream, time.Second)
	if !errors.Is(err, want) || stream.reads != 0 || frame != nil {
		t.Fatalf("rejected H3 frame: err=%v reads=%d frame=%t", err, stream.reads, frame != nil)
	}
}

func TestConnectTransportDeadlineH3AuthWriteRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H3 auth write deadline rejection")
	stream := &transportDeadlineStream{writeErr: want}
	err := writeConnectQuicAuthWithDeadline(testConnectDeadlineFramer(), stream, time.Second, []byte{1})
	if !errors.Is(err, want) || stream.writes != 0 {
		t.Fatalf("rejected H3 auth echo: err=%v writes=%d", err, stream.writes)
	}
}

func TestConnectTransportDeadlineH3BatchRejectsAndReturnsAllOwners(t *testing.T) {
	want := errors.New("synthetic H3 batch write deadline rejection")
	stream := &transportDeadlineStream{writeErr: want}
	messages := [][]byte{clientconnect.MessagePoolGet(37), clientconnect.MessagePoolGet(53)}
	witnesses := [][]byte{clientconnect.MessagePoolShareReadOnly(messages[0]), clientconnect.MessagePoolShareReadOnly(messages[1])}
	recorded := 0
	err := writeConnectQuicBatchWithDeadline(testConnectDeadlineFramer(), stream, time.Second, messages, make([]byte, 1024), returnConnectDeadlineMessage, func([]byte) { recorded++ })
	for _, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Error("rejected batch retained a pooled message owner")
		}
	}
	if !errors.Is(err, want) || stream.writes != 0 || recorded != 0 {
		t.Fatalf("rejected H3 batch: err=%v writes=%d records=%d", err, stream.writes, recorded)
	}
}

func TestConnectTransportDeadlineH3BatchWriteFailureReturnsOwners(t *testing.T) {
	want := errors.New("synthetic H3 batch write failure")
	stream := &transportDeadlineStream{ioErr: want}
	message := clientconnect.MessagePoolGet(37)
	witness := clientconnect.MessagePoolShareReadOnly(message)
	recorded := 0
	err := writeConnectQuicBatchWithDeadline(testConnectDeadlineFramer(), stream, time.Second, [][]byte{message}, make([]byte, 1024), returnConnectDeadlineMessage, func([]byte) { recorded++ })
	if !clientconnect.MessagePoolReturn(witness) {
		t.Error("failed batch retained a pooled message owner")
	}
	if !errors.Is(err, want) || stream.writes == 0 || recorded != 0 {
		t.Fatalf("failed H3 batch: err=%v writes=%d records=%d", err, stream.writes, recorded)
	}
}

func TestConnectTransportDeadlineH3BatchHealthyRecordsAfterWrite(t *testing.T) {
	stream := &transportDeadlineStream{}
	messages := [][]byte{clientconnect.MessagePoolGet(37), clientconnect.MessagePoolGet(53)}
	witnesses := [][]byte{clientconnect.MessagePoolShareReadOnly(messages[0]), clientconnect.MessagePoolShareReadOnly(messages[1])}
	recorded := 0
	err := writeConnectQuicBatchWithDeadline(testConnectDeadlineFramer(), stream, time.Second, messages, make([]byte, 1024), returnConnectDeadlineMessage, func([]byte) { recorded++ })
	for _, witness := range witnesses {
		if !clientconnect.MessagePoolReturn(witness) {
			t.Error("healthy batch retained a pooled message owner")
		}
	}
	if err != nil || stream.writes == 0 || recorded != 2 {
		t.Fatalf("healthy H3 batch: err=%v writes=%d records=%d", err, stream.writes, recorded)
	}
}

func TestConnectTransportDeadlineH3HeartbeatRejectsBeforeIo(t *testing.T) {
	want := errors.New("synthetic H3 heartbeat deadline rejection")
	stream := &transportDeadlineStream{writeErr: want}
	err := writeConnectQuicHeartbeatWithDeadline(testConnectDeadlineFramer(), stream, time.Second, nil)
	if !errors.Is(err, want) || stream.writes != 0 {
		t.Fatalf("rejected H3 heartbeat: err=%v writes=%d", err, stream.writes)
	}
}

func TestConnectTransportDeadlineH3HeartbeatHealthy(t *testing.T) {
	stream := &transportDeadlineStream{}
	err := writeConnectQuicHeartbeatWithDeadline(testConnectDeadlineFramer(), stream, time.Second, nil)
	if err != nil || stream.writes == 0 {
		t.Fatalf("healthy H3 heartbeat: err=%v writes=%d", err, stream.writes)
	}
}
