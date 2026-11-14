package server

// The direct path to the PgBouncer instances, against nginx's least_conn
// upstream with max_fails=3 fail_timeout=30s, and the single-port path left as
// it was. A scripted dialer and an explicit clock own every outcome; no
// database, listener or wall-clock wait is involved.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A clock the test moves.
type pgBouncerTestClock struct {
	stateLock sync.Mutex
	at        time.Time
}

func newPgBouncerTestClock() *pgBouncerTestClock {
	return &pgBouncerTestClock{at: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
}

func (self *pgBouncerTestClock) now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.at
}

func (self *pgBouncerTestClock) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.at = self.at.Add(duration)
}

// What the scripted dialer does for one port.
type pgBouncerTestOutcome int

const (
	pgBouncerTestAccept pgBouncerTestOutcome = iota
	pgBouncerTestRefuse
	// signals started, then waits for ctx and returns its error
	pgBouncerTestHang
)

// Answers each dial by its port's outcome and records every address dialed.
type pgBouncerTestDialer struct {
	stateLock    sync.Mutex
	portOutcomes map[int]pgBouncerTestOutcome
	addresses    []string
	started      chan string
}

func newPgBouncerTestDialer() *pgBouncerTestDialer {
	return &pgBouncerTestDialer{
		portOutcomes: map[int]pgBouncerTestOutcome{},
		started:      make(chan string, 64),
	}
}

func (self *pgBouncerTestDialer) set(port int, outcome pgBouncerTestOutcome) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.portOutcomes[port] = outcome
}

// The addresses dialed since the last call.
func (self *pgBouncerTestDialer) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	addresses := self.addresses
	self.addresses = nil
	return addresses
}

func (self *pgBouncerTestDialer) dial(ctx context.Context, network string, address string) (net.Conn, error) {
	_, portString, err := net.SplitHostPort(address)
	if err != nil {
		portString = "0"
	}
	port, _ := strconv.Atoi(portString)
	self.stateLock.Lock()
	self.addresses = append(self.addresses, address)
	outcome := self.portOutcomes[port]
	self.stateLock.Unlock()
	remoteAddr, _ := net.ResolveTCPAddr(network, address)
	switch outcome {
	case pgBouncerTestRefuse:
		return nil, &net.OpError{
			Op:   "dial",
			Net:  network,
			Addr: remoteAddr,
			Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		}
	case pgBouncerTestHang:
		self.started <- address
		<-ctx.Done()
		return nil, &net.OpError{Op: "dial", Net: network, Addr: remoteAddr, Err: ctx.Err()}
	default:
		return &pgBouncerTestConn{remoteAddr: remoteAddr}, nil
	}
}

// A connected socket that carries nothing.
type pgBouncerTestConn struct {
	remoteAddr net.Addr
}

var pgBouncerTestLocalAddr = &net.TCPAddr{IP: net.IPv4(192, 0, 2, 200)}

func (self *pgBouncerTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (self *pgBouncerTestConn) Write(b []byte) (int, error)      { return len(b), nil }
func (self *pgBouncerTestConn) Close() error                     { return nil }
func (self *pgBouncerTestConn) LocalAddr() net.Addr              { return pgBouncerTestLocalAddr }
func (self *pgBouncerTestConn) RemoteAddr() net.Addr             { return self.remoteAddr }
func (self *pgBouncerTestConn) SetDeadline(time.Time) error      { return nil }
func (self *pgBouncerTestConn) SetReadDeadline(time.Time) error  { return nil }
func (self *pgBouncerTestConn) SetWriteDeadline(time.Time) error { return nil }

// Settings for count instances on 6433 onward, with nginx's failure rule.
func pgBouncerTestSettings(count int) *PgBouncerDirectSettings {
	settings := DefaultPgBouncerDirectSettings()
	settings.InstanceCount = count
	return settings
}

// The single-port address a pool on 192.0.2.1 dials.
const pgBouncerTestSinglePortAddress = "192.0.2.1:6432"

func pgBouncerTestAddress(port int) string {
	return net.JoinHostPort("192.0.2.1", strconv.Itoa(port))
}

func pgBouncerTestAddresses(ports ...int) []string {
	addresses := []string{}
	for _, port := range ports {
		addresses = append(addresses, pgBouncerTestAddress(port))
	}
	return addresses
}

// One direct dial; the connection stays open.
func pgBouncerTestDial(t *testing.T, dialer *pgBouncerDirectDialer) net.Conn {
	t.Helper()
	conn, err := dialer.dialContext(t.Context(), "tcp", pgBouncerTestSinglePortAddress)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// One direct dial whose connection closes at once.
func pgBouncerTestDialAndClose(t *testing.T, dialer *pgBouncerDirectDialer) {
	t.Helper()
	if err := pgBouncerTestDial(t, dialer).Close(); err != nil {
		t.Fatal(err)
	}
}

// One instance's counts, read under the dialer's lock.
func pgBouncerTestInstance(dialer *pgBouncerDirectDialer, index int) (connectionCount int, failCount int) {
	dialer.stateLock.Lock()
	defer dialer.stateLock.Unlock()
	instance := dialer.instances[index]
	return instance.connectionCount, instance.failCount
}

func pgBouncerTestOut(dialer *pgBouncerDirectDialer, index int) bool {
	dialer.stateLock.Lock()
	defer dialer.stateLock.Unlock()
	return dialer.outWithLock(&dialer.instances[index], dialer.now())
}

// The defaults are nginx.conf.j2's upstream and listen, and playbook-dbs.yml's
// instance count (xops tests/test_pgbouncer_direct_path.py pins that side).
func TestPgBouncerDirectDefaultsMirrorNginx(t *testing.T) {
	settings := DefaultPgBouncerDirectSettings()
	if *settings != (PgBouncerDirectSettings{SinglePort: 6432, InstanceCount: 32, MaxFails: 3, FailTimeout: 30 * time.Second}) {
		t.Fatal("the direct path does not mirror nginx's upstream", *settings)
	}
	if DefaultPgBouncerDirectSettings() == settings {
		t.Fatal("the defaults share one mutable settings value")
	}
}

// Two failed dials leave an instance in; the third takes it out, and the dial
// that failed goes on to the next instance, as proxy_next_upstream does.
func TestPgBouncerDirectThirdFailureTakesInstanceOut(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dial.set(6433, pgBouncerTestRefuse)
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, clock.now, 0)
	for failure := 1; failure <= 3; failure += 1 {
		pgBouncerTestDialAndClose(t, dialer)
		if got, want := dial.take(), pgBouncerTestAddresses(6433, 6434); !reflect.DeepEqual(got, want) {
			t.Fatal("failure", failure, "dialed", got, "want", want)
		}
		if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != failure {
			t.Fatal("failure", failure, "counted", failCount)
		}
		if out := pgBouncerTestOut(dialer, 0); out != (failure == 3) {
			t.Fatal("after", failure, "failures out =", out)
		}
		clock.advance(time.Second)
	}
	pgBouncerTestDialAndClose(t, dialer)
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6434)) {
		t.Fatal("an instance that is out was dialed", got)
	}
	for index := range 2 {
		if connectionCount, _ := pgBouncerTestInstance(dialer, index); connectionCount != 0 {
			t.Fatal("instance", index, "kept", connectionCount, "connections after every dial failed or closed")
		}
	}
}

// An instance that is out stays out until FailTimeout has passed after its
// last failure (nginx compares with <=), then gets one dial, which a failure
// ends with another full window.
func TestPgBouncerDirectOutInstanceWaitsFailTimeout(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dial.set(6433, pgBouncerTestRefuse)
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, clock.now, 0)
	for range 3 {
		pgBouncerTestDialAndClose(t, dialer)
	}
	dial.take()
	clock.advance(30 * time.Second)
	pgBouncerTestDialAndClose(t, dialer)
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6434)) {
		t.Fatal("dialed an instance at the end of its window", got)
	}
	clock.advance(time.Nanosecond)
	pgBouncerTestDialAndClose(t, dialer)
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6433, 6434)) {
		t.Fatal("did not retry the instance after its window", got)
	}
	if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != 4 || !pgBouncerTestOut(dialer, 0) {
		t.Fatal("a failed retry left the instance in", failCount)
	}
	clock.advance(30 * time.Second)
	pgBouncerTestDialAndClose(t, dialer)
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6434)) {
		t.Fatal("a failed retry did not start a new window", got)
	}
}

// After its window an instance gets one dial at a time: the selection makes
// it out again until that dial ends. A dial its caller cancels counts nothing,
// and a connection clears the count, after which it takes three new failures,
// not one, to go out.
func TestPgBouncerDirectConnectionAfterWindowClearsFailures(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dial.set(6433, pgBouncerTestRefuse)
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, clock.now, 0)
	// 6434's connections stay open, so 6433, with fewer, comes first while in
	held := []net.Conn{}
	defer func() {
		for _, conn := range held {
			conn.Close()
		}
	}()
	for range 3 {
		held = append(held, pgBouncerTestDial(t, dialer))
	}
	dial.take()
	clock.advance(31 * time.Second)

	// the instance's retry is held at a barrier
	dial.set(6433, pgBouncerTestHang)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	retry := make(chan error, 1)
	go func() {
		conn, err := dialer.dialContext(ctx, "tcp", pgBouncerTestSinglePortAddress)
		if conn != nil {
			conn.Close()
		}
		retry <- err
	}()
	if got := <-dial.started; got != pgBouncerTestAddress(6433) {
		t.Fatal("the retry went to", got)
	}
	held = append(held, pgBouncerTestDial(t, dialer))
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6433, 6434)) {
		t.Fatal("a second dial reached the instance while its retry was open", got)
	}
	cancel()
	if err := <-retry; !errors.Is(err, context.Canceled) {
		t.Fatal("the held retry ended with", err)
	}
	if connectionCount, failCount := pgBouncerTestInstance(dialer, 0); connectionCount != 0 || failCount != 3 {
		t.Fatal("a canceled retry left connections", connectionCount, "failures", failCount)
	}

	clock.advance(31 * time.Second)
	dial.set(6433, pgBouncerTestAccept)
	conn := pgBouncerTestDial(t, dialer)
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6433)) {
		t.Fatal("the retry dialed", got)
	}
	if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != 0 || pgBouncerTestOut(dialer, 0) {
		t.Fatal("a connection after the window left the count at", failCount)
	}
	conn.Close()

	dial.set(6433, pgBouncerTestRefuse)
	for failure := 1; failure <= 3; failure += 1 {
		held = append(held, pgBouncerTestDial(t, dialer))
		if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6433, 6434)) {
			t.Fatal("failure", failure, "after recovery dialed", got)
		}
		if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != failure || pgBouncerTestOut(dialer, 0) != (failure == 3) {
			t.Fatal("failure", failure, "after recovery counted", failCount)
		}
		clock.advance(time.Second)
	}
}

// nginx clears a count only on a connection to an instance selected more than
// fail_timeout after its last failure. Failures closer together than that add
// up across connections; a connection after a quiet window starts over.
func TestPgBouncerDirectFailuresCloseTogetherAddUpAcrossConnections(t *testing.T) {
	clock := newPgBouncerTestClock()
	start := clock.now()
	dial := newPgBouncerTestDialer()
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, clock.now, 0)
	// 6434's connections stay open, so 6433, with fewer, comes first while in
	held := []net.Conn{}
	defer func() {
		for _, conn := range held {
			conn.Close()
		}
	}()
	step := func(at time.Duration, outcome pgBouncerTestOutcome) {
		t.Helper()
		clock.advance(start.Add(at).Sub(clock.now()))
		dial.set(6433, outcome)
		conn := pgBouncerTestDial(t, dialer)
		if got := dial.take(); got[0] != pgBouncerTestAddress(6433) {
			t.Fatal("at", at, "the dial skipped 6433:", got)
		}
		if outcome == pgBouncerTestAccept {
			conn.Close()
		} else {
			held = append(held, conn)
		}
	}
	step(0, pgBouncerTestRefuse)
	step(10*time.Second, pgBouncerTestAccept)
	step(20*time.Second, pgBouncerTestRefuse)
	step(25*time.Second, pgBouncerTestAccept)
	step(40*time.Second, pgBouncerTestRefuse)
	if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != 3 || !pgBouncerTestOut(dialer, 0) {
		t.Fatal("failures 20 s apart, 40 s from first to last, counted", failCount)
	}

	step(71*time.Second, pgBouncerTestAccept)
	if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != 0 {
		t.Fatal("a connection after a quiet window left the count at", failCount)
	}
	step(80*time.Second, pgBouncerTestRefuse)
	step(81*time.Second, pgBouncerTestRefuse)
	if _, failCount := pgBouncerTestInstance(dialer, 0); failCount != 2 || pgBouncerTestOut(dialer, 0) {
		t.Fatal("the count did not start over", failCount)
	}
}

// The fewest connections first, equals in turn from the pool's first index:
// counts never differ by more than one, a closed connection's instance is
// refilled first, and pools with different first indexes share the instances
// evenly even when each opens fewer connections than there are instances.
func TestPgBouncerDirectEvenSpread(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(32), dial.dial, clock.now, 7)
	instanceConns := map[string][]net.Conn{}
	for range 64 {
		conn := pgBouncerTestDial(t, dialer)
		address := conn.RemoteAddr().String()
		instanceConns[address] = append(instanceConns[address], conn)
	}
	if got := dial.take()[0]; got != pgBouncerTestAddress(6440) {
		t.Fatal("the first connection did not go to the first index's instance", got)
	}
	for index := range 32 {
		if connectionCount, _ := pgBouncerTestInstance(dialer, index); connectionCount != 2 {
			t.Fatal("instance", index, "holds", connectionCount, "of 64 connections over 32 instances")
		}
	}
	for _, conn := range instanceConns[pgBouncerTestAddress(6438)] {
		conn.Close()
	}
	for range 2 {
		pgBouncerTestDial(t, dialer)
	}
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6438, 6438)) {
		t.Fatal("closed connections were not refilled on their instance first", got)
	}

	portCounts := map[int]int{}
	for firstIndex := range 32 {
		dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(32), dial.dial, clock.now, firstIndex)
		for range 5 {
			conn := pgBouncerTestDial(t, dialer)
			portCounts[conn.RemoteAddr().(*net.TCPAddr).Port] += 1
		}
	}
	for port := 6433; port <= 6464; port += 1 {
		if portCounts[port] != 5 {
			t.Fatal("32 pools of 5 connections gave port", port, portCounts[port], "connections:", portCounts)
		}
	}
}

// When every instance fails, a dial tries each once and returns the last
// error. Once every instance is out, where nginx refuses with "no live
// upstreams", a dial still tries them, so the first instance back serves the
// next dial instead of one up to a window later.
func TestPgBouncerDirectEveryInstanceDown(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	for port := 6433; port <= 6436; port += 1 {
		dial.set(port, pgBouncerTestRefuse)
	}
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(4), dial.dial, clock.now, 2)
	for attempt := 1; attempt <= 4; attempt += 1 {
		conn, err := dialer.dialContext(t.Context(), "tcp", pgBouncerTestSinglePortAddress)
		if conn != nil || !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatal("dial", attempt, "with every instance down returned", conn, err)
		}
		if got, want := dial.take(), pgBouncerTestAddresses(6435, 6436, 6433, 6434); !reflect.DeepEqual(got, want) {
			t.Fatal("dial", attempt, "tried", got, "want each instance once:", want)
		}
		clock.advance(time.Second)
	}
	for index := range 4 {
		connectionCount, failCount := pgBouncerTestInstance(dialer, index)
		if connectionCount != 0 || failCount != 4 || !pgBouncerTestOut(dialer, index) {
			t.Fatal("instance", index, "connections", connectionCount, "failures", failCount)
		}
	}
	dial.set(6434, pgBouncerTestAccept)
	conn, err := dialer.dialContext(t.Context(), "tcp", pgBouncerTestSinglePortAddress)
	if err != nil || conn.RemoteAddr().String() != pgBouncerTestAddress(6434) {
		t.Fatal("the instance that came back did not serve the next dial", conn, err)
	}
	conn.Close()
}

// An instance that is in comes before every instance that is out, even one
// with fewer connections.
func TestPgBouncerDirectInInstancesComeBeforeOutInstances(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(3), dial.dial, clock.now, 0)
	held := []net.Conn{}
	for range 6 {
		held = append(held, pgBouncerTestDial(t, dialer))
	}
	dial.take()
	// 6433 loses its connections and fails three dials: out with none open
	for _, conn := range held {
		if conn.RemoteAddr().String() == pgBouncerTestAddress(6433) {
			conn.Close()
		}
	}
	dial.set(6433, pgBouncerTestRefuse)
	for range 3 {
		held = append(held, pgBouncerTestDial(t, dialer))
	}
	dial.take()
	if connectionCount, _ := pgBouncerTestInstance(dialer, 0); connectionCount != 0 || !pgBouncerTestOut(dialer, 0) {
		t.Fatal("the setup did not take 6433 out with no connections")
	}
	held = append(held, pgBouncerTestDial(t, dialer))
	if got := dial.take(); len(got) != 1 || got[0] == pgBouncerTestAddress(6433) {
		t.Fatal("an out instance came before the instances that are in", got)
	}
	for _, conn := range held {
		conn.Close()
	}
}

// A dial its caller cancels returns at once, tries no other instance and
// counts nothing against the instance.
func TestPgBouncerDirectCanceledDialCountsNothing(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dial.set(6433, pgBouncerTestHang)
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, clock.now, 0)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		conn, err := dialer.dialContext(ctx, "tcp", pgBouncerTestSinglePortAddress)
		if conn != nil {
			conn.Close()
		}
		result <- err
	}()
	<-dial.started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal("a canceled dial returned", err)
	}
	if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6433)) {
		t.Fatal("a canceled dial went on to", got)
	}
	if connectionCount, failCount := pgBouncerTestInstance(dialer, 0); connectionCount != 0 || failCount != 0 {
		t.Fatal("a canceled dial left connections", connectionCount, "failures", failCount)
	}
}

// The connect's deadline, connect_timeout, ends a dial that never answers. It
// counts against that instance, as nginx counts proxy_connect_timeout, and
// leaves no time for another instance.
func TestPgBouncerDirectConnectDeadlineCountsAgainstInstance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dial := newPgBouncerTestDialer()
		dial.set(6433, pgBouncerTestHang)
		dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, time.Now, 0)
		ctx, cancel := context.WithTimeout(t.Context(), PgConnectTimeout)
		defer cancel()
		started := time.Now()
		conn, err := dialer.dialContext(ctx, "tcp", pgBouncerTestSinglePortAddress)
		if conn != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != PgConnectTimeout {
			t.Fatal("a dial past the connect deadline returned", conn, err, "after", time.Since(started))
		}
		<-dial.started
		if got := dial.take(); !reflect.DeepEqual(got, pgBouncerTestAddresses(6433)) {
			t.Fatal("a dial past its deadline went on to", got)
		}
		if connectionCount, failCount := pgBouncerTestInstance(dialer, 0); connectionCount != 0 || failCount != 1 {
			t.Fatal("a timed-out dial left connections", connectionCount, "failures", failCount)
		}
	})
}

// Any address but the single port is dialed as it is: a cancel request goes
// to the instance its connection is on. IPv6 hosts keep their brackets.
func TestPgBouncerDirectPassesOtherAddressesThrough(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(4), dial.dial, clock.now, 1)
	for _, address := range []string{"192.0.2.1:6435", "192.0.2.1:5432", "[2001:db8::1]:6433", "/var/run/postgresql/.s.PGSQL.6432"} {
		conn, err := dialer.dialContext(t.Context(), "tcp", address)
		if err != nil {
			t.Fatal(address, err)
		}
		conn.Close()
		if got := dial.take(); !reflect.DeepEqual(got, []string{address}) {
			t.Fatal("dialing", address, "dialed", got)
		}
	}
	for index := range 4 {
		if connectionCount, failCount := pgBouncerTestInstance(dialer, index); connectionCount != 0 || failCount != 0 {
			t.Fatal("a passed-through dial changed instance", index)
		}
	}
	conn, err := dialer.dialContext(t.Context(), "tcp", "[2001:db8::1]:6432")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := dial.take(); !reflect.DeepEqual(got, []string{"[2001:db8::1]:6434"}) {
		t.Fatal("an IPv6 single port dialed", got)
	}
}

// Concurrent dials, some failing, leave each instance counting exactly its
// open connections, and none once they close.
func TestPgBouncerDirectConcurrentDialsKeepCounts(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dial.set(6434, pgBouncerTestRefuse)
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(8), dial.dial, clock.now, 3)
	conns := make(chan net.Conn, 256)
	var group sync.WaitGroup
	for range 256 {
		group.Go(func() {
			conn, err := dialer.dialContext(t.Context(), "tcp", pgBouncerTestSinglePortAddress)
			if err != nil {
				t.Error(err)
				return
			}
			conns <- conn
		})
	}
	group.Wait()
	close(conns)
	portCounts := map[int]int{}
	open := []net.Conn{}
	for conn := range conns {
		portCounts[conn.RemoteAddr().(*net.TCPAddr).Port] += 1
		open = append(open, conn)
	}
	for index := range 8 {
		connectionCount, _ := pgBouncerTestInstance(dialer, index)
		if connectionCount != portCounts[6433+index] || (index == 1) != (connectionCount == 0) {
			t.Fatal("instance", index, "counts", connectionCount, "for", portCounts[6433+index], "open connections")
		}
	}
	for _, conn := range open {
		conn.Close()
	}
	for index := range 8 {
		if connectionCount, _ := pgBouncerTestInstance(dialer, index); connectionCount != 0 {
			t.Fatal("instance", index, "counts", connectionCount, "after every connection closed")
		}
	}
}

// pgx can close a connection more than once; the count comes back once.
func TestPgBouncerDirectCloseReleasesOnce(t *testing.T) {
	clock := newPgBouncerTestClock()
	dial := newPgBouncerTestDialer()
	dialer := newPgBouncerDirectDialer(pgBouncerTestSettings(2), dial.dial, clock.now, 0)
	conn := pgBouncerTestDial(t, dialer)
	other := pgBouncerTestDial(t, dialer)
	defer other.Close()
	conn.Close()
	conn.Close()
	if connectionCount, _ := pgBouncerTestInstance(dialer, 0); connectionCount != 0 {
		t.Fatal("two closes left", connectionCount)
	}
	if connectionCount, _ := pgBouncerTestInstance(dialer, 1); connectionCount != 1 {
		t.Fatal("closing one instance's connection changed another's count to", connectionCount)
	}
}

// The single port is the default and changes nothing in a pool's
// configuration; the direct path changes only a pool on the single port.
func TestPgBouncerSinglePortPathLeavesPoolConfigUnchanged(t *testing.T) {
	if currentPgBouncerPath() != PgBouncerPathSinglePort {
		t.Fatal("a process starts on", currentPgBouncerPath())
	}
	cases := []struct {
		path    PgBouncerPath
		port    uint16
		changed bool
	}{
		{path: PgBouncerPathSinglePort, port: 6432, changed: false},
		{path: PgBouncerPathSinglePort, port: 5432, changed: false},
		{path: PgBouncerPathDirect, port: 5432, changed: false},
		{path: PgBouncerPathDirect, port: 6433, changed: false},
		{path: PgBouncerPathDirect, port: 6432, changed: true},
	}
	for _, c := range cases {
		config, err := pgxpool.ParseConfig(
			"postgres://synthetic:synthetic@192.0.2.1:" + strconv.Itoa(int(c.port)) + "/synthetic?sslmode=disable&connect_timeout=5",
		)
		if err != nil {
			t.Fatal(err)
		}
		dialFunc := reflect.ValueOf(config.ConnConfig.DialFunc).Pointer()
		lookupFunc := reflect.ValueOf(config.ConnConfig.LookupFunc).Pointer()
		dialer := configurePgBouncerPath(config, c.path, DefaultPgBouncerDirectSettings())
		changed := reflect.ValueOf(config.ConnConfig.DialFunc).Pointer() != dialFunc
		if changed != c.changed || (dialer != nil) != c.changed {
			t.Fatal(c.path, "on port", c.port, "changed the dialer:", changed)
		}
		if reflect.ValueOf(config.ConnConfig.LookupFunc).Pointer() != lookupFunc || config.ConnConfig.Port != c.port {
			t.Fatal(c.path, "on port", c.port, "changed the lookup or the authority")
		}
	}
}

// The base the wire fixture dials through: every address is recorded, and a
// connection reports the TCP address it was dialed at, as a socket does, so
// a cancel request follows it.
func pgBouncerTestWireDial(fixture *pgPoolWireFixture, stateLock *sync.Mutex, addresses *[]string) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network string, address string) (net.Conn, error) {
		stateLock.Lock()
		*addresses = append(*addresses, address)
		stateLock.Unlock()
		conn, err := fixture.dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		remoteAddr, err := net.ResolveTCPAddr(network, address)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return &pgBouncerTestWireConn{Conn: conn, remoteAddr: remoteAddr}, nil
	}
}

type pgBouncerTestWireConn struct {
	net.Conn
	remoteAddr net.Addr
}

func (self *pgBouncerTestWireConn) RemoteAddr() net.Addr { return self.remoteAddr }

// Opens count pool connections at once over the real pgx wire protocol and
// cancels through the first. Returns the addresses dialed, in order.
func pgBouncerTestPoolDials(t *testing.T, path PgBouncerPath, count int) (connectAddresses []string, cancelAddress string, firstRemote string) {
	t.Helper()
	var stateLock sync.Mutex
	addresses := []string{}
	_, pool := newPgPoolWireFixture(t, nil, neverPingTestPool, func(fixture *pgPoolWireFixture, config *pgxpool.Config) {
		config.MaxConns = int32(count)
		config.ConnConfig.Port = 6432
		config.ConnConfig.DialFunc = pgBouncerTestWireDial(fixture, &stateLock, &addresses)
		configurePgBouncerPath(config, path, pgBouncerTestSettings(count))
	})
	conns := []*pgxpool.Conn{}
	for range count {
		conn, err := pool.open().Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
	}
	stateLock.Lock()
	connectAddresses = append([]string{}, addresses...)
	stateLock.Unlock()
	firstRemote = conns[0].Conn().PgConn().Conn().RemoteAddr().String()
	if err := conns[0].Conn().PgConn().CancelRequest(t.Context()); err != nil {
		t.Fatal(err)
	}
	stateLock.Lock()
	cancelAddress = addresses[len(addresses)-1]
	stateLock.Unlock()
	for _, conn := range conns {
		conn.Release()
	}
	return connectAddresses, cancelAddress, firstRemote
}

// A pool on the direct path opens one connection on each instance and sends a
// cancel request to the instance its connection is on; nothing dials the
// single port.
func TestPgBouncerDirectPoolSpreadsConnectionsAndCancelsOnTheirInstance(t *testing.T) {
	connectAddresses, cancelAddress, firstRemote := pgBouncerTestPoolDials(t, PgBouncerPathDirect, 4)
	ports := map[string]bool{}
	for _, address := range connectAddresses {
		ports[address] = true
	}
	if len(connectAddresses) != 4 || !reflect.DeepEqual(ports, map[string]bool{
		pgBouncerTestAddress(6433): true,
		pgBouncerTestAddress(6434): true,
		pgBouncerTestAddress(6435): true,
		pgBouncerTestAddress(6436): true,
	}) {
		t.Fatal("four direct pool connections dialed", connectAddresses)
	}
	if cancelAddress != firstRemote || cancelAddress == pgBouncerTestSinglePortAddress {
		t.Fatal("a cancel request for a connection on", firstRemote, "went to", cancelAddress)
	}
}

// A pool on the single port dials the authority for every connection and
// every cancel request, as before.
func TestPgBouncerSinglePortPoolDialsTheAuthority(t *testing.T) {
	connectAddresses, cancelAddress, _ := pgBouncerTestPoolDials(t, PgBouncerPathSinglePort, 4)
	for _, address := range append(connectAddresses, cancelAddress) {
		if address != pgBouncerTestSinglePortAddress {
			t.Fatal("a single-port pool dialed", address, "of", connectAddresses, cancelAddress)
		}
	}
	if len(connectAddresses) != 4 {
		t.Fatal("four single-port pool connections dialed", connectAddresses)
	}
}

// The process's choice reaches the pool `Db` opens, and a change resets it.
// The maintenance pool, whose sessions bypass PgBouncer, never goes direct,
// even on the single port. The resources are synthetic overrides and the
// pools open no connection.
func TestSetPgBouncerPathReachesTheOpenedPool(t *testing.T) {
	pg := []byte("authority: 192.0.2.10:6432\nuser: synthetic\npassword: synthetic\ndb: synthetic\n")
	pool := []byte("min_connections: 0\nmax_connections: 1\n")
	t.Cleanup(Vault.PushSimpleResource(DefaultPgVaultResourceName, pg))
	t.Cleanup(Vault.PushSimpleResource(MaintenancePgVaultResourceName, pg))
	t.Cleanup(Config.PushSimpleResource(DefaultPgConfigResourceName, pool))
	t.Cleanup(Config.PushSimpleResource(MaintenancePgConfigResourceName, pool))
	t.Cleanup(func() {
		SetPgBouncerPath(PgBouncerPathSinglePort)
		PgReset()
	})
	PgReset()
	directDialFunc := reflect.ValueOf((&pgBouncerDirectDialer{}).dialContext).Pointer()
	dialFunc := func(pool *safePgPool) uintptr {
		return reflect.ValueOf(pool.open().Config().ConnConfig.DialFunc).Pointer()
	}
	if dialFunc(safePool) == directDialFunc || dialFunc(safeMaintenancePool) == directDialFunc {
		t.Fatal("a process that chose nothing dials the instances")
	}
	SetPgBouncerPath(PgBouncerPathDirect)
	if dialFunc(safePool) != directDialFunc {
		t.Fatal("a process that chose the direct path dials the single port")
	}
	if dialFunc(safeMaintenancePool) == directDialFunc {
		t.Fatal("the maintenance pool went through the instances")
	}
	SetPgBouncerPath(PgBouncerPathSinglePort)
	if dialFunc(safePool) == directDialFunc {
		t.Fatal("going back to the single port kept the open direct pool")
	}
}
