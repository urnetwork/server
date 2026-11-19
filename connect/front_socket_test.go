package connect

// The front's socket inventory and its supervision (D10, D13, D22).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The front's readiness headers with the latch ok and the rest of connect
// ready, so they read what the sockets and the drain decide.
func (self *testFrontEnv) statusHeaders() http.Header {
	header := http.Header{}
	self.frontServer.writeStatusHeaders(header, frontReadyLatchStatus, true)
	return header
}

// The X-UR-Front-Sockets entry of one group, "<group>=<up>/<expected>", or
// "" when the header has none.
func testFrontSocketsEntry(header http.Header, groupName string) string {
	for _, entry := range strings.Split(header.Get(frontSocketsHeader), ",") {
		if strings.HasPrefix(entry, groupName+"=") {
			return entry
		}
	}
	return ""
}

// Records each wait the supervisors observe before a re-creation, by socket.
type testFrontRestartWaits struct {
	stateLock sync.Mutex
	waits     map[frontSocketKey][]time.Duration
}

// An empty record.
func newTestFrontRestartWaits() *testFrontRestartWaits {
	return &testFrontRestartWaits{
		waits: map[frontSocketKey][]time.Duration{},
	}
}

// FrontServerSettings.restartWaitObserverForTest
func (self *testFrontRestartWaits) observe(key frontSocketKey, wait time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.waits[key] = append(self.waits[key], wait)
}

// The waits observed for one socket, in order.
func (self *testFrontRestartWaits) of(key frontSocketKey) []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.waits[key])
}

// The inventory key of the socket bound index-th on a group.
func (self *testFrontEnv) socketKey(address netip.Addr, portKind string, index int) frontSocketKey {
	self.t.Helper()
	for _, socket := range self.frontServer.sockets {
		if socket.key.address == address && socket.key.portKind == portKind && socket.key.index == index {
			return socket.key
		}
	}
	self.t.Fatalf("no socket %s %s #%d", address, portKind, index)
	return frontSocketKey{}
}

// Every interface address of the route table gets SocketsPerPort sockets of
// each public port, each bound by its family's network to the exact address,
// never a wildcard, and readiness counts them per protocol, address and port.
func TestFrontSocketInventoryBindsEveryAddressProtocolAndPort(t *testing.T) {
	const socketsPerPort = 4
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketsPerPort = socketsPerPort
	})
	routeListen := env.routeTable.Listen

	expectedAddresses := []string{}
	expectedEntries := []string{}
	addressCount := 0
	for _, routeInterface := range env.routeTable.Interfaces {
		for _, family := range []string{frontFamilyIpv4, frontFamilyIpv6} {
			address, ok := env.address(routeInterface.Name, family)
			if !ok {
				continue
			}
			addressCount += 1
			familyDigit := "4"
			if family == frontFamilyIpv6 {
				familyDigit = "6"
			}
			// the https and quic ports are one number, bound once per protocol
			httpAddress := env.bindAddress(address, frontPortHttp)
			sharedAddress := env.bindAddress(address, frontPortHttps)
			whodisAddress := env.bindAddress(address, frontPortWhodis)
			expectedAddresses = append(expectedAddresses, httpAddress, sharedAddress, whodisAddress)
			cases := []struct {
				bindAddress string
				networks    []string
			}{
				{bindAddress: httpAddress, networks: slices.Repeat([]string{"tcp" + familyDigit}, socketsPerPort)},
				{bindAddress: sharedAddress, networks: append(slices.Repeat([]string{"tcp" + familyDigit}, socketsPerPort), slices.Repeat([]string{"udp" + familyDigit}, socketsPerPort)...)},
				{bindAddress: whodisAddress, networks: slices.Repeat([]string{"udp" + familyDigit}, socketsPerPort)},
			}
			for _, c := range cases {
				networks := env.binds.bindNetworks(c.bindAddress)
				slices.Sort(networks)
				if !slices.Equal(networks, c.networks) {
					t.Errorf("binds of %s = %v, want %v", c.bindAddress, networks, c.networks)
				}
				for _, listener := range env.binds.boundListeners(c.bindAddress) {
					if got := listener.Addr().String(); got != c.bindAddress {
						t.Errorf("a listener for %s is bound to %s", c.bindAddress, got)
					}
				}
				for _, packetConn := range env.binds.boundPacketConns(c.bindAddress) {
					if got := packetConn.LocalAddr().String(); got != c.bindAddress {
						t.Errorf("a udp socket for %s is bound to %s", c.bindAddress, got)
					}
				}
			}
			for _, group := range []struct {
				protocol string
				port     int
			}{
				{protocol: frontProtocolTcp, port: routeListen.HttpPort},
				{protocol: frontProtocolTcp, port: routeListen.HttpsPort},
				{protocol: frontProtocolUdp, port: routeListen.QuicPort},
				{protocol: frontProtocolUdp, port: routeListen.WhodisPort},
			} {
				expectedEntries = append(expectedEntries, fmt.Sprintf("%s/%s/%d=%d/%d", group.protocol, address, group.port, socketsPerPort, socketsPerPort))
			}
		}
	}
	slices.Sort(expectedAddresses)
	if addresses := env.binds.bindAddresses(); !slices.Equal(addresses, expectedAddresses) {
		t.Fatalf("bound addresses = %v, want exactly %v", addresses, expectedAddresses)
	}
	if count := len(env.frontServer.sockets); count != addressCount*4*socketsPerPort {
		t.Fatalf("%d sockets for %d addresses, want %d", count, addressCount, addressCount*4*socketsPerPort)
	}

	env.waitSocketsUp(len(env.frontServer.sockets))
	header := env.statusHeaders()
	slices.Sort(expectedEntries)
	if got := header.Get(frontSocketsHeader); got != strings.Join(expectedEntries, ",") {
		t.Fatalf("%s = %q, want %q", frontSocketsHeader, got, strings.Join(expectedEntries, ","))
	}
	if got := header.Get(frontReadyHeader); got != "1" {
		t.Fatalf("%s = %q with every socket up", frontReadyHeader, got)
	}
}

// A bind that fails at start fails the start, and releases every socket the
// start had bound.
func TestFrontStartFailsOnABindFailure(t *testing.T) {
	env := newUnstartedTestFrontEnv(t, nil)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	failedAddress := env.bindAddress(address, frontPortWhodis)
	env.binds.failNext(failedAddress, 1)
	if err := env.frontServer.start(env.ctx, env.front, env.connectQuic); err == nil {
		t.Fatal("the start succeeded with a socket it could not bind")
	}
	if env.frontServer.isStarted() {
		t.Fatal("the front reads as started")
	}
	if len(env.binds.boundListeners(env.bindAddress(address, frontPortHttp))) == 0 {
		t.Fatal("the start failed before it bound anything")
	}
	// every socket bound before the failure was released: each port binds
	// again without SO_REUSEPORT, which no member of a reuseport group allows
	routeListen := env.routeTable.Listen
	for _, port := range []int{routeListen.HttpPort, routeListen.HttpsPort, routeListen.WhodisPort} {
		if !testFrontPortFree([]netip.Addr{address}, port) {
			t.Fatalf("port %d is still bound after the failed start", port)
		}
	}
}

// A udp socket lost while serving is re-created at once, without waiting
// out the restart delay (D22): its group reads 3/4 and the front not ready
// until it is back, while its siblings keep accepting new connections and
// are never re-bound.
func TestFrontRecreatesALostUdpSocketAtOnce(t *testing.T) {
	restartWaits := newTestFrontRestartWaits()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		// a re-creation that waited would never come within the test
		env.settings.SocketRestartInitialDelay = time.Hour
		env.settings.restartWaitObserverForTest = restartWaits.observe
	})
	socketCount := len(env.frontServer.sockets)
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortQuic)
	groupName := fmt.Sprintf("udp/%s/%d", address, env.routeTable.Listen.QuicPort)
	packetConns := env.binds.boundPacketConns(bindAddress)
	if len(packetConns) != env.settings.SocketsPerPort {
		t.Fatalf("%d udp sockets on %s", len(packetConns), bindAddress)
	}

	started, release := env.binds.holdNext(bindAddress)
	defer release()
	const lostIndex = 1
	packetConns[lostIndex].Close()
	testFrontWait(t, started, "the lost socket's re-creation")

	if waits := restartWaits.of(env.socketKey(address, frontPortQuic, lostIndex)); !slices.Equal(waits, []time.Duration{0}) {
		t.Fatalf("waits before re-creating = %v, want one zero wait", waits)
	}
	header := env.statusHeaders()
	if entry := testFrontSocketsEntry(header, groupName); entry != groupName+"=3/4" {
		t.Fatalf("the group with a socket lost reads %q", entry)
	}
	if got := header.Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q with a socket missing", frontReadyHeader, got)
	}

	// the siblings serve new connections meanwhile
	client, _ := env.apiClient(address, frontPortQuic, nil)
	status, body, err := testFrontGet(t, client, testFrontApiName, "/hello")
	if err != nil || status != http.StatusOK || body != "api "+testFrontInterfaceA+" /hello" {
		t.Fatalf("a request while the socket is lost = %d %q %v", status, body, err)
	}

	release()
	env.binds.waitPacketConns(t, bindAddress, env.settings.SocketsPerPort+1)
	env.waitSocketsUp(socketCount)
	header = env.statusHeaders()
	if entry := testFrontSocketsEntry(header, groupName); entry != groupName+"=4/4" {
		t.Fatalf("the group after the re-creation reads %q", entry)
	}
	if got := header.Get(frontReadyHeader); got != "1" {
		t.Fatalf("%s = %q with every socket back", frontReadyHeader, got)
	}
	// only the lost socket was bound again: with all four up, the three
	// siblings are the ones bound at start
	if count := len(env.binds.boundPacketConns(bindAddress)); count != env.settings.SocketsPerPort+1 {
		t.Fatalf("%d udp binds on %s, want one re-creation", count, bindAddress)
	}
}

// A tcp listener lost while serving is re-created at once: its group reads
// 3/4 meanwhile, a connection it accepted before keeps serving, and the
// siblings accept new ones.
func TestFrontRecreatesALostTcpSocketAtOnce(t *testing.T) {
	restartWaits := newTestFrontRestartWaits()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Hour
		env.settings.restartWaitObserverForTest = restartWaits.observe
	})
	socketCount := len(env.frontServer.sockets)
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortHttps)
	groupName := fmt.Sprintf("tcp/%s/%d", address, env.routeTable.Listen.HttpsPort)

	// a keepalive connection, accepted before the loss
	client := env.httpsClient(address, testFrontApiName, false, nil)
	reused := []bool{}
	get := func() string {
		request, err := http.NewRequestWithContext(
			httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) {
					reused = append(reused, info.Reused)
				},
			}),
			http.MethodGet,
			"https://"+testFrontApiName+"/",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	if body := get(); body != "https "+testFrontInterfaceA+" HTTP/1.1 "+testFrontApiName {
		t.Fatalf("first response %q", body)
	}

	started, release := env.binds.holdNext(bindAddress)
	defer release()
	listeners := env.binds.boundListeners(bindAddress)
	const lostIndex = 2
	listeners[lostIndex].Close()
	testFrontWait(t, started, "the lost listener's re-creation")
	if waits := restartWaits.of(env.socketKey(address, frontPortHttps, lostIndex)); !slices.Equal(waits, []time.Duration{0}) {
		t.Fatalf("waits before re-creating = %v, want one zero wait", waits)
	}
	header := env.statusHeaders()
	if entry := testFrontSocketsEntry(header, groupName); entry != groupName+"=3/4" {
		t.Fatalf("the group with a listener lost reads %q", entry)
	}
	if got := header.Get(frontReadyHeader); got != "0" {
		t.Fatalf("%s = %q with a listener missing", frontReadyHeader, got)
	}

	// the accepted connection serves on, and the siblings accept a new one
	if body := get(); body != "https "+testFrontInterfaceA+" HTTP/1.1 "+testFrontApiName {
		t.Fatalf("response on the kept connection %q", body)
	}
	if !slices.Equal(reused, []bool{false, true}) {
		t.Fatalf("connection reuse %v, want the first connection kept", reused)
	}
	freshClient := env.httpsClient(address, testFrontApiName, false, nil)
	response, err := freshClient.Get("https://" + testFrontApiName + "/")
	if err != nil {
		t.Fatalf("a new connection while a listener is lost: %v", err)
	}
	response.Body.Close()

	release()
	env.binds.waitListeners(t, bindAddress, env.settings.SocketsPerPort+1)
	env.waitSocketsUp(socketCount)
	if entry := testFrontSocketsEntry(env.statusHeaders(), groupName); entry != groupName+"=4/4" {
		t.Fatalf("the group after the re-creation reads %q", entry)
	}
}

// A socket lost after a healthy run is re-created at once every time: the
// backoff resets.
func TestFrontSocketRestartResetsAfterAHealthyRun(t *testing.T) {
	restartWaits := newTestFrontRestartWaits()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Hour
		// every run counts as healthy
		env.settings.SocketHealthyRunTimeout = time.Nanosecond
		env.settings.restartWaitObserverForTest = restartWaits.observe
	})
	socketCount := len(env.frontServer.sockets)
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortWhodis)
	key := env.socketKey(address, frontPortWhodis, 0)

	socketsPerPort := env.settings.SocketsPerPort
	env.binds.boundPacketConns(bindAddress)[0].Close()
	env.binds.waitPacketConns(t, bindAddress, socketsPerPort+1)
	env.waitSocketsUp(socketCount)
	env.binds.boundPacketConns(bindAddress)[socketsPerPort].Close()
	env.binds.waitPacketConns(t, bindAddress, socketsPerPort+2)
	env.waitSocketsUp(socketCount)
	if waits := restartWaits.of(key); !slices.Equal(waits, []time.Duration{0, 0}) {
		t.Fatalf("waits before re-creating = %v, want two zero waits", waits)
	}
}

// A re-creation that fails backs off, doubling from the initial delay up to
// the maximum, and a socket lost again before a healthy run keeps the backed
// off wait instead of re-creating at once.
func TestFrontSocketRestartBacksOffWhileItFails(t *testing.T) {
	restartWaits := newTestFrontRestartWaits()
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Millisecond
		env.settings.SocketRestartMaxDelay = 4 * time.Millisecond
		// no run counts as healthy
		env.settings.SocketHealthyRunTimeout = time.Hour
		env.settings.restartWaitObserverForTest = restartWaits.observe
	})
	socketCount := len(env.frontServer.sockets)
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortWhodis)
	key := env.socketKey(address, frontPortWhodis, 2)

	socketsPerPort := env.settings.SocketsPerPort
	env.binds.failNext(bindAddress, 3)
	env.binds.boundPacketConns(bindAddress)[2].Close()
	env.binds.waitPacketConns(t, bindAddress, socketsPerPort+1)
	env.waitSocketsUp(socketCount)
	wantWaits := []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	if waits := restartWaits.of(key); !slices.Equal(waits, wantWaits) {
		t.Fatalf("waits while failing = %v, want %v", waits, wantWaits)
	}

	env.binds.boundPacketConns(bindAddress)[socketsPerPort].Close()
	env.binds.waitPacketConns(t, bindAddress, socketsPerPort+2)
	env.waitSocketsUp(socketCount)
	wantWaits = append(wantWaits, 4*time.Millisecond)
	if waits := restartWaits.of(key); !slices.Equal(waits, wantWaits) {
		t.Fatalf("waits after an unhealthy run = %v, want %v", waits, wantWaits)
	}
}

// Once the drain begins, a lost socket stays lost: nothing is re-created
// for a generation that no new flow reaches.
func TestFrontDrainStopsRecreatingSockets(t *testing.T) {
	env := newTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Millisecond
	})
	socketCount := len(env.frontServer.sockets)
	env.waitSocketsUp(socketCount)
	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortWhodis)

	env.frontServer.beginDrain()
	env.binds.boundPacketConns(bindAddress)[0].Close()
	env.waitSocketsUp(socketCount - 1 - countTcpSockets(env))
	env.frontServer.close()
	if count := len(env.binds.boundPacketConns(bindAddress)); count != env.settings.SocketsPerPort {
		t.Fatalf("%d udp binds on %s after a loss during the drain, want none again", count, bindAddress)
	}
}

// The tcp sockets, which the drain's shutdown closes.
func countTcpSockets(env *testFrontEnv) int {
	count := 0
	for _, socket := range env.frontServer.sockets {
		if socket.key.protocol == frontProtocolTcp {
			count += 1
		}
	}
	return count
}

// The socket gauges count each group's sockets up, and a lost socket counts
// one restart, by interface, family, protocol and port kind, never by
// address.
func TestFrontSocketMetricsCountSocketsAndRestarts(t *testing.T) {
	env := newUnstartedTestFrontEnv(t, func(env *testFrontEnv) {
		env.settings.SocketRestartInitialDelay = time.Hour
	})
	labels := []string{testFrontInterfaceA, frontFamilyIpv4, frontProtocolUdp, frontPortQuic}
	upGauge := frontSocketsUpGauge.WithLabelValues(labels...)
	restartCounter := frontSocketRestartsCounter.WithLabelValues(labels...)
	upBefore := testutil.ToFloat64(upGauge)
	restartsBefore := testutil.ToFloat64(restartCounter)
	if err := env.frontServer.start(env.ctx, env.front, env.connectQuic); err != nil {
		t.Fatal(err)
	}
	socketsPerPort := float64(env.settings.SocketsPerPort)
	if up := testutil.ToFloat64(upGauge) - upBefore; up != socketsPerPort {
		t.Fatalf("%v sockets up for %v, want %v", up, labels, socketsPerPort)
	}

	address, _ := env.address(testFrontInterfaceA, frontFamilyIpv4)
	bindAddress := env.bindAddress(address, frontPortQuic)
	started, release := env.binds.holdNext(bindAddress)
	defer release()
	env.binds.boundPacketConns(bindAddress)[0].Close()
	testFrontWait(t, started, "the lost socket's re-creation")
	if up := testutil.ToFloat64(upGauge) - upBefore; up != socketsPerPort-1 {
		t.Fatalf("%v sockets up with one lost, want %v", up, socketsPerPort-1)
	}
	if restarts := testutil.ToFloat64(restartCounter) - restartsBefore; restarts != 1 {
		t.Fatalf("%v restarts counted for one loss", restarts)
	}
	release()
	env.binds.waitPacketConns(t, bindAddress, env.settings.SocketsPerPort+1)
	env.waitSocketsUp(len(env.frontServer.sockets))
	if up := testutil.ToFloat64(upGauge) - upBefore; up != socketsPerPort {
		t.Fatalf("%v sockets up after the re-creation, want %v", up, socketsPerPort)
	}

	env.frontServer.close()
	if up := testutil.ToFloat64(upGauge) - upBefore; up != 0 {
		t.Fatalf("%v sockets still counted up after the close", up)
	}
}
