package oauth

// The urnetwork oauth 2.1 / openid connect authorization server.
//
// Identity here is a user plus the network that user administers: the `sub` is
// the user id, and the network the token acts on is bound at authorization
// time so a token can never silently follow the user to a different network.
//
// Tokens issued here are signed with keys DEDICATED to oauth, loaded from the
// `oauth` block of vault auth.yml (the auth runtime config, beside the byjwt
// gates). They are deliberately disjoint from the ByJwt signing keys (vault
// jwt.yml). This is a hard security boundary, not a convention:
// `jwt.ParseByJwt` parses with claims validation disabled, so it checks
// neither `aud` nor `exp`. If an oauth access token were signed by a ByJwt
// key, a token scoped to `mcp:read` would verify as a full, unscoped,
// effectively non-expiring platform credential on every api route. Disjoint
// key sets make that impossible rather than merely disallowed.

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

const (
	// access tokens are short so that revoking a refresh token takes effect
	// quickly; the refresh token is the long lived, revocable half
	AccessTokenDuration = 1 * time.Hour
	// sliding: each use extends the window
	RefreshTokenDuration = 90 * 24 * time.Hour
	// an authorization code is single use and redeemed immediately
	AuthorizationCodeDuration = 1 * time.Minute
	// id tokens are proof of an authentication event, not a session
	IdTokenDuration = 1 * time.Hour
)

// Scopes. The set is deliberately minimal: one scope for reads, and a separate
// one for the egress tool because that provisions billed clients, so a caller
// can be granted lookups without being granted spend.
const (
	ScopeOpenid = "openid"
	// requests a refresh token. Never advertised in the protected resource
	// metadata, per the mcp spec: it is not a resource requirement.
	ScopeOfflineAccess = "offline_access"

	ScopeMcpRead  = "mcp:read"
	ScopeMcpFetch = "mcp:fetch"
)

// The scopes a client may request at the authorization server.
func SupportedScopes() []string {
	return []string{
		ScopeOpenid,
		ScopeOfflineAccess,
		ScopeMcpRead,
		ScopeMcpFetch,
	}
}

// The scopes the mcp resource server advertises. `offline_access` is
// deliberately absent.
func McpResourceScopes() []string {
	return []string{
		ScopeMcpRead,
		ScopeMcpFetch,
	}
}

type signerKeyConfig struct {
	Kid        string `yaml:"kid"`
	Path       string `yaml:"path"`
	Alg        string `yaml:"alg"`
	CreateTime string `yaml:"create_time"`
}

type oauthConfig struct {
	Issuer                string             `yaml:"issuer"`
	AuthorizationEndpoint string             `yaml:"authorization_endpoint"`
	SignerKeys            []*signerKeyConfig `yaml:"signer_keys"`
}

type authConfig struct {
	Oauth *oauthConfig `yaml:"oauth"`
}

// The oauth block of vault auth.yml. Panics when absent: every route in this
// package depends on it, and a server that silently issued unsigned or
// wrongly-issued tokens would be worse than one that fails to start.
var Config = sync.OnceValue(func() *oauthConfig {
	var auth authConfig
	server.Vault.RequireSimpleResource("auth.yml").UnmarshalYaml(&auth)

	if auth.Oauth == nil {
		panic(fmt.Errorf("auth.yml has no oauth block"))
	}
	if auth.Oauth.Issuer == "" {
		panic(fmt.Errorf("auth.yml oauth.issuer is required"))
	}
	if auth.Oauth.AuthorizationEndpoint == "" {
		panic(fmt.Errorf("auth.yml oauth.authorization_endpoint is required"))
	}
	if len(auth.Oauth.SignerKeys) == 0 {
		panic(fmt.Errorf("auth.yml oauth.signer_keys is empty; run `warpctl oauth keygen <env>`, deploy, then `warpctl oauth promote <env>`"))
	}

	// the issuer is compared verbatim by clients (rfc 9207 forbids
	// normalizing before comparison), so reject a form that would not match
	if strings.HasSuffix(auth.Oauth.Issuer, "/") {
		panic(fmt.Errorf("auth.yml oauth.issuer must not end in a slash"))
	}

	return auth.Oauth
})

func Issuer() string {
	return Config().Issuer
}

func AuthorizationEndpoint() string {
	return Config().AuthorizationEndpoint
}

// Splits a space delimited scope string, per rfc 6749.
func ParseScope(scope string) []string {
	scopes := []string{}
	for _, s := range strings.Fields(scope) {
		scopes = append(scopes, s)
	}
	return scopes
}

func FormatScope(scopes []string) string {
	return strings.Join(scopes, " ")
}

func HasScope(scopes []string, scope string) bool {
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}
