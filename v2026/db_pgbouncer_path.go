package server

// A main pool reaches PgBouncer one of two ways. pg.yml's authority names the
// database host's single port, where nginx's stream balancer hands each new
// connection to one of the PgBouncer instances listening on the ports after it
// (xops main/ansible: pgbouncer_count in playbook-dbs.yml, listen_port in
// pgbouncer.ini.j2, the upstream in nginx.conf.j2). Or the pool dials those
// instances itself and skips the hop, which cost edge-2 2.2 cores, 79% of it
// in the kernel, about 42 µs per query (CPUOPT5 Part D #2).
//
// The process's code chooses, with `SetPgBouncerPath` in its main: a service
// can go direct. A process that chooses nothing keeps the single port, which
// stays as it was. Tools such as bringyourctl need it: operators run them
// through a tunnel that forwards only that port. The direct path applies only
// to a pool whose authority is the single port, so a pool that names
// PostgreSQL itself (the maintenance pool, the local environment) dials its
// authority as before.
//
// A direct dial picks instances as nginx's least_conn upstream does with
// max_fails=3 fail_timeout=30s (ngx_stream_upstream_least_conn_module.c,
// ngx_stream_upstream_round_robin.c): the instance with the fewest of this
// pool's connections, equals in turn; after a failed dial the next instance,
// each at most once, as proxy_next_upstream does; an instance whose failures
// reach the limit is out until a window passes after its last one, and is
// then tried one dial at a time until a connection to it clears its count.
// Where nginx refuses a session because every instance is out ("no live
// upstreams"), a direct dial tries the out instances as well. Seen from a
// service host, every instance failing at once is most often the network, and
// refusing would keep the database closed for up to a window after the
// network recovers, which nginx, beside the instances on loopback, never saw.
//
// No timeout changes. Every attempt runs in the connect's own context and
// through the pool's own dialer, so connect_timeout still bounds the whole
// connect and each dial. nginx's proxy_timeout (300m) closed a client idle that
// long; PgBouncer's client_idle_timeout closes a direct client at the same
// bound (xops pgbouncer.ini.j2).

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/urnetwork/glog/v2026"
)

// A main pool's way to PgBouncer.
type PgBouncerPath int32

const (
	// nginx on the single port picks the instance. Every process starts here.
	PgBouncerPathSinglePort PgBouncerPath = 0
	// The process picks an instance and dials it, past nginx. The connection
	// still goes through PgBouncer, unlike the maintenance pool's direct
	// PostgreSQL sessions.
	PgBouncerPathDirect PgBouncerPath = 1
)

// For logs.
func (self PgBouncerPath) String() string {
	switch self {
	case PgBouncerPathSinglePort:
		return "single port"
	case PgBouncerPathDirect:
		return "direct"
	default:
		return fmt.Sprintf("unknown (%d)", int32(self))
	}
}

// the path of the pools this process opens
var pgBouncerPath atomic.Int32

// Chooses the path of the pools this process opens. Call it before the first
// database use, as a service's main does. A change resets the open pools
// (`PgReset`), which waits for their borrowed connections.
func SetPgBouncerPath(path PgBouncerPath) {
	if PgBouncerPath(pgBouncerPath.Swap(int32(path))) != path {
		PgReset()
	}
}

// The path of the pools this process opens.
func currentPgBouncerPath() PgBouncerPath {
	return PgBouncerPath(pgBouncerPath.Load())
}

// Where the instances behind the single port listen, and nginx's rule for
// taking one out, mirrored from xops (see the file header).
type PgBouncerDirectSettings struct {
	// The port nginx listens on. Only a pool whose authority has this port
	// goes direct.
	SinglePort int
	// Instance i, from 1, listens on SinglePort+i of the authority's host.
	// Non-positive keeps every pool on its authority.
	InstanceCount int
	// nginx's max_fails: the failed dials that take an instance out.
	// Non-positive never takes one out.
	MaxFails int
	// nginx's fail_timeout: an instance that is out stays out until this long
	// after its last failed dial. A connection to an instance selected later
	// than this after its last failure clears its count; one selected sooner
	// leaves it, so failures closer together than this add up across
	// connections.
	FailTimeout time.Duration
}

// nginx's and playbook-dbs.yml's values, in a new value on every call.
func DefaultPgBouncerDirectSettings() *PgBouncerDirectSettings {
	return &PgBouncerDirectSettings{
		// nginx.conf.j2 listen
		SinglePort: 6432,
		// playbook-dbs.yml pgbouncer_count, on ports 6433-6464
		InstanceCount: 32,
		// nginx.conf.j2 upstream pgbouncer: max_fails=3 fail_timeout=30s
		MaxFails:    3,
		FailTimeout: 30 * time.Second,
	}
}

// Gives config the path. The direct path wraps the pool's dialer, after the
// local test guard has wrapped it, so the guard still checks every instance
// address. The single port, or an authority on another port, leaves config as
// it was. Returns the direct dialer, or nil.
func configurePgBouncerPath(config *pgxpool.Config, path PgBouncerPath, settings *PgBouncerDirectSettings) *pgBouncerDirectDialer {
	if path != PgBouncerPathDirect {
		return nil
	}
	if settings.InstanceCount <= 0 || int(config.ConnConfig.Port) != settings.SinglePort {
		glog.Infof(
			"[db]pgbouncer path = direct, but the authority port %d is not the pgbouncer single port %d: dialing the authority\n",
			config.ConnConfig.Port,
			settings.SinglePort,
		)
		return nil
	}
	dialer := newPgBouncerDirectDialer(
		settings,
		config.ConnConfig.DialFunc,
		time.Now,
		// pools start at different instances, so the instances get even shares
		// of the processes' first connections
		mathrand.Intn(settings.InstanceCount),
	)
	config.ConnConfig.DialFunc = dialer.dialContext
	glog.Infof(
		"[db]pgbouncer path = direct: %d instances on ports %d-%d\n",
		settings.InstanceCount,
		settings.SinglePort+1,
		settings.SinglePort+settings.InstanceCount,
	)
	return dialer
}

// One instance's state in a dialer: nginx's conns, fails, accessed and
// checked for one peer.
type pgBouncerInstance struct {
	port int
	// the dialer's connections to the instance, counted from the instance's
	// selection until the dial fails or the connection closes
	connectionCount int
	// failed dials that no connection after a quiet window has cleared
	failCount int
	// the last failed dial
	failTime time.Time
	// the last failed dial, or a later selection that came more than
	// FailTimeout after the previous check
	checkTime time.Time
}

// Spreads one pool's new connections over the PgBouncer instances of its
// authority's host (see the file header). Safe for concurrent use: the
// instance state is under stateLock, and no dial holds it.
type pgBouncerDirectDialer struct {
	settings *PgBouncerDirectSettings
	// the pool's own dialer, for one address
	dial func(ctx context.Context, network string, address string) (net.Conn, error)
	now  func() time.Time

	stateLock sync.Mutex
	instances []pgBouncerInstance
	// where the search for the next of equally loaded instances starts
	nextIndex int
}

// firstIndex is the instance the first of equals is searched from.
func newPgBouncerDirectDialer(
	settings *PgBouncerDirectSettings,
	dial func(ctx context.Context, network string, address string) (net.Conn, error),
	now func() time.Time,
	firstIndex int,
) *pgBouncerDirectDialer {
	instances := make([]pgBouncerInstance, settings.InstanceCount)
	for i := range instances {
		instances[i] = pgBouncerInstance{port: settings.SinglePort + 1 + i}
	}
	return &pgBouncerDirectDialer{
		settings:  settings,
		dial:      dial,
		now:       now,
		instances: instances,
		nextIndex: firstIndex % len(instances),
	}
}

// A pgconn DialFunc. An address on the single port is dialed at its host's
// instances, one at a time, until one connects, every instance has been tried
// once, or ctx ends. A dial that ctx canceled says nothing about its instance
// and ends the connect; one that failed otherwise counts against its instance,
// and its deadline, the connect's, leaves no time for another. Any other
// address is dialed as is, such as a cancel request's, which goes to the
// instance its connection is on.
func (self *pgBouncerDirectDialer) dialContext(ctx context.Context, network string, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != strconv.Itoa(self.settings.SinglePort) {
		return self.dial(ctx, network, address)
	}
	triedIndexes := make([]bool, len(self.instances))
	triedCount := 0
	var lastErr error
	for {
		index, instancePort, ok := self.selectInstance(triedIndexes)
		if !ok {
			return nil, fmt.Errorf("each of %d pgbouncer instances failed, the last: %w", triedCount, lastErr)
		}
		triedIndexes[index] = true
		triedCount += 1
		conn, err := self.dial(ctx, network, net.JoinHostPort(host, strconv.Itoa(instancePort)))
		if err == nil {
			self.connectInstance(index)
			return &pgBouncerDirectConn{
				Conn:   conn,
				dialer: self,
				index:  index,
			}, nil
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			self.releaseInstance(index)
			return nil, err
		}
		self.failInstance(index)
		lastErr = err
		if ctx.Err() != nil {
			return nil, err
		}
	}
}

// Selects the next instance to dial among those triedIndexes leaves, and
// counts a connection for it (nginx's get_least_conn_peer). Instances that are
// in come first, the fewest connections first, equals from nextIndex on. Out
// instances follow in the same order, where nginx would refuse.
func (self *pgBouncerDirectDialer) selectInstance(triedIndexes []bool) (index int, port int, ok bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	now := self.now()
	count := len(self.instances)
	selectedIndex := -1
	selectedOut := false
	for offset := range count {
		i := (self.nextIndex + offset) % count
		if triedIndexes[i] {
			continue
		}
		out := self.outWithLock(&self.instances[i], now)
		if selectedIndex < 0 ||
			(selectedOut && !out) ||
			(selectedOut == out && self.instances[i].connectionCount < self.instances[selectedIndex].connectionCount) {
			selectedIndex = i
			selectedOut = out
		}
	}
	if selectedIndex < 0 {
		return -1, 0, false
	}
	instance := &self.instances[selectedIndex]
	if self.settings.FailTimeout < now.Sub(instance.checkTime) {
		instance.checkTime = now
	}
	instance.connectionCount += 1
	self.nextIndex = (selectedIndex + 1) % count
	return selectedIndex, instance.port, true
}

// nginx skips a peer whose fails reached max_fails within fail_timeout of its
// last check.
func (self *pgBouncerDirectDialer) outWithLock(instance *pgBouncerInstance, now time.Time) bool {
	return 0 < self.settings.MaxFails &&
		self.settings.MaxFails <= instance.failCount &&
		now.Sub(instance.checkTime) <= self.settings.FailTimeout
}

// A connection to an instance selected after a quiet window clears its count
// (nginx's notify on connect).
func (self *pgBouncerDirectDialer) connectInstance(index int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	instance := &self.instances[index]
	if instance.failTime.Before(instance.checkTime) {
		instance.failCount = 0
	}
}

// Counts a failed dial against an instance (nginx's free with
// NGX_PEER_FAILED).
func (self *pgBouncerDirectDialer) failInstance(index int) {
	var port int
	var failCount int
	out := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()

		now := self.now()
		instance := &self.instances[index]
		instance.connectionCount -= 1
		instance.failCount += 1
		instance.failTime = now
		instance.checkTime = now
		port = instance.port
		failCount = instance.failCount
		return failCount == self.settings.MaxFails
	}()
	if out {
		// nginx's "upstream server temporarily disabled"
		glog.Warningf(
			"[db]pgbouncer instance on port %d out for %s after %d failed dials\n",
			port,
			self.settings.FailTimeout,
			failCount,
		)
	}
}

// Ends a selection that made no connection, or a connection that closed.
func (self *pgBouncerDirectDialer) releaseInstance(index int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	self.instances[index].connectionCount -= 1
}

// A connection to one instance. Its first Close gives its count back.
type pgBouncerDirectConn struct {
	net.Conn
	dialer *pgBouncerDirectDialer
	index  int
	closed atomic.Bool
}

func (self *pgBouncerDirectConn) Close() error {
	if self.closed.CompareAndSwap(false, true) {
		self.dialer.releaseInstance(self.index)
	}
	return self.Conn.Close()
}
