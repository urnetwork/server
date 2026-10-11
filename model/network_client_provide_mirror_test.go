package model

// A client's provide mirror carries a ttl, and a client provides when its
// settings change, not periodically: a multi-client window provides once, when
// it is created. Once its mirror expired, every later lookup of the still
// active window read postgres. These tests drive the real lookups against the
// test database after the mirror is gone and count the PostgreSQL statements
// each sequence of reads costs.

import (
	"bytes"
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// Contract creation to a non-companion destination reads the destination's
// modes and then the selected mode's secret. After the mirror expires, the
// first pair of reads refills the mode list and every secret from one read,
// which one more read confirms is still current, and the rest are served by
// redis.
func TestProvideMirrorExpiryCostsOneRefill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		streamKey := bytes.Repeat([]byte{0x51}, 32)
		networkKey := bytes.Repeat([]byte{0x52}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream:  streamKey,
			ProvideModeNetwork: networkKey,
		})
		// what the ttl leaves 72h after the window's only provide
		Testing_DeleteProvideMirror(ctx, clientId)

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		for range 3 {
			provideModes, err := GetProvideModes(observed, clientId)
			if err != nil || len(provideModes) != 2 || !provideModes[ProvideModeStream] || !provideModes[ProvideModeNetwork] {
				t.Fatalf("modes after expiry = %v, %v", provideModes, err)
			}
			secretKey, err := GetProvideSecretKey(observed, clientId, ProvideModeStream)
			if err != nil || !bytes.Equal(secretKey, streamKey) {
				t.Fatalf("stream secret after expiry = %x, %v", secretKey, err)
			}
		}
		if reads := statements.take(); len(reads) != 2 {
			t.Fatalf("three mode and secret lookups after the mirror expired used %d statements, want 2 (the refill and its confirmation): %q", len(reads), reads)
		}
		secretKey, err := GetProvideSecretKey(observed, clientId, ProvideModeNetwork)
		if err != nil || !bytes.Equal(secretKey, networkKey) {
			t.Fatalf("network secret after refill = %x, %v", secretKey, err)
		}
		if reads := statements.take(); len(reads) != 0 {
			t.Fatalf("a secret of the refilled mirror used %d statements, want 0: %q", len(reads), reads)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			for _, key := range []string{
				provideModesKey(clientId),
				provideModeSecretKeyKey(clientId, ProvideModeStream),
				provideModeSecretKeyKey(clientId, ProvideModeNetwork),
			} {
				if ttl := r.TTL(ctx, key).Val(); ttl <= 0 || provideMirrorTtl < ttl {
					t.Errorf("refilled %s ttl = %v, want within %v", key, ttl, provideMirrorTtl)
				}
			}
		})
	})
}

// A companion request between networks reads only the Stream secret. After
// the mirror expires, its first read refills the whole mirror and confirms
// the fill, so the later secret reads and a later mode read cost nothing.
func TestProvideSecretKeyExpiryCostsOneRefill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		streamKey := bytes.Repeat([]byte{0x53}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: streamKey,
		})
		Testing_DeleteProvideMirror(ctx, clientId)

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		for range 3 {
			secretKey, err := GetProvideSecretKey(observed, clientId, ProvideModeStream)
			if err != nil || !bytes.Equal(secretKey, streamKey) {
				t.Fatalf("stream secret after expiry = %x, %v", secretKey, err)
			}
		}
		provideModes, err := GetProvideModes(observed, clientId)
		if err != nil || len(provideModes) != 1 || !provideModes[ProvideModeStream] {
			t.Fatalf("modes after expiry = %v, %v", provideModes, err)
		}
		if reads := statements.take(); len(reads) != 2 {
			t.Fatalf("three secret lookups and a mode lookup after the mirror expired used %d statements, want 2 (the refill and its confirmation): %q", len(reads), reads)
		}
	})
}

// A destination without provide keys is mirrored as an empty mode list, so
// repeated lookups of it stop reading postgres too. A later provide replaces
// the empty list.
func TestProvideMirrorCachesNoProvideKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		for range 2 {
			provideModes, err := GetProvideModes(observed, clientId)
			if err != nil || len(provideModes) != 0 {
				t.Fatalf("modes without keys = %v, %v", provideModes, err)
			}
			if secretKey, err := GetProvideSecretKey(observed, clientId, ProvideModeStream); err == nil {
				t.Fatalf("a client without keys returned a stream secret %x", secretKey)
			}
		}
		if reads := statements.take(); len(reads) != 1 {
			t.Fatalf("two mode and secret lookups of a client without keys used %d statements, want 1: %q", len(reads), reads)
		}

		streamKey := bytes.Repeat([]byte{0x54}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: streamKey,
		})
		secretKey, err := GetProvideSecretKey(observed, clientId, ProvideModeStream)
		if err != nil || !bytes.Equal(secretKey, streamKey) {
			t.Fatalf("stream secret after provide = %x, %v", secretKey, err)
		}
		provideModes, err := GetProvideModes(observed, clientId)
		if err != nil || len(provideModes) != 1 || !provideModes[ProvideModeStream] {
			t.Fatalf("modes after provide = %v, %v", provideModes, err)
		}
		if reads := statements.take(); len(reads) != 0 {
			t.Fatalf("lookups after a provide used %d statements, want 0: %q", len(reads), reads)
		}
	})
}
