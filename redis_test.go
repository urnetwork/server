package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

// TestRedisGetSetPipeline exercises the basic command path through the `Redis`
// wrapper: SET, GET (hit and miss), and a pipeline.
func TestRedisGetSetPipeline(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx := context.Background()

		key := fmt.Sprintf("test:redis:get-set:%s", NewId())
		pipelineKey := fmt.Sprintf("test:redis:pipeline:%s", NewId())
		missingKey := fmt.Sprintf("test:redis:missing:%s", NewId())

		Redis(ctx, func(r RedisClient) {
			// SET then GET the value back
			Raise(r.Set(ctx, key, "value1", 30*time.Second).Err())

			got, err := r.Get(ctx, key).Result()
			Raise(err)
			assert.Equal(t, got, "value1")

			// GET on a key that was never set returns RedisNil
			_, err = r.Get(ctx, missingKey).Result()
			assert.Equal(t, err, RedisNil)

			// pipeline: queue SET + GET on the same key and exec in one round
			// trip. The same key keeps both commands on one slot, so this is
			// also valid against a cluster client.
			pipe := r.Pipeline()
			setCmd := pipe.Set(ctx, pipelineKey, "value2", 30*time.Second)
			getCmd := pipe.Get(ctx, pipelineKey)
			_, err = pipe.Exec(ctx)
			Raise(err)
			Raise(setCmd.Err())

			pipelined, err := getCmd.Result()
			Raise(err)
			assert.Equal(t, pipelined, "value2")

			r.Del(ctx, key, pipelineKey)
		})
	})
}

// TestRedisPublishSubscribe exercises sharded pub/sub: a message published with
// `SPublish` is delivered to a subscriber created with the `Subscribe` wrapper
// (which uses `SSubscribe`). `Subscribe` returns a channel carrying both the
// subscription confirmation and subsequent messages, so the test waits for the
// confirmation before publishing -- pub/sub does not buffer for a subscriber
// that is not yet active.
func TestRedisPublishSubscribe(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		channel := fmt.Sprintf("test:redis:channel:%s", NewId())
		message := "hello from SPublish"

		ch, closeSubscribe := Subscribe(ctx, channel)
		defer closeSubscribe()

		// Wait until the subscription is active before publishing. The channel
		// may also carry other control events (e.g. a health-check pong), so
		// read until the subscription confirmation arrives.
	waitSubscribed:
		for {
			select {
			case event := <-ch:
				if _, ok := event.(RedisSubscription); ok {
					break waitSubscribed
				}
			case <-time.After(10 * time.Second):
				Raise(fmt.Errorf("timed out waiting for subscription confirmation"))
			}
		}

		Redis(ctx, func(r RedisClient) {
			Raise(r.SPublish(ctx, channel, message).Err())
		})

		// Wait for the published message, skipping any non-message events.
		var received RedisMessage
	waitMessage:
		for {
			select {
			case event := <-ch:
				if m, ok := event.(RedisMessage); ok {
					received = m
					break waitMessage
				}
			case <-time.After(10 * time.Second):
				Raise(fmt.Errorf("timed out waiting for published message"))
			}
		}

		assert.Equal(t, received.Channel, channel)
		assert.Equal(t, received.Payload, message)
	})
}
