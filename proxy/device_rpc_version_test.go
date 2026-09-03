package proxy

// Wire-version guard test for the hosted device rpc path. The hosted path is
// the one deployment where the two rpc halves genuinely skew: the browser runs
// DeviceRemote out of a cached sdk.wasm while the hosted DeviceLocal lives in
// this process and redeploys continuously. The rpc payload is gob, which fails
// QUIETLY across an incompatible struct change (a renamed field decodes as its
// zero value), so a remote built against an incompatible sdk.DeviceRpcVersion
// must be rejected outright by DeviceLocalRpc.Sync BEFORE any of its cached
// state is applied — mirroring the sdk's own
// TestDeviceRpcSyncRejectsVersionMismatch, but through the real hosted entry
// point:
//
//	ws /device-rpc (signed proxy id) -> deviceRpcHandler ->
//	  ProxyDevice.PushDeviceRpc -> HostedDeviceRpcListener.ServeWs ->
//	    hosted DeviceLocalRpc.Sync
//
// A DeviceRemote with a foreign RpcVersion cannot be built from outside the sdk
// (the rpc settings are unexported and always default to DeviceRpcVersion), so
// the mismatched remote is simulated at the wire level: the same net/rpc gob
// protocol over the same tagged-frame websocket mux the sdk speaks (see
// sdk/device_rpc_transport.go), with only RpcVersion differing — exactly the
// bytes a browser running an sdk.wasm from an incompatible build would put on
// the wire.
//
// The fixture is deliberately the CONTROL-PLANE-ONLY harness
// (proxyTestOptions.controlPlaneOnly): the guard rejects before any state is
// applied and before listeners register, so all it needs is the hosted
// DeviceLocal, the /device-rpc websocket handler in front of it, and one sync
// attempt — no live provider, no egress path, no data plane. Standing those up
// would make this test depend on a provider rendezvous the behavior under test
// never reaches.
//
// Requires the standard local test environment. Skipped under -short.

import (
	"fmt"
	"net/rpc"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// deviceRpcStreamForwardTag is the mux stream tag of the forward stream
// (DeviceRemote is the net/rpc client of DeviceLocalRpc). Mirrors the sdk's
// deviceRpcStreamForward: every websocket binary message is
// [streamTag][payload...], and a zero-length message is a keepalive.
const deviceRpcStreamForwardTag = 0

// deviceRpcForwardConn adapts a device-rpc websocket to the io.ReadWriteCloser
// net/rpc's gob client codec needs, speaking the sdk's mux framing for the
// forward stream only. Reverse-stream frames (tag 1) and keepalives are
// discarded — this client never issues SyncReverse, so no reverse rpc traffic
// occurs during the sync exchange.
type deviceRpcForwardConn struct {
	ws      *websocket.Conn
	readBuf []byte
}

func (self *deviceRpcForwardConn) Read(p []byte) (int, error) {
	for len(self.readBuf) == 0 {
		messageType, message, err := self.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		// skip non-binary frames, keepalives (zero-length), and other streams
		if messageType != websocket.BinaryMessage || len(message) < 1 || message[0] != deviceRpcStreamForwardTag {
			continue
		}
		self.readBuf = message[1:]
	}
	n := copy(p, self.readBuf)
	self.readBuf = self.readBuf[n:]
	return n, nil
}

func (self *deviceRpcForwardConn) Write(p []byte) (int, error) {
	framed := make([]byte, 1+len(p))
	framed[0] = deviceRpcStreamForwardTag
	copy(framed[1:], p)
	if err := self.ws.WriteMessage(websocket.BinaryMessage, framed); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (self *deviceRpcForwardConn) Close() error {
	return self.ws.Close()
}

// syncHostedDeviceRpc connects to the hosted device rpc endpoint as a
// DeviceRemote from a build with rpcVersion would, and performs one
// DeviceLocalRpc.Sync carrying offline as cached remote state (SetOffline is
// the allowed hosted setter, the same control device_rpc_safety_test.go uses).
// Returns the sync response the hosted local produced.
func syncHostedDeviceRpc(
	t testing.TB,
	h *proxyTestHarness,
	rpcVersion int,
	offline bool,
) *sdk.DeviceRemoteSyncResponse {
	u := h.deviceRpcUrl + "/device-rpc?proxy=" + url.QueryEscape(h.signedProxyId)
	ws, _, err := websocket.DefaultDialer.Dial(u, nil)
	connect.AssertEqual(t, err, nil)
	// bound the whole exchange; a hung call fails the test instead of wedging it
	ws.SetReadDeadline(time.Now().Add(60 * time.Second))

	client := rpc.NewClient(&deviceRpcForwardConn{ws: ws})
	defer client.Close()

	syncRequest := &sdk.DeviceRemoteSyncRequest{
		InstanceId: connect.Id(h.pdInstanceId),
		RpcVersion: rpcVersion,
	}
	// a cached remote write, as a DeviceRemote made while disconnected; a
	// rejected sync must never apply it
	syncRequest.State.Offline.Set(offline)

	var syncResponse *sdk.DeviceRemoteSyncResponse
	err = client.Call("DeviceLocalRpc.Sync", syncRequest, &syncResponse)
	connect.AssertEqual(t, err, nil)
	if syncResponse == nil {
		t.Fatal("sync returned no response")
	}
	return syncResponse
}

// TestProxyDeviceRpcVersionGuard proves the DeviceRpcVersion guard fires on the
// hosted/browser path, in both directions:
//
//   - a remote from an incompatible build is rejected with the distinguishable
//     "device rpc version mismatch:" error, BEFORE any of its state is applied
//   - a remote that predates the version field (RpcVersion 0 on the wire)
//     still syncs — shipping the guard must not break already-cached wasm
//   - the production pairing (RpcVersion == sdk.DeviceRpcVersion, what
//     NewPlatformDeviceRemote sends) syncs normally and applies state, so the
//     guard cannot silently break the ordinary path and the state assertions
//     above are the guard at work, not a dead channel
func TestProxyDeviceRpcVersionGuard(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	// The caller already controls repetition with go test -count. Retrying an
	// environment/setup failure only hides the missing local-test prerequisite
	// behind four 15-second backoffs, and this control-plane exchange has no
	// external provider timing to retry.
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		opts := defaultProxyTestOptions()
		opts.enableDeviceRpc = true
		// the guard is decided before any data flows; a live provider and a
		// usable egress path are irrelevant to it
		opts.controlPlaneOnly = true
		h := setupProxyTestWithOptions(t, opts)
		defer h.close(t)

		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		connect.AssertEqual(t, err, nil)
		hosted := pd.deviceLocal

		// ---- an incompatible remote is rejected before any state applies ----
		offlineBefore := hosted.GetOffline()
		syncResponse := syncHostedDeviceRpc(t, h, sdk.DeviceRpcVersion+1, !offlineBefore)

		// the rejection is distinguishable (an app can tell it from an instance
		// mismatch or a generic failure) and names both versions
		if !strings.HasPrefix(syncResponse.Error, "device rpc version mismatch:") {
			t.Fatalf("expected a version mismatch error, got %q", syncResponse.Error)
		}
		if !strings.Contains(syncResponse.Error, fmt.Sprintf("remote is %d", sdk.DeviceRpcVersion+1)) ||
			!strings.Contains(syncResponse.Error, fmt.Sprintf("local is %d", sdk.DeviceRpcVersion)) {
			t.Fatalf("expected the error to name both versions, got %q", syncResponse.Error)
		}
		// a rejection carries only the error, not a device generation
		connect.AssertEqual(t, syncResponse.DeviceGeneration, "")

		// rejected BEFORE the seeded offline write was applied
		connect.AssertEqual(t, hosted.GetOffline(), offlineBefore)

		// ---- a pre-guard remote (version 0 on the wire) still syncs ----------
		syncResponse = syncHostedDeviceRpc(t, h, 0, !offlineBefore)
		connect.AssertEqual(t, syncResponse.Error, "")
		connect.AssertEqual(t, syncResponse.DeviceGeneration, pd.deviceGeneration.String())
		// net/rpc returns only after DeviceLocalRpc.Sync applied the state.
		connect.AssertEqual(t, hosted.GetOffline(), !offlineBefore)

		// ---- the matching production version syncs normally ------------------
		syncResponse = syncHostedDeviceRpc(t, h, sdk.DeviceRpcVersion, offlineBefore)
		connect.AssertEqual(t, syncResponse.Error, "")
		connect.AssertEqual(t, syncResponse.DeviceGeneration, pd.deviceGeneration.String())
		connect.AssertEqual(t, hosted.GetOffline(), offlineBefore)
	})
}
