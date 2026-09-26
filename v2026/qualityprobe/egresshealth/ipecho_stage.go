package egresshealth

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
)

// A warm-up diagnostic keeps typed error evidence for TLS policy but renders
// only finite enums. It must not copy echo URLs, bodies or private identities.
type echoStageError struct {
	stage string
	err   error
}

// The message is the existing IpEchoErr/log payload, not a new metric label.
func (self *echoStageError) Error() string {
	errorClass := "other"
	var timed interface{ Timeout() bool }
	switch {
	case isTlsAuthenticationFailure(self.err):
		errorClass = "tls_authentication"
	case errors.Is(self.err, context.DeadlineExceeded):
		errorClass = "deadline"
	case errors.Is(self.err, context.Canceled):
		errorClass = "canceled"
	case errors.Is(self.err, io.ErrUnexpectedEOF):
		errorClass = "unexpected_eof"
	case errors.Is(self.err, io.EOF):
		errorClass = "eof"
	case errors.Is(self.err, os.ErrDeadlineExceeded):
		errorClass = "timeout"
	case errors.As(self.err, &timed) && timed.Timeout():
		errorClass = "timeout"
	}
	return "echo_stage=" + self.stage + " error_class=" + errorClass
}

// Security classification follows the actual cause, not the diagnostic text.
func (self *echoStageError) Unwrap() error { return self.err }

// The optional structural metadata belongs to the private provider HTTP
// owner. Unknown clients and unknown enum values remain unclassified.
func echoRequestStage(err error) string {
	var staged interface{ ProviderHttpStage() string }
	if errors.As(err, &staged) {
		switch stage := staged.ProviderHttpStage(); stage {
		case "dial_dns_or_socket", "tls", "policy":
			return stage
		}
	}
	// net/http may return a request-context error without reaching the
	// provider dial owner. Keep that distinct from a typed tunnel dial failure:
	// a whole run of request deadlines is an admission/transport clue, not
	// evidence that every destination's DNS or socket dial failed.
	var timed net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "request_timeout"
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	case errors.As(err, &timed) && timed.Timeout():
		return "request_timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "request_eof"
	}
	return "request_unknown"
}
