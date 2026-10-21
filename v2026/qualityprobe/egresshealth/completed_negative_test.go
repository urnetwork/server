package egresshealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

type completedLossPath struct {
	client  *http.Client
	signal  context.Context
	reopens int
}

func (p *completedLossPath) Current() (*http.Client, context.Context) {
	return p.client, p.signal
}
func (p *completedLossPath) Reopen(context.Context) error {
	p.reopens++
	return errors.New("unexpected recreation in a one-attempt control")
}

// Close runs after the actual sample reader has established peer EOF. Losing
// the path at this point cannot retrospectively erase a fully received reply.
type completedLossBody struct {
	reader  *strings.Reader
	lose    context.CancelCauseFunc
	closed  bool
	sawEOF  bool
	partial bool
}

func (b *completedLossBody) Read(p []byte) (int, error) {
	if b.partial {
		b.lose(errTunnelGone)
		return 0, context.Canceled
	}
	n, err := b.reader.Read(p)
	if err == io.EOF {
		b.sawEOF = true
	}
	return n, err
}
func (b *completedLossBody) Close() error {
	b.closed = true
	if b.lose != nil {
		b.lose(errTunnelGone)
	}
	return nil
}

// One URL turn intentionally has one load attempt. These cases distinguish a
// lost unfinished request from independent, complete provider-response evidence.
func TestUrlCompletedResponseSurvivesLaterLoss(t *testing.T) {
	for _, name := range []string{"generic_timeout", "lost_timeout", "completed_success", "tls_failure", "complete_negative_live", "complete_negative_then_loss", "complete_slow_then_loss", "partial_body_loss", "partial_body_loss_after_slow_headers", "missing_request_clock_then_loss"} {
		t.Run(name, func(t *testing.T) {
			signal, lose := context.WithCancelCause(t.Context())
			defer lose(context.Canceled)
			var body *completedLossBody
			requests := 0
			clock := newFakeClock()
			client := &http.Client{Transport: echoStageRoundTripper(func(request *http.Request) (*http.Response, error) {
				requests++
				switch name {
				case "generic_timeout":
					return nil, context.DeadlineExceeded
				case "lost_timeout":
					lose(errTunnelGone)
					return nil, context.DeadlineExceeded
				case "tls_failure":
					lose(errTunnelGone)
					return nil, &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
				}
				trace := httptrace.ContextClientTrace(request.Context())
				if name != "missing_request_clock_then_loss" {
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				}
				if name == "complete_slow_then_loss" || name == "partial_body_loss_after_slow_headers" {
					_ = clock.Sleep(t.Context(), 3*time.Second)
				}
				trace.GotFirstResponseByte()
				body = &completedLossBody{reader: strings.NewReader("A small complete synthetic document."), lose: lose, partial: name == "partial_body_loss" || name == "partial_body_loss_after_slow_headers"}
				status := http.StatusOK
				if name == "complete_negative_live" || name == "complete_negative_then_loss" {
					status = http.StatusServiceUnavailable
				}
				if name == "complete_negative_live" {
					body.lose = nil
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/plain"}}, ContentLength: int64(body.reader.Len()), Body: body}, nil
			})}
			path := &completedLossPath{client: client, signal: signal}
			r := newRun(path, Options{UrlProbe: true, LoadAttempts: 1, Now: clock.Now}, 1, time.Minute, rand.New(rand.NewSource(1)))
			result := r.load(t.Context(), Destination{Name: "synthetic-page", Class: ClassSite, Url: "https://synthetic.example/page"})
			wantUnknown := name == "lost_timeout" || name == "partial_body_loss" || name == "partial_body_loss_after_slow_headers" || name == "missing_request_clock_then_loss"
			wantOK := name == "completed_success"
			wantTLS := name == "tls_failure"
			if requests != 1 || path.reopens != 0 || result.Attempts != 1 {
				t.Fatalf("one-turn attempt geometry changed: requests=%d reopens=%d attempts=%d", requests, path.reopens, result.Attempts)
			}
			if body != nil && !body.partial && (!body.sawEOF || !body.closed || !result.BodyComplete) {
				t.Fatal("fixture did not establish complete body before closing it")
			}
			if result.Ok != wantOK || result.TlsAuthenticationFailure != wantTLS {
				t.Fatalf("independent result changed: ok=%t tls_failure=%t stage=%s", result.Ok, result.TlsAuthenticationFailure, result.FailureStage)
			}
			if result.NotMeasured != wantUnknown {
				t.Errorf("completed response was suppressed or incomplete request credited: not_measured=%t want=%t body_complete=%t stage=%s", result.NotMeasured, wantUnknown, result.BodyComplete, result.FailureStage)
			}
		})
	}
}
