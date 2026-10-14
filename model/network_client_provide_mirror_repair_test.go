// A provide mode mirror that cannot be decoded must be repaired by the read
// that finds it. Before, the refill never overwrote an existing value, so the
// undecodable value stayed for the mirror's 72 hour ttl: every read of the
// client went to postgres, and callers that treat GetProvideModes' error as
// no modes denied or evicted a providing client for that whole time.
package model

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The read that finds an undecodable mirror returns the keys' modes and
// leaves a decodable mirror of them behind, so later reads are served from
// Redis again.
func TestGetProvideModesRepairsAnUndecodableMirror(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
		})
		key := provideModesKey(clientId)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, "not a mode list", time.Hour).Err())
		})

		provideModes, err := GetProvideModes(ctx, clientId)

		mirrored := ""
		server.Redis(ctx, func(r server.RedisClient) {
			mirrored, _ = r.Get(ctx, key).Result()
		})
		var provideModesList []ProvideMode
		if decodeErr := json.Unmarshal([]byte(mirrored), &provideModesList); decodeErr != nil {
			t.Fatalf("the undecodable provide mode mirror %q is still in place after a read (read error %v); every later read goes to postgres until its ttl", mirrored, err)
		}
		if !slices.Contains(provideModesList, ProvideModePublic) {
			t.Fatalf("the repaired mirror lists %v, want the keys' public mode", provideModesList)
		}
		if err != nil || !provideModes[ProvideModePublic] {
			t.Fatalf("the read returned %v with error %v, want the keys' public mode", provideModes, err)
		}
		if again, err := GetProvideModes(ctx, clientId); err != nil || !again[ProvideModePublic] {
			t.Fatalf("the read after the repair returned %v with error %v, want the public mode", again, err)
		}
	})
}

// A secret key lookup that misses Redis reads postgres and refills the
// mirror. With an undecodable mode list the refill wrote nothing, so every
// such lookup, as for a mode the client does not provide, read postgres
// until the list's ttl. The lookup now replaces the list too.
func TestGetProvideSecretKeyRepairsAnUndecodableMirror(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: make([]byte, 32),
		})
		key := provideModesKey(clientId)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, "not a mode list", time.Hour).Err())
		})

		if _, err := GetProvideSecretKey(ctx, clientId, ProvideModeNetwork); err == nil {
			t.Fatal("fixture: the client provides no network mode key")
		}

		mirrored := ""
		server.Redis(ctx, func(r server.RedisClient) {
			mirrored, _ = r.Get(ctx, key).Result()
		})
		var provideModesList []ProvideMode
		if err := json.Unmarshal([]byte(mirrored), &provideModesList); err != nil {
			t.Fatalf("the undecodable provide mode mirror %q is still in place after a secret key lookup; every lookup that misses Redis reads postgres until its ttl", mirrored)
		}
		if !slices.Contains(provideModesList, ProvideModePublic) {
			t.Fatalf("the repaired mirror lists %v, want the keys' public mode", provideModesList)
		}
		if secretKey, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic); err != nil || len(secretKey) != 32 {
			t.Fatalf("the public secret key after the repair is %d bytes with error %v, want 32", len(secretKey), err)
		}
	})
}
