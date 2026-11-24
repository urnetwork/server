package session

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func validatorClassTestKey(seed byte) []byte {
	key := make([]byte, 32)
	key[0] = seed
	return key
}

func validatorClassTestToken(t *testing.T, key []byte, clientId server.Id, now time.Time, lifetime time.Duration) string {
	t.Helper()
	token, err := signValidatorClass(key, ValidatorClassClaims{ClientId: clientId, NetworkId: server.NewId(), Hotkey: "11", Activation: "22",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(lifetime).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// A minted token verifies as its own client's class until it expires.
func TestValidatorClassTokenVerifiesForItsClientUntilExpiry(t *testing.T) {
	key := validatorClassTestKey(1)
	verifier := newValidatorClassVerifier(func() [][]byte { return [][]byte{key} }, 16, 16)
	now := time.Unix(1_800_000_000, 0)
	clientId := server.NewId()
	token := validatorClassTestToken(t, key, clientId, now, 4*time.Minute)
	for range 3 {
		if claims, ok := verifier.Verify(token, clientId, now); !ok || claims.ClientId != clientId {
			t.Fatalf("a minted class token did not verify: %+v", claims)
		}
	}
	if stats := verifier.Stats(); stats.Verifications != 1 || stats.Cached != 2 {
		t.Fatalf("a verified token was not answered from the cache: %+v", stats)
	}
	if _, ok := verifier.Verify(token, server.NewId(), now); ok {
		t.Fatal("a class token verified for another client")
	}
	if _, ok := verifier.Verify(token, clientId, now.Add(4*time.Minute)); ok {
		t.Fatal("an expired class token verified")
	}
	// a key rotation keeps an older key's token verifying
	rotated := newValidatorClassVerifier(func() [][]byte { return [][]byte{validatorClassTestKey(2), key} }, 16, 16)
	if _, ok := rotated.Verify(token, clientId, now); !ok {
		t.Fatal("a token of the older key stopped verifying")
	}
}

// 10k requests with junk class headers make no HMAC verification: each fails
// the cheap checks of shape, audience, expiry or client.
func TestValidatorClassJunkCostsNoVerification(t *testing.T) {
	key := validatorClassTestKey(1)
	verifier := newValidatorClassVerifier(func() [][]byte { return [][]byte{key} }, 16, 16)
	now := time.Unix(1_800_000_000, 0)
	clientId := server.NewId()
	encode := func(claims map[string]any) string {
		payload, _ := json.Marshal(claims)
		return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	}
	junk := []func(int) string{
		func(i int) string { return fmt.Sprintf("bogus-%d", i) },
		func(i int) string { return fmt.Sprintf("a.b.c%d", i) },
		func(i int) string {
			return encode(map[string]any{"aud": "urnetwork:api", "client_id": clientId, "exp": now.Add(time.Minute).Unix(), "n": i})
		},
		func(i int) string {
			return encode(map[string]any{"aud": ValidatorClassAudience, "client_id": server.NewId(), "exp": now.Add(time.Minute).Unix(), "n": i})
		},
		func(i int) string {
			return encode(map[string]any{"aud": ValidatorClassAudience, "client_id": clientId, "exp": now.Add(-time.Second).Unix(), "n": i})
		},
		func(i int) string {
			return encode(map[string]any{"aud": ValidatorClassAudience, "client_id": clientId, "exp": now.Add(time.Hour).Unix(), "n": i})
		},
	}
	for i := range 10_000 {
		if _, ok := verifier.Verify(junk[i%len(junk)](i), clientId, now); ok {
			t.Fatal("a junk class header verified")
		}
	}
	if stats := verifier.Stats(); stats.Verifications != 0 || stats.Refused != 10_000 {
		t.Fatalf("junk class headers reached verification: %+v", stats)
	}
}

// A forged token that passes the cheap checks is verified once; repeats are
// answered from the refused cache.
func TestValidatorClassForgeryIsVerifiedOnce(t *testing.T) {
	key := validatorClassTestKey(1)
	verifier := newValidatorClassVerifier(func() [][]byte { return [][]byte{key} }, 16, 16)
	now := time.Unix(1_800_000_000, 0)
	clientId := server.NewId()
	forged := validatorClassTestToken(t, validatorClassTestKey(9), clientId, now, 4*time.Minute)
	for range 10_000 {
		if _, ok := verifier.Verify(forged, clientId, now); ok {
			t.Fatal("a forged class token verified")
		}
	}
	if stats := verifier.Stats(); stats.Verifications != 1 || stats.Cached != 9_999 {
		t.Fatalf("a repeated forgery was verified again: %+v", stats)
	}
}

// Neither cache outgrows its capacity, however many distinct tokens arrive.
func TestValidatorClassCachesAreBounded(t *testing.T) {
	key := validatorClassTestKey(1)
	verifier := newValidatorClassVerifier(func() [][]byte { return [][]byte{key} }, 8, 4)
	now := time.Unix(1_800_000_000, 0)
	for i := range 1000 {
		clientId := server.NewId()
		verifier.Verify(validatorClassTestToken(t, key, clientId, now, time.Minute+time.Duration(i)*time.Millisecond), clientId, now)
		verifier.Verify(validatorClassTestToken(t, validatorClassTestKey(9), clientId, now, time.Minute+time.Duration(i)*time.Millisecond), clientId, now)
	}
	if len(verifier.verified.expiries) > 8 || len(verifier.refused.expiries) > 4 || len(verifier.verified.order) > 16 || len(verifier.refused.order) > 8 {
		t.Fatalf("class caches outgrew their capacity: %d/%d verified, %d/%d refused",
			len(verifier.verified.expiries), len(verifier.verified.order), len(verifier.refused.expiries), len(verifier.refused.order))
	}
}

// A token signed by the minting key is exactly what the verifier checks.
func TestValidatorClassMintSignsWithTheDerivedKey(t *testing.T) {
	key := validatorClassTestKey(1)
	now := time.Unix(1_800_000_000, 0)
	token := validatorClassTestToken(t, key, server.NewId(), now, time.Minute)
	body, mac, found := cutValidatorClassTestToken(token)
	if !found || !hmac.Equal(validatorClassMac(key, body), mac) {
		t.Fatal("the class token's mac is not the derived key's")
	}
	if _, err := signValidatorClass(key, ValidatorClassClaims{IssuedAt: now.Unix(), ExpiresAt: now.Unix()}); err == nil {
		t.Fatal("a class token without a client or lifetime was minted")
	}
}

func cutValidatorClassTestToken(token string) (string, []byte, bool) {
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			mac, err := base64.RawURLEncoding.DecodeString(token[i+1:])
			return token[:i], mac, err == nil
		}
	}
	return "", nil, false
}
