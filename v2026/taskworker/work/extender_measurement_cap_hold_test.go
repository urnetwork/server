package work

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Amendment 24's hold on new certificates (lag.dev/PLAN.md C5, C7): the CA's
// refusal of an order the weekly cap counts, under a rate limit, is the
// registered domain's or the account's, so it holds every order the cap
// counts until its own next attempt, while renewals and orders for names with
// an unexpired certificate go on. A refusal of one of those is the name's and
// holds nothing.

// What the CA says when the registered domain's budget is spent
// (lag.dev/ops/letsencrypt.md §5.1).
const testMeasurementCapHoldDetail = `too many certificates (50) already issued for "ur.example" in the last 168h0m0s, retry after 2026-11-01 03:00:00 UTC: see https://letsencrypt.org/docs/rate-limits/#new-certificates-per-registered-domain`

// A rate limit in front of the fake CA on the names of new orders, on the
// client's transport: each new order for one of the refused names is answered
// as the CA answers a request over a limit, 429 with a rateLimited problem and
// the Retry-After, and never reaches the CA. Every other request goes through.
type testMeasurementCapHoldTransport struct {
	stateLock  sync.Mutex
	names      []string
	retryAfter string
	// the new orders refused so far, by name
	refusals map[string]int
}

// From now on refuses each new order for one of `names` with `retryAfter`, or
// none when no name is given.
func (self *testMeasurementCapHoldTransport) refuse(retryAfter string, names ...string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.names = names
	self.retryAfter = retryAfter
}

// The new orders refused so far for any of `names`.
func (self *testMeasurementCapHoldTransport) refusedCount(names ...string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, name := range names {
		count += self.refusals[name]
	}
	return count
}

func (self *testMeasurementCapHoldTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL.Path != "/new-order" || request.Body == nil {
		return http.DefaultTransport.RoundTrip(request)
	}
	body, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil {
		return nil, err
	}
	name := testMeasurementCapHoldOrderName(body)
	retryAfter, refused := func() (string, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !slices.Contains(self.names, name) {
			return "", false
		}
		self.refusals[name] += 1
		return self.retryAfter, true
	}()
	if !refused {
		forward := request.Clone(request.Context())
		forward.Body = io.NopCloser(bytes.NewReader(body))
		forward.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		forward.ContentLength = int64(len(body))
		return http.DefaultTransport.RoundTrip(forward)
	}
	recorder := httptest.NewRecorder()
	recorder.Header().Set("Content-Type", "application/problem+json")
	recorder.Header().Set("Retry-After", retryAfter)
	recorder.WriteHeader(http.StatusTooManyRequests)
	json.NewEncoder(recorder).Encode(map[string]any{
		"type":   "urn:ietf:params:acme:error:rateLimited",
		"detail": testMeasurementCapHoldDetail,
		"status": http.StatusTooManyRequests,
	})
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// The first identifier a new order names, read from its JWS body.
func testMeasurementCapHoldOrderName(body []byte) string {
	var jws struct {
		Payload string `json:"payload"`
	}
	if json.Unmarshal(body, &jws) != nil {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		return ""
	}
	var order struct {
		Identifiers []struct {
			Value string `json:"value"`
		} `json:"identifiers"`
	}
	if json.Unmarshal(payload, &order) != nil || len(order.Identifiers) == 0 {
		return ""
	}
	return order.Identifiers[0].Value
}

// Puts the rate limit in front of the CA for the configured account's client,
// the one every pass uses (extenderMeasurementAcmeForConfig). It refuses
// nothing until told to.
func useTestMeasurementCapHoldTransport(t testing.TB) *testMeasurementCapHoldTransport {
	t.Helper()
	config, err := controller.EnvExtenderConfig()
	if err != nil {
		t.Fatal(err)
	}
	acmeClient, err := extenderMeasurementAcmeForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	transport := &testMeasurementCapHoldTransport{refusals: map[string]int{}}
	acmeClient.client.HTTPClient.Transport = transport
	return transport
}

// The hold itself: only a refusal under a rate limit holds new certificates,
// with or without a Retry-After and under the client's wrapping, until the
// time it is given and not from then on, with a reason that names the CA's
// limit and that time. A refusal with an earlier time leaves the hold, and one
// with a later time moves it out.
func TestExtenderMeasurementCapHoldLastsUntilTheRefusalsNextAttempt(t *testing.T) {
	Testing_ResetExtenderMeasurementCapHold()
	t.Cleanup(Testing_ResetExtenderMeasurementCapHold)
	now := server.NowUtc()
	_, held := extenderMeasurementCapHeld(now)
	connect.AssertEqual(t, held, false)

	for _, err := range []error{
		errors.New("throttled"),
		context.DeadlineExceeded,
		&acme.Error{
			StatusCode:  http.StatusServiceUnavailable,
			ProblemType: "urn:ietf:params:acme:error:serverInternal",
			Header:      http.Header{"Retry-After": []string{"3600"}},
		},
		fmt.Errorf("the order was not created: %w", &acme.Error{StatusCode: http.StatusBadRequest, ProblemType: "urn:ietf:params:acme:error:malformed"}),
	} {
		connect.AssertEqual(t, holdExtenderMeasurementCapCounted(err, now.Add(time.Hour)), false)
		_, held = extenderMeasurementCapHeld(now)
		connect.AssertEqual(t, held, false)
	}

	refusal := newTestMeasurementRateLimitedError("3600")
	refusal.Detail = testMeasurementCapHoldDetail
	until := now.Add(time.Hour + ExtenderMeasurementRateLimitMargin)
	connect.AssertEqual(t, holdExtenderMeasurementCapCounted(fmt.Errorf("the order was not created: %w", refusal), until), true)
	reason, held := extenderMeasurementCapHeld(now)
	connect.AssertEqual(t, held, true)
	if !strings.Contains(reason, until.UTC().Format(time.RFC3339)) || !strings.Contains(reason, testMeasurementCapHoldDetail) {
		t.Fatalf("the hold says %q", reason)
	}
	_, held = extenderMeasurementCapHeld(until.Add(-time.Microsecond))
	connect.AssertEqual(t, held, true)
	_, held = extenderMeasurementCapHeld(until)
	connect.AssertEqual(t, held, false)

	// an earlier time leaves the hold as it is
	connect.AssertEqual(t, holdExtenderMeasurementCapCounted(refusal, now.Add(time.Minute)), true)
	_, held = extenderMeasurementCapHeld(until.Add(-time.Microsecond))
	connect.AssertEqual(t, held, true)
	// a later one, here a refusal without a Retry-After, moves it out
	later := until.Add(time.Hour)
	connect.AssertEqual(t, holdExtenderMeasurementCapCounted(newTestMeasurementRateLimitedError(""), later), true)
	_, held = extenderMeasurementCapHeld(until)
	connect.AssertEqual(t, held, true)
	_, held = extenderMeasurementCapHeld(later)
	connect.AssertEqual(t, held, false)
}

// The hold end to end (C5, C7). With room in the weekly cap, the CA refuses
// new certificates under the registered domain's limit and asks for an hour.
// The pass orders for the rotated key of a name with a certificate, then
// places one order the cap counts: refused, it is deferred as any refused
// order is, and it holds the other two, which are noted waiting with the CA's
// limit and the time, neither deferred nor counted a failure. While the hold
// lasts, a pass places no order the cap counts, a second before its end too,
// and still renews a due certificate and orders for another rotated key. At
// its end, with the CA's room back, all three are issued.
func TestExtenderMeasurementHoldsNewCertificatesAfterARateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		transport := useTestMeasurementCapHoldTransport(t)

		rotating := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		renewing := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		for _, extenderId := range []server.Id{rotating, renewing} {
			requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		}
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)

		// three new extenders, whose orders the cap counts, and a rotated key,
		// whose order it does not
		capped := []server.Id{}
		cappedNames := []string{}
		for i := range 3 {
			extenderId := createTestMeasurementExtender(t, ctx, 3+i, fmt.Sprintf("198.51.100.%d", 1+i))
			requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
			capped = append(capped, extenderId)
			cappedNames = append(cappedNames, testMeasurementName(extenderId))
		}
		requestTestMeasurementCert(t, ctx, rotating, server.NowUtc())

		transport.refuse("3600", cappedNames...)
		ordersBefore := env.ca.orderTotal()
		before := server.NowUtc()
		result := runTestMeasurementIssue(t, ctx)
		after := server.NowUtc()
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Failed, 1)
		connect.AssertEqual(t, result.Waiting, 2)
		connect.AssertEqual(t, transport.refusedCount(cappedNames...), 1)
		connect.AssertEqual(t, env.ca.orderTotal(), ordersBefore+1)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, rotating).State, model.NetworkExtenderMeasurementStateIssued)

		wait := time.Hour + ExtenderMeasurementRateLimitMargin
		var holdUntil *time.Time
		held := []*model.NetworkExtenderMeasurement{}
		for _, extenderId := range capped {
			measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
			if measurement.RetryTime == nil {
				held = append(held, measurement)
				continue
			}
			// the refused one, deferred as any refused order is
			connect.AssertEqual(t, holdUntil == nil, true)
			connect.AssertEqual(t, measurement.FailureCount, 1)
			if measurement.RetryTime.Before(before.Add(wait)) || after.Add(wait).Before(*measurement.RetryTime) {
				t.Fatalf("the refused request's next attempt is at %s, not an hour and the margin after %s", measurement.RetryTime, before)
			}
			holdUntil = measurement.RetryTime
		}
		if holdUntil == nil {
			t.Fatalf("no request was deferred")
		}
		// the rest are held until the same time
		reason, isHeld := extenderMeasurementCapHeld(server.NowUtc())
		connect.AssertEqual(t, isHeld, true)
		if !strings.Contains(reason, holdUntil.UTC().Format(time.RFC3339)) || !strings.Contains(reason, testMeasurementCapHoldDetail) {
			t.Fatalf("the hold says %q", reason)
		}
		connect.AssertEqual(t, len(held), 2)
		for _, measurement := range held {
			connect.AssertEqual(t, measurement.FailureCount, 0)
			connect.AssertEqual(t, measurement.Reason, reason)
		}

		// while held, a due renewal and another rotated key are ordered, and
		// no order the cap counts is placed
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(
			ctx,
			model.Testing_GetNetworkExtenderMeasurementCerts(ctx, renewing)[0].CertId,
			server.NowUtc().Add(-time.Second),
		)
		requestTestMeasurementCert(t, ctx, rotating, server.NowUtc())
		result = runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Renewed, 1)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Failed, 0)
		connect.AssertEqual(t, result.Waiting, 2)
		connect.AssertEqual(t, transport.refusedCount(cappedNames...), 1)

		config, err := controller.EnvExtenderConfig()
		if err != nil {
			t.Fatal(err)
		}
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		// a second before the hold ends, still none
		ordersBefore = env.ca.orderTotal()
		result = &ExtenderMeasurementIssueResult{}
		issueExtenderMeasurementCerts(clientSession, config, holdUntil.Add(-time.Second), result)
		connect.AssertEqual(t, result.Issued+result.Failed, 0)
		connect.AssertEqual(t, result.Waiting, 2)
		connect.AssertEqual(t, transport.refusedCount(cappedNames...), 1)
		connect.AssertEqual(t, env.ca.orderTotal(), ordersBefore)

		// at its end, with the CA's room back, all three are issued
		transport.refuse("")
		result = &ExtenderMeasurementIssueResult{}
		issueExtenderMeasurementCerts(clientSession, config, *holdUntil, result)
		connect.AssertEqual(t, result.Issued, 3)
		connect.AssertEqual(t, result.Waiting, 0)
		for _, extenderId := range capped {
			connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId).State, model.NetworkExtenderMeasurementStateIssued)
		}
	})
}

// A refusal of an order the cap does not count, a rotated key's or a
// renewal's, is under a limit of the name's, such as its five certificates a
// week, and holds nothing (C5, C6, C7): each refused row is deferred, and a new
// certificate later in the same pass is still ordered and issued.
func TestExtenderMeasurementHoldsNothingForARefusalOfAName(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestMeasurementEnv(t, testMeasurementEnvSettings{})
		transport := useTestMeasurementCapHoldTransport(t)

		rotating := createTestMeasurementExtender(t, ctx, 1, "203.0.113.1")
		renewing := createTestMeasurementExtender(t, ctx, 2, "203.0.113.2")
		for _, extenderId := range []server.Id{rotating, renewing} {
			requestTestMeasurementCert(t, ctx, extenderId, server.NowUtc())
		}
		connect.AssertEqual(t, runTestMeasurementIssue(t, ctx).Issued, 2)

		requestTestMeasurementCert(t, ctx, rotating, server.NowUtc())
		renewingCert := model.Testing_GetNetworkExtenderMeasurementCerts(ctx, renewing)[0]
		model.Testing_SetNetworkExtenderMeasurementCertRenewTime(ctx, renewingCert.CertId, server.NowUtc().Add(-time.Second))
		newcomer := createTestMeasurementExtender(t, ctx, 3, "203.0.113.3")
		requestTestMeasurementCert(t, ctx, newcomer, server.NowUtc())

		names := []string{testMeasurementName(rotating), testMeasurementName(renewing)}
		transport.refuse("3600", names...)
		result := runTestMeasurementIssue(t, ctx)
		connect.AssertEqual(t, result.Failed, 2)
		connect.AssertEqual(t, result.Issued, 1)
		connect.AssertEqual(t, result.Renewed, 0)
		connect.AssertEqual(t, result.Waiting, 0)
		connect.AssertEqual(t, transport.refusedCount(names...), 2)
		_, held := extenderMeasurementCapHeld(server.NowUtc())
		connect.AssertEqual(t, held, false)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurement(ctx, newcomer).State, model.NetworkExtenderMeasurementStateIssued)

		measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, rotating)
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		connect.AssertEqual(t, measurement.FailureCount, 1)
		connect.AssertEqual(t, measurement.RetryTime != nil, true)
		connect.AssertEqual(t, model.Testing_GetNetworkExtenderMeasurementCerts(ctx, renewing)[0].RenewFailureCount, 1)
	})
}
