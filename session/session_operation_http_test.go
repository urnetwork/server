package session

import (
	"errors"
	"github.com/urnetwork/server"
	"testing"
)

func TestSessionOperationApiErrorClassifiesKnownAndUnknownOutcomes(t *testing.T) {
	id := server.NewId()
	known := sessionError("session_not_found")
	if sessionOperationApiError(id, nil) != nil || sessionOperationApiError(id, known) != known {
		t.Fatal("known outcome changed")
	}
	var revoked *SessionError
	if !errors.As(sessionOperationApiError(id, ErrSessionRevoked), &revoked) || revoked.Status != 401 {
		t.Fatal("revoked credential treated as unknown")
	}
	if !errors.As(sessionOperationApiError(id, ErrByJwtInactive), &revoked) || revoked.Status != 401 || revoked.Code != "credential_rejected" {
		t.Fatal("inactive credential treated as unknown")
	}
	for _, cause := range []error{ErrAuthUnavailable, ErrSessionStoreUnavailable, errors.New("private SQL commit failure")} {
		err := sessionOperationApiError(id, cause)
		var unavailable *SessionOperationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.OperationId != id || !errors.Is(err, ErrAuthUnavailable) || !errors.Is(err, cause) {
			t.Fatal("unknown outcome lost cause or operation identity", err)
		}
	}
}
