package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Status-code regressions for POST /auth/network-create.
//
// The status is what every SDK, proxy and retry loop acts on. A rate limit
// reported as 503 tells a well-behaved client "the server is broken, retry",
// and each retry records another attempt (model/auth_model_attempt.go:52-58),
// so the retry makes the condition worse. A validation refusal reported as 500
// tells the user their form mistake crashed the server.

// the request's own address; not loopback, so ResolveClientAddress uses it
// verbatim and the handler buckets on exactly this client
const (
	statusTestClientAddress = "203.0.113.9:41001"
	// same /29 as statusTestClientAddress, so a session built here shares the
	// bucket the handler will compute
	statusTestSameBucketAddress = "203.0.113.14:41002"
	statusTestOtherAddress      = "198.51.100.20:41003"
)

func networkCreateRequest(t testing.TB, remoteAddr string, args model.NetworkCreateArgs) *http.Request {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal network create args: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/network-create", strings.NewReader(string(body)))
	req.RemoteAddr = remoteAddr
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestNetworkCreateEmitsOneStatusForOneRateLimit.
//
// WHAT THIS USED TO PIN. One endpoint answered the same class of event -- "you
// are rate limited on account creation" -- with 429 on the seedphrase branch
// and 503 on the email/SSO/wallet branch. This test asserted that divergence.
//
// WHY THAT WAS WRONG. A client cannot tell "you are limited" from "the server
// is down", and the 503 branch was the one with the tighter budget (5 per 5
// minutes, not 5 per 24h). A 5xx instructs every well-behaved SDK to retry;
// each retry records another attempt, so the status spent the remaining budget
// faster and the refusal reinforced itself. Both branches now answer 429, and
// both carry Retry-After so a client waits instead of guessing.
func TestNetworkCreateEmitsOneStatusForOneRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// --- seedphrase branch: exhausted CheckNetworkCreateRateLimit ---
		seedphraseSession := session.NewLocalClientSession(ctx, statusTestSameBucketAddress, nil)
		defer seedphraseSession.Cancel()
		for i := 0; i < model.NetworkCreateDailyLimit; i += 1 {
			if err := model.CheckNetworkCreateRateLimit(ctx, seedphraseSession); err != nil {
				t.Fatalf("priming create attempt %d was refused: %v", i+1, err)
			}
		}

		w := httptest.NewRecorder()
		NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, model.NetworkCreateArgs{
			Terms: true,
		}))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf(
				"seedphrase signup over the daily limit returned HTTP %d (%q), want 429",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		assertRateLimitBody(t, w, "seedphrase")

		// --- email/SSO/wallet branch: exhausted UserAuthAttempt ---
		ssoSession := session.NewLocalClientSession(ctx, statusTestOtherAddress, nil)
		defer ssoSession.Cancel()
		for i := 0; i < model.AttemptFailedCountThreshold; i += 1 {
			model.UserAuthAttempt(nil, ssoSession)
		}

		authJwt := "sso-token-for-a-first-time-user"
		authJwtType := "google"
		w = httptest.NewRecorder()
		NetworkCreate(w, networkCreateRequest(t, statusTestOtherAddress, model.NetworkCreateArgs{
			AuthJwt:     &authJwt,
			AuthJwtType: &authJwtType,
			NetworkName: "newcomernet",
			Terms:       true,
		}))
		if w.Code == http.StatusServiceUnavailable {
			t.Fatalf(
				"the auth-attempt limit still answers 503 (%q). Every SDK reads that as "+
					"'the server is broken, retry', and each retry records another attempt",
				strings.TrimSpace(w.Body.String()),
			)
		}
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf(
				"rate-limited SSO signup returned HTTP %d (%q), want 429 -- the same status "+
					"the seedphrase branch gives for the same class of event",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		if strings.Contains(w.Body.String(), "User auth attempts exceeded limits") {
			t.Fatalf("429 body still carries the old opaque message: %q", strings.TrimSpace(w.Body.String()))
		}
		assertRateLimitBody(t, w, "sso")
	})
}

// assertRateLimitBody checks the two properties item 4 is about: the refusal
// says the limit is scoped to the network address rather than accusing the
// caller, and it carries a Retry-After so a client waits a known interval
// instead of retrying immediately into the same limit.
func assertRateLimitBody(t testing.TB, w *httptest.ResponseRecorder, branch string) {
	t.Helper()
	body := strings.TrimSpace(w.Body.String())

	if strings.Contains(body, "You have reached the maximum number of account creations") {
		t.Fatalf(
			"the %s refusal still tells the user they created the accounts: %q. The "+
				"budget is address-scoped, so the usual recipient created none of them",
			branch, body,
		)
	}
	if !strings.Contains(body, "address") {
		t.Fatalf(
			"the %s refusal body %q never says the limit is scoped to the network "+
				"address; support cannot tell a wrongly-refused user from abuse",
			branch, body,
		)
	}

	retryAfter := w.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatalf(
			"the %s 429 carries no Retry-After; the client can only guess when to try "+
				"again, and guessing early costs it more of the same budget",
			branch,
		)
	}
	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds <= 0 {
		t.Fatalf("the %s 429 sent Retry-After: %q, want a positive number of seconds", branch, retryAfter)
	}
}

// TestNetworkCreateValidationRefusalIsNotReportedAsSuccess covers the
// error-return convention (task item 5).
//
// The model returns validation refusals in the body as
// NetworkCreateResult.Error with a nil Go error. controller.NetworkCreate
// (:21-24) converts that body error into a Go error, and RaiseHttpError
// (router/handler_utils.go:391-405) has no numeric prefix to peel, so the
// status is 500.
//
// Two properties are pinned here. The first is a genuine safety property: a
// refusal must never reach the client as 200 with an error buried in the body,
// which a status-checking client would read as a created account. The second is
// the current convention -- 500 with a text/plain body -- which is itself
// wrong for a form mistake and means NetworkCreateResult.Error is unreachable
// over HTTP. It is pinned so it cannot drift silently.
func TestNetworkCreateValidationRefusalIsNotReportedAsSuccess(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		w := httptest.NewRecorder()
		NetworkCreate(w, networkCreateRequest(t, statusTestClientAddress, model.NetworkCreateArgs{
			Terms: false,
		}))

		if w.Code == http.StatusOK {
			t.Fatalf(
				"a signup refused for %q returned HTTP 200 with body %q: a status-checking "+
					"client reads that as a created account",
				model.AgreeToTerms, strings.TrimSpace(w.Body.String()),
			)
		}
		if w.Code != http.StatusInternalServerError {
			t.Fatalf(
				"terms-not-accepted signup returned HTTP %d (%q); the currently-pinned "+
					"convention is 500 -- if this is now 400 that is the fix, update this test",
				w.Code, strings.TrimSpace(w.Body.String()),
			)
		}
		if strings.TrimSpace(w.Body.String()) != string(model.AgreeToTerms) {
			t.Fatalf("refusal body was %q, want the plain message %q",
				strings.TrimSpace(w.Body.String()), model.AgreeToTerms)
		}

		// the body is the bare message, not the NetworkCreateResult a client
		// parses; sdk NetworkCreateResult.Error is therefore never populated
		var result model.NetworkCreateResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil && result.Error != nil {
			t.Fatalf(
				"the refusal now arrives as a parseable NetworkCreateResult with Error=%q; "+
					"that is the fix -- update this test",
				result.Error.Message,
			)
		}
	})
}
