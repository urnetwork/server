package server

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"
	// "net/netip"
	// "fmt"
	"fmt"
	"runtime"

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

		readTimeout := 30 * time.Second
		writeTimeout := 15 * time.Second
		poolTimeout := 5 * time.Minute
		dialTimeout := 30 * time.Second
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
	} else {
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
