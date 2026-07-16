package jwt

import (
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server/v2026"
)

func testingSsoKey(t testing.TB) *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	connect.AssertEqual(t, err, nil)
	return key
}

func testingSsoSignJwt(t testing.TB, method gojwt.SigningMethod, key any, claims gojwt.MapClaims) string {
	jwtSigned, err := gojwt.NewWithClaims(method, claims).SignedString(key)
	connect.AssertEqual(t, err, nil)
	return jwtSigned
}

// example claims from the doc comments in google_jwt.go / apple_jwt.go,
// with a live expiration
func testingGoogleClaims() gojwt.MapClaims {
	return gojwt.MapClaims{
		"iss":            "https://accounts.google.com",
		"aud":            "338638865390-cg4m0t700mq9073smhn9do81mr640ig1.apps.googleusercontent.com",
		"sub":            "112929765236355348953",
		"email":          "xcolwell@gmail.com",
		"email_verified": true,
		"name":           "Brien Colwell",
		"iat":            time.Now().Add(-time.Hour).Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
	}
}

func testingAppleClaims() gojwt.MapClaims {
	return gojwt.MapClaims{
		"iss":            "https://appleid.apple.com",
		"aud":            "com.bringyour.network",
		"sub":            "000452.afe9f7e27713494cb914a9fd8f812718.1847",
		"email":          "xcolwell@gmail.com",
		"email_verified": "true",
		"iat":            time.Now().Add(-time.Hour).Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
	}
}

func TestParseGoogleJwt(t *testing.T) {
	key := testingSsoKey(t)
	keys := []any{&key.PublicKey}
	clientId := "338638865390-cg4m0t700mq9073smhn9do81mr640ig1.apps.googleusercontent.com"

	sign := func(claims gojwt.MapClaims) string {
		return testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, claims)
	}

	// valid token with the configured audience
	googleJwt, err := parseGoogleJwtWithKeys(sign(testingGoogleClaims()), keys, []string{clientId})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, googleJwt.UserAuth, "xcolwell@gmail.com")
	connect.AssertEqual(t, googleJwt.UserName, "Brien Colwell")

	// alternate issuer form
	claims := testingGoogleClaims()
	claims["iss"] = "accounts.google.com"
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertEqual(t, err, nil)

	// a valid token minted for another app must be rejected (token substitution)
	claims = testingGoogleClaims()
	claims["aud"] = "attacker-app.apps.googleusercontent.com"
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// audience list form: allowed if any audience matches
	claims = testingGoogleClaims()
	claims["aud"] = []any{"other", clientId}
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertEqual(t, err, nil)

	// multiple configured client ids (ios/android/web)
	claims = testingGoogleClaims()
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{"other-client", clientId})
	connect.AssertEqual(t, err, nil)

	// no configured client id: audience check is skipped
	claims = testingGoogleClaims()
	claims["aud"] = "attacker-app.apps.googleusercontent.com"
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, nil)
	connect.AssertEqual(t, err, nil)

	// wrong issuer
	claims = testingGoogleClaims()
	claims["iss"] = "https://accounts.evil.example"
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// expired
	claims = testingGoogleClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// missing exp
	claims = testingGoogleClaims()
	delete(claims, "exp")
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// unverified email
	claims = testingGoogleClaims()
	claims["email_verified"] = false
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// missing email_verified
	claims = testingGoogleClaims()
	delete(claims, "email_verified")
	_, err = parseGoogleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// signed by a different key
	otherKey := testingSsoKey(t)
	_, err = parseGoogleJwtWithKeys(
		testingSsoSignJwt(t, gojwt.SigningMethodRS256, otherKey, testingGoogleClaims()),
		keys,
		[]string{clientId},
	)
	connect.AssertNotEqual(t, err, nil)

	// algorithm confusion: an hmac token must never validate against the
	// rsa public keys
	_, err = parseGoogleJwtWithKeys(
		testingSsoSignJwt(t, gojwt.SigningMethodHS256, []byte("attacker-secret"), testingGoogleClaims()),
		keys,
		[]string{clientId},
	)
	connect.AssertNotEqual(t, err, nil)
}

func TestParseAppleJwt(t *testing.T) {
	key := testingSsoKey(t)
	keys := []any{&key.PublicKey}
	clientId := "com.bringyour.network"

	sign := func(claims gojwt.MapClaims) string {
		return testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, claims)
	}

	// valid token with the configured audience.
	// apple emits email_verified as the string "true"
	appleJwt, err := parseAppleJwtWithKeys(sign(testingAppleClaims()), keys, []string{clientId})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, appleJwt.UserAuth, "xcolwell@gmail.com")

	// bool email_verified also accepted
	claims := testingAppleClaims()
	claims["email_verified"] = true
	_, err = parseAppleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertEqual(t, err, nil)

	// a valid token minted for another app must be rejected (token substitution)
	claims = testingAppleClaims()
	claims["aud"] = "com.attacker.app"
	_, err = parseAppleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// no configured client id: audience check is skipped
	claims = testingAppleClaims()
	claims["aud"] = "com.attacker.app"
	_, err = parseAppleJwtWithKeys(sign(claims), keys, nil)
	connect.AssertEqual(t, err, nil)

	// wrong issuer
	claims = testingAppleClaims()
	claims["iss"] = "https://appleid.evil.example"
	_, err = parseAppleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// expired
	claims = testingAppleClaims()
	claims["exp"] = time.Now().Add(-time.Hour).Unix()
	_, err = parseAppleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// unverified email (apple string form)
	claims = testingAppleClaims()
	claims["email_verified"] = "false"
	_, err = parseAppleJwtWithKeys(sign(claims), keys, []string{clientId})
	connect.AssertNotEqual(t, err, nil)

	// algorithm confusion: an hmac token must never validate against the
	// rsa public keys
	_, err = parseAppleJwtWithKeys(
		testingSsoSignJwt(t, gojwt.SigningMethodHS256, []byte("attacker-secret"), testingAppleClaims()),
		keys,
		[]string{clientId},
	)
	connect.AssertNotEqual(t, err, nil)
}

// the vault `client_id` field accepts a single value or a list, and a missing
// file or empty value disables the audience check
func TestSsoAllowedClientIds(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		// missing resource
		connect.AssertEqual(t, len(ssoAllowedClientIds("does_not_exist.yml")), 0)

		pop := server.Vault.PushSimpleResource("test_sso.yml", []byte(`client_id: "a.example"`))
		connect.AssertEqual(t, ssoAllowedClientIds("test_sso.yml"), []string{"a.example"})
		pop()

		pop = server.Vault.PushSimpleResource("test_sso.yml", []byte(`client_id:
  - "a.example"
  - "b.example"
`))
		connect.AssertEqual(t, ssoAllowedClientIds("test_sso.yml"), []string{"a.example", "b.example"})
		pop()

		pop = server.Vault.PushSimpleResource("test_sso.yml", []byte(`client_id: ""`))
		connect.AssertEqual(t, len(ssoAllowedClientIds("test_sso.yml")), 0)
		pop()
	})
}
