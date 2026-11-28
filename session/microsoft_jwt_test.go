package session

// The Microsoft identity token checks (microsoft_jwt.go), against tokens
// signed by a test key: only our audience, only the token's own tenant as the
// issuer, RS256 only, and only an email Microsoft verified (a personal
// account's, or a work account's whose domain its organization verified).

import (
	"crypto/rand"
	"crypto/rsa"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

const microsoftTestClientId = "00000000-1111-2222-3333-444444444444"

// a work or school tenant
const microsoftTestTenantId = "72f988bf-0000-4000-8000-000000000011"

func microsoftTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The claims of a valid personal account token, which a case then changes.
func microsoftTestClaims(tenantId string) gojwt.MapClaims {
	now := time.Now()
	return gojwt.MapClaims{
		"iss":   "https://login.microsoftonline.com/" + tenantId + "/v2.0",
		"aud":   microsoftTestClientId,
		"tid":   tenantId,
		"sub":   "AAAAAAAAAAAAAAAAAAAAAIkzqFVrSaSaFHy782bbtaQ",
		"email": "person@example.com",
		"name":  "Synthetic Person",
		"nonce": "test-nonce",
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
}

func microsoftTestToken(t *testing.T, key *rsa.PrivateKey, method gojwt.SigningMethod, claims gojwt.MapClaims) string {
	t.Helper()
	var signingKey any = key
	if method == gojwt.SigningMethodHS256 {
		signingKey = []byte("a shared secret")
	}
	signed, err := gojwt.NewWithClaims(method, claims).SignedString(signingKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestMicrosoftJwtAcceptsVerifiedEmails(t *testing.T) {
	key := microsoftTestKey(t)
	keys := []any{&key.PublicKey}
	clientIds := []string{microsoftTestClientId}

	personal := microsoftTestClaims(MicrosoftConsumerTenantId)
	jwt, err := parseMicrosoftJwtWithKeys(microsoftTestToken(t, key, gojwt.SigningMethodRS256, personal), keys, clientIds)
	if err != nil || jwt.UserAuth != "person@example.com" || jwt.UserName != "Synthetic Person" {
		t.Fatalf("a personal account: %+v %v", jwt, err)
	}

	for _, verified := range []any{true, "true", "1", float64(1)} {
		work := microsoftTestClaims(microsoftTestTenantId)
		work["xms_edov"] = verified
		jwt, err := parseMicrosoftJwtWithKeys(microsoftTestToken(t, key, gojwt.SigningMethodRS256, work), keys, clientIds)
		if err != nil || jwt.UserAuth != "person@example.com" {
			t.Errorf("a work account with xms_edov %v: %+v %v", verified, jwt, err)
		}
	}

	// a name is optional
	nameless := microsoftTestClaims(MicrosoftConsumerTenantId)
	delete(nameless, "name")
	if jwt, err := parseMicrosoftJwtWithKeys(microsoftTestToken(t, key, gojwt.SigningMethodRS256, nameless), keys, clientIds); err != nil || jwt.UserName != "" {
		t.Fatalf("a token without a name: %+v %v", jwt, err)
	}
}

// The root cause of the nOAuth pattern: a work or school account's email is
// whatever its organization's admin set, so without the organization's proof
// of the domain it never signs in, and nothing else that would let a token
// stand for another account or app passes either.
func TestMicrosoftJwtRefusesUnverifiedOrForeignTokens(t *testing.T) {
	key := microsoftTestKey(t)
	otherKey := microsoftTestKey(t)
	keys := []any{&key.PublicKey}
	clientIds := []string{microsoftTestClientId}

	cases := []struct {
		name   string
		key    *rsa.PrivateKey
		method gojwt.SigningMethod
		change func(gojwt.MapClaims)
		want   string
	}{
		{"a work account without xms_edov", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["tid"] = microsoftTestTenantId
			c["iss"] = "https://login.microsoftonline.com/" + microsoftTestTenantId + "/v2.0"
		}, "Email not verified."},
		{"a work account with xms_edov false", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["tid"] = microsoftTestTenantId
			c["iss"] = "https://login.microsoftonline.com/" + microsoftTestTenantId + "/v2.0"
			c["xms_edov"] = false
		}, "Email not verified."},
		{"an issuer of another tenant", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["xms_edov"] = true
			c["iss"] = "https://login.microsoftonline.com/" + microsoftTestTenantId + "/v2.0"
		}, "Invalid issuer."},
		{"the consumer tenant claimed by another issuer", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["iss"] = "https://login.microsoftonline.example/" + MicrosoftConsumerTenantId + "/v2.0"
		}, "Invalid issuer."},
		{"a tenant that is not a guid", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["tid"] = "common"
			c["iss"] = "https://login.microsoftonline.com/common/v2.0"
		}, "Invalid tenant."},
		{"another app's token", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["aud"] = "99999999-1111-2222-3333-444444444444"
		}, "Invalid audience."},
		{"no email", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			delete(c, "email")
		}, "Malformed jwt."},
		{"an expired token", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			c["exp"] = time.Now().Add(-time.Minute).Unix()
		}, "Could not verify signed token."},
		{"no expiry", key, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {
			delete(c, "exp")
		}, "Could not verify signed token."},
		{"another key", otherKey, gojwt.SigningMethodRS256, func(c gojwt.MapClaims) {}, "Could not verify signed token."},
		{"a shared-secret algorithm", key, gojwt.SigningMethodHS256, func(c gojwt.MapClaims) {}, "Could not verify signed token."},
	}
	for _, c := range cases {
		claims := microsoftTestClaims(MicrosoftConsumerTenantId)
		c.change(claims)
		jwt, err := parseMicrosoftJwtWithKeys(microsoftTestToken(t, c.key, c.method, claims), keys, clientIds)
		if err == nil || jwt != nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %+v %v, want %q", c.name, jwt, err, c.want)
		}
	}

	// no configured client id refuses every token
	if _, err := parseMicrosoftJwtWithKeys(microsoftTestToken(t, key, gojwt.SigningMethodRS256, microsoftTestClaims(MicrosoftConsumerTenantId)), keys, nil); err == nil {
		t.Fatal("a token was accepted with no client id configured")
	}
}
