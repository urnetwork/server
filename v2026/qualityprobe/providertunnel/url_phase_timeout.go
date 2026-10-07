package providertunnel

import (
	"context"
	"errors"
	"net"
	"time"
)

// URL probes give DNS and read progress five seconds, and TCP connect and TLS
// handshake three seconds each. The original total deadline remains the owner.
const (
	urlProbeDnsTimeout      = 5 * time.Second
	urlProbeTcpTimeout      = 3 * time.Second
	urlProbeTlsTimeout      = 3 * time.Second
	urlProbeReadIdleTimeout = 5 * time.Second
)

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
