package model

// §6.8's hint state on the private test Redis (PROTO-M1-07): sidecartest.RunPushStoreSuite with the real
// Lua, which T2 runs on FakeRedis and the Go twins, so a twin that drifts fails one of the two (D-TEST-2).
// Then what only a real Redis shows: the keyspace events production's classes emit, the keys' ttls, and
// every key in a §2.8 row. The test Redis is one node, so §22.4's three-master run of the scripts waits on
// T3's cluster (TEST-M1-08); the slot rule is held at run time by SidecarPushRedis and statically by
// sidecartest/crossslot.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/pushservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// Calls callback with each keyspace event's key under {scp_*} until stop: one subscription for the
// test, as a process's shared subscriber would hold it.
func testSidecarPushWatchKeys(ctx context.Context) func(callback func(key string)) (func(), error) {
	return func(callback func(key string)) (func(), error) {
		watchCtx, cancel := context.WithCancel(ctx)
		pattern := strings.Replace(ScpHintKeyEventPattern(server.RedisDb()), "}n", "}*", 1)
		messages, done, unsubscribe, err := server.SubscribeKeyEvents(watchCtx, 60*time.Second, pattern)
		if err != nil {
			cancel()
			return nil, err
		}
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			for {
				select {
				case <-watchCtx.Done():
					return
				case <-done:
					return
				case message := <-messages:
					if key, ok := ParseKeyspaceChannelKey(message.Channel); ok {
						callback(key)
					}
				}
			}
		}()
		return func() {
			cancel()
			unsubscribe()
			<-stopped
		}, nil
	}
}

func TestSidecarPushStorePassesItsSuite(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		server.Testing_EnableKeyspaceNotifications(ctx)
		sidecartest.RunPushStoreSuite(t, &sidecartest.PushStoreTarget{
			Redis: NewSidecarPushRedis(&SidecarPushRedisSettings{CallTimeout: 10 * time.Second}),
			// a real store waits: only the expiry case passes time, and the keys live 2 s here
			Pass: func(d time.Duration) {
				<-time.After(d)
			},
			KeyTimeout:   2 * time.Second,
			WatchKeys:    testSidecarPushWatchKeys(ctx),
			EventTimeout: 10 * time.Second,
		})
	})
}

// ver, hint and n live an hour from the first hint, which a later hint does not extend; tok, set by
// api's projection with no ttl, keeps none; and every key the writer and the ack touch is in a row of
// §2.8's list (connect-4 and api-6).
func TestSidecarPushKeysTtlsAndRows(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		pushRedis := NewSidecarPushRedis(nil)
		writer, err := pushservice.NewHintWriter(pushRedis, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		clientId := connect.NewId()
		if _, err := writer.HintClient(ctx, clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicCore, Version: 1_731_000_000_000_000}); err != nil {
			t.Fatal(err)
		}
		keys := []string{pushservice.HintVersionsKey(clientId), pushservice.HintStreamKey(clientId), pushservice.HintSeqKey(clientId)}
		ttls := map[string]time.Duration{}
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys {
				ttl, err := client.PTTL(ctx, key).Result()
				if err != nil {
					t.Fatal(err)
				}
				ttls[key] = ttl
				// shorten it by hand: a second hint must not lift it back
				if err := client.PExpire(ctx, key, 30*time.Minute).Err(); err != nil {
					t.Fatal(err)
				}
			}
		})
		for key, ttl := range ttls {
			if ttl <= 59*time.Minute || time.Hour < ttl {
				t.Errorf("%s lives %v", key, ttl)
			}
		}
		if _, err := writer.HintClient(ctx, clientId, &sidecar.ClientHint{Topic: sidecar.PushTopicCore, Version: 1_731_000_000_000_001}); err != nil {
			t.Fatal(err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys {
				ttl, err := client.PTTL(ctx, key).Result()
				if err != nil {
					t.Fatal(err)
				}
				if 30*time.Minute < ttl {
					t.Errorf("a second hint extended %s to %v", key, ttl)
				}
			}
		})
		if err := pushRedis.Set(ctx, pushservice.HintTokenKey(clientId), []byte("synthetic-token"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := pushservice.RaiseHintAck(ctx, pushRedis, clientId, "1700000000000-1", time.Hour); err != nil {
			t.Fatal(err)
		}
		if set, err := pushRedis.SetIfAbsent(ctx, pushservice.HintPollKey(clientId), []byte("1"), 300*time.Second); err != nil || !set {
			t.Fatalf("hb: %t, %v", set, err)
		}
		if set, err := pushRedis.SetIfAbsent(ctx, pushservice.HintPollKey(clientId), []byte("1"), 300*time.Second); err != nil || set {
			t.Fatalf("a second hb: %t, %v", set, err)
		}

		clientKeys := []string{}
		server.Redis(ctx, func(client server.RedisClient) {
			found, err := client.Keys(ctx, pushservice.HintTag(clientId)+"*").Result()
			if err != nil {
				t.Fatal(err)
			}
			clientKeys = found
		})
		if len(clientKeys) != 6 {
			t.Fatalf("keys %v", clientKeys)
		}
		guard, err := sidecartest.LoadSchemaGuard()
		if err != nil {
			t.Fatal(err)
		}
		findings, err := guard.CheckStores(ctx, &sidecartest.SchemaGuardStores{RedisKeys: clientKeys})
		if err != nil {
			t.Fatal(err)
		}
		for _, finding := range findings {
			t.Error(finding)
		}
	})
}

// api writes its own topics and no other owner's.
func TestSidecarHintOnlyApiTopics(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		for _, topic := range []string{sidecar.PushTopicCore, sidecar.PushTopicTeam} {
			if err := SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: topic, Version: 3}); err != nil {
				t.Fatalf("%s: %v", topic, err)
			}
		}
		for _, topic := range []string{sidecar.PushTopicGkm, sidecar.PushTopicSock, sidecar.PushTopicEvs(sidecar.NewId())} {
			if err := SidecarHint(ctx, []server.Id{clientId}, &sidecar.ClientHint{Topic: topic, Version: 3}); !errors.Is(err, ErrSidecarHintTopic) {
				t.Errorf("%s: %v", topic, err)
			}
		}
		fieldValues, err := NewSidecarPushRedis(nil).HashGetAll(ctx, pushservice.HintVersionsKey(connect.Id(clientId)))
		if err != nil {
			t.Fatal(err)
		}
		if len(fieldValues) != 2 || string(fieldValues[sidecar.PushTopicCore]) != "3" || string(fieldValues[sidecar.PushTopicTeam]) != "3" {
			t.Errorf("ver holds %v", fieldValues)
		}
	})
}

func TestScpHintKeyEventParses(t *testing.T) {
	clientId := connect.NewId()
	channel := "__keyspace@3__:" + pushservice.HintSeqKey(clientId)
	if parsed, ok := ParseScpHintKeyEvent(channel); !ok || parsed != clientId {
		t.Errorf("%s parsed as %s, %t", channel, parsed, ok)
	}
	for _, other := range []string{
		"__keyspace@3__:" + pushservice.HintVersionsKey(clientId),
		"__keyevent@3__:incrby",
		"__keyspace@3__:{scp_not-an-id}n",
		"no channel",
	} {
		if _, ok := ParseScpHintKeyEvent(other); ok {
			t.Errorf("%s parsed", other)
		}
	}
	if ScpHintKeyEventPattern(3) != "__keyspace@3__:{scp_*}n" {
		t.Errorf("pattern %s", ScpHintKeyEventPattern(3))
	}
}
