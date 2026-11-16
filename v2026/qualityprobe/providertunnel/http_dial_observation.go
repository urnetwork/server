// The provider HTTP owner translates only its target tunnel's positive
// progress. Private DoH transports and late losing dials cannot change it.
package providertunnel

import (
	"context"
	"net/http/httptrace"
	"sync"

	"github.com/urnetwork/connect/v2026"
)

// Callback admission and close share one owner. Trace callbacks execute outside
// its lock, and close joins every admitted callback before classifying an error.
type providerHttpDialTrace struct {
	trace            *httptrace.ClientTrace
	network, address string
	stateLock        sync.Mutex
	phase            connect.TunDialEvent
	closed           bool
	callbacks        sync.WaitGroup
}

// A dedicated tunnel observer crosses Tun's lifetime join without lending the
// target HTTP trace to resolver-internal HTTP/TCP work.
func traceProviderHttpDial(ctx context.Context, network, address string) (context.Context, *providerHttpDialTrace) {
	owner := &providerHttpDialTrace{trace: httptrace.ContextClientTrace(ctx), network: network, address: address}
	return connect.WithTunDialObserver(ctx, owner.observe), owner
}

// Progress is monotonic across concurrent families and tunnel dial races.
func (self *providerHttpDialTrace) observe(event connect.TunDialEvent) {
	if event < connect.TunDialDnsStarted || connect.TunDialTcpConnected < event {
		return
	}
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed || event <= self.phase {
			return false
		}
		self.phase = event
		self.callbacks.Add(1)
		return true
	}()
	if !admitted {
		return
	}
	defer self.callbacks.Done()
	if self.trace == nil {
		return
	}
	switch event {
	case connect.TunDialDnsStarted:
		if self.trace.DNSStart != nil {
			self.trace.DNSStart(httptrace.DNSStartInfo{})
		}
	case connect.TunDialDnsAnswered:
		if self.trace.DNSDone != nil {
			self.trace.DNSDone(httptrace.DNSDoneInfo{})
		}
	case connect.TunDialTcpStarted:
		if self.trace.ConnectStart != nil {
			self.trace.ConnectStart(self.network, self.address)
		}
	case connect.TunDialTcpConnected:
		if self.trace.ConnectDone != nil {
			self.trace.ConnectDone(self.network, self.address, nil)
		}
	}
}

// Unknown custom dialers retain the combined class. A completed DNS answer
// without a launched TCP attempt is also ambiguous, not a DNS timeout. The
// target TCP class is furthest positive progress, not a claim that a pending
// second DNS family cannot also contribute to the final failure.
func (self *providerHttpDialTrace) finish(err error) string {
	phase := func() connect.TunDialEvent {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
		return self.phase
	}()
	self.callbacks.Wait()
	if self.trace != nil {
		switch {
		case phase == 0 && err == nil:
			// Success proves a connection even for an uninstrumented dialer.
			if self.trace.ConnectStart != nil {
				self.trace.ConnectStart(self.network, self.address)
			}
			if self.trace.ConnectDone != nil {
				self.trace.ConnectDone(self.network, self.address, nil)
			}
		case phase == connect.TunDialDnsStarted && err != nil:
			if self.trace.DNSDone != nil {
				self.trace.DNSDone(httptrace.DNSDoneInfo{Err: err})
			}
		case phase == connect.TunDialTcpStarted && err != nil:
			if self.trace.ConnectDone != nil {
				self.trace.ConnectDone(self.network, self.address, err)
			}
		}
	}
	switch phase {
	case connect.TunDialDnsStarted:
		return "dial_dns"
	case connect.TunDialTcpStarted, connect.TunDialTcpConnected:
		return "dial_tcp"
	default:
		return "dial_dns_or_socket"
	}
}
