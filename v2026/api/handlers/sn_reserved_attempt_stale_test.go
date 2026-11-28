// An instance that has no current authority cannot judge a validator. Its 403
// said the validator was refused, and the validator counted it as such.
package handlers

import (
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// One request against an owner whose refresh is failing or still in flight.
func requireSnReservedAttemptAuthorityNotCurrent(tb testing.TB, authority *snReservedAttemptAuthority) {
	tb.Helper()
	endpoint := newSnReservedAttemptEndpointWithLane(tb, authority, 1, authority.startUnrefreshed(tb, 1))
	data := []byte("{\"authority\":\"stale\"}\n")
	header := snReservedAttemptIntentHeader(tb, authority, endpoint, data)
	status, body := snReservedAttemptResponse(tb, endpoint.origin, endpoint.api.GetByJwt(), header, "metadata", data)
	requireDeclaredAnswer(tb, status, body, http.StatusServiceUnavailable, "Validator staging authority is not current on this instance.", "synthetic", "refreshed")
	if endpoint.reads.Load() != 0 || endpoint.stores.Load() != 0 {
		tb.Fatal("an instance without current authority reached body or storage")
	}
}

// A chain endpoint this one instance cannot reach fails its refresh. Production
// answered 403 for that: "dial tcp ...:9944: i/o timeout" on 2026-11-02.
func TestSnReservedAttemptUploadFailedRefreshAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		authority.refuseEventScan.Store(true)
		requireSnReservedAttemptAuthorityNotCurrent(tb, authority)
	})
}

// Every api process starts here. Its first refresh takes about 15 seconds, and
// an upload in that window was answered 403: "admission has not refreshed".
func TestSnReservedAttemptUploadFirstRefreshAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		hold := make(chan struct{})
		authority.holdEventScan.Store(&hold)
		// runs after the owner's close would wait on the held read
		tb.Cleanup(func() { close(hold) })
		requireSnReservedAttemptAuthorityNotCurrent(tb, authority)
	})
}

// The admission keeps its last publish through a transient refresh failure,
// until that publish's expiry. Past it, with the next refresh still in flight,
// the instance is not current. On g1 on 2026-11-24 it answered such uploads
// 403 "admission expired", where the lb retries a sibling only on a 503.
func TestSnReservedAttemptUploadExpiredRetainedAuthorityAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		// The first publish is five seconds from its expiry.
		authority.now = time.Now().Truncate(time.Second).Add(-55 * time.Second)
		expiry := authority.now.Add(60 * time.Second)
		owner := authority.start(tb, 1)
		// Later refreshes see a current chain. The next event scan fails as a
		// node's query timeout does, which the admission retains; the one
		// after it stays in flight past the expiry.
		current := time.Now()
		authority.chainClock.Store(&current)
		hold := make(chan struct{})
		// runs before the owner's close, which would wait on the held read
		tb.Cleanup(func() { close(hold) })
		authority.transientEventScans.Store(1)
		authority.holdEventScan.Store(&hold)
		endpoint := newSnReservedAttemptEndpointWithLane(tb, authority, 1, owner)
		for time.Now().Before(expiry.Add(500*time.Millisecond)) || authority.heldEventScans.Load() == 0 {
			if time.Now().After(expiry.Add(20 * time.Second)) {
				tb.Fatal("prerequisite: no refresh reached the held event scan")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if authority.transientEventScans.Load() != 0 {
			tb.Fatal("prerequisite: no refresh failed transiently before the expiry")
		}
		data := []byte("{\"authority\":\"expired\"}\n")
		header := snReservedAttemptIntentHeader(tb, authority, endpoint, data)
		status, body := snReservedAttemptResponse(tb, endpoint.origin, endpoint.api.GetByJwt(), header, "metadata", data)
		requireDeclaredAnswer(tb, status, body, http.StatusServiceUnavailable, "Validator staging authority is not current on this instance.", "expired", "published", "query timeout")
		if endpoint.reads.Load() != 0 || endpoint.stores.Load() != 0 {
			tb.Fatal("an instance without current authority reached body or storage")
		}
	})
}

// While the permit census is unavailable, discovery follows only admitted
// hotkeys, so a validator not admitted before is not looked for. This instance
// cannot judge it; a sibling with a census can admit it.
func TestSnReservedAttemptUploadUndiscoveredWithoutCensusAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		authority.refuseCensus.Store(true)
		owner := authority.start(tb, 1)
		endpoint := newSnReservedAttemptEndpointWithLane(tb, authority, 1, owner)
		data := []byte("{\"authority\":\"undiscovered\"}\n")
		header := snReservedAttemptIntentHeader(tb, authority, endpoint, data)
		status, body := snReservedAttemptResponse(tb, endpoint.origin, endpoint.api.GetByJwt(), header, "metadata", data)
		requireDeclaredAnswer(tb, status, body, http.StatusServiceUnavailable, "Validator staging authority is not current on this instance.", "census", "not admitted")
		if endpoint.reads.Load() != 0 || endpoint.stores.Load() != 0 {
			tb.Fatal("an instance without current authority reached body or storage")
		}
	})
}

// A refresh whose current eligibility read fails keeps the admitted entry
// only to its own expiry, while the admission publishes a later one. Past
// the entry's expiry this instance cannot judge the validator.
func TestSnReservedAttemptUploadExpiredRetainedEntryAnswersRetryably(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		authority := newSnReservedAttemptAuthority(tb)
		// The first publish is five seconds from its expiry.
		authority.now = time.Now().Truncate(time.Second).Add(-55 * time.Second)
		expiry := authority.now.Add(60 * time.Second)
		owner := authority.start(tb, 1)
		// The next refresh sees a current chain, so the admission publishes
		// a later expiry, but its eligibility read for the entry fails.
		current := time.Now()
		authority.chainClock.Store(&current)
		authority.refuseMetagraph.Store(true)
		endpoint := newSnReservedAttemptEndpointWithLane(tb, authority, 1, owner)
		for time.Now().Before(expiry.Add(500*time.Millisecond)) || authority.refusedMetagraphCalls.Load() == 0 {
			if time.Now().After(expiry.Add(20 * time.Second)) {
				tb.Fatal("prerequisite: no refresh read the entry's eligibility")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err := owner.WaitReady(tb.Context()); err != nil {
			tb.Fatalf("prerequisite: the admission as a whole is current: %v", err)
		}
		data := []byte("{\"authority\":\"retained\"}\n")
		header := snReservedAttemptIntentHeader(tb, authority, endpoint, data)
		status, body := snReservedAttemptResponse(tb, endpoint.origin, endpoint.api.GetByJwt(), header, "metadata", data)
		requireDeclaredAnswer(tb, status, body, http.StatusServiceUnavailable, "Validator staging authority is not current on this instance.", "metagraph", "eligibility")
		if endpoint.reads.Load() != 0 || endpoint.stores.Load() != 0 {
			tb.Fatal("an instance without current authority reached body or storage")
		}
	})
}
