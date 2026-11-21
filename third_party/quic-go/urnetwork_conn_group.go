package quic

// urnetwork: one Transport over several sockets (URNETWORK.md, "One transport
// over a group of sockets").

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/internal/protocol"
)

// connGroupQueueLen is how many received packets can wait between a group's
// sockets and the Transport's listen loop. Past it, each socket's reader waits,
// and what arrives meanwhile waits in the socket's own receive buffer, as when
// the listen loop is slow to read a lone socket.
const connGroupQueueLen = 256

// errConnGroupWriteDeadline is SetWriteDeadline's answer: a write goes out
// through one socket of the group, which no deadline of the group's reaches.
var errConnGroupWriteDeadline = errors.New("quic: a ConnGroup has no write deadline")

// A ConnGroup is the packet conn of one Transport that serves several UDP
// sockets bound to one local address and port, such as the sockets of one
// SO_REUSEPORT group.
//
// The kernel spreads a reuseport group's flows over its sockets by a hash of
// each flow's 4-tuple, and moves most of them whenever a socket leaves or joins
// the group; a NAT rebinding moves one. With a Transport per socket, a moved
// flow reaches a Transport that doesn't hold its connection. Through a
// ConnGroup, every socket's packets reach the same Transport, so a moved flow
// keeps its connection.
//
// Add wraps each socket as a Transport wraps the net.PacketConn it is given,
// so a *net.UDPConn keeps quic-go's optimizations: batched reads with ECN and
// packet info, the DF bit for path MTU discovery, and GSO. Each socket is read
// on its own goroutine into one queue, which the Transport's listen loop reads.
//
// A packet is written through one live socket, picked by a hash of its
// destination, so one flow leaves through one socket while the group's sockets
// stay the same. A socket closed under a write is skipped for the next one.
// With no socket left the packet is dropped, as the network drops one, and the
// connection's loss recovery sends it again: a write error would end the
// connection.
//
// Sockets come and go while the group serves: a socket whose read fails leaves
// the group at once, and Add brings in its replacement. The group's
// capabilities are those of its first socket, and a socket with others is
// refused, since a connection reads them when it starts and relies on them for
// its life. The group never closes on its own, so a Transport over it lives
// until it is closed. Closing the group closes every socket in it.
type ConnGroup struct {
	localAddr net.Addr
	writeSeed maphash.Seed

	packets chan receivedPacket
	closed  chan struct{}

	// the sockets a write may pick, replaced whole when one joins or leaves
	writeMembers atomic.Pointer[[]*ConnGroupMember]
	// those of the first socket, fixed once set
	firstCapabilities atomic.Pointer[connCapabilities]
	readDeadline      atomic.Pointer[connGroupDeadline]

	mutex sync.Mutex
	// in the order they were added
	members  []*ConnGroupMember
	isClosed bool
}

// A read deadline of a group, and a channel closed once it is replaced.
type connGroupDeadline struct {
	deadline time.Time
	replaced chan struct{}
}

// A ConnGroupMember is one socket of a ConnGroup.
type ConnGroupMember struct {
	group *ConnGroup
	conn  net.PacketConn
	raw   rawConn

	// closed by Close
	removed chan struct{}
	// closed once the socket's reader returned
	done chan struct{}
	// the read error that ended the reader, nil once closed; set before done
	// is closed
	err error
	// whether a write may pick the socket; guarded by the group's mutex
	writable bool

	closeOnce sync.Once
	closeErr  error
}

var (
	_ rawConn        = &ConnGroup{}
	_ net.PacketConn = &ConnGroup{}
)

// NewConnGroup returns a group with no sockets, for sockets bound to
// localAddr, which it reports as its local address.
func NewConnGroup(localAddr net.Addr) *ConnGroup {
	g := &ConnGroup{
		localAddr: localAddr,
		writeSeed: maphash.MakeSeed(),
		packets:   make(chan receivedPacket, connGroupQueueLen),
		closed:    make(chan struct{}),
	}
	g.writeMembers.Store(&[]*ConnGroupMember{})
	g.readDeadline.Store(&connGroupDeadline{replaced: make(chan struct{})})
	return g
}

// Add wraps conn as a Transport wraps the net.PacketConn it is given, and
// starts reading it into the group. The group owns conn from then on: closing
// the member or the group closes it. On an error conn is still the caller's:
// the group is closed, conn can't be wrapped, or its capabilities differ from
// the group's.
func (g *ConnGroup) Add(conn net.PacketConn) (*ConnGroupMember, error) {
	var raw rawConn
	if c, ok := conn.(rawConn); ok {
		raw = c
	} else {
		var err error
		raw, err = wrapConn(conn)
		if err != nil {
			return nil, err
		}
	}
	m := &ConnGroupMember{
		group:    g,
		conn:     conn,
		raw:      raw,
		removed:  make(chan struct{}),
		done:     make(chan struct{}),
		writable: true,
	}
	if err := g.join(m); err != nil {
		return nil, err
	}
	go m.read()
	return m, nil
}

func (g *ConnGroup) join(m *ConnGroupMember) error {
	g.mutex.Lock()
	defer g.mutex.Unlock()

	if g.isClosed {
		return net.ErrClosed
	}
	capabilities := m.raw.capabilities()
	if first := g.firstCapabilities.Load(); first == nil {
		g.firstCapabilities.Store(&capabilities)
	} else if *first != capabilities {
		return fmt.Errorf("quic: a socket with capabilities %+v can't join a group with %+v", capabilities, *first)
	}
	g.members = append(g.members, m)
	g.storeWriteMembersLocked()
	return nil
}

// storeWriteMembersLocked publishes the sockets a write may pick, in the order
// they were added.
func (g *ConnGroup) storeWriteMembersLocked() {
	writeMembers := make([]*ConnGroupMember, 0, len(g.members))
	for _, m := range g.members {
		if m.writable {
			writeMembers = append(writeMembers, m)
		}
	}
	g.writeMembers.Store(&writeMembers)
}

// unwritable keeps writes from picking a socket that is no longer read.
func (g *ConnGroup) unwritable(m *ConnGroupMember) {
	g.mutex.Lock()
	defer g.mutex.Unlock()

	if !m.writable {
		return
	}
	m.writable = false
	g.storeWriteMembersLocked()
}

// remove takes a socket out of the group.
func (g *ConnGroup) remove(m *ConnGroupMember) {
	g.mutex.Lock()
	defer g.mutex.Unlock()

	m.writable = false
	g.members = slices.DeleteFunc(g.members, func(member *ConnGroupMember) bool {
		return member == m
	})
	g.storeWriteMembersLocked()
}

// read reads the socket into the group until it is closed or a read fails.
// Temporary errors are retried, as Transport.listen retries them.
func (m *ConnGroupMember) read() {
	defer close(m.done)
	defer m.group.unwritable(m)

	for {
		p, err := m.raw.ReadPacket()
		if err != nil {
			select {
			case <-m.removed:
				return
			default:
			}
			//nolint:staticcheck // SA1019: as Transport.listen
			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				continue
			}
			if isRecvMsgSizeErr(err) {
				continue
			}
			m.err = err
			return
		}
		select {
		case m.group.packets <- p:
		case <-m.removed:
			p.buffer.Release()
			return
		}
	}
}

// Done is closed once the socket is no longer read: its read failed, or it or
// its group was closed.
func (m *ConnGroupMember) Done() <-chan struct{} {
	return m.done
}

// Err is the read error that ended the socket, once Done is closed. It is nil
// before, and for a socket that was closed.
func (m *ConnGroupMember) Err() error {
	select {
	case <-m.done:
		return m.err
	default:
		return nil
	}
}

// Conn is the socket as it was added.
func (m *ConnGroupMember) Conn() net.PacketConn {
	return m.conn
}

// Close takes the socket out of its group, closes it, and waits for its reader
// to return. Safe to call more than once.
func (m *ConnGroupMember) Close() error {
	m.closeOnce.Do(func() {
		m.group.remove(m)
		close(m.removed)
		m.closeErr = m.conn.Close()
	})
	<-m.done
	return m.closeErr
}

// ReadPacket returns the next packet any socket of the group received. It
// waits until one arrives, the read deadline passes, or the group is closed.
func (g *ConnGroup) ReadPacket() (receivedPacket, error) {
	for {
		p, replaced, err := g.readPacketBefore(g.readDeadline.Load())
		if !replaced {
			return p, err
		}
	}
}

// readPacketBefore waits for a packet under one read deadline, and reports
// when the deadline was replaced first.
func (g *ConnGroup) readPacketBefore(deadline *connGroupDeadline) (receivedPacket, bool, error) {
	var timeout <-chan time.Time
	if !deadline.deadline.IsZero() {
		wait := time.Until(deadline.deadline)
		if wait <= 0 {
			return receivedPacket{}, false, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case p := <-g.packets:
		return p, false, nil
	case <-g.closed:
		return receivedPacket{}, false, net.ErrClosed
	case <-deadline.replaced:
		return receivedPacket{}, true, nil
	case <-timeout:
		return receivedPacket{}, false, os.ErrDeadlineExceeded
	}
}

// WritePacket writes through one live socket, picked by a hash of addr. A
// socket closed under the write is skipped for the next one. With no socket
// left the packet is dropped, and once the group is closed the write fails
// with net.ErrClosed. Every other error is the socket's, unchanged.
func (g *ConnGroup) WritePacket(b []byte, addr net.Addr, packetInfoOOB []byte, gsoSize uint16, ecn protocol.ECN) (int, error) {
	members := *g.writeMembers.Load()
	if len(members) != 0 {
		first := int(g.hashAddr(addr) % uint64(len(members)))
		for i := range len(members) {
			m := members[(first+i)%len(members)]
			n, err := m.raw.WritePacket(b, addr, packetInfoOOB, gsoSize, ecn)
			if errors.Is(err, net.ErrClosed) {
				continue
			}
			return n, err
		}
	}
	select {
	case <-g.closed:
		return 0, net.ErrClosed
	default:
		// no socket to send through: the packet is lost
		return len(b), nil
	}
}

// hashAddr is the seeded hash of a destination that picks its socket.
func (g *ConnGroup) hashAddr(addr net.Addr) uint64 {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return maphash.String(g.writeSeed, addr.String())
	}
	addrPort := udpAddr.AddrPort()
	var key [18]byte
	ip := addrPort.Addr().As16()
	copy(key[:16], ip[:])
	binary.BigEndian.PutUint16(key[16:], addrPort.Port())
	return maphash.Bytes(g.writeSeed, key[:])
}

func (g *ConnGroup) capabilities() connCapabilities {
	if first := g.firstCapabilities.Load(); first != nil {
		return *first
	}
	return connCapabilities{}
}

// LocalAddr is the address the group's sockets are bound to.
func (g *ConnGroup) LocalAddr() net.Addr {
	return g.localAddr
}

// SetReadDeadline bounds the group's reads. The sockets are read on, and
// their packets wait in the group until it is read again.
func (g *ConnGroup) SetReadDeadline(t time.Time) error {
	g.mutex.Lock()
	defer g.mutex.Unlock()

	previous := g.readDeadline.Load()
	g.readDeadline.Store(&connGroupDeadline{
		deadline: t,
		replaced: make(chan struct{}),
	})
	close(previous.replaced)
	return nil
}

// SetWriteDeadline is not supported: each write goes out through one socket.
func (g *ConnGroup) SetWriteDeadline(time.Time) error {
	return errConnGroupWriteDeadline
}

// SetDeadline sets the read deadline, and answers as SetWriteDeadline.
func (g *ConnGroup) SetDeadline(t time.Time) error {
	if err := g.SetReadDeadline(t); err != nil {
		return err
	}
	return g.SetWriteDeadline(t)
}

// ReadFrom reads the next packet any socket of the group received, as
// ReadPacket does. A Transport over the group reads it itself.
func (g *ConnGroup) ReadFrom(b []byte) (int, net.Addr, error) {
	p, err := g.ReadPacket()
	if err != nil {
		return 0, nil, err
	}
	n := copy(b, p.data)
	p.buffer.Release()
	return n, p.remoteAddr, nil
}

// WriteTo writes through one live socket, as WritePacket does.
func (g *ConnGroup) WriteTo(b []byte, addr net.Addr) (int, error) {
	return g.WritePacket(b, addr, nil, 0, protocol.ECNUnsupported)
}

// Close closes every socket of the group and ends its reads with
// net.ErrClosed. A Transport over the group closes itself once its listen loop
// reads that error, so close the Transport first. Safe to call more than once.
func (g *ConnGroup) Close() error {
	members := func() []*ConnGroupMember {
		g.mutex.Lock()
		defer g.mutex.Unlock()

		if g.isClosed {
			return nil
		}
		g.isClosed = true
		close(g.closed)
		return slices.Clone(g.members)
	}()
	var errs []error
	for _, m := range members {
		if err := m.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	// what no reader will take any more
	for {
		select {
		case p := <-g.packets:
			p.buffer.Release()
		default:
			return errors.Join(errs...)
		}
	}
}
