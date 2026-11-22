package oauth

// Which redirect uris this server sends a user agent to.
//
// The consent page navigates the signed-in user's browser to the redirect uri:
// with the code on approval, and with the error on cancel. A scheme the
// browser handles itself (javascript:, data:, vbscript:, file:, about:, blob:)
// would run in, or read from, the consent page's origin, so a redirect uri is
// accepted only in a form that hands the response to a client, and every other
// form is refused:
//   - https with a host, for any client;
//   - http to a loopback host, for a native client (rfc 8252 section 7.3);
//   - for a native client, a private-use scheme in reverse domain name form,
//     which has a dot, such as com.example.app:/callback (rfc 8252 section
//     7.1), or one of the few private-use schemes named in
//     allowedPrivateUseSchemes.
//
// The uri must be absolute, with no fragment (rfc 6749 section 3.1.2), no user
// info and no backslash, which browsers and url parsers read differently.
//
// The rules run at registration, on every authorization step (the consent
// screen and the approval), and at the token exchange, so a client registered
// before them cannot use a redirect uri they refuse.

import (
	"fmt"
	"net/url"
	"strings"
)

// Private-use schemes accepted without a dot, each one an mcp client's that
// predates the reverse domain name convention. Only a scheme that names an
// app belongs here: a scheme the browser or the operating system handles
// itself never does.
var allowedPrivateUseSchemes = map[string]bool{
	// WorkBuddy's mcp connector
	"workbuddy": true,
}

// Reports why a redirect uri may not be used by a client of the application
// type, or nil when it may.
func checkRedirectUri(redirectUri string, applicationType string) error {
	if strings.ContainsAny(redirectUri, "#\\") {
		return fmt.Errorf("%w: a fragment or a backslash is not allowed", ErrInvalidRedirect)
	}
	redirectUrl, err := url.Parse(redirectUri)
	if err != nil {
		return fmt.Errorf("%w: not a uri", ErrInvalidRedirect)
	}
	// url.Parse lowercases the scheme
	if redirectUrl.Scheme == "" {
		return fmt.Errorf("%w: not an absolute uri", ErrInvalidRedirect)
	}
	if redirectUrl.User != nil {
		return fmt.Errorf("%w: user info is not allowed", ErrInvalidRedirect)
	}

	switch redirectUrl.Scheme {
	case "https":
		if redirectUrl.Opaque != "" || redirectUrl.Hostname() == "" {
			return fmt.Errorf("%w: https needs a host", ErrInvalidRedirect)
		}
		return nil
	case "http":
		if applicationType != ApplicationTypeNative || redirectUrl.Opaque != "" || !isLoopbackHost(redirectUrl.Hostname()) {
			return fmt.Errorf("%w: http is only allowed for native loopback", ErrInvalidRedirect)
		}
		return nil
	default:
		if applicationType != ApplicationTypeNative || !privateUseScheme(redirectUrl.Scheme) {
			return fmt.Errorf("%w: unsupported scheme %q", ErrInvalidRedirect, redirectUrl.Scheme)
		}
		return nil
	}
}

// A private-use scheme in reverse domain name form (rfc 8252 section 7.1):
// two or more dot separated labels of lowercase letters, digits and hyphens.
// None of the schemes a browser handles itself has a dot.
func privateUseScheme(scheme string) bool {
	if allowedPrivateUseSchemes[scheme] {
		return true
	}
	labels := strings.Split(scheme, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
		for _, c := range label {
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
