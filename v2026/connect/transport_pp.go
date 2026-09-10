package connect

import (
	"bytes"
	"container/heap"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/mailgun/proxyproto"
	"github.com/prometheus/client_golang/prometheus"
)

// proxy protocol suppport from nginx to transports
// some protocols like h1/2 nginx can proxy correctly with headers
// others like h3 it cannot proxy
// For udp it supports proxy protocol (apparently a mix of v1 and v2),
// which needs to be unwrapped per packet before handing the packet to the h3 server
// see https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt

// nginx appends the pp header to all of the packets in the first n ms
// of the stream, n~500-1000, and occasionally during the stream lifetime

const PpMaxHeaderSize = 2048

// see https://www.haproxy.org/download/1.8/doc/proxy-protocol.txt
// var ppv1Signature = [6]byte{
// 	0x50,
// 	0x52,
// 	0x4F,
// 	0x58,
// 	0x59,
// 	0x20,
// }
// var ppv2Signature = [12]byte{
// 	0x0D,
// 	0x0A,
// 	0x0D,
// 	0x0A,
// 	0x00,
// 	0x0D,
// 	0x0A,
// 	0x51,
// 	0x55,
// 	0x49,
// 	0x54,
// 	0x0A,
// }

var (
	V1Identifier = []byte("PROXY ")
	V2Identifier = []byte("\r\n\r\n\x00\r\nQUIT\n")
)

func parsePpHeaderPacket(b []byte) (h int, header *proxyproto.Header, err error) {
	if !(6 <= len(b) && ([6]byte)(V1Identifier) == ([6]byte)(b) ||
		12 <= len(b) && ([12]byte)(V2Identifier) == ([12]byte)(b)) {
		return 0, nil, nil
	}
	r := bytes.NewReader(b)
	header, err = proxyproto.ReadHeader(r)
	h = len(b) - r.Len()
	return
}

func parsePpHeader(r io.Reader) (header *proxyproto.Header, err error) {
	return proxyproto.ReadHeader(r)
}

func DefaultWarpPpSettings() *PpSettings {
	return &PpSettings{
		MaxPacketSize: 1500,
		// **important** this must be > proxy_timeout set in the nginx stream
		ProxyTimeout: 45 * time.Second,
	}
}

type PpSettings struct {
	MaxPacketSize int
	ProxyTimeout  time.Duration
	// DropObserver is an optional test/diagnostic hook. Production accounting
	// always goes through ppDroppedPacketsCounter as well.
	DropObserver func(reason string)
}

const (
	ppDropMalformedHeader    = "malformed_header"
	ppDropMissingHeader      = "missing_header"
	ppDropProxyAddressFamily = "proxy_address_family"
	ppDropTransportFamily    = "transport_family"
	ppDropAddressFamily      = "address_family"
)

var ppDroppedPacketsCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "pp_dropped_packets_total",
		Help:      "UDP packets rejected by the Proxy Protocol socket before QUIC",
	},
	[]string{"reason"},
)

func init() {
	// Bind the closed label set at startup so malformed traffic never performs
	// first-use metric allocation on the packet path.
	for _, reason := range []string{
		ppDropMalformedHeader,
		ppDropMissingHeader,
		ppDropProxyAddressFamily,
		ppDropTransportFamily,
		ppDropAddressFamily,
	} {
		ppDroppedPacketsCounter.WithLabelValues(reason)
	}
	prometheus.MustRegister(ppDroppedPacketsCounter)
}

// implements `net.PacketConn`
type PpPacketConn struct {
	conn net.PacketConn

	settings *PpSettings

	readBuffer []byte

	// state lock
	// proxy address to state
	// proxyStates map[net.Addr]*proxyState
	// real addr to proxy addr
	// proxyAddrs[net.Addr]net.Addr

	stateLock  sync.Mutex
	proxyQueue *proxyStateQueue
}

func NewPpPacketConn(conn net.PacketConn, settings *PpSettings) *PpPacketConn {
	return &PpPacketConn{
		conn:       conn,
		settings:   settings,
		readBuffer: make([]byte, settings.MaxPacketSize+PpMaxHeaderSize),
		proxyQueue: newProxyStateQueue(),
	}
}

func (self *PpPacketConn) drop(reason string) {
	ppDroppedPacketsCounter.WithLabelValues(reason).Inc()
	if self.settings.DropObserver != nil {
		self.settings.DropObserver(reason)
	}
}

func validPpUdpAddressFamily(source *net.UDPAddr, destination *net.UDPAddr) bool {
	if source == nil || destination == nil || source.IP == nil || destination.IP == nil {
		return false
	}
	sourceV4 := source.IP.To4() != nil
	destinationV4 := destination.IP.To4() != nil
	if sourceV4 != destinationV4 {
		return false
	}
	if sourceV4 {
		return true
	}
	return source.IP.To16() != nil && destination.IP.To16() != nil
}

func (self *PpPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	buffer := self.readBuffer

	// A validation failure is a property of one datagram, not of the shared
	// socket. Never return it to quic-go: doing so terminates the listener and
	// blackholes every client assigned to the block. Keep reading until a valid
	// packet arrives or the underlying socket itself fails/closes.
	for {
		n, addr, err = self.conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		proxyAddr, ok := addr.(*net.UDPAddr)
		if !ok {
			self.drop(ppDropProxyAddressFamily)
			continue
		}

		// the packet may contain a proxy protocol header at any time
		// if the last packet from addr was > proxy timeout,
		// we can trust that the header is from our proxy and the protocol header is used
		// otherwise the header is discarded because we can't tell if header is from our proxy or the user
		h, header, ppErr := parsePpHeaderPacket(buffer[0:n])
		if ppErr != nil {
			self.drop(ppDropMalformedHeader)
			continue
		}

		var packetErr error
		packetErr = func() error {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()

			now := time.Now()
			expireTime := now.Add(-self.settings.ProxyTimeout)
			for 0 < self.proxyQueue.Len() && self.proxyQueue.PeekFirst().lastUpdateTime.Before(expireTime) {
				self.proxyQueue.RemoveFirst()
			}

			s := self.proxyQueue.GetByProxyAddr(proxyAddr.AddrPort())
			if s == nil {
				if header == nil {
					return errors.New(ppDropMissingHeader)
				}

				realAddr, ok := header.Source.(*net.UDPAddr)
				if !ok {
					return errors.New(ppDropTransportFamily)
				}
				destinationAddr, ok := header.Destination.(*net.UDPAddr)
				if !ok {
					return errors.New(ppDropTransportFamily)
				}
				if !validPpUdpAddressFamily(realAddr, destinationAddr) {
					return errors.New(ppDropAddressFamily)
				}

				realAddrPort := realAddr.AddrPort()
				s = &proxyState{
					proxyAddr:      proxyAddr,
					proxyAddrPort:  proxyAddr.AddrPort(),
					realAddr:       realAddr,
					realAddrPort:   realAddrPort,
					lastUpdateTime: now,
				}
				self.proxyQueue.Add(s)

				buffer = buffer[h:n]
				n -= h
			} else {
				self.proxyQueue.Update(s, now)

				// *important* the header can be either from our proxy or the user
				//             do not use or store the header value. Just discard it.
				if 0 < h {
					buffer = buffer[h:n]
					n -= h
				}
				// else this is the common case - no proxy protocol
				// note if the input buffer was over-allocated,
				// we could ready directly into the output buffer for the common case
			}

			addr = s.realAddr
			return nil
		}()
		if packetErr != nil {
			self.drop(packetErr.Error())
			continue
		}

		n = copy(p, buffer[:n])
		return n, addr, nil
	}
}

func (self *PpPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	var addrPort netip.AddrPort
	switch v := addr.(type) {
	case *net.UDPAddr:
		addrPort = v.AddrPort()
	case *net.TCPAddr:
		addrPort = v.AddrPort()
	}

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := time.Now()
		expireTime := now.Add(-self.settings.ProxyTimeout)
		for 0 < self.proxyQueue.Len() && self.proxyQueue.PeekFirst().lastUpdateTime.Before(expireTime) {
			self.proxyQueue.RemoveFirst()
		}

		if s := self.proxyQueue.GetByRealAddr(addrPort); s != nil {
			self.proxyQueue.Update(s, now)
			addr = s.proxyAddr
		} else {
			err = fmt.Errorf("proxy protocol state not found")
		}
	}()
	if err != nil {
		return
	}

	n, err = self.conn.WriteTo(p, addr)
	return
}

func (self *PpPacketConn) LocalAddr() net.Addr {
	return self.conn.LocalAddr()
}

func (self *PpPacketConn) SetDeadline(t time.Time) error {
	return self.conn.SetDeadline(t)
}

func (self *PpPacketConn) SetReadDeadline(t time.Time) error {
	return self.conn.SetReadDeadline(t)
}

func (self *PpPacketConn) SetWriteDeadline(t time.Time) error {
	return self.conn.SetWriteDeadline(t)
}

func (self *PpPacketConn) Close() error {
	return self.conn.Close()
}

func (self *PpPacketConn) SetReadBuffer(bytes int) error {
	conn, ok := self.conn.(interface{ SetReadBuffer(int) error })
	if !ok {
		return fmt.Errorf("Set read buffer not supporter on underlying packet conn: %T", self.conn)
	}
	return conn.SetReadBuffer(PpMaxHeaderSize + bytes)
}

func (self *PpPacketConn) SetWriteBuffer(bytes int) error {
	conn, ok := self.conn.(interface{ SetWriteBuffer(int) error })
	if !ok {
		return fmt.Errorf("Set write buffer not supporter on underlying packet conn: %T", self.conn)
	}
	return conn.SetWriteBuffer(bytes)
}

type proxyState struct {
	proxyAddr      net.Addr
	proxyAddrPort  netip.AddrPort
	realAddr       net.Addr
	realAddrPort   netip.AddrPort
	lastUpdateTime time.Time
	heapIndex      int
}

// ordered by lastUpdateTime ascending
type proxyStateQueue struct {
	orderedStates []*proxyState
	// proxy addr -> state
	proxyStates map[netip.AddrPort]*proxyState
	// real addr -> state
	realStates map[netip.AddrPort]*proxyState
}

func newProxyStateQueue() *proxyStateQueue {
	proxyStateQueue := &proxyStateQueue{
		orderedStates: []*proxyState{},
		proxyStates:   map[netip.AddrPort]*proxyState{},
		realStates:    map[netip.AddrPort]*proxyState{},
	}
	heap.Init(proxyStateQueue)
	return proxyStateQueue
}

func (self *proxyStateQueue) GetByProxyAddr(proxyAddrPort netip.AddrPort) *proxyState {
	return self.proxyStates[proxyAddrPort]
}

func (self *proxyStateQueue) GetByRealAddr(proxyAddrPort netip.AddrPort) *proxyState {
	return self.realStates[proxyAddrPort]
}

func (self *proxyStateQueue) Add(s *proxyState) {
	self.proxyStates[s.proxyAddrPort] = s
	self.realStates[s.realAddrPort] = s
	heap.Push(self, s)
}

func (self *proxyStateQueue) Remove(proxyAddrPort netip.AddrPort) *proxyState {
	s, ok := self.proxyStates[proxyAddrPort]
	if !ok {
		return nil
	}
	self.remove(s)
	return s
}

func (self *proxyStateQueue) remove(s *proxyState) {
	delete(self.proxyStates, s.proxyAddrPort)
	delete(self.realStates, s.realAddrPort)
}

func (self *proxyStateQueue) RemoveFirst() *proxyState {
	if len(self.orderedStates) == 0 {
		return nil
	}

	s := heap.Remove(self, 0).(*proxyState)
	delete(self.proxyStates, s.proxyAddrPort)
	delete(self.realStates, s.realAddrPort)
	return s
}

func (self *proxyStateQueue) PeekFirst() *proxyState {
	if len(self.orderedStates) == 0 {
		return nil
	}
	return self.orderedStates[0]
}

func (self *proxyStateQueue) Update(s *proxyState, lastUpdateTime time.Time) {
	s.lastUpdateTime = lastUpdateTime
	heap.Fix(self, s.heapIndex)
}

// heap.Interface

func (self *proxyStateQueue) Push(x any) {
	s := x.(*proxyState)
	s.heapIndex = len(self.orderedStates)
	self.orderedStates = append(self.orderedStates, s)
}

func (self *proxyStateQueue) Pop() any {
	n := len(self.orderedStates)
	i := n - 1
	s := self.orderedStates[i]
	self.orderedStates[i] = nil
	self.orderedStates = self.orderedStates[:n-1]
	return s
}

// sort.Interface

func (self *proxyStateQueue) Len() int {
	return len(self.orderedStates)
}

func (self *proxyStateQueue) Less(i int, j int) bool {
	return self.orderedStates[i].lastUpdateTime.Before(self.orderedStates[j].lastUpdateTime)
}

func (self *proxyStateQueue) Swap(i int, j int) {
	a := self.orderedStates[i]
	b := self.orderedStates[j]
	b.heapIndex = i
	self.orderedStates[i] = b
	a.heapIndex = j
	self.orderedStates[j] = a
}

// implements `net.Listener`
type PpServerConn struct {
	listener net.Listener
	settings *PpSettings
}

func NewPpServerConn(listener net.Listener, settings *PpSettings) *PpServerConn {
	return &PpServerConn{
		listener: listener,
		settings: settings,
	}
}

func (self *PpServerConn) Accept() (net.Conn, error) {
	conn, err := self.listener.Accept()
	if err != nil {
		return nil, err
	}
	return NewPpConn(conn, self.settings)
}

func (self *PpServerConn) Close() error {
	return self.listener.Close()
}

func (self *PpServerConn) Addr() net.Addr {
	return self.listener.Addr()
}

// implements `net.Conn`
type PpConn struct {
	conn net.Conn

	settings *PpSettings

	realAddr *net.TCPAddr
}

func NewPpConn(conn net.Conn, settings *PpSettings) (*PpConn, error) {
	header, err := parsePpHeader(conn)
	if err != nil {
		return nil, err
	}

	realAddr, ok := header.Source.(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("Proxy protocol header must be TCP")
	}

	return &PpConn{
		conn:     conn,
		settings: settings,
		realAddr: realAddr,
	}, nil
}

func (self *PpConn) Read(b []byte) (n int, err error) {
	// if 0 < len(self.lookaheadBuffer) {
	// 	m := copy(b, self.lookaheadBuffer)
	// 	if len(self.lookaheadBuffer) <= m {
	// 		// free the memory
	// 		self.lookaheadBuffer = nil
	// 	} else {
	// 		self.lookaheadBuffer = self.lookaheadBuffer[m:]
	// 	}
	// 	n, err = self.conn.Read(b[m:])
	// 	n += m
	// 	return
	// } else {
	return self.conn.Read(b)
	// }
}

func (self *PpConn) Write(b []byte) (n int, err error) {
	return self.conn.Write(b)
}

func (self *PpConn) Close() error {
	return self.conn.Close()
}

func (self *PpConn) LocalAddr() net.Addr {
	return self.conn.LocalAddr()
}

func (self *PpConn) RemoteAddr() net.Addr {
	return self.realAddr
}

func (self *PpConn) SetDeadline(t time.Time) error {
	return self.conn.SetDeadline(t)
}

func (self *PpConn) SetReadDeadline(t time.Time) error {
	return self.conn.SetReadDeadline(t)
}

func (self *PpConn) SetWriteDeadline(t time.Time) error {
	return self.conn.SetWriteDeadline(t)
}
