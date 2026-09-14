// Synthetic tokens pin signing, canonical encodings, and landing destinations.
package onboarding

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// TestTokenRoundTrip pins the codec: sign, parse, expiry, key rotation, tamper
// and garbage.
func TestTokenRoundTrip(t *testing.T) {
	keyA := []byte(strings.Repeat("a", 32))
	keyB := []byte(strings.Repeat("b", 32))
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	// This synthetic id makes the signature end in w, whose unused bits alias x.
	networkId := server.Id{15: 11}

	claims := &TokenClaims{
		NetworkId: networkId,
		Step:      "e4_feedback",
		FlowStep:  StepE4,
		ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
		Rating:    4,
		Reason:    "too_slow",
	}
	token, err := SignToken(keyA, claims)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, true, len(token) <= MaxTokenLength)
	connect.AssertEqual(t, false, strings.ContainsAny(token, "+/= "))

	parsed, err := ParseToken([][]byte{keyA}, token, now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, networkId, parsed.NetworkId)
	connect.AssertEqual(t, "e4_feedback", parsed.Step)
	connect.AssertEqual(t, StepE4, parsed.FlowStep)
	connect.AssertEqual(t, 4, parsed.Rating)
	connect.AssertEqual(t, "too_slow", parsed.Reason)
	connect.AssertEqual(t, claims.ExpiresAt, parsed.ExpiresAt)

	// rotation: verify against every key, in any order
	parsed, err = ParseToken([][]byte{keyB, keyA}, token, now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, "e4_feedback", parsed.Step)

	// the wrong key alone is invalid
	_, err = ParseToken([][]byte{keyB}, token, now)
	connect.AssertEqual(t, ErrTokenInvalid, err)

	// expired: the claims still come back so a landing page can route
	expired, err := ParseToken([][]byte{keyA}, token, now.Add(31*24*time.Hour))
	connect.AssertEqual(t, ErrTokenExpired, err)
	connect.AssertEqual(t, "e4_feedback", expired.Step)
	_, err = ParseToken([][]byte{keyA}, token, time.Unix(claims.ExpiresAt, 0))
	connect.AssertEqual(t, ErrTokenExpired, err)

	// tamper with the body
	body, signature, _ := strings.Cut(token, ".")
	tampered := body[:len(body)-2] + "AA." + signature
	_, err = ParseToken([][]byte{keyA}, tampered, now)
	connect.AssertEqual(t, ErrTokenInvalid, err)
	// tamper with the signature
	connect.AssertEqual(t, "w", signature[len(signature)-1:])
	_, err = ParseToken([][]byte{keyA}, body+"."+signature[:len(signature)-1]+"x", now)
	connect.AssertEqual(t, ErrTokenInvalid, err)
	// Also change authenticated bytes while preserving canonical base64url.
	signatureBytes, err := base64.RawURLEncoding.DecodeString(signature)
	connect.AssertEqual(t, nil, err)
	signatureBytes[0] ^= 1
	_, err = ParseToken([][]byte{keyA}, body+"."+base64.RawURLEncoding.EncodeToString(signatureBytes), now)
	connect.AssertEqual(t, ErrTokenInvalid, err)

	// Surrounding whitespace remains an input-normalization boundary.
	parsed, err = ParseToken([][]byte{keyA}, " \r\n\t"+token+"\t\r\n ", now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, networkId, parsed.NetworkId)

	// garbage
	for _, bad := range []string{"", ".", "abc", "abc.", ".abc", "not base64!.sig", strings.Repeat("a", MaxTokenLength+1) + ".b"} {
		_, err = ParseToken([][]byte{keyA}, bad, now)
		connect.AssertEqual(t, ErrTokenInvalid, err)
	}

	// no keys at all
	_, err = ParseToken(nil, token, now)
	connect.AssertEqual(t, ErrTokenInvalid, err)

	// unsignable claims
	_, err = SignToken(keyA, &TokenClaims{Step: "x", ExpiresAt: 1})
	connect.AssertEqual(t, ErrTokenInvalid, err)
	_, err = SignToken(keyA, &TokenClaims{NetworkId: networkId, ExpiresAt: 1})
	connect.AssertEqual(t, ErrTokenInvalid, err)
	_, err = SignToken(keyA, &TokenClaims{NetworkId: networkId, Step: "e1_connect", FlowStep: "e99", ExpiresAt: 1})
	connect.AssertEqual(t, ErrTokenInvalid, err)
	_, err = SignToken(keyA, nil)
	connect.AssertEqual(t, ErrTokenInvalid, err)

	// a token without an expiry is expired
	noExpiry, _ := SignToken(keyA, &TokenClaims{NetworkId: networkId, Step: "e1_connect"})
	_, err = ParseToken([][]byte{keyA}, noExpiry, now)
	connect.AssertEqual(t, ErrTokenExpired, err)

	// Tokens minted before flow-step attribution remain valid.
	legacy, err := SignToken(keyA, &TokenClaims{NetworkId: networkId, Step: "e3_last_chance", ExpiresAt: now.Add(time.Hour).Unix()})
	connect.AssertEqual(t, nil, err)
	legacyClaims, err := ParseToken([][]byte{keyA}, legacy, now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, "", legacyClaims.FlowStep)
}

// Every nonzero trailing-bit spelling and embedded newline decodes to the same
// HMAC bytes with Go's permissive decoder, but must be rejected as a token.
func TestTokenSignatureEncoding(t *testing.T) {
	key := []byte("synthetic-onboarding-signing-key")
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	token, err := SignToken(key, &TokenClaims{
		NetworkId: server.Id{15: 1},
		Step:      "e1_connect",
		ExpiresAt: now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, signature, _ := strings.Cut(token, ".")
	signatureBytes, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		t.Fatal(err)
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	lastIndex := strings.IndexByte(alphabet, signature[len(signature)-1])
	aliases := []string{
		signature[:1] + "\r" + signature[1:],
		signature[:1] + "\n" + signature[1:],
		signature[:1] + "\r\n" + signature[1:],
	}
	for unusedBits := 1; unusedBits < 4; unusedBits++ {
		aliases = append(aliases, signature[:len(signature)-1]+string(alphabet[lastIndex+unusedBits]))
	}
	for _, alias := range aliases {
		aliasBytes, err := base64.RawURLEncoding.DecodeString(alias)
		if err != nil || !bytes.Equal(signatureBytes, aliasBytes) {
			t.Fatalf("signature fixture %q must preserve the decoded HMAC: %v", alias, err)
		}
		parsed, err := ParseToken([][]byte{key}, body+"."+alias, now)
		if err != ErrTokenInvalid || parsed != nil {
			t.Errorf("signature alias %q: claims present = %t, error = %v", alias, parsed != nil, err)
		}
	}
}

// Authenticated payload aliases exercise decoding after signature verification;
// three payload lengths cover both trailing-bit widths and no trailing bits.
func TestTokenPayloadEncoding(t *testing.T) {
	key := []byte("synthetic-onboarding-signing-key")
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for reasonLength := 1; reasonLength <= 3; reasonLength++ {
		claims := &TokenClaims{
			NetworkId: server.Id{15: 1},
			Step:      "e1_connect",
			ExpiresAt: now.Add(time.Hour).Unix(),
			Reason:    strings.Repeat("x", reasonLength),
		}
		token, err := SignToken(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseToken([][]byte{key}, token, now)
		if err != nil || parsed == nil || *parsed != *claims {
			t.Fatalf("canonical payload with reason length %d failed: %v", reasonLength, err)
		}
		body, signature, _ := strings.Cut(token, ".")
		payload, err := base64.RawURLEncoding.DecodeString(body)
		if err != nil {
			t.Fatal(err)
		}
		aliases := []string{
			body[:1] + "\r" + body[1:],
			body[:1] + "\n" + body[1:],
			body[:1] + "\r\n" + body[1:],
		}
		unusedBitCount := (3 - len(payload)%3) % 3 * 2
		lastIndex := strings.IndexByte(alphabet, body[len(body)-1])
		for unusedBits := 1; unusedBits < 1<<unusedBitCount; unusedBits++ {
			aliases = append(aliases, body[:len(body)-1]+string(alphabet[lastIndex+unusedBits]))
		}
		for _, alias := range aliases {
			aliasBytes, err := base64.RawURLEncoding.DecodeString(alias)
			if err != nil || !bytes.Equal(payload, aliasBytes) {
				t.Fatalf("payload fixture must preserve decoded claims: %v", err)
			}
			// An existing signature authenticates the encoded body, not its JSON.
			parsed, err := ParseToken([][]byte{key}, alias+"."+signature, now)
			if err != ErrTokenInvalid || parsed != nil {
				t.Errorf("unsigned payload alias with reason length %d was accepted", reasonLength)
			}
			aliasSignature := base64.RawURLEncoding.EncodeToString(sign(key, alias))
			parsed, err = ParseToken([][]byte{key}, alias+"."+aliasSignature, now)
			if err != ErrTokenInvalid || parsed != nil {
				t.Errorf("signed payload alias with reason length %d: claims present = %t, error = %v", reasonLength, parsed != nil, err)
			}
		}
	}
}

func TestDestination(t *testing.T) {
	connect.AssertEqual(t, DestinationConnect, Destination("e1_connect"))
	connect.AssertEqual(t, DestinationWidgets, Destination("e2_widget"))
	connect.AssertEqual(t, DestinationOffer, Destination("e3_last_chance"))
	connect.AssertEqual(t, DestinationFeedback, Destination("e4_feedback"))
	connect.AssertEqual(t, DestinationOffer, Destination("e4b_offer"))
	connect.AssertEqual(t, DestinationOffer, Destination("e5b_last_chance"))
	connect.AssertEqual(t, DestinationConnect, Destination("something_else"))
	connect.AssertEqual(t, DestinationConnect, Destination(""))
}
