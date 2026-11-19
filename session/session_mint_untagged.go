// R-mint-redis (owner decision 3, 2026-11-18): when Redis does not register a
// mint's session within sessionMintRedisBudget and the mint starts that
// session itself, the token is signed untagged, as tokens were before
// sessions, and counted in urnetwork_session_mint_untagged_total{kind}. No
// tagged token then carries a session Redis does not list, and the token gets
// its session at its next refresh.
//
// A mint starts its session when it chose the id: a new id (a sign-in, a code
// redemption, an API-key or MCP client, a device adoption, a hosted
// bootstrap), or the legacy lineage derived from an untagged credential. The
// presented authority, if any, was untagged, so the untagged token grants
// nothing its caller does not already hold. A fresh session's client binding,
// written earlier in the mint's transaction, is cleared with it, so the next
// refresh, which derives its own lineage, is not refused as
// client_session_changed; a derived lineage keeps its binding, since the next
// refresh derives the same id.
//
// A mint that carries a session from a presented tagged credential or a
// stored binding is not signed untagged: the new token would outlive the
// revocation of its session. It is signed on its session unextended when its
// presented credential bounds it (session_mint_unextended.go), and otherwise
// stays unavailable, and its caller keeps the credential it holds.
//
// The hooks in session_mint.go: PrepareSessionMint and MintHostedSession mark
// the session a mint starts, and RegisterAndSignInTx registers within the
// budget and falls back here.
package session

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

// The session a mint starts, when the mint chose its id.
type sessionMintStart struct {
	sessionId server.Id
	// a new random id, which no client row is bound to before this mint
	fresh bool
}

// The session credential starts at this mint, or nil.
func (self *ByJwt) startsSession() *sessionMintStart {
	if self.mintStart == nil || self.SessionId == nil || *self.SessionId != self.mintStart.sessionId {
		return nil
	}
	return self.mintStart
}

// StartSessionMint gives credential a new session that starts at this mint,
// for a producer that allocates its own candidate id outside its retryable
// callback (device adoption).
func StartSessionMint(credential *ByJwt, sessionId server.Id) {
	credential.SessionId = &sessionId
	credential.mintStart = &sessionMintStart{sessionId: sessionId, fresh: true}
}

// A mint waits at most this long for each Redis step of its registration,
// the top-level client's exemption read, the clock sample and the mint script,
// inside its SQL transaction. A Redis stall then holds a mint's PostgreSQL
// connection for at most three budgets, where RedisAuth alone allows two
// seconds each: at the 380 tagged mints a second S4 measured, two-second waits
// would hold more connections than PgBouncer has.
const sessionMintRedisBudget = RevocationCheckBudget

// SessionMintRedisBudget bounds a Redis read another package makes for a mint
// inside its transaction (model's client cap exemption,
// SetClientCapExemptUntilInTx). Such a read that Redis does not answer within
// it returns ErrSessionStoreUnavailable, and RegisterAndSignInTx falls back as
// for a registration Redis does not answer.
const SessionMintRedisBudget = sessionMintRedisBudget

// The service label comes from the stats push grouping (StartStatsPusher).
var sessionMintUntaggedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_session_mint_untagged_total",
	Help: "Mints that started a session but signed an untagged token because Redis did not register the session within the mint budget (R-mint-redis, decision 3), by session kind: signin, code or client. The token gets its session at its next refresh.",
}, []string{"kind"})

func init() {
	prometheus.MustRegister(sessionMintUntaggedCounter)
	for _, kind := range []string{SessionKindSignin, SessionKindCode, SessionKindClient} {
		sessionMintUntaggedCounter.WithLabelValues(kind)
	}
}

// The §15 kind of a mint's session, as the session cap counts it
// (sessionClass): a network credential's legacy lineage is a sign-in, a
// client credential's is the client's own.
func sessionKindClass(kind string, credential *ByJwt) string {
	return sessionKindLabel(sessionClass(kind, credential.ClientId == nil))
}

// The clock sample and the mint script, each within the mint budget. exempt
// is the provider exemption the mint sets (sessionMintExemption).
func registerSessionMint(ctx context.Context, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool, exempt string) (*registeredSession, error) {
	clockCtx, cancelClock := context.WithTimeout(ctx, sessionMintRedisBudget)
	err := verifySessionMintClock(clockCtx)
	cancelClock()
	if err != nil {
		return nil, err
	}
	registerCtx, cancelRegister := context.WithTimeout(ctx, sessionMintRedisBudget)
	defer cancelRegister()
	return registerNetworkSession(registerCtx, credential, kind, origin, fenceOrigin, server.NowUtc(), exempt)
}

// The fallback for a registration that failed with err, named for its first
// case. When Redis did not answer while the caller still waits, a mint that
// started its session leaves the session out of its token, clears a fresh
// session's client binding, is counted, and returns nil; a mint that carries
// its presented credential's session keeps it, unextended and expiring no
// later than the presented credential (session_mint_unextended.go), and
// returns nil. Any other failure returns err: a refusal by the mint script, a
// clock out of bounds, a session neither started nor carried within its
// presented credential's expiry, or a caller that ended.
func untagUnregisteredSessionMintInTx(ctx context.Context, tx server.PgTx, credential *ByJwt, kind string, err error) error {
	if !errors.Is(err, ErrSessionStoreUnavailable) || ctx.Err() != nil {
		return err
	}
	start := credential.startsSession()
	if start == nil {
		if unextendCarriedSessionMint(credential, kind) {
			return nil
		}
		return err
	}
	if start.fresh && credential.ClientId != nil {
		// only this mint can have bound a client row to a fresh id
		if _, execErr := tx.Exec(ctx, `UPDATE network_client SET session_id=NULL,session_create_time=NULL WHERE network_id=$1 AND client_id=$2 AND session_id=$3`, credential.NetworkId, *credential.ClientId, start.sessionId); execErr != nil {
			return execErr
		}
	}
	credential.SessionId = nil
	credential.mintStart = nil
	sessionMintUntaggedCounter.WithLabelValues(sessionKindClass(kind, credential)).Inc()
	return nil
}
