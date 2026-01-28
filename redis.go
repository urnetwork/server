package server

import (
	"context"
	"sync"
	"time"
	// "strings"
	"net"
	// "net/netip"
	// "fmt"

	"github.com/redis/go-redis/v9"
)

// type aliases to simplify user code
type RedisClient = redis.Cmdable

const RedisNil = redis.Nil

type safeRedisClient struct {
	mutex  sync.Mutex
	client redis.Cmdable
}

func (self *safeRedisClient) open() redis.Cmdable {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client == nil {
		redisKeys := Vault.RequireSimpleResource("redis.yml")
		redisConfigKeys := Config.RequireSimpleResource("redis.yml")

		minConnections := redisConfigKeys.RequireInt("min_connections")
		maxConnections := redisConfigKeys.RequireInt("max_connections")
		maxRetries := 32
		connectionMaxLifetime := "1h"
		connectionMaxIdleTime := "5m"
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

		dialer := &net.Dialer{
			Timeout: 90 * time.Second,
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
				ConnMaxLifetime: connectionMaxLifetimeDuration,
				ConnMaxIdleTime: connectionMaxIdleTimeDuration,
				// see https://redis.uptrace.dev/guide/go-redis-debugging.html#timeouts
				// see https://uptrace.dev/blog/golang-context-timeout.html
				ContextTimeoutEnabled: false,
				ReadTimeout:           readTimeout,
				WriteTimeout:          writeTimeout,
				Dialer:                dialContext,
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
				ConnMaxLifetime: connectionMaxLifetimeDuration,
				ConnMaxIdleTime: connectionMaxIdleTimeDuration,
				// see https://redis.uptrace.dev/guide/go-redis-debugging.html#timeouts
				// see https://uptrace.dev/blog/golang-context-timeout.html
				ContextTimeoutEnabled: false,
				ReadTimeout:           readTimeout,
				WriteTimeout:          writeTimeout,
				Dialer:                dialContext,
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

func client() redis.Cmdable {
	return safeClient.open()
}

func Redis(ctx context.Context, callback func(RedisClient)) {
	// From the go-redis code:
	// >> Client is a Redis client representing a pool of zero or more underlying connections.
	// >> It's safe for concurrent use by multiple goroutines.
	// context := context.Background()
	client := client()
	callback(client)
}

func RedisSetIfEqual(r RedisClient, ctx context.Context, key string, test []byte, value []byte, ttl time.Duration) *redis.Cmd {
	script := `local key = KEYS[1] local expected_value = ARGV[1] local new_value = ARGV[2] local ttl = ARGV[3] local current_value = redis.call('GET', key) if current_value == expected_value then redis.call('SET', key, new_value) redis.call('EXPIRE', key, ttl) return 1 else return 0 end`
	return r.Eval(ctx, script, []string{key}, test, value, (ttl+time.Second/2)/time.Second)
}

func RedisRemoveIfEqual(r RedisClient, ctx context.Context, key string, test []byte) *redis.Cmd {
	script := `local key = KEYS[1] local expected_value = ARGV[1] local current_value = redis.call('GET', key) if current_value == expected_value then redis.call('DEL', key) return 1 else return 0 end`
	return r.Eval(ctx, script, []string{key}, test)
}
