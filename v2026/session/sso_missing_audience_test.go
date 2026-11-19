// Signed provider tokens cannot authenticate another application's users when
// this service has no configured audience. Both provider parsers share it.
package session

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

func TestSsoProviderParsersRefuseMissingAudienceConfiguration(t *testing.T) {
	key := testingSsoKey(t)
	keys := []any{&key.PublicKey}
	claims := gojwt.MapClaims{
		"aud": "unrelated-application.example", "sub": "synthetic-subject",
		"email": "synthetic-user@fixture.example", "email_verified": true,
		"iat": time.Now().Add(-time.Minute).Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	for _, allowed := range [][]string{nil, {}, {""}, {"   "}} {
		claims["iss"] = "https://accounts.google.com"
		if result, err := parseGoogleJwtWithKeys(testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, claims), keys, allowed); result != nil || err == nil {
			t.Errorf("Google accepted an unrelated audience with configuration %q: result=%v err=%v", allowed, result, err)
		}
		claims["iss"] = "https://appleid.apple.com"
		if result, err := parseAppleJwtWithKeys(testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, claims), keys, allowed); result != nil || err == nil {
			t.Errorf("Apple accepted an unrelated audience with configuration %q: result=%v err=%v", allowed, result, err)
		}
	}
}
