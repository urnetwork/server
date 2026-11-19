// A revocation check that cannot reach Redis accepts the credential (owner
// decision 3, 2026-11-18: "Redis unreachable: accept and alert"; decision 3b
// the same day extends it from session tokens to client credentials). The
// check is the Redis read that says whether a credential was revoked: the
// revocation record of a tagged credential (revocation_record.go) and the
// session marker (CheckSession). When that read fails, or takes longer than
// RevocationCheckBudget, while the caller still waits for its answer, the
// credential is accepted and the acceptance is counted in
// urnetwork_session_check_unavailable_total{path,credential}. No caller gets a
// 503 for this reason: not the api's authentication, connect admission, a
// lease recheck, the hosted parent authority or a mint's own check.
//
// What still refuses:
//   - every check that needs no Redis: the signature and claims, the session
//     horizon, and this process's remembered refusals;
//   - a record that answers: its refusals, and its misses, which the SQL state
//     query answers exactly as before;
//   - an unavailable SQL state query, which is a database outage, not this
//     decision.
//
// A caller that ended during the check (canceled, or past its own deadline)
// is not accepted and not counted: nothing waits for the answer.
package session

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
)

// The longest one revocation check waits for Redis before it accepts the
// credential unchecked. It bounds what a Redis stall adds to authentication:
// one record read on the api, two reads and the connection authority's
// publication on a connect handshake. The budget sits above the slowest normal
// reads: on 2026-11-18 about 1.2% of connect's session hint reads overran
// their 100ms budget, while the api and proxy reads of the record failed about
// one time in 1,000 to 2,000 with two seconds to answer.
const RevocationCheckBudget = 250 * time.Millisecond

// The auth paths a check is counted under, a closed vocabulary.
const (
	revocationCheckPathApi         = "api"
	revocationCheckPathConnectH1   = "connect_h1"
	revocationCheckPathConnectH3   = "connect_h3"
	revocationCheckPathLease       = "lease"
	revocationCheckPathHosted      = "hosted"
	revocationCheckPathProber      = "prober"
	revocationCheckPathTransaction = "transaction"
	revocationCheckPathOther       = "other"
)

var revocationCheckPaths = []string{
	revocationCheckPathApi,
	revocationCheckPathConnectH1,
	revocationCheckPathConnectH3,
	revocationCheckPathLease,
	revocationCheckPathHosted,
	revocationCheckPathProber,
	revocationCheckPathTransaction,
	revocationCheckPathOther,
}

// The credential label: "session" for a network credential, whose session is
// its only revocation lineage, and "client" for a client credential.
const (
	revocationCheckCredentialSession = "session"
	revocationCheckCredentialClient  = "client"
)

// The service label comes from the stats push grouping (StartStatsPusher). A
// metric must not carry a grouping label itself: the push of the whole process
// fails on one.
var sessionCheckUnavailableCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_session_check_unavailable_total",
	Help: "Credentials accepted without their revocation check because Redis did not answer it within the check budget (owner decision 3, 2026-11-18), by auth path and credential (session or client). A caller that ended during the check is not counted.",
}, []string{"path", "credential"})

func init() {
	prometheus.MustRegister(sessionCheckUnavailableCounter)
	// every series exists from the start, so a rate is zero rather than absent
	for _, path := range revocationCheckPaths {
		for _, credential := range []string{revocationCheckCredentialSession, revocationCheckCredentialClient} {
			sessionCheckUnavailableCounter.WithLabelValues(path, credential)
		}
	}
}

type revocationCheckPathKey struct{}

// Names the path the revocation checks under ctx count under, for a caller its
// state query source does not describe (a lease recheck).
func withRevocationCheckPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, revocationCheckPathKey{}, path)
}

// The path of a check: the caller's own, else its state query source's caller.
func revocationCheckPathFromContext(ctx context.Context) string {
	if path, ok := ctx.Value(revocationCheckPathKey{}).(string); ok {
		return path
	}
	switch stateQuerySourceLabels[stateQuerySourceFromContext(ctx)][0] {
	case "api":
		return revocationCheckPathApi
	case "connect_h1":
		return revocationCheckPathConnectH1
	case "connect_h3":
		return revocationCheckPathConnectH3
	case "hosted":
		return revocationCheckPathHosted
	case "prober":
		return revocationCheckPathProber
	default:
		return revocationCheckPathOther
	}
}

func revocationCheckCredential(byJwt *ByJwt) string {
	if byJwt.ClientId != nil {
		return revocationCheckCredentialClient
	}
	return revocationCheckCredentialSession
}

// What one caller learns of its checks: whether one accepted a credential
// unchecked. An authorization lease reads it, so that an unchecked answer
// holds the lease rather than renewing it (authorization_lease.go).
type revocationCheckReport struct {
	unchecked atomic.Bool
}

func (self *revocationCheckReport) Unchecked() bool {
	return self.unchecked.Load()
}

type revocationCheckReportKey struct{}

func withRevocationCheckReport(ctx context.Context) (context.Context, *revocationCheckReport) {
	report := &revocationCheckReport{}
	return context.WithValue(ctx, revocationCheckReportKey{}, report), report
}

// Whether a revocation check that failed with err went unanswered by Redis
// while its caller still waits, the case decision 3 accepts. A caller that
// ended gets the unavailable error, as before.
func revocationCheckUnanswered(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil
}

// Counts one credential accepted without its revocation check, and reports it
// to the caller's report. The caller has made every other check first.
func noteUncheckedAcceptance(ctx context.Context, byJwt *ByJwt, path string) {
	sessionCheckUnavailableCounter.WithLabelValues(path, revocationCheckCredential(byJwt)).Inc()
	if report, ok := ctx.Value(revocationCheckReportKey{}).(*revocationCheckReport); ok {
		report.unchecked.Store(true)
	}
	noteUncheckedAcceptanceLog(path)
}

// The answer to a record check Redis did not answer: the credential is
// accepted, unless its caller ended.
func acceptUncheckedRevocation(ctx context.Context, byJwt *ByJwt) error {
	if ctx.Err() != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	noteUncheckedAcceptance(ctx, byJwt, revocationCheckPathFromContext(ctx))
	return nil
}

// The session marker read with the check budget. unchecked is true, and err
// nil, when Redis did not answer while the caller still waits: the marker is
// skipped and the caller's other checks decide.
func checkSessionWithinBudget(ctx context.Context, byJwt *ByJwt) (unchecked bool, err error) {
	checkCtx, cancel := context.WithTimeout(ctx, RevocationCheckBudget)
	err = CheckSession(checkCtx, byJwt.NetworkId, *byJwt.SessionId)
	cancel()
	if err == nil || errors.Is(err, ErrSessionRevoked) {
		return false, err
	}
	if revocationCheckUnanswered(ctx, err) {
		return true, nil
	}
	return false, err
}

// Process-wide summary of unchecked acceptances, logged at most once per
// interval, so an outage does not log one line per request.
const uncheckedAcceptanceLogInterval = 30 * time.Second

var uncheckedAcceptanceLog struct {
	lastUnixNano atomic.Int64
	count        atomic.Int64
}

func noteUncheckedAcceptanceLog(path string) {
	uncheckedAcceptanceLog.count.Add(1)
	now := time.Now().UnixNano()
	last := uncheckedAcceptanceLog.lastUnixNano.Load()
	if now-last < int64(uncheckedAcceptanceLogInterval) || !uncheckedAcceptanceLog.lastUnixNano.CompareAndSwap(last, now) {
		return
	}
	count := uncheckedAcceptanceLog.count.Swap(0)
	glog.Warningf("[session]%d credentials accepted without their revocation check since the last report (last on %s): Redis did not answer within %s (decision 3)\n", count, path, RevocationCheckBudget)
}
