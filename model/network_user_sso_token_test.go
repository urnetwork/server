package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// GET /network/user answers GetNetworkUser's user to any app credential, a
// client token included, and it carried each sign-in method's stored token.
// An unexpired token signs in at /auth/login as the network, so a client token
// could become a network credential. The answer names each sign-in by its type
// and user auth only. The token is synthetic.
func TestGetNetworkUserHoldsNoSignInToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "sso-token-net", userId)
		ssoUserAuth := "sso-token@example.com"
		authJwt := "synthetic-sso-header.synthetic-sso-claims.synthetic-sso-signature"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_user_auth_sso (user_id, auth_type, user_auth, auth_jwt)
					VALUES ($1, $2, $3, $4)
				`,
				userId,
				SsoAuthTypeGoogle,
				ssoUserAuth,
				authJwt,
			))
		})

		networkUser := GetNetworkUser(ctx, userId)
		if networkUser == nil || len(networkUser.SsoAuths) != 1 {
			t.Fatalf("the user has sign-ins %v", networkUser)
		}
		answer, err := json.Marshal(networkUser)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(answer), "synthetic-sso") || strings.Contains(string(answer), `"auth_jwt"`) {
			t.Errorf("the user answer holds the stored sign-in token")
		}
		decoded := struct {
			SsoAuths []map[string]any `json:"sso_auths"`
		}{}
		if err := json.Unmarshal(answer, &decoded); err != nil {
			t.Fatal(err)
		}
		if len(decoded.SsoAuths) != 1 || decoded.SsoAuths[0]["auth_type"] != string(SsoAuthTypeGoogle) || decoded.SsoAuths[0]["user_auth"] != ssoUserAuth {
			t.Errorf("the user answer names its sign-in as %v", decoded.SsoAuths)
		}
	})
}
