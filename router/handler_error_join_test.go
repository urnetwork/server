// Joined cancellation/Close causes retain their leading HTTP status without
// granting later message lines authority over response status or retry hints.
package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

// Reproduces the actual upload's joined deadline failure and cancellation,
// through the same tag helper used by authenticated and unauthenticated calls.
func TestRaiseHttpErrorPreservesJoinedCancellationStatus(t *testing.T) {
	t.Parallel()
	cause := errors.Join(errors.New("actual native upload read interruption failed"), context.Canceled)
	err := fmt.Errorf("408 Upload canceled: %w", cause)
	impl := func(clientSession *session.ClientSession) (string, error) { return "", err }
	for _, variation := range []struct {
		name string
		err  error
	}{
		{name: "untagged", err: err},
		{name: "one real tag", err: tagImplError(impl, err)},
		{name: "nested real tags", err: tagImplError(impl, tagImplError(impl, err))},
	} {
		response := httptest.NewRecorder()
		status := RaiseHttpError(variation.err, response)
		if !status || response.Code != http.StatusRequestTimeout || response.Body.String() != "Upload canceled: "+cause.Error()+"\n" || response.Header().Get("Retry-After") != "" {
			t.Fatalf("joined cancellation status was lost: %s status=%d explicit=%t body=%q", variation.name, response.Code, status, response.Body.String())
		}
		if !errors.Is(variation.err, context.Canceled) {
			t.Fatal("real tagged prerequisite lost cancellation identity")
		}
	}
}

// Newline-containing tails remain exact, including another numeric prefix.
// Only a status deliberately written on the first line owns the response.
func TestRaiseHttpErrorLeadingStatusOwnsMultilineTail(t *testing.T) {
	t.Parallel()
	for _, variation := range []struct {
		message string
		code    int
		tail    string
	}{
		{message: "403 primary refusal\n429 nested refusal", code: 403, tail: "primary refusal\n429 nested refusal"},
		{message: "[outer][inner]\t503\tprovider failed\r\n200 nested success", code: 503, tail: "provider failed\r\n200 nested success"},
		{message: "408 \ncontext canceled\n", code: 408, tail: "\ncontext canceled\n"},
		{message: "  499 canceled\n[child]401 stale", code: 499, tail: "canceled\n[child]401 stale"},
	} {
		response := httptest.NewRecorder()
		if !RaiseHttpError(errors.New(variation.message), response) || response.Code != variation.code || response.Body.String() != variation.tail+"\n" {
			t.Fatalf("leading status lost its joined tail: code=%d body=%q", response.Code, response.Body.String())
		}
	}
}

// Neither whitespace nor an implementation tag may cross a line to promote
// a later joined cause's number into a top-level HTTP status.
func TestRaiseHttpErrorRejectsLaterLineStatus(t *testing.T) {
	t.Parallel()
	for _, message := range []string{
		"\n408 nested", "[impl]\n408 nested", "[outer\ninner]408 nested",
		"[impl]\r\n429 nested", " \t\n503 nested", "[one][two\rthree]403 nested",
		"unclassified failure\n408 nested", "408\nmissing horizontal separator",
	} {
		response := httptest.NewRecorder()
		if RaiseHttpError(errors.New(message), response) || response.Code != http.StatusInternalServerError || response.Body.String() != message+"\n" {
			t.Fatalf("later-line status was promoted: input=%q code=%d body=%q", message, response.Code, response.Body.String())
		}
	}
}

// Informational/success/redirect/invalid/overflow values must not panic the
// real writer or turn an error return into a successful response.
func TestRaiseHttpErrorRejectsNonErrorOrNoncanonicalCodes(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"0", "99", "100", "199", "200", "299", "301", "399", "600", "999", "1000", "0408", "+408", "-408", strings.Repeat("9", 100)} {
		message := "[impl]" + prefix + " not an error-status declaration"
		response := httptest.NewRecorder()
		var recovered any
		explicit := false
		func() {
			defer func() { recovered = recover() }()
			explicit = RaiseHttpError(errors.New(message), response)
		}()
		if recovered != nil || explicit || response.Code != http.StatusInternalServerError || response.Body.String() != message+"\n" {
			t.Errorf("non-error status prefix was admitted: prefix=%s panic=%v code=%d explicit=%t", prefix, recovered, response.Code, explicit)
		}
	}
	for code := 400; code <= 599; code++ {
		response := httptest.NewRecorder()
		if !RaiseHttpError(errors.New(strconv.Itoa(code)+" admitted error"), response) || response.Code != code || response.Body.String() != "admitted error\n" {
			t.Fatalf("valid canonical error status %d changed", code)
		}
	}
}

// The actual errors.As path still finds a positive typed retry hint through
// joined causes and real implementation tags; zero/negative hints add nothing.
func TestRaiseHttpErrorJoinedRetryHintsPreserveStatus(t *testing.T) {
	t.Parallel()
	for _, seconds := range []int{300, 0, -1} {
		hint := &retryAfterTestError{message: "429 protected upload budget exhausted", seconds: seconds}
		err := errors.Join(hint, errors.New("joined storage observation"), context.Canceled)
		impl := func(clientSession *session.ClientSession) (string, error) { return "", err }
		response := httptest.NewRecorder()
		if !RaiseHttpError(tagImplError(impl, err), response) || response.Code != http.StatusTooManyRequests || response.Body.String() != "protected upload budget exhausted\njoined storage observation\ncontext canceled\n" {
			t.Fatalf("joined retry status was lost: seconds=%d code=%d body=%q", seconds, response.Code, response.Body.String())
		}
		expected := ""
		if seconds > 0 {
			expected = strconv.Itoa(seconds)
		}
		if response.Header().Get("Retry-After") != expected {
			t.Fatalf("joined retry hint differs: got=%q want=%q", response.Header().Get("Retry-After"), expected)
		}
	}
}

// A nested rate-limit error is not authority to label an unclassified outer
// failure retryable. Its diagnostic bytes remain visible without a new hint.
func TestRaiseHttpErrorRefusesNestedRetryHintWithoutStatus(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"unclassified failure", "[impl]unclassified failure", "0408 invalid prefix"} {
		err := errors.Join(errors.New(prefix), &retryAfterTestError{message: "429 nested hint", seconds: 60})
		response := httptest.NewRecorder()
		if RaiseHttpError(err, response) || response.Code != http.StatusInternalServerError || response.Body.String() != err.Error()+"\n" || response.Header().Get("Retry-After") != "" {
			t.Fatalf("nested retry hint escaped unrecognized error: code=%d retry=%q", response.Code, response.Header().Get("Retry-After"))
		}
	}
}

// The actual wrapper, tag and response path must not flatten joined errors or
// call a success formatter after a cancellation/Close failure.
func TestRaiseHttpErrorTaggedJoinedFailureThroughActualWrapper(t *testing.T) {
	t.Parallel()
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.RemoteAddr = "127.0.0.1:52344"
	response := httptest.NewRecorder()
	formatted := false
	WrapNoAuth(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", fmt.Errorf("408 Upload canceled: %w", errors.Join(errors.New("native close failure"), context.Canceled))
	}, response, request, func(string) bool { formatted = true; return true })
	if response.Code != http.StatusRequestTimeout || formatted || response.Body.String() != "Upload canceled: native close failure\ncontext canceled\n" {
		t.Fatalf("actual wrapper lost joined status: code=%d formatted=%t body=%q", response.Code, formatted, response.Body.String())
	}
}
