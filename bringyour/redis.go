package bringyour

import (
	"context"

	"github.com/redis/go-redis/v9"
)


func Redis(callback func(context context.Context, client *redis.Client)) {
	// fixme use a pool
	
	context := context.Background()
	client := redis.NewClient(&redis.Options{
        Addr:     "localhost:6379",
        Password: "", // no password set
        DB:       0,  // use default DB
    })
    callback(context, client)

    client.Close()
}
