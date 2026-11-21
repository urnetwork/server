package oauth

// Against the database: a credential rotation ends the user's grants
// (grant_revoke.go), including the ones its revocation cannot reach.
//
// The codes and refresh tokens are redeemed and rotated as the token endpoint
// does it, without minting access tokens (Testing_Grant).

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func testOpenRefreshTokenCount(t testing.TB, ctx context.Context, clientId string) int {
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM oauth_refresh_token WHERE client_id = $1 AND revoke_time IS NULL`,
			clientId,
		).Scan(&count))
	})
	return count
}

func testGrant(t testing.TB, ctx context.Context, user *AuthorizingUser) *Testing_Grant {
	grant, err := Testing_CreateGrant(ctx, user.UserId, user.NetworkId, user.CredentialTime)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, grant.Testing_Refresh(ctx), nil)
	return grant
}

// A grant approved before the user's credential cutoff is refused after it,
// although nothing revoked it: a refresh token or a code that a refresh or an
// exchange wrote across the rotation, after its revocation ran. Before, a
// refresh token lived 90 days past a password reset, sliding with each use.
func TestGrantApprovedBeforeACredentialRotationIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		user := testAuthorizingUser(t, ctx)
		grant := testGrant(t, ctx, user)

		// the cutoff moves past the approval; nothing is revoked
		testMoveCredentialCutoff(t, ctx, user.UserId, server.NowUtc().Add(time.Second))

		connect.AssertEqual(t, grant.Testing_Refresh(ctx) != nil, true)
		// and the family is revoked, not only refused
		connect.AssertEqual(t, testOpenRefreshTokenCount(t, ctx, grant.ClientId), 0)

		connect.AssertEqual(t, grant.Testing_RedeemPendingCode(ctx) != nil, true)
	})
}

// An approval by a sign-in older than the user's credential cutoff is refused
// and grants nothing; a sign-in after the cutoff approves.
func TestApprovalByARetiredSignInIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeMcpRead})

		cutoff := user.CredentialTime.Add(time.Second)
		testMoveCredentialCutoff(t, ctx, user.UserId, cutoff)

		_, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, errors.Is(err, errUnauthorizedSignInAgain), true)
		connect.AssertEqual(t, GetConsent(ctx, user.UserId, client.ClientId) == nil, true)

		// a user unknown to the database approves nothing either
		unknown := *user
		unknown.UserId = server.NewId()
		unknown.CredentialTime = cutoff
		_, err = Authorize(ctx, request, &unknown)
		connect.AssertEqual(t, errors.Is(err, errUnauthorizedSignInAgain), true)

		user.CredentialTime = cutoff
		_, err = Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ConsentSatisfies(ctx, user.UserId, client.ClientId, []string{ScopeMcpRead}), true)
	})
}

// The rotation's revocation ends every grant of the user and of no one else.
func TestRevokeUserGrantsEndsOnlyThatUsersGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		user := testAuthorizingUser(t, ctx)
		other := testAuthorizingUser(t, ctx)
		grants := []*Testing_Grant{testGrant(t, ctx, user), testGrant(t, ctx, user)}
		otherGrant := testGrant(t, ctx, other)

		var revoked int64
		server.Tx(ctx, func(tx server.PgTx) {
			revoked = RevokeUserGrantsInTx(ctx, tx, user.UserId, server.NowUtc())
		})
		connect.AssertEqual(t, revoked, int64(2))

		for _, grant := range grants {
			connect.AssertEqual(t, grant.Testing_Remembered(ctx), false)
			connect.AssertEqual(t, testOpenRefreshTokenCount(t, ctx, grant.ClientId), 0)
			connect.AssertEqual(t, grant.Testing_Refresh(ctx) != nil, true)
			connect.AssertEqual(t, errors.Is(grant.Testing_RedeemPendingCode(ctx), ErrInvalidCode), true)
		}

		// the other user's grant is untouched
		connect.AssertEqual(t, otherGrant.Testing_Remembered(ctx), true)
		connect.AssertEqual(t, otherGrant.Testing_Refresh(ctx), nil)
		connect.AssertEqual(t, otherGrant.Testing_RedeemPendingCode(ctx), nil)
	})
}

// An approval waits for a credential rotation that holds the network's session
// lifecycle lock, then is refused, so the rotation's revocation never misses a
// grant written while it ran. The interleaving is observed in pg_locks, not
// timed.
func TestApprovalWaitsForACredentialRotation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeMcpRead})

		var workers sync.WaitGroup
		locked := make(chan struct{})
		lockedOnce := sync.OnceFunc(func() { close(locked) })
		releaseRotation := make(chan struct{})
		release := sync.OnceFunc(func() { close(releaseRotation) })
		defer func() {
			release()
			cancel()
			workers.Wait()
		}()

		// a rotation, as a password reset runs it
		rotated := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					rotated <- fmt.Errorf("%v", recovered)
				}
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(session.LockSessionLifecycle(ctx, tx, user.NetworkId, true))
				cutoff := server.NowUtc()
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`, user.UserId, cutoff))
				RevokeUserGrantsInTx(ctx, tx, user.UserId, cutoff)
				lockedOnce()
				select {
				case <-releaseRotation:
				case <-ctx.Done():
				}
			}, server.TxReadCommitted)
			rotated <- nil
		}()
		select {
		case <-locked:
		case err := <-rotated:
			t.Fatal("the rotation ended before it held the lock", err)
		case <-ctx.Done():
			t.Fatal("the rotation did not take the lock")
		}

		approved := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					approved <- fmt.Errorf("%v", recovered)
				}
			}()
			_, err := Authorize(ctx, request, user)
			approved <- err
		}()

		hash := sha256.Sum256(append([]byte("urnetwork:session-lifecycle:v1:"), user.NetworkId[:]...))
		lock := binary.BigEndian.Uint64(hash[:8])
		waiting := false
		for !waiting && ctx.Err() == nil {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND classid = $1::oid AND objid = $2::oid AND NOT granted)`,
					int64(uint32(lock>>32)),
					int64(uint32(lock)),
				).Scan(&waiting))
			})
			select {
			case err := <-approved:
				t.Fatal("the approval did not wait for the rotation", err)
			default:
			}
		}
		if !waiting {
			t.Fatal("the approval did not wait for the rotation")
		}

		release()
		connect.AssertEqual(t, <-rotated, nil)
		err := <-approved
		connect.AssertEqual(t, errors.Is(err, errUnauthorizedSignInAgain), true)
		connect.AssertEqual(t, GetConsent(ctx, user.UserId, client.ClientId) == nil, true)
	})
}
