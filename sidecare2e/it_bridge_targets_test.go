//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// What the it-bridge case's flows reach (it_bridge_test.go's header): the services standing in for the hosts behind
// each subnet and for each port forward's target, and each forwarding device's NAT dialer, which C5's hook hands every
// flow it passes or maps.
//
//	service  a TCP and a UDP listener on loopback, ports the OS picks: each request line, "itb <client> <nonce>
//	         <destination>", is answered "itb-ok <owner> <incarnation> <nonce>" and noted as an arrival, so both the
//	         client and the case's oracle know which destination a flow reached, when, and for whom
//	dialer   the NAT's DialContext (connect.DialContextSettings) on a subnet's device and a forward's host: a dial to an
//	         address of the subnet the device stands in front of, on the subnets' service port, reaches that subnet's
//	         service, as the LAN host at the address would answer; a dial of a forward's target, 127.0.0.1 and the
//	         service's port, is dialed as it is. Every other dial is refused, as a LAN with nobody at the address or a
//	         loopback port nobody listens on refuses it, and noted: rule 3's loopback pass-through of a hosted address's
//	         port with no map (§17.6) lands here, and so would a flow to anywhere the hook should never have passed
//
// A test host has no LANs to put behind eight devices, so the dialer is the one stand-in on the forwarding path; the
// hook, the NAT, the provider and every flow's bytes are connect's and the bridge's own. ICMP echo goes through the
// NAT's own ICMP egress, never the dialer: the case sends it only to hosted addresses, which rule 3 maps to the host's
// loopback, since an echo into a routed subnet would leave the test host for a real network.

package sidecare2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The request a client sends and the answer a service writes, one line each (the file's header).
const (
	itBridgeRequestWord = "itb"
	itBridgeAnswerWord  = "itb-ok"
)

// One request a service received (the file's header): the oracle's record of what reached a destination.
type ItBridgeArrival struct {
	At time.Time
	// "tcp" or "udp".
	Network     string
	Client      string
	Nonce       string
	Destination string
	// The incarnation the service's owner was in when it answered.
	Incarnation int64
}

// One destination of the case (the file's header). Safe for concurrent use.
type itBridgeService struct {
	owner         string
	readByteCount int
	tcp           net.Listener
	udp           *net.UDPConn
	incarnation   atomic.Int64
	workers       sync.WaitGroup

	stateLock sync.Mutex
	arrivals  []*ItBridgeArrival
	conns     map[net.Conn]bool
	closed    bool
}

// A service for owner on loopback, its listeners open and answering until close. readByteCount bounds one request.
func newItBridgeService(owner string, readByteCount int) (*itBridgeService, error) {
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	udpAddress, err := net.ResolveUDPAddr("udp4", "127.0.0.1:0")
	if err != nil {
		tcp.Close()
		return nil, err
	}
	udp, err := net.ListenUDP("udp4", udpAddress)
	if err != nil {
		tcp.Close()
		return nil, err
	}
	self := &itBridgeService{owner: owner, readByteCount: readByteCount, tcp: tcp, udp: udp, conns: map[net.Conn]bool{}}
	self.workers.Go(self.acceptTcp)
	self.workers.Go(self.readUdp)
	return self, nil
}

// The loopback address of its TCP listener.
func (self *itBridgeService) TcpAddress() netip.AddrPort {
	return self.tcp.Addr().(*net.TCPAddr).AddrPort()
}

// The loopback address of its UDP listener.
func (self *itBridgeService) UdpAddress() netip.AddrPort {
	return self.udp.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Sets the incarnation its answers name from now on.
func (self *itBridgeService) SetIncarnation(incarnation int64) {
	self.incarnation.Store(incarnation)
}

// Every arrival so far, oldest first.
func (self *itBridgeService) Arrivals() []*ItBridgeArrival {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.arrivals)
}

// The answer to one request line, nil for a line that is no request; the arrival noted.
func (self *itBridgeService) answer(network string, line string) []byte {
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != itBridgeRequestWord {
		return nil
	}
	incarnation := self.incarnation.Load()
	arrival := &ItBridgeArrival{
		At:          time.Now(),
		Network:     network,
		Client:      fields[1],
		Nonce:       fields[2],
		Destination: fields[3],
		Incarnation: incarnation,
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.arrivals = append(self.arrivals, arrival)
	}()
	return []byte(fmt.Sprintf("%s %s %d %s\n", itBridgeAnswerWord, self.owner, incarnation, fields[2]))
}

// Answers each TCP connection's request lines until it or the service closes.
func (self *itBridgeService) acceptTcp() {
	for {
		conn, err := self.tcp.Accept()
		if err != nil {
			return
		}
		kept := func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.closed {
				return false
			}
			self.conns[conn] = true
			return true
		}()
		if !kept {
			conn.Close()
			return
		}
		self.workers.Go(func() {
			defer func() {
				conn.Close()
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				delete(self.conns, conn)
			}()
			reader := bufio.NewReaderSize(conn, self.readByteCount)
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if answer := self.answer("tcp", line); answer != nil {
					if _, err := conn.Write(answer); err != nil {
						return
					}
				}
			}
		})
	}
}

// Answers each UDP datagram's request until the service closes.
func (self *itBridgeService) readUdp() {
	buffer := make([]byte, self.readByteCount)
	for {
		n, address, err := self.udp.ReadFromUDPAddrPort(buffer)
		if err != nil {
			return
		}
		if answer := self.answer("udp", string(buffer[:n])); answer != nil {
			self.udp.WriteToUDPAddrPort(answer, address)
		}
	}
}

// Closes its listeners and connections and waits for its workers. Idempotent.
func (self *itBridgeService) close() {
	conns := func() []net.Conn {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		self.closed = true
		conns := []net.Conn{}
		for conn := range self.conns {
			conns = append(conns, conn)
		}
		return conns
	}()
	self.tcp.Close()
	self.udp.Close()
	for _, conn := range conns {
		conn.Close()
	}
	self.workers.Wait()
}

// One dial a device's NAT made (the file's header's dialer row).
type ItBridgeDial struct {
	At      time.Time
	Network string
	Address string
	// The owner of the service it reached; empty for a dial refused.
	Owner string
}

// A subnet the dialer stands in front of: its prefix, the port its hosts answer on, and the service standing in for
// them.
type itBridgeDialerSubnet struct {
	prefix  netip.Prefix
	port    uint16
	service *itBridgeService
}

// A forward's target the dialer reaches: the network, tcp or udp, and the loopback address.
type itBridgeDialerTarget struct {
	network string
	address netip.AddrPort
}

// One device's NAT dialer (the file's header). Safe for concurrent use.
type itBridgeDialer struct {
	device string
	dialer net.Dialer

	stateLock sync.Mutex
	subnets   []itBridgeDialerSubnet
	targets   map[itBridgeDialerTarget]*itBridgeService
	dials     []*ItBridgeDial
}

func newItBridgeDialer(deviceName string) *itBridgeDialer {
	return &itBridgeDialer{device: deviceName, targets: map[itBridgeDialerTarget]*itBridgeService{}}
}

// Stands the dialer in front of prefix, whose hosts answer on port through service.
func (self *itBridgeDialer) AddSubnet(prefix netip.Prefix, port uint16, service *itBridgeService) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subnets = append(self.subnets, itBridgeDialerSubnet{prefix: prefix, port: port, service: service})
}

// Lets the dialer reach service's listener of network, tcp or udp, as a forward's target.
func (self *itBridgeDialer) AddTarget(network string, service *itBridgeService) {
	address := service.TcpAddress()
	if network == "udp" {
		address = service.UdpAddress()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.targets[itBridgeDialerTarget{network: network, address: address}] = service
}

// Every dial so far, oldest first.
func (self *itBridgeDialer) Dials() []*ItBridgeDial {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.dials)
}

// The NAT's DialContext (the file's header's dialer row).
func (self *itBridgeDialer) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	kind := "tcp"
	if strings.HasPrefix(network, "udp") {
		kind = "udp"
	}
	dial := &ItBridgeDial{At: time.Now(), Network: kind, Address: address}
	var reach netip.AddrPort
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.dials = append(self.dials, dial)
		destination, err := netip.ParseAddrPort(address)
		if err != nil {
			return
		}
		destination = netip.AddrPortFrom(destination.Addr().Unmap(), destination.Port())
		for _, subnet := range self.subnets {
			if subnet.prefix.Contains(destination.Addr()) && subnet.port == destination.Port() {
				dial.Owner = subnet.service.owner
				reach = subnet.service.TcpAddress()
				if kind == "udp" {
					reach = subnet.service.UdpAddress()
				}
				return
			}
		}
		if service := self.targets[itBridgeDialerTarget{network: kind, address: destination}]; service != nil {
			dial.Owner = service.owner
			reach = destination
		}
	}()
	if !reach.IsValid() {
		return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
	}
	return self.dialer.DialContext(ctx, kind+"4", reach.String())
}

// A request line for client to destination, with nonce.
func itBridgeRequest(client string, nonce string, destination string) []byte {
	return []byte(fmt.Sprintf("%s %s %s %s\n", itBridgeRequestWord, client, nonce, destination))
}

// An answer line's owner and incarnation, refused unless it answers nonce.
func itBridgeParseAnswer(line string, nonce string) (owner string, incarnation int64, err error) {
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != itBridgeAnswerWord {
		return "", 0, fmt.Errorf("an answer that is none: %q", line)
	}
	if fields[3] != nonce {
		return "", 0, errors.New("an answer to another request")
	}
	incarnation, err = strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("an answer's incarnation: %w", err)
	}
	return fields[1], incarnation, nil
}
