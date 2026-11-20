package connect

// Fixtures for the front's tests with the real nginx front: a vault of the
// fixture's identity in the layout the nginx front loads, the real front
// built over it, an env started with it, and minimal h1 and h2 clients that
// keep to one connection, so a test sees what the server sends on it and
// when it closes it.

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/urnetwork/server/v2026/nginx"
)

// The version directory the fixture vault writes.
const testFrontVaultVersion = "2026.11.18"

// Writes the identity's leaf and key for every name the route table serves,
// in a host's flat vault layout, <vault>/tls/<version>/<dir>/<dir>.pem and
// .key, the one a block's container sees at its vault mount: a wildcard *.x
// under star.x, any other name under itself.
func writeTestFrontVault(t testing.TB, vaultDir string, routeTable *nginx.RouteTable, identity *testFrontIdentity) {
	t.Helper()
	pemBytes, keyBytes := identity.pem(t)
	for _, name := range routeTable.ServedNames() {
		dirName := name
		if base, ok := strings.CutPrefix(name, "*."); ok {
			dirName = "star." + base
		}
		dirPath := filepath.Join(vaultDir, "tls", testFrontVaultVersion, dirName)
		if err := os.MkdirAll(dirPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirPath, dirName+".pem"), pemBytes, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirPath, dirName+".key"), keyBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Encodes the generated leaf and key for the local nginx fixtures.
func (self *testFrontIdentity) pem(t testing.TB) ([]byte, []byte) {
	t.Helper()
	keyDer, err := x509.MarshalPKCS8PrivateKey(self.certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: self.certificate.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDer})
}

// The nginx front's certificate store for the route table, loaded from a
// new vault of the identity.
func loadTestFrontCertificateStore(t testing.TB, routeTable *nginx.RouteTable, identity *testFrontIdentity) *nginx.CertificateStore {
	t.Helper()
	vaultDir := t.TempDir()
	writeTestFrontVault(t, vaultDir, routeTable, identity)
	certificateStore, err := nginx.LoadCertificateStore(vaultDir, routeTable.Env, routeTable)
	if err != nil {
		t.Fatal(err)
	}
	return certificateStore
}

// A real nginx front for the env's route table and settings, serving
// connectHandler for connect names. It closes when the test ends.
func newTestNginxFront(t testing.TB, env *testFrontEnv, certificateStore *nginx.CertificateStore, connectHandler http.Handler) *nginx.Front {
	t.Helper()
	front, err := nginx.NewFront(env.ctx, env.routeTable, env.revision, certificateStore, connectHandler, "0.0.0-test", env.settings.Front)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		front.Close()
	})
	return front
}

// Builds and starts a front server with the real nginx front over the
// synthetic route table, serving connectHandler for connect names. mutate,
// when set, adjusts the settings and the route table before the start.
func newTestNginxFrontEnv(t testing.TB, mutate func(env *testFrontEnv), connectHandler http.Handler) *testFrontEnv {
	t.Helper()
	env := newUnstartedTestFrontEnv(t, mutate)
	front := newTestNginxFront(t, env, env.frontServer.certificateStore, connectHandler)
	if err := env.frontServer.start(env.ctx, front, env.connectQuic); err != nil {
		t.Fatal(err)
	}
	return env
}

// A minimal h1 client over one TLS connection to a public socket, so every
// request it sends goes over that connection, until the server closes it.
type testFrontH1Client struct {
	t      testing.TB
	conn   *tls.Conn
	reader *bufio.Reader
}

// Dials the https socket on address for serverName, for HTTP/1.1.
func (self *testFrontEnv) dialH1Client(address string, serverName string) *testFrontH1Client {
	self.t.Helper()
	conn, err := tls.Dial("tcp", address, self.clientTlsConfig(serverName, []string{"http/1.1"}))
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		conn.Close()
	})
	// bounds a broken test, never a passing one
	conn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	return &testFrontH1Client{
		t:      self.t,
		conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// GETs path at authority on the connection and reads the response whole,
// which must be a 200.
func (self *testFrontH1Client) get(authority string, path string) *http.Response {
	self.t.Helper()
	fmt.Fprintf(self.conn, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", path, authority)
	response, err := http.ReadResponse(self.reader, nil)
	if err != nil {
		self.t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		self.t.Fatalf("GET %s = %d %q, %v", path, response.StatusCode, body, err)
	}
	return response
}

// Whether the server has closed the connection: the next read finds its end.
func (self *testFrontH1Client) closedByServer() bool {
	self.t.Helper()
	_, err := self.reader.ReadByte()
	return errors.Is(err, io.EOF)
}

// A minimal h2 client over one TLS connection to a public socket, which
// reads the server's frames itself.
type testFrontH2Client struct {
	t            testing.TB
	conn         *tls.Conn
	framer       *http2.Framer
	encoder      *hpack.Encoder
	headerBlock  *bytes.Buffer
	nextStreamId uint32
	// the GOAWAY the server sent, once it has
	goAwayReceived     bool
	goAwayLastStreamId uint32
	goAwayErrCode      http2.ErrCode
}

// Dials the https socket on address for serverName, negotiating h2, and
// opens the connection.
func (self *testFrontEnv) dialH2Client(address string, serverName string) *testFrontH2Client {
	self.t.Helper()
	conn, err := tls.Dial("tcp", address, self.clientTlsConfig(serverName, []string{"h2"}))
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() {
		conn.Close()
	})
	// bounds a broken test, never a passing one
	conn.SetDeadline(time.Now().Add(testFrontProgressTimeout))
	if protocol := conn.ConnectionState().NegotiatedProtocol; protocol != "h2" {
		self.t.Fatalf("negotiated %q, want h2", protocol)
	}
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		self.t.Fatal(err)
	}
	headerBlock := &bytes.Buffer{}
	client := &testFrontH2Client{
		t:            self.t,
		conn:         conn,
		framer:       http2.NewFramer(conn, conn),
		encoder:      hpack.NewEncoder(headerBlock),
		headerBlock:  headerBlock,
		nextStreamId: 1,
	}
	client.framer.ReadMetaHeaders = hpack.NewDecoder(4*1024, nil)
	if err := client.framer.WriteSettings(); err != nil {
		self.t.Fatal(err)
	}
	return client
}

// Reads the next frame, answering the server's settings and pings, keeping
// the connection's flow-control window open and noting a GOAWAY. Nil once
// the server has closed the connection.
func (self *testFrontH2Client) readFrame() http2.Frame {
	self.t.Helper()
	frame, err := self.framer.ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
			return nil
		}
		self.t.Fatalf("read a frame: %v", err)
	}
	switch frame := frame.(type) {
	case *http2.SettingsFrame:
		if !frame.IsAck() {
			self.framer.WriteSettingsAck()
		}
	case *http2.PingFrame:
		if !frame.IsAck() {
			self.framer.WritePing(true, frame.Data)
		}
	case *http2.DataFrame:
		if dataBytes := len(frame.Data()); 0 < dataBytes {
			self.framer.WriteWindowUpdate(0, uint32(dataBytes))
		}
	case *http2.GoAwayFrame:
		self.goAwayReceived = true
		self.goAwayLastStreamId = frame.LastStreamID
		self.goAwayErrCode = frame.ErrCode
	}
	return frame
}

// GETs path at authority on a new stream and reads frames until its response
// ends. The response's status.
func (self *testFrontH2Client) get(authority string, path string) string {
	self.t.Helper()
	streamId := self.startGet(authority, path)
	status := ""
	for {
		switch frame := self.readFrame().(type) {
		case nil:
			self.t.Fatalf("the connection closed before stream %d's response ended", streamId)
		case *http2.MetaHeadersFrame:
			if frame.StreamID == streamId {
				status = frame.PseudoValue("status")
				if frame.StreamEnded() {
					return status
				}
			}
		case *http2.DataFrame:
			if frame.StreamID == streamId && frame.StreamEnded() {
				return status
			}
		case *http2.RSTStreamFrame:
			if frame.StreamID == streamId {
				self.t.Fatalf("stream %d was reset: %s", streamId, frame.ErrCode)
			}
		}
	}
}

// Opens a new stream that GETs path at authority, sending no body. The
// stream's id.
func (self *testFrontH2Client) startGet(authority string, path string) uint32 {
	self.t.Helper()
	streamId := self.nextStreamId
	self.nextStreamId += 2
	self.headerBlock.Reset()
	for _, field := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: path},
	} {
		self.encoder.WriteField(field)
	}
	err := self.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamId,
		BlockFragment: self.headerBlock.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
	if err != nil {
		self.t.Fatalf("stream %d: %v", streamId, err)
	}
	return streamId
}

// Reads frames until the response head of streamId. Its status.
func (self *testFrontH2Client) readResponseHead(streamId uint32) string {
	self.t.Helper()
	for {
		switch frame := self.readFrame().(type) {
		case nil:
			self.t.Fatalf("the connection closed before stream %d's response head", streamId)
		case *http2.MetaHeadersFrame:
			if frame.StreamID == streamId {
				return frame.PseudoValue("status")
			}
		case *http2.RSTStreamFrame:
			if frame.StreamID == streamId {
				self.t.Fatalf("stream %d was reset: %s", streamId, frame.ErrCode)
			}
		}
	}
}

// A connection's flow-control window when it opens, before any
// WINDOW_UPDATE (RFC 9113, section 6.9.2).
const testFrontH2InitialWindowBytes = 65535

// Opens the client's flow-control windows, the connection's and every
// stream's, to the largest h2 allows, far beyond any socket buffer, so the
// server's writes to a client that stops reading block on the socket, never
// on flow control.
func (self *testFrontH2Client) openWindows() {
	self.t.Helper()
	err := self.framer.WriteSettings(http2.Setting{
		ID:  http2.SettingInitialWindowSize,
		Val: math.MaxInt32,
	})
	if err != nil {
		self.t.Fatal(err)
	}
	if err := self.framer.WriteWindowUpdate(0, math.MaxInt32-testFrontH2InitialWindowBytes); err != nil {
		self.t.Fatal(err)
	}
}

// Reads frames until the server closes the connection, and reports whether
// it sent a GOAWAY first, and its last stream and code.
func (self *testFrontH2Client) readToClose() (goAwayReceived bool, lastStreamId uint32, errCode http2.ErrCode) {
	self.t.Helper()
	for self.readFrame() != nil {
	}
	return self.goAwayReceived, self.goAwayLastStreamId, self.goAwayErrCode
}

// The id of an h2 client's nth stream, from 1.
func testFrontH2StreamId(n int) uint32 {
	return uint32(2*n - 1)
}
