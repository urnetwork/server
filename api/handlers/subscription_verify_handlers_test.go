package handlers

// JWS-level tests for the client-reported StoreKit transaction path
// (/subscription/verify-apple-transaction): a client push of unauthenticated
// content must clear the SAME pinned-root bar as an App Store webhook
// notification before any claims reach the controller.

import (
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func appleTestSignedTransaction(
	t testing.TB,
	chain *appleTestCertificateChain,
	now time.Time,
	bundleId string,
	environment string,
) string {
	t.Helper()
	return chain.sign(t, gojwt.MapClaims{
		"signedDate":      now.UnixMilli(),
		"bundleId":        bundleId,
		"environment":     environment,
		"appAccountToken": server.NewId().String(),
		"transactionId":   "client-reported-transaction-1",
		"productId":       "supporter_monthly_26",
		"purchaseDate":    now.Add(-time.Hour).UnixMilli(),
		"expiresDate":     now.Add(30 * 24 * time.Hour).UnixMilli(),
		"price":           int64(4990),
	})
}

func TestAppleVerifierVerifyTransactionTrustAndIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	trustedChain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	verifier := appleTestVerifier(trustedChain, now)

	// a well-formed transaction JWS signed by the trusted chain verifies and
	// surfaces its claims
	signed := appleTestSignedTransaction(t, trustedChain, now, "network.ur", "Production")
	claims, err := verifier.verifyTransaction(signed)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, claims["transactionId"], "client-reported-transaction-1")
	connect.AssertEqual(t, claims["productId"], "supporter_monthly_26")

	// a chain not rooted at the pinned roots is rejected, even if internally
	// consistent -- the attacker-builds-their-own-CA case
	untrustedChain := newAppleTestCertificateChain(t, now, now.Add(24*time.Hour))
	untrustedSigned := appleTestSignedTransaction(t, untrustedChain, now, "network.ur", "Production")
	_, err = verifier.verifyTransaction(untrustedSigned)
	connect.AssertEqual(t, err != nil, true)

	// a tampered payload fails signature verification
	_, err = verifier.verifyTransaction(tamperAppleTestJws(signed))
	connect.AssertEqual(t, err != nil, true)

	// a transaction for someone else's app is rejected
	wrongBundle := appleTestSignedTransaction(t, trustedChain, now, "other.bundle", "Production")
	_, err = verifier.verifyTransaction(wrongBundle)
	connect.AssertEqual(t, err != nil, true)

	// an environment outside the configured allowlist is rejected
	wrongEnvironment := appleTestSignedTransaction(t, trustedChain, now, "network.ur", "Development")
	_, err = verifier.verifyTransaction(wrongEnvironment)
	connect.AssertEqual(t, err != nil, true)

	// garbage is rejected
	_, err = verifier.verifyTransaction("not-a-jws")
	connect.AssertEqual(t, err != nil, true)
}

// TestAppleVerifierVerifyTransactionAcceptsOldSignedDate pins the retry
// contract: the client may report a persisted proof days after purchase, so
// the webhook freshness window must NOT apply to a transaction JWS (the
// certificate chain is still validated at the JWS's own signing time).
func TestAppleVerifierVerifyTransactionAcceptsOldSignedDate(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	// signed 3 days ago -- far outside the 24h notification window
	old := now.Add(-3 * 24 * time.Hour)
	// the chain must be anchored so its validity covers the SIGNING time:
	// the verifier checks the chain at the JWS's own signedDate (the leaf
	// begins anchor-12h in the helper), so anchoring at `now` would make the
	// certs not-yet-valid at `old` and fail for the wrong reason
	chain := newAppleTestCertificateChain(t, old.Add(-24*time.Hour), now.Add(24*time.Hour))
	verifier := appleTestVerifier(chain, now)
	signed := chain.sign(t, gojwt.MapClaims{
		"signedDate":      old.UnixMilli(),
		"bundleId":        "network.ur",
		"environment":     "Production",
		"appAccountToken": server.NewId().String(),
		"transactionId":   "client-reported-transaction-old",
		"productId":       "supporter_monthly_26",
		"purchaseDate":    old.Add(-time.Hour).UnixMilli(),
		"expiresDate":     old.Add(30 * 24 * time.Hour).UnixMilli(),
		"price":           int64(4990),
	})
	claims, err := verifier.verifyTransaction(signed)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, claims["transactionId"], "client-reported-transaction-old")
}
