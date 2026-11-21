package oauth

// Pure: the redirect uri rules (redirect_uri.go). The consent page sends the
// signed-in user's browser to the redirect uri, on approval and on cancel, so a
// scheme the browser handles itself ran script in the consent page's origin.

import (
	"testing"
)

func TestRedirectUriRules(t *testing.T) {
	refusedForEveryClient := []string{
		// schemes the browser handles itself
		"javascript:alert(document.domain)//",
		"JavaScript:alert(1)//",
		"javascript://%0aalert(1)",
		"data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)",
		"file:///etc/passwd",
		"about:blank",
		"blob:https://ur.io/0b6e4a7c",
		// not absolute
		"invalid",
		"//evil.example/cb",
		"/callback",
		"",
		// out-of-band, removed by oauth 2.1
		"urn:ietf:wg:oauth:2.0:oob",
		// a private-use scheme not in reverse domain name form
		"cursor://anysphere.cursor-retrieval/oauth/callback",
		"ms-msdt:/id",
		"com..example:/callback",
		"com.example.:/callback",
		"com.example+app:/callback",
		// https without a host, or read differently by a browser
		"https:claude.ai/callback",
		"https:///claude.ai/callback",
		"https://claude.ai@evil.example/callback",
		"https://claude.ai\\@evil.example/callback",
		"https://claude.ai/callback#fragment",
		"https://claude.ai/callback#",
		// http off loopback, or to loopback through user info
		"http://evil.example/callback",
		"http://localhost.evil.example/callback",
		"http://evil.example@127.0.0.1/callback",
		"http://0.0.0.0:8080/callback",
	}
	for _, redirectUri := range refusedForEveryClient {
		for _, applicationType := range []string{ApplicationTypeWeb, ApplicationTypeNative} {
			if err := checkRedirectUri(redirectUri, applicationType); err == nil {
				t.Errorf("%s client: %q was allowed", applicationType, redirectUri)
			}
		}
	}

	allowedForEveryClient := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"HTTPS://chatgpt.com/connector_platform_oauth_redirect",
		"https://vscode.dev/redirect?x=1",
	}
	allowedForNative := []string{
		"http://127.0.0.1:33418/callback",
		"http://127.0.0.1/callback",
		"http://[::1]:8080/callback",
		"http://localhost:27890/callback",
		"com.example.app:/oauth2redirect/example-provider",
		"com.example.app://callback",
		"io.github.some-app.client:/callback",
		"workbuddy://workbuddy/mcp/custom-mcp%3Aurnetwork/oauth/callback",
	}
	for _, redirectUri := range allowedForEveryClient {
		for _, applicationType := range []string{ApplicationTypeWeb, ApplicationTypeNative} {
			if err := checkRedirectUri(redirectUri, applicationType); err != nil {
				t.Errorf("%s client: %q was refused: %s", applicationType, redirectUri, err)
			}
		}
	}
	for _, redirectUri := range allowedForNative {
		if err := checkRedirectUri(redirectUri, ApplicationTypeNative); err != nil {
			t.Errorf("native client: %q was refused: %s", redirectUri, err)
		}
		// a web client never redirects off https
		if err := checkRedirectUri(redirectUri, ApplicationTypeWeb); err == nil {
			t.Errorf("web client: %q was allowed", redirectUri)
		}
	}
}

// A client registered before the rules keeps its redirect uris, so the rules
// apply on every use, not only at registration. The redirect uris of the
// clients registered with a javascript: redirect, and the other forms the rules
// refuse, no longer match.
func TestRegisteredRedirectUriMustStillBeAllowed(t *testing.T) {
	for _, redirectUri := range []string{
		"javascript:alert(1)",
		"javascript:alert(document.domain)//",
		"data:text/html,<script>alert(1)</script>",
		"invalid",
		"//evil.example/cb",
		"urn:ietf:wg:oauth:2.0:oob",
	} {
		native := &Client{
			ApplicationType: ApplicationTypeNative,
			RedirectUris:    []string{redirectUri, "https://client.example/callback"},
		}
		if native.ValidRedirectUri(redirectUri) {
			t.Errorf("a registered %q matched", redirectUri)
		}
		// the client's other redirect uri still works
		if !native.ValidRedirectUri("https://client.example/callback") {
			t.Errorf("the client's https redirect stopped matching")
		}
	}

	native := &Client{
		ApplicationType: ApplicationTypeNative,
		RedirectUris:    []string{"com.example.app:/callback", "http://127.0.0.1:1234/callback"},
	}
	if !native.ValidRedirectUri("com.example.app:/callback") {
		t.Errorf("a private-use redirect stopped matching")
	}
	if !native.ValidRedirectUri("http://127.0.0.1:55123/callback") {
		t.Errorf("a loopback redirect on another port stopped matching")
	}
}
