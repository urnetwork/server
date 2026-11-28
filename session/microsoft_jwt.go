package session

// Sign in with Microsoft: the identity token of the Microsoft identity
// platform (v2.0), for personal Microsoft accounts and for work or school
// accounts of any organization (the `common` endpoint).
//
// The token is verified as the Apple and Google tokens are (sso_jwt.go): the
// signature against Microsoft's published keys, RS256 only, an expiry, and an
// audience that is one of our own client ids (microsoft.yml `client_id`). The
// issuer is the token's own tenant: https://login.microsoftonline.com/<tid>/v2.0,
// with tid a GUID.
//
// The email is the account identity here, so it must be an email Microsoft
// verified. A work or school account's `email` claim is set by its
// organization's admin and can name any address, so taking it alone would let
// a tenant sign in as anyone's account (the "nOAuth" pattern). Accepted are:
//   - a personal Microsoft account (the consumer tenant), whose email the
//     account proved when it was created;
//   - a work or school account whose token carries `xms_edov` true: its
//     organization verified the email's domain. The claim is an optional claim
//     of the app registration; without it such an account is refused.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"

	gojwt "github.com/golang-jwt/jwt/v5"
)

type MicrosoftJwt struct {
	UserAuth string
	UserName string
}

// The tenant of every personal Microsoft account.
const MicrosoftConsumerTenantId = "9188040d-6c67-4c5b-b112-36a304b66dad"

// A tenant id: a GUID.
var microsoftTenantIdPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func NewMicrosoftJwkValidator(ctx context.Context) *JwkValidator {
	return NewJwkValidator(
		ctx,
		"microsoft",
		"https://login.microsoftonline.com/common/discovery/v2.0/keys",
	)
}

var microsoftJwkValidator = sync.OnceValue(func() *JwkValidator {
	return NewMicrosoftJwkValidator(context.Background())
})

var microsoftSsoClientIds = sync.OnceValue(func() []string {
	return ssoAllowedClientIds("microsoft.yml")
})

func ParseMicrosoftJwt(jwtSigned string) (*MicrosoftJwt, error) {
	return parseMicrosoftJwtWithKeys(jwtSigned, ssoJwkKeys(microsoftJwkValidator), ssoClientIds(microsoftSsoClientIds))
}

func parseMicrosoftJwtWithKeys(jwtSigned string, keys []any, allowedClientIds []string) (*MicrosoftJwt, error) {
	for _, key := range keys {
		token, err := gojwt.Parse(
			jwtSigned,
			func(token *gojwt.Token) (any, error) {
				return key, nil
			},
			// the microsoft keys sign identity tokens with RS256; pinning the
			// method prevents algorithm confusion against the rsa public keys
			gojwt.WithValidMethods([]string{"RS256"}),
			gojwt.WithExpirationRequired(),
		)
		if err != nil {
			continue
		}
		claims := token.Claims.(gojwt.MapClaims)

		tenantId, _ := claims["tid"].(string)
		tenantId = strings.ToLower(strings.TrimSpace(tenantId))
		if !microsoftTenantIdPattern.MatchString(tenantId) {
			return nil, errors.New("Invalid tenant.")
		}
		if err := validateSsoClaims(
			claims,
			// the token's own tenant issues it
			[]string{"https://login.microsoftonline.com/" + tenantId + "/v2.0"},
			allowedClientIds,
		); err != nil {
			return nil, err
		}

		userAuth, ok := claims["email"].(string)
		if !ok || strings.TrimSpace(userAuth) == "" {
			return nil, errors.New("Malformed jwt.")
		}
		if !microsoftEmailVerified(claims, tenantId) {
			return nil, errors.New("Email not verified.")
		}
		jwt := &MicrosoftJwt{
			UserAuth: userAuth,
		}
		// `name` needs the profile scope and a display name; a token without
		// one still signs in
		if name, ok := claims["name"].(string); ok {
			jwt.UserName = name
		}
		return jwt, nil
	}
	return nil, errors.New("Could not verify signed token.")
}

// Whether the token's email is one Microsoft verified: a personal account's,
// or a work or school account's whose domain its organization verified
// (`xms_edov`, a bool, or the string "true" or "1").
func microsoftEmailVerified(claims gojwt.MapClaims, tenantId string) bool {
	if tenantId == MicrosoftConsumerTenantId {
		return true
	}
	switch v := claims["xms_edov"].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	case float64:
		return v == 1
	default:
		return false
	}
}

// ParseMicrosoftJwtUnverified does not verify the signature or claims. It
// must only be used to re-read a token that ParseMicrosoftJwt verified when
// it was first presented, never on untrusted input.
func ParseMicrosoftJwtUnverified(jwtStr string) (*MicrosoftJwt, error) {
	token, _, err := gojwt.NewParser().ParseUnverified(jwtStr, &gojwt.MapClaims{})
	if err != nil {
		return nil, err
	}
	claims := token.Claims.(*gojwt.MapClaims)
	if claims == nil {
		return nil, errors.New("Malformed jwt.")
	}
	userAuth, ok := (*claims)["email"].(string)
	if !ok {
		return nil, errors.New("Malformed jwt.")
	}
	jwt := &MicrosoftJwt{
		UserAuth: userAuth,
	}
	if name, ok := (*claims)["name"].(string); ok {
		jwt.UserName = name
	}
	return jwt, nil
}
