package server

import (
	"context"
	"maps"
	"net"
	"strings"
	"sync"
	"time"
	// "net/netip"
	// "fmt"
	"fmt"
	"runtime"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"
)

// type aliases to simplify user code
type RedisClient = redis.UniversalClient
type RedisPubSub = *redis.PubSub
type RedisMessage = *redis.Message
type RedisSubscription = *redis.Subscription

const RedisNil = redis.Nil
const NoTtl = time.Duration(0)

type safeRedisClient struct {
	mutex  sync.Mutex
	client redis.UniversalClient
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

		dialer := &net.Dialer{
			Timeout: dialTimeout,
			KeepAliveConfig: net.KeepAliveConfig{
				Enable: true,
			},
		}
		authority := redisKeys.RequireString("authority")
		host, _, _ := net.SplitHostPort(authority)
		dialContext := func(ctx context.Context, network string, addr string) (net.Conn, error) {
			// always connect to the original host
			// this is because the cluser is set up with ip endpoints, but the ip is not universally routable
			// note: this should be the default client behavior when `cluster-preferred-endpoint-type=unknown-endpoint`
			//       however, go-redis and most other redis clients don't seem to handle the `MOVE :port` commands
			//       correctly without a host
			_, portStr, _ := net.SplitHostPort(addr)
			return dialer.DialContext(ctx, network, net.JoinHostPort(host, portStr))
		}

		cluster := redisKeys.RequireBool("cluster")

		if cluster {
			options := &redis.ClusterOptions{
				Addrs:           []string{authority},
				Password:        redisKeys.RequireString("password"),
				MaxRetries:      maxRetries,
				MinIdleConns:    minConnections,
				MaxIdleConns:    maxConnections,
				PoolSize:        maxConnections,
				MaxActiveConns:  maxConnections,
				ConnMaxLifetime: connectionMaxLifetimeDuration,
				ConnMaxIdleTime: connectionMaxIdleTimeDuration,
				// see https://redis.uptrace.dev/guide/go-redis-debugging.html#timeouts
				// see https://uptrace.dev/blog/golang-context-timeout.html
				ContextTimeoutEnabled: false,
				ReadTimeout:           readTimeout,
				WriteTimeout:          writeTimeout,
				PoolTimeout:           poolTimeout,
				Dialer:                dialContext,
				// DialerRetries: maxRetries,
				DialTimeout:   dialTimeout,
				DialerRetries: dialRetries,
				// FailingTimeoutSeconds: 0,
				MaxRedirects: 8,
			}
			self.client = redis.NewClusterClient(options)
		} else {
			// see https://github.com/redis/go-redis/blob/master/options.go#L31
			options := &redis.Options{
				Addr:     redisKeys.RequireString("authority"),
				Password: redisKeys.RequireString("password"),
				DB:       redisKeys.RequireInt("db"),
				// Addr: "192.168.208.135:6379",
				// Password: "",
				// DB: 0,
				MaxRetries:      maxRetries,
				MinIdleConns:    minConnections,
				MaxIdleConns:    maxConnections,
				PoolSize:        maxConnections,
				MaxActiveConns:  maxConnections,
				ConnMaxLifetime: connectionMaxLifetimeDuration,
				ConnMaxIdleTime: connectionMaxIdleTimeDuration,
				// see https://redis.uptrace.dev/guide/go-redis-debugging.html#timeouts
				// see https://uptrace.dev/blog/golang-context-timeout.html
				ContextTimeoutEnabled: false,
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
		}
	}
	return self.client
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

// pool stats flow to grafana via the default registry (see StartStatsPusher).
// Timeouts is the earliest caller-side signal of a wedged node or exhausted
// pool — it fires before user-visible failures.
func init() {
	poolStat := func(f func(*redis.PoolStats) float64) func() float64 {
		return func() float64 {
			client := safeClient.current()
			if client == nil {
				return 0
			}
			return f(client.PoolStats())
		}
	}
	prometheus.MustRegister(
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

// resets the connection pool
// call this after changes to the env
func RedisReset() {
	safeClient.reset()
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

func redisWithClient(ctx context.Context, pool *safeRedisClient, callback func(RedisClient), options ...any) {
	retryOptions := OptRetryDefault()
	// debugOptions := OptNoDebug()
	for _, option := range options {
		switch v := option.(type) {
		case DbRetryOptions:
			retryOptions = v
		}
	}

	retryEndTime := NowUtc().Add(retryOptions.endRetryTimeout)
	// retryDebugTime := NowUtc().Add(retryOptions.debugRetryTimeout)
	backoff := retryOptions.Backoff()
	for {
		client := pool.open()

		connErr := client.Ping(ctx).Err()
		if connErr != nil {
			if retryOptions.rerunOnConnectionError {
				select {
				case <-ctx.Done():
					panic(DbContextDoneError)
				case <-time.After(backoff.NextRetryTimeout()):
					if retryEndTime.Before(NowUtc()) {
						panic(connErr)
					}
					continue
				}
			}
			panic(connErr)
		}

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
					panic(DbContextDoneError)
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

// SubscribeKeyEvents psubscribes the patterns on every master node — redis
// keyspace notifications are emitted only by the node holding the key's slot
// — and merges the messages into one channel (PEERSSTREAMS2.md). The done
// channel closes when any node subscription dies or the master set changes
// (checked every topologyRefresh); events during any gap are gone forever,
// so on done the caller must unsub, resubscribe, and RESYNC its state. On a
// non-cluster client this is a single psubscribe.
func SubscribeKeyEvents(
	ctx context.Context,
	topologyRefresh time.Duration,
	patterns ...string,
) (messages <-chan RedisMessage, done <-chan struct{}, unsub func(), returnErr error) {
	var client RedisClient
	Redis(ctx, func(c RedisClient) {
		client = c
	})

	cancelCtx, cancel := context.WithCancel(ctx)
	out := make(chan RedisMessage, 1024)
	doneOnce := sync.OnceFunc(cancel)

	// ForEachMaster runs its callback CONCURRENTLY, one goroutine per master —
	// every write to state shared across callbacks must hold stateLock
	// (unsynchronized writes here crash-looped the connect fleet on 2026-07-18:
	// fatal concurrent map writes at startup with 32 masters)
	var stateLock sync.Mutex
	pubsubs := []*redis.PubSub{}
	masterAddrs := func() (map[string]bool, error) {
		addrs := map[string]bool{}
		clusterClient, ok := client.(*redis.ClusterClient)
		if !ok {
			return addrs, nil
		}
		err := clusterClient.ForEachMaster(cancelCtx, func(ctx context.Context, node *redis.Client) error {
			stateLock.Lock()
			defer stateLock.Unlock()
			addrs[node.Options().Addr] = true
			return nil
		})
		return addrs, err
	}

	drain := func(pubsub *redis.PubSub) {
		defer doneOnce()
		c := pubsub.Channel()
		for {
			select {
			case <-cancelCtx.Done():
				return
			case message, ok := <-c:
				if !ok {
					// the subscription died; the caller resubscribes + resyncs
					return
				}
				select {
				case <-cancelCtx.Done():
					return
				case out <- message:
				}
			}
		}
	}

	if clusterClient, ok := client.(*redis.ClusterClient); ok {
		initialAddrs, err := masterAddrs()
		if err != nil {
			cancel()
			returnErr = err
			return
		}
		if len(initialAddrs) == 0 {
			cancel()
			returnErr = fmt.Errorf("no master nodes")
			return
		}
		err = clusterClient.ForEachMaster(cancelCtx, func(ctx context.Context, node *redis.Client) error {
			pubsub := node.PSubscribe(cancelCtx, patterns...)
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				pubsubs = append(pubsubs, pubsub)
			}()
			go HandleError(func() {
				drain(pubsub)
			})
			return nil
		})
		if err != nil {
			for _, pubsub := range pubsubs {
				pubsub.Close()
			}
			cancel()
			returnErr = err
			return
		}
		// watch the master set: a moved slot emits events on the new owner,
		// which the current subscriptions do not cover
		go HandleError(func() {
			defer doneOnce()
			for {
				select {
				case <-cancelCtx.Done():
					return
				case <-time.After(topologyRefresh):
				}
				clusterClient.ReloadState(cancelCtx)
				addrs, err := masterAddrs()
				if err != nil || !maps.Equal(addrs, initialAddrs) {
					return
				}
			}
		})
	} else {
		pubsub := client.PSubscribe(cancelCtx, patterns...)
		pubsubs = append(pubsubs, pubsub)
		go HandleError(func() {
			drain(pubsub)
		})
	}

	messages = out
	done = cancelCtx.Done()
	unsub = func() {
		doneOnce()
		for _, pubsub := range pubsubs {
			pubsub.Close()
		}
	}
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

// channel messages can be: RedisMessage, RedisSubscription
func Subscribe(ctx context.Context, channels ...string) (update <-chan any, unsub func()) {
	Redis(ctx, func(client RedisClient) {
		pubsub := client.SSubscribe(ctx, channels...)
		update = pubsub.ChannelWithSubscriptions()
		unsub = func() {
			pubsub.Close()
		}
	})
	return
}

func RedisSetIfEqual(r RedisClient, ctx context.Context, key string, test []byte, value []byte, ttl time.Duration) *redis.Cmd {
	script := `local key = KEYS[1] local expected_value = ARGV[1] local new_value = ARGV[2] local ttl = ARGV[3] local current_value = redis.call('GET', key) if current_value == expected_value then redis.call('SET', key, new_value) redis.call('EXPIRE', key, ttl) return 1 else return 0 end`
	return r.Eval(ctx, script, []string{key}, test, value, (ttl+time.Second/2)/time.Second)
}

func RedisRemoveIfEqual(r RedisClient, ctx context.Context, key string, test []byte) *redis.Cmd {
	script := `local key = KEYS[1] local expected_value = ARGV[1] local current_value = redis.call('GET', key) if current_value == expected_value then redis.call('DEL', key) return 1 else return 0 end`
	return r.Eval(ctx, script, []string{key}, test)
}
