//go:build sidecar_pending

package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
)

// The production source resolves the DNS label in SQL and evaluates these folded ACL rows.
type teamEdgeGate struct {
	networkId sidecar.Id
	snapshot  *sidecar.AclGateSnapshot
}

func (g *teamEdgeGate) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	if networkId != g.networkId || g.snapshot == nil {
		return nil, sidecar.AclGateUnknown
	}
	return g.snapshot, sidecar.AclGateLoaded
}

func (*teamEdgeGate) Changed(sidecar.Id) {}

func teamEdgeSnapshot(t testing.TB, published bool) *sidecar.AclGateSnapshot {
	t.Helper()
	view := sidecartest.NewFakeAclView()
	atom := sidecartest.FakeAclSidecarAtom(view.AddDevice(view.AddMember(bridge.MemberRole_MEMBER_ROLE_MEMBER)))
	rows := &protocol.AclRows{HasDocument: true, Atoms: []*protocol.AclAtom{atom}}
	if published {
		rows.Rules = []*protocol.AclRule{{
			RuleId: sidecar.NewId().Bytes(), SrcAtoms: [][]byte{atom.Atom},
			Audience: protocol.AclAudience_ACL_AUDIENCE_MEMBERS,
			Actions:  []uint32{uint32(protocol.AclAction_ACL_ACTION_LISTEN), uint32(protocol.AclAction_ACL_ACTION_PUBLISH)},
			Targets:  []*protocol.AclTarget{{PublicName: "web"}, {PublicName: "tls/443/web"}, {PublicName: "wiki"}, {PublicName: "tls/443/wiki"}},
		}}
	}
	acl, err := sidecar.NewAclState(rows, view)
	if err != nil {
		t.Fatal(err)
	}
	return &sidecar.AclGateSnapshot{Pin: view.Pin(), Config: &sidecar.ConfigState{}, Acl: acl}
}

func TestTeamMemberSignsInAtThePublishedOrigin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		defer oauth.Testing_SetIssuer("https://issuer.team.example")()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		server.Raise(err)
		kid, err := oauth.SignerKid(&key.PublicKey)
		server.Raise(err)
		defer oauth.Testing_SetSignerKeys(&oauth.SignerKey{
			Kid: oauth.SignerKeySetId.KidPrefix() + kid,
			Alg: "ES256", Set: oauth.SignerKeySetId, PrivateKey: key,
		})()

		personal, team := newRouteAccessDbFixture(t, ctx), newRouteAccessDbFixture(t, ctx)
		membership := &model.AddNetworkMemberArgs{
			NetworkId: team.networkId,
			UserId:    personal.userId,
			Role:      model.NetworkMemberRoleMember,
		}
		var member *model.NetworkMember
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team(network_id) VALUES($1)`, team.networkId))
			member, err = model.AddNetworkMemberInTx(ctx, tx, membership)
			server.Raise(err)
		})
		const root = "edge.example"
		server.Raise(model.EnsureDnsRoot(ctx, root))
		label, err := model.EnsureNetworkLabel(ctx, team.networkId, root)
		server.Raise(err)
		origin := "https://web." + label + "." + root
		granted := teamEdgeSnapshot(t, true)
		gate := &teamEdgeGate{networkId: sidecar.Id(team.networkId), snapshot: granted}
		oauth.SetEdgeLoginSource(oauth.NewProjectionEdgeLoginSource(gate))
		defer oauth.SetEdgeLoginSource(nil)
		teamSignIn := model.ResolveNetworkSignIn(ctx, personal.userId, team.networkId, server.NowUtc())
		if teamSignIn == nil || teamSignIn.MemberId == nil {
			t.Fatal("missing test membership")
		}
		teamToken := model.NewNetworkSignInByJwt(teamSignIn, false).Testing_Sign()

		const verifier = "team-edge-sign-in-private-stack-code-verifier-123456789"
		challenge := sha256.Sum256([]byte(verifier))
		request := oauth.ConsentRequest{
			ClientId:            origin,
			RedirectUri:         origin + oauth.EdgeCallbackPath,
			ResponseType:        "code",
			Scope:               "openid",
			State:               "test-state",
			Nonce:               "test-nonce",
			CodeChallenge:       base64.RawURLEncoding.EncodeToString(challenge[:]),
			CodeChallengeMethod: oauth.PkceMethodS256,
		}
		handler := router.NewRouter(ctx, Routes())
		post := func(path string, token string, body oauth.ConsentRequest, want int) *httptest.ResponseRecorder {
			t.Helper()
			raw, err := json.Marshal(body)
			server.Raise(err)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != want {
				t.Fatalf("%s status %d, want %d", path, w.Code, want)
			}
			return w
		}
		consent := func(token string) {
			t.Helper()
			w := post("/oauth/consent", token, request, http.StatusOK)
			var result oauth.ConsentResult
			server.Raise(json.Unmarshal(w.Body.Bytes(), &result))
			if result.ConsentRequired || result.NetworkName != team.networkName || result.ClientUri != origin {
				t.Fatal("consent did not identify the destination Team and site")
			}
		}
		authorize := func(token string) string {
			t.Helper()
			w := post("/oauth/authorize", token, request, http.StatusOK)
			var result oauth.AuthorizeResult
			server.Raise(json.Unmarshal(w.Body.Bytes(), &result))
			callback, err := url.Parse(result.RedirectUri)
			server.Raise(err)
			q := callback.Query()
			if q.Get("state") != request.State || q.Get("code") == "" || q.Get("iss") != oauth.Issuer() {
				t.Fatal("callback did not bind the code to the issuer and state")
			}
			return q.Get("code")
		}
		redeem := func(code, clientId string, want int) *httptest.ResponseRecorder {
			t.Helper()
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {clientId},
				"redirect_uri":  {clientId + oauth.EdgeCallbackPath},
				"code":          {code},
				"code_verifier": {verifier},
			}
			req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != want {
				t.Fatalf("token status %d, want %d", w.Code, want)
			}
			return w
		}

		gate.snapshot = nil
		post("/oauth/consent", teamToken, request, http.StatusServiceUnavailable)
		gate.snapshot = granted
		post("/oauth/consent", personal.apiKey, request, http.StatusForbidden)
		post("/oauth/authorize", personal.apiKey, request, http.StatusForbidden)
		for _, token := range []string{teamToken, personal.rootToken} {
			consent(token)
			code := authorize(token)
			w := redeem(code, origin, http.StatusOK)
			var result oauth.EdgeTokenResponse
			server.Raise(json.Unmarshal(w.Body.Bytes(), &result))
			claims, err := oauth.VerifyEdgeIdToken(result.IdToken, origin, request.Nonce, server.NowUtc())
			if err != nil || claims.Subject != member.MemberId.String() || claims.NetworkId != team.networkId {
				t.Fatal("edge token did not identify the destination membership")
			}
			redeem(code, origin, http.StatusBadRequest)
		}

		adminCommand := request
		adminCommand.ClientId = oauth.UrcliClientId
		adminCommand.Scope = oauth.UrcliScopeAuthKeys
		adminCommand.Resource = oauth.Issuer()
		post("/oauth/consent", teamToken, adminCommand, http.StatusForbidden)
		post("/oauth/authorize", teamToken, adminCommand, http.StatusForbidden)

		wrongCallback := request
		wrongCallback.RedirectUri = "https://other.example" + oauth.EdgeCallbackPath
		post("/oauth/authorize", personal.rootToken, wrongCallback, http.StatusBadRequest)
		codeForWeb := authorize(personal.rootToken)
		redeem(codeForWeb, "https://wiki."+label+"."+root, http.StatusBadRequest)
		redeem(codeForWeb, origin+":8443", http.StatusBadRequest)
		redeem(codeForWeb, origin, http.StatusOK)

		publishedCode := authorize(personal.rootToken)
		gate.snapshot = teamEdgeSnapshot(t, false)
		post("/oauth/consent", teamToken, request, http.StatusBadRequest)
		redeem(publishedCode, origin, http.StatusBadRequest)
		gate.snapshot = granted

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_team SET sso_required=true WHERE network_id=$1`, team.networkId))
		})
		post("/oauth/consent", personal.rootToken, request, http.StatusForbidden)
		post("/oauth/authorize", teamToken, request, http.StatusForbidden)
		tracked := model.NewNetworkSignInByJwt(teamSignIn, false)
		sid := server.NewId()
		tracked.SessionId = &sid
		legacySsoToken, err := session.MintNetworkSession(ctx, tracked, session.NetworkSsoSessionKind)
		server.Raise(err)
		consent(legacySsoToken)

		defer session.Testing_SetSessionCreationEnabled(false)()
		ssoToken, _, err := model.MintNetworkSignIn(ctx, personal.userId, team.networkId, session.NetworkSsoSessionKind, true)
		server.Raise(err)
		consent(ssoToken)

		code := authorize(ssoToken)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_team SET sso_required=false WHERE network_id=$1`, team.networkId))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_member WHERE member_id=$1`, member.MemberId))
		})
		post("/oauth/consent", personal.rootToken, request, http.StatusForbidden)
		post("/oauth/authorize", personal.rootToken, request, http.StatusForbidden)
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := model.AddNetworkMemberInTx(ctx, tx, membership)
			server.Raise(err)
		})
		consent(personal.rootToken)
		redeem(code, origin, http.StatusForbidden)
	})
}
