// The no-extend fallback (open question 1 of the 2026-11-18 session fix
// plan, for the owner): when Redis does not register a mint's session within
// sessionMintRedisBudget and the mint carries that session from the credential
// it was presented (a refresh, or a client minted under the credential, such
// as a hosted proxy's child), the token is signed on the session without
// extending it, and counted in urnetwork_session_mint_unextended_total{kind}.
// The caller had a 503 before.
//
// Registration is the check-and-extend of REVOKE-FINAL.md §12: it refuses a
// revoked session, and it extends the session's horizon through the new
// token's, and a revocation retains its marker through that horizon. Signed
// without it, the token expires no later than the presented credential, and
// the presented credential's own mint registered the session through that
// credential's horizon. A revocation therefore still outlives every token of
// its session. The presented credential was checked as every check treats it
// while Redis does not answer (decision 3): the mint's transaction accepted it
// unchecked, as API and connect checks do. So the token grants the same
// session, for no longer, with the claims the mint derives from SQL. When
// Redis answers again, the next refresh registers and extends the session as
// before.
//
// What stays unavailable: a presented credential past its expiry, which gains
// nothing from a token with the same expiry, and a hosted proxy's own mint on
// its stored session, which has no presented credential to bound its token by
// (MintHostedSession). Their callers keep what they hold.
//
// The hook is untagUnregisteredSessionMintInTx (session_mint_untagged.go),
// which RegisterAndSignInTx calls when the registration fails.
package session

import (
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// The session a renewal, or a credential minted from a renewal, carries from
// the credential it was presented, and the earliest expiry along the way.
type carriedSession struct {
	sessionId server.Id
	expiresAt time.Time
}

// What a renewal of self carries: self's session, with the earlier of self's
// expiry and the expiry self carries itself, or nil for a credential without a
// session or an expiry. Renew is called on presented credentials, verified
// tokens, whose expiry their session's registration covers: no mint signs a
// session it did not register, except this fallback, which keeps to the
// carried expiry.
func (self *ByJwt) carriedByRenewal() *carriedSession {
	if self.SessionId == nil || self.ExpiresAt == nil {
		return nil
	}
	carried := &carriedSession{sessionId: *self.SessionId, expiresAt: self.ExpiresAt.Time}
	if self.carried != nil {
		if self.carried.sessionId != carried.sessionId {
			// self's expiry is its own, not one the session is registered for
			return nil
		}
		if self.carried.expiresAt.Before(carried.expiresAt) {
			carried.expiresAt = self.carried.expiresAt
		}
	}
	return carried
}

// The service label comes from the stats push grouping (StartStatsPusher).
var sessionMintUnextendedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_session_mint_unextended_total",
	Help: "Mints that carried their presented credential's session but signed it without extending it, expiring no later than the presented credential, because Redis did not register the session within the mint budget (the no-extend fallback), by session kind: signin, code or client. The next refresh extends the session.",
}, []string{"kind"})

func init() {
	prometheus.MustRegister(sessionMintUnextendedCounter)
	for _, kind := range []string{SessionKindSignin, SessionKindCode, SessionKindClient} {
		sessionMintUnextendedCounter.WithLabelValues(kind)
	}
}

// Readies credential to be signed on the session it carries without a
// registration, when it is signed with the session it carries and the carried
// expiry is still ahead: its expiry is lowered to the carried one, and the
// mint is counted. False leaves credential as it was.
func unextendCarriedSessionMint(credential *ByJwt, kind string) bool {
	carried := credential.carried
	if carried == nil || credential.SessionId == nil || *credential.SessionId != carried.sessionId || credential.ExpiresAt == nil {
		return false
	}
	if !server.NowUtc().Before(carried.expiresAt) {
		return false
	}
	if carried.expiresAt.Before(credential.ExpiresAt.Time) {
		credential.ExpiresAt = gojwt.NewNumericDate(carried.expiresAt)
	}
	sessionMintUnextendedCounter.WithLabelValues(sessionKindClass(kind, credential)).Inc()
	return true
}
