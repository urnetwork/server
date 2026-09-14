// Package onboarding holds the pieces of the onboarding program that more than
// one stream builds against: the signed landing/feedback token and the step ->
// in-app destination map. The campaign engine mints tokens for its email links;
// the api verifies them on POST /onboarding/click and
// GET /onboarding/feedback/{token}.
package onboarding

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
)

// TokenKeyPurpose labels the HMAC key derived from the jwt signing key.
const TokenKeyPurpose = "urnetwork-onboarding-token-v1"

// MaxTokenLength bounds what the endpoints will even try to parse.
const MaxTokenLength = 512

var (
	ErrTokenInvalid = errors.New("invalid onboarding token")
	ErrTokenExpired = errors.New("expired onboarding token")
)

// TokenClaims is what a landing/feedback token carries: the network and step it
// was minted for, an expiry, and for feedback links the rating or reason the
// one-tap button stands for.
type TokenClaims struct {
	NetworkId server.Id `json:"n"`
	// Step is the template family used to choose the landing destination.
	// FlowStep is the actual E1-E5 campaign position. They are deliberately
	// separate: E3 and E5 can use the same template.
	Step     string `json:"s"`
	FlowStep string `json:"f,omitempty"`
	// unix seconds
	ExpiresAt int64  `json:"e"`
	Rating    int    `json:"r,omitempty"`
	Reason    string `json:"w,omitempty"`
}

// Expiry is the claims' expiry as a time.
func (c *TokenClaims) Expiry() time.Time {
	return time.Unix(c.ExpiresAt, 0).UTC()
}

// SignToken encodes and signs claims with one key: base64url(json) "." base64url(hmac-sha256).
func SignToken(key []byte, claims *TokenClaims) (string, error) {
	if claims == nil || claims.NetworkId == (server.Id{}) || strings.TrimSpace(claims.Step) == "" ||
		(claims.FlowStep != "" && !IsFlowStep(claims.FlowStep)) {
		return "", ErrTokenInvalid
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(sign(key, body)), nil
}

func sign(key []byte, body string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return mac.Sum(nil)
}

// ParseToken verifies a token against each key in turn and checks its expiry.
// ErrTokenExpired carries the parsed claims alongside, so a landing page can
// still route an expired link without attributing it.
func ParseToken(keys [][]byte, token string, now time.Time) (*TokenClaims, error) {
	token = strings.TrimSpace(token)
	if token == "" || MaxTokenLength < len(token) {
		return nil, ErrTokenInvalid
	}
	body, signature, ok := strings.Cut(token, ".")
	if !ok || body == "" || signature == "" {
		return nil, ErrTokenInvalid
	}
	signatureBytes, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	verified := false
	for _, key := range keys {
		if hmac.Equal(sign(key, body), signatureBytes) {
			verified = true
			break
		}
	}
	if !verified {
		return nil, ErrTokenInvalid
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	var claims TokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrTokenInvalid
	}
	if claims.NetworkId == (server.Id{}) || strings.TrimSpace(claims.Step) == "" ||
		(claims.FlowStep != "" && !IsFlowStep(claims.FlowStep)) {
		return nil, ErrTokenInvalid
	}
	if claims.ExpiresAt <= 0 || !now.Before(claims.Expiry()) {
		return &claims, ErrTokenExpired
	}
	return &claims, nil
}

// IsFlowStep reports whether step is one of the five durable campaign
// positions. Keep this closed so analytics labels cannot grow without bound.
func IsFlowStep(step string) bool {
	switch step {
	case StepE1, StepE2, StepE3, StepE4, StepE5:
		return true
	default:
		return false
	}
}

// FlowSteps returns the ordered, bounded flow-step vocabulary.
func FlowSteps() []string {
	return []string{StepE1, StepE2, StepE3, StepE4, StepE5}
}

// NewToken signs claims with the deployment's current key.
func NewToken(claims *TokenClaims) (string, error) {
	keys := jwt.DerivedKeys(TokenKeyPurpose)
	if len(keys) == 0 {
		return "", errors.New("no signing key")
	}
	return SignToken(keys[0], claims)
}

// Parse verifies a token against every key the deployment has.
func Parse(token string, now time.Time) (*TokenClaims, error) {
	return ParseToken(jwt.DerivedKeys(TokenKeyPurpose), token, now)
}

// In-app destinations a landing page routes to (mmm/onboarding/PLAN.md
// "Landing pages"): the apps register these as deep-link targets.
const (
	DestinationConnect  = "onboarding/connect"
	DestinationWidgets  = "onboarding/widgets"
	DestinationOffer    = "onboarding/offer"
	DestinationFeedback = "onboarding/feedback"
)

// Destination maps a campaign step to its in-app destination by the step's
// family keyword (e1_connect -> connect, e2_widget -> widgets, e3_last_chance and
// e4b_offer -> offer, e4_feedback -> feedback). Unknown steps land on the
// Connect tab, which is always a safe place to arrive.
func Destination(step string) string {
	s := strings.ToLower(step)
	switch {
	case strings.Contains(s, "feedback"), strings.Contains(s, "rating"):
		return DestinationFeedback
	case strings.Contains(s, "widget"):
		return DestinationWidgets
	case strings.Contains(s, "offer"), strings.Contains(s, "last_chance"), strings.Contains(s, "lastchance"), strings.Contains(s, "pro"):
		return DestinationOffer
	default:
		return DestinationConnect
	}
}
