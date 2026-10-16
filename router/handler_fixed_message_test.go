// The router's own failures, before and after the impl, answer with a fixed
// message; the cause, which can quote sockets or Go types, stays in the log.
package router

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

// A result whose encoding fails with a synthetic cause.
type fixedMessageTestUnencodable struct{}

func (self fixedMessageTestUnencodable) MarshalJSON() ([]byte, error) {
	return nil, errors.New(privateErrorTestCause)
}

// A body whose read fails the way a timed-out connection's does.
type fixedMessageTestFailingBody struct{}

func (self fixedMessageTestFailingBody) Read([]byte) (int, error) {
	return 0, errors.New("read tcp 192.0.2.1:443->198.51.100.2:5000: i/o timeout")
}

func (self fixedMessageTestFailingBody) Close() error {
	return nil
}

// Serves impl, with input, for a request built by the caller.
func fixedMessageTestServe(request *http.Request, bodyFormatter BodyFormatFunction) privateErrorTestResponse {
	response := httptest.NewRecorder()
	WrapWithInputBodyFormatterNoAuth(bodyFormatter, func(input map[string]string, clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "synthetic result", nil
	}, response, request)
	return privateErrorTestResponse{code: response.Code, body: response.Body.String(), contentType: response.Header().Get("Content-Type"), retryAfter: response.Header().Get("Retry-After")}
}

// Requires the fixed answer and that no listed cause text reached the body.
func requireFixedMessage(t *testing.T, response privateErrorTestResponse, status int, message string, causes ...string) {
	t.Helper()
	for _, cause := range causes {
		if strings.Contains(response.body, cause) {
			t.Fatalf("raw error text reached the caller: %q in %+v", cause, response)
		}
	}
	if response.code != status || response.body != message+"\n" {
		t.Fatalf("failure was not answered with its fixed message: %+v", response)
	}
}

// A peer address that does not parse cannot make a session. The session's
// error, which quotes the address, was the 500 body.
func TestWrapAnswersASessionFailureWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "synthetic-peer"
	response := httptest.NewRecorder()
	WrapNoAuth(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "synthetic result", nil
	}, response, request)
	requireFixedMessage(t, privateErrorTestResponse{code: response.Code, body: response.Body.String()}, http.StatusInternalServerError, UnexpectedErrorMessage, "synthetic-peer", "remote address")
}

// The same, for the wrapper that decodes a request body.
func TestWrapWithInputAnswersASessionFailureWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"name":"synthetic"}`)))
	request.RemoteAddr = "synthetic-peer"
	requireFixedMessage(t, fixedMessageTestServe(request, RequestBodyFormatter), http.StatusInternalServerError, UnexpectedErrorMessage, "synthetic-peer", "remote address")
}

// A result that does not encode was answered with the encoder's error, which
// carries the cause and names the Go type.
func TestWrapAnswersAResponseEncodingFailureWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:4500"
	response := httptest.NewRecorder()
	WrapNoAuth(func(clientSession *session.ClientSession) (fixedMessageTestUnencodable, error) {
		defer clientSession.Cancel()
		return fixedMessageTestUnencodable{}, nil
	}, response, request)
	requireFixedMessage(t, privateErrorTestResponse{code: response.Code, body: response.Body.String()}, http.StatusInternalServerError, UnexpectedErrorMessage, "synthetic", "fixedMessageTestUnencodable")
}

// A body that does not decode was answered with the decoder's error, which
// names the Go type the body was decoded into.
func TestWrapWithInputAnswersADecodeFailureWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"name":7}`)))
	request.RemoteAddr = "192.0.2.10:4500"
	requireFixedMessage(t, fixedMessageTestServe(request, RequestBodyFormatter), http.StatusBadRequest, InvalidRequestBodyMessage, "json", "map[string]string")
}

// A body read that fails was answered with the read error, which can name the
// connection's sockets.
func TestWrapWithInputAnswersAReadFailureWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.RemoteAddr = "192.0.2.10:4500"
	request.Body = fixedMessageTestFailingBody{}
	requireFixedMessage(t, fixedMessageTestServe(request, RequestBodyFormatter), http.StatusBadRequest, UnreadableRequestBodyMessage, "192.0.2.1", "timeout")
}

// A body formatter's refusal, such as a webhook signature check, was answered
// with the formatter's own error.
func TestWrapWithInputAnswersAFormatterFailureWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"name":"synthetic"}`)))
	request.RemoteAddr = "192.0.2.10:4500"
	formatter := func(*session.ClientSession, *http.Request) (io.Reader, error) {
		return nil, errors.New(privateErrorTestCause)
	}
	requireFixedMessage(t, fixedMessageTestServe(request, formatter), http.StatusBadRequest, InvalidRequestBodyMessage, "synthetic")
}

// An impl error that declares no status, such as a storage failure, was the
// 500's body, with the auth wrapper's tag.
func TestWrapAnswersAnUndeclaredImplErrorWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:4500"
	response := httptest.NewRecorder()
	WrapNoAuth(func(clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", errors.New(privateErrorTestCause)
	}, response, request)
	requireFixedMessage(t, privateErrorTestResponse{code: response.Code, body: response.Body.String()}, http.StatusInternalServerError, UnexpectedErrorMessage, "synthetic", "router")
}

// The same, for the wrapper that decodes a request body.
func TestWrapWithInputAnswersAnUndeclaredImplErrorWithAFixedMessage(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"name":"synthetic"}`)))
	request.RemoteAddr = "192.0.2.10:4500"
	response := httptest.NewRecorder()
	WrapWithInputNoAuth(func(input map[string]string, clientSession *session.ClientSession) (string, error) {
		defer clientSession.Cancel()
		return "", errors.New(privateErrorTestCause)
	}, response, request)
	requireFixedMessage(t, privateErrorTestResponse{code: response.Code, body: response.Body.String()}, http.StatusInternalServerError, UnexpectedErrorMessage, "synthetic")
}
