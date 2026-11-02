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

// The flag is set once per ttl: a second set inside it is a miss that reads
// back when the flag was set, another client's flag is its own, and the ttl
// is the one given. Once the flag expires it can be set again. A flag whose
// value is unreadable is a miss with no set time. A ttl that is not positive
// sets nothing, so no flag is ever immortal.
func TestSetClientRecycleOncePerCooldown(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)
		clientId := server.NewId()
		otherClientId := server.NewId()
		cooldown := time.Hour

		setTime := server.NowUtc()
		set, flagTime, err := SetClientRecycle(ctx, clientId, cooldown)
		if err != nil || !set || !flagTime.IsZero() {
			t.Fatalf("first set = %t, %s, %v; want set", set, flagTime, err)
		}
		set, flagTime, err = SetClientRecycle(ctx, clientId, cooldown)
		if err != nil || set {
			t.Fatalf("set inside the cooldown = %t, %v; want a miss", set, err)
		}
		// the first set's time, to the millisecond
		if flagTime.Before(setTime.Truncate(time.Millisecond)) || server.NowUtc().Before(flagTime) {
			t.Fatalf("the miss read back %s, want the first set at %s", flagTime, setTime)
		}
		set, _, err = SetClientRecycle(ctx, otherClientId, cooldown)
		if err != nil || !set {
			t.Fatalf("another client's set = %t, %v; want set", set, err)
		}

		pttl := func(clientId server.Id) time.Duration {
			var ttl time.Duration
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				ttl, err = r.PTTL(ctx, clientRecycleKey(clientId)).Result()
				server.Raise(err)
			})
			return ttl
		}
		if ttl := pttl(clientId); ttl <= 0 || cooldown < ttl {
			t.Fatalf("recycle flag ttl %s, want (0, %s]", ttl, cooldown)
		}

		// a flag that expires can be set again
		shortClientId := server.NewId()
		shortTtl := 1500 * time.Millisecond
		if set, _, err := SetClientRecycle(ctx, shortClientId, shortTtl); err != nil || !set {
			t.Fatalf("a short flag = %t, %v; want set", set, err)
		}
		if set, _, err := SetClientRecycle(ctx, shortClientId, shortTtl); err != nil || set {
			t.Fatalf("a short flag inside its ttl = %t, %v; want a miss", set, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for 0 < pttl(shortClientId) {
			if time.Now().After(deadline) {
				t.Fatal("the short flag never expired")
			}
			time.Sleep(100 * time.Millisecond)
		}
		if set, _, err := SetClientRecycle(ctx, shortClientId, shortTtl); err != nil || !set {
			t.Fatalf("set after the flag expired = %t, %v; want set", set, err)
		}

		// an unreadable value: a miss with no set time
		garbledClientId := server.NewId()
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, clientRecycleKey(garbledClientId), "not-a-time", time.Minute).Err())
		})
		if set, flagTime, err := SetClientRecycle(ctx, garbledClientId, cooldown); err != nil || set || !flagTime.IsZero() {
			t.Fatalf("an unreadable flag = %t, %s, %v; want a miss with no time", set, flagTime, err)
		}

		unsetClientId := server.NewId()
		if set, _, err := SetClientRecycle(ctx, unsetClientId, 0); err == nil || set {
			t.Fatalf("a zero ttl = %t, %v; want an error", set, err)
		}
		var exists int64
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			exists, err = r.Exists(ctx, clientRecycleKey(unsetClientId)).Result()
			server.Raise(err)
		})
		if exists != 0 {
			t.Fatal("a zero ttl set a flag")
		}
	})
}
