package router

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/session"
)

func TestRequestFormattersShareImplementationSession(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(`{"value":"healthy"}`))
	request.Header.Set("X-UR-Forwarded-For", "192.0.2.10:1234")
	recorder := httptest.NewRecorder()
	var bodySession, implementationSession *session.ClientSession
	formatted := false
	wrapWithInput(
		func(clientSession *session.ClientSession, req *http.Request) (io.Reader, error) {
			bodySession = clientSession
			return req.Body, nil
		},
		func(input map[string]string, clientSession *session.ClientSession) (string, error) {
			implementationSession = clientSession
			return input["value"], nil
		},
		recorder, request,
		func(clientSession *session.ClientSession, result string) bool {
			formatted = true
			if clientSession == nil || clientSession != bodySession || clientSession != implementationSession {
				t.Fatal("body, implementation and response did not share a session")
			}
			if clientSession.ClientAddress != "192.0.2.10:1234" || result != "healthy" {
				t.Fatal("formatter lost request identity or implementation result")
			}
			return false
		},
	)
	if !formatted || recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `"healthy"` {
		t.Fatal("formatter continuation did not retain the default JSON response")
	}
}

func TestNoInputFormatterSharesSessionAndCanComplete(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/output", nil)
	recorder := httptest.NewRecorder()
	var implementationSession *session.ClientSession
	wrap(func(clientSession *session.ClientSession) (int, error) {
		implementationSession = clientSession
		return 7, nil
	}, recorder, request, func(clientSession *session.ClientSession, result int) bool {
		if clientSession == nil || clientSession != implementationSession || result != 7 {
			t.Fatal("formatter did not receive the implementation session/result")
		}
		recorder.WriteHeader(http.StatusNoContent)
		return true
	})
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatal("completed formatter was followed by a second response")
	}
}
