// Drives the api's per-address-family listener supervision without sockets.
// Each synthetic address is an in-process listener of net.Pipe connections,
// served by the production route assembly through the injected bind seam.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// documentation addresses; no socket is ever bound to them
const proxyApiTestIpv4 = "192.0.2.10"
const proxyApiTestIpv6 = "2001:db8::10"
const proxyApiTestHostPort = 7300

// One synthetic address. Accept returns the server ends of piped connections
// until Close, which is idempotent.
type proxyApiTestListener struct {
	addr      string
	conns     chan net.Conn
	closed    chan struct{}
	closeOnce sync.Once
}

func newProxyApiTestListener(addr string) *proxyApiTestListener {
	return &proxyApiTestListener{
		addr:   addr,
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
}

// `net.Listener`
func (self *proxyApiTestListener) Accept() (net.Conn, error) {
	select {
	case conn := <-self.conns:
		return conn, nil
	case <-self.closed:
		return nil, net.ErrClosed
	}
}

// `net.Listener`
func (self *proxyApiTestListener) Close() error {
	self.closeOnce.Do(func() {
		close(self.closed)
	})
	return nil
}

// `net.Listener`
func (self *proxyApiTestListener) Addr() net.Addr {
	return proxyApiTestAddr(self.addr)
}

// Hands the server end to Accept and returns the client end. A closed listener
// refuses, like a closed port.
func (self *proxyApiTestListener) dial(ctx context.Context) (net.Conn, error) {
	serverConn, clientConn := net.Pipe()
	select {
	case self.conns <- serverConn:
		return clientConn, nil
	case <-self.closed:
	case <-ctx.Done():
	}
	serverConn.Close()
	clientConn.Close()
	return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: proxyApiTestAddr(self.addr), Err: syscall.ECONNREFUSED}
}

// A synthetic tcp address.
type proxyApiTestAddr string

// `net.Addr`
func (self proxyApiTestAddr) Network() string {
	return "tcp"
}

// `net.Addr`
func (self proxyApiTestAddr) String() string {
	return string(self)
}

// The injected bind seam and the test client's dialer. Each bind of an address
// is recorded with its network, serves the production handler on an
// in-process listener and announces the address on served; an address with no
// serving listener refuses. Bind failures are fixed before construction. Safe
// for concurrent use.
type proxyApiTestNetwork struct {
	addrBindErrs map[string]error
	served       chan string

	stateLock         sync.Mutex
	addrListeners     map[string]*proxyApiTestListener
	addrServeCounts   map[string]int
	addrReturnedBools map[string]bool
	// "network addr" of every bind
	binds []string
}

func newProxyApiTestNetwork(addrBindErrs map[string]error) *proxyApiTestNetwork {
	return &proxyApiTestNetwork{
		addrBindErrs: addrBindErrs,
		// one announcement per address family
		served:            make(chan string, 2),
		addrListeners:     map[string]*proxyApiTestListener{},
		addrServeCounts:   map[string]int{},
		addrReturnedBools: map[string]bool{},
	}
}

// Has the contract of server.HttpListenAndServeTlsWithReusePort: a bind failure
// returns at once; otherwise it serves until ctx is canceled, then returns the
// join result. The pipes carry plain http; the tls selection is covered by the
// shared assembly test.
func (self *proxyApiTestNetwork) listenAndServeTls(
	ctx context.Context,
	network string,
	addr string,
	handler http.Handler,
	reusePort bool,
	httpServerOptions server.HttpServerOptions,
	tlsConfig *tls.Config,
) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addrServeCounts[addr] += 1
		self.binds = append(self.binds, network+" "+addr)
	}()
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addrReturnedBools[addr] = true
	}()

	if err, ok := self.addrBindErrs[addr]; ok {
		return err
	}

	listener := newProxyApiTestListener(addr)
	owner := server.NewTestHttpServer(ctx, listener, &http.Server{Handler: handler})
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.addrListeners[addr] = listener
	}()
	self.served <- addr

	<-ctx.Done()

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.addrListeners, addr)
	}()
	joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return owner.CloseAndWait(joinCtx)
}

// `http.Transport.DialContext`
func (self *proxyApiTestNetwork) dial(ctx context.Context, network string, addr string) (net.Conn, error) {
	listener := func() *proxyApiTestListener {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.addrListeners[addr]
	}()
	if listener == nil {
		return nil, &net.OpError{Op: "dial", Net: network, Addr: proxyApiTestAddr(addr), Err: syscall.ECONNREFUSED}
	}
	return listener.dial(ctx)
}

// Number of binds of addr.
func (self *proxyApiTestNetwork) serveCount(addr string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.addrServeCounts[addr]
}

// Whether the serve of addr has returned.
func (self *proxyApiTestNetwork) returned(addr string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.addrReturnedBools[addr]
}

// The "network addr" of every bind so far, sorted.
func (self *proxyApiTestNetwork) bindList() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Sorted(slices.Values(self.binds))
}

// Assigns the synthetic host addresses and the api's host port, as warp does
// for a block with both address families.
func proxyApiTestEnv(t *testing.T) (ipv4Addr string, ipv6Addr string) {
	t.Setenv("WARP_HOST_IPV4", proxyApiTestIpv4)
	t.Setenv("WARP_HOST_IPV6", proxyApiTestIpv6)
	t.Setenv("WARP_PORTS", fmt.Sprintf("%d:%d", InternalApiPort, proxyApiTestHostPort))
	hostPort := strconv.Itoa(proxyApiTestHostPort)
	return net.JoinHostPort(proxyApiTestIpv4, hostPort), net.JoinHostPort(proxyApiTestIpv6, hostPort)
}

// The ipv6 listener must accept and answer while the ipv4 listener is still
// serving. A serve returns only at shutdown, so starting the address families
// in sequence left the ipv6 address unserved for the life of the process.
func TestProxyApiServesIpv6WhileIpv4Serves(t *testing.T) {
	ipv4Addr, ipv6Addr := proxyApiTestEnv(t)
	bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopBound()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := newProxyApiTestNetwork(map[string]error{})
	api := newApiServer(
		ctx,
		cancel,
		&ProxyDeviceManager{},
		&server.TransportTls{},
		nil,
		InternalApiPort,
		DefaultProxySettings(),
		network.listenAndServeTls,
	)
	defer func() {
		cancel()
		joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := api.CloseAndWait(joinCtx); err != nil {
			t.Errorf("api listeners did not shut down cleanly: %v", err)
		}
		for _, addr := range []string{ipv4Addr, ipv6Addr} {
			if !network.returned(addr) {
				t.Errorf("%s serve outlived its joined listener worker", addr)
			}
			if count := network.serveCount(addr); count != 1 {
				t.Errorf("%s bound %d times, want 1", addr, count)
			}
		}
	}()

	servedAddrBools := map[string]bool{}
	for len(servedAddrBools) < 2 {
		select {
		case addr := <-network.served:
			servedAddrBools[addr] = true
		case <-bound.Done():
			t.Fatalf(
				"served %v: ipv4 serve returned=%t, ipv6 binds=%d; the ipv6 listener must start while the ipv4 listener serves",
				servedAddrBools,
				network.returned(ipv4Addr),
				network.serveCount(ipv6Addr),
			)
		}
	}

	transport := &http.Transport{DialContext: network.dial, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	// the production routes answer an unauthenticated warmup without a device
	warmupStatus := func(addr string) (int, error) {
		request, err := http.NewRequestWithContext(bound, http.MethodPost, "http://"+addr+"/warmup", nil)
		if err != nil {
			return 0, err
		}
		response, err := client.Do(request)
		if err != nil {
			return 0, err
		}
		defer response.Body.Close()
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			return 0, err
		}
		return response.StatusCode, nil
	}
	for _, addr := range []string{ipv6Addr, ipv4Addr} {
		status, err := warmupStatus(addr)
		if err != nil {
			t.Fatalf("%s did not accept while the other family served: %v", addr, err)
		}
		if status != http.StatusUnauthorized {
			t.Errorf("%s warmup = %d, want %d from the production routes", addr, status, http.StatusUnauthorized)
		}
	}
	for _, addr := range []string{ipv4Addr, ipv6Addr} {
		if network.returned(addr) {
			t.Errorf("%s stopped serving before shutdown", addr)
		}
	}
}

// A bind failure of either address family cancels the process context, as for
// the socks/http ingress, and stays reported through repeated CloseAndWait
// after both workers join. Before, an ipv6 failure could only surface at
// shutdown, after the ipv4 serve returned.
func TestProxyApiListenerFailureCancelsAndIsRetained(t *testing.T) {
	ipv4Addr, ipv6Addr := proxyApiTestEnv(t)
	for _, failAddr := range []string{ipv6Addr, ipv4Addr} {
		func() {
			bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopBound()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			bindErr := fmt.Errorf("synthetic bind failure on %s: %w", failAddr, syscall.EADDRINUSE)
			network := newProxyApiTestNetwork(map[string]error{failAddr: bindErr})
			api := newApiServer(
				ctx,
				cancel,
				&ProxyDeviceManager{},
				&server.TransportTls{},
				nil,
				InternalApiPort,
				DefaultProxySettings(),
				network.listenAndServeTls,
			)
			defer func() {
				cancel()
				joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = api.CloseAndWait(joinCtx)
			}()

			select {
			case <-ctx.Done():
			case <-bound.Done():
				t.Errorf(
					"%s bind failure did not cancel the process: ipv4 serve returned=%t, ipv6 binds=%d",
					failAddr,
					network.returned(ipv4Addr),
					network.serveCount(ipv6Addr),
				)
				return
			}
			for i := 0; i < 2; i++ {
				err := api.CloseAndWait(bound)
				if !errors.Is(err, bindErr) {
					t.Errorf("%s close %d lost the bind failure: %v", failAddr, i, err)
				}
				if errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("%s close %d did not join the listener workers: %v", failAddr, i, err)
				}
			}
			for _, addr := range []string{ipv4Addr, ipv6Addr} {
				if !network.returned(addr) {
					t.Errorf("%s serve outlived its joined listener worker", addr)
				}
				if count := network.serveCount(addr); count != 1 {
					t.Errorf("%s bound %d times, want 1", addr, count)
				}
			}
		}()
	}
}

// Each api address binds with the network of its family, as the socks/http
// ingress does, and a family warp leaves empty is not bound. With "tcp", the
// empty ipv4 host of an ipv6-only block was a dual-stack [::] wildcard on the
// port of the ipv6 listener, and on Linux one of the two binds failed with
// EADDRINUSE, which exits the process at startup.
func TestProxyApiBindsEachAddressWithItsFamily(t *testing.T) {
	hostPort := strconv.Itoa(proxyApiTestHostPort)
	ipv4Bind := "tcp4 " + net.JoinHostPort(proxyApiTestIpv4, hostPort)
	ipv6Bind := "tcp6 " + net.JoinHostPort(proxyApiTestIpv6, hostPort)
	for _, c := range []struct {
		name     string
		hostIpv4 string
		hostIpv6 string
		binds    []string
	}{
		{name: "dual stack", hostIpv4: proxyApiTestIpv4, hostIpv6: proxyApiTestIpv6, binds: []string{ipv4Bind, ipv6Bind}},
		// warp exports WARP_HOST_IPV4="" for a network without ipv4
		{name: "ipv6 only", hostIpv4: "", hostIpv6: proxyApiTestIpv6, binds: []string{ipv6Bind}},
		{name: "ipv4 only", hostIpv4: proxyApiTestIpv4, hostIpv6: "", binds: []string{ipv4Bind}},
		// without warp's host addresses, the service port on the ipv4
		// wildcard, as for the socks/http ingress
		{name: "no host addresses", binds: []string{"tcp4 " + net.JoinHostPort("0.0.0.0", strconv.Itoa(InternalApiPort))}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("WARP_HOST_IPV4", c.hostIpv4)
			t.Setenv("WARP_HOST_IPV6", c.hostIpv6)
			t.Setenv("WARP_PORTS", fmt.Sprintf("%d:%d", InternalApiPort, proxyApiTestHostPort))
			bound, stopBound := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopBound()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			network := newProxyApiTestNetwork(map[string]error{})
			api := newApiServer(
				ctx,
				cancel,
				&ProxyDeviceManager{},
				&server.TransportTls{},
				nil,
				InternalApiPort,
				DefaultProxySettings(),
				network.listenAndServeTls,
			)
			defer func() {
				cancel()
				joinCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				_ = api.CloseAndWait(joinCtx)
			}()

			for served := 0; served < len(c.binds); served++ {
				select {
				case <-network.served:
				case <-bound.Done():
					t.Fatalf("%d of %d listeners serving, binds %q", served, len(c.binds), network.bindList())
				}
			}
			// each listener worker binds before it can join, so once the
			// workers have joined, the recorded binds are all the api made
			cancel()
			if err := api.CloseAndWait(bound); err != nil {
				t.Fatalf("api listeners did not shut down cleanly: %v", err)
			}
			if binds, want := network.bindList(), slices.Sorted(slices.Values(c.binds)); !slices.Equal(binds, want) {
				t.Errorf("binds %q, want %q", binds, want)
			}
		})
	}
}
