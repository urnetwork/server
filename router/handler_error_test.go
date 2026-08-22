package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// controller errors use the "<code> message" convention, and the auth
// wrappers prefix them with one or more "[implName]" tags. The status code
// must survive the tagging, and the tags must not reach the client.
func TestRaiseHttpError(t *testing.T) {
	raise := func(message string) (int, string, bool) {
		w := httptest.NewRecorder()
		statusError := RaiseHttpError(fmt.Errorf("%s", message), w)
		return w.Code, strings.TrimRight(w.Body.String(), "\n"), statusError
	}

	// plain status error
	code, message, statusError := raise("403 Feedback does not belong to your network.")
	connect.AssertEqual(t, code, 403)
	connect.AssertEqual(t, message, "Feedback does not belong to your network.")
	connect.AssertEqual(t, statusError, true)

	// tagged by an auth wrapper
	code, message, statusError = raise("[github.com/urnetwork/server/api/handlers.LogUpload.func1]404 Feedback not found.")
	connect.AssertEqual(t, code, 404)
	connect.AssertEqual(t, message, "Feedback not found.")
	connect.AssertEqual(t, statusError, true)

	// nested tags
	code, message, statusError = raise("[outer][inner]429 Rate limited.")
	connect.AssertEqual(t, code, 429)
	connect.AssertEqual(t, message, "Rate limited.")
	connect.AssertEqual(t, statusError, true)

	// no status code: internal error, message passed through
	code, message, statusError = raise("something broke")
	connect.AssertEqual(t, code, http.StatusInternalServerError)
	connect.AssertEqual(t, message, "something broke")
	connect.AssertEqual(t, statusError, false)

	// tag without a status code stays a 500 with the full message
	code, message, statusError = raise("[impl]something broke")
	connect.AssertEqual(t, code, http.StatusInternalServerError)
	connect.AssertEqual(t, message, "[impl]something broke")
	connect.AssertEqual(t, statusError, false)

	// a message that merely starts with a number is not a status code...
	// unless it parses as one; this mirrors the existing convention where
	// numbers must be deliberate
	code, _, statusError = raise("500 internal")
	connect.AssertEqual(t, code, 500)
	connect.AssertEqual(t, statusError, true)
}

// retryAfterTestError is the shape model.rateLimitError presents to the router:
// a "429 ..." message plus a RetryAfterSeconds() int read through a one-method
// interface. model's own type is unexported, so this stands in for it -- which
// means this pins the RaiseHttpError seam, not the seven model call sites.
type retryAfterTestError struct {
	message string
	seconds int
}

func (self *retryAfterTestError) Error() string {
	return self.message
}

func (self *retryAfterTestError) RetryAfterSeconds() int {
	return self.seconds
}

// TestRaiseHttpErrorRetryAfterSurvivesImplTagging.
//
// WrapRequireAuth, WrapRequireClient and WrapNoAuth all re-wrap an impl error
// to tag it with the impl name. They used to do that with
//
//	fmt.Errorf("[%s]%s", implName(impl), err)
//
// which flattens the error to text. The status code still survived, because
// httpErrorCodeRegex peels the bracketed tag out of the message -- so the
// symptom was invisible in the status: a 429 with the Retry-After silently
// gone. errors.As cannot traverse a %s wrap.
//
// Every rate limit that exists today happens to be reached through
// WrapWithInputNoAuth, which does not tag, so nothing is broken in the shipped
// code. This test is here so the next rate limit added behind an authenticated
// route does not lose its retry hint without anything failing.
func TestRaiseHttpErrorRetryAfterSurvivesImplTagging(t *testing.T) {
	rateLimited := &retryAfterTestError{message: "429 Too many attempts.", seconds: 300}

	for _, testCase := range []struct {
		what string
		err  error
	}{
		{"raised directly", rateLimited},
		{"tagged by one wrapper", fmt.Errorf("[impl]%w", rateLimited)},
		{"tagged by two wrappers", fmt.Errorf("[outer]%w", fmt.Errorf("[inner]%w", rateLimited))},
	} {
		w := httptest.NewRecorder()
		RaiseHttpError(testCase.err, w)

		if w.Code != 429 {
			t.Fatalf("%s: status %d, want 429", testCase.what, w.Code)
		}
		if got := w.Header().Get("Retry-After"); got != "300" {
			t.Fatalf(
				"%s: Retry-After=%q, want \"300\". The impl tag was applied with %%s "+
					"instead of %%w, so errors.As can no longer reach the rate limit and "+
					"the client is left guessing when to retry -- guessing early costs it "+
					"more budget",
				testCase.what, got,
			)
		}
		if body := strings.TrimRight(w.Body.String(), "\n"); body != "Too many attempts." {
			t.Fatalf("%s: body %q, want the untagged message", testCase.what, body)
		}
	}

	// an error with no retry hint must not grow one
	w := httptest.NewRecorder()
	RaiseHttpError(fmt.Errorf("[impl]403 Nope."), w)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a non-rate-limit error carried Retry-After=%q", got)
	}

	// a zero hint means "window unknown" and must not be sent as Retry-After: 0
	w = httptest.NewRecorder()
	RaiseHttpError(&retryAfterTestError{message: "429 Too many attempts.", seconds: 0}, w)
	if got := w.Header().Get("Retry-After"); got != "" {
		t.Fatalf("a zero retry hint was sent as Retry-After=%q", got)
	}
}
