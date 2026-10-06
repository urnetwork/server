// A refusal that carries its result keeps its 4xx status and answers with the
// result as its JSON body: network create's coded refusal for a client that
// asked for `result_errors`, which the signup liveness monitor must keep
// counting as a refusal, not as a created network.
package router

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/session"
)

// The shape controller.networkCreateCodedRefusal presents to the router: a
// "<status> <message>" error with its result read through a one-method
// interface. The controller's type is unexported, so this stands in for it.
type resultBodyTestError struct {
	message string
	body    any
}

// The refusal's transport text.
func (self *resultBodyTestError) Error() string {
	return self.message
}

// The result the refusal answers with.
func (self *resultBodyTestError) HttpErrorResultBody() any {
	return self.body
}

// The result is the body only with a 4xx status, also through an
// implementation tag. Without a status (an unclassified failure), with a 5xx,
// or when the result does not marshal, the plain message stands; an error
// without a result is unchanged.
func TestRaiseHttpErrorResultBodyKeepsTheRefusalStatus(t *testing.T) {
	const message = "The signature does not match this wallet address. Sign the challenge with this address."
	result := map[string]any{"error": map[string]any{"code": "signature_mismatch", "message": message}}
	const resultJson = `{"error":{"code":"signature_mismatch","message":"The signature does not match this wallet address. Sign the challenge with this address."}}` + "\n"
	impl := func(clientSession *session.ClientSession) (string, error) { return "", nil }
	const plainText = "text/plain; charset=utf-8"

	tests := []struct {
		err         error
		statusError bool
		status      int
		contentType string
		body        string
	}{
		{
			err:         &resultBodyTestError{message: "401 " + message, body: result},
			statusError: true,
			status:      http.StatusUnauthorized,
			contentType: "application/json",
			body:        resultJson,
		},
		{
			err:         tagImplError(impl, &resultBodyTestError{message: "401 " + message, body: result}),
			statusError: true,
			status:      http.StatusUnauthorized,
			contentType: "application/json",
			body:        resultJson,
		},
		{
			err:         &resultBodyTestError{message: "unclassified failure", body: result},
			statusError: false,
			status:      http.StatusInternalServerError,
			contentType: plainText,
			body:        "unclassified failure\n",
		},
		{
			err:         &resultBodyTestError{message: "500 internal failure", body: result},
			statusError: true,
			status:      http.StatusInternalServerError,
			contentType: plainText,
			body:        "internal failure\n",
		},
		{
			err:         &resultBodyTestError{message: "401 " + message, body: func() {}},
			statusError: true,
			status:      http.StatusUnauthorized,
			contentType: plainText,
			body:        message + "\n",
		},
		{
			err:         errors.New("401 invalid signature"),
			statusError: true,
			status:      http.StatusUnauthorized,
			contentType: plainText,
			body:        "invalid signature\n",
		},
	}
	for _, test := range tests {
		w := httptest.NewRecorder()
		statusError := RaiseHttpError(test.err, w)
		if statusError != test.statusError || w.Code != test.status {
			t.Errorf("%q: status error %t, status %d, want %t, %d", test.err, statusError, w.Code, test.statusError, test.status)
		}
		if contentType := w.Header().Get("Content-Type"); contentType != test.contentType {
			t.Errorf("%q: content type %q, want %q", test.err, contentType, test.contentType)
		}
		if body := w.Body.String(); body != test.body {
			t.Errorf("%q: body %q, want %q", test.err, body, test.body)
		}
	}
}
