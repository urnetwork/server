// Connection admission is shared by both Unix authority sockets. Rejected
// peers never acquire a goroutine, body buffer or upstream connection.
package competitionagent

import (
	"net"
	"sync"
)

// One instance budget spans worker HTTP and the real CLI's build sessions.
type dockerProxyLimitedListener struct {
	net.Listener
	slots chan struct{}
}

// Refusal is immediate when the management owner's fixed descriptor budget is
// occupied; healthy accepted peers retain their independent lifetimes.
func (self *dockerProxyLimitedListener) Accept() (net.Conn, error) {
	for {
		connection, err := self.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case self.slots <- struct{}{}:
			return &dockerProxyLimitedConn{Conn: connection, release: func() { <-self.slots }}, nil
		default:
			_ = connection.Close()
		}
	}
}

// A hijacked connection retains its admission until its actual protocol owner
// closes it, not merely until the original HTTP handler loses ordinary custody.
type dockerProxyLimitedConn struct {
	net.Conn
	closeOnce sync.Once
	release   func()
}

// A duplicate close cannot return another live connection's admission.
func (self *dockerProxyLimitedConn) Close() error {
	var err error
	self.closeOnce.Do(func() { err = self.Conn.Close(); self.release() })
	return err
}

// Operating-system peer credentials refer to the original accepted Unix peer.
func dockerProxyOriginalConn(connection net.Conn) net.Conn {
	for {
		if limited, ok := connection.(*dockerProxyLimitedConn); ok {
			connection = limited.Conn
			continue
		}
		return connection
	}
}
