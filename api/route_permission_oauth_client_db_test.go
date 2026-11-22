//go:build sidecar_pending

package api

// The role gate on a team's network OAuth client tokens (TEAM-M6-08; §7.8's scopes ∩ creator), on Postgres with
// MG6: a client token reaches a route only where both its scopes and its creating member's role hold the route's
// permission, the handler never reached otherwise.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
)

// An Admin's client carrying logs:read reads the admin events and creates no auth key, its scope lacking own devices;
// an Auditor's client carrying auth_keys reaches the creation of a key for its creator's own devices, which every role
// makes and the auth key model holds it to (network_auth_key_model.go), but neither lists nor revokes the network's
// keys, its creator's role lacking keys, and reads no admin event, its scope lacking read.
func TestOauthClientTokenScopesMeetTheCreatorsRole(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer oauth.Testing_SetIssuer("https://auth.oauth-client.example")()
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		server.Raise(err)
		thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
		server.Raise(err)
		defer oauth.Testing_SetSignerKeys(&oauth.SignerKey{Kid: oauth.SignerKeySetAdmin.KidPrefix() + thumbprint, Alg: "ES256", Set: oauth.SignerKeySetAdmin, PrivateKey: privateKey})()

		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
		})
		clientToken := func(role model.NetworkMemberRole, scopes ...string) string {
			var member *model.NetworkMember
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				member, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: server.NewId(), Role: role})
				server.Raise(err)
			})
			token, err := oauth.Testing_MintOauthClientToken(&model.NetworkOauthClient{
				OauthClientId: server.NewId(), NetworkId: teamId, Scopes: scopes, CreatorMemberId: member.MemberId,
			}, scopes)
			server.Raise(err)
			return token
		}
		adminLogs := clientToken(model.NetworkMemberRoleAdmin, model.ApiKeyScopeLogsRead)
		auditorKeys := clientToken(model.NetworkMemberRoleAuditor, model.ApiKeyScopeAuthKeys)

		cases := []struct {
			token    string
			routeKey string
			method   string
			path     string
			reached  bool
		}{
			{token: adminLogs, routeKey: "GET /network/admin-events", method: http.MethodGet, path: "/network/admin-events", reached: true},
			{token: adminLogs, routeKey: "POST /network/auth-keys", method: http.MethodPost, path: "/network/auth-keys", reached: false},
			{token: auditorKeys, routeKey: "POST /network/auth-keys", method: http.MethodPost, path: "/network/auth-keys", reached: true},
			{token: auditorKeys, routeKey: "GET /network/auth-keys", method: http.MethodGet, path: "/network/auth-keys", reached: false},
			{token: auditorKeys, routeKey: "POST /network/auth-key-revoke", method: http.MethodPost, path: "/network/auth-key-revoke", reached: false},
			{token: auditorKeys, routeKey: "GET /network/admin-events", method: http.MethodGet, path: "/network/admin-events", reached: false},
		}
		for _, c := range cases {
			reached := false
			route := testRoleGatedRoute(t, c.routeKey, &reached)
			w := testRoleServe(route, c.method, c.path, c.token)
			if reached != c.reached {
				t.Errorf("%s: reached %v, want %v (%d %s)", c.routeKey, reached, c.reached, w.Code, w.Body.String())
			}
			if !c.reached && w.Code != http.StatusForbidden {
				t.Errorf("%s: %d, want 403", c.routeKey, w.Code)
			}
		}
	})
}
