//go:build sidecar_pending

package model

// admin_token_use (admin_token_use_model.go; §7.8, D-TEAM-7, §22.5's §7 row):
// a one-command admin token works once, answers a retry after a lost response
// with the stored result and creates nothing more, spends nothing on a refused
// command, needs its scope, and its rows go two hours after their use.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// An admin token session of the network's owner, as the session builds one
// from a verified token: the network and member, unsigned, and its use.
func newTestAdminCommandSession(t testing.TB, ctx context.Context, networkId server.Id, userId server.Id, scopes []string) *session.ClientSession {
	t.Helper()
	clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "", false, false))
	clientSession.UnsignedIdentity = true
	clientSession.AdminCommand = &session.AdminCommandUse{
		NetworkId:  networkId,
		MemberId:   userId,
		Jti:        server.NewId().String(),
		Cmd:        "synthetic-cmd-digest",
		Scopes:     scopes,
		ExpireTime: server.NowUtc().Add(time.Hour),
	}
	return clientSession
}

func countNetworkAuthKeys(t testing.TB, ctx context.Context, networkId server.Id) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_auth_key WHERE network_id = $1`, networkId).Scan(&count))
	})
	return count
}

// The command runs once; a retry of the same token after a lost response gets
// the first answer, the same key, and creates nothing.
func TestAdminTokenCommandRunsOnceAndAnswersARetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "admin-token-use", userId)
		clientSession := newTestAdminCommandSession(t, ctx, networkId, userId, []string{"auth_keys"})
		args := &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1}

		first, err := CreateAuthKey(args, clientSession)
		if err != nil || first.Error != nil || first.AuthKey == "" {
			t.Fatalf("first use: %v %+v", err, first)
		}
		retry, err := CreateAuthKey(args, clientSession)
		if err != nil || retry.Error != nil {
			t.Fatalf("retry: %v %+v", err, retry)
		}
		if retry.AuthKey != first.AuthKey || *retry.AuthKeyId != *first.AuthKeyId || !retry.ExpireTime.Equal(*first.ExpireTime) {
			t.Fatalf("the retry answered another key: %+v then %+v", first, retry)
		}
		if count := countNetworkAuthKeys(t, ctx, networkId); count != 1 {
			t.Fatalf("%d keys after a retry", count)
		}
		// the stored key is the one the retry handed back, and it enrolls once
		if _, err := consumeTestAuthKey(ctx, retry.AuthKey, server.NowUtc()); err != nil {
			t.Fatal(err)
		}

		// another token of the same member is another use
		other := newTestAdminCommandSession(t, ctx, networkId, userId, []string{"auth_keys"})
		if second, err := CreateAuthKey(args, other); err != nil || second.Error != nil || second.AuthKey == first.AuthKey {
			t.Fatalf("another token: %v %+v", err, second)
		}
		if count := countNetworkAuthKeys(t, ctx, networkId); count != 2 {
			t.Fatalf("%d keys after another token", count)
		}
	})
}

// A command the model refuses spends no use, and a token without the
// auth_keys scope creates nothing.
func TestAdminTokenRefusalsSpendNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "admin-token-refusal", userId)
		clientSession := newTestAdminCommandSession(t, ctx, networkId, userId, []string{"auth_keys"})

		refused, err := CreateAuthKey(&CreateAuthKeyArgs{DurationSeconds: 10, MaxUses: 1}, clientSession)
		if err != nil || refused.Error == nil {
			t.Fatalf("a 10 s key: %v %+v", err, refused)
		}
		// the token's use is still unspent: its command can run
		if created, err := CreateAuthKey(&CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1}, clientSession); err != nil || created.Error != nil {
			t.Fatalf("after a refusal: %v %+v", err, created)
		}

		unscoped := newTestAdminCommandSession(t, ctx, networkId, userId, []string{"logs:read"})
		if _, err := CreateAuthKey(&CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1}, unscoped); err == nil {
			t.Fatal("a token without auth_keys created a key")
		}
		if count := countNetworkAuthKeys(t, ctx, networkId); count != 1 {
			t.Fatalf("%d keys", count)
		}
	})
}

// A jti names one member and one command: a replay that names another is
// refused rather than answered with the other's result.
func TestAdminTokenUseRefusesAnotherCommandUnderItsJti(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		use := &session.AdminCommandUse{MemberId: server.NewId(), Jti: server.NewId().String(), Cmd: "a"}
		server.Tx(ctx, func(tx server.PgTx) {
			_, first, err := ClaimAdminTokenUseInTx(ctx, tx, use, server.NowUtc())
			server.Raise(err)
			if !first {
				t.Fatal("a new jti was not first")
			}
			server.Raise(StoreAdminTokenUseResultInTx(ctx, tx, use.Jti, []byte(`{}`)))
		})
		other := *use
		other.Cmd = "b"
		server.Tx(ctx, func(tx server.PgTx) {
			if _, _, err := ClaimAdminTokenUseInTx(ctx, tx, &other, server.NowUtc()); err == nil {
				t.Fatal("a jti answered another command")
			}
		})
	})
}

// Rows go two hours after their use: an hour past their token's exp.
func TestAdminTokenUsesSweptAfterTheirHours(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		retention := DefaultAdminTokenUseSettings().Retention
		old := &session.AdminCommandUse{MemberId: server.NewId(), Jti: server.NewId().String(), Cmd: "old"}
		recent := &session.AdminCommandUse{MemberId: server.NewId(), Jti: server.NewId().String(), Cmd: "recent"}
		for _, claim := range []struct {
			use *session.AdminCommandUse
			at  time.Time
		}{
			{use: old, at: now.Add(-retention - time.Minute)},
			{use: recent, at: now.Add(-retention + time.Minute)},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := ClaimAdminTokenUseInTx(ctx, tx, claim.use, claim.at)
				server.Raise(err)
				server.Raise(StoreAdminTokenUseResultInTx(ctx, tx, claim.use.Jti, []byte(`{}`)))
			})
		}
		RemoveOldAdminTokenUses(ctx, now.Add(-retention))
		exists := func(jti string) bool {
			found := false
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM admin_token_use WHERE jti = $1)`, jti).Scan(&found))
			})
			return found
		}
		if exists(old.Jti) {
			t.Fatal("a use past its two hours stayed")
		}
		if !exists(recent.Jti) {
			t.Fatal("a use inside its two hours went")
		}
	})
}
