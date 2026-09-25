package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const deviceRpcPath = "/device-rpc"

// deviceRpcHandler is the device rpc endpoint a DeviceRemote (e.g. a browser)
// connects to directly on the proxy host to control the hosted proxy
// DeviceLocal. It terminates WebSocket or an enabled FramerXl upgrade, resolves the hosted DeviceLocal by
// the caller's signed proxy id, and serves its DeviceLocalRpc — the DeviceLocal
// lives in this process, so no connect-service or resident hop is involved.
//
// In production it is a GET /device-rpc route on the proxy api TLS listener
// (see apiServer), so the endpoint is wss only and terminates the same
// per-proxy SNI TLS as the rest of the proxy api. Tests wire the same handler
// into a plain listener and dial ws, the way server/connect exposes its handler
// for tests.
//
// Auth is the signed proxy id in native Authorization or the browser `proxy`
// query parameter (a browser WebSocket cannot set request headers). The signed
// proxy id is an HMAC bearer token — the same credential the wg and https data
// planes authenticate with (see model.SignProxyId) — so no JWT is needed.
type deviceRpcHandler struct {
	proxyDeviceManager *ProxyDeviceManager
	settings           *ProxySettings

	upgrader websocket.Upgrader
}

// deviceRpcWebsocket is the complete websocket surface consumed by the SDK's
// hosted mux. Keeping the delegate behind this local interface makes the
// observation wrapper independently testable without changing the SDK's
// transport contract.
type deviceRpcWebsocket interface {
	sdk.DeviceRpcWs
	WriteControl(messageType int, data []byte, deadline time.Time) error
	NextReader() (messageType int, r io.Reader, err error)
	SetReadLimit(limit int64)
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
	SetPongHandler(h func(appData string) error)
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

const (
	deviceRpcSessionCloseLocal uint32 = iota
	deviceRpcSessionCloseOrderly
	deviceRpcSessionCloseAbrupt
	deviceRpcSessionCloseEOF
	deviceRpcSessionCloseClosed
	deviceRpcSessionCloseIOError
)

// deviceRpcObservedWebsocket records only whether non-empty binary traffic
// crossed each direction and a bounded close class. It deliberately retains no
// frame bytes, counts, addresses, proxy ids, or raw error strings.
type deviceRpcObservedWebsocket struct {
	ws deviceRpcWebsocket

	ingress    atomic.Bool
	egress     atomic.Bool
	closeClass atomic.Uint32
	readLimit  atomic.Int64
}

var _ sdk.DeviceRpcWs = (*deviceRpcObservedWebsocket)(nil)

func newDeviceRpcObservedWebsocket(ws deviceRpcWebsocket) *deviceRpcObservedWebsocket {
	observed := &deviceRpcObservedWebsocket{ws: ws}
	observed.readLimit.Store(3 * 1024 * 1024)
	return observed
}

func (self *deviceRpcObservedWebsocket) WriteMessages(messages [][]byte) error {
	var err error
	if framed, ok := self.ws.(interface{ WriteMessages([][]byte) error }); ok {
		err = framed.WriteMessages(messages)
	} else {
		for _, message := range messages {
			if err = self.WriteMessage(websocket.BinaryMessage, message); err != nil {
				break
			}
		}
	}
	if err != nil {
		self.observeError(err)
		return err
	}
	for _, message := range messages {
		if len(message) > 0 {
			self.egress.Store(true)
			break
		}
	}
	return nil
}

func (self *deviceRpcObservedWebsocket) ReadPooledMessage() (int, []byte, error) {
	var kind int
	var message []byte
	var err error
	if framed, ok := self.ws.(interface{ ReadPooledMessage() (int, []byte, error) }); ok {
		kind, message, err = framed.ReadPooledMessage()
	} else {
		var reader io.Reader
		kind, reader, err = self.ws.NextReader()
		if err == nil && kind == websocket.BinaryMessage {
			message, err = connect.MessagePoolReadAllLimit(reader, self.readLimit.Load())
		}
	}
	if err != nil {
		self.observeError(err)
	} else if kind == websocket.BinaryMessage && len(message) > 0 {
		self.ingress.Store(true)
	}
	return kind, message, err
}

type deviceRpcObservedReader struct {
	reader   io.Reader
	observed *deviceRpcObservedWebsocket
}

func (self *deviceRpcObservedReader) Read(p []byte) (int, error) {
	n, err := self.reader.Read(p)
	if 0 < n {
		self.observed.ingress.Store(true)
	}
	// io.EOF is the ordinary end of one complete WebSocket message. Every
	// other body-read error is terminal transport evidence and must survive to
	// the one bounded session marker.
	if err != nil && !errors.Is(err, io.EOF) {
		self.observed.observeError(err)
	}
	return n, err
}

func deviceRpcSessionCloseClass(err error) uint32 {
	if err == nil {
		return deviceRpcSessionCloseLocal
	}
	var closeError *websocket.CloseError
	if errors.As(err, &closeError) {
		if closeError.Code == websocket.CloseNormalClosure || closeError.Code == websocket.CloseGoingAway {
			return deviceRpcSessionCloseOrderly
		}
		if closeError.Code == websocket.CloseAbnormalClosure {
			return deviceRpcSessionCloseAbrupt
		}
		return deviceRpcSessionCloseIOError
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return deviceRpcSessionCloseAbrupt
	}
	if errors.Is(err, io.EOF) {
		return deviceRpcSessionCloseEOF
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return deviceRpcSessionCloseClosed
	}
	return deviceRpcSessionCloseIOError
}

func deviceRpcSessionCloseClassName(class uint32) string {
	switch class {
	case deviceRpcSessionCloseOrderly:
		return "orderly-close"
	case deviceRpcSessionCloseAbrupt:
		return "abrupt-close"
	case deviceRpcSessionCloseEOF:
		return "eof"
	case deviceRpcSessionCloseClosed:
		return "closed"
	case deviceRpcSessionCloseIOError:
		return "io-error"
	default:
		return "local-close"
	}
}

func (self *deviceRpcObservedWebsocket) observation() (stage string, result string, ingress string, egress string) {
	hasIngress := self.ingress.Load()
	hasEgress := self.egress.Load()
	stage = "transport"
	if hasIngress {
		stage = "request"
	}
	if hasEgress {
		stage = "response"
	}
	ingress = "absent"
	if hasIngress {
		ingress = "present"
	}
	egress = "absent"
	if hasEgress {
		egress = "present"
	}
	return stage, deviceRpcSessionCloseClassName(self.closeClass.Load()), ingress, egress
}

func (self *deviceRpcObservedWebsocket) observeError(err error) {
	if err != nil {
		self.closeClass.CompareAndSwap(deviceRpcSessionCloseLocal, deviceRpcSessionCloseClass(err))
	}
}

func (self *deviceRpcObservedWebsocket) WriteMessage(messageType int, data []byte) error {
	err := self.ws.WriteMessage(messageType, data)
	if err == nil && messageType == websocket.BinaryMessage && 0 < len(data) {
		self.egress.Store(true)
	} else if err != nil {
		self.observeError(err)
	}
	return err
}

func (self *deviceRpcObservedWebsocket) WriteControl(messageType int, data []byte, deadline time.Time) error {
	err := self.ws.WriteControl(messageType, data, deadline)
	self.observeError(err)
	return err
}

func (self *deviceRpcObservedWebsocket) NextReader() (int, io.Reader, error) {
	messageType, reader, err := self.ws.NextReader()
	if err != nil {
		self.observeError(err)
		return messageType, reader, err
	}
	if messageType == websocket.BinaryMessage {
		reader = &deviceRpcObservedReader{reader: reader, observed: self}
	}
	return messageType, reader, nil
}

func (self *deviceRpcObservedWebsocket) Close() error { return self.ws.Close() }
func (self *deviceRpcObservedWebsocket) SetReadLimit(limit int64) {
	self.readLimit.Store(limit)
	self.ws.SetReadLimit(limit)
}
func (self *deviceRpcObservedWebsocket) SetReadDeadline(t time.Time) error {
	err := self.ws.SetReadDeadline(t)
	self.observeError(err)
	return err
}
func (self *deviceRpcObservedWebsocket) SetWriteDeadline(t time.Time) error {
	err := self.ws.SetWriteDeadline(t)
	self.observeError(err)
	return err
}
func (self *deviceRpcObservedWebsocket) SetPongHandler(h func(string) error) {
	self.ws.SetPongHandler(h)
}
func (self *deviceRpcObservedWebsocket) LocalAddr() net.Addr  { return self.ws.LocalAddr() }
func (self *deviceRpcObservedWebsocket) RemoteAddr() net.Addr { return self.ws.RemoteAddr() }

func NewDeviceRpcHandler(
	proxyDeviceManager *ProxyDeviceManager,
	settings *ProxySettings,
) *deviceRpcHandler {
	return &deviceRpcHandler{
		proxyDeviceManager: proxyDeviceManager,
		settings:           settings,
		upgrader: websocket.Upgrader{
			// clients connect from browser origins; the signed proxy id, not
			// the origin, is the security boundary
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (self *deviceRpcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	custom := connect.IsFramedUpgrade(r, connect.H1FramerXlProtocol)
	upgradeStart := time.Now()
	if custom {
		if !self.settings.EnableDeviceRpcH1Plus || !connect.H1PlusAvailable() {
			http.Error(w, "upgrade unavailable", http.StatusUpgradeRequired)
			return
		}
		if err := connect.ValidateFramedUpgradeRequest(r, connect.H1FramerXlProtocol); err != nil {
			http.Error(w, "invalid upgrade", http.StatusBadRequest)
			return
		}
	}
	proxyId, err := deviceRpcSignedProxyId(r)
	if err != nil {
		if custom {
			connect.RecordH1PlusSelection(self.settings.DeviceRpcH1PlusStats, time.Since(upgradeStart), &connect.HTTPUpgradeError{StatusCode: 401, Reason: "authorization", Terminal: true})
		}
		glog.Infof("[drpc]auth err = %s\n", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	pd, err := self.proxyDeviceManager.OpenProxyDevice(proxyId)
	if err != nil {
		glog.Infof("[drpc][%s]open device err = %s\n", proxyId, err)
		http.Error(w, "device unavailable", http.StatusServiceUnavailable)
		return
	}

	var ws deviceRpcWebsocket
	if custom {
		conn, upgradeErr := connect.AcceptFramedUpgrade(w, r, connect.H1FramerXlProtocol, self.settings.ProxyWriteTimeout)
		connect.RecordH1PlusSelection(self.settings.DeviceRpcH1PlusStats, time.Since(upgradeStart), upgradeErr)
		if upgradeErr != nil {
			return
		}
		// The RPC envelope includes the stream tag and preserves the SDK's
		// established 3-MiB admission limit. Each direction has its own mux
		// budget; this creates no maximum-sized per-session scratch buffer.
		ws, err = connect.NewFramedMessageConn(conn, connect.H1FramerXlProtocol, 3*1024*1024, self.settings.DeviceRpcH1PlusStats)
		if err != nil {
			conn.Close()
			return
		}
	} else {
		ws, err = self.upgradeWebsocket(w, r)
	}
	if err != nil {
		glog.Infof("[drpc][%s]ws upgrade err = %s\n", proxyId, err)
		return
	}
	glog.Infof("[drpc][%s]device rpc attached\n", proxyId)
	observedWs := newDeviceRpcObservedWebsocket(ws)

	// serves the rpc session; blocks until it ends, then closes the websocket.
	// PushDeviceRpc keeps the device non-idle for the session's duration.
	serveErr := pd.PushDeviceRpc(observedWs)
	if serveErr != nil {
		glog.Infof("[drpc][%s]device rpc done = %s\n", proxyId, serveErr)
	}
	observedWs.observeError(serveErr)
	stage, result, ingress, egress := observedWs.observation()
	glog.Infof(
		"[drpc-session] endpoint=proxy stage=%s result=%s ingress=%s egress=%s\n",
		stage, result, ingress, egress,
	)
	ws.Close()
	glog.Infof("[drpc][%s]device rpc detached\n", proxyId)
}

// deviceRpcSignedProxyId extracts the signed proxy id the client attached to the
// request. The `proxy` query parameter is the primary form (a browser can set
// it but not headers); the Authorization header is accepted for non-browser
// callers.
func deviceRpcSignedProxyId(r *http.Request) (server.Id, error) {
	if signed := r.URL.Query().Get("proxy"); signed != "" {
		return model.ParseSignedProxyId(signed)
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
			return model.ParseSignedProxyId(auth[len(prefix):])
		}
	}
	return server.Id{}, fmt.Errorf("missing signed proxy id")
}

// Preserves the authenticated handler's WebSocket upgrade branch as a narrow
// owner boundary, independently testable without creating a hosted device.
func (self *deviceRpcHandler) upgradeWebsocket(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return self.upgrader.Upgrade(&deviceRpcDeadlineResponseWriter{ResponseWriter: w}, r, nil)
}

// Wraps only the socket whose ownership the successful hijack transfers to
// Gorilla. HTTP behavior, buffered input and borrowed pre-hijack state stay intact.
type deviceRpcDeadlineResponseWriter struct{ http.ResponseWriter }

// Gorilla ignores inner deadline errors; the shared owned adapter latches them.
func (self *deviceRpcDeadlineResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := self.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("device rpc response writer does not support hijacking")
	}
	conn, readWriter, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return connect.NewWebSocketWriteBatchConn(conn), readWriter, nil
}
