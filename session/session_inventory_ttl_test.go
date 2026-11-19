package session

// Inventory reads keep a network's shared session keys alive past their
// horizon, and refresh the ttl only when the expiry falls short of it.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The expiry of each shared key of the network that exists with a ttl, by key.
// A shared key without a ttl fails the test.
func sessionSharedKeyExpireTimes(t testing.TB, ctx context.Context, networkId server.Id) map[string]time.Time {
	t.Helper()
	keyExpireTimes := map[string]time.Time{}
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		for _, key := range sessionSharedKeys(networkId) {
			expireTime, err := r.PExpireTime(ctx, key).Result()
			if err != nil {
				return err
			}
			switch expireTime {
			case -2:
				// no such key
			case -1:
				t.Errorf("shared key %s has no ttl", key)
			default:
				keyExpireTimes[key] = time.UnixMilli(expireTime.Milliseconds())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return keyExpireTimes
}

// Main 2026-11-15 (CPUOPT6): each connect hint read moved its 24h horizon by
// the time since the last read and refreshed the network's shared keys, so
// about 24k reads a second each issued PEXPIREATs, and every refresh of the
// event id key reached every connect process as an expire event. A read that
// finds the keys alive past its horizon writes no ttl and publishes nothing.
func TestSessionRevisionReadWithAmpleTtlWritesNoTtl(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Testing_EnableKeyspaceNotifications(ctx)
		networkId := server.NewId()

		// the first read creates the revision keys and gives them a ttl
		generation, eventId, err := NetworkSessionRevision(ctx, networkId)
		if err != nil {
			t.Fatal(err)
		}
		keyExpireTimes := sessionSharedKeyExpireTimes(t, ctx, networkId)
		if len(keyExpireTimes) != 2 {
			t.Fatalf("the first read left %d shared keys with a ttl; want the generation and event id", len(keyExpireTimes))
		}

		// every event of the network's keys, ttl refreshes included: a plain
		// subscription, without the key-event transport's filter
		var pubsub server.RedisPubSub
		server.Redis(ctx, func(r server.RedisClient) {
			pubsub = r.PSubscribe(ctx, fmt.Sprintf("__keyspace@%d__:{ns_%s}*", server.RedisDb(), networkId))
		})
		defer pubsub.Close()
		if _, err := pubsub.Receive(ctx); err != nil {
			t.Fatal(err)
		}
		messages := pubsub.Channel()

		now := server.NowUtc()
		for _, offset := range []time.Duration{time.Millisecond, time.Minute, 30 * time.Minute, 59 * time.Minute} {
			inventory, err := readSessionInventory(ctx, networkId, now.Add(offset))
			if err != nil {
				t.Fatal(err)
			}
			if inventory.Generation != generation || inventory.EventId != eventId {
				t.Fatal("a read changed the revision", inventory.Generation, inventory.EventId)
			}
		}
		if _, _, err := NetworkSessionRevision(ctx, networkId); err != nil {
			t.Fatal(err)
		}

		// a write after the reads: one node's key events arrive in order
		barrierKey := SessionKey(networkId, "barrier")
		barrierChannel := fmt.Sprintf("__keyspace@%d__:%s", server.RedisDb(), barrierKey)
		server.Raise(server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Set(ctx, barrierKey, "1", time.Minute).Err()
		}))
		for barrier := false; !barrier; {
			select {
			case message, ok := <-messages:
				if !ok {
					t.Fatal("the subscription ended")
				}
				if message.Channel == barrierChannel {
					barrier = true
				} else {
					t.Errorf("a read with an ample ttl published %s on %s", message.Payload, message.Channel)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the barrier write's event did not arrive")
			}
		}

		for key, expireTime := range sessionSharedKeyExpireTimes(t, ctx, networkId) {
			if !expireTime.Equal(keyExpireTimes[key]) {
				t.Errorf("a read with an ample ttl moved %s's expiry from %s to %s", key, keyExpireTimes[key], expireTime)
			}
		}
	})
}

// The headroom changes when a refresh happens, never how long the keys
// outlive a read: after every read each shared key lives past the read's
// horizon, the later of the read plus the retention and the latest retained
// session. A network read steadily is refreshed about once per headroom.
func TestSessionInventoryReadKeepsSharedKeysPastHorizon(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := defaultSessionInventorySettings()
		start := server.NowUtc()

		// a network with no session holds only its revision keys, as every
		// network did on Main while new session creation was dark; the other
		// holds a password session, so the live set, the retained set, the
		// revision keys and the sign-in class index exist
		networkId := server.NewId()
		sessionNetworkId := server.NewId()
		registration, err := RegisterNetworkSession(ctx, sessionFixtureCredential(sessionNetworkId, start), "password", nil, false, start)
		if err != nil {
			t.Fatal(err)
		}
		retainUntil := time.UnixMilli(registration.RetainUntil)

		span := 5 * time.Hour
		step := 7 * time.Minute
		refreshCount := 0
		var lastExpireTime time.Time
		for offset := time.Duration(0); offset <= span; offset += step {
			// a read sends its clock in whole milliseconds
			at := time.UnixMilli(start.Add(offset).UnixMilli())
			for _, id := range []server.Id{networkId, sessionNetworkId} {
				if _, err := readSessionInventory(ctx, id, at); err != nil {
					t.Fatal(err)
				}
				horizon := at.Add(settings.retention)
				keyCount := 2
				if id == sessionNetworkId {
					horizon = retainUntil
					keyCount = 5
				}
				keyExpireTimes := sessionSharedKeyExpireTimes(t, ctx, id)
				if len(keyExpireTimes) != keyCount {
					t.Fatalf("%d shared keys have a ttl after the read at +%s; want %d", len(keyExpireTimes), offset, keyCount)
				}
				for key, expireTime := range keyExpireTimes {
					if expireTime.Before(horizon) {
						t.Errorf("after the read at +%s %s expires at %s, before the horizon %s", offset, key, expireTime, horizon)
					}
					if horizon.Add(settings.retentionHeadroom).Before(expireTime) {
						t.Errorf("after the read at +%s %s expires at %s, past the horizon %s and its headroom", offset, key, expireTime, horizon)
					}
				}
				if id == networkId {
					if expireTime := keyExpireTimes[SessionKey(networkId, "eid")]; !expireTime.Equal(lastExpireTime) {
						refreshCount += 1
						lastExpireTime = expireTime
					}
				}
			}
		}
		// the first read, then about one refresh per headroom
		if maxRefreshCount := 2 + int(span/settings.retentionHeadroom); maxRefreshCount < refreshCount {
			t.Errorf("%d reads over %s refreshed the ttl %d times; want at most %d", int(span/step)+1, span, refreshCount, maxRefreshCount)
		}
	})
}
