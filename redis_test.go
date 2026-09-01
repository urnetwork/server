package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"
)

// TestRedisDoOnceDisablesCommandAndCallbackRetries pins both retry layers that
// can otherwise replay a non-idempotent pipeline after Redis applied it but its
// response was lost. Source-of-truth reconciliation, not retransmission, owns
// recovery for these calls.
func TestRedisDoOnceDisablesCommandAndCallbackRetries(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		callbackAttempts := 0
		func() {
			defer func() {
				if recovered := recover(); recovered == nil {
					t.Fatal("RedisDoOnce swallowed the synthetic connection error")
				}
			}()
			RedisDoOnce(ctx, func(client RedisClient) {
				callbackAttempts++
				switch typed := client.(type) {
				case *redis.Client:
					if typed.Options().MaxRetries != 0 {
						t.Fatalf("standalone command retries = %d, want 0", typed.Options().MaxRetries)
					}
				case *redis.ClusterClient:
					if typed.Options().MaxRedirects != 0 || typed.Options().MaxRetries > 0 {
						t.Fatalf(
							"cluster retries = redirects:%d node:%d, want both disabled",
							typed.Options().MaxRedirects,
							typed.Options().MaxRetries,
						)
					}
				default:
					t.Fatalf("unexpected RedisDoOnce client type %T", client)
				}
				panic(errors.New("synthetic i/o timeout"))
			})
		}()
		connect.AssertEqual(t, callbackAttempts, 1)
	})
}

func TestGoRedisNoRetrySentinelsDisableBothClientKinds(t *testing.T) {
	standalone := redis.NewClient(&redis.Options{
		Addr:       "127.0.0.1:1",
		MaxRetries: -1,
	})
	defer standalone.Close()
	if standalone.Options().MaxRetries != 0 {
		t.Fatalf("standalone -1 normalized to %d retries, want 0", standalone.Options().MaxRetries)
	}

	cluster := redis.NewClusterClient(&redis.ClusterOptions{
		Addrs:        []string{"127.0.0.1:1"},
		MaxRetries:   -1,
		MaxRedirects: -1,
	})
	defer cluster.Close()
	if cluster.Options().MaxRetries > 0 || cluster.Options().MaxRedirects != 0 {
		t.Fatalf(
			"cluster -1 normalized to redirects:%d node:%d, want both disabled",
			cluster.Options().MaxRedirects,
			cluster.Options().MaxRetries,
		)
	}
}

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

// TestSubscribeKeyEventsConnDeathResubscribes pins the reconnect-detection
// contract of `SubscribeKeyEvents`: go-redis transparently reconnects and
// re-PSUBSCRIBEs a killed pubsub connection, and events published during the
// gap are gone — so a post-initial psubscribe confirmation must terminate the
// epoch (`done` fires) and force the caller to resubscribe + resync. Without
// the detection, a killed connection would silently resume with a gap and the
// caller would never resync (REVIEW2 §5, PEERSSTREAMS2 §8).
func TestSubscribeKeyEventsConnDeathResubscribes(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		Testing_EnableKeyspaceNotifications(ctx)

		prefix := fmt.Sprintf("test:kes:%s", NewId())
		pattern := fmt.Sprintf("__keyspace@%d__:%s:*", RedisDb(), prefix)

		messages, done, unsub, err := SubscribeKeyEvents(ctx, 60*time.Second, pattern)
		Raise(err)
		defer unsub()

		// the subscription delivers: a SET on a matching key arrives. Retry
		// the SET until delivery — psubscribe confirmation timing means the
		// first write can race the subscription becoming active.
		deliverCtx, deliverCancel := context.WithTimeout(ctx, 15*time.Second)
		defer deliverCancel()
		delivered := false
		for !delivered {
			Redis(ctx, func(r RedisClient) {
				Raise(r.Set(ctx, fmt.Sprintf("%s:a", prefix), "1", 30*time.Second).Err())
			})
			select {
			case <-deliverCtx.Done():
				t.Fatal("no key event delivered before conn kill")
			case message := <-messages:
				connect.AssertEqual(t, message.Payload, "set")
				delivered = true
			case <-time.After(200 * time.Millisecond):
			}
		}

		// kill every pubsub connection server-side; go-redis auto-reconnects
		// and re-PSUBSCRIBEs, which must surface as an epoch termination
		Redis(ctx, func(r RedisClient) {
			Raise(r.Do(ctx, "CLIENT", "KILL", "TYPE", "pubsub").Err())
		})

		select {
		case <-done:
			// the epoch ended: the caller resubscribes + resyncs (the
			// key-event subscriber's run loop does exactly this)
		case <-time.After(15 * time.Second):
			t.Fatal("done did not fire after the pubsub connection was killed")
		}
	})
}
