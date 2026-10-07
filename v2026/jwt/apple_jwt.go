package jwt

import (
	"context"
	"errors"
	"sync"

	gojwt "github.com/golang-jwt/jwt/v5"
)

type AppleJwt struct {
	UserAuth string
	UserName string
}

func NewAppleJwkValidator(ctx context.Context) *JwkValidator {
	return NewJwkValidator(
		ctx,
		"apple",
		"https://appleid.apple.com/auth/keys",
	)
}

var appleJwkValidator = sync.OnceValue(func() *JwkValidator {
	return NewAppleJwkValidator(context.Background())
})

// fixme use cache-control header in the response to know when to refresh the list

// https://developer.apple.com/documentation/sign_in_with_apple/fetch_apple_s_public_key_for_verifying_token_signature
// save the keys to a file
// https://appleid.apple.com/auth/keys
// we do not do real time lookups against the api. new versions of the api will contain up to date keys

func ParseAppleJwt(jwtSigned string) (*AppleJwt, error) {
	return parseAppleJwtWithKeys(jwtSigned, appleJwkValidator().Keys(), appleSsoClientIds())
}

func parseAppleJwtWithKeys(jwtSigned string, keys []any, allowedClientIds []string) (*AppleJwt, error) {
	for _, key := range keys {
		token, err := gojwt.Parse(
			jwtSigned,
			func(token *gojwt.Token) (interface{}, error) {
				return key, nil
			},
			// the apple jwks signs id tokens with RS256; pinning the method
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
				  "iss": "https://appleid.apple.com",
				  "aud": "com.server.service",
				  "exp": 1681336265,
				  "iat": 1681249865,
				  "sub": "000452.afe9f7e27713494cb914a9fd8f812718.1847",
				  "nonce": "424cfe3e-56d2-4098-ae12-1688c9fa451a",
				  "c_hash": "HseBmSllAiRDd2lmEbXl_Q",
				  "email": "xcolwell@gmail.com",
				  "email_verified": "true",
				  "auth_time": 1681249865,
				  "nonce_supported": true
				}
			*/

			if err := validateSsoClaims(
				claims,
				[]string{"https://appleid.apple.com"},
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
			userNameString = ""

			jwt := &AppleJwt{
				UserAuth: userAuthString,
				UserName: userNameString,
			}
			return jwt, nil
		}
	}

	return nil, errors.New("Could not verify signed token.")
}

// ParseAppleJwtUnverified does not verify the signature or claims. It must
// only be used to re-read an auth jwt that was verified with `ParseAppleJwt`
// when it was first presented (e.g. the stored network_user auth_jwt), never
// on untrusted input.
func ParseAppleJwtUnverified(jwtStr string) (*AppleJwt, error) {
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
	userNameString = ""

	jwt := &AppleJwt{
		UserAuth: userAuthString,
		UserName: userNameString,
	}
	return jwt, nil
}
