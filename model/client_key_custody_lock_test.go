// Client-key custody serializes one client's key mutation on an advisory lock
// and holds the client row only FOR KEY SHARE: contracts to the provider share
// the row, while deactivation and deletion still wait for the mutation.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Waits until some backend waits for the client's custody lock.
func requireClientKeyCustodyWaiter(t testing.TB, ctx context.Context, clientId server.Id) {
	t.Helper()
	requireClientKeyCustodyLockState(t, ctx, clientId, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_locks, custody
			WHERE
				NOT granted AND
				locktype = 'advisory' AND
				objsubid = 1 AND
				classid::bigint = custody.high AND
				objid::bigint = custody.low
		)
	`, "a waiter for the client's custody lock")
}

// Waits until some backend waits behind the holder of the client's custody
// lock, on any lock.
func requireBlockedByClientKeyCustody(t testing.TB, ctx context.Context, clientId server.Id) {
	t.Helper()
	requireClientKeyCustodyLockState(t, ctx, clientId, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_locks waiting, pg_locks holding, custody
			WHERE
				NOT waiting.granted AND
				holding.granted AND
				holding.locktype = 'advisory' AND
				holding.objsubid = 1 AND
				holding.classid::bigint = custody.high AND
				holding.objid::bigint = custody.low AND
				holding.pid = ANY(pg_blocking_pids(waiting.pid))
		)
	`, "a backend blocked by the custody lock's holder")
}

func requireClientKeyCustodyLockState(t testing.TB, ctx context.Context, clientId server.Id, query string, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	key := fmt.Sprintf("provider-work-owner/%x", [16]byte(clientId))
	// an advisory lock on one bigint key shows its halves as classid and objid
	query = `
		WITH custody AS (
			SELECT
				(hashtextextended($1, 0) >> 32) & 4294967295 AS high,
				hashtextextended($1, 0) & 4294967295 AS low
		)
	` + query
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		found := false
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, query, key).Scan(&found))
		})
		if found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("observed no %s", want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("observed no %s: %v", want, ctx.Err())
		case <-ticker.C:
		}
	}
}

func clientKeyCustodyTestNetwork(t testing.TB, ctx context.Context, clientId server.Id) server.Id {
	t.Helper()
	var networkId server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM network_client WHERE client_id = $1`, clientId).Scan(&networkId))
	})
	return networkId
}

// A contract creation to the client, with a raised error as its error.
func createClientKeyCustodyTestContract(ctx context.Context, sourceNetworkId server.Id, sourceId server.Id, destinationNetworkId server.Id, destinationId server.Id) (contractId server.Id, createErr error) {
	server.HandleError(func() {
		contractId, createErr = CreateContractNoEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1024)
	}, func(err error) {
		createErr = err
	})
	return
}

// Installs the statement pauser for the test's pooled sessions, and returns
// what removes it. Remove it inside the test environment, before its teardown
// drops the database the traced pools still use. Removal first cancels the
// test context, which releases a statement a failed test left paused.
func clientKeyCustodyTestScope(t testing.TB, ctx context.Context, cancel context.CancelFunc) func() {
	t.Helper()
	scope, err := server.NewTestPgQueryScope(ctx, provideStatementPauser{})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		cancel()
		if err := scope.Close(); err != nil {
			t.Error(err)
		}
	}
}

// Starts a key registration that pauses at its history insert, holding every
// lock it takes.
func startPausedClientKeyRegistration(t testing.TB, ctx context.Context, input StClientKeyRegistrationInput) (*provideStatementPause, <-chan error) {
	t.Helper()
	pause := newProvideStatementPause("INSERT INTO st_client_key_history")
	done := runProvideRetirementCall(func() {
		_, err := StoreStClientKeyRegistration(pause.context(ctx), input)
		server.Raise(err)
	})
	awaitProvideStatementPause(t, ctx, pause, done)
	return pause, done
}

// A key registration used to lock its client FOR UPDATE for the whole
// transaction, so contract creations to that provider queued behind it until
// it committed or their 2 s lock bound refused them.
func TestClientKeyRegistrationDoesNotHoldContractsToItsClient(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		input := newStClientKeyHistoryTestInput(t)
		providerNetworkId := clientKeyCustodyTestNetwork(t, ctx, input.ClientID)
		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{sourceId: sourceNetworkId})
		defer clientKeyCustodyTestScope(t, ctx, cancel)()

		pause, registered := startPausedClientKeyRegistration(t, ctx, input)
		contractId, err := createClientKeyCustodyTestContract(ctx, sourceNetworkId, sourceId, providerNetworkId, input.ClientID)
		if err != nil || contractId == (server.Id{}) {
			t.Fatalf("contract to a client mid-registration: id=%s err=%v", contractId, err)
		}
		close(pause.resume)
		if err := <-registered; err != nil {
			t.Fatal(err)
		}
	})
}

// Work owner retention locked its client FOR UPDATE the same way.
func TestProviderWorkOwnerRetentionDoesNotHoldContractsToItsClient(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		input, owner, _, raw := providerWorkOwnerFixture(t)
		if _, err := StoreStClientKeyRegistration(ctx, input); err != nil {
			t.Fatal(err)
		}
		providerNetworkId := clientKeyCustodyTestNetwork(t, ctx, input.ClientID)
		sourceNetworkId := server.NewId()
		sourceId := server.NewId()
		insertContractLifecycleTestClients(t, ctx, map[server.Id]server.Id{sourceId: sourceNetworkId})
		defer clientKeyCustodyTestScope(t, ctx, cancel)()

		pause := newProvideStatementPause("INSERT INTO provider_work_owner")
		retained := runProvideRetirementCall(func() {
			_, err := RetainProviderWorkOwner(pause.context(ctx), raw, owner.DomainHash, crypto.PubkeyToAddress(input.RootKey.PublicKey))
			server.Raise(err)
		})
		awaitProvideStatementPause(t, ctx, pause, retained)
		contractId, err := createClientKeyCustodyTestContract(ctx, sourceNetworkId, sourceId, providerNetworkId, input.ClientID)
		if err != nil || contractId == (server.Id{}) {
			t.Fatalf("contract to a client mid-retention: id=%s err=%v", contractId, err)
		}
		close(pause.resume)
		if err := <-retained; err != nil {
			t.Fatal(err)
		}
	})
}

// With the client row shared, the custody lock alone keeps one client's first
// insertions and rotations in order: a second registration of the client waits
// on it before it reads the head, and another client's registration does not.
func TestClientKeyCustodySerializesRegistrationsPerClient(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		input := newStClientKeyHistoryTestInput(t)
		other := newStClientKeyHistoryTestInput(t)
		rotation := input
		rotation.PublicKey = bytes.Repeat([]byte{7}, 32)
		defer clientKeyCustodyTestScope(t, ctx, cancel)()

		// the first insertion holds the custody lock with no head written yet
		pause, first := startPausedClientKeyRegistration(t, ctx, input)
		var rotated *StClientKeyHistoryRecord
		second := runProvideRetirementCall(func() {
			var err error
			rotated, err = StoreStClientKeyRegistration(ctx, rotation)
			server.Raise(err)
		})
		requireClientKeyCustodyWaiter(t, ctx, input.ClientID)
		select {
		case err := <-second:
			t.Fatalf("second registration of the client crossed the custody lock: %v", err)
		default:
		}
		if record, err := StoreStClientKeyRegistration(ctx, other); err != nil || record == nil || record.Registration.Generation != 1 {
			t.Fatalf("another client's registration behind the custody lock: %v", err)
		}

		close(pause.resume)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
		if rotated == nil || rotated.Registration.Generation != 2 || !bytes.Equal(rotated.Registration.PublicKey[:], rotation.PublicKey) {
			t.Fatalf("second registration = %+v, want the rotation to generation 2", rotated)
		}
	})
}

// Retention waits on the same custody lock as the key registration it reads.
func TestClientKeyCustodySerializesRetentionWithRegistration(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		input, owner, _, raw := providerWorkOwnerFixture(t)
		defer clientKeyCustodyTestScope(t, ctx, cancel)()

		pause, registered := startPausedClientKeyRegistration(t, ctx, input)
		retained := runProvideRetirementCall(func() {
			_, err := RetainProviderWorkOwner(ctx, raw, owner.DomainHash, crypto.PubkeyToAddress(input.RootKey.PublicKey))
			server.Raise(err)
		})
		requireClientKeyCustodyWaiter(t, ctx, input.ClientID)
		select {
		case err := <-retained:
			t.Fatalf("retention crossed the registration's custody lock: %v", err)
		default:
		}

		close(pause.resume)
		if err := <-registered; err != nil {
			t.Fatal(err)
		}
		// it reads the registration that committed while it waited
		if err := <-retained; err != nil {
			t.Fatal(err)
		}
	})
}

// The client row's key-share lock still holds deactivation and DELETE until
// the key mutation commits, and the DELETE's trigger then retires the head
// that mutation wrote.
func TestClientKeyCustodyHoldsItsClientAgainstDeactivationAndDeletion(t *testing.T) {
	for _, lifecycle := range []string{"deactivation", "deletion"} {
		t.Run(lifecycle, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				input := newStClientKeyHistoryTestInput(t)
				networkId := clientKeyCustodyTestNetwork(t, ctx, input.ClientID)
				defer clientKeyCustodyTestScope(t, ctx, cancel)()

				pause, registered := startPausedClientKeyRegistration(t, ctx, input)
				var rowCount int64
				ended := runProvideRetirementCall(func() {
					server.Tx(ctx, func(tx server.PgTx) {
						if lifecycle == "deactivation" {
							count, err := deactivateNetworkClientsInTx(ctx, tx, []server.Id{input.ClientID}, networkId)
							server.Raise(err)
							rowCount = count
						} else {
							rowCount = server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id = $1`, input.ClientID)).RowsAffected()
						}
					}, server.TxReadCommitted, server.TxLockTimeout(15*time.Second))
				})
				requireBlockedByClientKeyCustody(t, ctx, input.ClientID)
				select {
				case err := <-ended:
					t.Fatalf("%s crossed the client's key mutation: %v", lifecycle, err)
				default:
				}

				close(pause.resume)
				if err := <-registered; err != nil {
					t.Fatal(err)
				}
				if err := <-ended; err != nil {
					t.Fatal(err)
				}
				if rowCount != 1 {
					t.Fatalf("%s rows = %d, want 1", lifecycle, rowCount)
				}
				if lifecycle != "deletion" {
					return
				}
				var retired bool
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT retired FROM st_client_key_head WHERE client_id = $1 AND is_current`, input.ClientID).Scan(&retired))
				})
				if !retired {
					t.Fatal("deletion after the registration left its head current")
				}
				if _, err := StoreStClientKeyRegistration(ctx, input); !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("a deleted client's registration = %v, want no active client row", err)
				}
			})
		})
	}
}
