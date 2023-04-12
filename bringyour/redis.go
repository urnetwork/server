package bringyour

import (
	"sync"
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)


type SafeRedisClient struct {
	mutex sync.Mutex
	client *redis.Client
}
func (self SafeRedisClient) open() *redis.Client {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client == nil {
		// see https://github.com/redis/go-redis/blob/master/options.go#L31
		options := &redis.Options{
	        Addr: "192.168.208.135:6379",
	        Password: "",
	        DB: 0,
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
func (self SafeRedisClient) close() {
	self.mutex.Lock()
	defer self.mutex.Unlock()

	if self.client != nil {
		self.client.Close()
		self.client = nil
	}
}


var safeClient *SafeRedisClient = &SafeRedisClient{}

func client() *redis.Client {
	return safeClient.open()
}



func Redis(callback func(context context.Context, client *redis.Client)) {
	// From the go-redis code:
	// >> Client is a Redis client representing a pool of zero or more underlying connections.
	// >> It's safe for concurrent use by multiple goroutines.
	context := context.Background()
    callback(context, client())
}
