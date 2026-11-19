package connect

// The tunables of connect's lb_alt front (LBALT.md §4.3, §4.4, D10, D12,
// D22; LBALT-IMPL.md §2.3, §2.8): the public sockets, their supervision and
// the drain. The nginx front's own tunables are nested rather than copied, so
// the public HTTP and h3 servers here and the handlers the nginx front builds
// read one set of client timeouts, header limits and drain bounds.

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/nginx"
)

// How connect binds, supervises and drains its public sockets. Read-only once
// the front server is built.
type FrontServerSettings struct {
	// the nginx front's settings, passed to nginx.NewFront. Http also bounds
	// the public servers here: ClientHeaderTimeout, KeepaliveTimeout and the
	// header limits on every HTTP server, DrainTimeout on the drain, and
	// ForcedCloseTimeout on each step of the forced close that ends it.
	Front *nginx.FrontSettings
	// SO_REUSEPORT sockets bound for each (interface address, protocol,
	// port), for receive parallelism within the block: N = 4 (D10, confirmed
	// by the owner 2026-11-18). warpctl promotes a generation only with every
	// one of them up. Non-positive is refused at start.
	SocketsPerPort int
	// the wait before re-creating a lost socket once the previous attempt
	// failed or the re-created socket was lost again before
	// SocketHealthyRunTimeout. The wait doubles from here up to
	// SocketRestartMaxDelay. The first re-creation after a healthy run is at
	// once (D22). 100 ms, as connect's trusted listeners start. Non-positive
	// is refused at start, since a socket that fails at once would otherwise
	// spin.
	SocketRestartInitialDelay time.Duration
	// the longest wait between re-creations of one socket: 5 s, as connect's
	// trusted listeners. A value under SocketRestartInitialDelay counts as
	// SocketRestartInitialDelay.
	SocketRestartMaxDelay time.Duration
	// how long a socket must serve for its loss to reset the backoff, so the
	// loss is re-created at once. One minute: a socket lost sooner is failing
	// repeatedly, and its re-creations back off. Non-positive never resets the
	// backoff, so only a socket's first loss is re-created at once.
	SocketHealthyRunTimeout time.Duration
	// binds one public udp socket, when set, in place of
	// listenFrontPacket's SO_REUSEPORT bind to the exact address. Tests use
	// it to observe, hold and fail binds, as the extender's
	// ExtenderSettings.ListenPacket. The front owns and closes what it
	// returns.
	ListenPacket func(network string, address string) (net.PacketConn, error)
	// binds one public tcp listener, when set, in place of listenFront's
	// SO_REUSEPORT bind to the exact address, as ListenPacket. The front owns
	// and closes what it returns.
	Listen func(network string, address string) (net.Listener, error)

	// Nil outside package tests. Called with each wait before a re-creation
	// attempt, a zero wait included, on the supervising goroutine.
	restartWaitObserverForTest func(key frontSocketKey, wait time.Duration)
	// Nil outside package tests. Called with the name and class an accepted
	// QUIC connection was dispatched by, before its front is entered.
	dispatchObserverForTest func(serverName string, nameClass nginx.NameClass)
}

// A new instance on every call, with its own nginx front defaults.
func DefaultFrontServerSettings() *FrontServerSettings {
	return &FrontServerSettings{
		Front:                     nginx.DefaultFrontSettings(),
		SocketsPerPort:            4,
		SocketRestartInitialDelay: 100 * time.Millisecond,
		SocketRestartMaxDelay:     5 * time.Second,
		SocketHealthyRunTimeout:   1 * time.Minute,
	}
}

// Refuses settings the front can't run with.
func (self *FrontServerSettings) validate() error {
	if self.Front == nil || self.Front.Http == nil {
		return errors.New("front settings have no nginx http settings")
	}
	if self.SocketsPerPort <= 0 {
		return errors.New("front settings need at least one socket per port")
	}
	if self.SocketRestartInitialDelay <= 0 {
		return errors.New("front settings need a positive socket restart delay")
	}
	return nil
}

// The wait after wait, under the settings' backoff: the initial delay after
// none, then doubling up to the maximum.
func (self *FrontServerSettings) nextRestartWait(wait time.Duration) time.Duration {
	if wait <= 0 {
		return self.SocketRestartInitialDelay
	}
	maxDelay := max(self.SocketRestartMaxDelay, self.SocketRestartInitialDelay)
	return min(2*wait, maxDelay)
}

// Binds one public udp socket to its exact interface address, in the block's
// SO_REUSEPORT group for that address and port (D10).
func listenFrontPacket(network string, address string) (net.PacketConn, error) {
	listenConfig := net.ListenConfig{
		Control: server.SoReusePort,
	}
	return listenConfig.ListenPacket(context.Background(), network, address)
}

// Binds one public tcp listener to its exact interface address, in the
// block's SO_REUSEPORT group for that address and port (D10).
func listenFront(network string, address string) (net.Listener, error) {
	listenConfig := net.ListenConfig{
		Control: server.SoReusePort,
	}
	return listenConfig.Listen(context.Background(), network, address)
}
