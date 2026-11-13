package model

import (
	"context"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

// The user auth migration (`bringyourctl migrate-user-auth`) logged each
// sign-in token it moved, in full, at the default level. It logs the user and
// the auth type instead. The token is synthetic.
func TestMigrateNetworkUserChildAuthsLogsNoSignInToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		userAuth := "migrate-sign-in@example.com"
		// the migration re-reads the stored token without verifying it
		authJwt, err := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{
			"iss":            "https://accounts.google.com",
			"sub":            "synthetic-migrate-subject",
			"email":          userAuth,
			"email_verified": true,
			"exp":            time.Now().Add(time.Hour).Unix(),
		}).SignedString([]byte("synthetic-migrate-signing-key"))
		if err != nil {
			t.Fatal(err)
		}
		// a legacy sign-in user, with its token on its own row and no child auth
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_user (user_id, user_name, auth_type, user_auth, auth_jwt)
					VALUES ($1, 'migrate-sign-in', $2, $3, $4)
				`,
				userId,
				AuthTypeGoogle,
				userAuth,
				authJwt,
			))
		})

		logs := captureStderr(t, func() {
			MigrateNetworkUserChildAuths(ctx)
		})

		// the migration moved the sign-in, and the capture holds its lines
		if count := countRows(ctx, `SELECT COUNT(*) FROM network_user_auth_sso WHERE user_id = $1`, userId); count != 1 {
			t.Fatalf("the migration added %d sign-in auths, want 1", count)
		}
		if !strings.Contains(logs, "Added SSO auth for user "+userId.String()) {
			t.Fatalf("the captured logs have no line for the migrated sign-in")
		}
		// the claims and the signature; the header is the same for every
		// token of its algorithm
		for _, part := range strings.Split(authJwt, ".")[1:] {
			if strings.Contains(logs, part) {
				t.Errorf("the migration logged the sign-in token (a %d byte part of it)", len(part))
			}
		}
	})
}
