package proxy

import (
	"fmt"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const deviceRpcPath = "/device-rpc"

// deviceRpcHandler is the device rpc endpoint a DeviceRemote (e.g. a browser)
// connects to directly on the proxy host to control the hosted proxy
// DeviceLocal. It terminates the websocket, resolves the hosted DeviceLocal by
// the caller's signed proxy id, and serves its DeviceLocalRpc — the DeviceLocal
// lives in this process, so no connect-service or resident hop is involved.
//
// In production it is a GET /device-rpc route on the proxy api TLS listener
// (see apiServer), so the endpoint is wss only and terminates the same
// per-proxy SNI TLS as the rest of the proxy api. Tests wire the same handler
// into a plain listener and dial ws, the way server/connect exposes its handler
// for tests.
//
// Auth is the signed proxy id, passed as the `proxy` query parameter (a browser
// WebSocket cannot set request headers, but can set query params). The signed
// proxy id is an HMAC bearer token — the same credential the wg and https data
// planes authenticate with (see model.SignProxyId) — so no JWT is needed.
type deviceRpcHandler struct {
	proxyDeviceManager *ProxyDeviceManager
	settings           *ProxySettings

	upgrader websocket.Upgrader
}

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
	proxyId, err := deviceRpcSignedProxyId(r)
	if err != nil {
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

	ws, err := self.upgrader.Upgrade(w, r, nil)
	if err != nil {
		glog.Infof("[drpc][%s]ws upgrade err = %s\n", proxyId, err)
		return
	}
	glog.Infof("[drpc][%s]device rpc attached\n", proxyId)

	// serves the rpc session; blocks until it ends, then closes the websocket.
	// PushDeviceRpc keeps the device non-idle for the session's duration.
	if err := pd.PushDeviceRpc(ws); err != nil {
		glog.Infof("[drpc][%s]device rpc done = %s\n", proxyId, err)
	}
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
