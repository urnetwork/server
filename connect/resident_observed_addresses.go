package connect

// A client's observed addresses: the addresses its transports connect from,
// as each edge resolved them behind the ingress (ResidentTransportEdge
// ClientIp). The resident is the one place that holds every transport of a
// client, so it sends the client the set, as one `ObservedAddresses` from the
// control id, whenever the set changes. A provider counts each public one as
// its own address (connect ip_security_local.go): behind a NAT it is the
// router's public address, and in a container the host's, neither on an
// interface the provider can read, so without it a consumer could reach the
// router by hairpin NAT. The client validates the addresses and keeps at most
// its own bound of them; the resident sends what the edges saw.
//
// Every client gets the message, provider or not, so a client that starts
// providing later already has its addresses. A client older than the message
// type ignores it, as it ignores `CloseSequences`, which residents have sent to
// every client since 2026-10-28; nothing is gated on a client version.

import (
	"context"
	"net/netip"
	"slices"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
)

var residentObservedAddressesSent = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_resident_observed_addresses_sent_total",
	Help: "ObservedAddresses frames residents sent to their clients",
})

func init() {
	prometheus.MustRegister(residentObservedAddressesSent)
}

// residentObservedAddressesListener sends a resident's client the addresses
// of its transports on every change of the set. Kicks coalesce while a send
// runs, so attaching or closing a transport never waits on it.
type residentObservedAddressesListener struct {
	ctx    context.Context
	cancel context.CancelFunc
	// the resident's current set (Resident.observedAddresses)
	addresses func() []netip.Addr
	// takes ownership of the frame (Resident.sendListenerFrame)
	send func(*protocol.Frame)
	kick <-chan struct{}
	done chan struct{}
}

// Starts the resident-owned worker. kick is the resident's coalesced change
// signal; a kick that arrived before the start is kept by its buffer.
func newResidentObservedAddressesListener(
	ctx context.Context,
	addresses func() []netip.Addr,
	send func(*protocol.Frame),
	kick <-chan struct{},
) *residentObservedAddressesListener {
	cancelCtx, cancel := context.WithCancel(ctx)
	listener := &residentObservedAddressesListener{
		ctx:       cancelCtx,
		cancel:    cancel,
		addresses: addresses,
		send:      send,
		kick:      kick,
		done:      make(chan struct{}),
	}
	go server.HandleError(listener.run)
	return listener
}

// Sends the set once per change. An empty set is not sent: the client then
// has no transport to receive it. The last sent set is forgotten with it, so
// a returning address is sent again.
func (self *residentObservedAddressesListener) run() {
	defer close(self.done)
	var sentAddresses []netip.Addr
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.kick:
		}
		addresses := self.addresses()
		if slices.Equal(addresses, sentAddresses) {
			continue
		}
		sentAddresses = addresses
		if len(addresses) == 0 {
			continue
		}
		observedAddresses := &protocol.ObservedAddresses{}
		for _, addr := range addresses {
			observedAddresses.Addresses = append(observedAddresses.Addresses, addr.AsSlice())
		}
		self.send(connect.RequireToFrameWithDefaultProtocolVersion(observedAddresses))
		residentObservedAddressesSent.Inc()
	}
}

// Joins the worker after withdrawing its send context.
func (self *residentObservedAddressesListener) CloseAndWait() {
	self.cancel()
	<-self.done
}

// The distinct client addresses of the resident's transports, sorted, at most
// ExchangeSettings.ObservedAddressesMaxCount of them. A transport whose edge
// resolved no address adds nothing.
func (self *Resident) observedAddresses() []netip.Addr {
	maxCount := self.exchange.settings.ObservedAddressesMaxCount
	if maxCount <= 0 {
		maxCount = DefaultExchangeSettings().ObservedAddressesMaxCount
	}
	addresses := []netip.Addr{}
	func() {
		self.stateLock.RLock()
		defer self.stateLock.RUnlock()
		for transport := range self.transports {
			if clientIp := transport.edge.ClientIp; clientIp.IsValid() && !slices.Contains(addresses, clientIp) {
				addresses = append(addresses, clientIp)
			}
		}
	}()
	slices.SortFunc(addresses, func(a netip.Addr, b netip.Addr) int {
		return a.Compare(b)
	})
	if maxCount < len(addresses) {
		addresses = addresses[:maxCount]
	}
	return addresses
}

// Wakes the observed addresses listener without blocking. A resident built
// without the signal (a test fixture) skips it.
func (self *Resident) kickObservedAddresses() {
	select {
	case self.observedAddressesKick <- struct{}{}:
	default:
	}
}
