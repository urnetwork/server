// Cancellation and response validation are independent receipts: both may
// fail on the same read, and the cancellation marker must not mask corruption.
package perfvar

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// The joined-error rows reproduce the old predicate's exact masking shape.
func TestNativeBrowsingCanceledPrefixDoesNotMaskValidationFailure(t *testing.T) {
	expected := []byte("expected-body")
	for _, test := range []struct {
		name         string
		status       int
		body         []byte
		readErr      error
		wantCanceled bool
		wantMismatch bool
	}{
		{name: "valid-empty-cancel", status: http.StatusOK, readErr: context.Canceled, wantCanceled: true},
		{name: "valid-prefix-cancel", status: http.StatusOK, body: expected[:4], readErr: context.Canceled, wantCanceled: true},
		{name: "corrupt-prefix-cancel", status: http.StatusOK, body: []byte("bad"), readErr: context.Canceled, wantMismatch: true},
		{name: "bad-status-cancel", status: http.StatusBadGateway, body: expected[:4], readErr: context.Canceled, wantMismatch: true},
		{name: "overlong-cancel", status: http.StatusOK, body: append(append([]byte(nil), expected...), '!'), readErr: context.Canceled, wantMismatch: true},
		{name: "short-complete", status: http.StatusOK, body: expected[:4], wantMismatch: true},
		{name: "valid-complete", status: http.StatusOK, body: expected},
	} {
		err := nativeBrowsingResponseError(test.status, test.body, expected, test.readErr)
		result := nativeBrowsingAssetResult{bytes: len(test.body), err: err}
		if got := nativeBrowsingCanceledAssetValid(result); got != test.wantCanceled {
			t.Errorf("%s canceled validity=%t want=%t err=%v", test.name, got, test.wantCanceled, err)
		}
		if errors.Is(err, errNativeBrowsingResponseMismatch) != test.wantMismatch {
			t.Errorf("%s lost exact validation error: %v", test.name, err)
		}
		if test.wantMismatch && test.readErr != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("%s did not exercise the formerly masked joined cancellation", test.name)
		}
	}
}
