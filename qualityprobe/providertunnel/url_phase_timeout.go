package providertunnel

import (
	"context"
	"errors"
	"net"
	"time"
)

// URL probes accept a negative result after fifteen seconds in any network
// phase. The request's original total deadline remains the outer owner.
const urlProbePhaseTimeout = 15 * time.Second

func providerUrlPhaseContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, scoped := ctx.Value(providerUrlProbeKey{}).(providerUrlProbeTarget); scoped {
		return context.WithTimeout(ctx, urlProbePhaseTimeout)
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
