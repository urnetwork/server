package model

// A read that misses the provide mirror refills it from a postgres snapshot
// that a concurrent SetProvide can make stale. These tests force both orders
// of that race directly: the snapshot is taken, then the provide commits and
// writes its mirror (or not yet), then the stale snapshot is offered to the
// refill.

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// Asserts the mirror describes exactly the newer provide: Stream with its new
// secret, and no Public secret left from the older provide.
func assertProvideMirrorIsNewer(t testing.TB, ctx context.Context, clientId server.Id, streamKey []byte) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result()
		if provideModesListJson != "[4]" {
			t.Errorf("mirrored mode list = %q, want [4]", provideModesListJson)
		}
		secretKeyStr, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModeStream)).Result()
		if !bytes.Equal([]byte(secretKeyStr), streamKey) {
			t.Errorf("mirrored stream secret = %x, want %x", secretKeyStr, streamKey)
		}
		if exists := r.Exists(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Val(); exists != 0 {
			t.Error("the removed public secret is still mirrored")
		}
	})
	if _, err := GetProvideSecretKey(ctx, clientId, ProvideModePublic); err == nil {
		t.Error("the removed public mode still returns a secret")
	}
}

// The provide lands between the refill's read and its write. The refill
// finds the provide's mirror and writes nothing.
func TestProvideMirrorRefillNeverReplacesNewerProvide(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: bytes.Repeat([]byte{0x61}, 32),
			ProvideModePublic: bytes.Repeat([]byte{0x62}, 32),
		})
		Testing_DeleteProvideMirror(ctx, clientId)
		staleSecretKeys := loadProvideSecretKeys(ctx, clientId)

		streamKey := bytes.Repeat([]byte{0x63}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: streamKey,
		})
		refillProvideMirror(ctx, clientId, staleSecretKeys)

		assertProvideMirrorIsNewer(t, ctx, clientId, streamKey)
	})
}

// The refill writes its stale snapshot first. The provide's unconditional
// write replaces the list and its secrets and deletes the removed secret.
func TestProvideMirrorProvideReplacesEarlierRefill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: bytes.Repeat([]byte{0x64}, 32),
			ProvideModePublic: bytes.Repeat([]byte{0x65}, 32),
		})
		Testing_DeleteProvideMirror(ctx, clientId)
		staleSecretKeys := loadProvideSecretKeys(ctx, clientId)
		refillProvideMirror(ctx, clientId, staleSecretKeys)

		streamKey := bytes.Repeat([]byte{0x66}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: streamKey,
		})

		assertProvideMirrorIsNewer(t, ctx, clientId, streamKey)
	})
}
