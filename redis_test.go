package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// TestRedisGetSetPipeline exercises the basic command path through the `Redis`
// wrapper: SET, GET (hit and miss), and a pipeline.
func TestRedisGetSetPipeline(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()

		key := fmt.Sprintf("test:redis:get-set:%s", NewId())
		pipelineKey := fmt.Sprintf("test:redis:pipeline:%s", NewId())
		missingKey := fmt.Sprintf("test:redis:missing:%s", NewId())

		Redis(ctx, func(r RedisClient) {
			// SET then GET the value back
			Raise(r.Set(ctx, key, "value1", 30*time.Second).Err())

			got, err := r.Get(ctx, key).Result()
			Raise(err)
			connect.AssertEqual(t, got, "value1")

			// GET on a key that was never set returns RedisNil
			_, err = r.Get(ctx, missingKey).Result()
			connect.AssertEqual(t, err, RedisNil)

			// Pipeline SET + GET on the same key in one round trip. Same key
			// keeps both on one slot, so this also works against a cluster.
			pipe := r.Pipeline()
			setCmd := pipe.Set(ctx, pipelineKey, "value2", 30*time.Second)
			getCmd := pipe.Get(ctx, pipelineKey)
			_, err = pipe.Exec(ctx)
			Raise(err)
			Raise(setCmd.Err())

			pipelined, err := getCmd.Result()
			Raise(err)
			connect.AssertEqual(t, pipelined, "value2")

			r.Del(ctx, key, pipelineKey)
		})
	})
}

// TestRedisPublishSubscribe exercises sharded pub/sub: `SPublish` delivers to a
// subscriber from the `Subscribe` wrapper (which uses `SSubscribe`). `Subscribe`'s
// channel carries the subscription confirmation and messages, so the test waits
// for the confirmation before publishing -- pub/sub doesn't buffer for an
// inactive subscriber.
func TestRedisPublishSubscribe(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		channel := fmt.Sprintf("test:redis:channel:%s", NewId())
		message := "hello from SPublish"

		ch, closeSubscribe := Subscribe(ctx, channel)
		defer closeSubscribe()

		// Wait for the subscription to go active before publishing. The channel
		// may carry other control events (e.g. a health-check pong), so read
		// until the subscription confirmation arrives.
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

		connect.AssertEqual(t, received.Channel, channel)
		connect.AssertEqual(t, received.Payload, message)
	})
}
