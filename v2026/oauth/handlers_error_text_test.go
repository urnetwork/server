// An access refusal's description carries one of its declared reasons, never
// detail an error picked up on the way.
package oauth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Decodes the rfc 6749 error a grant failure was written as.
func grantErrorTestResponse(t *testing.T, err error) (int, oauthError) {
	t.Helper()
	w := httptest.NewRecorder()
	writeGrantError(w, err)
	var refusal oauthError
	if json.Unmarshal(w.Body.Bytes(), &refusal) != nil {
		t.Fatalf("grant error is not json: %q", w.Body.String())
	}
	return w.Code, refusal
}

// An unauthorized user's error wrapped with other detail sent that detail as
// the description.
func TestWriteGrantErrorHidesUnauthorizedDetail(t *testing.T) {
	code, refusal := grantErrorTestResponse(t, fmt.Errorf("%w: synthetic private detail", ErrUnauthorizedUser))
	if code != http.StatusForbidden || refusal.Error != "access_denied" || strings.Contains(refusal.ErrorDescription, "synthetic") || refusal.ErrorDescription != ErrUnauthorizedUser.Error() {
		t.Fatalf("access refusal carried undeclared detail: %d %+v", code, refusal)
	}
}

// The declared reasons keep their words: they tell the user what to do.
func TestWriteGrantErrorKeepsTheDeclaredReasons(t *testing.T) {
	for _, reason := range []error{errUnauthorizedGuestNetwork, errUnauthorizedNoNetwork} {
		code, refusal := grantErrorTestResponse(t, reason)
		if code != http.StatusForbidden || refusal.Error != "access_denied" || refusal.ErrorDescription != reason.Error() {
			t.Errorf("declared reason changed: %d %+v, want %q", code, refusal, reason.Error())
		}
	}
}
