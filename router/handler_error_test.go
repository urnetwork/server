package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"
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
	assert.Equal(t, code, 403)
	assert.Equal(t, message, "Feedback does not belong to your network.")
	assert.Equal(t, statusError, true)

	// tagged by an auth wrapper
	code, message, statusError = raise("[github.com/urnetwork/server/api/handlers.LogUpload.func1]404 Feedback not found.")
	assert.Equal(t, code, 404)
	assert.Equal(t, message, "Feedback not found.")
	assert.Equal(t, statusError, true)

	// nested tags
	code, message, statusError = raise("[outer][inner]429 Rate limited.")
	assert.Equal(t, code, 429)
	assert.Equal(t, message, "Rate limited.")
	assert.Equal(t, statusError, true)

	// no status code: internal error, message passed through
	code, message, statusError = raise("something broke")
	assert.Equal(t, code, http.StatusInternalServerError)
	assert.Equal(t, message, "something broke")
	assert.Equal(t, statusError, false)

	// tag without a status code stays a 500 with the full message
	code, message, statusError = raise("[impl]something broke")
	assert.Equal(t, code, http.StatusInternalServerError)
	assert.Equal(t, message, "[impl]something broke")
	assert.Equal(t, statusError, false)

	// a message that merely starts with a number is not a status code...
	// unless it parses as one; this mirrors the existing convention where
	// numbers must be deliberate
	code, _, statusError = raise("500 internal")
	assert.Equal(t, code, 500)
	assert.Equal(t, statusError, true)
}
