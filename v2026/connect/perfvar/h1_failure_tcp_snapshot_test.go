//go:build acklineagetrace

package perfvar

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"gvisor.dev/gvisor/pkg/tcpip"
)

const h1FailureTCPConnectionCapacity = 64

type h1FailureTCPInfoReader interface {
	TcpInfo() (tcpip.TCPInfoOption, error)
}

// Immutable connection metadata is retained once at dial completion. No
// Read/Write wrapper, packet callback, background sampler, or timer is added.
type h1FailureTCPConnection struct {
	node       string
	connection net.Conn
	reader     h1FailureTCPInfoReader
	next       *h1FailureTCPConnection
}

type h1FailureTCPRecorder struct {
	head        atomic.Pointer[h1FailureTCPConnection]
	retained    atomic.Uint64
	overflow    atomic.Uint64
	unsupported atomic.Uint64
}

func (self *h1FailureTCPRecorder) observeDial(node string, connection net.Conn, ports ...int) {
	if connection == nil {
		return
	}
	address, ok := connection.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	matched := false
	for _, port := range ports {
		matched = matched || address.Port == port
	}
	if !matched {
		return
	}
	reader, ok := connection.(h1FailureTCPInfoReader)
	if !ok {
		self.unsupported.Add(1)
		return
	}
	for {
		count := self.retained.Load()
		if count >= h1FailureTCPConnectionCapacity {
			self.overflow.Add(1)
			return
		}
		if self.retained.CompareAndSwap(count, count+1) {
			break
		}
	}
	entry := &h1FailureTCPConnection{node: node, connection: connection, reader: reader}
	for {
		entry.next = self.head.Load()
		if self.head.CompareAndSwap(entry.next, entry) {
			break
		}
	}
}

type h1FailureTCPConnectionSnapshot struct {
	Node, Local, Remote, ConnectionPointer, ReadOwnerPointer string
	CapturedNS                                               int64
	Info                                                     tcpip.TCPInfoOption
	Error                                                    string `json:",omitempty"`
}

// gonet returns nil once an endpoint no longer has a local/remote address.
// A failure-only snapshot may legitimately race or follow socket retirement;
// preserve an empty address rather than panic or invent a live endpoint.
func h1FailureTCPAddress(address net.Addr) string {
	if address == nil {
		return ""
	}
	return address.String()
}

type h1FailureTCPStackCounters struct {
	SegmentsSent, SegmentsReceived, Retransmits, Timeouts, FastRetransmits, SlowStartRetransmits uint64
	ResetsSent, ResetsReceived, SegmentSendErrors                                                uint64
}

type h1FailureTCPSnapshot struct {
	Connections                     []h1FailureTCPConnectionSnapshot
	Retained, Overflow, Unsupported uint64
	Incomplete                      bool
	Device, Provider, Edge          h1FailureTCPStackCounters
	Links                           map[string]directionalLinkSnapshot `json:",omitempty"`
}

func h1FailureTCPStats(tun *clientconnect.Tun) h1FailureTCPStackCounters {
	if tun == nil {
		return h1FailureTCPStackCounters{}
	}
	stats := tun.Stats().TCP
	return h1FailureTCPStackCounters{
		SegmentsSent: stats.SegmentsSent.Value(), SegmentsReceived: stats.ValidSegmentsReceived.Value(),
		Retransmits: stats.Retransmits.Value(), Timeouts: stats.Timeouts.Value(),
		FastRetransmits: stats.FastRetransmit.Value(), SlowStartRetransmits: stats.SlowStartRetransmits.Value(),
		ResetsSent: stats.ResetsSent.Value(), ResetsReceived: stats.ResetsReceived.Value(),
		SegmentSendErrors: stats.SegmentSendErrors.Value(),
	}
}

func (self *h1FailureTCPRecorder) snapshot(path *fullTunPath) h1FailureTCPSnapshot {
	result := h1FailureTCPSnapshot{Retained: self.retained.Load(), Overflow: self.overflow.Load(), Unsupported: self.unsupported.Load()}
	for entry := self.head.Load(); entry != nil && len(result.Connections) < h1FailureTCPConnectionCapacity; entry = entry.next {
		item := h1FailureTCPConnectionSnapshot{
			Node: entry.node, Local: h1FailureTCPAddress(entry.connection.LocalAddr()), Remote: h1FailureTCPAddress(entry.connection.RemoteAddr()),
			ConnectionPointer: fmt.Sprintf("%p", entry.connection), CapturedNS: time.Now().UnixNano(),
		}
		if tunConnection, ok := entry.connection.(*clientconnect.TunTcpConn); ok {
			// This pointer joins directly to TCPConn.Read in the bounded stack.
			item.ReadOwnerPointer = fmt.Sprintf("%p", tunConnection.TCPConn)
		}
		var err error
		item.Info, err = entry.reader.TcpInfo()
		if err != nil {
			item.Error = err.Error()
			result.Incomplete = true
		}
		result.Connections = append(result.Connections, item)
	}
	if uint64(len(result.Connections)) != result.Retained || result.Overflow != 0 || result.Unsupported != 0 {
		result.Incomplete = true
	}
	if path != nil {
		if len(result.Connections) == 0 {
			result.Incomplete = true
		}
		result.Device, result.Provider = h1FailureTCPStats(path.deviceCarrierTun), h1FailureTCPStats(path.providerCarrierTun)
		if path.environment != nil {
			result.Edge = h1FailureTCPStats(path.environment.edgeTun)
			if path.environment.network != nil {
				result.Links = path.environment.network.snapshotLinks()
			}
		}
	}
	return result
}

type h1FailureFakeTCPConn struct {
	net.Conn
	port  int
	calls atomic.Uint64
	info  tcpip.TCPInfoOption
}

func (self *h1FailureFakeTCPConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}
func (self *h1FailureFakeTCPConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2), Port: self.port}
}
func (self *h1FailureFakeTCPConn) TcpInfo() (tcpip.TCPInfoOption, error) {
	self.calls.Add(1)
	return self.info, nil
}

func TestH1FailureTCPIsConnectionOnlyAndBounded(t *testing.T) {
	var recorder h1FailureTCPRecorder
	connection := &h1FailureFakeTCPConn{port: 443, info: tcpip.TCPInfoOption{RTO: 8 * time.Second, SndCwnd: 1}}
	recorder.observeDial("api", connection, 80)
	if recorder.retained.Load() != 0 {
		t.Fatal("unrelated API connection retained")
	}
	var workers sync.WaitGroup
	for range h1FailureTCPConnectionCapacity + 8 {
		workers.Go(func() { recorder.observeDial("device", connection, 443) })
	}
	workers.Wait()
	if connection.calls.Load() != 0 {
		t.Fatal("TcpInfo sampled before failure")
	}
	snapshot := recorder.snapshot(nil)
	if len(snapshot.Connections) != h1FailureTCPConnectionCapacity || snapshot.Overflow != 8 || !snapshot.Incomplete || connection.calls.Load() != h1FailureTCPConnectionCapacity {
		t.Fatalf("bounded registry mismatch: %+v", snapshot)
	}
	for _, item := range snapshot.Connections {
		if item.Info.RTO != 8*time.Second || item.Info.SndCwnd != 1 || item.Node != "device" {
			t.Fatal("connection identity/info changed")
		}
	}
	connection.info.SndCwnd = 9
	if snapshot.Connections[0].Info.SndCwnd != 1 {
		t.Fatal("snapshot aliases live TCP state")
	}
}

func TestH1FailureTCPEmptyRegistryIsExplicit(t *testing.T) {
	var recorder h1FailureTCPRecorder
	if recorder.snapshot(nil).Incomplete {
		t.Fatal("unit-only recorder requires a topology")
	}
	if !recorder.snapshot(&fullTunPath{}).Incomplete {
		t.Fatal("missing physical connections claimed complete")
	}
}
