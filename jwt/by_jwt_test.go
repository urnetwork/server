package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
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

func TestByJwtRegisteredClaimsAreEnforced(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRejectMissingExpiration(true)()
		defer Testing_SetRejectExpired(true)()
		ctx := context.Background()
		newClaims := func() *ByJwt {
			return NewByJwt(server.NewId(), server.NewId(), "test", false, false)
		}

		tests := []struct {
			name   string
			mutate func(*ByJwt)
		}{
			{
				name: "expired",
				mutate: func(claims *ByJwt) {
					claims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Minute))
				},
			},
			{
				name: "future not before",
				mutate: func(claims *ByJwt) {
					claims.NotBefore = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
				},
			},
			{
				name: "wrong issuer",
				mutate: func(claims *ByJwt) {
					claims.Issuer = "attacker"
				},
			},
			{
				name: "wrong audience",
				mutate: func(claims *ByJwt) {
					claims.Audience = gojwt.ClaimStrings{"attacker"}
				},
			},
			{
				name: "subject mismatch",
				mutate: func(claims *ByJwt) {
					claims.Subject = server.NewId().String()
				},
			},
			{
				name: "missing expiration",
				mutate: func(claims *ByJwt) {
					claims.ExpiresAt = nil
				},
			},
		}

		for _, test := range tests {
			claims := newClaims()
			test.mutate(claims)
			_, err := ParseByJwt(ctx, claims.Sign())
			if err == nil {
				t.Fatalf("invalid registered claims were accepted: %s", test.name)
			}
		}

		claims := newClaims()
		_, err := ParseByJwtForAudience(ctx, claims.Sign(), ByJwtAudienceConnect)
		connect.AssertEqual(t, err, nil)
		_, err = ParseByJwtForAudience(ctx, claims.Sign(), "urnetwork:other")
		connect.AssertEqual(t, err != nil, true)

		hmacToken := gojwt.NewWithClaims(gojwt.SigningMethodHS256, newClaims())
		hmacSigned, err := hmacToken.SignedString([]byte("attacker"))
		connect.AssertEqual(t, err, nil)
		_, err = ParseByJwt(ctx, hmacSigned)
		connect.AssertEqual(t, err != nil, true)
	})
}

// legacyByJwt mirrors a token minted before the claims hardening: business
// fields and create_time only, no registered claims at all.
func legacyByJwt() *ByJwt {
	return &ByJwt{
		NetworkId:   server.NewId(),
		UserId:      server.NewId(),
		NetworkName: "test",
		CreateTime:  server.CodecTime(server.NowUtc()),
	}
}

// TestByJwtMissingExpirationGate covers the credential-migration gate: with
// reject_missing_expiration off (the default), tokens minted before the
// claims hardening still parse, while any claim that is present — and the
// signature — is validated as strictly as ever. With the gate on, legacy
// tokens are rejected.
func TestByJwtMissingExpirationGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		popOff := Testing_SetRejectMissingExpiration(false)
		defer popOff()
		// expiry-when-present enforcement has its own gate; pin it on here so
		// the presentButWrong cases below cover it, and see
		// TestByJwtRejectExpiredGate for the gate-off behavior
		popExpired := Testing_SetRejectExpired(true)
		defer popExpired()

		legacy := legacyByJwt()
		signed := legacy.Sign()

		parsed, err := ParseByJwt(ctx, signed)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, parsed, nil)
		connect.AssertEqual(t, parsed.NetworkId, legacy.NetworkId)
		connect.AssertEqual(t, parsed.UserId, legacy.UserId)
		connect.AssertEqual(t, parsed.NetworkName, legacy.NetworkName)

		// legacy tokens predate audiences, so they parse for every audience
		_, err = ParseByJwtForAudience(ctx, signed, ByJwtAudienceConnect)
		connect.AssertEqual(t, err, nil)

		// a legacy-era client token (client_id and device_id, no registered
		// claims) parses too
		deviceId := server.NewId()
		clientId := server.NewId()
		legacyClient := legacyByJwt()
		legacyClient.DeviceId = &deviceId
		legacyClient.ClientId = &clientId
		parsedClient, err := ParseByJwtForAudience(ctx, legacyClient.Sign(), ByJwtAudienceConnect)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, *parsedClient.ClientId, clientId)
		connect.AssertEqual(t, *parsedClient.DeviceId, deviceId)

		// claims that are present are still validated with the gate off
		presentButWrong := []struct {
			name   string
			mutate func(*ByJwt)
		}{
			{
				name: "expired",
				mutate: func(claims *ByJwt) {
					claims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Minute))
				},
			},
			{
				name: "future not before",
				mutate: func(claims *ByJwt) {
					claims.NotBefore = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
				},
			},
			{
				name: "wrong issuer",
				mutate: func(claims *ByJwt) {
					claims.Issuer = "attacker"
				},
			},
			{
				name: "wrong audience",
				mutate: func(claims *ByJwt) {
					claims.Audience = gojwt.ClaimStrings{"attacker"}
				},
			},
			{
				name: "subject mismatch",
				mutate: func(claims *ByJwt) {
					claims.Subject = server.NewId().String()
				},
			},
		}
		for _, test := range presentButWrong {
			claims := legacyByJwt()
			test.mutate(claims)
			if _, err := ParseByJwt(ctx, claims.Sign()); err == nil {
				t.Fatalf("gate off accepted a token with an invalid present claim: %s", test.name)
			}
		}

		// the signature is enforced in both modes
		foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		connect.AssertEqual(t, err, nil)
		forgedToken := gojwt.NewWithClaims(gojwt.SigningMethodES256, legacyByJwt())
		forgedSigned, err := forgedToken.SignedString(foreignKey)
		connect.AssertEqual(t, err, nil)
		_, err = ParseByJwt(ctx, forgedSigned)
		connect.AssertNotEqual(t, err, nil)

		// with the gate on, the same legacy token is rejected
		popOn := Testing_SetRejectMissingExpiration(true)
		defer popOn()
		_, err = ParseByJwt(ctx, signed)
		connect.AssertNotEqual(t, err, nil)

		// and a current token is valid in both modes
		fresh := NewByJwt(server.NewId(), server.NewId(), "test", false, false).Sign()
		_, err = ParseByJwt(ctx, fresh)
		connect.AssertEqual(t, err, nil)
		popOn()
		_, err = ParseByJwt(ctx, fresh)
		connect.AssertEqual(t, err, nil)
	})
}

// TestByJwtRejectExpiredGate covers the expired-credential migration gate:
// with reject_expired off (the default), a present-but-passed expiration is
// tolerated in every mode, while not-before and issued-at stay enforced.
// With the gate on, expired tokens are rejected with the usual leeway.
func TestByJwtRejectExpiredGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// pushGates pins both gates and returns a single pop
		pushGates := func(rejectMissing bool, rejectExpired bool) func() {
			popMissing := Testing_SetRejectMissingExpiration(rejectMissing)
			popExpired := Testing_SetRejectExpired(rejectExpired)
			return func() {
				popExpired()
				popMissing()
			}
		}

		// a fully modern token whose lifetime has passed
		expiredByJwt := func() *ByJwt {
			claims := NewByJwt(server.NewId(), server.NewId(), "test", false, false)
			claims.IssuedAt = gojwt.NewNumericDate(server.NowUtc().Add(-25 * time.Hour))
			claims.NotBefore = claims.IssuedAt
			claims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
			return claims
		}

		// gate off: the expired token parses whether or not the
		// missing-expiration gate is on (the gates are independent)
		pop := pushGates(false, false)
		_, err := ParseByJwt(ctx, expiredByJwt().Sign())
		connect.AssertEqual(t, err, nil)
		pop()

		pop = pushGates(true, false)
		_, err = ParseByJwt(ctx, expiredByJwt().Sign())
		connect.AssertEqual(t, err, nil)
		pop()

		// gate off: a legacy token whose only registered claim is a passed
		// exp (the jan-era mints) parses too
		pop = pushGates(false, false)
		legacyExpired := legacyByJwt()
		legacyExpired.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Minute))
		_, err = ParseByJwt(ctx, legacyExpired.Sign())
		connect.AssertEqual(t, err, nil)

		// not-before and issued-at violations are fatal even with both gates
		// off
		futureNbf := legacyByJwt()
		futureNbf.NotBefore = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
		_, err = ParseByJwt(ctx, futureNbf.Sign())
		connect.AssertNotEqual(t, err, nil)

		futureIat := legacyByJwt()
		futureIat.IssuedAt = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
		_, err = ParseByJwt(ctx, futureIat.Sign())
		connect.AssertNotEqual(t, err, nil)
		pop()

		// gate on: expired is rejected, and the clock leeway still applies
		pop = pushGates(false, true)
		_, err = ParseByJwt(ctx, expiredByJwt().Sign())
		connect.AssertNotEqual(t, err, nil)

		withinLeeway := NewByJwt(server.NewId(), server.NewId(), "test", false, false)
		withinLeeway.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-10 * time.Second))
		_, err = ParseByJwt(ctx, withinLeeway.Sign())
		connect.AssertEqual(t, err, nil)

		// a current token is valid in every mode
		fresh := NewByJwt(server.NewId(), server.NewId(), "test", false, false).Sign()
		_, err = ParseByJwt(ctx, fresh)
		connect.AssertEqual(t, err, nil)
		pop()
	})
}

// TestAuthVaultSettings covers the auth.yml read: the
// reject_missing_expiration and reject_expired keys are honored, and an
// absent key or file means false (allow).
func TestAuthVaultSettings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		popBoth := server.Vault.PushSimpleResource("auth.yml", []byte("reject_missing_expiration: true\nreject_expired: true\n"))
		connect.AssertEqual(t, loadRejectMissingExpiration(), true)
		connect.AssertEqual(t, loadRejectExpired(), true)
		popBoth()

		popFalse := server.Vault.PushSimpleResource("auth.yml", []byte("reject_missing_expiration: false\nreject_expired: false\n"))
		connect.AssertEqual(t, loadRejectMissingExpiration(), false)
		connect.AssertEqual(t, loadRejectExpired(), false)
		popFalse()

		// each key defaults to false when the other is the only one set
		popOne := server.Vault.PushSimpleResource("auth.yml", []byte("reject_missing_expiration: true\n"))
		connect.AssertEqual(t, loadRejectExpired(), false)
		popOne()

		popOther := server.Vault.PushSimpleResource("auth.yml", []byte("unrelated: true\n"))
		connect.AssertEqual(t, loadRejectMissingExpiration(), false)
		connect.AssertEqual(t, loadRejectExpired(), false)
		popOther()
	})
}

// The lifetime is asserted rather than assumed because clients schedule their
// own rotation against it, and the two have already fallen out of step once: a
// move to 24h left every non-refreshing client silently dark after a day, while
// staying connected and accepting traffic it could not carry.
func TestByJwtLifetimeIsThirtyDays(t *testing.T) {
	claims := NewByJwt(server.NewId(), server.NewId(), "test", false, false)
	lifetime := claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time)
	connect.AssertEqual(t, lifetime, 30*24*time.Hour)
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
