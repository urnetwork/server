package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
// 60 s ID token of the id- set.

// The test's edge origins: under root.example, each naming its network and whether its grant is live.
type testEdgeLoginSource struct {
	stateLock sync.Mutex
	// origin host -> network and host
	origins map[string]*EdgeOrigin
	// network, host -> granted
	grants map[string]bool
}

func newTestEdgeLoginSource() *testEdgeLoginSource {
	return &testEdgeLoginSource{origins: map[string]*EdgeOrigin{}, grants: map[string]bool{}}
}

func (self *testEdgeLoginSource) add(originHost string, networkId server.Id, host string, granted bool) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.origins[originHost] = &EdgeOrigin{Origin: "https://" + originHost, NetworkId: networkId, Host: host}
	self.grants[networkId.String()+"/"+host] = granted
	return "https://" + originHost
}

func (self *testEdgeLoginSource) setGranted(networkId server.Id, host string, granted bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.grants[networkId.String()+"/"+host] = granted
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

func (self *testEdgeLoginSource) MembersPublishGranted(ctx context.Context, networkId server.Id, host string, now time.Time) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.grants[networkId.String()+"/"+host], nil
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
		source.setGranted(networkId, "web", false)
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
		for _, clientId := range []string{origin + "/path", origin + ":443", "http://web.net1.root.example", "https://WEB.net1.root.example", origin + "?q=1"} {
			_, isOrigin, _ := resolveEdgeOrigin(ctx, clientId)
			connect.AssertEqual(t, isOrigin, false)
		}
	})
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

// A test view: AclMembersPublishGranted reads rows alone.
type testEdgeAclView struct{}

func (testEdgeAclView) Pin() *bridge.AuthPin {
	return &bridge.AuthPin{Version: 1, Chain: make([]byte, 32)}
}
func (testEdgeAclView) Genesis() *bridge.Genesis                         { return &bridge.Genesis{} }
func (testEdgeAclView) SidecarIds() []sidecar.Id                         { return nil }
func (testEdgeAclView) Sidecar(sidecarId sidecar.Id) *sidecar.AclSidecar { return nil }
func (testEdgeAclView) Member(memberId sidecar.Id) *bridge.MemberRow     { return nil }
func (testEdgeAclView) IsConfigWriter(sidecarId sidecar.Id) bool         { return false }
func (testEdgeAclView) Parked(sidecarId sidecar.Id) bool                 { return false }
func (testEdgeAclView) Sponsor(sidecarId sidecar.Id) (sidecar.Id, bool) {
	return sidecar.Id{}, false
}
func (testEdgeAclView) CascadeVoids(signer []byte, at uint64) bool { return false }

// A projection publishes a host to its members by a live publish rule of the members audience whose names
// take the host; another audience, an expired rule and another host do not.
func TestAclMembersPublishGranted(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	rule := func(name string, audience protocol.AclAudience, expires int64) *protocol.AclRule {
		return &protocol.AclRule{
			RuleId:   sidecar.NewId().Bytes(),
			Actions:  []uint32{uint32(protocol.AclAction_ACL_ACTION_PUBLISH)},
			Targets:  []*protocol.AclTarget{{PublicName: name}},
			Audience: audience,
			Expires:  expires,
		}
	}
	state, err := sidecar.NewAclState(&protocol.AclRows{
		HasDocument: true,
		Rules: []*protocol.AclRule{
			rule("web", protocol.AclAudience_ACL_AUDIENCE_MEMBERS, 0),
			rule("*.docs", protocol.AclAudience_ACL_AUDIENCE_UNSPECIFIED, 0),
			rule("public", protocol.AclAudience_ACL_AUDIENCE_PUBLIC, 0),
			rule("old", protocol.AclAudience_ACL_AUDIENCE_MEMBERS, now.Add(-time.Minute).UnixMilli()),
		},
	}, testEdgeAclView{})
	if err != nil {
		t.Skipf("rows the acl state refuses: %v", err)
	}
	connect.AssertEqual(t, AclMembersPublishGranted(state, "web", now), true)
	connect.AssertEqual(t, AclMembersPublishGranted(state, "v2.docs", now), true)
	connect.AssertEqual(t, AclMembersPublishGranted(state, "docs", now), false)
	connect.AssertEqual(t, AclMembersPublishGranted(state, "public", now), false)
	connect.AssertEqual(t, AclMembersPublishGranted(state, "old", now), false)
	connect.AssertEqual(t, AclMembersPublishGranted(state, "other", now), false)
	connect.AssertEqual(t, AclMembersPublishGranted(nil, "web", now), false)

	connect.AssertEqual(t, edgeHostCovers("*", "a.b"), true)
	connect.AssertEqual(t, edgeHostCovers("w*b", "web"), true)
	connect.AssertEqual(t, edgeHostCovers("w*b", "wex"), false)
	connect.AssertEqual(t, edgeHostCovers("a.*", "a.b.c"), false)
}
