package onboarding

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// TestTokenRoundTrip pins the codec: sign, parse, expiry, key rotation, tamper
// and garbage.
func TestTokenRoundTrip(t *testing.T) {
	keyA := []byte(strings.Repeat("a", 32))
	keyB := []byte(strings.Repeat("b", 32))
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	networkId := server.NewId()

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
	_, err = ParseToken([][]byte{keyA}, body+"."+signature[:len(signature)-1]+"x", now)
	connect.AssertEqual(t, ErrTokenInvalid, err)

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
