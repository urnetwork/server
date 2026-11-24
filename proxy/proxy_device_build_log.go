package proxy

// The build line: one line for each device construction that succeeds, logged
// by the opener that ran the construction.
//
//	[pd][<proxy id>]build open=<cold|reopen> path=<path> caller=<hash>
//
// `open` is the construction's kind (setup_metrics.go) and `path` the entry
// point whose open started it. `caller` is the first 8 bytes of
// server.ClientIpHashForAddr of the caller's address, in hex: the peppered
// hash of its /29 (IPv4) or /56 (IPv6), never the address. It is the key of
// the caller refusal lines (checkCallerWithContext) and of the api's sign-up
// address buckets, so a build joins both. A path with no caller address logs
// caller=none.
//
// An open carries its caller in its context (withProxyDeviceCaller), and the
// opener that becomes the creator keeps it on its construction
// (deviceCreation). An opener that joins a construction already in flight
// logs nothing, so the lines count constructions, not opens. The line took the
// place of a per-construction line that named the api and platform urls, so
// the log carries as many lines as before.

import (
	"context"
	"encoding/hex"
	"net/netip"

	"github.com/urnetwork/server"
)

// The values of the build line's `path`.
const (
	// an HTTP CONNECT or forwarded request
	proxyDevicePathHttp = "http"
	// a SOCKS CONNECT
	proxyDevicePathSocks = "socks"
	// a SOCKS sign-in, which starts the device ahead of the session's first dial
	proxyDevicePathSocksSignIn = "socks-signin"
	// the first packet of a WireGuard session. The WireGuard device learns the
	// peer's endpoint, and the tun factory is never told it, so this path has
	// no caller address.
	proxyDevicePathWg = "wg"
	// the control api's /warmup
	proxyDevicePathWarmup = "warmup"
	// a device-rpc session
	proxyDevicePathRpc = "rpc"
	// a replacement instance's pre-warm of the proxies active on its block
	proxyDevicePathActivity = "activity"
	// the control api's flow trace
	proxyDevicePathFlowTrace = "flow-trace"
	// an open whose context names no caller
	proxyDevicePathUnknown = "unknown"
)

// The caller of a device open.
type proxyDeviceCaller struct {
	// the entry point, a proxyDevicePath value
	path string
	// the caller's address; invalid on a path that has none
	addr netip.Addr
}

type proxyDeviceCallerContextKey struct{}

// ctx, carrying the caller to the construction its open may start.
func withProxyDeviceCaller(ctx context.Context, caller proxyDeviceCaller) context.Context {
	return context.WithValue(ctx, proxyDeviceCallerContextKey{}, caller)
}

// The caller ctx carries, or the unknown path with no address.
func proxyDeviceCallerFromContext(ctx context.Context) proxyDeviceCaller {
	if caller, ok := ctx.Value(proxyDeviceCallerContextKey{}).(proxyDeviceCaller); ok {
		return caller
	}
	return proxyDeviceCaller{path: proxyDevicePathUnknown}
}

// The caller hash a log line prints: the first 8 bytes of the address's
// peppered bucket hash, in hex, or "none" for no address. A v4-mapped address
// hashes as its IPv4 address, as the api's sign-up buckets do
// (server.RateLimitClient), so a dual-stack listener's IPv4 caller lands in
// the same bucket.
func proxyCallerHash(addr netip.Addr) string {
	if !addr.IsValid() {
		return "none"
	}
	callerHash := server.ClientIpHashForAddr(addr.Unmap())
	return hex.EncodeToString(callerHash[:8])
}

// The address of a remote "host:port", invalid when it does not parse.
func proxyCallerAddr(remoteAddr string) netip.Addr {
	addrPort, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	return addrPort.Addr()
}

// Logs the build line of a construction that succeeded, once its open is
// recorded (proxyDeviceSetup.built).
func (self *ProxyDeviceManager) logBuild(proxyId server.Id, pd *ProxyDevice, caller proxyDeviceCaller) {
	openKind := proxyOpenReopen
	if open := pd.setup.open.Load(); open != nil {
		openKind = open.kind
	}
	self.buildLogf(
		"[pd][%s]build open=%s path=%s caller=%s\n",
		proxyId,
		openKind,
		caller.path,
		proxyCallerHash(caller.addr),
	)
}
