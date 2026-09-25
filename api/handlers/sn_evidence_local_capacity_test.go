// The same immutable publication endpoint reports physical local capacity
// without exposing filesystem paths or disguising authorization failures.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/urnetwork/server/startifact"
)

// Only complete capacity failures receive the actionable public diagnosis.
func TestSnEvidenceCapacityLocalOperatingSystemRefusals(t *testing.T) {
	prior := publishSnEvidence
	t.Cleanup(func() { publishSnEvidence = prior })
	for _, sample := range []struct {
		cause  error
		status int
	}{
		{cause: syscall.ENOSPC, status: http.StatusInsufficientStorage},
		{cause: syscall.EDQUOT, status: http.StatusInsufficientStorage},
		{cause: errors.Join(syscall.ENOSPC, os.ErrPermission), status: http.StatusBadRequest},
		{cause: syscall.EIO, status: http.StatusBadRequest},
	} {
		publishSnEvidence = func(_ context.Context, _ []byte) (*startifact.Published, error) {
			return nil, &os.PathError{Op: "write", Path: "synthetic-private-store", Err: sample.cause}
		}
		response := serveSnEvidence(http.MethodPost, "/sn/evidence", []byte(`{"synthetic":true}`))
		if response.Code != sample.status || strings.Contains(response.Body.String(), "synthetic-private-store") {
			t.Fatalf("local refusal lost status or exposed private path: %d %s", response.Code, response.Body.String())
		}
	}
}
