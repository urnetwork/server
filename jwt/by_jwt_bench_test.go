package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"testing"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

func benchMethodForKey(key crypto.PrivateKey) gojwt.SigningMethod {
	switch k := key.(type) {
	case *ecdsa.PrivateKey:
		switch k.Curve.Params().N.BitLen() {
		case 256:
			return gojwt.SigningMethodES256
		case 384:
			return gojwt.SigningMethodES384
		default:
			return gojwt.SigningMethodES512
		}
	case *rsa.PrivateKey:
		return gojwt.SigningMethodRS512
	}
	return nil
}

func benchSign(claims gojwt.Claims, key crypto.PrivateKey, withKid bool) string {
	token := gojwt.NewWithClaims(benchMethodForKey(key), claims)
	if withKid {
		kid, _ := jwtKid(publicKey(key))
		token.Header["kid"] = kid
	}
	signed, err := token.SignedString(key)
	if err != nil {
		panic(err)
	}
	return signed
}

// benchParseOld is a faithful copy of the pre-change ParseByJwt: try every key
// in order, then convert MapClaims -> ByJwt via a json marshal/unmarshal
// round-trip. fixByJwt is included so the scope matches the new ParseByJwt.
func benchParseOld(ctx context.Context, jwtSigned string) *ByJwt {
	parserOptions := []gojwt.ParserOption{gojwt.WithoutClaimsValidation()}
	var token *gojwt.Token
	var err error
	for _, byPrivateKey := range byPrivateKeys() {
		token, err = gojwt.Parse(jwtSigned, func(t *gojwt.Token) (any, error) {
			return byPrivateKey.(interface{ Public() crypto.PublicKey }).Public(), nil
		}, parserOptions...)
		if err == nil {
			break
		}
	}
	if token == nil {
		return nil
	}
	claims := token.Claims.(gojwt.MapClaims)
	claimsJson, _ := json.Marshal(claims)
	byJwt := &ByJwt{}
	json.Unmarshal(claimsJson, byJwt)
	fixByJwt(ctx, byJwt)
	return byJwt
}

// freshKey is what sign() uses (byPrivateKeys()[0]); oldKey is the last-loaded
// key, the worst case for the old all-keys scan.
func benchKeys() (freshKey crypto.PrivateKey, oldKey crypto.PrivateKey) {
	keys := byPrivateKeys()
	return keys[0], keys[len(keys)-1]
}

func benchClaims() *ByJwt {
	return NewByJwt(server.NewId(), server.NewId(), "test", false, false)
}

// These benchmarks need only the vault keys (from the WARP_* env vars), not the
// test DB: benchClaims sets NetworkId so fixByJwt early-returns without redis/pg.

// fresh token (signed by key[0], the common case): isolates the json round-trip
func BenchmarkParseByJwt_Old_FreshToken(b *testing.B) {
	ctx := context.Background()
	freshKey, _ := benchKeys()
	signed := benchSign(benchClaims(), freshKey, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchParseOld(ctx, signed)
	}
}

func BenchmarkParseByJwt_New_FreshToken(b *testing.B) {
	ctx := context.Background()
	signed := benchClaims().Sign() // real path: sets kid, signs with key[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseByJwt(ctx, signed)
	}
}

// old token (signed by the last key): the old scan verifies against every key
// before the match; the new kid lookup goes straight to it
func BenchmarkParseByJwt_Old_OldToken(b *testing.B) {
	ctx := context.Background()
	_, oldKey := benchKeys()
	signed := benchSign(benchClaims(), oldKey, false)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchParseOld(ctx, signed)
	}
}

func BenchmarkParseByJwt_New_OldToken(b *testing.B) {
	ctx := context.Background()
	_, oldKey := benchKeys()
	signed := benchSign(benchClaims(), oldKey, true) // kid present -> direct lookup
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ParseByJwt(ctx, signed)
	}
}
