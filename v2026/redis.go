package server

import (
	"context"
	"maps"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	// "net/netip"
	// "fmt"
	"fmt"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"
)

// type aliases to simplify user code
type RedisClient = redis.UniversalClient
type RedisPubSub = *redis.PubSub
type RedisMessage = *redis.Message
type RedisSubscription = *redis.Subscription

const RedisNil = redis.Nil
const NoTtl = time.Duration(0)

type safeRedisClient struct {
	mutex                 sync.Mutex
	client                redis.UniversalClient
	disableCommandRetry   bool
	contextTimeoutEnabled bool
	authentication        bool
	// reads a reply that comes after its caller's deadline rather than closing
	// its connection (socketDeadlineFromContext)
	keepLateReplies bool
}

type redisDialFunc func(ctx context.Context, network string, addr string) (net.Conn, error)

// redisClusterDialer dials every node address the cluster hands the client
// (CLUSTER SLOTS, MOVED and ASK) at the address this host reaches it by. The
// cluster spans more than one host, so the entry host alone no longer reaches
// every node: nodes announce their own lan ips and each node port is unique
// across the cluster.
func redisClusterDialer(authority string, nodeRoutes map[string]string, dial redisDialFunc) redisDialFunc {
	authorityHost, authorityPort, _ := net.SplitHostPort(authority)
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		dialAddr := redisNodeDialAddr(authorityHost, nodeRoutes, addr)
		conn, err := dial(ctx, network, dialAddr)
		if err != nil {
			route := redisDialFailureRoute(authorityHost, authorityPort, addr, dialAddr)
			redisDialFailures.WithLabelValues(route, redisDialFailureReason(err)).Inc()
		}
		return conn, err
	}
}

// The dial path of every Redis pool. A test process pins each node dial to its
// launcher attestation (test_env_dial_guard.go). Every other process has a nil
// guard, which hands dial back unchanged, so this is redisClusterDialer over
// dial itself.
func redisPoolDialer(guard *testEnvDialGuard, authority string, nodeRoutes map[string]string, dial redisDialFunc) redisDialFunc {
	return redisClusterDialer(authority, nodeRoutes, guard.wrapDial("redis", dial))
}

// redisNodeDialAddr maps an announced node address to the address to dial:
//   - a loopback authority is an ssh tunnel (xops/bin/by-patch-main-data)
//     that forwards every node port to loopback, so every node is dialed there
//   - a node that announces no host (`:port`, or `?` for an unknown endpoint)
//     is dialed on the authority host, as before the cluster spanned hosts
//   - a node whose announced ip is a settings lan_hosts address is dialed at
//     this host's route to that lan host. Lan hosts route to the lan ip
//     itself; hosts outside the lan (sille) route over the management vpn.
//   - any other address is dialed as announced
func redisNodeDialAddr(authorityHost string, nodeRoutes map[string]string, addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if redisLoopbackHost(authorityHost) {
		return net.JoinHostPort(authorityHost, port)
	}
	if host == "" || host == "?" {
		return net.JoinHostPort(authorityHost, port)
	}
	if route, ok := nodeRoutes[host]; ok {
		return net.JoinHostPort(route, port)
	}
	return addr
}

// redisNodeRoutes maps each lan host's lan ip (settings lan_hosts) to this
// host's route to it (settings routes). Lan hosts without a route are dialed
// at their announced ip.
func redisNodeRoutes(lanHosts map[string]string, routes map[string]string) map[string]string {
	nodeRoutes := map[string]string{}
	for host, lanIp := range lanHosts {
		if route, ok := routes[host]; ok && route != "" {
			nodeRoutes[lanIp] = route
		}
	}
	return nodeRoutes
}

func redisLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Whether go-redis takes a command's socket deadlines from its ctx, as the
// deadline and authentication pools do. go-redis closes a connection whose
// command failed with any timeout, so there a reply or handshake that comes
// after its caller's deadline costs the connection, and the next caller dials
// and handshakes again on the node that was late. On 2026-10-26 the proxies'
// reservation calls opened up to 25k connections per second to single slow
// nodes that way: connection handling held each node's main thread at 100%
// and halved its command throughput. A pool that keeps late replies leaves the
// deadline off its sockets. go-redis still bounds the wait for a pool turn and
// a dial with the caller's ctx, and a command that has a connection ends within
// the pool's own one-second socket timeouts.
func (self *safeRedisClient) socketDeadlineFromContext() bool {
	return self.contextTimeoutEnabled && !self.keepLateReplies
}

func (self *safeRedisClient) open() redis.UniversalClient {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client == nil {
		redisKeys := Vault.RequireSimpleResource("redis.yml")
		redisConfigKeys := Config.RequireSimpleResource("redis.yml")

		minConnections := redisConfigKeys.RequireInt("min_connections")
		maxConnections := redisConfigKeys.RequireInt("max_connections")
		maxRetries := 4
		connectionMaxLifetime := "8h"
		connectionMaxIdleTime := "60m"
		if allMaxRetries := redisConfigKeys.Int("max_retries"); 0 < len(allMaxRetries) {
			maxRetries = allMaxRetries[0]
		}
		if connectionMaxLifetimes := redisConfigKeys.String("conn_max_lifetime"); 0 < len(connectionMaxLifetimes) {
			connectionMaxLifetime = connectionMaxLifetimes[0]
		}
		if connectionMaxIdleTimes := redisConfigKeys.String("conn_max_idle_time"); 0 < len(connectionMaxIdleTimes) {
			connectionMaxIdleTime = connectionMaxIdleTimes[0]
		}
		if service, err := Service(); err == nil && service != "" {
			if serviceMinConnections := redisConfigKeys.Int(service, "min_connections"); 0 < len(serviceMinConnections) {
				minConnections = serviceMinConnections[0]
			}
			if serviceMaxConnections := redisConfigKeys.Int(service, "max_connections"); 0 < len(serviceMaxConnections) {
				maxConnections = serviceMaxConnections[0]
			}
			if allMaxRetries := redisConfigKeys.Int(service, "max_retries"); 0 < len(allMaxRetries) {
				maxRetries = allMaxRetries[0]
			}
			if connectionMaxLifetimes := redisConfigKeys.String(service, "conn_max_lifetime"); 0 < len(connectionMaxLifetimes) {
				connectionMaxLifetime = connectionMaxLifetimes[0]
			}
			if connectionMaxIdleTimes := redisConfigKeys.String(service, "conn_max_idle_time"); 0 < len(connectionMaxIdleTimes) {
				connectionMaxIdleTime = connectionMaxIdleTimes[0]
			}
		}
		maxRedirects := 8
		if self.disableCommandRetry {
			// go-redis retries a failed pipeline as a whole. If the server
			// applied a non-idempotent command before the response was lost,
			// retransmission applies it twice. -1 normalizes to zero retries
			// for both standalone clients and cluster redirects, leaving the
			// caller's source-of-truth reconciliation to repair an ambiguous
			// result instead of guessing whether to replay it.
			maxRetries = -1
			maxRedirects = -1
			// This secondary pool is opened only by non-idempotent write paths;
			// do not double every service's configured idle connection floor.
			minConnections = 0
		}

		connectionMaxLifetimeDuration, err := time.ParseDuration(connectionMaxLifetime)
		if err != nil {
			panic(err)
		}

		connectionMaxIdleTimeDuration, err := time.ParseDuration(connectionMaxIdleTime)
		if err != nil {
			panic(err)
		}

		// fail fast: during the 2026-07-15 single-node wedges, the previous
		// patient values (dial 30s, pool 5min, read 30s) queued goroutines for
		// minutes per operation while they held pg transactions and connection
		// handshakes — one sick node became a fleet-wide pileup. Bounded
		// waits turn a wedge into quick, retryable errors instead.
		readTimeout := 15 * time.Second
		writeTimeout := 15 * time.Second
		poolTimeout := 15 * time.Second
		dialTimeout := 5 * time.Second
		dialRetries := 4

		if self.contextTimeoutEnabled {
			// This pool is only for optional, one-second cleanup. Native caps
			// also bound driver maintenance that does not carry a caller ctx,
			// and in a pool that keeps late replies each command that has a
			// connection.
			readTimeout = time.Second
			writeTimeout = time.Second
			poolTimeout = time.Second
			dialTimeout = time.Second
			dialRetries = 1                                 // go-redis counts dial attempts, not extra retries
			maxConnections = max(1, min(maxConnections, 8)) // per node in cluster mode
		}
		if self.authentication {
			readTimeout, writeTimeout, poolTimeout, dialTimeout = 2*time.Second, 2*time.Second, 2*time.Second, 2*time.Second
			dialRetries, maxRetries, maxRedirects = 1, -1, 2
			maxConnections = max(16, redisConfigKeys.RequireInt("max_connections"))
		}
		// go-redis refuses a dial with ErrPoolExhausted, without waiting for
		// PoolTimeout, once its counted connections reach MaxActiveConns. Its
		// refill of idle connections counts a slot before dialing it, so with
		// the cap at the pool size a refill can take the last slot between a
		// turn holder's miss and its dial. Pool turns already bound the pool,
		// so a pool that keeps idle connections leaves the cap off; a raced
		// dial is then used once and closed.
		maxActiveConnections := maxConnections
		if 0 < minConnections {
			maxActiveConnections = 0
		}

		skipConnCheckWithin := redisSkipConnCheckWithin(maxRetries)

		dialer := NewDialer(dialTimeout)
		authority := redisKeys.RequireString("authority")
		password := localEvaluationCredential("EVALUATION_REDIS_PASSWORD", redisKeys.RequireString("password"))
		// every pooled socket counts its deadlines in the timer census
		// (redis_timer_census.go)
		dialContext := redisTimerCensusDialer(redisPoolDialer(
			currentTestEnvDialGuard(),
			authority,
			redisNodeRoutes(LanHosts(), Routes()),
			dialer.DialContext,
		))

		cluster := redisKeys.RequireBool("cluster")

		if cluster {
			options := &redis.ClusterOptions{
				Addrs:               []string{authority},
				Password:            password,
				MaxRetries:          maxRetries,
				MinIdleConns:        minConnections,
				MaxIdleConns:        maxConnections,
				PoolSize:            maxConnections,
				MaxActiveConns:      maxActiveConnections,
				ConnMaxLifetime:     connectionMaxLifetimeDuration,
				ConnMaxIdleTime:     connectionMaxIdleTimeDuration,
				SkipConnCheckWithin: skipConnCheckWithin,
				// see https://redis.uptrace.dev/guide/go-redis-debugging.html#timeouts
				// see https://uptrace.dev/blog/golang-context-timeout.html
				ContextTimeoutEnabled: self.socketDeadlineFromContext(),
				// PING, GET and same-slot EVAL use direct slot routing. Policy
				// discovery otherwise detaches COMMAND from the caller deadline.
				DisableRoutingPolicies: self.contextTimeoutEnabled,
				ReadTimeout:            readTimeout,
				WriteTimeout:           writeTimeout,
				PoolTimeout:            poolTimeout,
				Dialer:                 dialContext,
				// DialerRetries: maxRetries,
				DialTimeout:   dialTimeout,
				DialerRetries: dialRetries,
				// FailingTimeoutSeconds: 0,
				MaxRedirects: maxRedirects,
			}
			self.client = redis.NewClusterClient(options)
			self.client.AddHook(redisTtlWarnHook{})
		} else {
			// see https://github.com/redis/go-redis/blob/master/options.go#L31
			options := &redis.Options{
				Addr:     redisKeys.RequireString("authority"),
				Password: password,
				DB:       redisKeys.RequireInt("db"),
				// Addr: "192.168.208.135:6379",
				// Password: "",
				// DB: 0,
				MaxRetries:          maxRetries,
				MinIdleConns:        minConnections,
				MaxIdleConns:        maxConnections,
				PoolSize:            maxConnections,
				MaxActiveConns:      maxActiveConnections,
				ConnMaxLifetime:     connectionMaxLifetimeDuration,
				ConnMaxIdleTime:     connectionMaxIdleTimeDuration,
				SkipConnCheckWithin: skipConnCheckWithin,
				// see https://redis.uptrace.dev/guide/go-redis-debugging.html#timeouts
				// see https://uptrace.dev/blog/golang-context-timeout.html
				ContextTimeoutEnabled: self.socketDeadlineFromContext(),
				ReadTimeout:           readTimeout,
				WriteTimeout:          writeTimeout,
				PoolTimeout:           poolTimeout,
				Dialer:                dialContext,
				// DialerRetries: maxRetries,
				DialTimeout:   dialTimeout,
				DialerRetries: dialRetries,
				// FailingTimeoutSeconds: 0,
			}
			self.client = redis.NewClient(options)
			self.client.AddHook(redisTtlWarnHook{})
		}
	}
	return self.client
}

// How long after a pooled connection was put back its next checkout skips
// go-redis's socket check (SkipConnCheckWithin; third_party/go-redis
// URNETWORK.md; LBALTOPT2 R19), where a pool may skip it.
const redisSkipConnCheckWindow = time.Second

// The SkipConnCheckWithin of a pool whose commands go-redis retries
// maxRetries times. go-redis checks a pooled connection's socket at every
// checkout, a non-blocking peek, about 0.8% of connect's CPU on 2026-11-20
// (LBALTOPT2 §3.3). A pool that retries its commands skips the check for a
// connection put back within redisSkipConnCheckWindow: one the server closed
// within the window fails the command it serves with EOF, which go-redis
// retries on another connection, and that removal has every checkout check
// again for the window, so one command per pool, not each, meets a closed
// connection. A pool without command retries, maxRetries -1 or 0, checks
// every checkout: there the closed connection's failure would be its
// caller's, and its callers count on the check (redisWithClient).
func redisSkipConnCheckWithin(maxRetries int) time.Duration {
	if maxRetries <= 0 {
		return 0
	}
	return redisSkipConnCheckWindow
}

func (self *safeRedisClient) current() redis.UniversalClient {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	return self.client
}

func (self *safeRedisClient) close() {
	self.reset()
}
func (self *safeRedisClient) reset() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client != nil {
		if v, ok := self.client.(interface{ Close() error }); ok {
			v.Close()
		}
		self.client = nil
	}
}

var safeClient = &safeRedisClient{}
var safeNoCommandRetryClient = &safeRedisClient{disableCommandRetry: true}

// Optional deadline-bound work has a separate, lazily opened pool. It keeps a
// zero idle connection floor and disables command/redirect retries. Existing
// credential and accounting pools retain their original timeout policies.
var safeDeadlineClient = &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true}

// Deadline-bound calls that wait out a late reply rather than close its
// connection (OptRedisKeepLateReplies) have a pool of their own with the same
// policy otherwise, so that a storm of them never takes the turns of callers
// whose deadline must bound their socket.
var safeLateReplyClient = &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, keepLateReplies: true}

// Authentication owns its pool and total budget; no preflight command borrows
// authority or consumes a second round trip. Routing redirects share the context.
var safeAuthClient = &safeRedisClient{disableCommandRetry: true, contextTimeoutEnabled: true, authentication: true}

func RedisAuth(ctx context.Context, callback func(context.Context, RedisClient) error) (returnErr error) {
	observeTxIo(TxIoRedis)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("authentication Redis unavailable")
			}
		}
		if ctx.Err() != nil {
			returnErr = ctx.Err()
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return callback(ctx, safeAuthClient.open())
}

var redisKeyEventMergeDrops = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_redis_key_event_merge_drops_total",
	Help: "Keyspace notifications dropped at the process-wide merge after the subscription was handed to its caller; each drop terminates the subscription epoch and forces a full resync",
})

var redisKeyEventStartupDrops = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_redis_key_event_startup_drops_total",
	Help: "Keyspace notifications dropped at a full merge before the subscription was handed to its caller; the caller's recovery read covers every event before the handoff, so these end no epoch",
})

var redisKeyEventIgnored = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_redis_key_event_ignored_total",
	Help: "Keyspace notifications a node drain discarded before the merge as an ignored event, by default expire (a ttl refresh)",
})

// pool stats flow to grafana via the default registry (see StartStatsPusher).
// Timeouts is the earliest caller-side signal of a wedged node or exhausted
// pool — it fires before user-visible failures.
func init() {
	poolStat := func(f func(*redis.PoolStats) float64) func() float64 {
		return func() float64 {
			return redisPoolStat(f)
		}
	}
	prometheus.MustRegister(
		redisDialFailures,
		redisKeyEventMergeDrops,
		redisKeyEventStartupDrops,
		redisKeyEventIgnored,
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{Name: "urnetwork_redis_pool_hits_total", Help: "redis pool free-connection hits"},
			poolStat(func(s *redis.PoolStats) float64 { return float64(s.Hits) }),
		),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{Name: "urnetwork_redis_pool_misses_total", Help: "redis pool misses (new dial needed)"},
			poolStat(func(s *redis.PoolStats) float64 { return float64(s.Misses) }),
		),
		prometheus.NewCounterFunc(
			prometheus.CounterOpts{Name: "urnetwork_redis_pool_timeouts_total", Help: "redis pool timeouts (waited PoolTimeout without a free connection)"},
			poolStat(func(s *redis.PoolStats) float64 { return float64(s.Timeouts) }),
		),
		prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: "urnetwork_redis_pool_total_conns", Help: "redis pool total connections (all nodes)"},
			poolStat(func(s *redis.PoolStats) float64 { return float64(s.TotalConns) }),
		),
		prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{Name: "urnetwork_redis_pool_idle_conns", Help: "redis pool idle connections (all nodes)"},
			poolStat(func(s *redis.PoolStats) float64 { return float64(s.IdleConns) }),
		),
	)
}

// Every pool the process opens, for reset and pool telemetry.
func redisPools() []*safeRedisClient {
	return []*safeRedisClient{safeClient, safeNoCommandRetryClient, safeDeadlineClient, safeLateReplyClient, safeAuthClient}
}

// One pool statistic summed over the open pools.
func redisPoolStat(stat func(*redis.PoolStats) float64) float64 {
	value := float64(0)
	for _, pool := range redisPools() {
		if client := pool.current(); client != nil {
			value += stat(client.PoolStats())
		}
	}
	return value
}

// resets the connection pool
// call this after changes to the env
func RedisReset() {
	for _, pool := range redisPools() {
		pool.reset()
	}
}

// func client() redis.UniversalClient {
// 	return safeClient.open()
// }

func isRedisConnectionError(err error) bool {
	// m := strings.ToLower(err.Error())
	m := err.Error()
	if strings.Contains(m, "redis: client is closed") {
		// the callback waited too long to first command, try again
		return true
		//  i/o timeout
	} else if strings.Contains(m, "dial tcp") {
		// tcp dial error, continue
		return true
	} else if strings.Contains(m, "i/o timeout") {
		return true
	} else if strings.Contains(m, "connection reset by peer") {
		return true
	} else if strings.Contains(m, "cannot assign requested address") {
		// client ephemeral ports exhausted by a redial storm; drains in ~60s
		return true
	} else if strings.Contains(m, "CLUSTERDOWN") {
		// transient during failover elections (~node-timeout); NOT an OOM or
		// slot-moved condition, those re-panic immediately
		return true
	} else if strings.Contains(m, "LOADING") {
		// node restarting and loading its rdb
		return true
	} else if strings.Contains(m, "READONLY") {
		// command routed to a replica mid-failover; retry after topology settles
		return true
	} else {
		// NOT retryable, deliberately: "connection pool timeout" — pool
		// exhaustion is local backpressure; a callback rerun re-enters the
		// pool queue and re-issues all its commands, amplifying demand into
		// livelock (observed: TestStream under a 16-conn pool), and reruns
		// double-apply non-idempotent commands. Fail fast; outer layers
		// (task backoff, request retry) shed the load instead.
		return false
	}
}

func Redis(ctx context.Context, callback func(RedisClient), options ...any) {
	// From the go-redis code:
	// >> Client is a Redis client representing a pool of zero or more underlying connections.
	// >> It's safe for concurrent use by multiple goroutines.
	// context := context.Background()
	c := func() {
		redisWithClient(ctx, safeClient, callback, options...)
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[redis] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

// RedisDoOnce runs a callback through a client whose command-level retries and
// the server wrapper's callback retries are both disabled. Use it for
// non-idempotent writes whose response can be lost after Redis applied the
// mutation. The caller must surface the error and use an authoritative repair
// path; blindly replaying the write can apply it twice.
func RedisDoOnce(ctx context.Context, callback func(RedisClient)) {
	c := func() {
		redisWithClient(ctx, safeNoCommandRetryClient, callback, OptNoRetry())
	}
	if glog.V(2) {
		pc, filename, line, _ := runtime.Caller(1)
		pcName := runtime.FuncForPC(pc).Name()
		parts := strings.Split(filename, "/")
		Trace(
			fmt.Sprintf("[redis-once] %s %s:%d\n", pcName, parts[len(parts)-1], line),
			c,
		)
	} else {
		c()
	}
}

// RedisWithDeadline is the non-retrying boundary for work with an explicit
// caller deadline: optional cleanup, admission, and the provider liveness
// lease. Its dedicated pool applies that deadline to socket reads and writes for
// both standalone and cluster clients, unless the caller passes
// OptRedisKeepLateReplies. The callback must pass the same ctx to every
// command. RedisReset and pool telemetry own this pool just like the
// ordinary pools; no per-operation client is leaked.
// The pool bypasses cluster command-policy discovery, so each command routes by
// its own first key: PING, GET, same-slot EVAL and other single-key commands,
// alone or pipelined across slots. It must not route a command whose keys span
// slots. EVAL callers must keep every declared key in one explicit Redis hash
// tag. No call pings before its callback (redisWithClient): a connection that
// cannot serve fails the callback's first command, within that command's
// deadline.
func RedisWithDeadline(ctx context.Context, callback func(RedisClient) error, options ...any) (returnErr error) {
	if _, bounded := ctx.Deadline(); !bounded {
		return fmt.Errorf("RedisWithDeadline requires an explicit deadline")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The ordinary wrapper raises connection failures. Optional cleanup must
	// return them without treating an expected deadline as an unexpected panic.
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("deadline-bound Redis operation failed")
			}
		}
		if ctx.Err() != nil {
			returnErr = ctx.Err()
		} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			// A socket deadline can fire before the context timer is scheduled.
			returnErr = context.DeadlineExceeded
		}
	}()
	pool := safeDeadlineClient
	for _, option := range options {
		if v, ok := option.(RedisLateReplyOptions); ok && v.keep {
			pool = safeLateReplyClient
		}
	}
	redisWithClient(ctx, pool, func(client RedisClient) {
		returnErr = callback(client)
	}, append([]any{OptNoRetry()}, options...)...)
	return
}

// Whether a deadline-bound call waits out a late reply on its connection.
type RedisLateReplyOptions struct {
	keep bool
}

// Runs a RedisWithDeadline call on the deadline pool that keeps a connection
// whose reply or handshake comes after its caller's deadline: the deadline
// bounds the wait for a pool turn and a dial, and a command that has a
// connection ends within the pool's one-second socket timeouts. The call can
// then end up to that long after its deadline, and still returns the
// deadline's error. A slow node then costs its callers their deadlines, not
// a new connection and handshake per call. A call inside a PostgreSQL
// transaction keeps the default pool, so that a slow node never holds the
// transaction past its caller's deadline.
func OptRedisKeepLateReplies() RedisLateReplyOptions {
	return RedisLateReplyOptions{keep: true}
}

// The option OptRedisNoPreflight returns.
type RedisPreflightOptions struct {
	skip bool
}

// Accepted and ignored: no call pings before its callback since 2026-11-15
// (redisWithClient), so there is no preflight to skip. It remains so that the
// callers that pass it, and branches written before the change, still build.
func OptRedisNoPreflight() RedisPreflightOptions {
	return RedisPreflightOptions{skip: true}
}

// Runs callback with a client of pool. No pool pings before the callback:
// until 2026-11-15 the non-retrying and deadline-bound pools sent a PING first
// unless the caller passed OptRedisNoPreflight, about 77k a second across the
// fleet (CPUOPT5 E 5.1, CPUOPT6 E #2). Those pools replay no command, so the
// PING guarded no command's outcome: it reached an arbitrary node, not
// necessarily the one a command routes to, could not promise that the next
// command succeeds, and cost a round trip and a pool turn in the command's own
// budget. A connection that cannot serve now fails the callback's first
// command, under that command's deadline (the deadline pools put the caller's
// context deadline on their sockets) or its pool's socket timeouts, through the
// same error path. go-redis's own pool checks keep the connections healthy.
func redisWithClient(ctx context.Context, pool *safeRedisClient, callback func(RedisClient), options ...any) {
	observeTxIo(TxIoRedis)
	retryOptions := OptRetryDefault()
	// debugOptions := OptNoDebug()
	for _, option := range options {
		if v, ok := option.(DbRetryOptions); ok {
			retryOptions = v
		}
	}

	retryEndTime := NowUtc().Add(retryOptions.endRetryTimeout)
	// retryDebugTime := NowUtc().Add(retryOptions.debugRetryTimeout)
	backoff := retryOptions.Backoff()
	for {
		if err := ctx.Err(); err != nil {
			if retryOptions.rerunOnConnectionError {
				panic(dbContextDoneCause(ctx, err))
			}
			panic(err)
		}
		client := pool.open()

		// every callback reports its command errors through the retry boundary
		// below, with no preflight command
		var connErr error
		func() {
			defer func() {
				if err := recover(); err != nil {
					switch v := err.(type) {
					case error:
						if isRedisConnectionError(v) && retryOptions.rerunOnConnectionError {
							connErr = v
						} else {
							panic(v)
						}
					default:
						panic(v)
					}
				}
			}()
			callback(client)
		}()

		if connErr != nil {
			if retryOptions.rerunOnConnectionError {
				select {
				case <-ctx.Done():
					panic(dbContextDoneCause(ctx, connErr))
				case <-time.After(backoff.NextRetryTimeout()):
					if retryEndTime.Before(NowUtc()) {
						panic(connErr)
					}
					continue
				}
			}
			panic(connErr)
		}

		return
	}
}

// RedisDb returns the logical db index of the configured redis, used to
// build keyspace-notification channel names (cluster mode is always db 0).
func RedisDb() int {
	redisKeys := Vault.RequireSimpleResource("redis.yml")
	if redisKeys.RequireBool("cluster") {
		return 0
	}
	return redisKeys.RequireInt("db")
}

// Bounds one keyspace subscription's startup and buffered delivery.
type RedisKeyEventSubscriptionSettings struct {
	// Time allowed for subscription writes and every master's acknowledgements.
	// Native Redis connection setup retains its socket timeouts. Must be positive.
	AcknowledgementTimeout time.Duration
	// Interval between cluster master-set checks. Must be positive.
	TopologyRefreshInterval time.Duration
	// Messages buffered per node and in the shared merge. Must be positive.
	MessageBufferSize int
	// Keyspace events, the payload of a `__keyspace@` message, that each node
	// drain discards before the shared merge, so they take no merge slot and
	// cannot end an epoch. expire: a ttl set on a key that still exists, which
	// changes no value; no subscriber acts on it, and an expiry itself arrives
	// as expired. Session ttl refreshes made expire 87% of connect's stream,
	// and it overflowed the merge every few seconds (CPUOPT6, 2026-11-15).
	// Empty delivers every event.
	IgnoredKeyspaceEvents []string
}

// Allows establishment latency while bounding notification-free startup.
func DefaultRedisKeyEventSubscriptionSettings() *RedisKeyEventSubscriptionSettings {
	return &RedisKeyEventSubscriptionSettings{
		AcknowledgementTimeout:  5 * time.Second,
		TopologyRefreshInterval: 60 * time.Second,
		MessageBufferSize:       1024,
		IgnoredKeyspaceEvents:   []string{"expire"},
	}
}

// Subscribes on every master and returns only after all patterns are acknowledged,
// so the caller's recovery read covers the entire preceding gap. The done channel
// closes on a lost subscription, merge overflow or master-set change; the caller
// must unsubscribe, resubscribe and reconcile its state (PEERSSTREAMS2.md).
// Ignored keyspace events never reach the merge, and a merge that overflows
// before the subscription is returned drops the message without ending it.
func SubscribeKeyEvents(
	ctx context.Context,
	topologyRefresh time.Duration,
	patterns ...string,
) (messages <-chan RedisMessage, done <-chan struct{}, unsub func(), returnErr error) {
	settings := DefaultRedisKeyEventSubscriptionSettings()
	settings.TopologyRefreshInterval = topologyRefresh
	return SubscribeKeyEventsWithSettings(ctx, settings, patterns...)
}

// Applies an owner's startup and buffering limits to a keyspace subscription.
// Failure closes every opened socket and returns no subscription to the caller.
func SubscribeKeyEventsWithSettings(
	ctx context.Context,
	settings *RedisKeyEventSubscriptionSettings,
	patterns ...string,
) (messages <-chan RedisMessage, done <-chan struct{}, unsub func(), returnErr error) {
	if settings == nil || settings.AcknowledgementTimeout <= 0 || settings.TopologyRefreshInterval <= 0 || settings.MessageBufferSize <= 0 {
		return nil, nil, nil, fmt.Errorf("invalid redis key-event subscription settings")
	}
	// Duplicate patterns produce extra confirmations with an unchanged count;
	// subscribe once so they cannot look like a reconnect or prevent readiness.
	uniquePatterns := []string{}
	seenPatternKVs := map[string]bool{}
	for _, pattern := range patterns {
		if !seenPatternKVs[pattern] {
			seenPatternKVs[pattern] = true
			uniquePatterns = append(uniquePatterns, pattern)
		}
	}
	if len(uniquePatterns) == 0 {
		return nil, nil, nil, fmt.Errorf("no redis key-event patterns")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, context.Cause(ctx)
	}
	var client RedisClient
	Redis(ctx, func(c RedisClient) {
		client = c
	})

	cancelCtx, cancel := context.WithCancelCause(ctx)
	startupCtx, startupCancel := context.WithTimeout(cancelCtx, settings.AcknowledgementTimeout)
	defer startupCancel()
	stopStartupCancel := context.AfterFunc(startupCtx, func() { cancel(context.Cause(startupCtx)) })
	defer stopStartupCancel()
	out := make(chan RedisMessage, settings.MessageBufferSize)
	ignoredKeyspaceEvents := map[string]bool{}
	for _, event := range settings.IgnoredKeyspaceEvents {
		ignoredKeyspaceEvents[event] = true
	}
	// set just before the subscription is returned. The caller's recovery read
	// runs after that, so it covers whatever a drain drops before it.
	var handedOff atomic.Bool

	// ForEachMaster runs its callback CONCURRENTLY, one goroutine per master —
	// every write to state shared across callbacks must hold stateLock
	// (unsynchronized writes here crash-looped the connect fleet on 2026-07-18:
	// fatal concurrent map writes at startup with 32 masters)
	var stateLock sync.Mutex
	pubsubs := []*redis.PubSub{}
	readyChannels := []<-chan struct{}{}
	closeSubscriptions := func() {
		cancel(context.Canceled)
		for _, pubsub := range pubsubs {
			_ = pubsub.Close()
		}
	}
	started := false
	defer func() {
		if !started {
			closeSubscriptions()
		}
	}()
	masterAddrs := func(ctx context.Context) (map[string]bool, error) {
		addrs := map[string]bool{}
		clusterClient, ok := client.(*redis.ClusterClient)
		if !ok {
			return addrs, nil
		}
		err := clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
			stateLock.Lock()
			defer stateLock.Unlock()
			addrs[node.Options().Addr] = true
			return nil
		})
		return addrs, err
	}

	drain := func(pubsub *redis.PubSub, ready chan<- struct{}) {
		defer cancel(fmt.Errorf("redis key-event subscription ended"))
		c := pubsub.ChannelWithSubscriptions(
			redis.WithChannelSize(settings.MessageBufferSize),
		)
		initialSubscriptionReady := false
		for {
			select {
			case <-cancelCtx.Done():
				return
			case value, ok := <-c:
				if !ok {
					// the subscription died; the caller resubscribes + resyncs
					return
				}
				switch value := value.(type) {
				case *redis.Subscription:
					if value.Kind == "psubscribe" {
						if initialSubscriptionReady {
							// go-redis transparently reconnected. End this
							// generation so the caller performs the required
							// full resync for the unseen gap.
							return
						}
						if len(uniquePatterns) <= value.Count {
							initialSubscriptionReady = true
							close(ready)
						}
					}
					continue
				case *redis.Message:
					if ignoredKeyspaceEvents[value.Payload] && strings.HasPrefix(value.Channel, "__keyspace@") {
						redisKeyEventIgnored.Inc()
						continue
					}
					select {
					case <-cancelCtx.Done():
						return
					case out <- value:
					default:
						if !handedOff.Load() {
							// No one drains the merge until the subscription is
							// returned, and the caller's recovery read covers
							// this message, so the startup goes on without it.
							redisKeyEventStartupDrops.Inc()
							continue
						}
						// Never stop draining a node's PubSub socket behind a
						// congested shared merge. A drop invalidates this
						// epoch; reconnect + full resync restores state.
						redisKeyEventMergeDrops.Inc()
						return
					}
				}
			}
		}
	}

	subscribe := func(node RedisClient) error {
		pubsub := node.PSubscribe(cancelCtx)
		ready := make(chan struct{})
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			pubsubs = append(pubsubs, pubsub)
			readyChannels = append(readyChannels, ready)
		}()
		if err := pubsub.PSubscribe(startupCtx, uniquePatterns...); err != nil {
			cancel(err)
			return err
		}
		go HandleError(func() { drain(pubsub, ready) })
		return nil
	}

	var initialAddrs map[string]bool
	if clusterClient, ok := client.(*redis.ClusterClient); ok {
		var err error
		initialAddrs, err = masterAddrs(startupCtx)
		if err != nil {
			returnErr = err
			return
		}
		if len(initialAddrs) == 0 {
			returnErr = fmt.Errorf("no master nodes")
			return
		}
		err = clusterClient.ForEachMaster(startupCtx, func(ctx context.Context, node *redis.Client) error {
			return subscribe(node)
		})
		if err != nil {
			returnErr = err
			return
		}
	} else if err := subscribe(client); err != nil {
		returnErr = err
		return
	}

	// The node drains keep running while readiness waits, so early messages do
	// not block acknowledgements. Early messages wait in the merge for the
	// caller, and one that finds it full is dropped (redisKeyEventStartupDrops).
	for _, ready := range readyChannels {
		select {
		case <-cancelCtx.Done():
			returnErr = context.Cause(cancelCtx)
			return
		case <-ready:
		}
	}
	stopStartupCancel()
	if err := startupCtx.Err(); err != nil {
		returnErr = context.Cause(startupCtx)
		return
	}
	if clusterClient, ok := client.(*redis.ClusterClient); ok {
		// watch the master set: a moved slot emits events on the new owner,
		// which the current subscriptions do not cover
		go HandleError(func() {
			defer cancel(fmt.Errorf("redis key-event master set changed"))
			for {
				select {
				case <-cancelCtx.Done():
					return
				case <-time.After(settings.TopologyRefreshInterval):
				}
				clusterClient.ReloadState(cancelCtx)
				addrs, err := masterAddrs(cancelCtx)
				if err != nil || !maps.Equal(addrs, initialAddrs) {
					return
				}
			}
		})
	}

	handedOff.Store(true)
	started = true
	messages = out
	done = cancelCtx.Done()
	unsub = closeSubscriptions
	return
}

// Testing_EnableKeyspaceNotifications turns on the keyspace-notification
// classes the key-event transport needs (PEERSSTREAMS2.md), on every node.
// Tests only — production nodes get this from redis.conf.j2 (xops).
func Testing_EnableKeyspaceNotifications(ctx context.Context) {
	Testing_SetKeyspaceNotifications(ctx, "Kg$sx")
}

// Testing_SetKeyspaceNotifications sets the notification classes on every
// node ("" disables — used by the drop-repair test to silence events).
func Testing_SetKeyspaceNotifications(ctx context.Context, classes string) {
	Redis(ctx, func(client RedisClient) {
		set := func(c *redis.Client) error {
			return c.ConfigSet(ctx, "notify-keyspace-events", classes).Err()
		}
		if clusterClient, ok := client.(*redis.ClusterClient); ok {
			err := clusterClient.ForEachMaster(ctx, func(ctx context.Context, node *redis.Client) error {
				return set(node)
			})
			if err != nil {
				panic(err)
			}
		} else if nodeClient, ok := client.(*redis.Client); ok {
			if err := set(nodeClient); err != nil {
				panic(err)
			}
		}
	})
}

func RedisSetIfEqual(r RedisClient, ctx context.Context, key string, test []byte, value []byte, ttl time.Duration) *redis.Cmd {
	script := `local key = KEYS[1] local expected_value = ARGV[1] local new_value = ARGV[2] local ttl = ARGV[3] local current_value = redis.call('GET', key) if current_value == expected_value then redis.call('SET', key, new_value) redis.call('EXPIRE', key, ttl) return 1 else return 0 end`
	return r.Eval(ctx, script, []string{key}, test, value, redisExpirySeconds(ttl))
}

func redisExpirySeconds(ttl time.Duration) int64 {
	return int64((ttl + time.Second/2) / time.Second)
}

func RedisRemoveIfEqual(r redis.Scripter, ctx context.Context, key string, test []byte) *redis.Cmd {
	script := `local key = KEYS[1] local expected_value = ARGV[1] local current_value = redis.call('GET', key) if current_value == expected_value then redis.call('DEL', key) return 1 else return 0 end`
	return r.Eval(ctx, script, []string{key}, test)
}
