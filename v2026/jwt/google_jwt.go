package jwt

import (
	"context"
	"errors"
	"sync"

	gojwt "github.com/golang-jwt/jwt/v5"
)

type GoogleJwt struct {
	UserAuth string
	UserName string
}

func NewGoogleJwkValidator(ctx context.Context) *JwkValidator {
	return NewJwkValidator(
		ctx,
		"google",
		"https://www.googleapis.com/oauth2/v3/certs",
	)
}

var googleJwkValidator = sync.OnceValue(func() *JwkValidator {
	return NewGoogleJwkValidator(context.Background())
})

// see https://developers.google.com/identity/sign-in/web/backend-auth
// https://www.googleapis.com/oauth2/v3/certs

func ParseGoogleJwt(jwtSigned string) (*GoogleJwt, error) {
	return parseGoogleJwtWithKeys(jwtSigned, googleJwkValidator().Keys(), googleSsoClientIds())
}

func parseGoogleJwtWithKeys(jwtSigned string, keys []any, allowedClientIds []string) (*GoogleJwt, error) {
	for _, key := range keys {
		token, err := gojwt.Parse(
			jwtSigned,
			func(token *gojwt.Token) (any, error) {
				return key, nil
			},
			// the google jwks signs id tokens with RS256; pinning the method
			// prevents algorithm confusion against the rsa public keys
			gojwt.WithValidMethods([]string{"RS256"}),
			gojwt.WithExpirationRequired(),
		)
		if err == nil {
			var userAuthString string
			var userNameString string
			var ok bool

			claims := token.Claims.(gojwt.MapClaims)

			/*
				example:

				{
				  "iss": "https://accounts.google.com",
				  "nbf": 1681249433,
				  "aud": "338638865390-cg4m0t700mq9073smhn9do81mr640ig1.apps.googleusercontent.com",
				  "sub": "112929765236355348953",
				  "nonce": "d77cdf41-4d48-4aa9-97c0-66ced5ff198d",
				  "email": "xcolwell@gmail.com",
				  "email_verified": true,
				  "azp": "338638865390-cg4m0t700mq9073smhn9do81mr640ig1.apps.googleusercontent.com",
				  "name": "Brien Colwell",
				  "picture": "https://lh3.googleusercontent.com/a/AGNmyxblbyflWp_rkNjB0_9xQlGA11JZ6hL95cCiHHrUjEA=s96-c",
				  "given_name": "Brien",
				  "family_name": "Colwell",
				  "iat": 1681249733,
				  "exp": 1681253333,
				  "jti": "f807e0f78837d9659d165c352e514d2eb7a8ca1e"
				}
			*/

			if err := validateSsoClaims(
				claims,
				// google issues with and without the scheme
				[]string{"https://accounts.google.com", "accounts.google.com"},
				allowedClientIds,
			); err != nil {
				return nil, err
			}

			userAuthString, ok = claims["email"].(string)
			if !ok {
				return nil, errors.New("Malformed jwt.")
			}
			// the email is the account identity: an unverified email on the
			// provider account must not authenticate it here
			if !ssoEmailVerified(claims) {
				return nil, errors.New("Email not verified.")
			}
			userNameString, ok = claims["name"].(string)
			if !ok {
				return nil, errors.New("Malformed jwt.")
			}

			jwt := &GoogleJwt{
				UserAuth: userAuthString,
				UserName: userNameString,
			}
			return jwt, nil
		}
	}

	return nil, errors.New("Could not verify signed token.")
}

// ParseGoogleJwtUnverified does not verify the signature or claims. It must
// only be used to re-read an auth jwt that was verified with `ParseGoogleJwt`
// when it was first presented (e.g. the stored network_user auth_jwt), never
// on untrusted input.
func ParseGoogleJwtUnverified(jwtStr string) (*GoogleJwt, error) {
	token, _, err := gojwt.NewParser().ParseUnverified(jwtStr, &gojwt.MapClaims{})
	if err != nil {
		return nil, err
	}

	var userAuthString string
	var userNameString string
	var ok bool

	claims := token.Claims.(*gojwt.MapClaims)

	if claims == nil {
		return nil, errors.New("Malformed jwt.")
	}

	userAuthString, ok = (*claims)["email"].(string)
	if !ok {
		return nil, errors.New("Malformed jwt.")
	}
	userNameString, ok = (*claims)["name"].(string)
	if !ok {
		return nil, errors.New("Malformed jwt.")
	}

	jwt := &GoogleJwt{
		UserAuth: userAuthString,
		UserName: userNameString,
	}
	return jwt, nil
}
