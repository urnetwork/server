package providertunnel

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"
)

// URL probes give DNS and read progress five seconds, and TCP connect and TLS
// handshake three seconds each; DNS and TCP connect are the resolver's
// (providerUrlPhaseSettings). The original total deadline remains the owner.
const (
	urlProbeTlsTimeout      = 3 * time.Second
	urlProbeReadIdleTimeout = 5 * time.Second
)

// The budgets of the phases a URL request's resolver bounds
// (providerUrlResolver), each from the start of its phase.
type providerUrlPhaseSettings struct {
	// Route waiting, every resolver wave and their jitter: five seconds.
	DnsTimeout time.Duration
	// The TCP connect to a resolved address: three seconds.
	TcpTimeout time.Duration
}

// A new value on every call.
func defaultProviderUrlPhaseSettings() *providerUrlPhaseSettings {
	return &providerUrlPhaseSettings{
		DnsTimeout: 5 * time.Second,
		TcpTimeout: 3 * time.Second,
	}
}

// The hold a tunnel's generated clients put on their first sends while the
// end-to-end session first establishes (CapabilityObservation.EstablishHold),
// given to the URL phase that makes those sends on top of its own budget: the
// tunnel's first DNS query, or a literal host's TCP connect when no query came
// first. A provider whose session seals releases the sends at the seal; one
// without the end-to-end responder only when the hold ends, so a phase bounded
// without it would leave that provider its budget less the hold, and a turn
// that measures post-quantum would fail for the measurement.
//
// Only the first claim gets it. The hold belongs to the session and ends for
// good once the session seals, fails to establish or reaches its deadline, and
// a URL turn's legs run one at a time, so later phases send unheld. A window
// client that replaces the first (KeepTunnelOnReplacement) holds its own first
// sends again, which is not given back. Safe for concurrent use.
type providerFirstSendsHold struct {
	hold    time.Duration
	claimed atomic.Bool
}

// The first-sends hold of a tunnel opened with observation; nil, which adds
// nothing, for one that holds no sends: every tunnel's but a post-quantum URL
// turn's.
func newProviderFirstSendsHold(observation *CapabilityObservation) *providerFirstSendsHold {
	if observation == nil || observation.EstablishHold <= 0 {
		return nil
	}
	return &providerFirstSendsHold{hold: observation.EstablishHold}
}

// The hold for the first phase to claim it, zero for every later one.
func (self *providerFirstSendsHold) claim() time.Duration {
	if self == nil || !self.claimed.CompareAndSwap(false, true) {
		return 0
	}
	return self.hold
}

func providerUrlPhaseContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if _, scoped := ctx.Value(providerUrlProbeKey{}).(providerUrlProbeTarget); scoped {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}

// Installed below TLS so net/http still receives an actual *tls.Conn and its
// authenticated connection state. Reads become bounded only after the owned
// handshake finishes, before the connection is published to net/http.
type providerUrlReadConn struct {
	net.Conn
	ownerDeadline time.Time
	idleTimeout   time.Duration
}

func (self *providerUrlReadConn) Read(p []byte) (int, error) {
	if self.idleTimeout > 0 {
		deadline := time.Now().Add(self.idleTimeout)
		if !self.ownerDeadline.IsZero() && self.ownerDeadline.Before(deadline) {
			deadline = self.ownerDeadline
		}
		if err := self.Conn.SetReadDeadline(deadline); err != nil {
			return 0, err
		}
	}
	n, err := self.Conn.Read(p)
	var timeout net.Error
	if self.idleTimeout > 0 && errors.As(err, &timeout) && timeout.Timeout() {
		// This non-reusing HTTP connection is already unusable. Close its raw
		// socket now rather than spending TLS's close-notify grace period on
		// the same stalled peer after the read budget has expired.
		self.Conn.Close()
	}
	return n, err
}
