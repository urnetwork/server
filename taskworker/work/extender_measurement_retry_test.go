package work

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Amendment 24 (lag.dev/PLAN.md C5, C6, C9): the client returns a refusal
// under one of the CA's rate limits at once instead of waiting out its
// Retry-After inside the order, and the order, renewal or revocation it
// refused waits out the Retry-After on its row instead of the doubling
// backoff. The CA's limits are in lag.dev/ops/letsencrypt.md §5.1.

// The CA's answer to a request over one of its rate limits: 429, a
// rateLimited problem and the Retry-After, as Let's Encrypt answered on Main
// for a name past its five certificates a week.
func writeTestMeasurementRateLimited(w http.ResponseWriter, retryAfter string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Retry-After", retryAfter)
	w.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(w).Encode(map[string]any{
		"type":   "urn:ietf:params:acme:error:rateLimited",
		"detail": "too many certificates (5) already issued for this exact set of identifiers in the last 168h0m0s, retry after 2026-10-31 02:25:45 UTC: see https://letsencrypt.org/docs/rate-limits/#new-certificates-per-exact-set-of-identifiers",
		"status": http.StatusTooManyRequests,
	})
}

// That refusal as the client returns it, with `retryAfter` as its Retry-After,
// or none when it is empty.
func newTestMeasurementRateLimitedError(retryAfter string) *acme.Error {
	header := http.Header{}
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &acme.Error{
		StatusCode:  http.StatusTooManyRequests,
		ProblemType: "urn:ietf:params:acme:error:rateLimited",
		Detail:      "too many certificates (5) already issued for this exact set of identifiers in the last 168h0m0s",
		Header:      header,
	}
}

// A rate limit in front of the fake CA, on the client's transport: each POST
// to one of the refused paths is answered as the CA answers a request over a
// limit and never reaches the CA. Every other request goes through.
type testMeasurementRateLimitedTransport struct {
	stateLock  sync.Mutex
	paths      []string
	retryAfter string
	refusals   int
}

// From now on refuses each POST to one of `paths` with `retryAfter`, or
// nothing when no path is given.
func (self *testMeasurementRateLimitedTransport) refuse(retryAfter string, paths ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.paths = paths
	self.retryAfter = retryAfter
}

// The requests refused so far.
func (self *testMeasurementRateLimitedTransport) refusedCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.refusals
}

func (self *testMeasurementRateLimitedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	retryAfter, refused := func() (string, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if request.Method != http.MethodPost || !slices.Contains(self.paths, request.URL.Path) {
			return "", false
		}
		self.refusals += 1
		return self.retryAfter, true
	}()
	if !refused {
		return http.DefaultTransport.RoundTrip(request)
	}
	if request.Body != nil {
		request.Body.Close()
	}
	recorder := httptest.NewRecorder()
	writeTestMeasurementRateLimited(recorder, retryAfter)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// Puts a rate limit in front of the CA for the configured account's client,
// the one every pass uses (extenderMeasurementAcmeForConfig). It refuses
// nothing until told to.
func useTestMeasurementRateLimitedTransport(t testing.TB) *testMeasurementRateLimitedTransport {
	t.Helper()
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	acmeClient, err := extenderMeasurementAcmeForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	transport := &testMeasurementRateLimitedTransport{}
	acmeClient.client.HTTPClient.Transport = transport
	return transport
}

// The next attempt after a refusal under a rate limit is the CA's Retry-After
// and the margin, given in seconds or as a date, however many failures came
// before: sooner than the backoff of a name that kept failing, as on Main, or
// later than a first failure's. A rate limit without a Retry-After, or with
// one that is not ahead, any other error of the CA's, and an error that is not
// the CA's back off by the failures so far. The client's wrapping is seen
// through.
func TestExtenderMeasurementRetryTimeWaitsOutARateLimit(t *testing.T) {
	now := server.NowUtc()
	margin := ExtenderMeasurementRateLimitMargin
	rateLimited := newTestMeasurementRateLimitedError

	// in seconds
	connect.AssertEqual(t, extenderMeasurementRetryTime(rateLimited("3600"), 0, now), now.Add(time.Hour+margin))
	connect.AssertEqual(t, extenderMeasurementRetryTime(rateLimited("3600"), 10, now), now.Add(time.Hour+margin))
	// the exact set's refill of 34 hours (lag.dev/ops/letsencrypt.md §5.1):
	// later than the backoff after three failures, sooner than after ten
	connect.AssertEqual(t, extenderMeasurementRetryTime(rateLimited("122400"), 3, now), now.Add(34*time.Hour+margin))
	connect.AssertEqual(t, extenderMeasurementRetryTime(rateLimited("122400"), 10, now), now.Add(34*time.Hour+margin))

	// as a date, which names a whole second and is read against the clock
	date := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	retryTime := extenderMeasurementRetryTime(rateLimited(date.Format(http.TimeFormat)), 10, now)
	if retryTime.After(date.Add(margin)) || time.Second <= date.Add(margin).Sub(retryTime) {
		t.Fatalf("the next attempt is at %s, not the date %s and the margin", retryTime, date)
	}

	// wrapped as the client wraps it
	connect.AssertEqual(
		t,
		extenderMeasurementRetryTime(fmt.Errorf("the order was not created: %w", rateLimited("600")), 10, now),
		now.Add(10*time.Minute+margin),
	)
	connect.AssertEqual(
		t,
		extenderMeasurementRetryTime(fmt.Errorf("the renewal: %w", fmt.Errorf("the acme account could not be registered: %w", rateLimited("60"))), 0, now),
		now.Add(time.Minute+margin),
	)

	backoff := func(failureCount int) time.Time {
		return now.Add(extenderMeasurementRetryTimeout(failureCount))
	}
	unansweredRateLimit := rateLimited("")
	unansweredRateLimit.Header = nil
	serverInternal := &acme.Error{
		StatusCode:  http.StatusServiceUnavailable,
		ProblemType: "urn:ietf:params:acme:error:serverInternal",
		Detail:      "the service is down for maintenance",
		Header:      http.Header{"Retry-After": []string{"3600"}},
	}
	for _, failureCount := range []int{0, 3, 10} {
		for _, err := range []error{
			// a rate limit that names no time
			rateLimited(""),
			unansweredRateLimit,
			rateLimited("0"),
			rateLimited("soon"),
			rateLimited(time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)),
			fmt.Errorf("the order was not created: %w", rateLimited("")),
			// not a rate limit, even with a Retry-After
			serverInternal,
			fmt.Errorf("the order was not finalized: %w", serverInternal),
			&acme.Error{StatusCode: http.StatusBadRequest, ProblemType: "urn:ietf:params:acme:error:malformed"},
			// not the CA's
			context.DeadlineExceeded,
			fmt.Errorf("the challenge records were not published: %w", errors.New("throttled")),
		} {
			connect.AssertEqual(t, extenderMeasurementRetryTime(err, failureCount, now), backoff(failureCount))
		}
	}
}

// The client's backoff (amendment 24): a 429 is not retried, with or without
// a Retry-After, so the CA's refusal comes back at once. Anything else is
// retried as the library's default retries it: after 2^(n-1) seconds and up
// to a second of jitter, never more than 10 seconds, or after the response's
// Retry-After and the jitter. A Retry-After that has passed, or that does not
// read, waits the jitter alone.
func TestExtenderMeasurementAcmeRetryBackoff(t *testing.T) {
	response := func(statusCode int, retryAfter string) *http.Response {
		header := http.Header{}
		if retryAfter != "" {
			header.Set("Retry-After", retryAfter)
		}
		return &http.Response{StatusCode: statusCode, Header: header}
	}
	assertWait := func(wait time.Duration, least time.Duration, most time.Duration) {
		t.Helper()
		if wait < least || most < wait {
			t.Fatalf("waits %s, not within [%s, %s]", wait, least, most)
		}
	}

	for _, n := range []int{1, 2, 5, 30} {
		connect.AssertEqual(t, extenderMeasurementAcmeRetryBackoff(n, nil, response(http.StatusTooManyRequests, "3600")), time.Duration(0))
		connect.AssertEqual(t, extenderMeasurementAcmeRetryBackoff(n, nil, response(http.StatusTooManyRequests, "")), time.Duration(0))
	}

	for _, statusCode := range []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadRequest} {
		for _, n := range []int{0, 1, 2, 3, 4, 5, 30, 31, 1000} {
			retry := min(max(n, 1), 30)
			exponential := time.Duration(1<<(retry-1)) * time.Second
			assertWait(
				extenderMeasurementAcmeRetryBackoff(n, nil, response(statusCode, "")),
				min(exponential+time.Millisecond, extenderMeasurementAcmeMaxRetryBackoff),
				min(exponential+time.Second, extenderMeasurementAcmeMaxRetryBackoff),
			)
		}
	}
	assertWait(extenderMeasurementAcmeRetryBackoff(1, nil, nil), time.Second+time.Millisecond, 2*time.Second)

	// the response's Retry-After, in seconds or as a date
	assertWait(extenderMeasurementAcmeRetryBackoff(1, nil, response(http.StatusServiceUnavailable, "2")), 2*time.Second+time.Millisecond, 3*time.Second)
	date := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	assertWait(extenderMeasurementAcmeRetryBackoff(1, nil, response(http.StatusServiceUnavailable, date)), 28*time.Second, 31*time.Second)
	for _, retryAfter := range []string{"-5", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), "soon"} {
		assertWait(extenderMeasurementAcmeRetryBackoff(3, nil, response(http.StatusServiceUnavailable, retryAfter)), time.Millisecond, time.Second)
	}
}

// The client returns a refusal at once (amendment 24). A CA that refuses
// every request under a rate limit, asking for an hour, is asked once, and the
// order fails within moments though its context allows five minutes. The
// error, the CA's own under the client's wrapping, puts the next attempt an
// hour and the margin out.
func TestExtenderMeasurementAcmeReturnsARefusalAtOnce(t *testing.T) {
	refusals := atomic.Int64{}
	ca := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refusals.Add(1)
		writeTestMeasurementRateLimited(w, "3600")
	}))
	defer ca.Close()
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acmeClient := newExtenderMeasurementAcme(ca.URL+"/directory", accountKey, nil)

	orderCtx, orderCancel := context.WithTimeout(context.Background(), ExtenderMeasurementOrderTimeout)
	defer orderCancel()
	start := time.Now()
	_, err = acmeClient.issue(orderCtx, []string{"refused." + testMeasurementZone}, nil, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("the refused order was issued")
	}
	if ExtenderMeasurementOrderTimeout/10 <= elapsed {
		t.Fatalf("the refusal came back after %s", elapsed)
	}
	connect.AssertEqual(t, refusals.Load(), int64(1))
	connect.AssertEqual(t, orderCtx.Err(), nil)
	now := server.NowUtc()
	connect.AssertEqual(t, extenderMeasurementRetryTime(err, 0, now), now.Add(time.Hour+ExtenderMeasurementRateLimitMargin))
}

// Amendment 24 end to end, for a pending request (C5). The request failed
// eight times before, so the backoff would put its next attempt over 21 hours
// out. The CA refuses its order under a rate limit and asks for an hour. The
// pass returns at once rather than when the order's five minutes run out,
// having asked the CA once, and the request's next attempt is an hour and the
// margin out, counted as its ninth failure. A pass a second before that time
// orders nothing, and the pass at it, with the CA's room back, issues.
func TestExtenderMeasurementDefersARateLimitedOrderToRetryAfter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		transport := useTestMeasurementRateLimitedTransport(t)
		extenderId := createTestMeasurementExtender(t, ctx, 1, "203.0.113.7")
		_, request := requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		for range 8 {
			model.DeferPendingNetworkExtenderMeasurement(ctx, extenderId, request.Csr, "an earlier failure", server.NowUtc().Add(-time.Second), server.NowUtc())
		}
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).FailureCount, 8)

		transport.refuse("3600", "/new-order")
		before := server.NowUtc()
		result := runTestMeasurementIssue(t, ctx)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Failed, 1)
		connect.AssertEqual(t, result.Issued, 0)
		if ExtenderMeasurementOrderTimeout/10 <= after.Sub(before) {
			t.Fatalf("the refused pass took %s", after.Sub(before))
		}
		connect.AssertEqual(t, transport.refusedCount(), 1)
		connect.AssertEqual(t, env.ca.orderTotal(), 0)

		measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, measurement.FailureCount, 9)
		wait := time.Hour + ExtenderMeasurementRateLimitMargin
		if measurement.RetryTime == nil || measurement.RetryTime.Before(before.Add(wait)) || after.Add(wait).Before(*measurement.RetryTime) {
			t.Fatalf("the next attempt is at %v, not an hour and the margin after %s", measurement.RetryTime, before)
		}
		if !strings.Contains(measurement.Reason, "rateLimited") {
			t.Fatalf("the request says %q", measurement.Reason)
		}

		// the next pass leaves it
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Failed+result.Issued, 0)
		connect.AssertEqual(t, transport.refusedCount(), 1)

		// the CA has room again
		transport.refuse("")
		config, err := controller.EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		result = &ExtenderMeasurementIssueResult{}
		issueExtenderMeasurementCerts(clientSession, config, measurement.RetryTime.Add(-time.Second), result)
		connect.AssertEqual(t, result.Failed+result.Issued, 0)
		connect.AssertEqual(t, env.ca.orderTotal(), 0)
		result = &ExtenderMeasurementIssueResult{}
		issueExtenderMeasurementCerts(clientSession, config, *measurement.RetryTime, result)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, env.ca.orderTotal(), 1)
		measurement = model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStateIssued)
		connect.AssertEqual(t, measurement.FailureCount, 0)
	})
}

// Amendment 24 for a renewal and a revocation (C6, C9). The CA refuses both
// under a rate limit and asks for two hours. The pass returns at once, having
// asked the CA once for each, and each certificate's next attempt is two hours
// and the margin out, counted as a failure on its row. The next pass asks for
// neither.
func TestExtenderMeasurementDefersARateLimitedRenewalAndRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		transport := useTestMeasurementRateLimitedTransport(t)
		renewing := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		revoking := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		for _, extenderId := range []server.Id{renewing, revoking} {
			requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		}
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)

		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(
			ctx,
			model.Testing_GetNetworkExtenderMeasurementCerts(ctx, renewing)[0].CertId,
			server.NowUtc().Add(-time.Second),
		)
		// an operator's move to the gated tier withdraws it at once (A9)
		if err := model.SetNetworkExtenderDirectoryTier(ctx, revoking, connect.ExtenderDirectoryTierGated, true); err != nil {
			t.Fatal(err)
		}

		transport.refuse("7200", "/new-order", "/revoke-cert")
		before := server.NowUtc()
		result := runTestMeasurementIssue(t, ctx)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Failed, 2)
		connect.AssertEqual(t, result.Renewed+result.Revoked, 0)
		if ExtenderMeasurementOrderTimeout/10 <= after.Sub(before) {
			t.Fatalf("the refused pass took %s", after.Sub(before))
		}
		connect.AssertEqual(t, transport.refusedCount(), 2)
		connect.AssertEqual(t, env.ca.issuedCount(), 2)
		connect.AssertEqual(t, len(env.ca.revokedReasons()), 0)

		wait := 2*time.Hour + ExtenderMeasurementRateLimitMargin
		renewingCerts := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, renewing)
		connect.AssertEqual(t, len(renewingCerts), 1)
		connect.AssertEqual(t, renewingCerts[0].RenewFailureCount, 1)
		if renewingCerts[0].RenewTime.Before(before.Add(wait)) || after.Add(wait).Before(renewingCerts[0].RenewTime) {
			t.Fatalf("the renewal is at %s, not two hours and the margin after %s", renewingCerts[0].RenewTime, before)
		}

		revokingCert := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, revoking)[0]
		connect.AssertEqual(t, revokingCert.RevokeFailureCount, 1)
		connect.AssertEqual(t, revokingCert.RevokeTime == nil, true)
		toRevoke := func(now time.Time) bool {
			return slices.ContainsFunc(
				model.GetNetworkExtenderMeasurementCertsToRevoke(ctx, now, ExtenderMeasurementRevokeLimit),
				func(cert *model.NetworkExtenderMeasurementCert) bool {
					return cert.CertId == revokingCert.CertId
				},
			)
		}
		connect.AssertEqual(t, toRevoke(before.Add(wait).Add(-time.Second)), false)
		connect.AssertEqual(t, toRevoke(after.Add(wait)), true)

		// the next pass asks for neither
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Failed+result.Renewed+result.Revoked, 0)
		connect.AssertEqual(t, transport.refusedCount(), 2)
	})
}
