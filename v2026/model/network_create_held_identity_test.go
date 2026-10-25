package model

// Network creation refuses a user auth or a sign-in identity that another user
// holds only in a child table (an auth added to an existing account) before it
// writes anything. The add-auth helpers used to refuse it after the user row
// was written: the password path ignored the refusal and created the network
// without its password auth, and the sign-in path committed the user row
// alone. Each call runs under forcedFailureCallTimeout (see
// forced_statement_failure_test.go).

import (
	"context"
	"net/http"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Creates a user whose own row carries no user auth, as a seedphrase account
// does, to hold user auths in the child tables only.
func createChildTableHolder(ctx context.Context) (holderUserId server.Id) {
	holderUserId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type) VALUES ($1, 'child-table-holder', $2)`,
			holderUserId,
			AuthTypeSeedphrase,
		))
	})
	return
}

// Another user holds the user auth only in a child table, as an auth added to
// an existing account: the password table or the sign-in table. A password
// network create for it is refused like any existing account, and writes
// nothing. It used to create the network without its password auth, because
// the add-auth refusal came after the user was written and was ignored.
func TestNetworkCreateRefusesUserAuthAnotherUserHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		holderUserId := createChildTableHolder(ctx)
		password := "synthetic-password-1"

		for _, c := range []struct {
			name        string
			userAuth    string
			networkName string
			// gives the holder the user auth in a child table only
			holdSql string
		}{
			{
				name:        "password table",
				userAuth:    "held-password@example.com",
				networkName: "held-password-create",
				holdSql: `
					INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified)
					VALUES ($1, $2, 'email', true)
				`,
			},
			{
				name:        "sign-in table",
				userAuth:    "held-sign-in@example.com",
				networkName: "held-sign-in-create",
				holdSql: `
					INSERT INTO network_user_auth_sso (user_id, auth_type, user_auth, auth_jwt)
					VALUES ($1, 'google', $2, 'synthetic-jwt')
				`,
			},
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, c.holdSql, holderUserId, c.userAuth))
			})

			userAuth := c.userAuth
			var result *NetworkCreateResult
			var err error
			panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
				result, err = NetworkCreate(
					NetworkCreateArgs{
						UserName:    "held-identity",
						UserAuth:    &userAuth,
						Password:    &password,
						NetworkName: c.networkName,
						Terms:       true,
					},
					session.Testing_CreateClientSession(callCtx, nil),
				)
			})
			if panicValue != nil || err != nil {
				t.Errorf("%s: a held user auth ended with %v (panic %v), want the refusal", c.name, err, panicValue)
				continue
			}
			if result == nil || result.Error == nil || result.Error.Error() != "409 Account might already exist. Please start over." {
				t.Errorf("%s: a held user auth answered %+v, want the 409 existing-account refusal", c.name, result)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network WHERE network_name = $1`, c.networkName); count != 0 {
				t.Errorf("%s: the refused create wrote %d networks", c.name, count)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user WHERE user_auth = $1`, c.userAuth); count != 0 {
				t.Errorf("%s: the refused create wrote %d users", c.name, count)
			}
			if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_password WHERE user_auth = $1 AND user_id <> $2`, c.userAuth, holderUserId); count != 0 {
				t.Errorf("%s: the refused create wrote %d password auths", c.name, count)
			}
		}
	})
}

// The sign-in create path refuses an identity another user holds only in a
// child table before writing anything. The add-sso helper used to refuse it
// after the user row was written, and the user row then committed alone, with
// no network and no sign-in.
func TestNetworkCreateAuthJwtRefusesIdentityAnotherUserHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		holderUserId := createChildTableHolder(ctx)
		userAuth := "held-sign-in-create@example.com"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified)
					VALUES ($1, $2, 'email', true)
				`,
				holderUserId,
				userAuth,
			))
		})

		authJwt := "synthetic-jwt"
		authJwtType := SsoAuthTypeGoogle
		var result networkCreateResult
		panicValue := callWithForcedFailure(ctx, func(callCtx context.Context) {
			result = networkCreateAuthJwt(
				callCtx,
				&NetworkCreateArgs{
					UserName:    "held-sign-in",
					AuthJwt:     &authJwt,
					AuthJwtType: &authJwtType,
					NetworkName: "held-sign-in-network",
					Terms:       true,
				},
				false,
				AuthJwt{
					AuthType: AuthTypeGoogle,
					UserAuth: userAuth,
					UserName: "held-sign-in",
				},
				"held-sign-in-network",
				userAuth,
			)
		})
		if panicValue != nil {
			t.Fatalf("a held identity ended with %v, want the refusal", panicValue)
		}
		if result.Created || result.refusalStatus != http.StatusConflict {
			t.Errorf("a held identity answered created %t, status %d, want the 409 refusal", result.Created, result.refusalStatus)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user WHERE user_auth = $1`, userAuth); count != 0 {
			t.Errorf("the refused create wrote %d users", count)
		}
		if count := countRows(ctx, `SELECT COUNT(*) FROM network WHERE network_name = $1`, "held-sign-in-network"); count != 0 {
			t.Errorf("the refused create wrote %d networks", count)
		}
	})
}
