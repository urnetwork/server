//go:build sidecar_pending

package api

// End to end through the router the API serves, against the database with MG0:
// §22.5's §7 row for the P0 auth fix. A real marked client, minted with POST
// /network/auth-client, is refused on the 37 routes and changes nothing, a client
// whose marks cannot be read is refused alike, a real strict_exp bearer reaches
// its allowlist and proves its key to renew and mint, and a login minted with an
// API key's auth code ends with the key.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

// The classes whose routes stage 1 adds a refusal to.
var clientAdminClasses = []routeAccess{routeAccessAppAdmin, routeAccessOwnClientPayout}

// Mints a client with the marks, requiring that the reply confirms them.
func (self *routeAccessDbFixture) mintMarked(t testing.TB, authorization string, body map[string]any, extra func(r *http.Request)) *model.AuthNetworkClientResult {
	t.Helper()
	body["description"] = "sidecar"
	body["device_spec"] = "sidecar"
	w := self.serveEdited(http.MethodPost, "/network/auth-client", authorization, body, extra)
	if w.Code != http.StatusOK {
		t.Fatalf("auth-client status = %d: %s", w.Code, w.Body.String())
	}
	result := &model.AuthNetworkClientResult{}
	server.Raise(json.Unmarshal(w.Body.Bytes(), result))
	if result.Error != nil || result.ClientId == nil || result.ByClientJwt == nil {
		t.Fatalf("auth-client = %+v, error = %+v", result, result.Error)
	}
	if !result.AdminRefused {
		t.Fatalf("the reply does not confirm admin_refused: %s", w.Body.String())
	}
	return result
}

// Serves the request after edit changes it.
func (self *routeAccessDbFixture) serveEdited(method string, path string, authorization string, body any, edit func(r *http.Request)) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		server.Raise(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	r.RemoteAddr = "198.51.100.44:4000"
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	self.router.ServeHTTP(w, r)
	return w
}

// The network as the only preview network.
func (self *routeAccessDbFixture) previewYaml() string {
	return fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", self.networkId)
}

// Stage 1, whole: a child and an admin_refused client are refused on every one of
// the 37 routes, changing nothing and each refusal counted, with no sidecar.yml at
// all, and an unmarked top-level client, what the shipped apps hold, is left
// alone.
func TestMarkedClientTokensAreRefusedOnEveryAdminRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		defer server.Testing_PushSidecarConfig("")()

		legacyClientId, legacyToken := self.mintClient(t, self.root(), nil)
		_, childToken := self.mintClient(t, self.root(), &legacyClientId)
		marked := self.mintMarked(t, self.root(), map[string]any{"admin_refused": true}, nil)
		victimClientId, _ := self.mintClient(t, self.root(), nil)
		const codeCreate = "POST /auth/code-create"

		for name, token := range map[string]string{"child": childToken, "admin_refused": *marked.ByClientJwt} {
			before := childAdminCallCount(codeCreate, p0RefusalClientAdmin, p0ResultRefused)
			w := self.serve(http.MethodPost, "/auth/code-create", "Bearer "+token, map[string]any{})
			if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
				t.Fatalf("%s: code-create status = %d: %s", name, w.Code, w.Body.String())
			}
			if after := childAdminCallCount(codeCreate, p0RefusalClientAdmin, p0ResultRefused); after != before+1 {
				t.Fatalf("%s: counted %v", name, after-before)
			}
		}

		for _, token := range []string{childToken, *marked.ByClientJwt} {
			refusedCount := requireClientTokenRefused(t, self, token, victimClientId, clientAdminClasses...)
			if refusedCount != 37 {
				t.Fatalf("refused %d routes, want §7.4's 37", refusedCount)
			}
		}

		// the client's own routes still work: what is refused is administration
		w := self.serve(http.MethodGet, "/transfer/stats", "Bearer "+childToken, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("a child's own route status = %d: %s", w.Code, w.Body.String())
		}

		// and the shipped apps' token keeps the admin routes it calls today
		w = self.serve(http.MethodGet, "/network/clients", "Bearer "+legacyToken, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("a legacy client's admin route status = %d: %s", w.Code, w.Body.String())
		}
	})
}

// D-TEAM-1's lookup error through the routes: a client token whose client the
// check cannot read, here one its network has no row for, is refused with 403 on
// every one of the 37 routes by the real model check, never a 401 that would sign
// an sdk out.
func TestClientWhoseMarksCannotBeReadIsRefusedOnEveryAdminRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)
		model.Testing_ForgetClientRefusesAdmin()

		byJwt, err := session.ParseByJwt(ctx, strings.TrimPrefix(self.root(), "Bearer "))
		server.Raise(err)
		unreadable := "Bearer " + byJwt.Client(server.NewId(), server.NewId()).Testing_Sign()
		refusedCount := 0
		for _, route := range Routes() {
			switch routeAccessFor(route) {
			case routeAccessAppAdmin, routeAccessOwnClientPayout:
			default:
				continue
			}
			key := routeAccessKey(route)
			before := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused)
			w := self.serve(route.Method(), routeAccessTestPath(route.Pattern()), unreadable, map[string]any{})
			if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.ClientCredentialRefusedMessage {
				t.Fatalf("%s: status = %d body = %q, want the client token refusal", key, w.Code, w.Body.String())
			}
			if after := childAdminCallCount(key, p0RefusalClientAdmin, p0ResultRefused); after != before+1 {
				t.Fatalf("%s: counted %v", key, after-before)
			}
			refusedCount += 1
		}
		if refusedCount != 37 {
			t.Fatalf("refused %d routes, want §7.4's 37", refusedCount)
		}
	})
}

// A real strict_exp client and the key bound to it.
type strictExpBearer struct {
	clientId   server.Id
	token      string
	privateKey ed25519.PrivateKey
	jti        int
}

// Mints a strict_exp client with the network credential and binds it a key with
// §5's POST /sidecar/bind, as a sidecar does before it refreshes, so its proofs
// check against the key model.ClientProofPublicKey reads from the binding.
func mintStrictExpBearer(t *testing.T, self *routeAccessDbFixture) *strictExpBearer {
	t.Helper()
	result := self.mintMarked(t, self.root(), map[string]any{"admin_refused": true, "strict_exp": true}, nil)
	if !result.StrictExp {
		t.Fatalf("the reply does not confirm strict_exp: %+v", result)
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	bearer := &strictExpBearer{clientId: *result.ClientId, token: *result.ByClientJwt, privateKey: privateKey}

	byJwt, err := session.ParseByJwt(context.Background(), bearer.token)
	server.Raise(err)
	identitySeed := make([]byte, ed25519.SeedSize)
	_, err = rand.Read(identitySeed)
	server.Raise(err)
	signedBinding, err := sidecar.NewSidecarIdentityBinding(&sidecar.SidecarIdentityBinding{
		NetworkId:  sidecar.Id(byJwt.NetworkId),
		SidecarId:  sidecar.Id(server.NewId()),
		ClientId:   sidecar.Id(bearer.clientId),
		Generation: 1,
		IssuedAt:   server.NowUtc(),
	}, identitySeed, privateKey.Seed())
	server.Raise(err)
	w := self.serve(http.MethodPost, "/sidecar/bind", bearer.authorization(), map[string]any{"binding": signedBinding})
	if w.Code != http.StatusOK {
		t.Fatalf("bind status = %d: %s", w.Code, w.Body.String())
	}
	return bearer
}

func (self *strictExpBearer) authorization() string {
	return "Bearer " + self.token
}

// A fresh proof of the request by the bearer's key.
func (self *strictExpBearer) proof() func(r *http.Request) {
	self.jti += 1
	jti := fmt.Sprintf("jti-%d", self.jti)
	return func(r *http.Request) {
		url := "https://" + r.Host + r.URL.Path
		r.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(self.privateKey, r.Method, url, self.token, jti, server.NowUtc()))
	}
}

// The sidecar's life on its allowlist: it renews with its key, the renewal is
// another 7-day strict_exp token, it mints a child that inherits both marks, and
// off the list it hears 403, which never signs an sdk out.
func TestStrictExpBearerLivesOnItsAllowlist(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		bearer := mintStrictExpBearer(t, self)

		w := self.serveEdited(http.MethodGet, "/auth/refresh", bearer.authorization(), nil, bearer.proof())
		if w.Code != http.StatusOK {
			t.Fatalf("refresh status = %d: %s", w.Code, w.Body.String())
		}
		var refreshed struct {
			ByJwt string `json:"by_jwt"`
		}
		server.Raise(json.Unmarshal(w.Body.Bytes(), &refreshed))
		byJwt, err := session.ParseByJwt(ctx, refreshed.ByJwt)
		server.Raise(err)
		if !byJwt.StrictExp || byJwt.RootClientId == nil || 7*24*time.Hour+time.Minute < time.Until(byJwt.ExpiresAt.Time) {
			t.Fatalf("the renewal is not a strict_exp token: %+v", byJwt)
		}

		child := self.mintMarked(t, bearer.authorization(), map[string]any{"source_client_id": bearer.clientId}, bearer.proof())
		if !child.StrictExp {
			t.Fatalf("the child did not inherit strict_exp: %+v", child)
		}

		for _, path := range []string{"/transfer/stats", "/subscription/balance", "/account/points"} {
			w := self.serve(http.MethodGet, path, bearer.authorization(), nil)
			if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
				t.Fatalf("%s: status = %d body = %q, want the sidecar refusal", path, w.Code, w.Body.String())
			}
		}

		// a sidecar leaves by removing its own client
		w = self.serve(http.MethodPost, "/network/remove-client", bearer.authorization(), map[string]any{"client_id": bearer.clientId})
		if w.Code != http.StatusOK {
			t.Fatalf("remove-client status = %d: %s", w.Code, w.Body.String())
		}
	})
}

// §22.5's §7 row, on a real token: a copy alone fails refresh and auth-client, as
// do a replay and a proof by a key that is no longer the newest.
func TestACopiedStrictExpTokenRenewsAndMintsNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		bearer := mintStrictExpBearer(t, self)

		_, olderKey, err := ed25519.GenerateKey(rand.Reader)
		server.Raise(err)
		older := func(r *http.Request) {
			url := "https://" + r.Host + r.URL.Path
			r.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(olderKey, r.Method, url, bearer.token, "jti-older", server.NowUtc()))
		}
		replayed := bearer.proof()
		w := self.serveEdited(http.MethodGet, "/auth/refresh", bearer.authorization(), nil, replayed)
		if w.Code != http.StatusOK {
			t.Fatalf("refresh status = %d: %s", w.Code, w.Body.String())
		}

		before := self.snapshot(t)
		for name, edit := range map[string]func(r *http.Request){
			"a copy alone":         nil,
			"a replay":             replayed,
			"an older key's proof": older,
		} {
			for _, request := range []struct {
				method string
				path   string
				body   any
			}{
				{method: http.MethodGet, path: "/auth/refresh"},
				{method: http.MethodPost, path: "/network/auth-client", body: map[string]any{"source_client_id": bearer.clientId, "description": "child"}},
			} {
				w := self.serveEdited(request.method, request.path, bearer.authorization(), request.body, edit)
				if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpProofRefusedMessage {
					t.Fatalf("%s on %s: status = %d body = %q, want the proof refused", name, request.path, w.Code, w.Body.String())
				}
			}
		}
		requireRouteAccessSnapshot(t, before, self.snapshot(t), "a refused proof")
	})
}

// Enforced, api refuses a strict_exp token past its exp with reject_expired off, a
// 401 on every route, the allowlist's included. The sidecar re-enrolls.
func TestApiRefusesAnExpiredStrictExpToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer session.Testing_SetRejectExpired(false)()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		bearer := mintStrictExpBearer(t, self)

		byJwt, err := session.ParseByJwt(ctx, bearer.token)
		server.Raise(err)
		byJwt.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Second))
		expired := byJwt.Testing_Sign()
		bearer.token = expired

		for _, request := range []struct {
			method string
			path   string
			body   any
		}{
			{method: http.MethodGet, path: "/auth/refresh"},
			{method: http.MethodPost, path: "/network/auth-client", body: map[string]any{"source_client_id": bearer.clientId, "description": "child"}},
			{method: http.MethodPost, path: "/network/remove-client", body: map[string]any{"client_id": bearer.clientId}},
		} {
			w := self.serveEdited(request.method, request.path, "Bearer "+expired, request.body, bearer.proof())
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: status = %d body = %q, want 401", request.path, w.Code, w.Body.String())
			}
		}
	})
}

// D-TRUST-3 through the route: a network off preview_networks refuses a strict_exp
// mint, with no sidecar.yml and with one listing another network, and the reply is
// a refusal, not a client without the mark.
func TestNonPreviewNetworkRefusesStrictExpThroughTheRoute(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		for _, config := range []string{"", fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", server.NewId())} {
			func() {
				defer server.Testing_PushSidecarConfig(config)()
				w := self.serve(http.MethodPost, "/network/auth-client", self.root(), map[string]any{"description": "sidecar", "device_spec": "sidecar", "admin_refused": true, "strict_exp": true})
				result := &model.AuthNetworkClientResult{}
				server.Raise(json.Unmarshal(w.Body.Bytes(), result))
				if result.Error == nil || result.ClientId != nil || result.ByClientJwt != nil {
					t.Fatalf("%q: status = %d: %s", config, w.Code, w.Body.String())
				}
			}()
		}
	})
}

// A sidecar token buys nothing, and X-PAYMENT on the one allowlisted route that
// settles it is no way around that: it is refused before anything is settled.
func TestStrictExpBearerCannotPayOnAuthClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(tb, ctx)
		defer server.Testing_PushSidecarConfig(self.previewYaml())()
		bearer := mintStrictExpBearer(t, self)
		controller.SetX402Config(&controller.X402Config{Enabled: true})
		defer controller.SetX402Config(nil)

		proof := bearer.proof()
		w := self.serveEdited(http.MethodPost, "/network/auth-client", bearer.authorization(), map[string]any{"source_client_id": bearer.clientId, "description": "child"}, func(r *http.Request) {
			proof(r)
			r.Header.Set(controller.X402PaymentHeader, "c2lnbmVkLXBheW1lbnQ=")
		})
		if w.Code != http.StatusForbidden || strings.TrimSpace(w.Body.String()) != router.StrictExpCredentialRefusedMessage {
			t.Fatalf("status = %d body = %q, want the sidecar refusal", w.Code, w.Body.String())
		}
	})
}

// server/session/AUTH.md's gap through the routes: an API key creates an auth code,
// the code logs in, the login administers, and POST /account/api-key/remove ends it
// at its next request and refuses the code's remaining uses.
func TestRemovingAnApiKeyEndsItsLoginThroughTheRoutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		var code model.AuthCodeCreateResult
		self.call(t, http.MethodPost, "/auth/code-create", self.key(), map[string]any{"uses": 2, "duration_minutes": 5}, http.StatusOK, &code)
		if code.Error != nil || code.AuthCode == "" {
			t.Fatalf("code-create = %+v", code)
		}
		var login model.AuthCodeLoginResult
		self.call(t, http.MethodPost, "/auth/code-login", "", map[string]any{"auth_code": code.AuthCode}, http.StatusOK, &login)
		if login.Error != nil || login.ByJwt == "" {
			t.Fatalf("code-login = %+v", login)
		}
		byJwt, err := session.ParseByJwt(ctx, login.ByJwt)
		server.Raise(err)
		if byJwt.ApiKeyId == nil || *byJwt.ApiKeyId != self.apiKeyId || byJwt.ClientId != nil {
			t.Fatalf("the login does not name its key: %+v", byJwt)
		}

		self.call(t, http.MethodGet, "/account/api-keys", "Bearer "+login.ByJwt, nil, http.StatusOK, nil)

		self.call(t, http.MethodPost, "/account/api-key/remove", self.root(), map[string]any{"id": self.apiKeyId}, http.StatusOK, nil)

		w := self.serve(http.MethodGet, "/account/api-keys", "Bearer "+login.ByJwt, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("the login after its key's removal: status = %d: %s", w.Code, w.Body.String())
		}

		var refused model.AuthCodeLoginResult
		self.call(t, http.MethodPost, "/auth/code-login", "", map[string]any{"auth_code": code.AuthCode}, http.StatusOK, &refused)
		if refused.Error == nil || refused.ByJwt != "" {
			t.Fatalf("code-login after the removal = %+v", refused)
		}

		// the network's own credential is untouched
		self.call(t, http.MethodGet, "/account/api-keys", self.root(), nil, http.StatusOK, nil)
	})
}
