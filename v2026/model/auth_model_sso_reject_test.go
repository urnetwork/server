package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// A rejected provider id token must be distinguishable from a generic bad
// login, and the response must not echo the token or any claim from it.
func TestSsoAuthJwtRejectedLoginResultIsDistinctAndPrivate(t *testing.T) {
	token := "synthetic-provider-token.with.claims"

	// verification failure (here an unsupported type, which needs no network
	// key fetch) is reported as a nil parse, not a partial jwt
	if parsed := parseSsoAuthJwt("login", token, AuthType("synthetic-unsupported")); parsed != nil {
		t.Fatal("rejected sso token produced a parsed jwt")
	}

	result := ssoAuthJwtRejectedLoginResult()
	if result == nil || result.Error == nil || result.Network != nil {
		t.Fatal("rejected sso token did not produce an error result")
	}
	if result.Error.Message == "Invalid login credentials." {
		t.Fatal("rejected sso token is indistinguishable from a generic bad login")
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("rejected sso result echoed the provider token")
	}
}
