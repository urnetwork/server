package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §7.8's edge login (TEAM-M5-01): an origin's grant, its exact callback, the members it signs in, and the
// 60 s ID token of the id- set; and QD-723's origins on tlsedge's other ports, each a client and an audience
// of its own under its own port's grant.

// The test's edge origins: under root.example, each host naming its network, and whether each of its ports'
// grants is live.
type testEdgeLoginSource struct {
	stateLock sync.Mutex
	// origin host -> network and host
	origins map[string]*EdgeOrigin
	// network, host, port -> granted
	grants map[string]bool
}

func newTestEdgeLoginSource() *testEdgeLoginSource {
	return &testEdgeLoginSource{origins: map[string]*EdgeOrigin{}, grants: map[string]bool{}}
}

func testEdgeGrantKey(networkId server.Id, host string, port int) string {
	return fmt.Sprintf("%s/%s/%d", networkId, host, port)
}

// A host under the root with its 443 grant live or not, no other port's granted: its bare origin.
func (self *testEdgeLoginSource) add(originHost string, networkId server.Id, host string, granted bool) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.origins[originHost] = &EdgeOrigin{NetworkId: networkId, Host: host}
	self.grants[testEdgeGrantKey(networkId, host, 443)] = granted
	return "https://" + originHost
}

func (self *testEdgeLoginSource) setGranted(networkId server.Id, host string, port int, granted bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.grants[testEdgeGrantKey(networkId, host, port)] = granted
}

func (self *testEdgeLoginSource) ResolveOrigin(ctx context.Context, originHost string) (server.Id, string, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	origin := self.origins[originHost]
	if origin == nil {
		return server.Id{}, "", false, nil
	}
	return origin.NetworkId, origin.Host, true, nil
}

func (self *testEdgeLoginSource) MembersPublishGranted(ctx context.Context, networkId server.Id, host string, port int, now time.Time) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.grants[testEdgeGrantKey(networkId, host, port)], nil
}

func installTestEdgeLoginSource(t testing.TB) *testEdgeLoginSource {
	t.Helper()
	source := newTestEdgeLoginSource()
	SetEdgeLoginSource(source)
	t.Cleanup(func() { SetEdgeLoginSource(nil) })
	return source
}

// A personal network and its owner, signed in to it.
func testEdgeNetwork(t testing.TB, ctx context.Context) (server.Id, *AuthorizingUser) {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "edge-"+networkId.String(), userId)
	return networkId, &AuthorizingUser{UserId: userId, NetworkId: networkId, NetworkName: "edge", AuthTime: server.NowUtc()}
}

func testEdgeRequest(origin string, nonce string) *AuthorizationRequest {
	return &AuthorizationRequest{
		ClientId:            origin,
		RedirectUri:         origin + EdgeCallbackPath,
		ResponseType:        "code",
		Scopes:              []string{EdgeScopeOpenid},
		State:               "opaque-state",
		CodeChallenge:       testCodeChallenge(),
		CodeChallengeMethod: PkceMethodS256,
		Nonce:               nonce,
	}
}

func testEdgeRedeem(ctx context.Context, origin string, code string) (*EdgeTokenResponse, error) {
	return ExchangeEdgeCode(ctx, &ExchangeCodeArgs{
		Code:         code,
		ClientId:     origin,
		RedirectUri:  origin + EdgeCallbackPath,
		CodeVerifier: testCodeVerifier,
	})
}

// The owner signs in at its network's published origin: the code redeems once for an ID token of the id-
// set, aud the origin, its nonce, the network and the owner as sub, for 60 s; another origin, another nonce
// and a replay after 60 s fail verification.
func TestEdgeLoginSignsInTheNetworksOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		keys := installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		origin := source.add("web.net1.root.example", networkId, "web", true)

		client, err := GetClient(ctx, origin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, client.ClientType, ClientTypeEdge)
		connect.AssertEqual(t, client.RedirectUris, []string{origin + EdgeCallbackPath})

		code, err := Authorize(ctx, testEdgeRequest(origin, "nonce-1"), owner)
		if err != nil {
			t.Fatal(err)
		}
		response, err := testEdgeRedeem(ctx, origin, code)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, response.ExpiresIn, 60)
		now := server.NowUtc()
		claims, err := VerifyEdgeIdToken(response.IdToken, origin, "nonce-1", now)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, claims.Subject, owner.UserId.String())
		connect.AssertEqual(t, claims.NetworkId, networkId)
		connect.AssertEqual(t, claims.Issuer, testIssuer)
		connect.AssertEqual(t, claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time), 60*time.Second)

		_, err = VerifyEdgeIdToken(response.IdToken, "https://other.net1.root.example", "nonce-1", now)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)
		_, err = VerifyEdgeIdToken(response.IdToken, origin, "nonce-2", now)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)
		_, err = VerifyEdgeIdToken(response.IdToken, origin, "nonce-1", now.Add(61*time.Second))
		connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)
		// a token of another set is never one of these
		mcpToken, err := MintIdToken(&MintIdTokenArgs{UserId: owner.UserId, NetworkId: networkId, ClientId: origin, Nonce: "nonce-1"})
		connect.AssertEqual(t, err, nil)
		_, err = VerifyEdgeIdToken(mcpToken, origin, "nonce-1", now)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)

		// the code is spent
		_, err = testEdgeRedeem(ctx, origin, code)
		connect.AssertEqual(t, err != nil, true)

		// the id- set's keys, apart from the mcp set's
		recorder := httptest.NewRecorder()
		EdgeJwksHandler(recorder, httptest.NewRequest(http.MethodGet, "/.well-known/ur/id-jwks.json", nil))
		connect.AssertEqual(t, recorder.Code, http.StatusOK)
		jwks := recorder.Body.String()
		connect.AssertEqual(t, strings.Contains(jwks, keys.id.Kid), true)
		connect.AssertEqual(t, strings.Contains(jwks, keys.mcp.Kid), false)
	})
}

// An origin with no grant is no client, and one whose grant ends between the sign-in and the redemption
// redeems nothing; an origin no source resolves is no edge origin at all.
func TestEdgeLoginRefusesAnOriginWithNoGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		ungranted := source.add("draft.net1.root.example", networkId, "draft", false)
		_, err := GetClient(ctx, ungranted)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
		_, err = Authorize(ctx, testEdgeRequest(ungranted, "nonce"), owner)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)

		origin := source.add("web.net1.root.example", networkId, "web", true)
		code, err := Authorize(ctx, testEdgeRequest(origin, "nonce"), owner)
		if err != nil {
			t.Fatal(err)
		}
		source.setGranted(networkId, "web", 443, false)
		_, err = testEdgeRedeem(ctx, origin, code)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)

		// no source installed: every edge origin refused, never treated as a client of its own
		SetEdgeLoginSource(nil)
		_, err = AuthorizeEdgeOrigin(ctx, testEdgeRequest(origin, "nonce"), owner)
		connect.AssertEqual(t, err != nil, true)
	})
}

// Only a member of the origin's network signs in: another network's owner, the network's other users and a
// team network's visitors are refused.
func TestEdgeLoginRefusesAnotherNetworksMember(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		_, otherOwner := testEdgeNetwork(t, ctx)
		origin := source.add("web.net1.root.example", networkId, "web", true)

		_, err := Authorize(ctx, testEdgeRequest(origin, "nonce"), otherOwner)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeVisitorRefused), true)
		stranger := *owner
		stranger.UserId = server.NewId()
		_, err = Authorize(ctx, testEdgeRequest(origin, "nonce"), &stranger)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeVisitorRefused), true)
		guest := *owner
		guest.GuestMode = true
		_, err = Authorize(ctx, testEdgeRequest(origin, "nonce"), &guest)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeVisitorRefused), true)
	})
}

// The callback is exact, and the request is openid with PKCE S256 and a nonce, nothing else.
func TestEdgeLoginRedirectIsExact(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		origin := source.add("web.net1.root.example", networkId, "web", true)

		for _, redirectUri := range []string{
			origin + "/.well-known/ur/callback/",
			origin + EdgeCallbackPath + "?next=1",
			"https://web.net1.root.example:8443" + EdgeCallbackPath,
			"https://evil.example" + EdgeCallbackPath,
			origin + "/callback",
		} {
			request := testEdgeRequest(origin, "nonce")
			request.RedirectUri = redirectUri
			_, err := Authorize(ctx, request, owner)
			connect.AssertEqual(t, errors.Is(err, ErrInvalidRedirect), true)
		}
		for _, mutate := range []func(request *AuthorizationRequest){
			func(request *AuthorizationRequest) { request.Nonce = "" },
			func(request *AuthorizationRequest) { request.CodeChallenge = "" },
			func(request *AuthorizationRequest) { request.Scopes = []string{EdgeScopeOpenid, "mcp:read"} },
			func(request *AuthorizationRequest) { request.Resource = "https://mcp.example" },
		} {
			request := testEdgeRequest(origin, "nonce")
			mutate(request)
			_, err := Authorize(ctx, request, owner)
			connect.AssertEqual(t, errors.Is(err, ErrInvalidRequest), true)
		}
		// a client_id that is no bare origin is no edge origin
		for _, clientId := range []string{
			origin + "/path", origin + "/", origin + ":8443/", origin + ":8443/path", "http://web.net1.root.example", "http://web.net1.root.example:8443",
			"https://WEB.net1.root.example", "https://WEB.net1.root.example:8443", origin + "?q=1", origin + ":8443?q=1", origin + "#f",
			"https://user@web.net1.root.example:8443", origin + ":port", "https://[::1]:8443",
		} {
			_, isOrigin, _ := resolveEdgeOrigin(ctx, clientId)
			connect.AssertEqual(t, isOrigin, false)
		}
	})
}

// QD-723: a members endpoint on 8443 signs its network's owner in end to end as its own origin, the port
// written in client_id, redirect_uri and aud. The host's 443 endpoint is another client: neither one's code
// redeems as the other, neither one's callback is the other's, and neither one's ID token verifies for the
// other.
func TestEdgeLoginSignsInOnAPortedOrigin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		bare := source.add("web.net1.root.example", networkId, "web", true)
		ported := bare + ":8443"
		source.setGranted(networkId, "web", 8443, true)

		client, err := GetClient(ctx, ported)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, client.ClientType, ClientTypeEdge)
		connect.AssertEqual(t, client.ClientId, ported)
		connect.AssertEqual(t, client.RedirectUris, []string{ported + EdgeCallbackPath})
		origin, isOrigin, err := resolveEdgeOrigin(ctx, ported)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, isOrigin, true)
		connect.AssertEqual(t, *origin, EdgeOrigin{Origin: ported, NetworkId: networkId, Host: "web", Port: 8443})
		origin, _, err = resolveEdgeOrigin(ctx, bare)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *origin, EdgeOrigin{Origin: bare, NetworkId: networkId, Host: "web", Port: 443})

		code, err := Authorize(ctx, testEdgeRequest(ported, "nonce-p"), owner)
		if err != nil {
			t.Fatal(err)
		}
		response, err := testEdgeRedeem(ctx, ported, code)
		if err != nil {
			t.Fatal(err)
		}
		now := server.NowUtc()
		claims, err := VerifyEdgeIdToken(response.IdToken, ported, "nonce-p", now)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, []string(claims.Audience), []string{ported})
		connect.AssertEqual(t, claims.Subject, owner.UserId.String())
		connect.AssertEqual(t, claims.NetworkId, networkId)
		// 8443's token on 443, and 443's on 8443
		_, err = VerifyEdgeIdToken(response.IdToken, bare, "nonce-p", now)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)
		bareCode, err := Authorize(ctx, testEdgeRequest(bare, "nonce-p"), owner)
		if err != nil {
			t.Fatal(err)
		}
		bareResponse, err := testEdgeRedeem(ctx, bare, bareCode)
		if err != nil {
			t.Fatal(err)
		}
		bareClaims, err := VerifyEdgeIdToken(bareResponse.IdToken, bare, "nonce-p", now)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, []string(bareClaims.Audience), []string{bare})
		_, err = VerifyEdgeIdToken(bareResponse.IdToken, ported, "nonce-p", now)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)

		// the token route takes the ported origin as it does the bare one (the front's redemption), and
		// refuses 443 written out before it reads the code
		redeemForm := func(clientId string, redirectUri string, code string) (int, string) {
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {clientId},
				"code":          {code},
				"redirect_uri":  {redirectUri},
				"code_verifier": {testCodeVerifier},
			}
			request := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode())).WithContext(ctx)
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			recorder := httptest.NewRecorder()
			TokenHandler(recorder, request)
			return recorder.Code, recorder.Body.String()
		}
		formCode, err := Authorize(ctx, testEdgeRequest(ported, "nonce-f"), owner)
		if err != nil {
			t.Fatal(err)
		}
		status, body := redeemForm(bare+":443", ported+EdgeCallbackPath, formCode)
		connect.AssertEqual(t, status, http.StatusBadRequest)
		connect.AssertEqual(t, strings.Contains(body, "id_token"), false)
		status, body = redeemForm(ported, ported+EdgeCallbackPath, formCode)
		connect.AssertEqual(t, status, http.StatusOK)
		formResponse := &EdgeTokenResponse{}
		if err := json.Unmarshal([]byte(body), formResponse); err != nil {
			t.Fatal(err)
		}
		formClaims, err := VerifyEdgeIdToken(formResponse.IdToken, ported, "nonce-f", server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, []string(formClaims.Audience), []string{ported})

		// each one's callback is its own
		for _, c := range []struct {
			origin      string
			otherOrigin string
		}{{origin: ported, otherOrigin: bare}, {origin: bare, otherOrigin: ported}} {
			request := testEdgeRequest(c.origin, "nonce")
			request.RedirectUri = c.otherOrigin + EdgeCallbackPath
			_, err := Authorize(ctx, request, owner)
			connect.AssertEqual(t, errors.Is(err, ErrInvalidRedirect), true)

			// and its code redeems as itself alone: not as the other origin, with either callback
			code, err := Authorize(ctx, testEdgeRequest(c.origin, "nonce"), owner)
			if err != nil {
				t.Fatal(err)
			}
			_, err = testEdgeRedeem(ctx, c.otherOrigin, code)
			connect.AssertEqual(t, err != nil, true)
			code, err = Authorize(ctx, testEdgeRequest(c.origin, "nonce"), owner)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ExchangeEdgeCode(ctx, &ExchangeCodeArgs{Code: code, ClientId: c.otherOrigin, RedirectUri: c.origin + EdgeCallbackPath, CodeVerifier: testCodeVerifier})
			connect.AssertEqual(t, err != nil, true)
			code, err = Authorize(ctx, testEdgeRequest(c.origin, "nonce"), owner)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ExchangeEdgeCode(ctx, &ExchangeCodeArgs{Code: code, ClientId: c.origin, RedirectUri: c.otherOrigin + EdgeCallbackPath, CodeVerifier: testCodeVerifier})
			connect.AssertEqual(t, errors.Is(err, ErrInvalidRedirect), true)
		}
	})
}

// QD-723: under a root this server serves, only the four tlsedge ports are origins, 443 bare and never
// written: 443 written out, a port outside the four and a port in another spelling are refused as edge
// origins, never passed on to be fetched as a client metadata document, whatever grants the host holds. Under
// no such root a ported origin is no edge origin at all.
func TestEdgeLoginRefusesAPortOutsideTheFour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		bare := source.add("web.net1.root.example", networkId, "web", true)
		for _, port := range []int{80, 8080, 8443, 8883, 5671, 9443, 25, 65535} {
			source.setGranted(networkId, "web", port, true)
		}

		for _, port := range []string{"8443", "8883", "5671"} {
			origin, isOrigin, err := resolveEdgeOrigin(ctx, bare+":"+port)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, isOrigin, true)
			connect.AssertEqual(t, origin.Origin, bare+":"+port)
			connect.AssertEqual(t, fmt.Sprint(origin.Port), port)
			_, err = Authorize(ctx, testEdgeRequest(bare+":"+port, "nonce"), owner)
			connect.AssertEqual(t, err, nil)
		}
		for _, port := range []string{"443", "80", "8080", "9443", "25", "65535", "0", "08443", "0443", "", "99999"} {
			clientId := bare + ":" + port
			origin, isOrigin, err := resolveEdgeOrigin(ctx, clientId)
			connect.AssertEqual(t, origin == nil, true)
			connect.AssertEqual(t, isOrigin, true)
			connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
			_, err = GetClient(ctx, clientId)
			connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
			_, err = Authorize(ctx, testEdgeRequest(clientId, "nonce"), owner)
			connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
			_, err = testEdgeRedeem(ctx, clientId, "code")
			connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
		}
		// a code of the bare origin never redeems under 443 written out
		code, err := Authorize(ctx, testEdgeRequest(bare, "nonce"), owner)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ExchangeEdgeCode(ctx, &ExchangeCodeArgs{Code: code, ClientId: bare + ":443", RedirectUri: bare + EdgeCallbackPath, CodeVerifier: testCodeVerifier})
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)

		// under no root this server serves
		for _, clientId := range []string{"https://web.other.example:8443", "https://web.other.example:9443", "https://web.other.example:443"} {
			_, isOrigin, err := resolveEdgeOrigin(ctx, clientId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, isOrigin, false)
		}
		// with no source installed nothing under a ported client id is an origin this binary can judge
		SetEdgeLoginSource(nil)
		_, err = AuthorizeEdgeOrigin(ctx, testEdgeRequest(bare+":8443", "nonce"), owner)
		connect.AssertEqual(t, err != nil, true)
	})
}

// QD-723: the grant is the port's own. A host granted on 443 alone refuses its 8443 origin and the reverse,
// and a port's grant ending between the sign-in and the redemption redeems nothing while the other port's
// stands.
func TestEdgeLoginGrantIsPerPort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		installTestSignerKeys(t)
		source := installTestEdgeLoginSource(t)
		networkId, owner := testEdgeNetwork(t, ctx)
		bare := source.add("web.net1.root.example", networkId, "web", true)
		ported := bare + ":8443"

		_, err := GetClient(ctx, ported)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
		_, err = Authorize(ctx, testEdgeRequest(ported, "nonce"), owner)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
		_, err = Authorize(ctx, testEdgeRequest(bare, "nonce"), owner)
		connect.AssertEqual(t, err, nil)

		// 8443 alone
		source.setGranted(networkId, "web", 8443, true)
		source.setGranted(networkId, "web", 443, false)
		_, err = Authorize(ctx, testEdgeRequest(bare, "nonce"), owner)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
		code, err := Authorize(ctx, testEdgeRequest(ported, "nonce"), owner)
		if err != nil {
			t.Fatal(err)
		}

		// 8443's ends before its code redeems, 443's back
		source.setGranted(networkId, "web", 8443, false)
		source.setGranted(networkId, "web", 443, true)
		_, err = testEdgeRedeem(ctx, ported, code)
		connect.AssertEqual(t, errors.Is(err, ErrEdgeOriginRefused), true)
	})
}

// The ID token's life is the settings' (QD-702), 60 s by default.
func TestEdgeLoginIdTokenDurationIsASetting(t *testing.T) {
	installTestSignerKeys(t)
	connect.AssertEqual(t, DefaultEdgeLoginSettings().IdTokenDuration, 60*time.Second)
	SetEdgeLoginSettings(&EdgeLoginSettings{IdTokenDuration: 30 * time.Second})
	t.Cleanup(func() { SetEdgeLoginSettings(nil) })
	origin := "https://web.net1.root.example:8443"
	now := time.Now().UTC().Truncate(time.Second)
	token, err := MintEdgeIdToken(origin, server.NewId(), server.NewId(), "nonce", now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := VerifyEdgeIdToken(token, origin, "nonce", now.Add(29*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time), 30*time.Second)
	_, err = VerifyEdgeIdToken(token, origin, "nonce", now.Add(31*time.Second))
	connect.AssertEqual(t, errors.Is(err, ErrEdgeIdTokenInvalid), true)
}

// GET /oauth/authorize forwards a visitor to the advertised consent page with the query unchanged.
func TestEdgeAuthorizeRedirectKeepsTheQuery(t *testing.T) {
	installTestIssuer(t)
	recorder := httptest.NewRecorder()
	query := "client_id=https%3A%2F%2Fweb.net1.root.example&scope=openid&state=s&nonce=n"
	AuthorizeRedirectHandler(recorder, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query, nil))
	connect.AssertEqual(t, recorder.Code, http.StatusFound)
	connect.AssertEqual(t, recorder.Header().Get("Location"), testIssuer+"/authorize?"+query)
}

// A projection's facts for AclMembersPublishGranted: one owner member's admitted sidecars.
type testEdgeAclView struct {
	memberId sidecar.Id
	sidecars map[sidecar.Id]*sidecar.AclSidecar
	parked   map[sidecar.Id]bool
}

func (self *testEdgeAclView) Pin() *bridge.AuthPin {
	return &bridge.AuthPin{Version: 1, Chain: make([]byte, 32)}
}
func (self *testEdgeAclView) Genesis() *bridge.Genesis { return nil }
func (self *testEdgeAclView) SidecarIds() []sidecar.Id {
	sidecarIds := []sidecar.Id{}
	for sidecarId := range self.sidecars {
		sidecarIds = append(sidecarIds, sidecarId)
	}
	return sidecarIds
}
func (self *testEdgeAclView) Sidecar(sidecarId sidecar.Id) *sidecar.AclSidecar {
	return self.sidecars[sidecarId]
}
func (self *testEdgeAclView) Member(memberId sidecar.Id) *bridge.MemberRow {
	if memberId != self.memberId {
		return nil
	}
	return &bridge.MemberRow{MemberId: memberId.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_OWNER}
}
func (self *testEdgeAclView) IsConfigWriter(sidecarId sidecar.Id) bool { return false }
func (self *testEdgeAclView) Parked(sidecarId sidecar.Id) bool         { return self.parked[sidecarId] }
func (self *testEdgeAclView) Sponsor(sidecarId sidecar.Id) (sidecar.Id, bool) {
	return sidecar.Id{}, false
}
func (self *testEdgeAclView) CascadeVoids(signer []byte, at uint64) bool { return false }

// QD-723: a projection publishes tls/<port>/<host> to its members where some admitted sidecar holds
// AclState.PublishGrant for it, http and of the members audience. A rule binding 443's socket grants nothing on
// 8443 and the reverse; a host-only rule takes the port its sidecar may listen on; another audience, an
// expired rule, a publish with no listen, a parked sidecar, a sidecar the state does not admit and a port
// outside the four give none.
func TestAclMembersPublishGranted(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	view := &testEdgeAclView{memberId: sidecar.NewId(), sidecars: map[sidecar.Id]*sidecar.AclSidecar{}, parked: map[sidecar.Id]bool{}}
	rows := &protocol.AclRows{HasDocument: true}
	position := uint64(0)
	device := func() (sidecar.Id, []byte) {
		position += 1
		sidecarId := sidecar.NewId()
		view.sidecars[sidecarId] = &sidecar.AclSidecar{
			Row:      &bridge.SidecarRow{SidecarId: sidecarId.Bytes(), AdmittedAt: position},
			MemberId: view.memberId,
		}
		atom := sidecar.NewId().Bytes()
		rows.Atoms = append(rows.Atoms, &protocol.AclAtom{Atom: atom, SidecarIds: [][]byte{sidecarId.Bytes()}})
		return sidecarId, atom
	}
	rule := func(atom []byte, audience protocol.AclAudience, expires int64, actions []protocol.AclAction, names ...string) {
		row := &protocol.AclRule{RuleId: sidecar.NewId().Bytes(), SrcAtoms: [][]byte{atom}, Audience: audience, Expires: expires}
		for _, action := range actions {
			row.Actions = append(row.Actions, uint32(action))
		}
		for _, name := range names {
			row.Targets = append(row.Targets, &protocol.AclTarget{PublicName: name})
		}
		rows.Rules = append(rows.Rules, row)
	}
	publish, listen := protocol.AclAction_ACL_ACTION_PUBLISH, protocol.AclAction_ACL_ACTION_LISTEN
	both := []protocol.AclAction{publish, listen}
	members, public := protocol.AclAudience_ACL_AUDIENCE_MEMBERS, protocol.AclAudience_ACL_AUDIENCE_PUBLIC

	_, listener := device()
	// bound to one port's socket each
	rule(listener, members, 0, both, "web", "tls/443/web")
	rule(listener, protocol.AclAudience_ACL_AUDIENCE_UNSPECIFIED, 0, both, "api", "tls/8443/api")
	rule(listener, protocol.AclAudience_ACL_AUDIENCE_MEMBERS_DEVICE, 0, both, "mq", "tls/8883/mq")
	// a host-only publish, its listen from a rule of its own on 8443 alone
	rule(listener, members, 0, []protocol.AclAction{publish}, "split")
	rule(listener, 0, 0, []protocol.AclAction{listen}, "tls/8443/split")
	// a wildcard host on every port's socket
	rule(listener, members, 0, both, "*.docs", "tls/*/*.docs")
	rule(listener, public, 0, both, "public", "tls/443/public", "tls/8443/public")
	rule(listener, members, now.Add(-time.Minute).UnixMilli(), both, "old", "tls/443/old")
	rule(listener, members, 0, []protocol.AclAction{publish}, "no-listen")
	parkedId, parked := device()
	view.parked[parkedId] = true
	rule(parked, members, 0, both, "parked", "tls/443/parked")
	// a rule of a sidecar the state does not admit
	strangerAtom := sidecar.NewId().Bytes()
	rows.Atoms = append(rows.Atoms, &protocol.AclAtom{Atom: strangerAtom, SidecarIds: [][]byte{sidecar.NewId().Bytes()}})
	rule(strangerAtom, members, 0, both, "stranger", "tls/443/stranger")

	state, err := sidecar.NewAclState(rows, view)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host    string
		port    int
		granted bool
	}{
		{host: "web", port: 443, granted: true},
		{host: "web", port: 8443},
		{host: "web", port: 8883},
		{host: "web", port: 5671},
		{host: "api", port: 8443, granted: true},
		{host: "api", port: 443},
		{host: "mq", port: 8883, granted: true},
		{host: "mq", port: 443},
		{host: "split", port: 8443, granted: true},
		{host: "split", port: 443},
		{host: "v2.docs", port: 443, granted: true},
		{host: "v2.docs", port: 8443, granted: true},
		{host: "v2.docs", port: 5671, granted: true},
		{host: "docs", port: 443},
		{host: "public", port: 443},
		{host: "public", port: 8443},
		{host: "old", port: 443},
		{host: "no-listen", port: 443},
		{host: "parked", port: 443},
		{host: "stranger", port: 443},
		{host: "other", port: 443},
		// no tlsedge port, whatever the rules take
		{host: "v2.docs", port: 9443},
		{host: "v2.docs", port: 0},
		{host: "v2.docs", port: -443},
		{host: "", port: 443},
		{host: "*.docs", port: 443},
	} {
		if granted := AclMembersPublishGranted(state, c.host, c.port, now); granted != c.granted {
			t.Errorf("%s on %d: granted %t", c.host, c.port, granted)
		}
	}
	connect.AssertEqual(t, AclMembersPublishGranted(nil, "web", 443, now), false)
}
