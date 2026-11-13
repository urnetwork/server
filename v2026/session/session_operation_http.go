package session

import (
	"errors"
	"github.com/urnetwork/server/v2026"
)

// An operation may have crossed its Redis cutoff even when its caller cannot
// observe the result. Keep the submitted identity available for safe recovery.
type SessionOperationUnavailableError struct {
	OperationId server.Id
	cause       error
}

func (self *SessionOperationUnavailableError) Error() string {
	return "503 Session operation outcome is temporarily unavailable."
}
func (self *SessionOperationUnavailableError) Unwrap() error {
	return errors.Join(ErrAuthUnavailable, self.cause)
}
func (self *SessionOperationUnavailableError) RetryAfterSeconds() int { return 1 }
func (self *SessionOperationUnavailableError) HttpUnavailableResultBody() any {
	return struct {
		OperationId server.Id `json:"operation_id"`
		Status      string    `json:"status"`
		State       string    `json:"state"`
		Code        string    `json:"code"`
		Error       string    `json:"error"`
	}{self.OperationId, "unknown", "unknown", "session_operation_unavailable", "Session operation outcome is temporarily unavailable."}
}

func sessionOperationApiError(operationId server.Id, err error) error {
	if err == nil {
		return nil
	}
	var refusal *SessionError
	if errors.As(err, &refusal) && refusal.Status != 503 {
		return err
	}
	if errors.Is(err, ErrSessionRevoked) {
		return sessionError("session_revoked")
	}
	if errors.Is(err, ErrByJwtInactive) {
		return &SessionError{Code: "credential_rejected", Status: 401}
	}
	return &SessionOperationUnavailableError{OperationId: operationId, cause: err}
}
