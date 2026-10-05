// Companion verification distinguishes unavailable originals from an actual
// contradiction even when the owning request is canceled at the same boundary.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/urfoundation/sn/payoutartifact"
)

// SDK transport may retry unavailable I/O, but cannot renew contradicted
// evidence by treating the final independent verifier's refusal as a 5xx.
func TestProviderWorkWindowHttpPreservesEvidenceRefusal(t *testing.T) {
	for _, entry := range []struct {
		cause  error
		status int
	}{{cause: errors.Join(payoutartifact.ErrClosedWorkIntegrity, context.Canceled), status: http.StatusConflict}, {cause: payoutartifact.ErrClosedWorkUnavailable, status: http.StatusNotFound}, {cause: payoutartifact.ErrClosedWorkCapacity, status: http.StatusTooManyRequests}, {cause: errors.Join(syscall.EIO, context.DeadlineExceeded), status: http.StatusServiceUnavailable}} {
		writer := httptest.NewRecorder()
		providerWorkHttpError(writer, entry.cause)
		if writer.Code != entry.status {
			t.Fatal("whole-work evidence refusal lost its actual cause", entry.cause, writer.Code)
		}
	}
}
