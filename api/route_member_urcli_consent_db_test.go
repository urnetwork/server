//go:build sidecar_pending

package api

// `ur login` for a team Member at the role gate (§7.5's Member: own devices; §5.6, §7.8), on Postgres with MG6 and the
// device grant's store on Redis. The consent page's four routes ask the keys permission, which a Member lacks, and admit
// it only to `ur login`'s one command, a single-use key for its own devices; each route's handler is replaced, since
// the gate alone decides here (oauth's own tests run the grant and the token).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// A team Member's signed-in credential reaches POST /oauth/consent and POST /oauth/authorize for `ur login`'s command,
// and POST /oauth/device_consent and POST /oauth/device_approve for a waiting device authorization of it; it is refused
// 403 RolePermissionRefused, the handler never reached, for an all command such as an invite, for the key command under
// the all scope, and for a device authorization of the invite. An Admin, who holds the keys permission, reaches the
// invite's consent as before.
func TestTeamMemberConsentsToUrLoginsOwnKeyAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Setenv("WARP_DOMAIN", "urcli-gate.example")
		defer oauth.Testing_SetIssuer("https://auth.urcli-gate.example")()
		apiOrigin, err := session.DpopApiOrigin()
		if err != nil {
			t.Fatal(err)
		}
		teamId, ownerOfRecordId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, teamId, "team-"+teamId.String(), ownerOfRecordId)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, teamId))
		})
		tokenOf := func(role model.NetworkMemberRole) string {
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, server.NewId(), "member-"+userId.String(), userId)
			server.Tx(ctx, func(tx server.PgTx) {
				_, err := model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: teamId, UserId: userId, Role: role})
				server.Raise(err)
			})
			signIn := model.ResolveNetworkSignIn(ctx, userId, teamId, server.NowUtc())
			if signIn == nil || signIn.MemberId == nil {
				t.Fatalf("no sign-in for the %s", role)
			}
			return model.NewNetworkSignInByJwt(signIn, false).Testing_Sign()
		}
		memberToken := tokenOf(model.NetworkMemberRoleMember)
		adminToken := tokenOf(model.NetworkMemberRoleAdmin)

		publicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		jkt := session.DpopKeyThumbprint(publicKey)
		detailsOf := func(command *sidecar.AdminCommand) string {
			consentCode, err := sidecar.NewAdminConsentCode(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			details, err := sidecar.FormatAdminCommandDetails(command, consentCode)
			if err != nil {
				t.Fatal(err)
			}
			return details
		}
		ownKey := &sidecar.AdminCommand{Method: "POST", Path: "/network/auth-keys", Body: []byte(`{"duration_seconds":300,"max_uses":1}`)}
		invite := &sidecar.AdminCommand{Method: "POST", Path: "/network/member-invite", Body: []byte(`{"email":"invitee@example.com","role":"admin","expire_days":7}`)}
		consentBody := func(command *sidecar.AdminCommand, scope string) string {
			body, err := json.Marshal(&oauth.ConsentRequest{
				ClientId:             oauth.UrcliClientId,
				RedirectUri:          "http://127.0.0.1:49152/callback",
				ResponseType:         "code",
				Scope:                scope,
				State:                "synthetic-state",
				CodeChallenge:        "synthetic-challenge",
				CodeChallengeMethod:  oauth.PkceMethodS256,
				Resource:             apiOrigin,
				DpopJkt:              jkt,
				AuthorizationDetails: detailsOf(command),
			})
			if err != nil {
				t.Fatal(err)
			}
			return string(body)
		}

		defer oauth.SetDeviceAuthorizationStore(nil)
		oauth.SetDeviceAuthorizationStore(oauth.NewRedisDeviceAuthorizationStore(oauth.DefaultDeviceGrantSettings()))
		userCodeOf := func(command *sidecar.AdminCommand, scope string) string {
			response, err := oauth.AuthorizeDevice(ctx, &oauth.DeviceAuthorizationArgs{
				ClientId:             oauth.UrcliClientId,
				Scopes:               []string{scope},
				Resource:             apiOrigin,
				DpopJkt:              jkt,
				AuthorizationDetails: detailsOf(command),
			})
			if err != nil {
				t.Fatal(err)
			}
			return response.UserCode
		}
		deviceBody := func(userCode string) string {
			body, err := json.Marshal(&oauth.DeviceConsentRequest{UserCode: userCode, ConsentReceipt: "synthetic-receipt", Approve: true})
			if err != nil {
				t.Fatal(err)
			}
			return string(body)
		}
		ownKeyUserCode := userCodeOf(ownKey, oauth.UrcliScopeAuthKeys)
		inviteUserCode := userCodeOf(invite, oauth.UrcliScopeAll)

		serve := func(routeKey string, token string, body string) (bool, *httptest.ResponseRecorder) {
			reached := false
			route := testRoleGatedRoute(t, routeKey, &reached)
			method, path, _ := strings.Cut(routeKey, " ")
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.RemoteAddr = "198.51.100.45:4000"
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			routerCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			router.NewRouter(routerCtx, []*router.Route{route}).ServeHTTP(w, r)
			return reached, w
		}
		cases := []struct {
			name     string
			routeKey string
			token    string
			body     string
			reached  bool
		}{
			{"the Member's consent to ur login", "POST /oauth/consent", memberToken, consentBody(ownKey, oauth.UrcliScopeAuthKeys), true},
			{"the Member's authorization of ur login", "POST /oauth/authorize", memberToken, consentBody(ownKey, oauth.UrcliScopeAuthKeys), true},
			{"the Member's device consent to ur login", "POST /oauth/device_consent", memberToken, deviceBody(ownKeyUserCode), true},
			{"the Member's device approval of ur login", "POST /oauth/device_approve", memberToken, deviceBody(ownKeyUserCode), true},
			{"the Member's consent to an invite", "POST /oauth/consent", memberToken, consentBody(invite, oauth.UrcliScopeAll), false},
			{"the Member's authorization of an invite", "POST /oauth/authorize", memberToken, consentBody(invite, oauth.UrcliScopeAll), false},
			{"the Member's consent to the key under all", "POST /oauth/consent", memberToken, consentBody(ownKey, oauth.UrcliScopeAll), false},
			{"the Member's device consent to an invite", "POST /oauth/device_consent", memberToken, deviceBody(inviteUserCode), false},
			{"the Member's device approval of an invite", "POST /oauth/device_approve", memberToken, deviceBody(inviteUserCode), false},
			{"the Admin's consent to an invite", "POST /oauth/consent", adminToken, consentBody(invite, oauth.UrcliScopeAll), true},
		}
		for _, c := range cases {
			reached, w := serve(c.routeKey, c.token, c.body)
			if reached != c.reached {
				t.Errorf("%s: reached %t, want %t (%d %s)", c.name, reached, c.reached, w.Code, w.Body.String())
				continue
			}
			if !c.reached && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), router.RolePermissionRefusedMessage)) {
				t.Errorf("%s: %d %s, not the role gate's refusal", c.name, w.Code, w.Body.String())
			}
		}
	})
}
