package main

import (
	"bytes"
	"container/heap"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/mailgun/proxyproto"
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

func DefaultWarpQuicPpSettings() *PpSettings {
	return &PpSettings{
		MaxPacketSize: 1500,
		ProxyTimeout:  45 * time.Second,
	}
}

type PpSettings struct {
	MaxPacketSize int
	ProxyTimeout  time.Duration
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

func (self *PpPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	buffer := self.readBuffer

	n, addr, err = self.conn.ReadFrom(buffer)
	if err != nil {
		return
	}

	// the packet may contain a proxy protocol header at any time
	// if the last packet from addr was > proxy timeout,
	// we can trust that the header is from our proxy and the protocol header is used
	// otherwise the header is discarded because we can't tell if header is from our proxy or the user
	h, header, ppErr := parsePpHeader(buffer[0:n])
	if ppErr != nil {
		err = ppErr
		return
	}

	// fmt.Printf("parse pp %d %v\n", h, header)

	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := time.Now()
		expireTime := now.Add(-self.settings.ProxyTimeout)
		for 0 < self.proxyQueue.Len() && self.proxyQueue.PeekFirst().lastUpdateTime.Before(expireTime) {
			self.proxyQueue.RemoveFirst()
		}

		s := self.proxyQueue.GetByProxyAddr(addr.(*net.UDPAddr).AddrPort())
		if s == nil {
			if h == 0 {
				err = fmt.Errorf("proxy protocol header required but not found")
				return
			}

			var realAddr *net.UDPAddr
			switch v := header.Source.(type) {
			case *net.UDPAddr:
				realAddr = v
			case *net.TCPAddr:
				realAddr = &net.UDPAddr{
					IP:   v.IP,
					Port: v.Port,
					Zone: v.Zone,
				}
			}
			realAddrPort := realAddr.AddrPort()
			s = &proxyState{
				proxyAddr:      addr,
				proxyAddrPort:  addr.(*net.UDPAddr).AddrPort(),
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
	}()

	if err != nil {
		return
	}

	copy(p[0:n], buffer[0:n])
	return
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

func parsePpHeader(b []byte) (h int, header *proxyproto.Header, err error) {
	if !(6 <= len(b) && ([6]byte)(V1Identifier) == ([6]byte)(b) ||
		12 <= len(b) && ([12]byte)(V2Identifier) == ([12]byte)(b)) {
		// fmt.Printf("pp signature not found %v\n", ([12]byte)(b))
		return 0, nil, nil
	}
	r := bytes.NewReader(b)
	header, err = proxyproto.ReadHeader(r)
	h = len(b) - r.Len()
	return
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
