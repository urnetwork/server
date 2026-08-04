package oauth

// Coverage for the grant flows against a real database: the code exchange with
// pkce, refresh rotation and reuse detection, consent, and redirect matching.
//
// These are the paths where a mistake is a security bug rather than a bug, so
// the negative cases matter as much as the happy ones.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

const (
	testResource     = "https://mcp.bringyour.com"
	testClientId     = "urn_client_test"
	testRedirectUri  = "https://claude.ai/api/mcp/auth_callback"
	testCodeVerifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
)

func testCodeChallenge() string {
	sum := sha256.Sum256([]byte(testCodeVerifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Registers a client directly, bypassing cimd, so the grant tests do not need
// an http server.
func createTestClient(t testing.TB, ctx context.Context, applicationType string, redirectUris []string) *Client {
	client := &Client{
		ClientId:        testClientId + "_" + server.NewId().String(),
		ClientType:      ClientTypePreregistered,
		ClientName:      "Test Client",
		ApplicationType: applicationType,
		RedirectUris:    redirectUris,
	}
	saveClient(ctx, client, server.NowUtc().Add(AccessTokenDuration))

	loaded, err := GetClient(ctx, client.ClientId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, loaded.ClientId, client.ClientId)

	return client
}

func testAuthorizingUser(t testing.TB, ctx context.Context) *AuthorizingUser {
	return &AuthorizingUser{
		UserId:      server.NewId(),
		NetworkId:   server.NewId(),
		NetworkName: "testnetwork",
		Principal:   "user@example.com",
		AuthTime:    server.NowUtc(),
	}
}

func testAuthorizationRequest(client *Client, scopes []string) *AuthorizationRequest {
	return &AuthorizationRequest{
		ClientId:            client.ClientId,
		RedirectUri:         client.RedirectUris[0],
		ResponseType:        "code",
		Scopes:              scopes,
		State:               "opaque-state",
		CodeChallenge:       testCodeChallenge(),
		CodeChallengeMethod: PkceMethodS256,
		Resource:            testResource,
		Nonce:               "n-0S6_WzA2Mj",
	}
}

func TestAuthorizationCodeExchange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{
			ScopeOpenid, ScopeOfflineAccess, ScopeMcpRead, ScopeMcpFetch,
		})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		response, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, response.TokenType, "Bearer")
		// offline_access was granted, so a refresh token comes back
		connect.AssertEqual(t, response.RefreshToken != "", true)
		// openid was granted, so an id token does too
		connect.AssertEqual(t, response.IdToken != "", true)

		// the access token is bound to the resource that was consented to
		claims, err := VerifyAccessToken(response.AccessToken, testResource)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Subject, user.UserId.String())
		connect.AssertEqual(t, claims.NetworkId, user.NetworkId)
		connect.AssertEqual(t, claims.Principal, "user@example.com")

		idClaims, err := VerifyIdToken(response.IdToken, client.ClientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, idClaims.Nonce, "n-0S6_WzA2Mj")

		// a code is single use: the replay must not mint a second token
		_, err = ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestAuthorizationCodeRequiresPkce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		// the wrong verifier must not redeem the code
		_, err = ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: "wrong-verifier-wrong-verifier-wrong-verifier",
			Resource:     testResource,
		})
		connect.AssertEqual(t, err != nil, true)

		// oauth 2.1 requires a challenge on the authorization request at all
		noPkce := testAuthorizationRequest(client, []string{ScopeMcpRead})
		noPkce.CodeChallenge = ""
		_, err = Authorize(ctx, noPkce, user)
		connect.AssertEqual(t, err != nil, true)

		// and only S256
		plainPkce := testAuthorizationRequest(client, []string{ScopeMcpRead})
		plainPkce.CodeChallengeMethod = "plain"
		_, err = Authorize(ctx, plainPkce, user)
		connect.AssertEqual(t, err != nil, true)
	})
}

// The audience is what stops a token for one resource being used at another,
// so the code must not be redeemable for a different resource than consented.
func TestAuthorizationCodeBindsResource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		_, err = ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     "https://other.bringyour.com",
		})
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestAuthorizationCodeBindsClientAndRedirect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		other := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		// another client must not redeem this code
		_, err = ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     other.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err != nil, true)

		// nor a different redirect uri
		_, err = ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  "https://evil.example.com/callback",
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestRefreshRotationAndReuseDetection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeOfflineAccess, ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		first, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, first.RefreshToken != "", true)

		// a refresh rotates: the successor differs from what was presented
		second, err := Refresh(ctx, &RefreshArgs{
			RefreshToken: first.RefreshToken,
			ClientId:     client.ClientId,
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, second.RefreshToken != "", true)
		connect.AssertEqual(t, second.RefreshToken != first.RefreshToken, true)

		// reusing the retired token is treated as theft
		_, err = Refresh(ctx, &RefreshArgs{
			RefreshToken: first.RefreshToken,
			ClientId:     client.ClientId,
		})
		connect.AssertEqual(t, err != nil, true)

		// and it takes the whole family with it, including the token that was
		// legitimately issued -- the point is that a stolen copy cannot keep
		// working just because the thief refreshed first
		_, err = Refresh(ctx, &RefreshArgs{
			RefreshToken: second.RefreshToken,
			ClientId:     client.ClientId,
		})
		connect.AssertEqual(t, err != nil, true)
	})
}

// A refresh may narrow scope but never widen it.
func TestRefreshCannotWidenScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeOfflineAccess, ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		first, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err, nil)

		// mcp:fetch was never granted, so it cannot be acquired by refreshing
		_, err = Refresh(ctx, &RefreshArgs{
			RefreshToken: first.RefreshToken,
			ClientId:     client.ClientId,
			Scopes:       []string{ScopeMcpFetch},
		})
		connect.AssertEqual(t, err != nil, true)

		// narrowing is allowed
		narrowed, err := Refresh(ctx, &RefreshArgs{
			RefreshToken: first.RefreshToken,
			ClientId:     client.ClientId,
			Scopes:       []string{ScopeMcpRead},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, narrowed.Scope, ScopeMcpRead)
	})
}

func TestGuestNetworkCannotAuthorize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		user.GuestMode = true

		_, err := Authorize(ctx, testAuthorizationRequest(client, []string{ScopeMcpRead}), user)
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestConsentIsRememberedAndRevocable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)

		// nothing approved yet, so the screen is required
		connect.AssertEqual(t, ConsentSatisfies(ctx, user.UserId, client.ClientId, []string{ScopeMcpRead}), false)

		_, err := Authorize(ctx, testAuthorizationRequest(client, []string{ScopeMcpRead}), user)
		connect.AssertEqual(t, err, nil)

		// the same scope no longer prompts
		connect.AssertEqual(t, ConsentSatisfies(ctx, user.UserId, client.ClientId, []string{ScopeMcpRead}), true)
		// but a new one does
		connect.AssertEqual(t, ConsentSatisfies(ctx, user.UserId, client.ClientId, []string{ScopeMcpFetch}), false)

		// approving the new scope unions rather than replaces, so the client
		// does not silently lose what it already had
		_, err = Authorize(ctx, testAuthorizationRequest(client, []string{ScopeMcpFetch}), user)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ConsentSatisfies(ctx, user.UserId, client.ClientId, []string{ScopeMcpRead, ScopeMcpFetch}), true)

		// disconnecting drops the consent
		RevokeConsent(ctx, user.UserId, client.ClientId)
		connect.AssertEqual(t, ConsentSatisfies(ctx, user.UserId, client.ClientId, []string{ScopeMcpRead}), false)
	})
}

// Disconnecting must also kill the refresh tokens, or the client keeps working.
func TestRevokeConsentRevokesRefreshTokens(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeOfflineAccess, ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		response, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err, nil)

		RevokeConsent(ctx, user.UserId, client.ClientId)

		_, err = Refresh(ctx, &RefreshArgs{
			RefreshToken: response.RefreshToken,
			ClientId:     client.ClientId,
		})
		connect.AssertEqual(t, err != nil, true)
	})
}

// Exact matching, except for the loopback port a native client cannot reserve
// in advance (rfc 8252).
func TestRedirectUriMatching(t *testing.T) {
	web := &Client{
		ApplicationType: ApplicationTypeWeb,
		RedirectUris:    []string{"https://claude.ai/callback"},
	}
	connect.AssertEqual(t, web.ValidRedirectUri("https://claude.ai/callback"), true)
	connect.AssertEqual(t, web.ValidRedirectUri("https://claude.ai/callback/extra"), false)
	connect.AssertEqual(t, web.ValidRedirectUri("https://claude.ai.evil.com/callback"), false)
	// a web client gets no loopback relaxation
	connect.AssertEqual(t, web.ValidRedirectUri("http://127.0.0.1:1234/callback"), false)

	native := &Client{
		ApplicationType: ApplicationTypeNative,
		RedirectUris:    []string{"http://127.0.0.1:1234/callback"},
	}
	// the port may differ, because it is chosen at run time
	connect.AssertEqual(t, native.ValidRedirectUri("http://127.0.0.1:55123/callback"), true)
	// but nothing else may
	connect.AssertEqual(t, native.ValidRedirectUri("http://127.0.0.1:55123/other"), false)
	connect.AssertEqual(t, native.ValidRedirectUri("http://evil.example.com:55123/callback"), false)
}

func TestRegisterClientRejectsUnsafeRedirects(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a web client may not redirect to loopback: that is how a code gets
		// handed to something listening locally
		_, err := RegisterClient(ctx, &clientMetadataDocument{
			ApplicationType: ApplicationTypeWeb,
			RedirectUris:    []string{"http://127.0.0.1:1234/callback"},
		})
		connect.AssertEqual(t, err != nil, true)

		// nor to plain http
		_, err = RegisterClient(ctx, &clientMetadataDocument{
			ApplicationType: ApplicationTypeWeb,
			RedirectUris:    []string{"http://example.com/callback"},
		})
		connect.AssertEqual(t, err != nil, true)

		// a native client may
		client, err := RegisterClient(ctx, &clientMetadataDocument{
			ApplicationType: ApplicationTypeNative,
			RedirectUris:    []string{"http://127.0.0.1:1234/callback"},
		})
		connect.AssertEqual(t, err, nil)
		// a dynamically registered id must not look like a cimd url
		connect.AssertEqual(t, isCimdClientId(client.ClientId), false)
	})
}

// The client id is caller supplied and gets fetched, so the ssrf guard is
// load-bearing.
func TestCimdRefusesPrivateTargets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		refused := []string{
			"https://127.0.0.1/client.json",
			"https://10.1.2.3/client.json",
			"https://192.168.1.1/client.json",
			"https://169.254.169.254/client.json",
			// not https
			"http://example.com/client.json",
		}

		for _, clientId := range refused {
			_, err := GetClient(ctx, clientId)
			if err == nil {
				t.Errorf("expected %s to be refused", clientId)
			}
		}
	})
}

func TestCanonicalResource(t *testing.T) {
	canonical, err := CanonicalResource("https://mcp.bringyour.com/")
	connect.AssertEqual(t, err, nil)
	// the trailing slash is dropped so it matches the minted audience
	connect.AssertEqual(t, canonical, "https://mcp.bringyour.com")

	_, err = CanonicalResource("https://mcp.bringyour.com#frag")
	connect.AssertEqual(t, err != nil, true)

	_, err = CanonicalResource("mcp.bringyour.com")
	connect.AssertEqual(t, err != nil, true)
}

func TestServerMetadataIsSpecCompliant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		metadata := ServerMetadata()

		connect.AssertEqual(t, metadata.Issuer, Issuer())
		// rfc 9207: clients key their mix-up defense on this
		connect.AssertEqual(t, metadata.AuthorizationResponseIssParameterSupported, true)
		connect.AssertEqual(t, metadata.ClientIdMetadataDocumentSupported, true)
		connect.AssertEqual(t, metadata.ResourceIndicatorsSupported, true)
		// oauth 2.1: no implicit grant, and S256 only
		connect.AssertEqual(t, len(metadata.ResponseTypesSupported), 1)
		connect.AssertEqual(t, metadata.ResponseTypesSupported[0], "code")
		connect.AssertEqual(t, len(metadata.CodeChallengeMethodsSupported), 1)
		connect.AssertEqual(t, metadata.CodeChallengeMethodsSupported[0], PkceMethodS256)
		// the issuer is compared verbatim, so it must not carry a trailing slash
		connect.AssertEqual(t, metadata.Issuer[len(metadata.Issuer)-1:] != "/", true)
	})
}

// Userinfo takes whatever access token the client already holds, whose
// audience is the resource it was minted for. Constraining the audience there
// made the endpoint unreachable for every real client.
func TestUserinfoAcceptsAResourceToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeOpenid, ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		response, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err, nil)

		// the token's audience is the mcp resource, not the issuer
		claims, err := VerifyAccessTokenAnyResource(response.AccessToken)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Subject, user.UserId.String())
		connect.AssertEqual(t, claims.Audience[0], testResource)

		// and the audience-checked path still refuses it for another resource,
		// so relaxing userinfo did not relax resource servers
		_, err = VerifyAccessToken(response.AccessToken, "https://other.bringyour.com")
		connect.AssertEqual(t, err != nil, true)

		// a token this issuer did not mint is still refused
		_, err = VerifyAccessTokenAnyResource("not.a.token")
		connect.AssertEqual(t, err != nil, true)
	})
}

// Dynamic registration is openly writable, so it is metered per caller ip.
func TestRegistrationIsRateLimited(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// a caller address outside the infrastructure exclusion, so the limit
		// actually applies
		clientAddress := "203.0.113.7:44001"

		allowed := 0
		for range rateLimitSettings.RegistrationBurstCount + 5 {
			if AllowRegistration(ctx, clientAddress) {
				allowed += 1
			}
		}
		// the burst is allowed, the excess is not
		connect.AssertEqual(t, allowed, rateLimitSettings.RegistrationBurstCount)

		// a different caller has its own budget, so one abuser cannot deny
		// registration to everyone else
		connect.AssertEqual(t, AllowRegistration(ctx, "203.0.113.8:44002"), true)

		// an unparseable caller address cannot be metered, so it is refused
		// rather than becoming an unmetered path
		connect.AssertEqual(t, AllowRegistration(ctx, "not-an-address"), false)
	})
}

// The reaper removes only what has expired; live grants must survive it.
func TestReapRemovesOnlyExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		client := createTestClient(t, ctx, ApplicationTypeWeb, []string{testRedirectUri})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeOfflineAccess, ScopeMcpRead})

		code, err := Authorize(ctx, request, user)
		connect.AssertEqual(t, err, nil)

		response, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  request.RedirectUri,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err, nil)

		// a sweep now must not touch a refresh token that is still valid
		ReapOauthTokens(ctx)

		_, err = Refresh(ctx, &RefreshArgs{
			RefreshToken: response.RefreshToken,
			ClientId:     client.ClientId,
		})
		connect.AssertEqual(t, err, nil)

		// expire everything this grant owns, then sweep
		server.Tx(ctx, func(tx server.PgTx) {
			past := server.NowUtc().Add(-1 * time.Hour)
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE oauth_refresh_token SET expire_time = $2 WHERE client_id = $1`,
				client.ClientId,
				past,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE oauth_authorization_code SET expire_time = $2 WHERE client_id = $1`,
				client.ClientId,
				past,
			))
		})

		codeCount, refreshTokenCount := ReapOauthTokens(ctx)
		connect.AssertEqual(t, 0 < codeCount, true)
		connect.AssertEqual(t, 0 < refreshTokenCount, true)
	})
}
