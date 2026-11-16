// An instance that has no current authority cannot judge a validator. Its 403
// said the validator was refused, and the validator counted it as such.
package handlers

import (
	"net/http"
	"testing"

	"github.com/urnetwork/server"
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
