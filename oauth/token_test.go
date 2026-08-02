package oauth

// Coverage for the token foundation: the signer keys resolve out of the vault,
// the jwks matches what actually signs, and the audience binding that keeps an
// mcp token from being usable anywhere else actually holds.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

const testAudience = "https://mcp.bringyour.com"

func TestSignerKeysResolve(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		keys := VerificationKeys()
		connect.AssertEqual(t, 0 < len(keys), true)

		// the loader rejects a kid that does not match its key, so reaching
		// here means config and key file agree
		signingKey := SigningKey()
		connect.AssertEqual(t, signingKey, keys[0])
		connect.AssertEqual(t, signingKey.Alg, signerAlgEs256)

		// every signer key is published, so a token signed by any of them can
		// be verified by a client reading the jwks
		jwks := Jwks()
		connect.AssertEqual(t, len(jwks.Keys), len(keys))
		for i, jwk := range jwks.Keys {
			connect.AssertEqual(t, jwk.Kid, keys[i].Kid)
			connect.AssertEqual(t, jwk.Kty, "EC")
			connect.AssertEqual(t, jwk.Crv, "P-256")
			connect.AssertEqual(t, jwk.Use, "sig")
			connect.AssertEqual(t, jwk.X != "", true)
			connect.AssertEqual(t, jwk.Y != "", true)
		}
	})
}

// The whole point of the separate key set: an oauth token must not be a
// platform credential. If this ever fails, a scoped token has become an
// unscoped one.
func TestAccessTokenIsNotAByJwt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		accessToken, _, err := MintAccessToken(&MintAccessTokenArgs{
			UserId:    server.NewId(),
			NetworkId: server.NewId(),
			ClientId:  "https://claude.ai/mcp",
			Audience:  testAudience,
			Scopes:    []string{ScopeMcpRead},
		})
		connect.AssertEqual(t, err, nil)

		// the ByJwt key set must not verify an oauth token
		_, err = jwtParseByJwt(t, accessToken)
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestAccessTokenRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		userId := server.NewId()
		networkId := server.NewId()

		accessToken, expiry, err := MintAccessToken(&MintAccessTokenArgs{
			UserId:    userId,
			NetworkId: networkId,
			ClientId:  "https://claude.ai/mcp",
			Audience:  testAudience,
			Scopes:    []string{ScopeMcpRead, ScopeMcpFetch},
			Principal: "user@example.com",
		})
		connect.AssertEqual(t, err, nil)
		// short lived by design, so revoking the refresh token bites quickly
		connect.AssertEqual(t, expiry.After(server.NowUtc()), true)
		connect.AssertEqual(t, expiry.Before(server.NowUtc().Add(AccessTokenDuration+time.Minute)), true)

		claims, err := VerifyAccessToken(accessToken, testAudience)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Subject, userId.String())
		connect.AssertEqual(t, claims.NetworkId, networkId)
		connect.AssertEqual(t, claims.Principal, "user@example.com")
		connect.AssertEqual(t, claims.Issuer, Issuer())
		connect.AssertEqual(t, HasScope(ParseScope(claims.Scope), ScopeMcpFetch), true)
	})
}

// rfc 8707: a resource server must only accept tokens minted for itself.
func TestAccessTokenAudienceIsEnforced(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		accessToken, _, err := MintAccessToken(&MintAccessTokenArgs{
			UserId:    server.NewId(),
			NetworkId: server.NewId(),
			ClientId:  "https://claude.ai/mcp",
			Audience:  "https://other.bringyour.com",
			Scopes:    []string{ScopeMcpRead},
		})
		connect.AssertEqual(t, err, nil)

		_, err = VerifyAccessToken(accessToken, testAudience)
		connect.AssertEqual(t, err != nil, true)

		// and there is no way to verify without naming an audience
		_, err = VerifyAccessToken(accessToken, "")
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestIdTokenRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		userId := server.NewId()
		networkId := server.NewId()
		clientId := "https://claude.ai/mcp"
		authTime := server.NowUtc()

		accessToken, _, err := MintAccessToken(&MintAccessTokenArgs{
			UserId:    userId,
			NetworkId: networkId,
			ClientId:  clientId,
			Audience:  testAudience,
			Scopes:    []string{ScopeOpenid, ScopeMcpRead},
		})
		connect.AssertEqual(t, err, nil)

		idToken, err := MintIdToken(&MintIdTokenArgs{
			UserId:      userId,
			NetworkId:   networkId,
			NetworkName: "testnetwork",
			ClientId:    clientId,
			Nonce:       "n-0S6_WzA2Mj",
			AuthTime:    authTime,
			AccessToken: accessToken,
		})
		connect.AssertEqual(t, err, nil)

		claims, err := VerifyIdToken(idToken, clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Subject, userId.String())
		connect.AssertEqual(t, claims.Nonce, "n-0S6_WzA2Mj")
		connect.AssertEqual(t, claims.NetworkName, "testnetwork")
		connect.AssertEqual(t, claims.AuthTime, authTime.Unix())
		// openid connect core 3.1.3.6 binds the id token to its access token
		connect.AssertEqual(t, claims.AccessTokenHash, accessTokenHash(accessToken))

		// an id token is addressed to the client, so it must not verify for
		// another client
		_, err = VerifyIdToken(idToken, "https://evil.example.com/mcp")
		connect.AssertEqual(t, err != nil, true)

		// and an id token is not an access token
		_, err = VerifyAccessToken(idToken, testAudience)
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestScopeHelpers(t *testing.T) {
	scopes := ParseScope("openid mcp:read  mcp:fetch")
	connect.AssertEqual(t, len(scopes), 3)
	connect.AssertEqual(t, HasScope(scopes, ScopeMcpFetch), true)
	connect.AssertEqual(t, HasScope(scopes, ScopeOfflineAccess), false)
	connect.AssertEqual(t, FormatScope(scopes), "openid mcp:read mcp:fetch")

	// the resource metadata must not advertise offline_access, per the mcp spec
	connect.AssertEqual(t, HasScope(McpResourceScopes(), ScopeOfflineAccess), false)
}

// Parses with the ByJwt key set, to prove the oauth key set is disjoint.
func jwtParseByJwt(t testing.TB, tokenStr string) (any, error) {
	return jwt.ParseByJwt(context.Background(), tokenStr)
}
