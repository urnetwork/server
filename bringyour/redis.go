package bringyour

import (
	"sync"
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)


// type aliases to simplify user code
type RedisClient = *redis.Client
const RedisNil = redis.Nil


type safeRedisClient struct {
	mutex sync.Mutex
	client *redis.Client
}
func (self *safeRedisClient) open() *redis.Client {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client == nil {
		redisKeys := Vault.RequireSimpleResource("redis.yml")

		// see https://github.com/redis/go-redis/blob/master/options.go#L31
		options := &redis.Options{
			Addr: redisKeys.RequireString("authority"),
	        Password: redisKeys.RequireString("password"),
	        DB: redisKeys.RequireInt("db"),
	        // Addr: "192.168.208.135:6379",
	        // Password: "",
	        // DB: 0,
	        MaxRetries: 32,
	        MinIdleConns: 4,
			MaxIdleConns: 32,
			ConnMaxLifetime: 0,
			ConnMaxIdleTime: 60 * time.Second,
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


var safeClient *safeRedisClient = &safeRedisClient{}

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
