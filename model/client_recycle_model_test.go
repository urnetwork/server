package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The recycle flag's keyspace channel parses back to its client, and only its
// own parser accepts it.
func TestClientRecycleKeyEvents(t *testing.T) {
	clientId := server.NewId()

	recycleChannel := "__keyspace@0__:" + clientRecycleKey(clientId)
	if parsed, ok := ParseClientRecycleKeyEvent(recycleChannel); !ok || parsed != clientId {
		t.Fatalf("recycle channel parsed to %s %t", parsed, ok)
	}
	removedChannel := "__keyspace@0__:" + clientRemovedKey(clientId)
	closeChannel := "__keyspace@0__:" + clientCloseKey(clientId)
	if _, ok := ParseClientRecycleKeyEvent(removedChannel); ok {
		t.Fatal("recycle parser accepted a removed channel")
	}
	if _, ok := ParseClientRecycleKeyEvent(closeChannel); ok {
		t.Fatal("recycle parser accepted a close channel")
	}
	if _, ok := ParseClientRemovedKeyEvent(recycleChannel); ok {
		t.Fatal("removed parser accepted a recycle channel")
	}
	if _, ok := ParseClientCloseKeyEvent(recycleChannel); ok {
		t.Fatal("close parser accepted a recycle channel")
	}
	if _, ok := ParseClientRecycleKeyEvent("__keyspace@0__:{not-an-id}c_recycle"); ok {
		t.Fatal("recycle parser accepted a malformed id")
	}
	if ClientRecycleKeyEventPattern(3) != "__keyspace@3__:{*}c_recycle" {
		t.Fatalf("recycle pattern %s", ClientRecycleKeyEventPattern(3))
	}
	// one per-client hash tag: the key distributes with its client
	if clientRecycleKey(clientId) != "{"+clientId.String()+"}c_recycle" {
		t.Fatalf("recycle key %s", clientRecycleKey(clientId))
	}
}

// The flag is set once per cooldown: a second set inside the cooldown is a
// miss, another client's flag is its own, and the ttl is the cooldown. A
// cooldown that is not positive sets nothing, so no flag is ever immortal.
func TestSetClientRecycleOncePerCooldown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		clientId := server.NewId()
		otherClientId := server.NewId()
		cooldown := time.Hour

		set, err := SetClientRecycle(ctx, clientId, cooldown)
		if err != nil || !set {
			t.Fatalf("first set = %t, %v; want set", set, err)
		}
		set, err = SetClientRecycle(ctx, clientId, cooldown)
		if err != nil || set {
			t.Fatalf("set inside the cooldown = %t, %v; want a miss", set, err)
		}
		set, err = SetClientRecycle(ctx, otherClientId, cooldown)
		if err != nil || !set {
			t.Fatalf("another client's set = %t, %v; want set", set, err)
		}

		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, clientRecycleKey(clientId)).Result()
			server.Raise(err)
		})
		if ttl <= 0 || cooldown < ttl {
			t.Fatalf("recycle flag ttl %s, want (0, %s]", ttl, cooldown)
		}

		// past the cooldown the flag can be set again
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, clientRecycleKey(clientId)).Err())
		})
		set, err = SetClientRecycle(ctx, clientId, cooldown)
		if err != nil || !set {
			t.Fatalf("set after the cooldown = %t, %v; want set", set, err)
		}

		unsetClientId := server.NewId()
		if set, err := SetClientRecycle(ctx, unsetClientId, 0); err == nil || set {
			t.Fatalf("a zero cooldown = %t, %v; want an error", set, err)
		}
		var exists int64
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			exists, err = r.Exists(ctx, clientRecycleKey(unsetClientId)).Result()
			server.Raise(err)
		})
		if exists != 0 {
			t.Fatal("a zero cooldown set a flag")
		}
	})
}
