//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd || solaris || illumos

package pool

// URNETWORK (third_party/go-redis/URNETWORK.md): Get hands out a connection
// put back within SkipConnCheckWithin without its socket check; checks one
// put back longer ago, replacing it when its peer closed it; checks every
// connection for the window after a removal or an unusable connection; and
// keeps the lifetime and idle checks. Each test counts the socket checks
// through the connection's RawConn.Read, which connCheck's peek goes through,
// over real loopback TCP sockets, so a peer's close reaches the socket as a
// Redis server's does.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A loopback TCP peer whose accepted connections a test can close.
type urnetworkPeer struct {
	listener net.Listener
	accepted chan net.Conn
	// every connection the pool dialed, in order, with its count of checks
	dialsLock sync.Mutex
	dials     []*urnetworkCountedConn
}

// A peer listening until the test ends.
func newUrnetworkPeer(t testing.TB) *urnetworkPeer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	peer := &urnetworkPeer{
		listener: listener,
		accepted: make(chan net.Conn, 16),
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			peer.accepted <- conn
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		for {
			select {
			case conn := <-peer.accepted:
				conn.Close()
			default:
				return
			}
		}
	})
	return peer
}

// The pool's dialer: a loopback connection whose socket checks are counted.
func (self *urnetworkPeer) dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", self.listener.Addr().String())
	if err != nil {
		return nil, err
	}
	countedConn := &urnetworkCountedConn{
		Conn: conn,
	}
	self.dialsLock.Lock()
	defer self.dialsLock.Unlock()
	self.dials = append(self.dials, countedConn)
	return countedConn, nil
}

// The connections dialed so far.
func (self *urnetworkPeer) dialed() []*urnetworkCountedConn {
	self.dialsLock.Lock()
	defer self.dialsLock.Unlock()
	return append([]*urnetworkCountedConn(nil), self.dials...)
}

// Closes the peer's side of the next connection it accepted, and waits until
// the client's socket reads its close.
func (self *urnetworkPeer) closeNext(t *testing.T, client *urnetworkCountedConn) {
	t.Helper()
	select {
	case conn := <-self.accepted:
		conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("the peer accepted nothing")
	}
	// connCheck only peeks, so polling it doesn't count or consume
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(connCheck(client.Conn), io.EOF) {
		if time.Now().After(deadline) {
			t.Fatal("the client never read the peer's close")
		}
		time.Sleep(time.Millisecond)
	}
}

// A client connection whose socket checks are counted.
type urnetworkCountedConn struct {
	net.Conn
	checks atomic.Int64
}

// See syscall.Conn.
func (self *urnetworkCountedConn) SyscallConn() (syscall.RawConn, error) {
	rawConn, err := self.Conn.(syscall.Conn).SyscallConn()
	if err != nil {
		return nil, err
	}
	return &urnetworkCountedRawConn{
		RawConn: rawConn,
		checks:  &self.checks,
	}, nil
}

// A RawConn that counts its reads, the socket checks.
type urnetworkCountedRawConn struct {
	syscall.RawConn
	checks *atomic.Int64
}

// See syscall.RawConn.
func (self *urnetworkCountedRawConn) Read(f func(fd uintptr) bool) error {
	self.checks.Add(1)
	return self.RawConn.Read(f)
}

// A pool of the peer's connections, with window as SkipConnCheckWithin and
// options' other fields, closed when the test ends.
func newUrnetworkPool(t *testing.T, peer *urnetworkPeer, window time.Duration, configure func(options *Options)) *ConnPool {
	t.Helper()
	options := &Options{
		Dialer:              peer.dial,
		PoolSize:            4,
		MaxConcurrentDials:  4,
		PoolTimeout:         5 * time.Second,
		DialTimeout:         5 * time.Second,
		DialerRetries:       1,
		SkipConnCheckWithin: window,
	}
	if configure != nil {
		configure(options)
	}
	connPool := NewConnPool(options)
	t.Cleanup(func() {
		connPool.Close()
	})
	return connPool
}

// One Get, which must succeed.
func getUrnetworkConn(t *testing.T, connPool *ConnPool) *Conn {
	t.Helper()
	cn, err := connPool.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return cn
}

// The counted connection under cn.
func urnetworkCounted(t *testing.T, cn *Conn) *urnetworkCountedConn {
	t.Helper()
	countedConn, ok := cn.getNetConn().(*urnetworkCountedConn)
	if !ok {
		t.Fatalf("the pool's connection is a %T", cn.getNetConn())
	}
	return countedConn
}

// A connection put back within the window is handed out unchecked, again
// and again; one put back longer ago is checked; and with no window, as
// upstream, every Get from the idle list checks.
func TestUrnetworkGetSkipsTheCheckOfAConnPutBackWithinTheWindow(t *testing.T) {
	peer := newUrnetworkPeer(t)
	connPool := newUrnetworkPool(t, peer, time.Second, nil)

	cn := getUrnetworkConn(t, connPool)
	countedConn := urnetworkCounted(t, cn)
	for range 3 {
		connPool.Put(context.Background(), cn)
		cn = getUrnetworkConn(t, connPool)
		if urnetworkCounted(t, cn) != countedConn {
			t.Fatal("the pool handed out another connection")
		}
	}
	if checks := countedConn.checks.Load(); checks != 0 {
		t.Fatalf("%d checks of a connection put back within the window, want 0", checks)
	}

	connPool.Put(context.Background(), cn)
	cn.SetLastPutAtNs(time.Now().Add(-2 * time.Second).UnixNano())
	cn = getUrnetworkConn(t, connPool)
	if checks := countedConn.checks.Load(); checks != 1 {
		t.Fatalf("%d checks of a connection put back past the window, want 1", checks)
	}
	connPool.Put(context.Background(), cn)

	upstreamPeer := newUrnetworkPeer(t)
	upstreamPool := newUrnetworkPool(t, upstreamPeer, 0, nil)
	cn = getUrnetworkConn(t, upstreamPool)
	for range 3 {
		upstreamPool.Put(context.Background(), cn)
		cn = getUrnetworkConn(t, upstreamPool)
	}
	if checks := urnetworkCounted(t, cn).checks.Load(); checks != 3 {
		t.Fatalf("%d checks without a window, want 3", checks)
	}
}

// A connection the peer closed while it sat in the pool past the window is
// checked, found closed and replaced by a new dial, so no command ever sees
// it; and every Get then checks for the window, a connection put back within
// it included.
func TestUrnetworkGetReplacesAConnClosedPastTheWindow(t *testing.T) {
	peer := newUrnetworkPeer(t)
	connPool := newUrnetworkPool(t, peer, time.Second, nil)

	first := getUrnetworkConn(t, connPool)
	second := getUrnetworkConn(t, connPool)
	connPool.Put(context.Background(), second)
	connPool.Put(context.Background(), first)
	firstCounted := urnetworkCounted(t, first)
	// the peer accepted the first dial first
	peer.closeNext(t, firstCounted)
	first.SetLastPutAtNs(time.Now().Add(-2 * time.Second).UnixNano())

	// LIFO: the first connection, put back last, comes out first, is checked
	// and closed, and the second follows
	cn := getUrnetworkConn(t, connPool)
	if checks := firstCounted.checks.Load(); checks != 1 {
		t.Fatalf("%d checks of the closed connection, want 1", checks)
	}
	if !first.IsClosed() {
		t.Fatal("the closed connection was kept")
	}
	if got := urnetworkCounted(t, cn); got == firstCounted {
		t.Fatal("the pool handed out the closed connection")
	}
	// the second, put back within the window, was checked all the same
	if checks := urnetworkCounted(t, cn).checks.Load(); checks != 1 {
		t.Fatalf("%d checks of the next connection after a closed one, want 1", checks)
	}
	connPool.Put(context.Background(), cn)
}

// A connection the peer closed within the window is handed out unchecked:
// the command it serves finds the close. Its removal, as the client's after a
// failed command, has every Get check for the window, so no second command
// meets a closed connection; after the window, Get skips again.
func TestUrnetworkARemovalHasEveryGetCheckForTheWindow(t *testing.T) {
	peer := newUrnetworkPeer(t)
	window := 200 * time.Millisecond
	connPool := newUrnetworkPool(t, peer, window, nil)

	first := getUrnetworkConn(t, connPool)
	second := getUrnetworkConn(t, connPool)
	connPool.Put(context.Background(), second)
	connPool.Put(context.Background(), first)
	firstCounted := urnetworkCounted(t, first)
	peer.closeNext(t, firstCounted)
	// the close came within the window: keep the put time fresh
	first.SetLastPutAtNs(time.Now().UnixNano())

	cn := getUrnetworkConn(t, connPool)
	if urnetworkCounted(t, cn) != firstCounted || firstCounted.checks.Load() != 0 {
		t.Fatal("the connection put back within the window wasn't handed out unchecked")
	}
	// the command's read finds the close
	if _, err := firstCounted.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the closed connection read %v, want EOF", err)
	}
	connPool.Remove(context.Background(), cn, io.EOF)

	cn = getUrnetworkConn(t, connPool)
	secondCounted := urnetworkCounted(t, cn)
	if secondCounted == firstCounted {
		t.Fatal("the removed connection was handed out")
	}
	if checks := secondCounted.checks.Load(); checks != 1 {
		t.Fatalf("%d checks of a connection put back within the window after a removal, want 1", checks)
	}
	connPool.Put(context.Background(), cn)

	time.Sleep(window + 50*time.Millisecond)
	cn.SetLastPutAtNs(time.Now().UnixNano())
	cn = getUrnetworkConn(t, connPool)
	if checks := urnetworkCounted(t, cn).checks.Load(); checks != 1 {
		t.Fatalf("%d checks once the window after the removal passed, want still 1", checks)
	}
	connPool.Put(context.Background(), cn)
}

// A put time later than the clock, after the clock went back, never skips
// the check; and a connection put back within the window keeps the lifetime
// and idle checks, which close it unchecked when it is over either.
func TestUrnetworkGetKeepsTheOtherChecks(t *testing.T) {
	peer := newUrnetworkPeer(t)
	connPool := newUrnetworkPool(t, peer, time.Second, nil)
	cn := getUrnetworkConn(t, connPool)
	connPool.Put(context.Background(), cn)
	cn.SetLastPutAtNs(time.Now().Add(time.Hour).UnixNano())
	cn = getUrnetworkConn(t, connPool)
	if checks := urnetworkCounted(t, cn).checks.Load(); checks != 1 {
		t.Fatalf("%d checks of a connection put back in the future, want 1", checks)
	}
	connPool.Put(context.Background(), cn)

	for _, c := range []struct {
		what      string
		configure func(options *Options)
		age       func(cn *Conn)
	}{
		{
			what: "over its lifetime",
			configure: func(options *Options) {
				options.ConnMaxLifetime = time.Hour
			},
			age: func(cn *Conn) {
				cn.expiresAt = time.Now().Add(-time.Second)
			},
		},
		{
			what: "idle too long",
			configure: func(options *Options) {
				options.ConnMaxIdleTime = time.Minute
			},
			age: func(cn *Conn) {
				cn.SetUsedAtNs(time.Now().Add(-2 * time.Minute).UnixNano())
			},
		},
	} {
		agedPeer := newUrnetworkPeer(t)
		agedPool := newUrnetworkPool(t, agedPeer, time.Second, c.configure)
		aged := getUrnetworkConn(t, agedPool)
		agedPool.Put(context.Background(), aged)
		c.age(aged)
		cn := getUrnetworkConn(t, agedPool)
		if cn == aged || !aged.IsClosed() {
			t.Errorf("%s: the pool handed out the connection", c.what)
		}
		if len(agedPeer.dialed()) != 2 {
			t.Errorf("%s: %d dials, want 2", c.what, len(agedPeer.dialed()))
		}
		agedPool.Put(context.Background(), cn)
	}
}

// One checkout and return of an idle connection, the pool's work around each
// command, with the socket check (no window, as upstream) and without it (a
// connection put back within the window): serially over one connection, and
// from GOMAXPROCS goroutines over as many. Nothing blocks, so ns/op is the
// CPU each checkout costs. The connections are real TCP sockets: to the
// address in URNETWORK_BENCH_PEER_ADDR, such as the local Redis, which they
// never write to, or else to a loopback listener.
func BenchmarkUrnetworkGetPut(b *testing.B) {
	peerAddr := os.Getenv("URNETWORK_BENCH_PEER_ADDR")
	for _, c := range []struct {
		name     string
		window   time.Duration
		parallel bool
	}{
		{name: "check_every_checkout", window: 0},
		{name: "skip_within_1s", window: time.Second},
		{name: "check_every_checkout_parallel", window: 0, parallel: true},
		{name: "skip_within_1s_parallel", window: time.Second, parallel: true},
	} {
		b.Run(c.name, func(b *testing.B) {
			dial := func(ctx context.Context) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "tcp", peerAddr)
			}
			if peerAddr == "" {
				dial = newUrnetworkPeer(b).dial
			}
			poolSize := 1
			if c.parallel {
				poolSize = runtime.GOMAXPROCS(0)
			}
			connPool := NewConnPool(&Options{
				Dialer:              dial,
				PoolSize:            int32(poolSize),
				MaxConcurrentDials:  poolSize,
				PoolTimeout:         5 * time.Second,
				DialTimeout:         5 * time.Second,
				DialerRetries:       1,
				SkipConnCheckWithin: c.window,
			})
			defer connPool.Close()
			ctx := context.Background()
			// every connection dialed and put back before the timing
			conns := []*Conn{}
			for range poolSize {
				cn, err := connPool.Get(ctx)
				if err != nil {
					b.Fatal(err)
				}
				conns = append(conns, cn)
			}
			for _, cn := range conns {
				connPool.Put(ctx, cn)
			}
			b.ReportAllocs()
			b.ResetTimer()
			if c.parallel {
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						cn, err := connPool.Get(ctx)
						if err != nil {
							b.Error(err)
							return
						}
						connPool.Put(ctx, cn)
					}
				})
				return
			}
			for range b.N {
				cn, err := connPool.Get(ctx)
				if err != nil {
					b.Fatal(err)
				}
				connPool.Put(ctx, cn)
			}
		})
	}
}
