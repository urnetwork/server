package jwt

import (
	"errors"
	"slices"
	"strings"
	"sync"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server/v2026"
)

// sso id token validation shared by the apple and google sign in paths.
//
// The signature check against the provider jwks proves who signed the token,
// but not who it was minted for: without an audience check, a valid id token
// issued to any other app authenticates its email here (token substitution).
// The audience must match one of our own oauth client ids.

// ssoAllowedClientIds reads the sign in oauth client id(s) from the vault
// resource (`client_id` at the top level of vault/<env>/apple.yml or
// google.yml, either a single value or a list). A missing file or empty value
// means not configured: the audience check is skipped so environments that
// have not populated the vault keep working.
func ssoAllowedClientIds(vaultResource string) []string {
	resource, err := server.Vault.SimpleResource(vaultResource)
	if err != nil {
		return nil
	}
	clientIds := []string{}
	for _, clientId := range resource.String("client_id") {
		if clientId = strings.TrimSpace(clientId); clientId != "" {
			clientIds = append(clientIds, clientId)
		}
	}
	return clientIds
}

var appleSsoClientIds = sync.OnceValue(func() []string {
	return ssoAllowedClientIds("apple.yml")
})

var googleSsoClientIds = sync.OnceValue(func() []string {
	return ssoAllowedClientIds("google.yml")
})

// validateSsoClaims checks the non-signature claims of a provider id token:
// the issuer must be the provider and the audience must be one of our client
// ids (when configured). The time claims (exp/nbf/iat) are validated by the
// gojwt parser itself.
func validateSsoClaims(claims gojwt.MapClaims, issuers []string, allowedClientIds []string) error {
	issuer, err := claims.GetIssuer()
	if err != nil || !slices.Contains(issuers, issuer) {
		return errors.New("Invalid issuer.")
	}

	if 0 < len(allowedClientIds) {
		audiences, err := claims.GetAudience()
		if err != nil {
			return errors.New("Invalid audience.")
		}
		audienceAllowed := slices.ContainsFunc(audiences, func(audience string) bool {
			return slices.Contains(allowedClientIds, audience)
		})
		if !audienceAllowed {
			return errors.New("Invalid audience.")
		}
	}

	return nil
}

// ssoEmailVerified interprets the `email_verified` claim, which google emits
// as a bool and apple emits as a bool or the string "true". The email is the
// account identity here, so an unverified email must not authenticate (a
// provider account can carry an arbitrary unverified email address).
func ssoEmailVerified(claims gojwt.MapClaims) bool {
	switch v := claims["email_verified"].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	default:
		return false
	}
}
