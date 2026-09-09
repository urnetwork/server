package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

func TestJsonRequestBodyLimitAcceptsExactBoundary(t *testing.T) {
	prefix := `{"padding":"`
	suffix := `"}`
	paddingBytes := int(MaxJsonRequestBytes) - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("x", paddingBytes) + suffix
	if int64(len(body)) != MaxJsonRequestBytes {
		t.Fatalf("fixture body length = %d, want %d", len(body), MaxJsonRequestBytes)
	}

	request := httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	called := false

	WrapWithInputNoAuth(
		func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
			called = true
			return map[string]any{}, nil
		},
		recorder,
		request,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if !called {
		t.Fatal("handler was not called for an exact-limit body")
	}
}

func TestJsonRequestBodyLimitRejectsKnownAndStreamingLengths(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/input",
			strings.NewReader(strings.Repeat("x", int(MaxJsonRequestBytes)+1)),
		)
		if unknownLength {
			request.ContentLength = -1
		}
		recorder := httptest.NewRecorder()
		called := false

		WrapWithInputNoAuth(
			func(input map[string]any, clientSession *session.ClientSession) (map[string]any, error) {
				called = true
				return map[string]any{}, nil
			},
			recorder,
			request,
		)

		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("unknown length=%t: status = %d, want %d", unknownLength, recorder.Code, http.StatusRequestEntityTooLarge)
		}
		if called {
			t.Errorf("unknown length=%t: handler was called for an oversized body", unknownLength)
		}
	}
}
