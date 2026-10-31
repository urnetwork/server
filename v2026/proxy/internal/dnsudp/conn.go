// Package dnsudp adapts gVisor UDP error readiness for the proxy acceptance
// resolver. It is not used by the production proxy's forwarding path.
package dnsudp

import (
	"errors"
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Conn retains gonet's UDP framing, deadlines, errors and datagram behavior.
// A connected UDP endpoint publishes ICMP refusal using EventErr, whereas
// gonet.commonRead waits only for ReadableEvents. Forwarding that notification
// makes an already-blocked read recheck the endpoint's real pending error.
type Conn struct {
	*gonet.UDPConn
	queue      *waiter.Queue
	errorEntry waiter.Entry
	stop       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	closeErr   error
}

// Dial is the IPv4, connected, automatically bound UDP path used by the
// acceptance profile's DNS server. No packet, retry or timeout is added.
func Dial(s *stack.Stack, remote tcpip.FullAddress) (*Conn, error) {
	var queue waiter.Queue
	endpoint, err := s.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &queue)
	if err != nil {
		return nil, errors.New(err.String())
	}
	if err := endpoint.Connect(remote); err != nil {
		endpoint.Close()
		return nil, &net.OpError{
			Op: "connect", Net: "udp",
			Addr: &net.UDPAddr{IP: net.IP(remote.Addr.AsSlice()), Port: int(remote.Port)},
			Err:  errors.New(err.String()),
		}
	}
	return NewConn(&queue, endpoint), nil
}

// NewConn takes ownership of an endpoint and its queue. The separate notifier
// must not call queue.Notify recursively from a waiter callback: callbacks
// execute under the queue's RLock, and a pending writer could deadlock it.
func NewConn(queue *waiter.Queue, endpoint tcpip.Endpoint) *Conn {
	c := &Conn{UDPConn: gonet.NewUDPConn(queue, endpoint), queue: queue,
		stop: make(chan struct{}), done: make(chan struct{})}
	entry, errorsReady := waiter.NewChannelEntry(waiter.EventErr)
	c.errorEntry = entry
	queue.EventRegister(&c.errorEntry)
	go func() {
		defer close(c.done)
		for {
			// Bound close latency even if a peer keeps producing errors.
			select {
			case <-c.stop:
				return
			default:
			}
			select {
			case <-c.stop:
				return
			case <-errorsReady:
				queue.Notify(waiter.ReadableEvents)
			}
		}
	}()
	return c
}

// Close unblocks socket readers, joins the single notification owner and
// unregisters its waiter before returning. Concurrent/duplicate close is safe.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		close(c.stop)
		c.closeErr = c.UDPConn.Close()
		<-c.done
		c.queue.EventUnregister(&c.errorEntry)
	})
	return c.closeErr
}
