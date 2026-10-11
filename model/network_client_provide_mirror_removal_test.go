package model

// A read that misses the provide mirror refills it from a postgres snapshot.
// Every removal (client reap, orphan sweep, prober shard cleanup) commits the
// delete of the client's keys and then deletes the mirror, so a refill whose
// snapshot predates the commit but whose write lands after the mirror delete
// put the removed client's modes and secrets back until the mirror ttl
// (72h). These tests force that order directly: the snapshot is taken, the
// removal commits and clears the mirror, then the snapshot is offered to the
// refill. A fill now keeps itself only when a second read confirms its
// snapshot, so the last tests cover a retried, an unconfirmed and a current
// fill.

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type cancelAtPgStatementKey struct{}

// Cancels the marked context when a statement is issued under it, after
// recording the statement. Safe for concurrent use.
type cancelAtPgStatement struct {
	cancel     context.CancelFunc
	stateLock  sync.Mutex
	statements []string
}

func (self *cancelAtPgStatement) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if marker, _ := ctx.Value(cancelAtPgStatementKey{}).(*cancelAtPgStatement); marker != self {
		return ctx
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.statements = append(self.statements, strings.Join(strings.Fields(data.SQL), " "))
	}()
	self.cancel()
	return ctx
}

func (self *cancelAtPgStatement) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Returns the statements recorded since the last take.
func (self *cancelAtPgStatement) take() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	statements := self.statements
	self.statements = nil
	return statements
}

// Takes a lookup's snapshot of the client's keys after its mirror expired, runs
// the removal, and then offers the snapshot to the refill.
func refillProvideMirrorAcrossRemoval(t testing.TB, ctx context.Context, clientId server.Id, remove func()) {
	t.Helper()
	Testing_DeleteProvideMirror(ctx, clientId)
	staleSecretKeys := loadProvideSecretKeys(ctx, clientId)
	if len(staleSecretKeys) == 0 {
		t.Fatal("the lookup's snapshot has no keys")
	}

	remove()

	if secretKeys := loadProvideSecretKeys(ctx, clientId); len(secretKeys) != 0 {
		t.Fatalf("the removal left provide keys for modes %v", secretKeys)
	}
	server.Redis(ctx, func(r server.RedisClient) {
		if exists := r.Exists(ctx, provideModesKey(clientId)).Val(); exists != 0 {
			t.Fatal("the removal left a mirrored mode list")
		}
	})

	refillProvideMirror(ctx, clientId, staleSecretKeys)
}

// Asserts that nothing of the removed client is mirrored or returned. Redis is
// read first, since a lookup that misses refills the mirror.
func assertProvideMirrorRemoved(t testing.TB, ctx context.Context, clientId server.Id) {
	t.Helper()
	allProvideModes := []ProvideMode{
		ProvideModeDefault,
		ProvideModeNone,
		ProvideModeNetwork,
		ProvideModeFriendsAndFamily,
		ProvideModePublic,
		ProvideModeStream,
	}
	server.Redis(ctx, func(r server.RedisClient) {
		provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result()
		if provideModesListJson != "" && provideModesListJson != "[]" {
			t.Errorf("the removed client's mirrored mode list = %s", provideModesListJson)
		}
		for _, provideMode := range allProvideModes {
			if exists := r.Exists(ctx, provideModeSecretKeyKey(clientId, provideMode)).Val(); exists != 0 {
				t.Errorf("the removed client's mode %d secret is mirrored", provideMode)
			}
		}
	})
	provideModes, err := GetProvideModes(ctx, clientId)
	if err != nil || len(provideModes) != 0 {
		t.Errorf("modes of the removed client = %v, %v", provideModes, err)
	}
	for _, provideMode := range allProvideModes {
		if secretKey, err := GetProvideSecretKey(ctx, clientId, provideMode); err == nil {
			t.Errorf("the removed client returned a mode %d secret %x", provideMode, secretKey)
		}
	}
}

// The reap deletes an inactive client between a lookup's read and its refill.
func TestProvideMirrorRefillNeverRestoresReapedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: bytes.Repeat([]byte{0x71}, 32),
			ProvideModeStream: bytes.Repeat([]byte{0x72}, 32),
		})

		refillProvideMirrorAcrossRemoval(t, ctx, clientId, func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					UPDATE network_client
					SET active = false
					WHERE client_id = $1
					`,
					clientId,
				))
			})
			RemoveDisconnectedNetworkClients(ctx, server.NowUtc().Add(time.Minute), server.NowUtc().Add(time.Minute), time.Time{})
		})

		assertProvideMirrorRemoved(t, ctx, clientId)
	})
}

// The orphan sweep deletes keys without a client between a lookup's read and
// its refill.
func TestProvideMirrorRefillNeverRestoresSweptOrphan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// provide keys without a network_client row
		clientId := server.NewId()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: bytes.Repeat([]byte{0x73}, 32),
		})

		refillProvideMirrorAcrossRemoval(t, ctx, clientId, func() {
			SweepOrphanNetworkClientData(ctx, 1000)
		})

		assertProvideMirrorRemoved(t, ctx, clientId)
	})
}

// The prober shard cleanup deletes a drained shard's client between a lookup's
// read and its refill.
func TestProvideMirrorRefillNeverRestoresRetiredProberShardClient(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := shardTestOwner(t, ctx, shardTestKey(0))
		SetProvide(ctx, owner.ClientId, map[ProvideMode][]byte{
			ProvideModeStream: bytes.Repeat([]byte{0x74}, 32),
		})

		refillProvideMirrorAcrossRemoval(t, ctx, owner.ClientId, func() {
			if err := DrainProberShard(ctx, owner.Key); err != nil {
				t.Fatal(err)
			}
			if deleted, err := ReapProberShard(ctx, owner.Key); err != nil || !deleted {
				t.Fatal("the drained shard was not deleted", err)
			}
		})

		assertProvideMirrorRemoved(t, ctx, owner.ClientId)
	})
}

// The redis client retries a refill whose reply was lost, and the retry finds
// the list its first attempt wrote after the orphan sweep removed the keys.
// The retried fill is confirmed like one that wrote, so the sweep still wins.
func TestProvideMirrorRefillConfirmsRetriedFill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// provide keys without a network_client row
		clientId := server.NewId()
		publicKey := bytes.Repeat([]byte{0x77}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: publicKey,
		})
		Testing_DeleteProvideMirror(ctx, clientId)
		staleSecretKeys := loadProvideSecretKeys(ctx, clientId)
		SweepOrphanNetworkClientData(ctx, 1000)

		// the first attempt's write, whose reply was lost
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, provideModesKey(clientId), "[3]", provideMirrorTtl).Err())
			server.Raise(r.Set(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic), publicKey, provideMirrorTtl).Err())
		})
		refillProvideMirror(ctx, clientId, staleSecretKeys)

		assertProvideMirrorRemoved(t, ctx, clientId)
	})
}

// The caller is canceled after the fill lands, as the read that would confirm
// it starts. A fill that cannot be confirmed is taken back, so the next read
// refills instead of trusting it until the ttl.
func TestProvideMirrorRefillTakesBackUnconfirmedFill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: bytes.Repeat([]byte{0x76}, 32),
		})
		Testing_DeleteProvideMirror(ctx, clientId)
		secretKeys := loadProvideSecretKeys(ctx, clientId)

		callerCtx, callerCancel := context.WithCancel(ctx)
		defer callerCancel()
		canceler := &cancelAtPgStatement{
			cancel: callerCancel,
		}
		scope, err := server.NewTestPgQueryScope(ctx, canceler)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()

		refillProvideMirror(context.WithValue(callerCtx, cancelAtPgStatementKey{}, canceler), clientId, secretKeys)

		if statements := canceler.take(); len(statements) != 1 {
			t.Fatalf("the fill issued %d statements, want 1 (its confirmation): %q", len(statements), statements)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			if n := r.Exists(ctx, provideModesKey(clientId), provideModeSecretKeyKey(clientId, ProvideModePublic)).Val(); n != 0 {
				t.Errorf("the unconfirmed fill left %d mirror keys", n)
			}
		})
	})
}

// A refill whose snapshot is still current keeps its fill, so the next lookups
// are served by redis.
func TestProvideMirrorRefillKeepsCurrentSnapshot(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		publicKey := bytes.Repeat([]byte{0x75}, 32)
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: publicKey,
		})
		Testing_DeleteProvideMirror(ctx, clientId)

		refillProvideMirror(ctx, clientId, loadProvideSecretKeys(ctx, clientId))

		server.Redis(ctx, func(r server.RedisClient) {
			provideModesListJson, _ := r.Get(ctx, provideModesKey(clientId)).Result()
			if provideModesListJson != "[3]" {
				t.Errorf("mirrored mode list = %q, want [3]", provideModesListJson)
			}
			secretKeyStr, _ := r.Get(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic)).Result()
			if !bytes.Equal([]byte(secretKeyStr), publicKey) {
				t.Errorf("mirrored public secret = %x, want %x", secretKeyStr, publicKey)
			}
		})
	})
}
