package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

func TestByJwtLegacy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := false
		byJwt := NewByJwt(networkId, userId, networkName, guestMode, isPro)
		jwtSigned := byJwt.Sign()

		parsedByJwt, err := ParseByJwt(ctx, jwtSigned)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsedByJwt, nil)

		connect.AssertEqual(t, byJwt.NetworkId, parsedByJwt.NetworkId)
		connect.AssertEqual(t, byJwt.UserId, parsedByJwt.UserId)
		connect.AssertEqual(t, byJwt.NetworkName, parsedByJwt.NetworkName)
		connect.AssertEqual(t, byJwt.Pro, parsedByJwt.Pro)
	})
}

// TestByJwtKid covers the `kid` key-selection behavior: a freshly signed token
// carries the signing key's kid and that kid resolves to a loaded key (fast
// path); a token without a kid still verifies via the all-keys fallback (old
// tokens); and a token signed by a key we do not hold is rejected even if it
// claims a real kid (no trust hole — an embedded/forged key is never trusted).
func TestByJwtKid(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		newClaims := func() *ByJwt {
			return NewByJwt(server.NewId(), server.NewId(), "test", false, false)
		}

		// the key sign() uses: ecdsa first, else rsa
		var signingKey crypto.PrivateKey
		if k := byEcdsaSigningKey(); k != nil {
			signingKey = k
		} else {
			signingKey = byRsaSigningKey()
		}
		connect.AssertNotEqual(t, signingKey, nil)

		// signWithoutKid mirrors sign()'s method selection but omits the kid header
		signWithoutKid := func(claims gojwt.Claims, key crypto.PrivateKey) string {
			var method gojwt.SigningMethod
			switch k := key.(type) {
			case *ecdsa.PrivateKey:
				switch k.Curve.Params().N.BitLen() {
				case 256:
					method = gojwt.SigningMethodES256
				case 384:
					method = gojwt.SigningMethodES384
				default:
					method = gojwt.SigningMethodES512
				}
			case *rsa.PrivateKey:
				method = gojwt.SigningMethodRS512
			}
			token := gojwt.NewWithClaims(method, claims)
			signed, err := token.SignedString(key)
			connect.AssertEqual(t, err, nil)
			return signed
		}

		// fast path: a normally-signed token carries the signing key's kid, that
		// kid resolves to a loaded key, and parsing succeeds
		signed := newClaims().Sign()
		unverified, _, err := gojwt.NewParser().ParseUnverified(signed, gojwt.MapClaims{})
		connect.AssertEqual(t, err, nil)
		kid, _ := unverified.Header["kid"].(string)
		connect.AssertNotEqual(t, kid, "")
		expectedKid, err := jwtKid(publicKey(signingKey))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, kid, expectedKid)
		_, ok := byPublicKeysByKid()[kid]
		connect.AssertEqual(t, ok, true)

		parsed, err := ParseByJwt(ctx, signed)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsed, nil)

		// fallback: a token with no kid still verifies against the full key set
		noKid := signWithoutKid(newClaims(), signingKey)
		parsedNoKid, err := ParseByJwt(ctx, noKid)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsedNoKid, nil)

		// security: a token signed by a foreign key is rejected, both with no kid
		// (falls back to our keys, none match) and with a spoofed real kid (the
		// signature does not match that key)
		foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		connect.AssertEqual(t, err, nil)

		forgedNoKid := signWithoutKid(newClaims(), foreignKey)
		_, err = ParseByJwt(ctx, forgedNoKid)
		connect.AssertNotEqual(t, err, nil)

		forgedToken := gojwt.NewWithClaims(gojwt.SigningMethodES256, newClaims())
		forgedToken.Header["kid"] = kid // a real, known kid, but signed by foreignKey
		forgedSpoofedKid, err := forgedToken.SignedString(foreignKey)
		connect.AssertEqual(t, err, nil)
		_, err = ParseByJwt(ctx, forgedSpoofedKid)
		connect.AssertNotEqual(t, err, nil)
	})
}

// TestByJwtKidUnawareParser proves the `kid` header we now add is a standard,
// optional JOSE header: a parser that never looks at kid (verifying with the
// signing key supplied out-of-band) still parses a kid-tagged token, as does the
// codebase's own kid-agnostic ParseByJwtUnverified. Adding kid stays backward
// compatible with any consumer that does not understand or use it.
func TestByJwtKidUnawareParser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkName := "test"
		userId := server.NewId()
		byJwt := NewByJwt(server.NewId(), userId, networkName, false, false)
		signed := byJwt.Sign()

		// precondition: the token actually carries a kid header, so the test is
		// meaningful
		unverified, _, err := gojwt.NewParser().ParseUnverified(signed, gojwt.MapClaims{})
		connect.AssertEqual(t, err, nil)
		kid, ok := unverified.Header["kid"].(string)
		connect.AssertEqual(t, ok, true)
		connect.AssertNotEqual(t, kid, "")

		// the key sign() used, supplied to the parser out-of-band
		var signingKey crypto.PrivateKey
		if k := byEcdsaSigningKey(); k != nil {
			signingKey = k
		} else {
			signingKey = byRsaSigningKey()
		}
		signingPublicKey := publicKey(signingKey)

		// a kid-unaware parser: the keyfunc verifies with the key directly and
		// never consults token.Header["kid"]
		claims := gojwt.MapClaims{}
		_, err = gojwt.NewParser(gojwt.WithoutClaimsValidation()).ParseWithClaims(
			signed,
			claims,
			func(token *gojwt.Token) (any, error) {
				return signingPublicKey, nil
			},
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims["network_name"], networkName)

		// the codebase's own kid-agnostic parser also handles the kid-tagged token
		parsedUnverified, err := ParseByJwtUnverified(ctx, signed)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsedUnverified, nil)
		connect.AssertEqual(t, parsedUnverified.NetworkName, networkName)
		connect.AssertEqual(t, parsedUnverified.UserId, userId)
	})
}

func TestByJwtFull(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := true
		byJwt := NewByJwt(networkId, userId, networkName, guestMode, isPro)
		jwtSigned := byJwt.Sign()

		parsedByJwt, err := ParseByJwt(ctx, jwtSigned)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsedByJwt, nil)

		connect.AssertEqual(t, byJwt.NetworkId, parsedByJwt.NetworkId)
		connect.AssertEqual(t, byJwt.UserId, parsedByJwt.UserId)
		connect.AssertEqual(t, byJwt.NetworkName, parsedByJwt.NetworkName)
		connect.AssertEqual(t, byJwt.CreateTime, parsedByJwt.CreateTime)
		connect.AssertEqual(t, byJwt.Pro, parsedByJwt.Pro)
	})
}

func TestByJwtFullWithClientId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false
		isPro := true
		byJwt := NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
			isPro,
		)

		deviceId := server.NewId()
		clientId := server.NewId()
		byClientJwt := byJwt.Client(deviceId, clientId)

		clientJwtSigned := byClientJwt.Sign()

		parsedByClientJwt, err := ParseByJwt(ctx, clientJwtSigned)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsedByClientJwt, nil)

		connect.AssertEqual(t, byJwt.NetworkId, parsedByClientJwt.NetworkId)
		connect.AssertEqual(t, byJwt.UserId, parsedByClientJwt.UserId)
		connect.AssertEqual(t, byJwt.NetworkName, parsedByClientJwt.NetworkName)
		connect.AssertEqual(t, byJwt.CreateTime, parsedByClientJwt.CreateTime)
		connect.AssertEqual(t, byClientJwt.DeviceId, parsedByClientJwt.DeviceId)
		connect.AssertEqual(t, byClientJwt.ClientId, parsedByClientJwt.ClientId)
		connect.AssertEqual(t, byClientJwt.Pro, parsedByClientJwt.Pro)
	})
}
