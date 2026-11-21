package sidecarhost

// The Authenticator and the rev/ reader's signature check under a given svc- verifier, so a block a test runs
// verifies under keys the test made (e2e-hosts-in-process): every block of an env verifies under the one svc- set
// its issuer signs with, which a block mounts as oauth-jwks-svc.json and the process reads once
// (oauth.VerifyServiceToken), so several blocks in one test process share that read unless each is given its own.

import (
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/oauth"
)

// A service token's claims by verifier, or by the process's verifier when it is nil.
func serviceTokenVerify(verifier *oauth.ServiceTokenVerifier) func(token string, audience string) (*oauth.ServiceTokenClaims, error) {
	if verifier == nil {
		return oauth.VerifyServiceToken
	}
	return verifier.Verify
}

// NewAuthenticator's, verifying under verifier: nil is the process's.
func NewAuthenticatorWithVerifier(verifier *oauth.ServiceTokenVerifier) *Authenticator {
	return &Authenticator{verify: serviceTokenVerify(verifier)}
}

// NewRevocationAuthenticator's, verifying under verifier: nil is the process's. A nil revocations answers
// audience's tokens unavailable, never an unchecked allow.
func NewRevocationAuthenticatorWithVerifier(audience string, revocations RevocationChecker, verifier *oauth.ServiceTokenVerifier) *Authenticator {
	return &Authenticator{verify: serviceTokenVerify(verifier), revocationAudience: audience, revocations: revocations}
}

// What a rev/ object's signature is checked by for sidecar.NewRevocationChecker: verifier's svc- keys, or the
// process's verifier's when it is nil (oauth.VerifyRevocation).
func RevocationVerify(verifier *oauth.ServiceTokenVerifier) sidecar.RevocationVerifyFunction {
	if verifier == nil {
		return oauth.VerifyRevocation
	}
	return verifier.VerifyRevocation
}
