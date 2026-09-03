package proxy

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type testingDeviceRpcObservedWebsocket struct {
	messageType int
	message     string
	reader      io.Reader
	readError   error
	writeError  error
}

func (self *testingDeviceRpcObservedWebsocket) WriteMessage(int, []byte) error {
	return self.writeError
}
func (self *testingDeviceRpcObservedWebsocket) WriteControl(int, []byte, time.Time) error {
	return nil
}
func (self *testingDeviceRpcObservedWebsocket) NextReader() (int, io.Reader, error) {
	if self.readError != nil {
		return 0, nil, self.readError
	}
	if self.reader != nil {
		return self.messageType, self.reader, nil
	}
	return self.messageType, strings.NewReader(self.message), nil
}
func (self *testingDeviceRpcObservedWebsocket) Close() error                      { return nil }
func (self *testingDeviceRpcObservedWebsocket) SetReadLimit(int64)                {}
func (self *testingDeviceRpcObservedWebsocket) SetReadDeadline(time.Time) error   { return nil }
func (self *testingDeviceRpcObservedWebsocket) SetWriteDeadline(time.Time) error  { return nil }
func (self *testingDeviceRpcObservedWebsocket) SetPongHandler(func(string) error) {}
func (self *testingDeviceRpcObservedWebsocket) LocalAddr() net.Addr               { return nil }
func (self *testingDeviceRpcObservedWebsocket) RemoteAddr() net.Addr              { return nil }

func TestDeviceRpcSessionObservationStagesAreBounded(t *testing.T) {
	const privateFrame = "signedProxyId=private endpoint=wss://private.example"
	delegate := &testingDeviceRpcObservedWebsocket{
		messageType: websocket.BinaryMessage,
		message:     privateFrame,
	}
	observed := newDeviceRpcObservedWebsocket(delegate)

	stage, result, ingress, egress := observed.observation()
	if stage != "transport" || result != "local-close" || ingress != "absent" || egress != "absent" {
		t.Fatalf("initial observation = %s/%s/%s/%s", stage, result, ingress, egress)
	}

	_, reader, err := observed.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	stage, result, ingress, egress = observed.observation()
	if stage != "request" || result != "local-close" || ingress != "present" || egress != "absent" {
		t.Fatalf("request observation = %s/%s/%s/%s", stage, result, ingress, egress)
	}

	if err := observed.WriteMessage(websocket.BinaryMessage, []byte(privateFrame)); err != nil {
		t.Fatal(err)
	}
	stage, result, ingress, egress = observed.observation()
	if stage != "response" || result != "local-close" || ingress != "present" || egress != "present" {
		t.Fatalf("response observation = %s/%s/%s/%s", stage, result, ingress, egress)
	}

	summary := strings.Join([]string{stage, result, ingress, egress}, " ")
	if strings.Contains(summary, privateFrame) || strings.Contains(summary, "private.example") {
		t.Fatalf("observation retained private frame data: %q", summary)
	}
}

func TestDeviceRpcSessionObservationIgnoresEmptyAndFailedFrames(t *testing.T) {
	delegate := &testingDeviceRpcObservedWebsocket{
		messageType: websocket.BinaryMessage,
		writeError:  errors.New("private write error"),
	}
	observed := newDeviceRpcObservedWebsocket(delegate)

	_, reader, err := observed.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	_ = observed.WriteMessage(websocket.BinaryMessage, []byte("private response"))

	stage, result, ingress, egress := observed.observation()
	if stage != "transport" || ingress != "absent" || egress != "absent" {
		t.Fatalf("empty/failed observation = stage=%s ingress=%s egress=%s", stage, ingress, egress)
	}
	if result != "io-error" {
		t.Fatalf("failed write result = %q, want io-error", result)
	}
}

type testingDeviceRpcUnexpectedEOFReader struct {
	delivered bool
}

func (self *testingDeviceRpcUnexpectedEOFReader) Read(p []byte) (int, error) {
	if self.delivered {
		return 0, io.ErrUnexpectedEOF
	}
	self.delivered = true
	return copy(p, "private partial frame"), nil
}

func TestDeviceRpcSessionObservationCapturesReaderBodyFailure(t *testing.T) {
	delegate := &testingDeviceRpcObservedWebsocket{
		messageType: websocket.BinaryMessage,
		reader:      &testingDeviceRpcUnexpectedEOFReader{},
	}
	observed := newDeviceRpcObservedWebsocket(delegate)
	_, reader, err := observed.NextReader()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("body read error = %v, want unexpected EOF", err)
	}

	stage, result, ingress, egress := observed.observation()
	if stage != "request" || result != "abrupt-close" || ingress != "present" || egress != "absent" {
		t.Fatalf("partial-body observation = %s/%s/%s/%s", stage, result, ingress, egress)
	}
}

func TestDeviceRpcSessionObservationCapturesTerminalServeFailure(t *testing.T) {
	observed := newDeviceRpcObservedWebsocket(&testingDeviceRpcObservedWebsocket{})
	observed.observeError(errors.New("signedProxyId=private endpoint=private"))
	stage, result, ingress, egress := observed.observation()
	if stage != "transport" || result != "io-error" || ingress != "absent" || egress != "absent" {
		t.Fatalf("terminal observation = %s/%s/%s/%s", stage, result, ingress, egress)
	}
}

func TestDeviceRpcSessionCloseClassIsBounded(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "local", err: nil, want: "local-close"},
		{name: "orderly", err: &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "private"}, want: "orderly-close"},
		{name: "going away", err: &websocket.CloseError{Code: websocket.CloseGoingAway, Text: "private"}, want: "orderly-close"},
		{name: "abnormal", err: &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: "private"}, want: "abrupt-close"},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: "abrupt-close"},
		{name: "eof", err: io.EOF, want: "eof"},
		{name: "closed", err: net.ErrClosed, want: "closed"},
		{name: "other websocket", err: &websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: "private"}, want: "io-error"},
		{name: "other", err: errors.New("signedProxyId=private endpoint=private"), want: "io-error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := deviceRpcSessionCloseClassName(deviceRpcSessionCloseClass(test.err))
			if got != test.want {
				t.Fatalf("close class = %q, want %q", got, test.want)
			}
			if strings.Contains(got, "private") {
				t.Fatalf("close class retained private error text: %q", got)
			}
		})
	}
}

var _ deviceRpcWebsocket = (*testingDeviceRpcObservedWebsocket)(nil)
