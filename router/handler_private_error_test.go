// An impl failure that declares no status is answered with a fixed 503 and
// never with its own text; every declared answer stays exactly as it was.
package router

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

// Synthetic causes a failing dependency could put in its error text.
const privateErrorTestCause = "synthetic storage failure: dial tcp 192.0.2.7:5432: password=synthetic-secret"

// A generic impl, whose runtime name carries "[...]".
func privateErrorTestGenericImpl[R any](clientSession *session.ClientSession) (R, error) {
	defer clientSession.Cancel()
	var empty R
	return empty, errors.New("409 synthetic refusal")
}

// The response of one call, by value, so two calls compare directly.
type privateErrorTestResponse struct {
	code        int
	body        string
	contentType string
	retryAfter  string
}

// Serves impl through WrapNoAuth to a recorder.
func privateErrorTestServe(impl ImplFunction[string]) privateErrorTestResponse {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:4500"
	response := httptest.NewRecorder()
	WrapNoAuth(impl, response, request)
	return privateErrorTestResponse{code: response.Code, body: response.Body.String(), contentType: response.Header().Get("Content-Type"), retryAfter: response.Header().Get("Retry-After")}
}

// Serves impl through WrapWithInputNoAuth to a recorder, with a JSON body.
func privateErrorTestServeWithInput(impl ImplWithInputFunction[map[string]string, string]) privateErrorTestResponse {
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"name":"synthetic"}`)))
	request.RemoteAddr = "192.0.2.10:4500"
	response := httptest.NewRecorder()
	WrapWithInputNoAuth(impl, response, request)
	return privateErrorTestResponse{code: response.Code, body: response.Body.String(), contentType: response.Header().Get("Content-Type"), retryAfter: response.Header().Get("Retry-After")}
}

// The tag ended at the first "]" of "[...]", so RaiseHttpError read no status
// after it and answered a declared refusal with a 500 and the tagged text.
func TestImplTagOfAGenericImplKeepsTheDeclaredStatus(t *testing.T) {
	response := privateErrorTestServe(privateErrorTestGenericImpl[string])
	if response.code != http.StatusConflict || response.body != "synthetic refusal\n" {
		t.Fatalf("generic impl lost its declared status: code=%d body=%q", response.code, response.body)
	}
}

// The undeclared failure's text, including where it came from, stays private.
func TestPrivateErrorsAnswersAnUndeclaredFailureWithAFixedUnavailable(t *testing.T) {
	response := privateErrorTestServe(PrivateErrors(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "partial", errors.Join(errors.New(privateErrorTestCause), context.Canceled)
	}))
	if strings.Contains(response.body, "synthetic") || strings.Contains(response.body, "192.0.2.7") || strings.Contains(response.body, "PrivateErrors") {
		t.Fatalf("raw error text reached the caller: %q", response.body)
	}
	if response.code != http.StatusServiceUnavailable || response.body != "Service temporarily unavailable.\n" || response.retryAfter != "" {
		t.Fatalf("undeclared failure was not answered as unavailable: %+v", response)
	}
}

// The same, through the wrapper that decodes a request body.
func TestPrivateErrorsWithInputAnswersAnUndeclaredFailureWithAFixedUnavailable(t *testing.T) {
	response := privateErrorTestServeWithInput(PrivateErrorsWithInput(func(input map[string]string, clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "partial", fmt.Errorf("decode stored %s: %w", input["name"], errors.New(privateErrorTestCause))
	}))
	if strings.Contains(response.body, "synthetic") || strings.Contains(response.body, "192.0.2.7") {
		t.Fatalf("raw error text reached the caller: %q", response.body)
	}
	if response.code != http.StatusServiceUnavailable || response.body != "Service temporarily unavailable.\n" {
		t.Fatalf("undeclared failure was not answered as unavailable: %+v", response)
	}
}

// Each declared answer, and a success, is exactly the answer without the
// wrapper: status, message, joined tail, retry hint and result body.
func TestPrivateErrorsKeepsEveryDeclaredAnswer(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("400 Synthetic input refused."),
		errors.New("[inner]409 synthetic conflict"),
		fmt.Errorf("408 Upload canceled: %w", errors.Join(errors.New("synthetic close failure"), context.Canceled)),
		&retryAfterTestError{message: "429 Synthetic rate exceeded.", seconds: 7},
		&resultBodyTestError{message: "400 synthetic refusal", body: map[string]any{"error": map[string]any{"code": "synthetic"}}},
		errors.New("500 synthetic declared failure"),
		errors.Join(session.ErrAuthUnavailable, errors.New(privateErrorTestCause)),
	} {
		impl := func(clientSession *session.ClientSession) (string, error) {
			defer clientSession.Cancel()
			if err != nil {
				return "", err
			}
			return "synthetic result", nil
		}
		bare, private := privateErrorTestServe(impl), privateErrorTestServe(PrivateErrors(impl))
		if bare != private {
			t.Errorf("declared answer changed for %v: bare=%+v private=%+v", err, bare, private)
		}
		inputImpl := func(input map[string]string, clientSession *session.ClientSession) (string, error) {
			return impl(clientSession)
		}
		bare, private = privateErrorTestServeWithInput(inputImpl), privateErrorTestServeWithInput(PrivateErrorsWithInput(inputImpl))
		if bare != private {
			t.Errorf("declared answer with input changed for %v: bare=%+v private=%+v", err, bare, private)
		}
	}
}

// A storage failure raised inside the impl, rather than returned, is the same
// unexpected failure. It escaped to the router as a panic.
func TestPrivateErrorsAnswersARaisedFailureWithAFixedUnavailable(t *testing.T) {
	response := privateErrorTestServe(PrivateErrors(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		panic(errors.New(privateErrorTestCause))
	}))
	if strings.Contains(response.body, "synthetic") || response.code != http.StatusServiceUnavailable || response.body != "Service temporarily unavailable.\n" {
		t.Fatalf("raised failure was not answered as unavailable: %+v", response)
	}
}

// A refusal declared deep in a model call and raised from there keeps its own
// status and message. It escaped to the router as a panic, a generic 500.
func TestPrivateErrorsKeepsARaisedDeclaredRefusal(t *testing.T) {
	response := privateErrorTestServeWithInput(PrivateErrorsWithInput(func(input map[string]string, clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		panic(errors.New("409 synthetic request permanently closed"))
	}))
	if response.code != http.StatusConflict || response.body != "synthetic request permanently closed\n" {
		t.Fatalf("raised declared refusal lost its status: %+v", response)
	}
}

// A declared answer keeps its status and message without the cause's text,
// and the cause's identity, here a cancellation, survives for errors.Is.
func TestPrivateCauseAnswersOnlyTheDeclaredMessage(t *testing.T) {
	cause := errors.Join(errors.New(privateErrorTestCause), context.Canceled)
	err := PrivateCause(http.StatusRequestTimeout, "Synthetic upload canceled.", cause)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("private cause lost its identity", err)
	}
	response := privateErrorTestServe(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", err
	})
	if response.code != http.StatusRequestTimeout || response.body != "Synthetic upload canceled.\n" {
		t.Fatalf("private cause changed the declared answer or sent its text: %+v", response)
	}
}

// A cause that carries a retry hint keeps it on the declared answer.
func TestPrivateCauseKeepsTheRetryHint(t *testing.T) {
	response := privateErrorTestServe(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", PrivateCause(http.StatusTooManyRequests, "Synthetic budget exhausted.", &retryAfterTestError{message: privateErrorTestCause, seconds: 9})
	})
	if response.code != http.StatusTooManyRequests || response.body != "Synthetic budget exhausted.\n" || response.retryAfter != "9" {
		t.Fatalf("private cause lost the retry hint: %+v", response)
	}
}
