package model

// `SetProvide` writes the mirror after its postgres commit. When that write
// fails (redis unreachable, or the caller canceled after the commit), the
// mirror from before the provide used to stay for its ttl. For a new client
// that is the empty list a lookup cached before its first provide, so the
// client was mirrored as providing nothing for up to 72h while postgres had
// its keys: contract creation refused it and its own announce did not count
// it as a provider. These tests cancel the caller as the provide's commit
// completes, which skips the write after the commit, and check what the next
// lookups see, before and after the provide's hold expires. The last bounds
// the empty list for when redis also refused the hold.

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type cancelAfterPgCommitKey struct{}

// Cancels the marked context when a commit issued under it completes, after
// running `beforeCommit` (if set) as the commit starts. The commit runs on a
// context detached from the caller's cancellation but keeps its values, so
// only the work after the commit sees the cancellation. Safe for concurrent
// use.
type cancelAfterPgCommit struct {
	cancel       context.CancelFunc
	beforeCommit func()
	stateLock    sync.Mutex
	commitCount  int
}

func (self *cancelAfterPgCommit) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if marker, _ := ctx.Value(cancelAfterPgCommitKey{}).(*cancelAfterPgCommit); marker != self || data.SQL != "commit" {
		return ctx
	}
	if self.beforeCommit != nil {
		self.beforeCommit()
	}
	return ctx
}

func (self *cancelAfterPgCommit) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if marker, _ := ctx.Value(cancelAfterPgCommitKey{}).(*cancelAfterPgCommit); marker != self || data.Err != nil || data.CommandTag.String() != "COMMIT" {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.commitCount += 1
	}()
	self.cancel()
}

func (self *cancelAfterPgCommit) commits() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.commitCount
}

// Runs `SetProvide` with a caller that is canceled as its commit completes, and
// returns once it has raised or returned.
func setProvideCanceledAfterCommit(t testing.TB, ctx context.Context, clientId server.Id, secretKeys map[ProvideMode][]byte, beforeCommit func()) {
	t.Helper()
	callerCtx, callerCancel := context.WithCancel(ctx)
	defer callerCancel()
	canceler := &cancelAfterPgCommit{
		cancel:       callerCancel,
		beforeCommit: beforeCommit,
	}
	scope, err := server.NewTestPgQueryScope(ctx, canceler)
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		server.HandleError(func() {
			SetProvide(context.WithValue(callerCtx, cancelAfterPgCommitKey{}, canceler), clientId, secretKeys)
		})
	}()
	if commits := canceler.commits(); commits != 1 {
		t.Fatalf("the provide committed %d times, want 1", commits)
	}
	if storedSecretKeys := loadProvideSecretKeys(ctx, clientId); len(storedSecretKeys) != len(secretKeys) {
		t.Fatalf("postgres holds modes %v after the provide, want %d", storedSecretKeys, len(secretKeys))
	}
}

// Asserts that the lookups return exactly the provided keys.
func assertProvideLookupsReturn(t testing.TB, ctx context.Context, clientId server.Id, secretKeys map[ProvideMode][]byte) {
	t.Helper()
	provideModes, err := GetProvideModes(ctx, clientId)
	if err != nil || len(provideModes) != len(secretKeys) {
		t.Errorf("modes after the provide = %v, %v, want %d modes", provideModes, err, len(secretKeys))
	}
	for provideMode, secretKey := range secretKeys {
		if !provideModes[provideMode] {
			t.Errorf("mode %d is missing after the provide", provideMode)
		}
		storedSecretKey, err := GetProvideSecretKey(ctx, clientId, provideMode)
		if err != nil || !bytes.Equal(storedSecretKey, secretKey) {
			t.Errorf("mode %d secret after the provide = %x, %v, want %x", provideMode, storedSecretKey, err, secretKey)
		}
	}
}

// A lookup caches the empty list before a new client's first provide, and the
// provide's write after its commit fails.
func TestProvideMirrorFailedProvideWriteLeavesNoStaleEmptyList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		if provideModes, err := GetProvideModes(ctx, clientId); err != nil || len(provideModes) != 0 {
			t.Fatalf("modes before the first provide = %v, %v", provideModes, err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result(); provideModesListJson != "[]" {
				t.Fatalf("the lookup before the first provide mirrored %q, want []", provideModesListJson)
			}
		})

		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: bytes.Repeat([]byte{0x81}, 32),
			ProvideModeStream: bytes.Repeat([]byte{0x82}, 32),
		}
		setProvideCanceledAfterCommit(t, ctx, clientId, secretKeys, nil)

		assertProvideLookupsReturn(t, ctx, clientId, secretKeys)
	})
}

// After a provide's write fails, its hold keeps lookups on postgres without a
// fill until it expires, and the next lookup then mirrors the provided keys.
func TestProvideMirrorRefillsAfterFailedProvideHoldExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		publicKey := bytes.Repeat([]byte{0x84}, 32)
		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: publicKey,
		}
		setProvideCanceledAfterCommit(t, ctx, clientId, secretKeys, nil)

		assertProvideLookupsReturn(t, ctx, clientId, secretKeys)
		server.Redis(ctx, func(r server.RedisClient) {
			if exists := r.Exists(ctx, provideModesKey(clientId)).Val(); exists != 0 {
				t.Error("a lookup filled the mirror while the failed provide held it")
			}
			// the hold expires
			server.Raise(r.Del(ctx, provideMirrorHoldKey(clientId)).Err())
		})

		assertProvideLookupsReturn(t, ctx, clientId, secretKeys)
		server.Redis(ctx, func(r server.RedisClient) {
			if provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result(); provideModesListJson != "[3]" {
				t.Errorf("mirrored mode list after the hold = %q, want [3]", provideModesListJson)
			}
			secretKeyStr, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			if !bytes.Equal([]byte(secretKeyStr), publicKey) {
				t.Errorf("mirrored public secret after the hold = %x, want %x", secretKeyStr, publicKey)
			}
		})
	})
}

// An empty list is mirrored for at most `provideMirrorEmptyTtl`, whether a
// lookup or a provide of no keys wrote it. That bounds a stale empty list when
// redis was unavailable for both a provide's hold and its write.
func TestProvideMirrorEmptyListIsShortLived(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		lookedUpClientId := server.NewId()
		if provideModes, err := GetProvideModes(ctx, lookedUpClientId); err != nil || len(provideModes) != 0 {
			t.Fatalf("modes of a client without keys = %v, %v", provideModes, err)
		}
		providedClientId := server.NewId()
		SetProvide(ctx, providedClientId, map[ProvideMode][]byte{})

		server.Redis(ctx, func(r server.RedisClient) {
			for _, clientId := range []server.Id{lookedUpClientId, providedClientId} {
				if provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result(); provideModesListJson != "[]" && provideModesListJson != "null" {
					t.Errorf("mirrored mode list of a client without keys = %q", provideModesListJson)
				}
				if ttl := r.TTL(ctx, provideModesKey(clientId)).Val(); ttl <= 0 || provideMirrorEmptyTtl < ttl {
					t.Errorf("empty mode list ttl = %v, want within %v", ttl, provideMirrorEmptyTtl)
				}
			}
		})
	})
}

// A lookup inside the provide's transaction reads the keys from before the
// provide, and the provide's write after its commit fails.
func TestProvideMirrorRefillDuringFailedProvideLeavesNoStaleList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: bytes.Repeat([]byte{0x83}, 32),
		}
		lookedUp := false
		setProvideCanceledAfterCommit(t, ctx, clientId, secretKeys, func() {
			// a concurrent request, on a context the tracer does not mark
			provideModes, err := GetProvideModes(ctx, clientId)
			if err != nil || len(provideModes) != 0 {
				t.Errorf("modes read inside the provide's transaction = %v, %v", provideModes, err)
			}
			lookedUp = true
		})
		if !lookedUp {
			t.Fatal("the lookup inside the provide's transaction did not run")
		}

		assertProvideLookupsReturn(t, ctx, clientId, secretKeys)
	})
}
