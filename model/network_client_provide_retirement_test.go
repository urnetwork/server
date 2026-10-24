package model

// SetProvide writes only the difference between the stored keys and the
// provided set, and a deactivation deletes a client's keys and their mirror.
// The concurrency tests order the transactions with statement pauses and
// observed lock waits, never with sleeps.
//
// DB-backed: under the owner rule these run only after the branch, with its
// migrations, is merged to main.

import (
	"bytes"
	"context"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type provideStatementPauseKey struct{}

// Pauses the statement of a context that carries this pause, at the start of
// the first statement whose SQL holds the needle, until resumed.
type provideStatementPause struct {
	needle  string
	reached chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func newProvideStatementPause(needle string) *provideStatementPause {
	return &provideStatementPause{
		needle:  needle,
		reached: make(chan struct{}),
		resume:  make(chan struct{}),
	}
}

func (self *provideStatementPause) context(ctx context.Context) context.Context {
	return context.WithValue(ctx, provideStatementPauseKey{}, self)
}

// Dispatches each statement to the pause its context carries. Safe for
// concurrent use.
type provideStatementPauser struct{}

func (provideStatementPauser) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	pause, _ := ctx.Value(provideStatementPauseKey{}).(*provideStatementPause)
	if pause == nil || !strings.Contains(data.SQL, pause.needle) {
		return ctx
	}
	pause.once.Do(func() {
		close(pause.reached)
		select {
		case <-pause.resume:
		case <-ctx.Done():
		}
	})
	return ctx
}

func (provideStatementPauser) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func awaitProvideStatementPause(t testing.TB, ctx context.Context, pause *provideStatementPause, done <-chan error) {
	t.Helper()
	select {
	case <-pause.reached:
	case err := <-done:
		t.Fatalf("the paused call returned before %q: %v", pause.needle, err)
	case <-ctx.Done():
		t.Fatalf("the paused call never reached %q", pause.needle)
	}
}

// Runs fn in its own goroutine and reports what it raised.
func runProvideRetirementCall(fn func()) <-chan error {
	done := make(chan error, 1)
	go func() {
		var err error
		server.HandleError(fn, func(value error) { err = value })
		done <- err
	}()
	return done
}

// Each stored key's row version, its xmin and tuple address: an untouched row
// keeps both, and any rewrite changes them.
func provideKeyRowVersions(t testing.TB, ctx context.Context, clientId server.Id) map[ProvideMode]string {
	t.Helper()
	versions := map[ProvideMode]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT provide_mode, xmin::text || '/' || ctid::text FROM provide_key WHERE client_id = $1`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var provideMode ProvideMode
				var version string
				server.Raise(result.Scan(&provideMode, &version))
				versions[provideMode] = version
			}
		})
	})
	return versions
}

func provideKeyChangeCount(t testing.TB, ctx context.Context, clientId server.Id) int {
	t.Helper()
	var count int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provide_key_change WHERE client_id = $1`, clientId).Scan(&count))
	})
	return count
}

func assertProvideSecretKeys(t testing.TB, ctx context.Context, clientId server.Id, want map[ProvideMode][]byte) {
	t.Helper()
	if stored := loadProvideSecretKeys(ctx, clientId); !maps.EqualFunc(stored, want, bytes.Equal) {
		t.Fatalf("stored keys = %v, want %v", stored, want)
	}
	provideModes, err := GetProvideModes(ctx, clientId)
	if err != nil || len(provideModes) != len(want) {
		t.Fatalf("mirrored modes = %v, %v, want %d", provideModes, err, len(want))
	}
	for provideMode, secretKey := range want {
		mirrored, err := GetProvideSecretKey(ctx, clientId, provideMode)
		if err != nil || !bytes.Equal(mirrored, secretKey) {
			t.Fatalf("mirrored mode %d secret = %x, %v, want %x", provideMode, mirrored, err, secretKey)
		}
	}
}

func provideRetirementTestKey(fill byte) []byte {
	return bytes.Repeat([]byte{fill}, 32)
}

// A repeated provide of the same set leaves every provide_key row untouched,
// and still records the provide for the announce loop.
func TestSetProvideUnchangedSetWritesNoProvideKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "test", "test")
		secretKeys := map[ProvideMode][]byte{
			ProvideModePublic: provideRetirementTestKey(0x11),
			ProvideModeStream: provideRetirementTestKey(0x12),
		}
		SetProvide(ctx, clientId, secretKeys)
		before := provideKeyRowVersions(t, ctx, clientId)
		if len(before) != 2 {
			t.Fatalf("first provide stored %d keys, want 2", len(before))
		}

		SetProvide(ctx, clientId, maps.Clone(secretKeys))
		if after := provideKeyRowVersions(t, ctx, clientId); !maps.Equal(before, after) {
			t.Fatalf("an unchanged provide rewrote provide_key rows: %v became %v", before, after)
		}
		assertProvideSecretKeys(t, ctx, clientId, secretKeys)
		if count := provideKeyChangeCount(t, ctx, clientId); count != 2 {
			t.Fatalf("provide changes recorded = %d, want one per provide", count)
		}
	})
}

// A changed set writes only its difference: an unchanged mode keeps its row,
// a rotated secret is rewritten, a dropped mode is deleted and a new one
// inserted.
func TestSetProvideWritesOnlyTheDifference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "test", "test")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeNetwork: provideRetirementTestKey(0x21),
			ProvideModePublic:  provideRetirementTestKey(0x22),
			ProvideModeStream:  provideRetirementTestKey(0x23),
		})
		before := provideKeyRowVersions(t, ctx, clientId)

		changed := map[ProvideMode][]byte{
			ProvideModeNetwork:          provideRetirementTestKey(0x21),
			ProvideModePublic:           provideRetirementTestKey(0x24),
			ProvideModeFriendsAndFamily: provideRetirementTestKey(0x25),
		}
		SetProvide(ctx, clientId, changed)
		after := provideKeyRowVersions(t, ctx, clientId)
		if after[ProvideModeNetwork] != before[ProvideModeNetwork] {
			t.Fatal("an unchanged mode was rewritten")
		}
		if after[ProvideModePublic] == before[ProvideModePublic] {
			t.Fatal("a rotated secret kept its old row")
		}
		if _, ok := after[ProvideModeStream]; ok {
			t.Fatal("a dropped mode kept its row")
		}
		assertProvideSecretKeys(t, ctx, clientId, changed)
		server.Redis(ctx, func(r server.RedisClient) {
			if exists := r.Exists(ctx, provideModeSecretKeyKey(clientId, ProvideModeStream)).Val(); exists != 0 {
				t.Fatal("a dropped mode's secret is still mirrored")
			}
		})
	})
}

// Two provides of one client with different sets: the second waits on the
// first's advisory lock, then reads the first's committed keys and writes only
// its difference. Both converge on the set committed last, in postgres and in
// the mirror.
func TestSetProvideConcurrentCallsConverge(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "test", "test")
		first := map[ProvideMode][]byte{
			ProvideModeNetwork: provideRetirementTestKey(0x31),
			ProvideModePublic:  provideRetirementTestKey(0x32),
		}
		second := map[ProvideMode][]byte{
			ProvideModeNetwork: provideRetirementTestKey(0x31),
			ProvideModeStream:  provideRetirementTestKey(0x33),
		}

		scope, err := server.NewTestPgQueryScope(ctx, provideStatementPauser{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()

		// each pauses holding the advisory lock, at its read of the stored keys
		firstPause := newProvideStatementPause("SELECT provide_mode, secret_key FROM provide_key")
		secondPause := newProvideStatementPause("SELECT provide_mode, secret_key FROM provide_key")
		firstDone := runProvideRetirementCall(func() {
			SetProvide(firstPause.context(ctx), clientId, first)
		})
		awaitProvideStatementPause(t, ctx, firstPause, firstDone)
		secondDone := runProvideRetirementCall(func() {
			SetProvide(secondPause.context(ctx), clientId, second)
		})
		if err := waitForPaymentLockWaiter(ctx, "provide_key/"); err != nil {
			t.Fatal(err)
		}

		close(firstPause.resume)
		if err := <-firstDone; err != nil {
			t.Fatal(err)
		}
		firstVersions := provideKeyRowVersions(t, ctx, clientId)
		assertProvideSecretKeys(t, ctx, clientId, first)

		awaitProvideStatementPause(t, ctx, secondPause, secondDone)
		close(secondPause.resume)
		if err := <-secondDone; err != nil {
			t.Fatal(err)
		}
		secondVersions := provideKeyRowVersions(t, ctx, clientId)
		if secondVersions[ProvideModeNetwork] != firstVersions[ProvideModeNetwork] {
			t.Fatal("the second provide rewrote the mode both sets share")
		}
		assertProvideSecretKeys(t, ctx, clientId, second)
	})
}

// A deactivation deletes the client's keys and, after its commit, their
// mirror, on the network and on the owned path.
func TestDeactivationDeletesProvideKeysAndMirror(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		parentId, childId, keptId := server.NewId(), server.NewId(), server.NewId()
		parentDeviceId := server.NewId()
		Testing_CreateDevice(ctx, networkId, parentDeviceId, parentId, "test", "test")
		Testing_CreateDevice(ctx, networkId, server.NewId(), keptId, "test", "test")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id)
				VALUES ($1, $2, $3, 'test', now(), now(), $4)
				`,
				childId,
				networkId,
				parentDeviceId,
				parentId,
			))
		})
		for _, clientId := range []server.Id{parentId, childId, keptId} {
			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: provideRetirementTestKey(0x41),
				ProvideModeStream: provideRetirementTestKey(0x42),
			})
		}

		// the parent's client token removes its child
		owned, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: childId}, &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId, ClientId: &parentId},
		})
		if err != nil || owned.Error != nil {
			t.Fatal("owned removal failed", err, owned.Error)
		}
		// the network session removes the parent
		network, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: parentId}, &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId},
		})
		if err != nil || network.Error != nil {
			t.Fatal("network removal failed", err, network.Error)
		}

		for _, clientId := range []server.Id{parentId, childId} {
			assertProvideMirrorRemoved(t, ctx, clientId)
			if secretKeys := loadProvideSecretKeys(ctx, clientId); len(secretKeys) != 0 {
				t.Fatalf("a deactivated client kept provide keys %v", secretKeys)
			}
		}
		assertProvideSecretKeys(t, ctx, keptId, map[ProvideMode][]byte{
			ProvideModePublic: provideRetirementTestKey(0x41),
			ProvideModeStream: provideRetirementTestKey(0x42),
		})
	})
}

// The abandoned top-level marker is a deactivation too: its keys go with it.
func TestAbandonedTopLevelMarkDeletesProvideKeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, server.NewId(), server.NewId(), clientId, "test", "test")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: provideRetirementTestKey(0x51),
		})
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_client SET auth_time = $2 WHERE client_id = $1`,
				clientId,
				now.Add(-TopLevelClientIdleExpiration-time.Hour),
			))
		})

		RemoveDisconnectedNetworkClients(ctx, now.Add(-8*time.Hour), now.Add(-NetworkClientReapAfterDeactivate), now.Add(-TopLevelClientIdleExpiration))

		var active *bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId).Scan(&active))
		})
		if active == nil || *active {
			t.Fatal("the abandoned client was not marked inactive, or was reaped")
		}
		assertProvideMirrorRemoved(t, ctx, clientId)
	})
}

// A provide of an inactive client writes nothing: no key, no change record
// and no mirror.
func TestSetProvideRefusedForInactiveClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: provideRetirementTestKey(0x61),
		})
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId},
		})
		if err != nil || removed.Error != nil {
			t.Fatal("removal failed", err, removed.Error)
		}
		changeCount := provideKeyChangeCount(t, ctx, clientId)
		refusals := testutil.ToFloat64(provideInactiveClientRefusals)

		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModePublic: provideRetirementTestKey(0x61),
		})

		assertProvideMirrorRemoved(t, ctx, clientId)
		if count := provideKeyChangeCount(t, ctx, clientId); count != changeCount {
			t.Fatalf("a refused provide recorded a change: %d became %d", changeCount, count)
		}
		if after := testutil.ToFloat64(provideInactiveClientRefusals); after != refusals+1 {
			t.Fatalf("refusals counted %v, want %v", after, refusals+1)
		}
	})
}

// A provide that waits behind a deactivation's row lock finds the client
// inactive once the deactivation commits, and writes nothing.
func TestSetProvideWaitingOnDeactivationWritesNothing(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: provideRetirementTestKey(0x71),
		})

		scope, err := server.NewTestPgQueryScope(ctx, provideStatementPauser{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()

		// the deactivation pauses holding the client row FOR UPDATE
		deactivationPause := newProvideStatementPause("lifecycle_clock")
		deactivationDone := runProvideRetirementCall(func() {
			removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{
				Ctx:   deactivationPause.context(ctx),
				ByJwt: &session.ByJwt{NetworkId: networkId},
			})
			server.Raise(err)
			if removed.Error != nil {
				panic(removed.Error.Message)
			}
		})
		awaitProvideStatementPause(t, ctx, deactivationPause, deactivationDone)

		provideDone := runProvideRetirementCall(func() {
			SetProvide(ctx, clientId, map[ProvideMode][]byte{
				ProvideModePublic: provideRetirementTestKey(0x72),
			})
		})
		if err := waitForPaymentLockWaiter(ctx, "FOR KEY SHARE"); err != nil {
			t.Fatal(err)
		}
		close(deactivationPause.resume)
		if err := <-deactivationDone; err != nil {
			t.Fatal(err)
		}
		if err := <-provideDone; err != nil {
			t.Fatal(err)
		}
		assertProvideMirrorRemoved(t, ctx, clientId)
	})
}

// A deactivation that waits behind a provide's row lock deletes the keys that
// provide committed, and then their mirror.
func TestDeactivationWaitingOnProvideDeletesItsKeys(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		SetProvide(ctx, clientId, map[ProvideMode][]byte{
			ProvideModeStream: provideRetirementTestKey(0x81),
		})

		scope, err := server.NewTestPgQueryScope(ctx, provideStatementPauser{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()

		// the provide pauses holding the client row FOR KEY SHARE, at the
		// delete of its dropped mode
		providePause := newProvideStatementPause("provide_mode = ANY($2::int[])")
		provideDone := runProvideRetirementCall(func() {
			SetProvide(providePause.context(ctx), clientId, map[ProvideMode][]byte{
				ProvideModePublic: provideRetirementTestKey(0x82),
			})
		})
		awaitProvideStatementPause(t, ctx, providePause, provideDone)

		// the deactivation then pauses holding the row, until the provide has
		// also written its mirror
		deactivationPause := newProvideStatementPause("lifecycle_clock")
		deactivationDone := runProvideRetirementCall(func() {
			removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{
				Ctx:   deactivationPause.context(ctx),
				ByJwt: &session.ByJwt{NetworkId: networkId},
			})
			server.Raise(err)
			if removed.Error != nil {
				panic(removed.Error.Message)
			}
		})
		if err := waitForPaymentLockWaiter(ctx, "network_client_deactivation_write_boundary"); err != nil {
			t.Fatal(err)
		}
		close(providePause.resume)
		if err := <-provideDone; err != nil {
			t.Fatal(err)
		}
		awaitProvideStatementPause(t, ctx, deactivationPause, deactivationDone)
		close(deactivationPause.resume)
		if err := <-deactivationDone; err != nil {
			t.Fatal(err)
		}
		assertProvideMirrorRemoved(t, ctx, clientId)
	})
}

// The hard delete removes the whole provide mirror and the roles of a reaped
// client, even when a lost post left the mirror behind its deactivation.
func TestReapedClientRemovesMirrorAndRoles(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "test", "test")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_role (client_id, role) VALUES ($1, 'synthetic-role')`, clientId))
		})
		removed, err := RemoveNetworkClient(&RemoveNetworkClientArgs{ClientId: clientId}, &session.ClientSession{
			Ctx:   ctx,
			ByJwt: &session.ByJwt{NetworkId: networkId},
		})
		if err != nil || removed.Error != nil {
			t.Fatal("removal failed", err, removed.Error)
		}
		// what a lost post leaves
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, provideModesKey(clientId), "[3]", provideMirrorTtl).Err())
			server.Raise(r.Set(ctx, provideModeSecretKeyKey(clientId, ProvideModePublic), provideRetirementTestKey(0x91), provideMirrorTtl).Err())
		})

		now := server.NowUtc()
		RemoveDisconnectedNetworkClients(ctx, now, now.Add(time.Minute), time.Time{})

		if GetNetworkClientNetwork(ctx, clientId) != nil {
			t.Fatal("the inactive client was not reaped")
		}
		assertProvideMirrorRemoved(t, ctx, clientId)
		server.Db(ctx, func(conn server.PgConn) {
			var roleCount int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client_role WHERE client_id = $1`, clientId).Scan(&roleCount))
			if roleCount != 0 {
				t.Fatalf("the reaped client kept %d roles", roleCount)
			}
		})
	})
}
