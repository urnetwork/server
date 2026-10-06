// Fixed request-stage diagnostics retain privacy and TLS identity evidence.
package egresshealth

import (
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"os"
	"testing"
)

// Unknown stage text cannot become an identity-bearing diagnostic.
func TestRequestStageUnknownRemainsPrivate(t *testing.T) {
	err := &echoStageSyntheticError{stage: "private.example/synthetic-token", cause: errors.New("synthetic-token")}
	if got := echoRequestStage(err); got != "request_unknown" {
		t.Fatalf("unknown stage leaked: %s", got)
	}
}

// Typed target stages survive error wrapping without losing TLS security.
func TestRequestStageTypedTlsAndTimeout(t *testing.T) {
	for _, stage := range []string{"dial_dns", "dial_tcp", "dial_dns_or_socket", "tls", "policy"} {
		err := &echoStageSyntheticError{stage: stage, cause: os.ErrDeadlineExceeded}
		if got := echoRequestStage(err); got != stage {
			t.Fatalf("stage=%s got=%s", stage, got)
		}
	}
	if !isTlsAuthenticationFailure(&echoStageSyntheticError{stage: "tls", cause: x509.UnknownAuthorityError{}}) {
		t.Fatal("typed TLS identity failure was hidden")
	}
}

// Supplies an HTTP response without network, timing or scheduler dependencies.
type echoStageRoundTripper func(*http.Request) (*http.Response, error)

func (self echoStageRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return self(req)
}

// The structural method represents metadata from the owned provider dialer.
type echoStageSyntheticError struct {
	stage string
	cause error
}

func (self *echoStageSyntheticError) Error() string             { return "synthetic-private-token" }
func (self *echoStageSyntheticError) Unwrap() error             { return self.cause }
func (self *echoStageSyntheticError) ProviderHttpStage() string { return self.stage }

// Forces a body failure synchronously after status receipt.
type echoStageFailReader struct{}

func (echoStageFailReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
