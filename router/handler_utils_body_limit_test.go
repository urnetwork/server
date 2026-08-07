package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

func TestJsonRequestBodyLimitRejectsKnownAndStreamingLengths(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "streaming"}[unknownLength], func(t *testing.T) {
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
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusRequestEntityTooLarge)
			}
			if called {
				t.Fatal("handler was called for an oversized body")
			}
		})
	}
}
