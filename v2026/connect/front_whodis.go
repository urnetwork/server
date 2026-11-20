package connect

// The whodis sockets of one group as one net.PacketConn, under the group's one
// decode53 translation (D22 (b), LBALT-IMPL.md §8.4).
//
// The quic port's sockets feed their transport through a quic.ConnGroup,
// which keeps quic-go's oob path on each socket. Whodis has none to keep: its
// transport reads the translation, which quic-go wraps as a basic conn, so the
// translation's own socket was read and written plainly. The fan-in keeps
// that, for every socket of the group: each socket is read on its own
// goroutine, a whole datagram at a time, into one queue the translation reads,
// and the translation's writes go out through one live socket, picked by a
// hash of the destination, as the ConnGroup picks one.
//
// With one translation per group, the pump state of every query the group
// receives is in one place, so an answer can use a query's id whichever socket
// the query reached, and a QUIC flow the kernel moves to another socket of the
// group still reaches its connection.
//
// A socket whose read fails leaves the fan-in at once and reports the error;
// the front re-creates it, and the replacement joins. A socket closed under a
// write is skipped for the next one, and with no socket left a datagram is
// dropped, as the translation drops one it can't write: only the fan-in's own
// close ends the translation's encoder. The buffer sizes quic-go asks of the
// translation reach every socket, including those that join later.

import (
	"encoding/binary"
	"errors"
	"hash/maphash"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/glog/v2026"
)

// The largest udp payload. The fan-in reads a whole datagram, whatever its
// size, so the translation reads what it read from its socket before, cut to
// its own buffer.
const frontWhodisMaxDatagramByteCount = 64 * 1024

// The deadlines a fan-in refuses: each read is of whichever socket has a
// datagram, and each write goes out through one socket.
var errFrontWhodisFanInDeadline = errors.New("the whodis fan-in has no deadlines")

// One datagram a socket of the fan-in read, waiting for the translation.
type frontWhodisDatagram struct {
	// from connect's message pool, returned once read
	data []byte
	addr net.Addr
}

// The sockets of one whodis group as one net.PacketConn.
type frontWhodisFanIn struct {
	localAddr net.Addr
	settings  *FrontServerSettings
	writeSeed maphash.Seed

	datagrams chan frontWhodisDatagram
	closed    chan struct{}

	// the sockets a write may pick, replaced whole when one joins or leaves
	writeMembers atomic.Pointer[[]*frontWhodisFanInMember]

	stateLock sync.Mutex
	// in the order they joined
	members  []*frontWhodisFanInMember
	isClosed bool
	// the sizes asked for every socket, zero when never asked
	readBufferByteCount  int
	writeBufferByteCount int
}

// One socket of a whodis fan-in.
type frontWhodisFanInMember struct {
	fanIn      *frontWhodisFanIn
	packetConn net.PacketConn

	// closed by Close
	removed chan struct{}
	// closed once the socket's reader returned
	done chan struct{}
	// the read error that ended the reader, nil once closed; set before done
	// is closed
	err error
	// whether a write may pick the socket, guarded by the fan-in's stateLock
	writable bool

	closeOnce sync.Once
	closeErr  error
}

var _ net.PacketConn = (*frontWhodisFanIn)(nil)

// A fan-in with no socket, for sockets bound to localAddr.
func newFrontWhodisFanIn(localAddr net.Addr, settings *FrontServerSettings) *frontWhodisFanIn {
	fanIn := &frontWhodisFanIn{
		localAddr: localAddr,
		settings:  settings,
		writeSeed: maphash.MakeSeed(),
		datagrams: make(chan frontWhodisDatagram, settings.WhodisFanInQueueLength),
		closed:    make(chan struct{}),
	}
	fanIn.writeMembers.Store(&[]*frontWhodisFanInMember{})
	return fanIn
}

// Starts reading packetConn into the fan-in, with the buffer sizes asked so
// far. The fan-in owns packetConn from then on; on an error, the fan-in being
// closed, packetConn is still the caller's.
func (self *frontWhodisFanIn) add(packetConn net.PacketConn) (*frontWhodisFanInMember, error) {
	member := &frontWhodisFanInMember{
		fanIn:      self,
		packetConn: packetConn,
		removed:    make(chan struct{}),
		done:       make(chan struct{}),
		writable:   true,
	}
	readBufferByteCount, writeBufferByteCount, err := func() (int, int, error) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.isClosed {
			return 0, 0, net.ErrClosed
		}
		self.members = append(self.members, member)
		self.storeWriteMembersWithLock()
		return self.readBufferByteCount, self.writeBufferByteCount, nil
	}()
	if err != nil {
		return nil, err
	}
	if 0 < readBufferByteCount {
		if err := setFrontReadBuffer(packetConn, readBufferByteCount); err != nil && glog.V(1) {
			glog.Infof("[front]a whodis socket kept its read buffer: %s\n", err)
		}
	}
	if 0 < writeBufferByteCount {
		if err := setFrontWriteBuffer(packetConn, writeBufferByteCount); err != nil && glog.V(1) {
			glog.Infof("[front]a whodis socket kept its write buffer: %s\n", err)
		}
	}
	go member.read()
	return member, nil
}

// Publishes the sockets a write may pick, in the order they joined.
func (self *frontWhodisFanIn) storeWriteMembersWithLock() {
	writeMembers := make([]*frontWhodisFanInMember, 0, len(self.members))
	for _, member := range self.members {
		if member.writable {
			writeMembers = append(writeMembers, member)
		}
	}
	self.writeMembers.Store(&writeMembers)
}

// Keeps writes from picking a socket that is no longer read.
func (self *frontWhodisFanIn) unwritable(member *frontWhodisFanInMember) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !member.writable {
		return
	}
	member.writable = false
	self.storeWriteMembersWithLock()
}

// Takes a socket out of the fan-in.
func (self *frontWhodisFanIn) remove(member *frontWhodisFanInMember) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	member.writable = false
	self.members = slices.DeleteFunc(self.members, func(other *frontWhodisFanInMember) bool {
		return other == member
	})
	self.storeWriteMembersWithLock()
}

// Reads the socket into the fan-in until it is closed or a read fails. A
// temporary error, which net/http and quic-go wait out, is retried after a
// wait that doubles from SocketRestartInitialDelay up to SocketRestartMaxDelay,
// so a socket that keeps failing can't spin; any other error is the socket's
// loss.
func (self *frontWhodisFanInMember) read() {
	defer close(self.done)
	defer self.fanIn.unwritable(self)
	scratch := make([]byte, frontWhodisMaxDatagramByteCount)
	retryWait := time.Duration(0)
	for {
		n, addr, err := self.packetConn.ReadFrom(scratch)
		if err != nil {
			select {
			case <-self.removed:
				return
			default:
			}
			if !frontWhodisReadRetries(err) {
				self.err = err
				return
			}
			retryWait = self.fanIn.settings.nextRestartWait(retryWait)
			if glog.V(1) {
				glog.Infof("[front]a whodis socket's read failed, retrying in %s: %s\n", retryWait, frontErrorWithoutAddress(err))
			}
			select {
			case <-self.removed:
				return
			case <-time.After(retryWait):
			}
			continue
		}
		retryWait = 0
		datagram := frontWhodisDatagram{
			data: connect.MessagePoolCopy(scratch[:n]),
			addr: addr,
		}
		select {
		case self.fanIn.datagrams <- datagram:
		case <-self.removed:
			connect.MessagePoolReturn(datagram.data)
			return
		}
	}
}

// A socket's error without the addresses a *net.OpError prints: a read's
// names the socket's own address, and a write's the far end's.
func frontErrorWithoutAddress(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		return opErr.Err
	}
	return err
}

// Whether a read error is one to wait out: what net/http and quic-go read as
// temporary. A closed socket's never is.
func frontWhodisReadRetries(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	var netErr net.Error
	//nolint:staticcheck // SA1019: as net/http and quic-go read a read error
	return errors.As(err, &netErr) && netErr.Temporary()
}

// Closed once the socket is no longer read: its read failed, or it or its
// fan-in was closed.
func (self *frontWhodisFanInMember) Done() <-chan struct{} {
	return self.done
}

// The read error that ended the socket, once Done is closed. Nil before, and
// for a socket that was closed.
func (self *frontWhodisFanInMember) Err() error {
	select {
	case <-self.done:
		return self.err
	default:
		return nil
	}
}

// Takes the socket out of its fan-in, closes it, and waits for its reader to
// return. Safe to call more than once.
func (self *frontWhodisFanInMember) Close() error {
	self.closeOnce.Do(func() {
		self.fanIn.remove(self)
		close(self.removed)
		self.closeErr = self.packetConn.Close()
	})
	<-self.done
	return self.closeErr
}

// net.PacketConn: the next datagram any socket read, cut to p as a socket's
// read cuts it.
func (self *frontWhodisFanIn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case datagram := <-self.datagrams:
		n := copy(p, datagram.data)
		connect.MessagePoolReturn(datagram.data)
		return n, datagram.addr, nil
	case <-self.closed:
		return 0, nil, net.ErrClosed
	}
}

// net.PacketConn: writes through one live socket, picked by a hash of addr. A
// socket closed under the write is skipped for the next one. With no socket
// left the datagram is dropped, and once the fan-in is closed the write fails
// with net.ErrClosed. Every other error is the socket's, unchanged.
func (self *frontWhodisFanIn) WriteTo(p []byte, addr net.Addr) (int, error) {
	members := *self.writeMembers.Load()
	if len(members) != 0 {
		first := int(self.hashAddr(addr) % uint64(len(members)))
		for i := range len(members) {
			member := members[(first+i)%len(members)]
			n, err := member.packetConn.WriteTo(p, addr)
			if errors.Is(err, net.ErrClosed) {
				continue
			}
			return n, err
		}
	}
	select {
	case <-self.closed:
		return 0, net.ErrClosed
	default:
		// no socket to send through: the datagram is lost
		return len(p), nil
	}
}

// The seeded hash of a destination that picks its socket.
func (self *frontWhodisFanIn) hashAddr(addr net.Addr) uint64 {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return maphash.String(self.writeSeed, addr.String())
	}
	addrPort := udpAddr.AddrPort()
	var key [18]byte
	ip := addrPort.Addr().As16()
	copy(key[:16], ip[:])
	binary.BigEndian.PutUint16(key[16:], addrPort.Port())
	return maphash.Bytes(self.writeSeed, key[:])
}

// net.PacketConn: the address every socket of the group is bound to.
func (self *frontWhodisFanIn) LocalAddr() net.Addr {
	return self.localAddr
}

// net.PacketConn: refused, as is every deadline.
func (self *frontWhodisFanIn) SetDeadline(time.Time) error {
	return errFrontWhodisFanInDeadline
}

// net.PacketConn: refused; the translation reads without a deadline.
func (self *frontWhodisFanIn) SetReadDeadline(time.Time) error {
	return errFrontWhodisFanInDeadline
}

// net.PacketConn: refused; each write goes out through one socket.
func (self *frontWhodisFanIn) SetWriteDeadline(time.Time) error {
	return errFrontWhodisFanInDeadline
}

// Sizes the read buffer of every socket of the fan-in, and of each that joins
// later, as the translation passes quic-go's sizing on to its socket.
func (self *frontWhodisFanIn) SetReadBuffer(byteCount int) error {
	members := func() []*frontWhodisFanInMember {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.readBufferByteCount = byteCount
		return slices.Clone(self.members)
	}()
	var errs []error
	for _, member := range members {
		if err := setFrontReadBuffer(member.packetConn, byteCount); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Sizes the write buffer of every socket of the fan-in, and of each that
// joins later, as SetReadBuffer.
func (self *frontWhodisFanIn) SetWriteBuffer(byteCount int) error {
	members := func() []*frontWhodisFanInMember {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.writeBufferByteCount = byteCount
		return slices.Clone(self.members)
	}()
	var errs []error
	for _, member := range members {
		if err := setFrontWriteBuffer(member.packetConn, byteCount); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Sizes a socket's read buffer, when it has one to size.
func setFrontReadBuffer(packetConn net.PacketConn, byteCount int) error {
	conn, ok := packetConn.(interface{ SetReadBuffer(int) error })
	if !ok {
		return errors.New("the socket has no read buffer to size")
	}
	return conn.SetReadBuffer(byteCount)
}

// Sizes a socket's write buffer, when it has one to size.
func setFrontWriteBuffer(packetConn net.PacketConn, byteCount int) error {
	conn, ok := packetConn.(interface{ SetWriteBuffer(int) error })
	if !ok {
		return errors.New("the socket has no write buffer to size")
	}
	return conn.SetWriteBuffer(byteCount)
}

// net.PacketConn: closes every socket of the fan-in, and ends its reads with
// net.ErrClosed. Safe to call more than once.
func (self *frontWhodisFanIn) Close() error {
	members := func() []*frontWhodisFanInMember {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.isClosed {
			return nil
		}
		self.isClosed = true
		close(self.closed)
		return slices.Clone(self.members)
	}()
	for _, member := range members {
		member.Close()
	}
	// what no reader will take any more
	for {
		select {
		case datagram := <-self.datagrams:
			connect.MessagePoolReturn(datagram.data)
		default:
			return nil
		}
	}
}
