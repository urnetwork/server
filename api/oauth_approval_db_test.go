package api

// End to end through the router, against the database: what the ur.io consent
// page calls, /oauth/consent for the screen and /oauth/authorize on Allow.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

const oauthApprovalTestCodeVerifier = "oauth-approval-test-code-verifier-oauth-approval-test"

// Registers a client as registration stored one, whatever its redirect uris.
func oauthApprovalTestClient(t testing.TB, ctx context.Context, applicationType string, redirectUri string) string {
	t.Helper()
	clientId := "urn_client_" + server.NewId().String()
	redirectUrisJson, err := json.Marshal([]string{redirectUri})
	server.Raise(err)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO oauth_client (client_id, client_type, client_name, application_type, redirect_uris_json)
			VALUES ($1, 'dynamic', 'Claude', $2, $3)
			`,
			clientId,
			applicationType,
			string(redirectUrisJson),
		))
	})
	return clientId
}

func oauthApprovalTestRequest(clientId string, redirectUri string) map[string]any {
	sum := sha256.Sum256([]byte(oauthApprovalTestCodeVerifier))
	return map[string]any{
		"client_id":             clientId,
		"redirect_uri":          redirectUri,
		"response_type":         "code",
		"scope":                 "offline_access mcp:read mcp:fetch",
		"state":                 "approval-test-state",
		"code_challenge":        base64.RawURLEncoding.EncodeToString(sum[:]),
		"code_challenge_method": "S256",
		"resource":              "https://mcp.bringyour.com",
	}
}

func oauthApprovalTestRows(ctx context.Context, clientId string) (consents int, codes int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM oauth_consent WHERE client_id = $1`, clientId).Scan(&consents))
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM oauth_authorization_code WHERE client_id = $1`, clientId).Scan(&codes))
	})
	return
}

// Only a person signed in to the account reads the consent screen and approves
// an application. Before, an API key approved one: anyone holding a key could
// connect an application to the account, and its grant outlived the key.
func TestOauthApprovalNeedsASignedInUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		redirectUri := "https://client.example/callback"
		clientId := oauthApprovalTestClient(t, ctx, "web", redirectUri)
		request := oauthApprovalTestRequest(clientId, redirectUri)

		for _, path := range []string{"/oauth/consent", "/oauth/authorize"} {
			if w := self.serve(http.MethodPost, path, self.key(), request); w.Code != http.StatusForbidden {
				t.Fatalf("POST %s with an API key: status = %d, want 403: %s", path, w.Code, w.Body.String())
			}
		}
		if consents, codes := oauthApprovalTestRows(ctx, clientId); consents != 0 || codes != 0 {
			t.Fatalf("the API key's approval wrote %d consents and %d codes", consents, codes)
		}

		// The sign-in approves. Both responses carry the issuer from vault
		// auth.yml, which the local fixture vault does not have, so where it is
		// missing the approval is read from what it recorded.
		for _, path := range []string{"/oauth/consent", "/oauth/authorize"} {
			if w := self.serve(http.MethodPost, path, self.root(), request); w.Code == http.StatusForbidden || w.Code == http.StatusBadRequest {
				t.Fatalf("POST %s with the sign-in: status = %d: %s", path, w.Code, w.Body.String())
			} else if w.Code == http.StatusOK && path == "/oauth/authorize" {
				var authorized struct {
					RedirectUri string `json:"redirect_uri"`
				}
				if json.Unmarshal(w.Body.Bytes(), &authorized) != nil || !strings.HasPrefix(authorized.RedirectUri, redirectUri+"?") || !strings.Contains(authorized.RedirectUri, "code=") {
					t.Fatalf("approval redirect: %s", w.Body.String())
				}
			}
		}
		if consents, codes := oauthApprovalTestRows(ctx, clientId); consents != 1 || codes != 1 {
			t.Fatalf("the sign-in's approval recorded %d consents and %d codes, want 1 and 1", consents, codes)
		}
	})
}

// A client registered with a javascript: redirect before the redirect uri
// rules, as three were, cannot use it. The consent page navigates to the
// redirect on Allow and on Cancel, so the javascript ran in ur.io's origin,
// where the session token is. Now the screen's request is refused, the page has
// nothing to cancel to, and Allow returns no redirect.
func TestOauthConsentRefusesARegisteredJavascriptRedirect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		self := newRouteAccessDbFixture(t, ctx)

		for _, redirectUri := range []string{
			"javascript:alert(document.domain)//",
			"data:text/html,<script>alert(document.domain)</script>",
		} {
			clientId := oauthApprovalTestClient(t, ctx, "native", redirectUri)
			request := oauthApprovalTestRequest(clientId, redirectUri)

			for _, path := range []string{"/oauth/consent", "/oauth/authorize"} {
				w := self.serve(http.MethodPost, path, self.root(), request)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("POST %s with redirect %q: status = %d, want 400: %s", path, redirectUri, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "redirect_uri\":") {
					t.Fatalf("POST %s returned a redirect: %s", path, w.Body.String())
				}
			}
			if consents, codes := oauthApprovalTestRows(ctx, clientId); consents != 0 || codes != 0 {
				t.Fatalf("redirect %q: %d consents and %d codes written", redirectUri, consents, codes)
			}
		}
	})
}
