package server

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// type aliases to simplify user code
type RedisClient = *redis.Client

const RedisNil = redis.Nil

// resets the connection pool
// call this after changes to the env
func RedisReset() {
	safeClient.close()
	safeClient = &safeRedisClient{}
}

type safeRedisClient struct {
	mutex  sync.Mutex
	client *redis.Client
}

func (self *safeRedisClient) open() *redis.Client {
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
		}
		self.client = redis.NewClient(options)
	}
	return self.client
}
func (self *safeRedisClient) close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client != nil {
		self.client.Close()
		self.client = nil
	}
}

var safeClient = &safeRedisClient{}

func client() *redis.Client {
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
