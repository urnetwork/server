package egresshealth

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Executes the actual warm-up with a synchronous synthetic HTTP owner.
func runEchoStage(t *testing.T, roundTrip func(*http.Request) (*http.Response, error)) (string, string, bool) {
	t.Helper()
	exit := &exitRecord{}
	run := &run{
		opts:    Options{IpEchoUrl: "https://echo.example/my-ip-info", IpEchoTimeout: time.Minute},
		profile: DefaultRequestProfile(), exit: exit,
	}
	run.warmClient(context.Background(), &http.Client{Transport: echoStageRoundTripper(roundTrip)})
	ip, _, message, tlsFailure := exit.read()
	return ip, message, tlsFailure
}

// Diagnostic strings contain only fixed enums, even when lower errors contain
// request URLs, headers, private identifiers, or synthetic secrets.
func TestEchoStageDialTimeoutPrivacy(t *testing.T) {
	_, message, tlsFailure := runEchoStage(t, func(*http.Request) (*http.Response, error) {
		return nil, &echoStageSyntheticError{stage: "dial_dns_or_socket", cause: os.ErrDeadlineExceeded}
	})
	if message != "echo_stage=dial_dns_or_socket error_class=timeout" || tlsFailure {
		t.Fatalf("unexpected fixed diagnostic: %q tls=%t", message, tlsFailure)
	}
}

// An unknown/custom client's stage is not trusted as a label or causal claim.
func TestEchoStageUnknownFailsClosed(t *testing.T) {
	_, message, _ := runEchoStage(t, func(*http.Request) (*http.Response, error) {
		return nil, &echoStageSyntheticError{stage: "secret.example/private-token", cause: errors.New("private-token")}
	})
	if message != "echo_stage=request_unknown error_class=other" {
		t.Fatalf("unknown stage escaped finite domain: %q", message)
	}
}

// Receiving a non-200 response is a distinct, observed HTTP status boundary.
func TestEchoStageResponseStatus(t *testing.T) {
	_, message, _ := runEchoStage(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("private-token"))}, nil
	})
	if message != "echo_stage=response_status error_class=other" {
		t.Fatalf("status not diagnosed safely: %q", message)
	}
}

// A body error occurs after an actual successful status, not during dialing.
func TestEchoStageResponseBody(t *testing.T) {
	_, message, _ := runEchoStage(t, func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(echoStageFailReader{})}, nil
	})
	if message != "echo_stage=response_body error_class=unexpected_eof" {
		t.Fatalf("body not diagnosed safely: %q", message)
	}
}

// Neither malformed JSON nor missing info.ip may leak its response fragment.
func TestEchoStageResponseSchemaPrivacy(t *testing.T) {
	for _, body := range []string{"private-token", `{"info":{"ip":"private-token"}}`} {
		_, message, _ := runEchoStage(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
		})
		if message != "echo_stage=response_schema error_class=other" {
			t.Fatalf("schema not diagnosed safely: %q", message)
		}
	}
}

// Stage sanitization must retain the typed TLS-authentication signal used by
// the existing security verdict, even though raw certificate data is withheld.
func TestEchoStageTlsAuthenticationPreserved(t *testing.T) {
	_, message, tlsFailure := runEchoStage(t, func(*http.Request) (*http.Response, error) {
		return nil, &echoStageSyntheticError{stage: "tls", cause: x509.UnknownAuthorityError{}}
	})
	if message != "echo_stage=tls error_class=tls_authentication" || !tlsFailure {
		t.Fatalf("TLS authentication evidence changed: %q tls=%t", message, tlsFailure)
	}
}

// A healthy echo keeps its exit and produces no error diagnostic.
func TestEchoStageHealthy(t *testing.T) {
	ip, message, tlsFailure := runEchoStage(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.Path != IpEchoPath || req.Header.Get("Accept") != "application/json" {
			t.Fatal("warm-up request contract changed")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"info":{"ip":"192.0.2.7"}}`))}, nil
	})
	if ip != "192.0.2.7" || message != "" || tlsFailure {
		t.Fatal("healthy echo changed")
	}
}

// With no configured echo, there is no attempted echo or manufactured error.
func TestEchoStageUnconfigured(t *testing.T) {
	exit := &exitRecord{}
	run := &run{opts: Options{}, profile: DefaultRequestProfile(), exit: exit}
	run.warmClient(context.Background(), &http.Client{Transport: echoStageRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("unconfigured echo made a request")
		return nil, context.Canceled
	})})
	ip, _, message, tlsFailure := exit.read()
	if ip != "" || message != "" || tlsFailure {
		t.Fatal("unconfigured echo manufactured evidence")
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
